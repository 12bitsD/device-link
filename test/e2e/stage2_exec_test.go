package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type execCLIResult struct {
	stdout   string
	stderr   string
	exitCode int
	err      error
}

type stage2JobStatus struct {
	JobID    string `json:"job_id"`
	Device   string `json:"device"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
	Bytes    int    `json:"bytes"`
}

func TestStage2ExecSyncStdoutStderrExitCodeAndStdinClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands require unix")
	}
	h := newHub(t)
	_ = h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")

	verifyExecStreamsAndExitCode(t, h)
	verifyExecStdinClosedAndMultiArg(t, h)
	verifyExecWorkingDirectory(t, h)
	verifyExecEnvAndLoginPath(t, h)
}

func verifyExecStreamsAndExitCode(t *testing.T, h *hub) {
	t.Helper()
	okRes := runExecCLI(t, h, "mac", "printf 'alpha-out'; printf 'beta-err' >&2")
	assertExecExit(t, okRes, 0)
	if okRes.stdout != "alpha-out" || okRes.stderr != "beta-err" {
		t.Fatalf("streams mismatch: stdout=%q stderr=%q", okRes.stdout, okRes.stderr)
	}

	failRes := runExecCLI(t, h, "mac", "printf 'fail-out'; printf 'fail-err' >&2; exit 17")
	assertExecExit(t, failRes, 17)
	if failRes.stdout != "fail-out" {
		t.Fatalf("failed stdout = %q, want %q", failRes.stdout, "fail-out")
	}
	assertTextContains(t, failRes.stderr, "fail-err")
}

func verifyExecStdinClosedAndMultiArg(t *testing.T, h *hub) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	catArgs := append([]string{"exec"}, h.controllerArgs()...)
	catArgs = append(catArgs, "mac", "cat")
	catRes := runSplitCLI(ctx, t, h.binary, catArgs...)
	assertExecExit(t, catRes, 0)
	if catRes.stdout != "" || catRes.stderr != "" {
		t.Fatalf("cat output = (%q, %q), want empty", catRes.stdout, catRes.stderr)
	}

	directRes := runExecCLI(t, h, "mac", "--", "sh", "-c", `printf '%s:%s' "$1" "$2"`, "sh", "ok", "two words")
	assertExecExit(t, directRes, 0)
	if directRes.stdout != "ok:two words" {
		t.Fatalf("multi-arg stdout = %q, want %q", directRes.stdout, "ok:two words")
	}
}

func verifyExecWorkingDirectory(t *testing.T, h *hub) {
	t.Helper()
	workDir := t.TempDir()
	realWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", workDir, err)
	}
	cwdRes := runExecCLI(t, h, "--cwd", workDir, "mac", "printf 'cwd-data' > marker.txt && pwd")
	assertExecExit(t, cwdRes, 0)
	gotDir := strings.TrimSpace(cwdRes.stdout)
	if gotDir != workDir && gotDir != realWorkDir {
		t.Fatalf("pwd = %q, want %q or %q", gotDir, workDir, realWorkDir)
	}
	markerBytes, err := os.ReadFile(filepath.Join(workDir, "marker.txt"))
	if err != nil || string(markerBytes) != "cwd-data" {
		t.Fatalf("marker file = %q, err = %v", string(markerBytes), err)
	}

	missingDir := filepath.Join(workDir, "does-not-exist")
	badRes := runExecCLI(t, h, "--cwd", missingDir, "mac", "pwd")
	if badRes.exitCode == 0 {
		t.Fatalf("expected non-zero exit for missing --cwd, got stdout=%q stderr=%q", badRes.stdout, badRes.stderr)
	}
}

func verifyExecEnvAndLoginPath(t *testing.T, h *hub) {
	t.Helper()
	_ = joinMinimalPathNode(t, h, "minpath")
	h.waitDevice(t, "minpath", "alive")

	envRes := runExecCLI(t, h, "--env", "FOO=bar", "--env", "BAZ=qux", "minpath", `printf '%s|%s|%s' "$FOO" "$BAZ" "$PATH"`)
	assertExecExit(t, envRes, 0)
	parts := strings.SplitN(envRes.stdout, "|", 3)
	if len(parts) != 3 || parts[0] != "bar" || parts[1] != "qux" {
		t.Fatalf("env output = %q, want bar|qux|<PATH>", envRes.stdout)
	}
	if parts[2] == "" || parts[2] == "/bin" || !strings.Contains(parts[2], "/usr/bin") {
		t.Fatalf("login PATH not loaded: got %q", parts[2])
	}

	overrideRes := runExecCLI(t, h, "--env", "PATH=/custom-path:/bin", "minpath", `printf '%s' "$PATH"`)
	assertExecExit(t, overrideRes, 0)
	if overrideRes.stdout != "/custom-path:/bin" {
		t.Fatalf("overridden PATH = %q, want /custom-path:/bin", overrideRes.stdout)
	}
}

func TestStage2ExecDetachWaitExceedAndJobStatusLogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands require unix")
	}
	h := newHub(t)
	_ = h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")

	verifyDetachedJobLifecycle(t, h)
	verifyWaitExceededReturnsJobID(t, h)
}

func verifyDetachedJobLifecycle(t *testing.T, h *hub) {
	t.Helper()
	detachRes := runExecCLI(t, h, "--detach", "mac", "printf 'detach-start\n'; sleep 1; printf 'detach-end\n'")
	assertExecExit(t, detachRes, 0)
	jobID := parseDetachedJobID(t, detachRes.stdout)

	runningText := runJobCLI(t, h, "status", jobID)
	assertExecExit(t, runningText, 0)
	assertTextContains(t, runningText.stdout, "job_id="+jobID)
	assertTextContains(t, runningText.stdout, "device=mac")
	assertTextContains(t, runningText.stdout, "state=running")
	assertTextContains(t, runningText.stdout, "exit_code=none")

	runningJSON := readJobStatusJSON(t, h, "--json", jobID)
	if runningJSON.State != "running" || runningJSON.ExitCode != nil {
		t.Fatalf("expected running json status with nil exit_code, got %+v", runningJSON)
	}

	wantLogs := "detach-start\ndetach-end\n"
	doneJSON := waitJobState(t, h, jobID, "completed")
	if doneJSON.ExitCode == nil || *doneJSON.ExitCode != 0 || doneJSON.Bytes != len(wantLogs) {
		t.Fatalf("unexpected completed json status: %+v", doneJSON)
	}
	assertCompletedStatusAndLogs(t, h, jobID, wantLogs)
}

func assertCompletedStatusAndLogs(t *testing.T, h *hub, jobID, wantLogs string) {
	t.Helper()
	doneText := runJobCLI(t, h, "status", jobID)
	assertExecExit(t, doneText, 0)
	assertTextContains(t, doneText.stdout, "state=completed")
	assertTextContains(t, doneText.stdout, "exit_code=0")
	assertTextContains(t, doneText.stdout, fmt.Sprintf("bytes=%d", len(wantLogs)))

	logsRes := runJobCLI(t, h, "logs", jobID)
	assertExecExit(t, logsRes, 0)
	if logsRes.stdout != wantLogs {
		t.Fatalf("job logs = %q, want %q", logsRes.stdout, wantLogs)
	}
}

func verifyWaitExceededReturnsJobID(t *testing.T, h *hub) {
	t.Helper()
	start := time.Now()
	waitRes := runExecCLI(t, h, "--wait", "150ms", "mac", "printf 'wait-one\n'; sleep 1; printf 'wait-two\n'")
	elapsed := time.Since(start)
	assertExecExit(t, waitRes, 0)
	if elapsed >= 900*time.Millisecond {
		t.Fatalf("--wait 150ms blocked for %s, expected early return", elapsed)
	}
	jobID := parseDetachedJobID(t, waitRes.stdout)
	wantLogs := "wait-one\nwait-two\n"
	done := waitJobState(t, h, jobID, "completed")
	if done.ExitCode == nil || *done.ExitCode != 0 {
		t.Fatalf("wait-exceeded job status = %+v, want exit_code=0", done)
	}
	logsRes := runJobCLI(t, h, "logs", jobID)
	assertExecExit(t, logsRes, 0)
	if logsRes.stdout != wantLogs {
		t.Fatalf("wait-exceeded logs = %q, want %q", logsRes.stdout, wantLogs)
	}
}

func TestStage2OfflineContinuationLogResumeAndWaitOnline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands require unix")
	}
	h := newHub(t)
	proxy := newTCPProxy(t, strings.TrimPrefix(h.url, "https://"))
	t.Cleanup(func() { _ = proxy.Resume() })
	_ = joinNodeViaProxy(t, h, proxy, "mac")
	h.waitDevice(t, "mac", "alive")

	verifyOfflineContinuationAndLogResume(t, h, proxy)
	verifyExecOfflineAndWaitOnline(t, h, proxy)
}

const (
	offlineChunk1 = "chunk-1-online\n"
	offlineChunk2 = "chunk-2-offline\n"
)

func verifyOfflineContinuationAndLogResume(t *testing.T, h *hub, proxy *tcpProxy) {
	t.Helper()
	doneFile := filepath.Join(t.TempDir(), "offline.done")
	script := fmt.Sprintf("printf %q; sleep 1; printf %q; printf done > %q", offlineChunk1, offlineChunk2, doneFile)
	detachRes := runExecCLI(t, h, "--detach", "mac", "--", "sh", "-c", script)
	assertExecExit(t, detachRes, 0)
	jobID := parseDetachedJobID(t, detachRes.stdout)

	waitJobLogsExact(t, h, jobID, offlineChunk1)
	if err := proxy.Drop(); err != nil {
		t.Fatalf("proxy drop: %v", err)
	}
	h.waitDevice(t, "mac", "missed")
	waitFileText(t, doneFile, "done")

	if err := proxy.Resume(); err != nil {
		t.Fatalf("proxy resume: %v", err)
	}
	h.waitDevice(t, "mac", "alive")
	_ = waitJobState(t, h, jobID, "completed")
	assertOfflineResumedLogs(t, h, jobID)
}

func assertOfflineResumedLogs(t *testing.T, h *hub, jobID string) {
	t.Helper()
	offset := len(offlineChunk1)
	tailRes := runJobCLI(t, h, "logs", jobID, "--offset", strconv.Itoa(offset))
	assertExecExit(t, tailRes, 0)
	if tailRes.stdout != offlineChunk2 {
		t.Fatalf("logs --offset %d = %q, want %q", offset, tailRes.stdout, offlineChunk2)
	}

	wantFull := offlineChunk1 + offlineChunk2
	fullRes := runJobCLI(t, h, "logs", "--offset", "0", jobID)
	assertExecExit(t, fullRes, 0)
	if fullRes.stdout != wantFull {
		t.Fatalf("logs --offset 0 = %q, want %q", fullRes.stdout, wantFull)
	}
}

func verifyExecOfflineAndWaitOnline(t *testing.T, h *hub, proxy *tcpProxy) {
	t.Helper()
	if err := proxy.Drop(); err != nil {
		t.Fatalf("proxy drop: %v", err)
	}
	h.waitDevice(t, "mac", "missed")

	start := time.Now()
	offlineRes := runExecCLI(t, h, "mac", "printf unreachable")
	if offlineRes.exitCode == 0 || time.Since(start) >= 2*time.Second {
		t.Fatalf("offline exec should fail immediately: code=%d elapsed=%s stderr=%q", offlineRes.exitCode, time.Since(start), offlineRes.stderr)
	}
	lowerErr := strings.ToLower(offlineRes.stderr + offlineRes.stdout)
	assertTextContains(t, lowerErr, "offline")
	if strings.Contains(lowerErr, "not found") {
		t.Fatalf("offline device reported not found: %q", lowerErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resumeDone := resumeProxyAfter(ctx, proxy, 300*time.Millisecond)
	onlineRes := runExecCLI(t, h, "--wait-online", "4s", "mac", "printf reconnected-ok")
	if resumeErr := <-resumeDone; resumeErr != nil {
		t.Fatalf("delayed proxy resume: %v", resumeErr)
	}
	assertExecExit(t, onlineRes, 0)
	if onlineRes.stdout != "reconnected-ok" {
		t.Fatalf("wait-online stdout = %q, want reconnected-ok", onlineRes.stdout)
	}
	h.waitDevice(t, "mac", "alive")
}

func TestStage2TimeoutAndKillTerminateProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands require unix")
	}
	h := newHub(t)
	_ = h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")

	verifyTimeoutKillsProcessTree(t, h)
	verifyJobKillTerminatesProcessTree(t, h)
}

func verifyTimeoutKillsProcessTree(t *testing.T, h *hub) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "timeout-child.pid")
	script := fmt.Sprintf("sleep 30 & echo $! > %q; wait", pidFile)
	res := runExecCLI(t, h, "--timeout", "400ms", "mac", "--", "sh", "-c", script)
	assertExecExit(t, res, 124)
	assertTextContains(t, res.stderr, "timed out")

	jobID := parseTimedOutJobID(t, res.stderr)
	status := waitJobState(t, h, jobID, "timed_out")
	if status.JobID != jobID {
		t.Fatalf("status job_id = %q, want %q", status.JobID, jobID)
	}
	pid := waitPIDFile(t, pidFile)
	waitPIDDead(t, pid)
}

func verifyJobKillTerminatesProcessTree(t *testing.T, h *hub) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "kill-child.pid")
	script := fmt.Sprintf("sleep 30 & echo $! > %q; wait", pidFile)
	detachRes := runExecCLI(t, h, "--detach", "mac", "--", "sh", "-c", script)
	assertExecExit(t, detachRes, 0)
	jobID := parseDetachedJobID(t, detachRes.stdout)

	pid := waitPIDFile(t, pidFile)
	killRes := runJobCLI(t, h, "kill", jobID)
	assertExecExit(t, killRes, 0)

	_ = waitJobState(t, h, jobID, "killed")
	waitPIDDead(t, pid)
}

func TestStage2RoleIsolationAndUnknownErrors(t *testing.T) {
	h := newHub(t)
	n := h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")
	devToken := n.deviceToken(t)

	detachRes := runExecCLI(t, h, "--detach", "mac", "echo role-check")
	assertExecExit(t, detachRes, 0)
	jobID := parseDetachedJobID(t, detachRes.stdout)
	_ = waitJobState(t, h, jobID, "completed")

	verifyDeviceTokenRejectedOnExecAndJob(t, h, devToken, jobID)
	verifyUnknownDeviceAndJobErrors(t, h)
}

func verifyDeviceTokenRejectedOnExecAndJob(t *testing.T, h *hub, devToken, jobID string) {
	t.Helper()
	nodeAuthArgs := replaceFlag(h.controllerArgs(), "--token", devToken)

	execArgs := append([]string{"exec"}, nodeAuthArgs...)
	execArgs = append(execArgs, "mac", "echo forbidden")
	assertExecUnauthorized(t, runDefaultSplitCLI(t, h.binary, execArgs...))

	for _, subcmd := range []string{"status", "logs", "kill"} {
		jobArgs := append([]string{"job", subcmd}, nodeAuthArgs...)
		jobArgs = append(jobArgs, jobID)
		assertExecUnauthorized(t, runDefaultSplitCLI(t, h.binary, jobArgs...))
	}
}

func verifyUnknownDeviceAndJobErrors(t *testing.T, h *hub) {
	t.Helper()
	unknownDev := runExecCLI(t, h, "no-such-device", "echo nope")
	assertExecNotFound(t, unknownDev)

	for _, subcmd := range []string{"status", "logs", "kill"} {
		unknownJob := runJobCLI(t, h, subcmd, "no-such-job-id")
		assertExecNotFound(t, unknownJob)
	}
}

func runDefaultSplitCLI(t *testing.T, binary string, args ...string) execCLIResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	return runSplitCLI(ctx, t, binary, args...)
}

func runSplitCLI(ctx context.Context, t *testing.T, binary string, args ...string) execCLIResult {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = processEnvironment(t)
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		code = exitCodeFromErr(err)
	}
	return execCLIResult{
		stdout:   stdoutBuf.String(),
		stderr:   stderrBuf.String(),
		exitCode: code,
		err:      err,
	}
}

func exitCodeFromErr(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func runExecCLI(t *testing.T, h *hub, args ...string) execCLIResult {
	t.Helper()
	allArgs := append([]string{"exec"}, h.controllerArgs()...)
	allArgs = append(allArgs, args...)
	return runDefaultSplitCLI(t, h.binary, allArgs...)
}

func runJobCLI(t *testing.T, h *hub, subcmd string, args ...string) execCLIResult {
	t.Helper()
	allArgs := append([]string{"job", subcmd}, h.controllerArgs()...)
	allArgs = append(allArgs, args...)
	return runDefaultSplitCLI(t, h.binary, allArgs...)
}

func assertExecExit(t *testing.T, res execCLIResult, want int) {
	t.Helper()
	if res.exitCode != want {
		t.Fatalf("exit code = %d (err=%v), want %d\nstdout: %s\nstderr: %s", res.exitCode, res.err, want, res.stdout, res.stderr)
	}
}

func assertTextContains(t *testing.T, got, wantSub string) {
	t.Helper()
	if !strings.Contains(got, wantSub) {
		t.Fatalf("output %q does not contain %q", got, wantSub)
	}
}

func assertExecUnauthorized(t *testing.T, res execCLIResult) {
	t.Helper()
	if res.exitCode == 0 {
		t.Fatalf("expected unauthorized failure, got exit 0: stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	lower := strings.ToLower(res.stdout + "\n" + res.stderr)
	if strings.Contains(lower, "unauthorized") || strings.Contains(lower, "forbidden") {
		return
	}
	if strings.Contains(lower, "401") || strings.Contains(lower, "403") {
		return
	}
	t.Fatalf("expected unauthorized/401/403 error, got stdout=%q stderr=%q", res.stdout, res.stderr)
}

func assertExecNotFound(t *testing.T, res execCLIResult) {
	t.Helper()
	if res.exitCode == 0 {
		t.Fatalf("expected not-found failure, got exit 0: stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	lower := strings.ToLower(res.stdout + "\n" + res.stderr)
	if strings.Contains(lower, "offline") {
		t.Fatalf("expected not found, got offline error: %q", lower)
	}
	assertTextContains(t, lower, "not found")
}

func parseDetachedJobID(t *testing.T, stdout string) string {
	t.Helper()
	trimmed := strings.TrimSpace(stdout)
	if strings.Contains(trimmed, "\n") {
		t.Fatalf("expected single-line job_id output, got %q", stdout)
	}
	jobID, ok := strings.CutPrefix(trimmed, "job_id=")
	if !ok || jobID == "" {
		t.Fatalf("expected job_id=<id>, got %q", stdout)
	}
	return jobID
}

func parseTimedOutJobID(t *testing.T, stderr string) string {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "dl: job ")
		if !ok {
			continue
		}
		jobID, ok := strings.CutSuffix(rest, " timed out")
		if ok && jobID != "" && !strings.Contains(jobID, " ") {
			return jobID
		}
	}
	t.Fatalf("expected 'dl: job <job_id> timed out' in stderr, got %q", stderr)
	return ""
}

func readJobStatusJSON(t *testing.T, h *hub, args ...string) stage2JobStatus {
	t.Helper()
	res := runJobCLI(t, h, "status", args...)
	assertExecExit(t, res, 0)
	var status stage2JobStatus
	if err := json.Unmarshal([]byte(res.stdout), &status); err != nil {
		t.Fatalf("decode job status %q: %v", res.stdout, err)
	}
	return status
}

func waitJobState(t *testing.T, h *hub, jobID, wantState string) stage2JobStatus {
	t.Helper()
	var current stage2JobStatus
	waitFor(t, statusTimeout, func() (bool, error) {
		res := runJobCLI(t, h, "status", jobID, "--json")
		if res.exitCode != 0 {
			return false, fmt.Errorf("status exit %d: %s", res.exitCode, res.stderr)
		}
		if err := json.Unmarshal([]byte(res.stdout), &current); err != nil {
			return false, fmt.Errorf("decode status: %w", err)
		}
		if current.State != wantState {
			return false, fmt.Errorf("state = %s, want %s", current.State, wantState)
		}
		return true, nil
	})
	return current
}

func waitJobLogsExact(t *testing.T, h *hub, jobID, want string) {
	t.Helper()
	waitFor(t, statusTimeout, func() (bool, error) {
		res := runJobCLI(t, h, "logs", jobID)
		if res.exitCode != 0 {
			return false, fmt.Errorf("logs exit %d: %s", res.exitCode, res.stderr)
		}
		if res.stdout != want {
			return false, fmt.Errorf("logs = %q, want %q", res.stdout, want)
		}
		return true, nil
	})
}

func waitFileText(t *testing.T, path, want string) {
	t.Helper()
	waitFor(t, statusTimeout, func() (bool, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		if string(data) != want {
			return false, fmt.Errorf("file %s = %q, want %q", path, string(data), want)
		}
		return true, nil
	})
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, statusTimeout, func() (bool, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		raw := string(data)
		if !strings.HasSuffix(raw, "\n") {
			return false, errors.New("pid file write incomplete")
		}
		parsed, parseErr := strconv.Atoi(strings.TrimSpace(raw))
		if parseErr != nil || parsed <= 0 {
			return false, fmt.Errorf("invalid pid %q: %w", raw, parseErr)
		}
		pid = parsed
		return true, nil
	})
	return pid
}

func waitPIDDead(t *testing.T, pid int) {
	t.Helper()
	waitFor(t, statusTimeout, func() (bool, error) {
		if isPIDDead(pid) {
			return true, nil
		}
		return false, fmt.Errorf("pid %d is still alive", pid)
	})
}

func isPIDDead(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	sigErr := proc.Signal(syscall.Signal(0))
	return errors.Is(sigErr, os.ErrProcessDone)
}

func resumeProxyAfter(ctx context.Context, proxy *tcpProxy, delay time.Duration) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			done <- nil
		case <-timer.C:
			done <- proxy.Resume()
		}
	}()
	return done
}

func joinNodeViaProxy(t *testing.T, h *hub, proxy *tcpProxy, name string) *node {
	t.Helper()
	proxyURL := "https://" + proxy.address
	joinArgs := replaceFlag(h.invite(t, name), "--hub", proxyURL)
	stateDir := t.TempDir()
	joinArgs = append(joinArgs, "--state-dir", stateDir)
	proc := startProcess(t, h.binary, joinArgs...)
	return &node{binary: h.binary, hubURL: proxyURL, caPath: h.caPath, stateDir: stateDir, process: proc}
}

func joinMinimalPathNode(t *testing.T, h *hub, name string) *node {
	t.Helper()
	joinArgs := h.invite(t, name)
	stateDir := t.TempDir()
	joinArgs = append(joinArgs, "--state-dir", stateDir)
	env := replaceEnvEntry(processEnvironment(t), "PATH", "/bin")
	proc := startProcessWithEnv(t, h.binary, env, joinArgs...)
	return &node{binary: h.binary, hubURL: h.url, caPath: h.caPath, stateDir: stateDir, process: proc}
}

func replaceEnvEntry(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		entryKey, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(entryKey, key) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, key+"="+value)
}

func startProcessWithEnv(t *testing.T, binary string, env []string, args ...string) *childProcess {
	t.Helper()
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
