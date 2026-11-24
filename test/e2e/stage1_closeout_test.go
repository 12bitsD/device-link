package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStage1JoinConnectedStatusLine(t *testing.T) {
	h := newHub(t)

	first := h.joinWithInvite(t, "mac")
	firstDev := h.waitDevice(t, "mac", "alive")
	if firstDev.ExpiresAt != nil {
		t.Fatalf("default invite expires_at = %v, want nil", *firstDev.ExpiresAt)
	}
	waitConnectedLineCount(t, first.process, expectedConnectedLine(firstDev), 1)

	second := h.joinWithInvite(t, "mac")
	secondDev := h.waitDevice(t, "mac-2", "alive")
	waitConnectedLineCount(t, second.process, expectedConnectedLine(secondDev), 1)

	verifyTTLConnectedLine(t, h, "ttl-mac", "1h")
	verifyProxyReconnectStatusLine(t, h, "proxy-mac")

	first.process.stop()
	restarted := h.restartNode(t, first)
	restartedDev := h.waitDevice(t, "mac", "alive")
	waitConnectedLineCount(t, restarted.process, expectedConnectedLine(restartedDev), 1)
}

func TestStage1LSHumanTableIncludesShell(t *testing.T) {
	h := newHub(t)
	h.joinWithInvite(t, "workstation")
	dev := h.waitDevice(t, "workstation", "alive")
	if dev.Shell == "" {
		t.Fatalf("expected non-empty shell in ls --json: %+v", dev)
	}
	assertLSHumanTableRow(t, mustController(t, h, "ls"), dev)
}

func TestStage1DecoupledReadAndBackoffReset(t *testing.T) {
	verifyDecoupledKickExit(t)
	verifyHealthySessionResetsBackoff(t)
}

func TestStage1LinuxContainerJoinAndLS(t *testing.T) {
	dockerBin, linuxArch := requireLinuxContainerEnv(t)
	stageDir := newContainerStageDir(t)
	buildLinuxDLBinary(t, stageDir, linuxArch)

	h, containerHubURL := startContainerTestHub(t)
	stageContainerCA(t, h.caPath, stageDir)
	code := mintContainerInviteCode(t, h)

	containerName := fmt.Sprintf("dl-e2e-linux-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { removeDockerContainer(dockerBin, containerName) })
	containerProc := startProcess(
		t,
		dockerBin,
		"run", "--rm",
		"--name", containerName,
		"--add-host=host.docker.internal:host-gateway",
		"--tmpfs", "/tmp:rw,exec,size=32m",
		"-v", stageDir+":/work:ro",
		"node:24-alpine",
		"/work/dl", "join",
		"--hub", containerHubURL,
		"--code", code,
		"--ca", "/work/cert.pem",
		"--state-dir", "/tmp/dl-node",
	)

	dev := h.waitDevice(t, "linux-node", "alive")
	assertLinuxContainerDevice(t, dev, linuxArch)
	waitConnectedLineCount(t, containerProc, expectedConnectedLine(dev), 1)
	assertLSHumanTableRow(t, mustController(t, h, "ls"), dev)

	mustController(t, h, "rm", "linux-node")
	requireNonZeroProcessExit(t, containerProc, 5*time.Second)
	requireAbsent(t, h.devices(t), "linux-node")
}

func verifyTTLConnectedLine(t *testing.T, h *hub, name, ttl string) {
	t.Helper()
	inviteOut := mustController(t, h, "invite", "--name", name, "--ttl", ttl)
	fields := strings.Fields(strings.TrimSpace(inviteOut))
	if len(fields) != 8 || fields[0] != "dl" || fields[1] != "join" {
		t.Fatalf("unexpected ttl invite output: %q", inviteOut)
	}
	joinArgs := append([]string{}, fields[1:]...)
	validateJoinArgs(t, joinArgs)
	joinArgs = append(joinArgs, "--state-dir", t.TempDir())
	proc := startProcess(t, h.binary, joinArgs...)

	dev := h.waitDevice(t, name, "alive")
	if dev.ExpiresAt == nil || *dev.ExpiresAt == "" {
		t.Fatalf("ttl invite expires_at is nil for %s", name)
	}
	parsedExpiry, err := time.Parse(time.RFC3339, *dev.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expires_at %q: %v", *dev.ExpiresAt, err)
	}
	remaining := time.Until(parsedExpiry)
	if remaining < 50*time.Minute || remaining > 70*time.Minute {
		t.Fatalf("expires_at %s remaining = %s, want ~1h", *dev.ExpiresAt, remaining)
	}
	waitConnectedLineCount(t, proc, expectedConnectedLine(dev), 1)
}

func verifyProxyReconnectStatusLine(t *testing.T, h *hub, name string) {
	t.Helper()
	proxy := newTCPProxy(t, strings.TrimPrefix(h.url, "https://"))
	nodeArgs := replaceFlag(h.invite(t, name), "--hub", "https://"+proxy.address)
	nodeArgs = append(nodeArgs, "--state-dir", t.TempDir())
	proc := startProcess(t, h.binary, nodeArgs...)

	dev := h.waitDevice(t, name, "alive")
	waitConnectedLineCount(t, proc, expectedConnectedLine(dev), 1)

	if err := proxy.Drop(); err != nil {
		t.Fatalf("drop proxy: %v", err)
	}
	h.waitDevice(t, name, "missed")

	if err := proxy.Resume(); err != nil {
		t.Fatalf("resume proxy: %v", err)
	}
	reconnected := h.waitDevice(t, name, "alive")
	waitConnectedLineCount(t, proc, expectedConnectedLine(reconnected), 2)
	requireAbsent(t, h.devices(t), name+"-2")
}

func verifyDecoupledKickExit(t *testing.T) {
	t.Helper()
	h := newHub(t)
	n := h.joinWithInvite(t, "kick-fast")
	dev := h.waitDevice(t, "kick-fast", "alive")
	waitConnectedLineCount(t, n.process, expectedConnectedLine(dev), 1)
	// bits:allow(E2E_SLEEP): position rm mid-heartbeat interval to prove decoupled WS read
	time.Sleep(100 * time.Millisecond)

	kickStart := time.Now()
	mustController(t, h, "rm", "kick-fast")
	requireNonZeroProcessExit(t, n.process, 900*time.Millisecond)
	if elapsed := time.Since(kickStart); elapsed >= 900*time.Millisecond {
		t.Fatalf("dl join exited in %s after rm, want < 900ms\n%s", elapsed, n.process.output())
	}
}

func verifyHealthySessionResetsBackoff(t *testing.T) {
	t.Helper()
	h := newHub(t)
	proxy, proc := startOfflineEscalatedJoin(t, h, "backoff-node")

	if err := proxy.Resume(); err != nil {
		t.Fatalf("first proxy resume: %v", err)
	}
	dev := h.waitDevice(t, "backoff-node", "alive")
	wantLine := expectedConnectedLine(dev)
	waitConnectedLineCount(t, proc, wantLine, 1)
	// bits:allow(E2E_SLEEP): keep session healthy across more than one 1s heartbeat
	time.Sleep(1200 * time.Millisecond)

	if err := proxy.Drop(); err != nil {
		t.Fatalf("second proxy drop: %v", err)
	}
	// bits:allow(E2E_SLEEP): brief network flap before measuring reset reconnect latency
	time.Sleep(50 * time.Millisecond)
	resumeAt := time.Now()
	if err := proxy.Resume(); err != nil {
		t.Fatalf("second proxy resume: %v", err)
	}
	waitFor(t, 1200*time.Millisecond, func() (bool, error) {
		return countLineOccurrences(proc.stdoutOutput(), wantLine) >= 2, nil
	})
	if elapsed := time.Since(resumeAt); elapsed >= 1200*time.Millisecond {
		t.Fatalf("reconnect after healthy session took %s, want < 1.2s", elapsed)
	}
}

func startOfflineEscalatedJoin(t *testing.T, h *hub, name string) (*tcpProxy, *childProcess) {
	t.Helper()
	seed := h.joinWithInvite(t, name)
	h.waitDevice(t, name, "alive")
	seed.process.stop()

	proxy := newTCPProxy(t, strings.TrimPrefix(h.url, "https://"))
	if err := proxy.Drop(); err != nil {
		t.Fatalf("initial proxy drop: %v", err)
	}
	proc := startProcess(t, h.binary, "join", "--hub", "https://"+proxy.address, "--ca", h.caPath, "--state-dir", seed.stateDir)
	// bits:allow(E2E_SLEEP): let offline reconnect backoff escalate past 250ms toward 2s
	time.Sleep(2200 * time.Millisecond)
	if proc.exited() {
		t.Fatalf("dl join exited while offline: %v\n%s", proc.exitError(), proc.output())
	}
	return proxy, proc
}

func expectedConnectedLine(dev device) string {
	return fmt.Sprintf(
		"dl connected: name=%s status=alive platform=%s/%s shell=%s expires_at=%s",
		dev.Name,
		dev.OS,
		dev.Arch,
		dev.Shell,
		expectedExpiresText(dev),
	)
}

func expectedExpiresText(dev device) string {
	if dev.ExpiresAt == nil || *dev.ExpiresAt == "" {
		return "never"
	}
	return *dev.ExpiresAt
}

func waitConnectedLineCount(t *testing.T, proc *childProcess, wantLine string, wantCount int) {
	t.Helper()
	waitFor(t, statusTimeout, func() (bool, error) {
		got := countLineOccurrences(proc.stdoutOutput(), wantLine)
		if got >= wantCount {
			return true, nil
		}
		return false, fmt.Errorf("stdout connected count = %d, want >= %d for %q in:\n%s", got, wantCount, wantLine, proc.output())
	})
}

func countLineOccurrences(output, wantLine string) int {
	count := 0
	for _, raw := range strings.Split(output, "\n") {
		if strings.TrimRight(raw, "\r") == wantLine {
			count++
		}
	}
	return count
}

func assertLSHumanTableRow(t *testing.T, tableOut string, dev device) {
	t.Helper()
	lines := nonEmptyLines(tableOut)
	if len(lines) < 2 {
		t.Fatalf("expected header and device row in ls output:\n%s", tableOut)
	}
	assertSubstringsInOrder(t, lines[0], []string{"NAME", "STATUS", "OS/ARCH", "SHELL", "EXPIRES", "LAST SEEN"})
	row := findLineContaining(lines[1:], dev.Name)
	if row == "" {
		t.Fatalf("device %s missing from ls human table:\n%s", dev.Name, tableOut)
	}
	platform := dev.OS + "/" + dev.Arch
	assertSubstringsInOrder(t, row, []string{dev.Name, "alive", platform, dev.Shell, expectedExpiresText(dev)})
}

func nonEmptyLines(text string) []string {
	var out []string
	for _, raw := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func findLineContaining(lines []string, needle string) string {
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func assertSubstringsInOrder(t *testing.T, line string, tokens []string) {
	t.Helper()
	offset := 0
	for _, token := range tokens {
		idx := strings.Index(line[offset:], token)
		if idx < 0 {
			t.Fatalf("line %q missing %q after offset %d (want order %v)", line, token, offset, tokens)
		}
		offset += idx + len(token)
	}
}

func requireLinuxContainerEnv(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("linux container test skipped on windows")
	}
	dockerBin, ok := findDockerBinary()
	if !ok {
		t.Skip("docker binary not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	archOut, err := exec.CommandContext(ctx, dockerBin, "info", "--format", "{{.Architecture}}").CombinedOutput()
	if err != nil {
		t.Skipf("docker info unavailable: %v (%s)", err, strings.TrimSpace(string(archOut)))
	}
	linuxArch, supported := mapContainerArch(strings.TrimSpace(string(archOut)))
	if !supported {
		t.Skipf("unsupported container architecture %q", strings.TrimSpace(string(archOut)))
	}
	if _, inspectErr := exec.CommandContext(ctx, dockerBin, "image", "inspect", "node:24-alpine").CombinedOutput(); inspectErr != nil {
		t.Skipf("docker image node:24-alpine not available locally: %v", inspectErr)
	}
	return dockerBin, linuxArch
}

func findDockerBinary() (string, bool) {
	if path, err := exec.LookPath("docker"); err == nil {
		return path, true
	}
	const fallback = "/Users/user/.local/bin/docker"
	if info, err := os.Stat(fallback); err == nil && !info.IsDir() {
		return fallback, true
	}
	return "", false
}

func mapContainerArch(raw string) (string, bool) {
	switch raw {
	case "aarch64", "arm64":
		return "arm64", true
	case "x86_64", "amd64":
		return "amd64", true
	default:
		return "", false
	}
}

func newContainerStageDir(t *testing.T) string {
	t.Helper()
	benchDir := filepath.Clean(filepath.Join("..", "..", ".bench"))
	if err := os.MkdirAll(benchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(benchDir, "e2e-linux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return absDir
}

func stageContainerCA(t *testing.T, caPath, stageDir string) {
	t.Helper()
	certBytes, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "cert.pem"), certBytes, 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildLinuxDLBinary(t *testing.T, stageDir, linuxArch string) {
	t.Helper()
	outBin := filepath.Join(stageDir, "dl")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-ldflags=-s -w", "-o", outBin, "./cmd/dl")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+linuxArch)
	if output, buildErr := cmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("cross-compile linux/%s dl: %v\n%s", linuxArch, buildErr, output)
	}
	if chmodErr := os.Chmod(outBin, 0o755); chmodErr != nil {
		t.Fatal(chmodErr)
	}
}

func startContainerTestHub(t *testing.T) (*hub, string) {
	t.Helper()
	binary := os.Getenv(binaryEnv)
	if binary == "" {
		t.Fatal("DL_E2E_BINARY is not set")
	}
	stateDir := t.TempDir()
	proc := startProcess(t, binary, "hub", "--addr", ":0", "--host", "host.docker.internal", "--state-dir", stateDir)
	port := waitBoundPort(t, proc)
	h := &hub{
		binary:   binary,
		url:      "https://127.0.0.1:" + port,
		stateDir: stateDir,
		caPath:   filepath.Join(stateDir, "cert.pem"),
		process:  proc,
	}
	waitHubReady(t, h)
	containerHubURL := "https://host.docker.internal:" + port
	if tcpReachable("host.docker.internal:" + port) {
		h.url = containerHubURL
	}
	return h, containerHubURL
}

func waitBoundPort(t *testing.T, proc *childProcess) string {
	t.Helper()
	var port string
	waitFor(t, readinessTimout, func() (bool, error) {
		if err := checkHubProcessRunning(proc); err != nil {
			return false, err
		}
		port = readBoundPort(proc.output())
		return port != "", nil
	})
	return port
}

func checkHubProcessRunning(proc *childProcess) error {
	if !proc.exited() {
		return nil
	}
	if exitErr := proc.exitError(); exitErr != nil {
		return fmt.Errorf("hub exited: %w\n%s", exitErr, proc.output())
	}
	return fmt.Errorf("hub exited unexpectedly with code 0\n%s", proc.output())
}

func tcpReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func mintContainerInviteCode(t *testing.T, h *hub) string {
	t.Helper()
	output, err := runCLI(t, h.binary, "invite", "--name", "linux-node", "--hub", h.url, "--state-dir", h.stateDir)
	if err != nil {
		t.Fatalf("invite linux-node: %v\n%s", err, output)
	}
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) < 4 || fields[0] != "dl" || fields[1] != "join" {
		t.Fatalf("unexpected invite output: %q", output)
	}
	validateJoinArgs(t, fields[1:])
	for i := 2; i+1 < len(fields); i += 2 {
		if fields[i] == "--code" {
			return fields[i+1]
		}
	}
	t.Fatalf("invite output missing --code: %q", output)
	return ""
}

func assertLinuxContainerDevice(t *testing.T, dev device, wantArch string) {
	t.Helper()
	if dev.OS != "linux" || dev.Arch != wantArch || dev.Shell == "" {
		t.Fatalf("unexpected linux container device metadata: %+v (want linux/%s)", dev, wantArch)
	}
}

func removeDockerContainer(dockerBin, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, dockerBin, "rm", "-f", name).Run()
}
