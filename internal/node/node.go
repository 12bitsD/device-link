package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/12bitsD/device-link/internal/client"
	"github.com/coder/websocket"
)

const (
	initialBackoff = 250 * time.Millisecond
	maxBackoff     = 2 * time.Second
	frameTimeout   = 3 * time.Second
)

var errUnauthorized = errors.New("hub authentication rejected")

type Config struct {
	HubURL   string
	Code     string
	CAPath   string
	StateDir string
}

type state struct {
	DeviceToken string `json:"device_token"`
}

type hubFrame struct {
	Type      string `json:"type"`
	Name      string `json:"name,omitempty"`
	OS        string `json:"os,omitempty"`
	Arch      string `json:"arch,omitempty"`
	Shell     string `json:"shell,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

func Run(ctx context.Context, config Config) error {
	hubURL, err := client.NormalizeHubURL(config.HubURL)
	if err != nil {
		return err
	}
	config.HubURL = hubURL
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return fmt.Errorf("create node state: %w", err)
	}
	httpClient, err := client.HTTPClient(config.CAPath)
	if err != nil {
		return err
	}
	defer httpClient.CloseIdleConnections()
	creds, err := credentials(ctx, config, httpClient)
	if err != nil {
		return err
	}
	httpClient.CloseIdleConnections()
	return maintainConnection(ctx, config.HubURL, creds.DeviceToken, httpClient)
}

func credentials(ctx context.Context, config Config, httpClient *http.Client) (state, error) {
	statePath := filepath.Join(config.StateDir, "node.json")
	saved, err := readState(statePath)
	if err == nil && saved.DeviceToken != "" {
		return saved, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return state{}, err
	}
	if config.Code == "" {
		return state{}, fmt.Errorf("invite code required for first join")
	}
	saved, err = exchangeInvite(ctx, config, httpClient)
	if err != nil {
		return state{}, err
	}
	if err := writeState(statePath, saved); err != nil {
		return state{}, err
	}
	return saved, nil
}

func readState(path string) (state, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return state{}, err
	}
	var saved state
	if err := json.Unmarshal(contents, &saved); err != nil {
		return state{}, fmt.Errorf("decode node state: %w", err)
	}
	return saved, nil
}

func writeState(path string, saved state) error {
	encoded, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("encode node state: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write node state: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure node state: %w", err)
	}
	return nil
}

func exchangeInvite(ctx context.Context, config Config, httpClient *http.Client) (state, error) {
	joinCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request := joinRequest{Code: config.Code, OS: runtime.GOOS, Arch: runtime.GOARCH, Shell: shell()}
	response, err := postJSON(joinCtx, httpClient, config.HubURL+"/v1/join", request)
	if err != nil {
		return state{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		return state{}, fmt.Errorf("join rejected: %s", response.Status)
	}
	var joined state
	if err := json.NewDecoder(response.Body).Decode(&joined); err != nil {
		return state{}, fmt.Errorf("decode join response: %w", err)
	}
	return joined, nil
}

type joinRequest struct {
	Code  string `json:"code"`
	OS    string `json:"os"`
	Arch  string `json:"arch"`
	Shell string `json:"shell"`
}

func shell() string {
	if runtime.GOOS == "windows" {
		if value := os.Getenv("COMSPEC"); value != "" {
			return value
		}
		return "powershell.exe"
	}
	if value := os.Getenv("SHELL"); value != "" {
		return value
	}
	return "/bin/sh"
}

func maintainConnection(ctx context.Context, hubURL, token string, httpClient *http.Client) error {
	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		healthy, err := connect(ctx, hubURL, token, httpClient)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errUnauthorized) {
			return err
		}
		if healthy {
			backoff = initialBackoff
		}
		if !sleep(ctx, backoff) {
			return nil
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func connect(ctx context.Context, hubURL, token string, httpClient *http.Client) (bool, error) {
	dialCtx, cancel := context.WithTimeout(ctx, frameTimeout)
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	conn, response, err := websocket.Dial(dialCtx, hubURL+"/v1/node", &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: header})
	cancel()
	if err != nil {
		return false, dialError(response, err)
	}
	return runSession(ctx, conn)
}

func dialError(response *http.Response, err error) error {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		return fmt.Errorf("%w: %s", errUnauthorized, response.Status)
	}
	return fmt.Errorf("connect to hub: %w", err)
}

func runSession(ctx context.Context, conn *websocket.Conn) (bool, error) {
	conn.SetReadLimit(4096)
	var healthy atomic.Bool
	readDone := make(chan error, 1)
	go func() {
		defer close(readDone)
		readDone <- readFrames(ctx, conn, &healthy)
	}()
	defer func() {
		_ = conn.CloseNow()
		<-readDone
	}()
	err := writeHeartbeats(ctx, conn, readDone)
	return healthy.Load(), err
}

func writeHeartbeats(ctx context.Context, conn *websocket.Conn, readDone <-chan error) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readDone:
			return err
		case <-ticker.C:
		}
		if err := writeFrame(ctx, conn, hubFrame{Type: "ping"}); err != nil {
			return fmt.Errorf("write hub heartbeat: %w", err)
		}
	}
}

func writeFrame(ctx context.Context, conn *websocket.Conn, frame hubFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode node frame: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, frameTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, payload)
}

func readFrames(ctx context.Context, conn *websocket.Conn, healthy *atomic.Bool) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, frameTimeout)
		_, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("read hub frame: %w", err)
		}
		handleFrame(data, healthy)
	}
}

func handleFrame(data []byte, healthy *atomic.Bool) {
	var frame hubFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return
	}
	switch frame.Type {
	case "connected":
		fmt.Printf("dl connected: name=%s status=alive platform=%s/%s shell=%s expires_at=%s\n",
			frame.Name, frame.OS, frame.Arch, frame.Shell, frame.ExpiresAt)
	case "pong":
		healthy.Store(true)
	}
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func postJSON(ctx context.Context, httpClient *http.Client, targetURL string, value any) (*http.Response, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	return response, nil
}
