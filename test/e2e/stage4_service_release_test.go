package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type s4ServiceEnv struct {
	homeDir  string
	logFile  string
	unitPath string
	env      []string
}

type s4CmdResult struct {
	stdout   string
	combined string
}

type s4ReleaseArchive struct {
	name   string
	path   string
	digest string
}

func TestStage4JoinServiceInstallAndBootLifecycle(t *testing.T) {
	h := newHub(t)
	svcEnv := s4NewServiceEnv(t)
	s4AssertInvalidServiceInputs(t, h, svcEnv)

	joinArgs := h.invite(t, "svc-node")
	code := s4FlagValue(t, joinArgs, "--code")
	stateDir := filepath.Join(t.TempDir(), "svc-state")

	res, runErr := s4RunEnvCLI(
		t,
		svcEnv.env,
		h.binary,
		"join", "--service",
		"--hub", h.url,
		"--code", code,
		"--ca", h.caPath,
		"--state-dir", stateDir,
	)
	if runErr != nil {
		t.Fatalf("dl join --service failed: %v\n%s", runErr, res.combined)
	}
	wantStdout := "dl service installed: platform=" + runtime.GOOS + " unit="
	if !strings.Contains(res.stdout, wantStdout) {
		t.Fatalf("stdout = %q, want substring %q", res.stdout, wantStdout)
	}

	s4AssertNodeStateFile(t, stateDir)
	s4AssertInviteConsumed(t, h, code)
	s4AssertServiceArtifacts(t, svcEnv, code)
	s4VerifyEnrolledServiceBoot(t, h, stateDir)
}

func TestStage4InstallScriptChecksumAndOneLineJoin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets POSIX shells on darwin and linux")
	}
	scriptPath := filepath.Join(s4RepoRoot(t), "scripts", "install.sh")
	if _, statErr := os.Stat(scriptPath); statErr != nil {
		t.Fatalf("missing install script %s: %v", scriptPath, statErr)
	}
	h := newHub(t)
	archive := s4BuildReleaseTarGz(t, h.binary)

	s4VerifyInstallChecksumMismatch(t, scriptPath, archive)
	s4VerifyInstallAndServiceJoin(t, h, scriptPath, archive)
}

func TestStage4WorkflowsAndDocsContract(t *testing.T) {
	root := s4RepoRoot(t)

	ciPath := filepath.Join(root, ".github", "workflows", "ci.yml")
	ciContent := s4ReadNonEmptyFile(t, ciPath)
	s4RequireContainsAll(t, ciPath, ciContent, []string{
		"push", "pull_request", "ubuntu-latest", "macos-latest", "windows-latest", "./test/e2e",
	})

	relPath := filepath.Join(root, ".github", "workflows", "release.yml")
	relContent := s4ReadNonEmptyFile(t, relPath)
	s4RequireContainsAny(t, relPath, relContent, []string{
		"CGO_ENABLED=0", `CGO_ENABLED: "0"`, `CGO_ENABLED: '0'`, "CGO_ENABLED: 0",
	})
	s4RequireContainsAll(t, relPath, relContent, []string{
		"v*", "darwin", "linux", "windows", "amd64", "arm64", "checksums.txt",
	})

	readmePath := filepath.Join(root, "README.md")
	readmeContent := s4ReadNonEmptyFile(t, readmePath)
	s4RequireContainsAll(t, readmePath, readmeContent, []string{
		"dl join", "--service", "dl exec", "dl cp", "install.sh",
	})
}

func s4NewServiceEnv(t *testing.T) s4ServiceEnv {
	t.Helper()
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Fatal("refusing to run service install test as root")
	}
	root := t.TempDir()
	homeDir := filepath.Join(root, "home")
	xdgDir := filepath.Join(root, "xdg")
	shimDir := filepath.Join(root, "bin")
	for _, dir := range []string{homeDir, xdgDir, shimDir} {
		if mkdirErr := os.MkdirAll(dir, 0o755); mkdirErr != nil {
			t.Fatalf("mkdir %s: %v", dir, mkdirErr)
		}
	}
	logFile := filepath.Join(root, "shim.log")
	s4InstallServiceShim(t, shimDir, logFile)
	env := s4BuildIsolatedEnv(t, homeDir, xdgDir, shimDir)
	env = s4SetEnv(env, "DL_S4_SHIM_LOG", logFile)
	return s4ServiceEnv{
		homeDir:  homeDir,
		logFile:  logFile,
		unitPath: s4ExpectedUnitPath(homeDir, xdgDir),
		env:      env,
	}
}

func s4ExpectedUnitPath(homeDir, xdgDir string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir, "Library", "LaunchAgents", "io.devicelink.node.plist")
	case "linux":
		return filepath.Join(xdgDir, "systemd", "user", "device-link.service")
	default:
		return ""
	}
}

func s4InstallServiceShim(t *testing.T, shimDir, logFile string) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		s4WriteUnixShim(t, filepath.Join(shimDir, "launchctl"), logFile)
	case "linux":
		s4WriteUnixShim(t, filepath.Join(shimDir, "systemctl"), logFile)
	case "windows":
		s4BuildWindowsShim(t, shimDir, logFile)
	default:
		t.Fatalf("unsupported GOOS %s", runtime.GOOS)
	}
}

func s4WriteUnixShim(t *testing.T, shimPath, logFile string) {
	t.Helper()
	escapedLog := strings.ReplaceAll(logFile, "'", "'\\''")
	script := fmt.Sprintf(
		"#!/bin/sh\nlog_target=\"${DL_S4_SHIM_LOG}\"\nif [ -z \"$log_target\" ]; then\n  log_target='%s'\nfi\nprintf '%%s\\n' \"$*\" >> \"$log_target\"\nexit 0\n",
		escapedLog,
	)
	if writeErr := os.WriteFile(shimPath, []byte(script), 0o755); writeErr != nil {
		t.Fatalf("write unix shim %s: %v", shimPath, writeErr)
	}
}

func s4BuildWindowsShim(t *testing.T, shimDir, logFile string) {
	t.Helper()
	srcDir := t.TempDir()
	buildTmp := t.TempDir()
	srcPath := filepath.Join(srcDir, "main.go")
	src := fmt.Sprintf(`package main

import (
	"os"
	"strings"
)

func main() {
	logPath := os.Getenv("DL_S4_SHIM_LOG")
	if logPath == "" {
		logPath = %q
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(1)
	}
	_, _ = f.WriteString(strings.Join(os.Args[1:], " ") + "\n")
	_ = f.Close()
}
`, logFile)
	if writeErr := os.WriteFile(srcPath, []byte(src), 0o600); writeErr != nil {
		t.Fatalf("write windows shim source: %v", writeErr)
	}
	s4CompileGoBinary(t, srcPath, filepath.Join(shimDir, "sc.exe"), buildTmp)
}

func s4CompileGoBinary(t *testing.T, srcPath, outPath, buildTmp string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	env := s4SetEnv(os.Environ(), "TMP", buildTmp)
	env = s4SetEnv(env, "TEMP", buildTmp)
	env = s4SetEnv(env, "TMPDIR", buildTmp)
	cmd := exec.CommandContext(ctx, "go", "build", "-o", outPath, srcPath)
	cmd.Env = env
	out, buildErr := cmd.CombinedOutput()
	if buildErr != nil {
		t.Fatalf("compile shim %s: %v\n%s", outPath, buildErr, out)
	}
}

func s4BuildIsolatedEnv(t *testing.T, homeDir, xdgDir, shimDir string) []string {
	t.Helper()
	env := processEnvironment(t)
	origPath := s4EnvLookup(env, "PATH")
	mergedPath := shimDir
	if origPath != "" {
		mergedPath = shimDir + string(os.PathListSeparator) + origPath
	}
	env = s4SetEnv(env, "HOME", homeDir)
	env = s4SetEnv(env, "XDG_CONFIG_HOME", xdgDir)
	env = s4SetEnv(env, "USERPROFILE", homeDir)
	env = s4SetEnv(env, "PATH", mergedPath)
	return env
}

func s4RunEnvCLI(t *testing.T, env []string, binary string, args ...string) (s4CmdResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	stdoutBuf := &lockedBuffer{}
	combinedBuf := &lockedBuffer{}
	cmd.Stdout = io.MultiWriter(stdoutBuf, combinedBuf)
	cmd.Stderr = combinedBuf
	runErr := cmd.Run()
	res := s4CmdResult{
		stdout:   stdoutBuf.String(),
		combined: combinedBuf.String(),
	}
	if ctx.Err() != nil {
		return res, fmt.Errorf("command timed out: %w", ctx.Err())
	}
	if runErr != nil {
		return res, fmt.Errorf("command failed: %w", runErr)
	}
	return res, nil
}

func s4AssertInvalidServiceInputs(t *testing.T, h *hub, svcEnv s4ServiceEnv) {
	t.Helper()
	missingRes, missingErr := s4RunEnvCLI(t, svcEnv.env, h.binary, "join", "--service")
	requireFailure(t, missingRes.combined, missingErr)
	s4AssertPreInstallClean(t, svcEnv)

	badStateDir := filepath.Join(t.TempDir(), "bad-state")
	badRes, badErr := s4RunEnvCLI(
		t,
		svcEnv.env,
		h.binary,
		"join", "--service",
		"--hub", h.url,
		"--code", "invalid-code",
		"--ca", h.caPath,
		"--state-dir", badStateDir,
	)
	requireFailure(t, badRes.combined, badErr)
	s4RequireFileAbsent(t, filepath.Join(badStateDir, "node.json"))
	s4AssertPreInstallClean(t, svcEnv)
}

func s4AssertPreInstallClean(t *testing.T, svcEnv s4ServiceEnv) {
	t.Helper()
	if svcEnv.unitPath != "" {
		s4RequireFileAbsent(t, svcEnv.unitPath)
	}
	fallbackLinuxUnit := filepath.Join(svcEnv.homeDir, ".config", "systemd", "user", "device-link.service")
	s4RequireFileAbsent(t, fallbackLinuxUnit)
	shimLog := s4ReadShimLog(t, svcEnv.logFile)
	if strings.TrimSpace(shimLog) != "" {
		t.Fatalf("expected zero service-manager calls on failed install, got:\n%s", shimLog)
	}
}

func s4AssertNodeStateFile(t *testing.T, stateDir string) {
	t.Helper()
	nodeFile := filepath.Join(stateDir, "node.json")
	info, statErr := os.Stat(nodeFile)
	if statErr != nil {
		t.Fatalf("stat %s: %v", nodeFile, statErr)
	}
	if info.Size() == 0 {
		t.Fatalf("expected non-empty %s", nodeFile)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("%s perm = %#o, want 0600", nodeFile, info.Mode().Perm())
	}
}

func s4AssertInviteConsumed(t *testing.T, h *hub, code string) {
	t.Helper()
	reuseState := filepath.Join(t.TempDir(), "reuse-state")
	proc := startProcess(
		t,
		h.binary,
		"join",
		"--hub", h.url,
		"--code", code,
		"--ca", h.caPath,
		"--state-dir", reuseState,
	)
	requireNonZeroProcessExit(t, proc, statusTimeout)
}

func s4AssertServiceArtifacts(t *testing.T, svcEnv s4ServiceEnv, code string) {
	t.Helper()
	shimLog := s4ReadShimLog(t, svcEnv.logFile)
	if strings.Contains(shimLog, code) {
		t.Fatalf("shim log leaked one-time invite code %q:\n%s", code, shimLog)
	}
	switch runtime.GOOS {
	case "darwin":
		s4AssertDarwinService(t, svcEnv.unitPath, shimLog, code)
	case "linux":
		s4AssertLinuxService(t, svcEnv.unitPath, shimLog, code)
	case "windows":
		s4RequireContainsAll(t, "windows sc shim log", shimLog, []string{
			"DeviceLink", "join", "--hub", "--state-dir", "--ca", "start",
		})
	default:
		t.Fatalf("unsupported GOOS %s", runtime.GOOS)
	}
}

func s4AssertDarwinService(t *testing.T, unitPath, shimLog, code string) {
	t.Helper()
	unitContent := s4ReadNonEmptyFile(t, unitPath)
	if strings.Contains(unitContent, code) {
		t.Fatalf("launchd plist leaked one-time invite code %q:\n%s", code, unitContent)
	}
	s4RequireContainsAll(t, unitPath, unitContent, []string{
		"io.devicelink.node", "RunAtLoad", "KeepAlive", "join", "--hub", "--state-dir", "--ca",
	})
	s4RequireContainsAll(t, "darwin launchctl shim log", shimLog, []string{
		"bootstrap", "kickstart",
	})
}

func s4AssertLinuxService(t *testing.T, unitPath, shimLog, code string) {
	t.Helper()
	unitContent := s4ReadNonEmptyFile(t, unitPath)
	if strings.Contains(unitContent, code) {
		t.Fatalf("systemd unit leaked one-time invite code %q:\n%s", code, unitContent)
	}
	s4RequireContainsAll(t, unitPath, unitContent, []string{
		"ExecStart=", "Restart=always", "join", "--hub", "--state-dir", "--ca",
	})
	s4RequireContainsAll(t, "linux systemctl shim log", shimLog, []string{
		"daemon-reload", "enable --now device-link.service",
	})
}

func s4VerifyEnrolledServiceBoot(t *testing.T, h *hub, stateDir string) {
	t.Helper()
	proc := startProcess(
		t,
		h.binary,
		"join",
		"--hub", h.url,
		"--state-dir", stateDir,
		"--ca", h.caPath,
	)
	defer proc.stop()
	h.waitDevice(t, "svc-node", "alive")
	execOut, execErr := h.runController(t, "exec", "svc-node", "echo stage4-service-ok")
	if execErr != nil {
		t.Fatalf("exec on svc-node: %v\n%s", execErr, execOut)
	}
	if !strings.Contains(execOut, "stage4-service-ok") {
		t.Fatalf("exec output = %q, want stage4-service-ok", execOut)
	}
}

func s4BuildReleaseTarGz(t *testing.T, binaryPath string) s4ReleaseArchive {
	t.Helper()
	rawBin, readErr := os.ReadFile(binaryPath)
	if readErr != nil {
		t.Fatalf("read dl binary %s: %v", binaryPath, readErr)
	}
	archiveBytes := s4ArchiveBinaryBytes(t, rawBin)
	archiveName := fmt.Sprintf("dl_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(t.TempDir(), archiveName)
	if writeErr := os.WriteFile(archivePath, archiveBytes, 0o644); writeErr != nil {
		t.Fatalf("write release archive %s: %v", archivePath, writeErr)
	}
	sum := sha256.Sum256(archiveBytes)
	return s4ReleaseArchive{
		name:   archiveName,
		path:   archivePath,
		digest: hex.EncodeToString(sum[:]),
	}
}

func s4ArchiveBinaryBytes(t *testing.T, rawBin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	defer func() { _ = gzw.Close() }()
	tw := tar.NewWriter(gzw)
	defer func() { _ = tw.Close() }()
	hdr := &tar.Header{
		Name:     "dl",
		Mode:     0o755,
		Size:     int64(len(rawBin)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Unix(1700000000, 0),
	}
	if hdrErr := tw.WriteHeader(hdr); hdrErr != nil {
		t.Fatalf("write tar header: %v", hdrErr)
	}
	if _, copyErr := tw.Write(rawBin); copyErr != nil {
		t.Fatalf("write tar payload: %v", copyErr)
	}
	if closeTarErr := tw.Close(); closeTarErr != nil {
		t.Fatalf("close tar writer: %v", closeTarErr)
	}
	if closeGzErr := gzw.Close(); closeGzErr != nil {
		t.Fatalf("close gzip writer: %v", closeGzErr)
	}
	return buf.Bytes()
}

func s4VerifyInstallChecksumMismatch(t *testing.T, scriptPath string, archive s4ReleaseArchive) {
	t.Helper()
	badHex := strings.Repeat("0", 64)
	if archive.digest == badHex {
		badHex = strings.Repeat("f", 64)
	}
	srv := httptest.NewServer(s4ReleaseMux(archive, badHex))
	defer srv.Close()

	badBinDir := filepath.Join(t.TempDir(), "bad-bin")
	svcEnv := s4NewServiceEnv(t)
	env := s4SetEnv(svcEnv.env, "DL_BASE_URL", srv.URL)
	env = s4SetEnv(env, "DL_INSTALL_DIR", badBinDir)

	res, runErr := s4RunEnvCLI(t, env, "sh", scriptPath)
	requireFailure(t, res.combined, runErr)
	s4RequireFileAbsent(t, filepath.Join(badBinDir, "dl"))
}

func s4VerifyInstallAndServiceJoin(t *testing.T, h *hub, scriptPath string, archive s4ReleaseArchive) {
	t.Helper()
	srv := httptest.NewServer(s4ReleaseMux(archive, archive.digest))
	defer srv.Close()

	goodBinDir := filepath.Join(t.TempDir(), "good-bin")
	stateDir := filepath.Join(t.TempDir(), "install-state")
	svcEnv := s4NewServiceEnv(t)
	env := s4SetEnv(svcEnv.env, "DL_BASE_URL", srv.URL)
	env = s4SetEnv(env, "DL_INSTALL_DIR", goodBinDir)

	joinArgs := h.invite(t, "install-node")
	code := s4FlagValue(t, joinArgs, "--code")
	res, runErr := s4RunEnvCLI(
		t,
		env,
		"sh", scriptPath, "--",
		"join", "--service",
		"--hub", h.url,
		"--code", code,
		"--ca", h.caPath,
		"--state-dir", stateDir,
	)
	if runErr != nil {
		t.Fatalf("install.sh -- join --service failed: %v\n%s", runErr, res.combined)
	}
	if !strings.Contains(res.stdout, "dl service installed:") {
		t.Fatalf("stdout = %q, want substring %q", res.stdout, "dl service installed:")
	}
	s4AssertExecutableFile(t, filepath.Join(goodBinDir, "dl"))
	s4AssertNodeStateFile(t, stateDir)
}

func s4ReleaseMux(archive s4ReleaseArchive, checksumHex string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/"+archive.name, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archive.path)
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "%s  %s\n", checksumHex, archive.name)
	})
	return mux
}

func s4RepoRoot(t *testing.T) string {
	t.Helper()
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		t.Fatalf("getwd: %v", wdErr)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func s4FlagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] != "" {
			return args[i+1]
		}
	}
	t.Fatalf("flag %s not found in %v", flag, args)
	return ""
}

func s4EnvLookup(env []string, key string) string {
	for _, entry := range env {
		entryKey, val, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(entryKey, key) {
			return val
		}
	}
	return ""
}

func s4SetEnv(env []string, key, value string) []string {
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

func s4ReadShimLog(t *testing.T, logFile string) string {
	t.Helper()
	raw, readErr := os.ReadFile(logFile)
	if errors.Is(readErr, os.ErrNotExist) {
		return ""
	}
	if readErr != nil {
		t.Fatalf("read shim log %s: %v", logFile, readErr)
	}
	return string(raw)
}

func s4ReadNonEmptyFile(t *testing.T, path string) string {
	t.Helper()
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read %s: %v", path, readErr)
	}
	content := string(raw)
	if strings.TrimSpace(content) == "" {
		t.Fatalf("expected non-empty file %s", path)
	}
	return content
}

func s4AssertExecutableFile(t *testing.T, path string) {
	t.Helper()
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stat %s: %v", path, statErr)
	}
	if info.IsDir() || info.Size() == 0 {
		t.Fatalf("expected non-empty regular binary at %s", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s mode = %#o, want +x", path, info.Mode().Perm())
	}
}

func s4RequireFileAbsent(t *testing.T, path string) {
	t.Helper()
	_, statErr := os.Stat(path)
	if statErr == nil {
		t.Fatalf("expected %s not to exist", path)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat %s: %v", path, statErr)
	}
}

func s4RequireContainsAll(t *testing.T, label, content string, substrings []string) {
	t.Helper()
	for _, sub := range substrings {
		if !strings.Contains(content, sub) {
			t.Fatalf("%s missing %q in:\n%s", label, sub, content)
		}
	}
}

func s4RequireContainsAny(t *testing.T, label, content string, options []string) {
	t.Helper()
	for _, opt := range options {
		if strings.Contains(content, opt) {
			return
		}
	}
	t.Fatalf("%s missing any of %v in:\n%s", label, options, content)
}
