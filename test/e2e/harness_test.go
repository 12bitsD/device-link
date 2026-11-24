package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	binaryEnv       = "DL_E2E_BINARY"
	commandTimeout  = 15 * time.Second
	readinessTimout = 10 * time.Second
	statusTimeout   = 6 * time.Second
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type childProcess struct {
	cmd      *exec.Cmd
	logs     *lockedBuffer
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	waitErr  error
}

func TestMain(m *testing.M) {
	tempDir, err := os.MkdirTemp("", "device-link-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary := filepath.Join(tempDir, "dl")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	code := buildAndRunTests(m, tempDir, binary)
	_ = os.RemoveAll(tempDir)
	os.Exit(code)
}

func buildAndRunTests(m *testing.M, tempDir, binary string) int {
	workDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	root := filepath.Clean(filepath.Join(workDir, "..", ".."))
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/dl")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build dl: %v\n%s", err, output)
		return 1
	}
	if err := os.Setenv(binaryEnv, binary); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

type hub struct {
	binary   string
	url      string
	stateDir string
	token    string
	caPath   string
	process  *childProcess
}

type node struct {
	binary   string
	hubURL   string
	caPath   string
	stateDir string
	process  *childProcess
}

type device struct {
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	OS        string  `json:"os"`
	Arch      string  `json:"arch"`
	Shell     string  `json:"shell"`
	ExpiresAt *string `json:"expires_at"`
	LastSeen  string  `json:"last_seen"`
}

func newHub(t *testing.T) *hub {
	t.Helper()
	binary := os.Getenv(binaryEnv)
	if binary == "" {
		t.Fatal("DL_E2E_BINARY is not set")
	}
	stateDir := t.TempDir()
	addr := freeAddress(t)
	process := startProcess(t, binary, "hub", "--addr", addr, "--state-dir", stateDir)
	h := &hub{
		binary:   binary,
		url:      "https://" + addr,
		stateDir: stateDir,
		caPath:   filepath.Join(stateDir, "cert.pem"),
		process:  process,
	}
	waitHubReady(t, h)
	return h
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func startProcess(t *testing.T, binary string, args ...string) *childProcess {
	t.Helper()
	cmd := exec.Command(binary, args...)
	logs := &lockedBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	cmd.Env = cleanEnvironment()
	prepareProcess(cmd)
	p := &childProcess{cmd: cmd, logs: logs, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %v: %v", binary, args, err)
	}
	go p.wait()
	t.Cleanup(p.stop)
	return p
}

func cleanEnvironment() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "DL_TOKEN=") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func (p *childProcess) wait() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waitErr = err
	p.mu.Unlock()
	close(p.done)
}

func (p *childProcess) stop() {
	p.stopOnce.Do(func() {
		_ = killProcessTree(p.cmd)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
}

func (p *childProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *childProcess) output() string {
	return p.logs.String()
}

func (p *childProcess) exitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func waitHubReady(t *testing.T, h *hub) {
	t.Helper()
	deadline := time.Now().Add(readinessTimout)
	for time.Now().Before(deadline) {
		if h.process.exited() {
			t.Fatalf("hub exited during startup: %v\n%s", h.process.exitError(), h.process.output())
		}
		if h.readTokenAndProbe() {
			return
		}
		// bits:allow(E2E_SLEEP): central readiness poll interval
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("hub did not become ready\n%s", h.process.output())
}

func (h *hub) readTokenAndProbe() bool {
	tokenBytes, err := os.ReadFile(filepath.Join(h.stateDir, "controller_token"))
	if err != nil {
		return false
	}
	if _, err := os.Stat(h.caPath); err != nil {
		return false
	}
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	config := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	conn, err := tls.DialWithDialer(dialer, "tcp", strings.TrimPrefix(h.url, "https://"), config)
	if err != nil {
		return false
	}
	_ = conn.Close()
	h.token = strings.TrimSpace(string(tokenBytes))
	return h.token != ""
}

func runCLI(t *testing.T, binary string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = cleanEnvironment()
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(output), fmt.Errorf("command timed out: %w", ctx.Err())
	}
	if err != nil {
		return string(output), fmt.Errorf("command failed: %w", err)
	}
	return string(output), nil
}

func (h *hub) controllerArgs() []string {
	return []string{"--hub", h.url, "--token", h.token, "--ca", h.caPath}
}

func (h *hub) runController(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if len(args) == 0 {
		t.Fatal("controller command is required")
	}
	allArgs := []string{args[0]}
	allArgs = append(allArgs, h.controllerArgs()...)
	allArgs = append(allArgs, args[1:]...)
	return runCLI(t, h.binary, allArgs...)
}

func (h *hub) devices(t *testing.T) []device {
	t.Helper()
	output, err := h.runController(t, "ls", "--json")
	if err != nil {
		t.Fatalf("ls: %v\n%s", err, output)
	}
	var devices []device
	if err := json.Unmarshal([]byte(output), &devices); err != nil {
		t.Fatalf("decode ls output %q: %v", output, err)
	}
	return devices
}

func (h *hub) invite(t *testing.T, name string) []string {
	t.Helper()
	output, err := h.runController(t, "invite", "--name", name)
	if err != nil {
		t.Fatalf("invite %s: %v\n%s", name, err, output)
	}
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) != 8 || fields[0] != "dl" || fields[1] != "join" {
		t.Fatalf("invite output is not one dl join command: %q", output)
	}
	joinArgs := fields[1:]
	validateJoinArgs(t, joinArgs)
	return joinArgs
}

func validateJoinArgs(t *testing.T, args []string) {
	t.Helper()
	values := make(map[string]string)
	for i := 1; i+1 < len(args); i += 2 {
		values[args[i]] = args[i+1]
	}
	if values["--code"] == "" || values["--hub"] == "" || values["--ca"] == "" {
		t.Fatalf("join command lacks hub, code, or ca: %v", args)
	}
}

func (h *hub) joinWithInvite(t *testing.T, name string) *node {
	t.Helper()
	joinArgs := h.invite(t, name)
	stateDir := t.TempDir()
	joinArgs = append(joinArgs, "--state-dir", stateDir)
	process := startProcess(t, h.binary, joinArgs...)
	return &node{binary: h.binary, hubURL: h.url, caPath: h.caPath, stateDir: stateDir, process: process}
}

func (h *hub) joinAgain(t *testing.T, joinArgs []string) *node {
	t.Helper()
	args := append([]string{}, joinArgs...)
	stateDir := t.TempDir()
	args = append(args, "--state-dir", stateDir)
	process := startProcess(t, h.binary, args...)
	return &node{binary: h.binary, hubURL: h.url, caPath: h.caPath, stateDir: stateDir, process: process}
}

func (h *hub) restartNode(t *testing.T, n *node) *node {
	t.Helper()
	args := []string{"join", "--hub", n.hubURL, "--ca", n.caPath, "--state-dir", n.stateDir}
	process := startProcess(t, h.binary, args...)
	return &node{binary: h.binary, hubURL: n.hubURL, caPath: n.caPath, stateDir: n.stateDir, process: process}
}

func (n *node) deviceToken(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(n.stateDir, "node.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		DeviceToken string `json:"device_token"`
	}
	if err := json.Unmarshal(contents, &state); err != nil {
		t.Fatal(err)
	}
	if state.DeviceToken == "" {
		t.Fatalf("empty device token in %s", contents)
	}
	return state.DeviceToken
}

func waitFor(t *testing.T, timeout time.Duration, probe func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ready, err := probe()
		if err == nil && ready {
			return
		}
		lastErr = err
		// bits:allow(E2E_SLEEP): central condition poll interval
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %v", timeout, lastErr)
}

func (h *hub) waitDevice(t *testing.T, name, status string) device {
	t.Helper()
	var found device
	waitFor(t, statusTimeout, func() (bool, error) {
		for _, candidate := range h.devices(t) {
			if candidate.Name == name && candidate.Status == status {
				found = candidate
				return true, nil
			}
		}
		return false, nil
	})
	return found
}

func requireDevice(t *testing.T, devices []device, name, status string) device {
	t.Helper()
	for _, candidate := range devices {
		if candidate.Name == name && candidate.Status == status {
			return candidate
		}
	}
	t.Fatalf("device %s with status %s not found in %+v", name, status, devices)
	return device{}
}

func requireAbsent(t *testing.T, devices []device, name string) {
	t.Helper()
	for _, candidate := range devices {
		if candidate.Name == name {
			t.Fatalf("device %s still present in %+v", name, devices)
		}
	}
}

func waitProcessExit(t *testing.T, p *childProcess) {
	t.Helper()
	waitProcessExitWithin(t, p, commandTimeout)
}

func waitProcessExitWithin(t *testing.T, p *childProcess, timeout time.Duration) {
	t.Helper()
	select {
	case <-p.done:
		return
	case <-time.After(timeout):
		t.Fatalf("process did not exit within %s\n%s", timeout, p.output())
	}
}

func requireNonZeroProcessExit(t *testing.T, p *childProcess, timeout time.Duration) {
	t.Helper()
	waitProcessExitWithin(t, p, timeout)
	if p.exitError() == nil {
		t.Fatalf("process exited with code 0, want non-zero\n%s", p.output())
	}
}

func requireCleanProcessExit(t *testing.T, p *childProcess, timeout time.Duration) {
	t.Helper()
	waitProcessExitWithin(t, p, timeout)
	if err := p.exitError(); err != nil {
		t.Fatalf("process exit = %v, want code 0\n%s", err, p.output())
	}
}

func requireFailure(t *testing.T, output string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("command unexpectedly succeeded: %s", output)
	}
}
