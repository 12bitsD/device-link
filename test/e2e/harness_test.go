package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	binaryEnv         = "DL_E2E_BINARY"
	tempRootEnv       = "DL_E2E_TMPROOT"
	commandTimeout    = 15 * time.Second
	readinessTimout   = 10 * time.Second
	statusTimeout     = 6 * time.Second
	gracefulStopLimit = 1500 * time.Millisecond
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
	stdout   *lockedBuffer
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
	if os.Getenv(binaryEnv) == "" {
		if err := buildDLBinary(binary); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.Setenv(binaryEnv, binary); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	procTmpRoot := filepath.Join(tempDir, "tmp")
	if err := os.MkdirAll(procTmpRoot, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.Setenv(tempRootEnv, procTmpRoot); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

func buildDLBinary(binary string) error {
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	root := filepath.Clean(filepath.Join(workDir, "..", ".."))
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/dl")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build dl: %w\n%s", err, output)
	}
	return nil
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
	env := processEnvironment(t)
	if hubStateDir := hubStateDirArg(args); hubStateDir != "" {
		t.Cleanup(func() { assertHubStateDirClean(t, hubStateDir) })
	}
	cmd := exec.Command(binary, args...)
	logs := &lockedBuffer{}
	stdout := &lockedBuffer{}
	cmd.Stdout = io.MultiWriter(stdout, logs)
	cmd.Stderr = logs
	cmd.Env = env
	prepareProcess(cmd)
	p := &childProcess{cmd: cmd, logs: logs, stdout: stdout, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %v: %v", binary, args, err)
	}
	go p.wait()
	t.Cleanup(func() { assertProcessStopped(t, p) })
	t.Cleanup(p.stop)
	return p
}

func cleanEnvironment() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if isScrubbedEnv(entry) {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func isScrubbedEnv(entry string) bool {
	key, _, _ := strings.Cut(entry, "=")
	return strings.EqualFold(key, "DL_TOKEN") ||
		strings.EqualFold(key, "TMPDIR") ||
		strings.EqualFold(key, "TMP") ||
		strings.EqualFold(key, "TEMP")
}

func processEnvironment(t *testing.T) []string {
	t.Helper()
	tmpDir := processTempDir(t)
	env := cleanEnvironment()
	return append(env, "TMPDIR="+tmpDir, "TMP="+tmpDir, "TEMP="+tmpDir)
}

func processTempDir(t *testing.T) string {
	t.Helper()
	root := os.Getenv(tempRootEnv)
	if root == "" {
		t.Fatalf("%s is not set", tempRootEnv)
	}
	sum := sha256.Sum256([]byte(t.Name()))
	dir := filepath.Join(root, fmt.Sprintf("%x", sum[:8]))
	err := os.Mkdir(dir, 0o700)
	if err == nil {
		t.Cleanup(func() {
			assertEmptyTempDir(t, dir)
			_ = os.RemoveAll(dir)
		})
		return dir
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("create process temp dir %s: %v", dir, err)
	}
	return dir
}

func assertEmptyTempDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Errorf("read process temp dir %s: %v", dir, err)
		return
	}
	for _, entry := range entries {
		t.Errorf("leaked temp entry in %s: %s", dir, entry.Name())
	}
}

func hubStateDirArg(args []string) string {
	if len(args) == 0 || args[0] != "hub" {
		return ""
	}
	for i := 1; i < len(args); i++ {
		if after, ok := strings.CutPrefix(args[i], "--state-dir="); ok {
			return after
		}
		if args[i] == "--state-dir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func assertHubStateDirClean(t *testing.T, stateDir string) {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Errorf("read hub state dir %s: %v", stateDir, err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !isAllowedHubStateFile(entry.Name()) {
			t.Errorf("unexpected entry in hub state dir %s: %s", stateDir, entry.Name())
		}
	}
}

func isAllowedHubStateFile(name string) bool {
	switch name {
	case "cert.pem", "key.pem", "controller_token",
		"hub.db", "hub.db-wal", "hub.db-shm", "hub.db-journal":
		return true
	default:
		return false
	}
}

func assertProcessStopped(t *testing.T, p *childProcess) {
	t.Helper()
	if !p.exited() {
		t.Errorf("child process did not exit\n%s", p.output())
		return
	}
	if err := waitProcessGroupDead(p.cmd, 500*time.Millisecond); err != nil {
		t.Errorf("leaked process group: %v", err)
	}
}

func (p *childProcess) wait() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waitErr = err
	p.mu.Unlock()
	close(p.done)
}

func (p *childProcess) stop() {
	p.stopOnce.Do(p.stopProcess)
}

func (p *childProcess) stopProcess() {
	if p.exited() {
		return
	}
	_ = terminateProcessTree(p.cmd)
	if p.waitDone(gracefulStopLimit) {
		return
	}
	_ = killProcessTree(p.cmd)
	if p.waitDone(5 * time.Second) {
		return
	}
	_ = p.cmd.Process.Kill()
	<-p.done
}

func (p *childProcess) waitDone(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
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

func (p *childProcess) stdoutOutput() string {
	return p.stdout.String()
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
	cmd.Env = processEnvironment(t)
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
	fields, parseErr := splitCommandFields(output)
	if parseErr != nil || len(fields) != 8 || fields[0] != "dl" || fields[1] != "join" {
		t.Fatalf("invite output is not one dl join command (%v): %q", parseErr, output)
	}
	joinArgs := fields[1:]
	validateJoinArgs(t, joinArgs)
	return joinArgs
}

func splitCommandFields(line string) ([]string, error) {
	trimmed := strings.TrimSpace(line)
	if strings.ContainsRune(trimmed, '\n') {
		return nil, fmt.Errorf("unexpected multiline command: %q", line)
	}
	var args []string
	for rest := trimmed; rest != ""; {
		arg, next, err := nextCommandField(rest)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		rest = strings.TrimLeft(next, " \t\r")
	}
	return args, nil
}

func nextCommandField(raw string) (string, string, error) {
	var b strings.Builder
	input := raw
	for i := 0; i < len(input); {
		ch := input[i]
		if strings.ContainsRune(" \t\r", rune(ch)) {
			return b.String(), input[i:], nil
		}
		if ch == '\'' {
			seg, next, err := readSingleQuoted(input[i+1:])
			if err != nil {
				return "", "", err
			}
			b.WriteString(seg)
			input = next
			i = 0
			continue
		}
		if ch == '"' {
			seg, next, err := readWindowsQuoted(input[i+1:])
			if err != nil {
				return "", "", err
			}
			b.WriteString(seg)
			input = next
			i = 0
			continue
		}
		b.WriteByte(ch)
		i++
	}
	return b.String(), "", nil
}

func readSingleQuoted(input string) (string, string, error) {
	end := strings.IndexByte(input, '\'')
	if end < 0 {
		return "", "", errors.New("unterminated single quote")
	}
	return input[:end], input[end+1:], nil
}

func readWindowsQuoted(input string) (string, string, error) {
	var b strings.Builder
	slashes := 0
	for i := 0; i < len(input); i++ {
		if input[i] == '\\' {
			slashes++
			continue
		}
		if input[i] == '"' {
			b.WriteString(strings.Repeat(`\`, slashes/2))
			if slashes%2 == 0 {
				return b.String(), input[i+1:], nil
			}
			b.WriteByte('"')
			slashes = 0
			continue
		}
		b.WriteString(strings.Repeat(`\`, slashes))
		b.WriteByte(input[i])
		slashes = 0
	}
	return "", "", errors.New("unterminated double quote")
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
