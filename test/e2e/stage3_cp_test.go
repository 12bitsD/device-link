package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	s3MiB            = 1024 * 1024
	s3OversizedBytes = 525 * s3MiB
	s3ThrottleBytes  = 192 * 1024
	s3CancelBytes    = 384 * 1024
	s3ThrottleChunk  = 2048
	s3ThrottleTick   = 45 * time.Millisecond
	s3PollTimeout    = 20 * time.Second
)

type s3Env struct {
	h     *hub
	nodeA *node
	nodeB *node
}

type s3CmdResult struct {
	stdout string
	stderr string
	err    error
}

func (r s3CmdResult) combined() string {
	return r.stdout + r.stderr
}

type s3TransferSummary struct {
	id          string
	state       string
	files       int
	skipped     int
	transferred int64
	total       int64
}

type s3TransferStatus struct {
	id          string
	from        string
	to          string
	state       string
	files       int
	skipped     int
	transferred int64
	total       int64
}

type s3TransferStatusJSON struct {
	TransferID string `json:"transfer_id"`
	From       string `json:"from"`
	To         string `json:"to"`
	State      string `json:"state"`
	Files      int    `json:"files"`
	Skipped    int    `json:"skipped"`
	Bytes      int64  `json:"bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

type s3AuthReq struct {
	method string
	url    string
	token  string
	body   string
}

func TestStage3CopyDirectionsAndMultiSourceExclude(t *testing.T) {
	env := s3NewEnv(t)
	s3VerifyLocalToRemoteAndExclude(t, env)
	s3VerifyRemoteToRemoteAndRemoteToLocal(t, env)
}

func TestStage3SHA256SkipAndPermissions(t *testing.T) {
	env := s3NewEnv(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "perm-target")
	execSrc := filepath.Join(srcDir, "run.sh")
	plainSrc := filepath.Join(srcDir, "config.txt")
	s3WriteFile(t, execSrc, []byte("#!/bin/sh\necho hi\n"), 0o755)
	s3WriteFile(t, plainSrc, []byte("port=8443\n"), 0o644)

	sum1 := s3RequireCompletedCP(t, s3RunCP(t, env.h, "node-a:"+execSrc, "node-a:"+plainSrc, "node-b:"+dstDir))
	s3AssertSummary(t, sum1, s3TransferSummary{files: 2, skipped: 0, transferred: 28, total: 28})
	s3AssertFileContent(t, filepath.Join(dstDir, "run.sh"), "#!/bin/sh\necho hi\n")
	s3AssertFileContent(t, filepath.Join(dstDir, "config.txt"), "port=8443\n")
	s3AssertTargetPermissions(t, filepath.Join(dstDir, "run.sh"), filepath.Join(dstDir, "config.txt"))

	s3VerifyUnchangedAndModifiedSkip(t, env, srcDir, dstDir)
}

func TestStage3PreflightLimitAndPathSafety(t *testing.T) {
	env := s3NewEnv(t)
	s3VerifyOversizedPreflightRejected(t, env)
	s3VerifySymlinkEscapeRejected(t, env)
	s3VerifyInvalidEndpointCombinations(t, env)
}

func TestStage3ThrottledRelayConcurrentExecAndCancel(t *testing.T) {
	h := newHub(t)
	proxy := s3NewThrottleProxy(t, strings.TrimPrefix(h.url, "https://"))
	_ = s3JoinViaProxy(t, h, proxy.url, "node-a")
	_ = s3JoinViaProxy(t, h, proxy.url, "node-b")

	s3VerifyThrottledTransferWithConcurrentExec(t, h)
	s3VerifyThrottledTransferCancel(t, h)
}

func TestStage3SecurityAndErrorClassification(t *testing.T) {
	h := newHub(t)
	nodeA := h.joinWithInvite(t, "node-a")
	h.waitDevice(t, "node-a", "alive")

	s3VerifyOfflineAndUnknownErrors(t, h)
	s3VerifyDeviceTokenRejectedCLI(t, h, nodeA.deviceToken(t))
	s3VerifyDeviceTokenRejectedHTTPS(t, h, nodeA.deviceToken(t))
}

func s3NewEnv(t *testing.T) *s3Env {
	t.Helper()
	h := newHub(t)
	nodeA := h.joinWithInvite(t, "node-a")
	nodeB := h.joinWithInvite(t, "node-b")
	h.waitDevice(t, "node-a", "alive")
	h.waitDevice(t, "node-b", "alive")
	return &s3Env{h: h, nodeA: nodeA, nodeB: nodeB}
}

func s3VerifyLocalToRemoteAndExclude(t *testing.T, env *s3Env) {
	t.Helper()
	srcRoot := t.TempDir()
	singleSrc := filepath.Join(srcRoot, "hello.txt")
	s3WriteFile(t, singleSrc, []byte("hello-stage3\n"), 0o644)

	target1 := filepath.Join(t.TempDir(), "auto", "nested", "dst1")
	sum1 := s3RequireCompletedCP(t, s3RunCP(t, env.h, singleSrc, "node-b:"+target1))
	s3AssertSummary(t, sum1, s3TransferSummary{files: 1, skipped: 0, transferred: 13, total: 13})
	s3AssertFileContent(t, filepath.Join(target1, "hello.txt"), "hello-stage3\n")

	topSrc, bundleDir := s3BuildExcludeFixture(t)
	target2 := filepath.Join(t.TempDir(), "multi", "dst2")
	trailingArgs := []string{
		"cp", "--exclude", "*.log",
		"local:" + topSrc, "local:" + bundleDir, "node-b:" + target2,
		"--exclude", "skipdir",
	}
	trailingArgs = append(trailingArgs, env.h.controllerArgs()...)
	sum2 := s3RequireCompletedCP(t, s3RunSeparate(t, env.h.binary, trailingArgs...))
	s3AssertSummary(t, sum2, s3TransferSummary{files: 3, skipped: 0, transferred: 30, total: 30})
	s3AssertExcludeFixtureResult(t, target2)
}

func s3BuildExcludeFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	topSrc := filepath.Join(root, "top.txt")
	bundleDir := filepath.Join(root, "bundle")
	s3WriteFile(t, topSrc, []byte("top-file"), 0o644)
	s3WriteFile(t, filepath.Join(bundleDir, "keep.txt"), []byte("keep-nested"), 0o644)
	s3WriteFile(t, filepath.Join(bundleDir, "sub", "deep.txt"), []byte("deep-nested"), 0o644)
	s3WriteFile(t, filepath.Join(bundleDir, "trace.log"), []byte("excluded-log"), 0o644)
	s3WriteFile(t, filepath.Join(bundleDir, "skipdir", "secret.txt"), []byte("excluded-dir"), 0o644)
	return topSrc, bundleDir
}

func s3AssertExcludeFixtureResult(t *testing.T, targetDir string) {
	t.Helper()
	s3AssertFileContent(t, filepath.Join(targetDir, "top.txt"), "top-file")
	s3AssertFileContent(t, filepath.Join(targetDir, "bundle", "keep.txt"), "keep-nested")
	s3AssertFileContent(t, filepath.Join(targetDir, "bundle", "sub", "deep.txt"), "deep-nested")
	s3AssertPathAbsent(t, filepath.Join(targetDir, "bundle", "trace.log"))
	s3AssertPathAbsent(t, filepath.Join(targetDir, "bundle", "skipdir"))
}

func s3VerifyRemoteToRemoteAndRemoteToLocal(t *testing.T, env *s3Env) {
	t.Helper()
	aRoot := filepath.Join(t.TempDir(), "afiles")
	s3WriteFile(t, filepath.Join(aRoot, "alpha.txt"), []byte("alpha-from-a"), 0o644)
	s3WriteFile(t, filepath.Join(aRoot, "nested", "beta.txt"), []byte("beta-from-a"), 0o644)
	s3WriteFile(t, filepath.Join(aRoot, "nested", "debug.log"), []byte("skip-me"), 0o644)

	bDst := filepath.Join(t.TempDir(), "from-a", "dst")
	sumAB := s3RequireCompletedCP(t, s3RunCP(t, env.h, "node-a:"+aRoot, "node-b:"+bDst, "--exclude", "*.log"))
	s3AssertSummary(t, sumAB, s3TransferSummary{files: 2, skipped: 0, transferred: 23, total: 23})
	s3AssertFileContent(t, filepath.Join(bDst, "afiles", "alpha.txt"), "alpha-from-a")
	s3AssertFileContent(t, filepath.Join(bDst, "afiles", "nested", "beta.txt"), "beta-from-a")
	s3AssertPathAbsent(t, filepath.Join(bDst, "afiles", "nested", "debug.log"))

	localDst1 := filepath.Join(t.TempDir(), "to-local-1")
	sumLocal1 := s3RequireCompletedCP(t, s3RunCP(t, env.h, "node-b:"+filepath.Join(bDst, "afiles", "alpha.txt"), localDst1))
	s3AssertSummary(t, sumLocal1, s3TransferSummary{files: 1, skipped: 0, transferred: 12, total: 12})
	s3AssertFileContent(t, filepath.Join(localDst1, "alpha.txt"), "alpha-from-a")

	localDst2 := filepath.Join(t.TempDir(), "to-local-2")
	sumLocal2 := s3RequireCompletedCP(t, s3RunCP(t, env.h, "--exclude", "*.log", "node-a:"+filepath.Join(aRoot, "nested"), "local:"+localDst2))
	s3AssertSummary(t, sumLocal2, s3TransferSummary{files: 1, skipped: 0, transferred: 11, total: 11})
	s3AssertFileContent(t, filepath.Join(localDst2, "nested", "beta.txt"), "beta-from-a")
	s3AssertPathAbsent(t, filepath.Join(localDst2, "nested", "debug.log"))
}

func s3VerifyUnchangedAndModifiedSkip(t *testing.T, env *s3Env, srcDir, dstDir string) {
	t.Helper()
	execSrc := filepath.Join(srcDir, "run.sh")
	plainSrc := filepath.Join(srcDir, "config.txt")
	dstPlain := filepath.Join(dstDir, "config.txt")
	past := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(dstPlain, past, past); err != nil {
		t.Fatalf("chtimes %s: %v", dstPlain, err)
	}

	res2 := s3RunCP(t, env.h, "node-a:"+execSrc, "node-a:"+plainSrc, "node-b:"+dstDir)
	sum2 := s3RequireCompletedCP(t, res2)
	s3AssertSummary(t, sum2, s3TransferSummary{files: 0, skipped: 2, transferred: 0})
	if !strings.Contains(res2.stdout, "bytes=0/") {
		t.Fatalf("expected bytes=0/ in stdout, got %q", res2.stdout)
	}
	s3AssertModTimeEqual(t, dstPlain, past)

	s3WriteFile(t, execSrc, []byte("#!/bin/sh\necho updated\n"), 0o755)
	sum3 := s3RequireCompletedCP(t, s3RunCP(t, env.h, "node-a:"+execSrc, "node-a:"+plainSrc, "node-b:"+dstDir))
	s3AssertSummary(t, sum3, s3TransferSummary{files: 1, skipped: 1, transferred: 23})
	s3AssertFileContent(t, filepath.Join(dstDir, "run.sh"), "#!/bin/sh\necho updated\n")
	s3AssertFileContent(t, dstPlain, "port=8443\n")
	s3AssertModTimeEqual(t, dstPlain, past)
	s3AssertTargetPermissions(t, filepath.Join(dstDir, "run.sh"), dstPlain)
}

func s3AssertTargetPermissions(t *testing.T, execPath, plainPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	execInfo, err := os.Stat(execPath)
	if err != nil {
		t.Fatalf("stat %s: %v", execPath, err)
	}
	if execInfo.Mode().Perm()&0o111 == 0 {
		t.Fatalf("expected executable bit on %s, got mode %v", execPath, execInfo.Mode())
	}
	plainInfo, plainErr := os.Stat(plainPath)
	if plainErr != nil {
		t.Fatalf("stat %s: %v", plainPath, plainErr)
	}
	if plainInfo.Mode().Perm()&0o111 != 0 {
		t.Fatalf("expected non-executable mode on %s, got mode %v", plainPath, plainInfo.Mode())
	}
}

func s3AssertModTimeEqual(t *testing.T, path string, want time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.ModTime().Equal(want) {
		t.Fatalf("expected %s mtime %v unchanged, got %v", path, want, info.ModTime())
	}
}

func s3VerifyOversizedPreflightRejected(t *testing.T, env *s3Env) {
	t.Helper()
	srcDir := filepath.Join(t.TempDir(), "oversized-src")
	smallPath := filepath.Join(srcDir, "small.txt")
	s3WriteFile(t, smallPath, []byte("small"), 0o644)
	giantPath := filepath.Join(srcDir, "giant.bin")
	s3CreateSparseFile(t, giantPath, s3OversizedBytes)
	defer func() { _ = os.Remove(giantPath) }()

	dstDir := filepath.Join(t.TempDir(), "oversized-dst")
	res := s3RunCP(t, env.h, "node-a:"+srcDir, "node-b:"+dstDir)
	if res.err == nil {
		t.Fatalf("expected >500MB preflight failure, got stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "exceeds 500MB limit") {
		t.Fatalf("expected stderr to contain 'exceeds 500MB limit', got %q", res.stderr)
	}
	if !strings.Contains(res.stderr, "giant.bin") {
		t.Fatalf("expected stderr to list oversized file 'giant.bin', got %q", res.stderr)
	}
	s3AssertPathAbsent(t, filepath.Join(dstDir, "oversized-src", "small.txt"))
	s3AssertPathAbsent(t, filepath.Join(dstDir, "oversized-src", "giant.bin"))
}

func s3CreateSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	t.Cleanup(func() { _ = os.Remove(path) })
	if truncErr := f.Truncate(size); truncErr != nil {
		t.Fatalf("truncate %s: %v", path, truncErr)
	}
}

func s3VerifySymlinkEscapeRejected(t *testing.T, env *s3Env) {
	t.Helper()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	s3WriteFile(t, outsideFile, []byte("outside-secret"), 0o644)

	srcEscape := filepath.Join(t.TempDir(), "src-escape")
	s3WriteFile(t, filepath.Join(srcEscape, "ok.txt"), []byte("ok"), 0o644)
	if !s3TrySymlink(t, outsideFile, filepath.Join(srcEscape, "leak.txt")) {
		return
	}
	dstEscape := filepath.Join(t.TempDir(), "dst-escape")
	resSrc := s3RunCP(t, env.h, "node-a:"+srcEscape, "node-b:"+dstEscape)
	requireFailure(t, resSrc.combined(), resSrc.err)
	s3AssertPathAbsent(t, filepath.Join(dstEscape, "src-escape", "leak.txt"))

	s3VerifyTargetSymlinkEscapeRejected(t, env, outsideDir)
}

func s3VerifyTargetSymlinkEscapeRejected(t *testing.T, env *s3Env, outsideDir string) {
	t.Helper()
	targetDir := t.TempDir()
	if !s3TrySymlink(t, outsideDir, filepath.Join(targetDir, "sub")) {
		return
	}
	srcSub := filepath.Join(t.TempDir(), "sub")
	s3WriteFile(t, filepath.Join(srcSub, "secret.txt"), []byte("must-not-write"), 0o644)

	resRemote := s3RunCP(t, env.h, "node-a:"+srcSub, "node-b:"+targetDir)
	requireFailure(t, resRemote.combined(), resRemote.err)
	s3AssertPathAbsent(t, filepath.Join(outsideDir, "secret.txt"))

	resLocal := s3RunCP(t, env.h, srcSub, "node-b:"+targetDir)
	requireFailure(t, resLocal.combined(), resLocal.err)
	s3AssertPathAbsent(t, filepath.Join(outsideDir, "secret.txt"))
}

func s3TrySymlink(t *testing.T, target, link string) bool {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return true
	}
	if runtime.GOOS == "windows" {
		return false
	}
	t.Fatalf("symlink %s -> %s: %v", link, target, err)
	return false
}

func s3VerifyInvalidEndpointCombinations(t *testing.T, env *s3Env) {
	t.Helper()
	p1 := filepath.Join(t.TempDir(), "p1.txt")
	p2 := filepath.Join(t.TempDir(), "p2.txt")
	s3WriteFile(t, p1, []byte("one"), 0o644)
	s3WriteFile(t, p2, []byte("two"), 0o644)
	dst := filepath.Join(t.TempDir(), "invalid-dst")

	resLocalPlain := s3RunCP(t, env.h, p1, dst)
	requireFailure(t, resLocalPlain.combined(), resLocalPlain.err)

	resLocalPrefixed := s3RunCP(t, env.h, "local:"+p1, "local:"+dst)
	requireFailure(t, resLocalPrefixed.combined(), resLocalPrefixed.err)

	resMixedRemote := s3RunCP(t, env.h, "node-a:"+p1, "node-b:"+p2, "node-b:"+dst)
	requireFailure(t, resMixedRemote.combined(), resMixedRemote.err)

	resDetachLocalToB := s3RunCP(t, env.h, "--detach", p1, "node-b:"+dst)
	requireFailure(t, resDetachLocalToB.combined(), resDetachLocalToB.err)

	resDetachAToLocal := s3RunCP(t, env.h, "--detach", "node-a:"+p1, dst)
	requireFailure(t, resDetachAToLocal.combined(), resDetachAToLocal.err)
}

func s3VerifyThrottledTransferWithConcurrentExec(t *testing.T, h *hub) {
	t.Helper()
	payload := s3DeterministicBytes(s3ThrottleBytes)
	srcFile := filepath.Join(t.TempDir(), "stream192.bin")
	s3WriteFile(t, srcFile, payload, 0o644)
	dstDir := filepath.Join(t.TempDir(), "throttled-dst-1")

	transferID := s3ParseDetachedID(t, s3RunCP(t, h, "--detach", "node-a:"+srcFile, "node-b:"+dstDir))
	running := s3WaitTransferState(t, h, transferID, "running")
	s3AssertStatusEndpoints(t, running, "node-a", "node-b")
	s3VerifyStatusJSON(t, h, transferID, "running")
	s3VerifyPromptExecOnNodeA(t, h)

	completed := s3WaitTransferState(t, h, transferID, "completed")
	if completed.files != 1 || completed.skipped != 0 || completed.transferred != s3ThrottleBytes {
		t.Fatalf("unexpected completed status: %+v", completed)
	}
	s3VerifyStatusJSONTrailingFlag(t, h, transferID, "completed")
	got, err := os.ReadFile(filepath.Join(dstDir, "stream192.bin"))
	if err != nil {
		t.Fatalf("read transferred file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("transferred payload mismatch: got %d bytes, want %d bytes", len(got), len(payload))
	}
}

func s3AssertStatusEndpoints(t *testing.T, st s3TransferStatus, wantFrom, wantTo string) {
	t.Helper()
	if st.from != wantFrom || st.to != wantTo || st.total <= 0 {
		t.Fatalf("unexpected status endpoints/total: %+v", st)
	}
}

func s3VerifyPromptExecOnNodeA(t *testing.T, h *hub) {
	t.Helper()
	start := time.Now()
	execArgs := []string{"exec"}
	execArgs = append(execArgs, h.controllerArgs()...)
	execArgs = append(execArgs, "node-a", "echo stage3-exec-marker")
	res := s3RunSeparate(t, h.binary, execArgs...)
	elapsed := time.Since(start)
	if res.err != nil {
		t.Fatalf("concurrent exec failed: %v\n%s", res.err, res.combined())
	}
	if !strings.Contains(res.stdout, "stage3-exec-marker") {
		t.Fatalf("unexpected concurrent exec stdout: %q", res.stdout)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("concurrent exec took %s, blocked by slow transfer", elapsed)
	}
}

func s3VerifyThrottledTransferCancel(t *testing.T, h *hub) {
	t.Helper()
	payload := s3DeterministicBytes(s3CancelBytes)
	srcFile := filepath.Join(t.TempDir(), "stream384.bin")
	s3WriteFile(t, srcFile, payload, 0o644)
	dstDir := filepath.Join(t.TempDir(), "throttled-dst-2")

	transferID := s3ParseDetachedID(t, s3RunCP(t, h, "--detach", "node-a:"+srcFile, "node-b:"+dstDir))
	_ = s3WaitTransferState(t, h, transferID, "running")

	cancelRes := s3RunTransfer(t, h, "cancel", transferID)
	if cancelRes.err != nil {
		t.Fatalf("transfer cancel %s failed: %v\n%s", transferID, cancelRes.err, cancelRes.combined())
	}
	cancelled := s3WaitTransferState(t, h, transferID, "cancelled")
	s3AssertStatusEndpoints(t, cancelled, "node-a", "node-b")
	s3VerifyStatusJSON(t, h, transferID, "cancelled")
}

func s3VerifyStatusJSON(t *testing.T, h *hub, transferID, wantState string) {
	t.Helper()
	res := s3RunTransfer(t, h, "status", "--json", transferID)
	if res.err != nil {
		t.Fatalf("transfer status --json %s: %v\n%s", transferID, res.err, res.combined())
	}
	s3CheckDecodedStatusJSON(t, s3DecodeStatusJSON(t, res.stdout), transferID, wantState)
}

func s3VerifyStatusJSONTrailingFlag(t *testing.T, h *hub, transferID, wantState string) {
	t.Helper()
	res := s3RunTransfer(t, h, "status", transferID, "--json")
	if res.err != nil {
		t.Fatalf("transfer status %s --json: %v\n%s", transferID, res.err, res.combined())
	}
	s3CheckDecodedStatusJSON(t, s3DecodeStatusJSON(t, res.stdout), transferID, wantState)
}

func s3CheckDecodedStatusJSON(t *testing.T, decoded s3TransferStatusJSON, transferID, wantState string) {
	t.Helper()
	if decoded.TransferID != transferID || decoded.From != "node-a" || decoded.To != "node-b" {
		t.Fatalf("unexpected json identity fields: %+v", decoded)
	}
	if decoded.State != wantState {
		t.Fatalf("json state = %q, want %q (%+v)", decoded.State, wantState, decoded)
	}
	if decoded.TotalBytes <= 0 || decoded.Bytes < 0 || decoded.Bytes > decoded.TotalBytes {
		t.Fatalf("invalid json byte counters: %+v", decoded)
	}
}

func s3WaitTransferState(t *testing.T, h *hub, transferID, wantState string) s3TransferStatus {
	t.Helper()
	var matched s3TransferStatus
	waitFor(t, s3PollTimeout, func() (bool, error) {
		res := s3RunTransfer(t, h, "status", transferID)
		if res.err != nil {
			return false, fmt.Errorf("status %s: %w (%s)", transferID, res.err, res.combined())
		}
		st, err := s3ParseStatusLine(res.stdout)
		if err != nil {
			return false, err
		}
		if st.state == wantState {
			matched = st
			return true, nil
		}
		return false, fmt.Errorf("state=%s, want %s", st.state, wantState)
	})
	return matched
}

func s3VerifyOfflineAndUnknownErrors(t *testing.T, h *hub) {
	t.Helper()
	offlineNode := h.joinWithInvite(t, "node-offline")
	h.waitDevice(t, "node-offline", "alive")
	offlineNode.process.stop()
	h.waitDevice(t, "node-offline", "missed")

	srcFile := filepath.Join(t.TempDir(), "probe.txt")
	s3WriteFile(t, srcFile, []byte("probe"), 0o644)
	dstDir := filepath.Join(t.TempDir(), "err-dst")

	offlineRes := s3RunCP(t, h, srcFile, "node-offline:"+dstDir)
	requireFailure(t, offlineRes.combined(), offlineRes.err)
	if !strings.Contains(strings.ToLower(offlineRes.combined()), "offline") {
		t.Fatalf("expected offline classification in output, got %q", offlineRes.combined())
	}

	unknownRes := s3RunCP(t, h, srcFile, "ghost-device:"+dstDir)
	requireFailure(t, unknownRes.combined(), unknownRes.err)
	s3AssertUnknownDeviceOutput(t, unknownRes.combined())

	missingStatus := s3RunTransfer(t, h, "status", "missing-transfer-id")
	requireFailure(t, missingStatus.combined(), missingStatus.err)

	missingCancel := s3RunTransfer(t, h, "cancel", "missing-transfer-id")
	requireFailure(t, missingCancel.combined(), missingCancel.err)
}

func s3AssertUnknownDeviceOutput(t *testing.T, output string) {
	t.Helper()
	lower := strings.ToLower(output)
	if strings.Contains(lower, "offline") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "forbidden") {
		t.Fatalf("unknown device misclassified as offline/unauthorized: %q", output)
	}
	if !strings.Contains(lower, "not found") && !strings.Contains(lower, "unknown") {
		t.Fatalf("expected 'not found' or 'unknown' in output, got %q", output)
	}
}

func s3VerifyDeviceTokenRejectedCLI(t *testing.T, h *hub, deviceToken string) {
	t.Helper()
	srcFile := filepath.Join(t.TempDir(), "unauth.txt")
	s3WriteFile(t, srcFile, []byte("deny"), 0o644)
	dstDir := filepath.Join(t.TempDir(), "unauth-dst")
	authArgs := []string{"--hub", h.url, "--token", deviceToken, "--ca", h.caPath}

	cpArgs := append([]string{"cp"}, authArgs...)
	cpArgs = append(cpArgs, srcFile, "node-a:"+dstDir)
	cpRes := s3RunSeparate(t, h.binary, cpArgs...)
	requireFailure(t, cpRes.combined(), cpRes.err)
	s3AssertPathAbsent(t, filepath.Join(dstDir, "unauth.txt"))

	statusArgs := append([]string{"transfer", "status"}, authArgs...)
	statusArgs = append(statusArgs, "missing")
	statusRes := s3RunSeparate(t, h.binary, statusArgs...)
	requireFailure(t, statusRes.combined(), statusRes.err)

	cancelArgs := append([]string{"transfer", "cancel"}, authArgs...)
	cancelArgs = append(cancelArgs, "missing")
	cancelRes := s3RunSeparate(t, h.binary, cancelArgs...)
	requireFailure(t, cancelRes.combined(), cancelRes.err)
}

func s3VerifyDeviceTokenRejectedHTTPS(t *testing.T, h *hub, deviceToken string) {
	t.Helper()
	client := s3HTTPClient(t, h.caPath)
	defer client.CloseIdleConnections()

	createBody := `{"from":"local","to":"node-a","sources":["a"],"target_dir":"/tmp/b"}`
	s3AssertBearerDenied(t, client, s3AuthReq{
		method: http.MethodPost, url: h.url + "/v1/transfers", token: deviceToken, body: createBody,
	})
	s3AssertBearerDenied(t, client, s3AuthReq{
		method: http.MethodGet, url: h.url + "/v1/transfers/missing", token: deviceToken,
	})
	s3AssertBearerDenied(t, client, s3AuthReq{
		method: http.MethodPost, url: h.url + "/v1/transfers/missing/cancel", token: deviceToken,
	})
}

func s3HTTPClient(t *testing.T, caPath string) *http.Client {
	t.Helper()
	pemBytes, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read ca %s: %v", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("parse ca %s failed", caPath)
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func s3AssertBearerDenied(t *testing.T, client *http.Client, spec s3AuthReq) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var bodyReader io.Reader
	if spec.body != "" {
		bodyReader = strings.NewReader(spec.body)
	}
	req, err := http.NewRequestWithContext(ctx, spec.method, spec.url, bodyReader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", spec.method, spec.url, err)
	}
	req.Header.Set("Authorization", "Bearer "+spec.token)
	if spec.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, doErr := client.Do(req)
	if doErr != nil {
		t.Fatalf("%s %s: %v", spec.method, spec.url, doErr)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%s %s status = %d, want 401 or 403", spec.method, spec.url, resp.StatusCode)
	}
}

func s3RunCP(t *testing.T, h *hub, args ...string) s3CmdResult {
	t.Helper()
	allArgs := []string{"cp"}
	allArgs = append(allArgs, h.controllerArgs()...)
	allArgs = append(allArgs, args...)
	return s3RunSeparate(t, h.binary, allArgs...)
}

func s3RunTransfer(t *testing.T, h *hub, action string, args ...string) s3CmdResult {
	t.Helper()
	allArgs := []string{"transfer", action}
	allArgs = append(allArgs, h.controllerArgs()...)
	allArgs = append(allArgs, args...)
	return s3RunSeparate(t, h.binary, allArgs...)
}

func s3RunSeparate(t *testing.T, binary string, args ...string) s3CmdResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = processEnvironment(t)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return s3CmdResult{
			stdout: stdoutBuf.String(),
			stderr: stderrBuf.String(),
			err:    fmt.Errorf("command timed out: %w", ctx.Err()),
		}
	}
	if runErr != nil {
		return s3CmdResult{
			stdout: stdoutBuf.String(),
			stderr: stderrBuf.String(),
			err:    fmt.Errorf("command failed: %w", runErr),
		}
	}
	return s3CmdResult{stdout: stdoutBuf.String(), stderr: stderrBuf.String()}
}

func s3RequireCompletedCP(t *testing.T, res s3CmdResult) s3TransferSummary {
	t.Helper()
	if res.err != nil {
		t.Fatalf("dl cp failed: %v\nstdout: %s\nstderr: %s", res.err, res.stdout, res.stderr)
	}
	kv, err := s3ParseKVLine(res.stdout)
	if err != nil || len(kv) != 5 {
		t.Fatalf("invalid cp stdout %q: err=%v", res.stdout, err)
	}
	files, filesErr := strconv.Atoi(kv["files"])
	skipped, skipErr := strconv.Atoi(kv["skipped"])
	transferred, total, bytesErr := s3ParseBytePair(kv["bytes"])
	if filesErr != nil || skipErr != nil || bytesErr != nil {
		t.Fatalf("invalid numeric fields in cp stdout %q", res.stdout)
	}
	if kv["transfer_id"] == "" || kv["state"] != "completed" {
		t.Fatalf("unexpected transfer_id or state in cp stdout %q", res.stdout)
	}
	return s3TransferSummary{
		id:          kv["transfer_id"],
		state:       kv["state"],
		files:       files,
		skipped:     skipped,
		transferred: transferred,
		total:       total,
	}
}

func s3AssertSummary(t *testing.T, got, want s3TransferSummary) {
	t.Helper()
	if got.files != want.files || got.skipped != want.skipped || got.transferred != want.transferred {
		t.Fatalf("summary = %+v, want files=%d skipped=%d transferred=%d", got, want.files, want.skipped, want.transferred)
	}
	if want.total > 0 && got.total != want.total {
		t.Fatalf("summary total = %d, want %d (%+v)", got.total, want.total, got)
	}
}

func s3ParseDetachedID(t *testing.T, res s3CmdResult) string {
	t.Helper()
	if res.err != nil {
		t.Fatalf("detached cp failed: %v\nstdout: %s\nstderr: %s", res.err, res.stdout, res.stderr)
	}
	trimmed := strings.TrimSpace(res.stdout)
	if strings.ContainsAny(trimmed, " \t\r\n") {
		t.Fatalf("expected single token transfer_id=<id>, got %q", res.stdout)
	}
	id, ok := strings.CutPrefix(trimmed, "transfer_id=")
	if !ok || id == "" {
		t.Fatalf("expected transfer_id=<id>, got %q", res.stdout)
	}
	return id
}

func s3ParseStatusLine(raw string) (s3TransferStatus, error) {
	kv, err := s3ParseKVLine(raw)
	if err != nil {
		return s3TransferStatus{}, fmt.Errorf("parse status line: %w", err)
	}
	if len(kv) != 7 || !s3HasNonEmptyKeys(kv, "transfer_id", "from", "to", "state") {
		return s3TransferStatus{}, fmt.Errorf("unexpected status fields in %q", raw)
	}
	files, filesErr := strconv.Atoi(kv["files"])
	skipped, skipErr := strconv.Atoi(kv["skipped"])
	transferred, total, bytesErr := s3ParseBytePair(kv["bytes"])
	if filesErr != nil || skipErr != nil || bytesErr != nil {
		return s3TransferStatus{}, fmt.Errorf("invalid status numbers in %q", raw)
	}
	return s3TransferStatus{
		id:          kv["transfer_id"],
		from:        kv["from"],
		to:          kv["to"],
		state:       kv["state"],
		files:       files,
		skipped:     skipped,
		transferred: transferred,
		total:       total,
	}, nil
}

func s3HasNonEmptyKeys(kv map[string]string, keys ...string) bool {
	for _, k := range keys {
		if kv[k] == "" {
			return false
		}
	}
	return true
}

func s3DecodeStatusJSON(t *testing.T, raw string) s3TransferStatusJSON {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		t.Fatalf("decode status json map %q: %v", raw, err)
	}
	required := []string{"transfer_id", "from", "to", "state", "files", "skipped", "bytes", "total_bytes"}
	for _, k := range required {
		if _, ok := keys[k]; !ok {
			t.Fatalf("missing key %q in status json %s", k, raw)
		}
	}
	var status s3TransferStatusJSON
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatalf("decode status json struct %q: %v", raw, err)
	}
	return status
}

func s3ParseKVLine(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.Contains(trimmed, "\n") {
		return nil, fmt.Errorf("expected single non-empty line, got %q", raw)
	}
	tokens := strings.Fields(trimmed)
	values := make(map[string]string, len(tokens))
	for _, tok := range tokens {
		key, val, ok := strings.Cut(tok, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("malformed token %q in %q", tok, raw)
		}
		values[key] = val
	}
	return values, nil
}

func s3ParseBytePair(raw string) (int64, int64, error) {
	left, right, ok := strings.Cut(raw, "/")
	if !ok {
		return 0, 0, fmt.Errorf("missing slash in bytes=%q", raw)
	}
	transferred, err1 := strconv.ParseInt(left, 10, 64)
	if err1 != nil {
		return 0, 0, fmt.Errorf("parse transferred %q: %w", left, err1)
	}
	total, err2 := strconv.ParseInt(right, 10, 64)
	if err2 != nil {
		return 0, 0, fmt.Errorf("parse total %q: %w", right, err2)
	}
	return transferred, total, nil
}

func s3WriteFile(t *testing.T, path string, content []byte, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func s3AssertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s content = %q, want %q", path, string(got), want)
	}
}

func s3AssertPathAbsent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to not exist, lstat err = %v", path, err)
	}
}

func s3DeterministicBytes(size int) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte((i*31 + 17) & 0xff)
	}
	return buf
}

func s3JoinViaProxy(t *testing.T, h *hub, proxyURL, name string) *node {
	t.Helper()
	joinArgs := h.invite(t, name)
	rewritten := make([]string, len(joinArgs))
	copy(rewritten, joinArgs)
	for i := 0; i+1 < len(rewritten); i++ {
		if rewritten[i] == "--hub" {
			rewritten[i+1] = proxyURL
		}
	}
	stateDir := t.TempDir()
	rewritten = append(rewritten, "--state-dir", stateDir)
	proc := startProcess(t, h.binary, rewritten...)
	n := &node{binary: h.binary, hubURL: proxyURL, caPath: h.caPath, stateDir: stateDir, process: proc}
	h.waitDevice(t, name, "alive")
	return n
}

type s3ThrottleProxy struct {
	addr       string
	url        string
	targetAddr string
	listener   net.Listener
	stopCh     chan struct{}
	stopOnce   sync.Once
	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	wg         sync.WaitGroup
}

func s3NewThrottleProxy(t *testing.T, targetAddr string) *s3ThrottleProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen throttle proxy: %v", err)
	}
	p := &s3ThrottleProxy{
		addr:       listener.Addr().String(),
		url:        "https://" + listener.Addr().String(),
		targetAddr: targetAddr,
		listener:   listener,
		stopCh:     make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
	}
	p.wg.Add(1)
	go p.acceptLoop()
	t.Cleanup(p.close)
	return p
}

func (p *s3ThrottleProxy) isStopped() bool {
	select {
	case <-p.stopCh:
		return true
	default:
		return false
	}
}

func (p *s3ThrottleProxy) trackConn(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isStopped() {
		_ = c.Close()
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *s3ThrottleProxy) untrackAndClose(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	_ = c.Close()
}

func (p *s3ThrottleProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		clientConn, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.startRelay(clientConn)
	}
}

func (p *s3ThrottleProxy) startRelay(clientConn net.Conn) {
	if !p.trackConn(clientConn) {
		return
	}
	upstreamConn, err := net.DialTimeout("tcp", p.targetAddr, 2*time.Second)
	if err != nil {
		p.untrackAndClose(clientConn)
		return
	}
	if !p.trackConn(upstreamConn) {
		p.untrackAndClose(clientConn)
		return
	}
	p.wg.Add(2)
	go p.pipeThrottled(upstreamConn, clientConn)
	go p.pipeThrottled(clientConn, upstreamConn)
}

func (p *s3ThrottleProxy) pipeThrottled(dst, src net.Conn) {
	defer p.wg.Done()
	defer p.untrackAndClose(dst)
	defer p.untrackAndClose(src)
	buf := make([]byte, s3ThrottleChunk)
	for !p.isStopped() {
		n, readErr := src.Read(buf)
		if n > 0 && !p.writeAndPace(dst, buf[:n]) {
			return
		}
		if readErr != nil {
			return
		}
	}
}

func (p *s3ThrottleProxy) writeAndPace(dst net.Conn, chunk []byte) bool {
	if _, err := dst.Write(chunk); err != nil {
		return false
	}
	if p.isStopped() {
		return false
	}
	delay := time.Duration(len(chunk)) * s3ThrottleTick / s3ThrottleChunk
	if delay > 0 {
		// bits:allow(E2E_SLEEP): bandwidth throttle tick
		time.Sleep(delay)
	}
	return true
}

func (p *s3ThrottleProxy) close() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		_ = p.listener.Close()
		p.mu.Lock()
		active := make([]net.Conn, 0, len(p.conns))
		for c := range p.conns {
			active = append(active, c)
		}
		p.mu.Unlock()
		for _, c := range active {
			_ = c.Close()
		}
		p.wg.Wait()
	})
}
