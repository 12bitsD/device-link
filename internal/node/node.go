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
	"time"

	"github.com/12bitsD/device-link/internal/client"
	"github.com/coder/websocket"
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

func Run(ctx context.Context, config Config) error {
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return fmt.Errorf("create node state: %w", err)
	}
	httpClient, err := client.HTTPClient(config.CAPath)
	if err != nil {
		return err
	}
	credentials, err := credentials(ctx, config, httpClient)
	if err != nil {
		return err
	}
	return maintainConnection(ctx, config.HubURL, credentials.DeviceToken, httpClient)
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
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write node state: %w", err)
	}
	if err := json.NewEncoder(file).Encode(saved); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode node state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close node state: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure node state: %w", err)
	}
	return nil
}

func exchangeInvite(ctx context.Context, config Config, httpClient *http.Client) (state, error) {
	request := joinRequest{Code: config.Code, OS: runtime.GOOS, Arch: runtime.GOARCH, Shell: shell()}
	response, err := postJSON(ctx, httpClient, config.HubURL+"/v1/join", request)
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
		if shell := os.Getenv("COMSPEC"); shell != "" {
			return shell
		}
		return "powershell.exe"
	}
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func maintainConnection(ctx context.Context, hubURL, token string, client *http.Client) error {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := connect(ctx, hubURL, token, client)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errUnauthorized) {
			return err
		}
		if !sleep(ctx, backoff) {
			return nil
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

func connect(ctx context.Context, hubURL, token string, client *http.Client) error {
	dialCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	conn, response, err := websocket.Dial(dialCtx, hubURL+"/v1/node", &websocket.DialOptions{HTTPClient: client, HTTPHeader: header})
	cancel()
	if err != nil {
		return dialError(response, err)
	}
	defer func() { _ = conn.CloseNow() }()
	return heartbeatLoop(ctx, conn)
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

func heartbeatLoop(ctx context.Context, conn *websocket.Conn) error {
	conn.SetReadLimit(64)
	for {
		if !sleep(ctx, time.Second) {
			return nil
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
			return fmt.Errorf("write hub heartbeat: %w", err)
		}
		readCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		_, _, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("read hub heartbeat: %w", err)
		}
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

func postJSON(ctx context.Context, client *http.Client, url string, value any) (*http.Response, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	return response, nil
}
