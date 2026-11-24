package e2e

import (
	"context"
	"database/sql"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	// Register pure-Go SQLite driver for fault-injection tests.
	_ "modernc.org/sqlite"
)

func TestStage1CorruptNodeStateFailsClosed(t *testing.T) {
	h := newHub(t)
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "node.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := runCLI(t, h.binary, "join", "--hub", h.url, "--ca", h.caPath, "--state-dir", stateDir)
	requireFailure(t, output, err)
	if !strings.Contains(output, "decode node state") {
		t.Fatalf("expected decode node state error, got: %s", output)
	}
	joinArgs := h.invite(t, "mac")
	joinArgs = append(joinArgs, "--state-dir", stateDir)
	output, err = runCLI(t, h.binary, joinArgs...)
	requireFailure(t, output, err)
	requireAbsent(t, h.devices(t), "mac")
}

func TestStage1ControllerReadsTokenFromStateDir(t *testing.T) {
	h := newHub(t)
	output, err := runCLI(t, h.binary, "ls", "--json", "--hub", h.url, "--state-dir", h.stateDir)
	if err != nil {
		t.Fatalf("ls with --state-dir failed: %v\n%s", err, output)
	}
	if strings.TrimSpace(output) != "[]" {
		t.Fatalf("ls output = %q, want []", output)
	}
	output, err = runCLI(t, h.binary, "ls", "--json", "--hub", h.url, "--state-dir", h.stateDir, "--token", "wrong")
	requireFailure(t, output, err)
}

func TestStage1HubAdvertisedHostSAN(t *testing.T) {
	host := nonLoopbackIPv4()
	if host == "" {
		t.Skip("no non-loopback IPv4 interface available")
	}
	binary := os.Getenv(binaryEnv)
	stateDir := t.TempDir()
	proc := startProcess(t, binary, "hub", "--addr", host+":0", "--host", host, "--state-dir", stateDir)
	h := &hub{binary: binary, stateDir: stateDir, caPath: filepath.Join(stateDir, "cert.pem"), process: proc}
	waitHubBoundAddress(t, h, host)
	t.Logf("probed advertised host %s", h.url)
	output, err := runCLI(t, binary, "ls", "--json", "--hub", h.url, "--state-dir", stateDir)
	if err != nil {
		t.Fatalf("ls via advertised host failed: %v\n%s", err, output)
	}
}

func nonLoopbackIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagBroadcast == 0 {
			continue
		}
		if ip := broadcastIPv4(iface); ip != "" {
			return ip
		}
	}
	return ""
}

func broadcastIPv4(iface net.Interface) string {
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

func TestStage1NegativeTTLRejectedAndSubsecondCeil(t *testing.T) {
	h := newHub(t)
	h.joinWithInvite(t, "mac")
	h.waitDevice(t, "mac", "alive")
	client := trustedHTTPClient(t, h.caPath)
	call := apiRequest{
		method: http.MethodPut, targetURL: h.url + "/v1/devices/mac/ttl",
		token: h.token, body: `{"ttl_seconds":-5}`,
	}
	assertAPIStatus(t, client, call, http.StatusBadRequest)
	mustController(t, h, "ttl", "mac", "2500ms")
	dev := h.waitDevice(t, "mac", "alive")
	requireFutureExpiry(t, dev.ExpiresAt, 1500*time.Millisecond, 3500*time.Millisecond)
}

func TestStage1DatabaseFailureReturns500(t *testing.T) {
	h := newHub(t)
	db, err := sql.Open("sqlite", filepath.Join(h.stateDir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("DROP TABLE devices"); err != nil {
		t.Fatal(err)
	}
	client := trustedHTTPClient(t, h.caPath)
	call := apiRequest{method: http.MethodDelete, targetURL: h.url + "/v1/devices/mac", token: h.token}
	assertAPIStatus(t, client, call, http.StatusInternalServerError)
	if _, err := db.Exec("DROP TABLE settings"); err != nil {
		t.Fatal(err)
	}
	call = apiRequest{method: http.MethodGet, targetURL: h.url + "/v1/devices", token: h.token}
	assertAPIStatus(t, client, call, http.StatusInternalServerError)
}

func TestStage1InviteQuotesPathsWithSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh quoting test")
	}
	h := newHub(t)
	spaceDir := filepath.Join(t.TempDir(), "ca dir")
	if err := os.MkdirAll(spaceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	spaceCA := filepath.Join(spaceDir, "hub cert.pem")
	data, err := os.ReadFile(h.caPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spaceCA, data, 0o600); err != nil {
		t.Fatal(err)
	}
	output := mustController(t, h, "invite", "--name", "spaced", "--ca", spaceCA)
	nodeDir := filepath.Join(t.TempDir(), "node dir")
	script := fmt.Sprintf("export PATH=%q:$PATH; %s --state-dir %q & pid=$!; sleep 2; kill -TERM $pid; wait $pid", filepath.Dir(h.binary), strings.TrimSpace(output), nodeDir)
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = processEnvironment(t)
	prepareProcess(cmd)
	t.Cleanup(func() { _ = killProcessTree(cmd) })
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("quoted join failed: %v\n%s", err, combined)
	}
	h.waitDevice(t, "spaced", "missed")
}

func TestStage1NodeAnswersWebSocketClosePromptly(t *testing.T) {
	closed := make(chan time.Duration, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/join", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"device_token":"tok","name":"fake"}`))
	})
	mux.HandleFunc("/v1/node", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		start := time.Now()
		_ = conn.Close(websocket.StatusGoingAway, "shutdown")
		select {
		case closed <- time.Since(start):
		default:
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	der := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, der, 0o600); err != nil {
		t.Fatal(err)
	}
	startProcess(t, os.Getenv(binaryEnv), "join", "--hub", srv.URL, "--code", "abc", "--ca", caPath, "--state-dir", t.TempDir())
	select {
	case elapsed := <-closed:
		if elapsed > 2*time.Second {
			t.Fatalf("WebSocket close handshake took %s (node is not reading close frames)", elapsed)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("fake hub did not reach /v1/node")
	}
}

func waitHubBoundAddress(t *testing.T, h *hub, host string) {
	t.Helper()
	waitFor(t, readinessTimout, func() (bool, error) {
		if h.process.exited() {
			return false, fmt.Errorf("hub exited: %s", h.process.output())
		}
		port := readBoundPort(h.process.output())
		if port == "" {
			return false, nil
		}
		h.url = "https://" + host + ":" + port
		return h.readTokenAndProbe(), nil
	})
}

func readBoundPort(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "DL_ADDR=") {
			addr := strings.TrimPrefix(line, "DL_ADDR=")
			if idx := strings.LastIndex(addr, ":"); idx >= 0 {
				return addr[idx+1:]
			}
		}
	}
	return ""
}
