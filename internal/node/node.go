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
	"sync"
	"sync/atomic"
	"time"

	"github.com/12bitsD/device-link/internal/client"
	"github.com/12bitsD/device-link/internal/transfer"
	"github.com/coder/websocket"
)

const (
	initialBackoff = 250 * time.Millisecond
	maxBackoff     = 2 * time.Second
	frameTimeout   = 3 * time.Second
	maxFrameBytes  = 16 << 20
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
	Type           string               `json:"type"`
	ReqID          string               `json:"req_id,omitempty"`
	Error          string               `json:"error,omitempty"`
	Name           string               `json:"name,omitempty"`
	OS             string               `json:"os,omitempty"`
	Arch           string               `json:"arch,omitempty"`
	Shell          string               `json:"shell,omitempty"`
	ExpiresAt      string               `json:"expires_at,omitempty"`
	JobID          string               `json:"job_id,omitempty"`
	Command        []string             `json:"command,omitempty"`
	Cwd            string               `json:"cwd,omitempty"`
	Env            []string             `json:"env,omitempty"`
	TimeoutMS      int64                `json:"timeout_ms,omitempty"`
	State          string               `json:"state,omitempty"`
	ExitCode       *int                 `json:"exit_code,omitempty"`
	Stdout         []byte               `json:"stdout,omitempty"`
	Stderr         []byte               `json:"stderr,omitempty"`
	Bytes          int64                `json:"bytes,omitempty"`
	Offset         int64                `json:"offset,omitempty"`
	Chunk          []byte               `json:"chunk,omitempty"`
	EOF            bool                 `json:"eof,omitempty"`
	TransferID     string               `json:"transfer_id,omitempty"`
	Sources        []string             `json:"sources,omitempty"`
	Excludes       []string             `json:"excludes,omitempty"`
	TargetDir      string               `json:"target_dir,omitempty"`
	Candidates     []transfer.Candidate `json:"candidates,omitempty"`
	ExistingHashes map[string]string    `json:"existing_hashes,omitempty"`
	StreamToken    string               `json:"stream_token,omitempty"`
	Files          int                  `json:"files,omitempty"`
	Skipped        int                  `json:"skipped,omitempty"`
	TransferBytes  int64                `json:"transfer_bytes,omitempty"`
	TotalBytes     int64                `json:"total_bytes,omitempty"`
}

type nodeRunner struct {
	hubURL     string
	token      string
	httpClient *http.Client
	jobs       *jobManager
}

type sessionConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	runner  *nodeRunner
	healthy atomic.Bool
	rpcWG   sync.WaitGroup
}

func (s *sessionConn) send(ctx context.Context, frame hubFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode node frame: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, frameTimeout)
	defer cancel()
	return s.conn.Write(writeCtx, websocket.MessageText, payload)
}

func Enroll(ctx context.Context, config Config) error {
	_, _, httpClient, err := enroll(ctx, config)
	if err != nil {
		return err
	}
	httpClient.CloseIdleConnections()
	return nil
}

func Run(ctx context.Context, config Config) error {
	hubURL, token, httpClient, err := enroll(ctx, config)
	if err != nil {
		return err
	}
	defer httpClient.CloseIdleConnections()
	jobs, err := newJobManager(ctx, config.StateDir, shell())
	if err != nil {
		return err
	}
	defer jobs.Close()
	runner := &nodeRunner{hubURL: hubURL, token: token, httpClient: httpClient, jobs: jobs}
	return runner.maintain(ctx)
}

func enroll(ctx context.Context, config Config) (string, string, *http.Client, error) {
	hubURL, err := client.NormalizeHubURL(config.HubURL)
	if err != nil {
		return "", "", nil, err
	}
	httpClient, err := client.HTTPClient(config.CAPath)
	if err != nil {
		return "", "", nil, err
	}
	creds, err := credentials(ctx, hubURL, config, httpClient)
	if err != nil {
		httpClient.CloseIdleConnections()
		return "", "", nil, err
	}
	httpClient.CloseIdleConnections()
	return hubURL, creds.DeviceToken, httpClient, nil
}

func credentials(ctx context.Context, hubURL string, config Config, httpClient *http.Client) (state, error) {
	statePath := filepath.Join(config.StateDir, "node.json")
	saved, err := readState(statePath)
	if err == nil && saved.DeviceToken != "" {
		if chmodErr := os.Chmod(statePath, 0o600); chmodErr != nil {
			return state{}, fmt.Errorf("secure node state: %w", chmodErr)
		}
		return saved, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return state{}, err
	}
	if config.Code == "" {
		return state{}, fmt.Errorf("invite code required for first join")
	}
	saved, err = exchangeInvite(ctx, hubURL, config.Code, httpClient)
	if err != nil {
		return state{}, err
	}
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return state{}, fmt.Errorf("create node state: %w", err)
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".node-state-*")
	if err != nil {
		return fmt.Errorf("stage node state: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write node state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure node state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close node state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit node state: %w", err)
	}
	committed = true
	return nil
}

func exchangeInvite(ctx context.Context, hubURL, code string, httpClient *http.Client) (state, error) {
	joinCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request := joinRequest{Code: code, OS: runtime.GOOS, Arch: runtime.GOARCH, Shell: shell()}
	response, err := postJSON(joinCtx, httpClient, hubURL+"/v1/join", request)
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

func (r *nodeRunner) maintain(ctx context.Context) error {
	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		healthy, err := r.connectOnce(ctx)
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

func (r *nodeRunner) connectOnce(ctx context.Context) (bool, error) {
	dialCtx, cancel := context.WithTimeout(ctx, frameTimeout)
	header := http.Header{"Authorization": []string{"Bearer " + r.token}}
	conn, response, err := websocket.Dial(dialCtx, r.hubURL+"/v1/node", &websocket.DialOptions{HTTPClient: r.httpClient, HTTPHeader: header})
	cancel()
	if err != nil {
		return false, dialError(response, err)
	}
	session := &sessionConn{conn: conn, runner: r}
	return session.run(ctx)
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

func (s *sessionConn) run(ctx context.Context) (bool, error) {
	s.conn.SetReadLimit(maxFrameBytes)
	sessionCtx, cancel := context.WithCancel(ctx)
	s.runner.jobs.setNotifier(func(frame hubFrame) {
		_ = s.send(sessionCtx, frame)
	})
	readDone := make(chan error, 1)
	go func() {
		defer close(readDone)
		readDone <- s.readFrames(sessionCtx)
	}()
	err := s.writeHeartbeats(sessionCtx, readDone)
	cancel()
	_ = s.conn.CloseNow()
	<-readDone
	s.rpcWG.Wait()
	s.runner.jobs.setNotifier(nil)
	return s.healthy.Load(), err
}

func (s *sessionConn) writeHeartbeats(ctx context.Context, readDone <-chan error) error {
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
		if err := s.send(ctx, hubFrame{Type: "ping"}); err != nil {
			return fmt.Errorf("write hub heartbeat: %w", err)
		}
	}
}

func (s *sessionConn) readFrames(ctx context.Context) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, frameTimeout)
		_, data, err := s.conn.Read(readCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("read hub frame: %w", err)
		}
		s.dispatch(ctx, data)
	}
}

func (s *sessionConn) dispatch(ctx context.Context, data []byte) {
	var frame hubFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return
	}
	switch frame.Type {
	case "connected":
		fmt.Printf("dl connected: name=%s status=alive platform=%s/%s shell=%s expires_at=%s\n",
			frame.Name, frame.OS, frame.Arch, frame.Shell, frame.ExpiresAt)
	case "pong":
		s.healthy.Store(true)
	case "exec_start", "job_status", "job_logs", "job_kill",
		"transfer_scan", "transfer_inspect", "transfer_send", "transfer_recv":
		s.rpcWG.Add(1)
		go func() {
			defer s.rpcWG.Done()
			s.handleRPC(ctx, frame)
		}()
	}
}

func (s *sessionConn) handleRPC(ctx context.Context, frame hubFrame) {
	switch frame.Type {
	case "transfer_scan", "transfer_inspect", "transfer_send", "transfer_recv":
		reply, postSend := s.executeTransferRPC(ctx, frame)
		if s.send(ctx, reply) == nil && reply.Error == "" && postSend != nil {
			postSend()
		}
	default:
		_ = s.send(ctx, s.executeRPC(frame))
	}
}

func (s *sessionConn) executeRPC(frame hubFrame) hubFrame {
	reply := hubFrame{Type: "rpc_reply", ReqID: frame.ReqID, JobID: frame.JobID}
	switch frame.Type {
	case "exec_start":
		setRPCError(&reply, s.runner.jobs.start(frame))
	case "job_status":
		status, err := s.runner.jobs.status(frame.JobID)
		if !setRPCError(&reply, err) {
			reply.State = status.State
			reply.ExitCode = status.ExitCode
			reply.Bytes = status.Bytes
		}
	case "job_logs":
		chunk, eof, err := s.runner.jobs.readLogs(frame.JobID, frame.Offset)
		if !setRPCError(&reply, err) {
			reply.Chunk = chunk
			reply.EOF = eof
		}
	case "job_kill":
		setRPCError(&reply, s.runner.jobs.kill(frame.JobID))
	}
	return reply
}

func setRPCError(reply *hubFrame, err error) bool {
	if err == nil {
		return false
	}
	reply.Error = err.Error()
	if errors.Is(err, os.ErrNotExist) {
		reply.State = "not_found"
	}
	return true
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
