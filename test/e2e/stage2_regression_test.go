package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStage2MultiArgJobPathAndLargeOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable bit test")
	}
	h := newHub(t)
	_ = joinMinimalPathNode(t, h, "mac")
	h.waitDevice(t, "mac", "alive")

	verifyMultiArgPathLookupAndExecBit(t, h)
	verifyLargeSynchronousOutput(t, h)
}

func verifyMultiArgPathLookupAndExecBit(t *testing.T, h *hub) {
	t.Helper()
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir1, "mytool"), []byte("not-exec"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s-%s' \"$1\" \"$2\"\n"
	if err := os.WriteFile(filepath.Join(dir2, "mytool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	res := runExecCLI(t, h, "--env", "PATH="+dir1+":"+dir2+":/bin", "mac", "mytool", "hello", "world")
	assertExecExit(t, res, 0)
	if res.stdout != "hello-world" {
		t.Fatalf("mytool stdout = %q, want hello-world", res.stdout)
	}
}

func verifyLargeSynchronousOutput(t *testing.T, h *hub) {
	t.Helper()
	const wantBytes = 900000
	cmd := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x", wantBytes)
	res := runExecCLI(t, h, "--wait", "5s", "mac", cmd)
	assertExecExit(t, res, 0)
	if len(res.stdout) != wantBytes {
		t.Fatalf("large sync stdout len = %d, want %d (got prefix %q)", len(res.stdout), wantBytes, res.stdout[:min(len(res.stdout), 64)])
	}
}

func TestStage2DisconnectMidWaitAndEdgeGuards(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands require unix")
	}
	h := newHub(t)
	proxy := newTCPProxy(t, strings.TrimPrefix(h.url, "https://"))
	t.Cleanup(func() { _ = proxy.Resume() })
	_ = joinNodeViaProxy(t, h, proxy, "mac")
	h.waitDevice(t, "mac", "alive")

	verifyEmptyDeviceRejected(t, h)
	verifySubMillisecondTimeout(t, h)
	verifyCompletedShellCleansBackgroundChild(t, h)
	verifyDisconnectMidWaitDetachesPromptly(t, h, proxy)
}

func verifyEmptyDeviceRejected(t *testing.T, h *hub) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "should-not-exist")
	res := runExecCLI(t, h, "", "mac", fmt.Sprintf("printf bad > %q", marker))
	if res.exitCode == 0 {
		t.Fatalf("empty device arg unexpectedly succeeded: stdout=%q", res.stdout)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("empty device arg shifted command onto mac and executed it")
	}
}

func verifySubMillisecondTimeout(t *testing.T, h *hub) {
	t.Helper()
	res := runExecCLI(t, h, "--timeout", "500us", "--wait", "2s", "mac", "sleep 5")
	assertExecExit(t, res, 124)
}

func verifyCompletedShellCleansBackgroundChild(t *testing.T, h *hub) {
	t.Helper()
	res := runExecCLI(t, h, "--wait", "3s", "mac", "sleep 30 >/dev/null 2>&1 & printf '%s\n' \"$!\"")
	assertExecExit(t, res, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(res.stdout))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid background pid %q: %v", res.stdout, err)
	}
	waitPIDDead(t, pid)
}

func verifyDisconnectMidWaitDetachesPromptly(t *testing.T, h *hub, proxy *tcpProxy) {
	t.Helper()
	readyFile := filepath.Join(t.TempDir(), "started")
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitFileText(t, readyFile, "go")
		_ = proxy.Drop()
	}()
	start := time.Now()
	res := runExecCLI(t, h, "--wait", "8s", "mac", fmt.Sprintf("printf go > %q; sleep 3", readyFile))
	<-done
	elapsed := time.Since(start)
	assertExecExit(t, res, 0)
	_ = parseDetachedJobID(t, res.stdout)
	if elapsed >= 3*time.Second {
		t.Fatalf("mid-wait disconnect blocked for %s, want immediate detach (< 3s)", elapsed)
	}
}
