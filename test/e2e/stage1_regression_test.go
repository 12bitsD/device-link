package e2e

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStage1HubRestartPreservesControllerToken(t *testing.T) {
	h := newHub(t)
	originalToken := h.token
	n := h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")

	addr := strings.TrimPrefix(h.url, "https://")
	h.process.stop()
	h.process = startProcess(t, h.binary, "hub", "--addr", addr, "--state-dir", h.stateDir)
	waitHubReady(t, h)

	if h.token != originalToken {
		t.Fatalf("restarted hub overwrote controller_token: got %q, want %q", h.token, originalToken)
	}
	h.waitDevice(t, "mac", "alive")
	if n.process.exited() {
		t.Fatalf("node exited across hub restart: %v\n%s", n.process.exitError(), n.process.output())
	}
}

func TestStage1CrossPrefixNameCollision(t *testing.T) {
	h := newHub(t)
	h.joinWithInvite(t, "mac")
	h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")
	h.waitDevice(t, "mac-2", "alive")

	explicit := h.joinWithInvite(t, "mac-2")
	h.waitDevice(t, "mac-2-2", "alive")
	if explicit.process.exited() {
		t.Fatalf("mac-2 invite join failed: %v\n%s", explicit.process.exitError(), explicit.process.output())
	}

	h.joinWithInvite(t, "box-2")
	h.waitDevice(t, "box-2", "alive")
	h.joinWithInvite(t, "box")
	h.waitDevice(t, "box", "alive")
	h.joinWithInvite(t, "box")
	h.waitDevice(t, "box-3", "alive")
}

func TestStage1ValidationAndAuthHardening(t *testing.T) {
	h := newHub(t)
	verifyHTTPValidationAndBearerScheme(t, h)
	verifyCLIValidationAndSecretScrub(t, h)
	verifyJoinHubURLAndKeyPermissions(t, h)
}

func verifyHTTPValidationAndBearerScheme(t *testing.T, h *hub) {
	t.Helper()
	httpClient := trustedHTTPClient(t, h.caPath)
	cases := []struct {
		name   string
		method string
		path   string
		auth   string
		body   string
		status int
	}{
		{name: "slash in device name", method: http.MethodPost, path: "/v1/invites", auth: "Bearer " + h.token, body: `{"name":"foo/bar"}`, status: http.StatusBadRequest},
		{name: "reserved local name", method: http.MethodPost, path: "/v1/invites", auth: "Bearer " + h.token, body: `{"name":"local"}`, status: http.StatusBadRequest},
		{name: "negative invite ttl", method: http.MethodPost, path: "/v1/invites", auth: "Bearer " + h.token, body: `{"name":"mac","ttl_seconds":-5}`, status: http.StatusBadRequest},
		{name: "overflow invite ttl", method: http.MethodPost, path: "/v1/invites", auth: "Bearer " + h.token, body: `{"name":"mac","ttl_seconds":99999999999}`, status: http.StatusBadRequest},
		{name: "schemeless authorization", method: http.MethodGet, path: "/v1/devices", auth: h.token, status: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, h.url+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", tc.auth)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("%s: got %d, want %d", tc.name, resp.StatusCode, tc.status)
			}
		})
	}
}

func verifyCLIValidationAndSecretScrub(t *testing.T, h *hub) {
	t.Helper()
	cmdOut, cmdErr := runCLIWithEnv(t, h.binary, []string{"DL_TOKEN=top-secret-value"}, "ls", "--unknown-flag")
	requireFailure(t, cmdOut, cmdErr)
	if strings.Contains(cmdOut, "top-secret-value") {
		t.Fatalf("flag error leaked DL_TOKEN value:\n%s", cmdOut)
	}
	out, err := h.runController(t, "ls", "extra-arg")
	requireFailure(t, out, err)
	out, err = runCLI(t, h.binary, "ls", "--json", "--hub", h.url+"/", "--token", h.token, "--ca", h.caPath)
	if err != nil {
		t.Fatalf("trailing-slash --hub failed: %v\n%s", err, out)
	}
}

func verifyJoinHubURLAndKeyPermissions(t *testing.T, h *hub) {
	t.Helper()
	n := h.joinWithInvite(t, "saved-node")
	h.waitDevice(t, "saved-node", "alive")
	n.process.stop()

	missingHub := startProcess(t, h.binary, "join", "--ca", h.caPath, "--state-dir", n.stateDir)
	requireNonZeroProcessExit(t, missingHub, 3*time.Second)

	if runtime.GOOS == "windows" {
		return
	}
	keyPath := filepath.Join(h.stateDir, "key.pem")
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(h.url, "https://")
	h.process.stop()
	h.process = startProcess(t, h.binary, "hub", "--addr", addr, "--state-dir", h.stateDir)
	waitHubReady(t, h)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key.pem mode after restart = %o, want 600", info.Mode().Perm())
	}
}

func runCLIWithEnv(t *testing.T, binary string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(processEnvironment(t), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
