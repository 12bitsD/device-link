package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStage4RegressionPreExistingNodePermAndControlChars(t *testing.T) {
	h := newHub(t)
	svcEnv := s4NewServiceEnv(t)

	badJoinArgs := h.invite(t, "newline-guard")
	goodCode := s4FlagValue(t, badJoinArgs, "--code")
	newlineDir := filepath.Join(t.TempDir(), "bad\nExecStartPre=evil")
	badRes, badErr := s4RunEnvCLI(
		t, svcEnv.env, h.binary,
		"join", "--service",
		"--hub", h.url,
		"--code", goodCode,
		"--ca", h.caPath,
		"--state-dir", newlineDir,
	)
	requireFailure(t, badRes.combined, badErr)

	stateDir := filepath.Join(t.TempDir(), "reused-code-state")
	okRes, okErr := s4RunEnvCLI(
		t, svcEnv.env, h.binary,
		"join", "--service",
		"--hub", h.url,
		"--code", goodCode,
		"--ca", h.caPath,
		"--state-dir", stateDir,
	)
	if okErr != nil {
		t.Fatalf("one-time code was burned before path validation: %v\n%s", okErr, okRes.combined)
	}

	if runtime.GOOS == "windows" {
		return
	}
	nodeFile := filepath.Join(stateDir, "node.json")
	if err := os.Chmod(nodeFile, 0o644); err != nil {
		t.Fatal(err)
	}
	reRes, reErr := s4RunEnvCLI(
		t, svcEnv.env, h.binary,
		"join", "--service",
		"--hub", h.url,
		"--ca", h.caPath,
		"--state-dir", stateDir,
	)
	if reErr != nil {
		t.Fatalf("re-install service failed: %v\n%s", reErr, reRes.combined)
	}
	s4AssertNodeStateFile(t, stateDir)
}

func TestStage4RegressionInstallScriptHardening(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets POSIX shells on darwin and linux")
	}
	scriptPath := filepath.Join(s4RepoRoot(t), "scripts", "install.sh")
	s4VerifySymlinkArchiveRejected(t, scriptPath)
	s4VerifyCRLFCommentChecksumAndRunningUpgrade(t, scriptPath)
}

func s4VerifySymlinkArchiveRejected(t *testing.T, scriptPath string) {
	t.Helper()
	secretPath := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secretPath, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := s4BuildSymlinkTarGz(t, secretPath)
	srv := httptest.NewServer(s4ReleaseMux(archive, archive.digest))
	defer srv.Close()

	binDir := filepath.Join(t.TempDir(), "sym-bin")
	svcEnv := s4NewServiceEnv(t)
	env := s4SetEnv(svcEnv.env, "DL_BASE_URL", srv.URL)
	env = s4SetEnv(env, "DL_INSTALL_DIR", binDir)
	res, err := s4RunEnvCLI(t, env, "sh", scriptPath)
	requireFailure(t, res.combined, err)
	s4RequireFileAbsent(t, filepath.Join(binDir, "dl"))
}

func s4BuildSymlinkTarGz(t *testing.T, linkTarget string) s4ReleaseArchive {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	hdr := &tar.Header{
		Name:     "dl",
		Linkname: linkTarget,
		Mode:     0o777,
		Typeflag: tar.TypeSymlink,
		ModTime:  time.Unix(1700000000, 0),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	archiveName := fmt.Sprintf("dl_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(t.TempDir(), archiveName)
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return s4ReleaseArchive{name: archiveName, path: archivePath, digest: hex.EncodeToString(sum[:])}
}

func s4VerifyCRLFCommentChecksumAndRunningUpgrade(t *testing.T, scriptPath string) {
	t.Helper()
	h := newHub(t)
	archive := s4BuildReleaseTarGz(t, h.binary)
	mux := http.NewServeMux()
	mux.HandleFunc("/"+archive.name, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archive.path)
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "# %s\r\n%s  %s\r\n", archive.name, strings.ToUpper(archive.digest), archive.name)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	binDir := filepath.Join(t.TempDir(), "upgrade-bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	installedBin := filepath.Join(binDir, "dl")
	rawBin, err := os.ReadFile(h.binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installedBin, rawBin, 0o755); err != nil {
		t.Fatal(err)
	}
	runningHub := startProcess(t, installedBin, "hub", "--addr", freeAddress(t), "--state-dir", t.TempDir())
	defer runningHub.stop()

	svcEnv := s4NewServiceEnv(t)
	env := s4SetEnv(svcEnv.env, "DL_BASE_URL", srv.URL)
	env = s4SetEnv(env, "DL_INSTALL_DIR", binDir)
	res, runErr := s4RunEnvCLI(t, env, "sh", scriptPath)
	if runErr != nil {
		t.Fatalf("upgrade install over running binary failed: %v\n%s", runErr, res.combined)
	}
	s4AssertExecutableFile(t, installedBin)
}

func TestStage4RegressionWindowsCIPwshGuard(t *testing.T) {
	ciPath := filepath.Join(s4RepoRoot(t), ".github", "workflows", "ci.yml")
	content := s4ReadNonEmptyFile(t, ciPath)
	if !strings.Contains(content, "$PSNativeCommandUseErrorActionPreference = $true") {
		t.Fatalf("%s missing PowerShell native error preference guard", ciPath)
	}
}

func TestStage4RegressionQuotedInviteAndForeignBitsCLI(t *testing.T) {
	h := newHub(t)
	spacedDir := filepath.Join(t.TempDir(), "ca with space")
	if err := os.MkdirAll(spacedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	caBytes, err := os.ReadFile(h.caPath)
	if err != nil {
		t.Fatal(err)
	}
	spacedCA := filepath.Join(spacedDir, "cert.pem")
	if err := os.WriteFile(spacedCA, caBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	h.caPath = spacedCA
	_ = h.joinWithInvite(t, "quoted-ca-node")
	h.waitDevice(t, "quoted-ca-node", "alive")

	winInvite := `dl join --hub https://127.0.0.1:8443 --code abc123 --ca "C:\Users\runneradmin\AppData\Local\Temp\Test dir\cert.pem"`
	parsed, parseErr := splitCommandFields(winInvite)
	wantCA := `C:\Users\runneradmin\AppData\Local\Temp\Test dir\cert.pem`
	if parseErr != nil || len(parsed) != 8 || parsed[7] != wantCA {
		t.Fatalf("parsed windows invite = %q (err=%v), want [7]=%q", parsed, parseErr, wantCA)
	}

	verifyPath := filepath.Join(s4RepoRoot(t), "scripts", "verify.sh")
	verifyContent := s4ReadNonEmptyFile(t, verifyPath)
	if !strings.Contains(verifyContent, "Usage: bits style") {
		t.Fatalf("%s must verify 'Usage: bits style' output because util-linux /usr/bin/bits exits 0 on --help", verifyPath)
	}
}
