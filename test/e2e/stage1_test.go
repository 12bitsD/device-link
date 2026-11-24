package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/12bitsD/device-link/internal/client"
)

func TestStage1HubBootAndEmptyLS(t *testing.T) {
	h := newHub(t)
	output := mustController(t, h, "ls", "--json")
	if strings.TrimSpace(output) != "[]" {
		t.Fatalf("new hub ls output = %q, want []", output)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "key.pem")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(h.stateDir, "controller_token"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("controller token mode = %o, want 600", info.Mode().Perm())
	}
	output, err = runCLI(t, h.binary, "ls", "--json", "--hub", h.url, "--ca", h.caPath)
	requireFailure(t, output, err)
	output, err = runCLI(t, h.binary, "ls", "--json", "--hub", h.url, "--token", "invalid", "--ca", h.caPath)
	requireFailure(t, output, err)
}

func TestStage1OneTimeInviteAndJoin(t *testing.T) {
	h := newHub(t)
	joinArgs := h.invite(t, "mac")
	stateDir := t.TempDir()
	args := append(append([]string{}, joinArgs...), "--state-dir", stateDir)
	process := startProcess(t, h.binary, args...)
	n := &node{binary: h.binary, hubURL: h.url, caPath: h.caPath, stateDir: stateDir, process: process}
	joined := h.waitDevice(t, "mac", "alive")
	if joined.OS != runtime.GOOS || joined.Arch != runtime.GOARCH {
		t.Fatalf("platform = %s/%s, want %s/%s", joined.OS, joined.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if joined.Shell == "" {
		t.Fatal("joined device has an empty shell")
	}
	if joined.ExpiresAt != nil {
		t.Fatalf("expires_at = %q, want null", *joined.ExpiresAt)
	}
	if _, err := time.Parse(time.RFC3339, joined.LastSeen); err != nil {
		t.Fatalf("invalid last_seen %q: %v", joined.LastSeen, err)
	}
	if n.deviceToken(t) == "" {
		t.Fatal("node did not persist its device token")
	}
	second := h.joinAgain(t, joinArgs)
	waitProcessExit(t, second.process)
	if second.process.exitError() == nil {
		t.Fatalf("reused invite unexpectedly succeeded\n%s", second.process.output())
	}
}

func TestStage1RoleIsolation(t *testing.T) {
	h := newHub(t)
	n := h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")
	client := trustedHTTPClient(t, h.caPath)
	token := n.deviceToken(t)
	requests := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v1/devices"},
		{method: http.MethodPost, path: "/v1/invites"},
		{method: http.MethodDelete, path: "/v1/devices/mac"},
	}
	for _, request := range requests {
		call := apiRequest{method: request.method, targetURL: h.url + request.path, token: token}
		assertAPIStatus(t, client, call, http.StatusUnauthorized, http.StatusForbidden)
	}
}

func TestStage1NamespaceCollisionAndReconnectReclaim(t *testing.T) {
	h := newHub(t)
	first := h.joinWithInvite(t, "mac")
	h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")
	h.waitDevice(t, "mac-2", "alive")
	first.process.stop()
	h.waitDevice(t, "mac", "missed")
	h.waitDevice(t, "mac-2", "alive")
	restarted := h.restartNode(t, first)
	h.waitDevice(t, "mac", "alive")
	requireAbsent(t, h.devices(t), "mac-3")
	if restarted.process.exited() {
		t.Fatalf("restarted node exited: %v\n%s", restarted.process.exitError(), restarted.process.output())
	}
}

func TestStage1NetworkDropAndResume(t *testing.T) {
	h := newHub(t)
	proxy := newTCPProxy(t, strings.TrimPrefix(h.url, "https://"))
	joinArgs := h.invite(t, "mac")
	joinArgs = replaceFlag(joinArgs, "--hub", "https://"+proxy.address)
	stateDir := t.TempDir()
	joinArgs = append(joinArgs, "--state-dir", stateDir)
	startProcess(t, h.binary, joinArgs...)
	h.waitDevice(t, "mac", "alive")
	if err := proxy.Drop(); err != nil {
		t.Fatal(err)
	}
	h.waitDevice(t, "mac", "missed")
	if err := proxy.Resume(); err != nil {
		t.Fatal(err)
	}
	h.waitDevice(t, "mac", "alive")
	requireAbsent(t, h.devices(t), "mac-2")
}

func TestStage1TTLAndRevoke(t *testing.T) {
	h := newHub(t)
	first := h.joinWithInvite(t, "mac")
	second := h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")
	h.waitDevice(t, "mac-2", "alive")
	mustController(t, h, "ttl", "mac", "2s")
	withTTL := h.waitDevice(t, "mac", "alive")
	requireFutureExpiry(t, withTTL.ExpiresAt, 0, 10*time.Second)
	h.waitDevice(t, "mac", "missed")
	requireNonZeroProcessExit(t, first.process, statusTimeout)
	requireDevice(t, h.devices(t), "mac", "missed")
	mustController(t, h, "ttl", "mac-2", "1h")
	longTTL := h.waitDevice(t, "mac-2", "alive")
	requireFutureExpiry(t, longTTL.ExpiresAt, 55*time.Minute, 65*time.Minute)
	mustController(t, h, "ttl", "mac-2", "never")
	forever := h.waitDevice(t, "mac-2", "alive")
	if forever.ExpiresAt != nil {
		t.Fatalf("expires_at = %q after never, want null", *forever.ExpiresAt)
	}
	mustController(t, h, "rm", "mac-2")
	waitFor(t, statusTimeout, func() (bool, error) {
		for _, candidate := range h.devices(t) {
			if candidate.Name == "mac-2" {
				return false, nil
			}
		}
		return true, nil
	})
	second.process.stop()
	restarted := h.restartNode(t, second)
	requireNonZeroProcessExit(t, restarted.process, statusTimeout)
	requireAbsent(t, h.devices(t), "mac-2")
}

func mustController(t *testing.T, h *hub, args ...string) string {
	t.Helper()
	output, err := h.runController(t, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, output)
	}
	return output
}

func requireFutureExpiry(t *testing.T, value *string, minimum, maximum time.Duration) {
	t.Helper()
	if value == nil {
		t.Fatal("expires_at is null")
	}
	expires, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		t.Fatalf("invalid expires_at %q: %v", *value, err)
	}
	remaining := time.Until(expires)
	if remaining < minimum || remaining > maximum {
		t.Fatalf("expires_at is %s away, want between %s and %s", remaining, minimum, maximum)
	}
}

func trustedHTTPClient(t *testing.T, caPath string) *http.Client {
	t.Helper()
	httpClient, err := client.HTTPClient(caPath)
	if err != nil {
		t.Fatal(err)
	}
	httpClient.Timeout = commandTimeout
	return httpClient
}

func replaceFlag(args []string, flag, value string) []string {
	result := append([]string{}, args...)
	for i := 0; i+1 < len(result); i++ {
		if result[i] == flag {
			result[i+1] = value
			return result
		}
	}
	return append(result, flag, value)
}
