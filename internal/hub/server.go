package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/12bitsD/device-link/internal/client"
	"github.com/12bitsD/device-link/internal/registry"
)

type Config struct {
	Addr     string
	Host     string
	StateDir string
}

type Server struct {
	store         *registry.Store
	mu            sync.Mutex
	live          map[int64]*nodeSession
	onlineNotify  chan struct{}
	jobWaiters    map[jobWaiterKey]chan nodeFrame
	transfers     map[string]*hubTransfer
	transferOrder []string
	closing       bool
	wg            sync.WaitGroup
}

func Run(ctx context.Context, config Config) error {
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return fmt.Errorf("create hub state: %w", err)
	}
	store, err := registry.Open(ctx, filepath.Join(config.StateDir, "hub.db"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := initializeState(ctx, config.StateDir, config.Host, store); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", config.Addr, err)
	}
	fmt.Printf("DL_ADDR=%s\n", listener.Addr())
	server := &Server{store: store}
	return serve(ctx, listener, server, config.StateDir)
}

func initializeState(ctx context.Context, stateDir, host string, store *registry.Store) error {
	token, err := ensureControllerToken(ctx, filepath.Join(stateDir, "controller_token"), store)
	if err != nil {
		return err
	}
	if err := ensureCertificate(stateDir, host); err != nil {
		return err
	}
	fmt.Printf("DL_CONTROLLER_TOKEN=%s\n", token)
	return nil
}

func ensureControllerToken(ctx context.Context, tokenPath string, store *registry.Store) (string, error) {
	raw, err := os.ReadFile(tokenPath)
	if err == nil {
		token := strings.TrimSpace(string(raw))
		ok, checkErr := store.HasControllerToken(ctx, token)
		if checkErr != nil {
			return "", checkErr
		}
		if !ok {
			return "", fmt.Errorf("controller token in %s does not match registry", tokenPath)
		}
		if chmodErr := os.Chmod(tokenPath, 0o600); chmodErr != nil {
			return "", fmt.Errorf("secure controller token: %w", chmodErr)
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read controller token: %w", err)
	}
	token, err := store.MintControllerToken(ctx)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write controller token: %w", err)
	}
	if err := os.Chmod(tokenPath, 0o600); err != nil {
		return "", fmt.Errorf("secure controller token: %w", err)
	}
	return token, nil
}

func serve(ctx context.Context, listener net.Listener, server *Server, stateDir string) error {
	httpServer := &http.Server{
		Handler:           server.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	serveErr := make(chan error, 1)
	go func() {
		defer close(serveErr)
		serveErr <- httpServer.ServeTLS(listener, filepath.Join(stateDir, "cert.pem"), filepath.Join(stateDir, "key.pem"))
	}()
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve HTTPS: %w", err)
		}
	}
	server.closeConnections()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
	}
	server.wg.Wait()
	<-serveErr
	return runErr
}

func (s *Server) routes() http.Handler {
	controllerMux := http.NewServeMux()
	controllerMux.HandleFunc("POST /v1/invites", s.handleInvites)
	controllerMux.HandleFunc("GET /v1/devices", s.handleDevices)
	controllerMux.HandleFunc("PUT /v1/devices/{name}/ttl", s.handleTTL)
	controllerMux.HandleFunc("DELETE /v1/devices/{name}", s.handleDelete)
	controllerMux.HandleFunc("POST /v1/devices/{name}/exec", s.handleExec)
	controllerMux.HandleFunc("GET /v1/jobs/{id}", s.handleJobStatus)
	controllerMux.HandleFunc("GET /v1/jobs/{id}/logs", s.handleJobLogs)
	controllerMux.HandleFunc("POST /v1/jobs/{id}/kill", s.handleJobKill)
	controllerMux.HandleFunc("POST /v1/transfers/scan", s.handleTransferScan)
	controllerMux.HandleFunc("POST /v1/transfers", s.handleStartTransfer)
	controllerMux.HandleFunc("PUT /v1/transfers/{id}/upload", s.handleControllerUpload)
	controllerMux.HandleFunc("GET /v1/transfers/{id}/download", s.handleControllerDownload)
	controllerMux.HandleFunc("POST /v1/transfers/{id}/complete", s.handleControllerComplete)
	controllerMux.HandleFunc("GET /v1/transfers/{id}", s.handleTransferStatus)
	controllerMux.HandleFunc("POST /v1/transfers/{id}/cancel", s.handleTransferCancel)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/join", s.handleJoin)
	mux.HandleFunc("/v1/node", s.handleNode)
	mux.HandleFunc("PUT /v1/node-transfers/{id}/upload", s.handleNodeUpload)
	mux.HandleFunc("GET /v1/node-transfers/{id}/download", s.handleNodeDownload)
	mux.HandleFunc("POST /v1/node-transfers/{id}/done", s.handleNodeTransferDone)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizeController(w, r) {
			return
		}
		controllerMux.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.beginRequest() {
			writeError(w, http.StatusServiceUnavailable, "hub is shutting down")
			return
		}
		defer s.wg.Done()
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) authorizeController(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.store.HasControllerToken(r.Context(), bearerToken(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not verify controller token")
		return false
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "controller token required")
		return false
	}
	return true
}

func bearerToken(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return token
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	return json.NewDecoder(r.Body).Decode(dst)
}

func validDeviceName(name string) bool {
	if name == "" || len(name) > 63 || name == "local" {
		return false
	}
	for i, ch := range name {
		isAlnum := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
		isPunct := i > 0 && (ch == '-' || ch == '_' || ch == '.')
		if !isAlnum && !isPunct {
			return false
		}
	}
	return true
}

func parseTTLSeconds(seconds *int64) (*time.Time, bool) {
	if seconds == nil {
		return nil, true
	}
	if *seconds < 0 || *seconds > math.MaxInt64/int64(time.Second) {
		return nil, false
	}
	expiry := time.Now().Add(time.Duration(*seconds) * time.Second)
	return &expiry, true
}

func (s *Server) handleInvites(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string `json:"name"`
		TTLSeconds *int64 `json:"ttl_seconds"`
	}
	if err := decodeJSON(w, r, &request); err != nil || !validDeviceName(request.Name) {
		writeError(w, http.StatusBadRequest, "valid device name is required")
		return
	}
	deviceExpiry, ok := parseTTLSeconds(request.TTLSeconds)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid TTL")
		return
	}
	code, err := s.store.CreateInvite(r.Context(), request.Name, deviceExpiry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create invite")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"code": code, "name": request.Name})
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Code  string `json:"code"`
		OS    string `json:"os"`
		Arch  string `json:"arch"`
		Shell string `json:"shell"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid join request")
		return
	}
	platform := registry.Platform{OS: request.OS, Arch: request.Arch, Shell: request.Shell}
	credential, err := s.store.ExchangeInvite(r.Context(), request.Code, platform)
	if errors.Is(err, registry.ErrUnauthorized) {
		writeError(w, http.StatusUnauthorized, "invalid or expired invite")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not join")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"device_token": credential.Token, "name": credential.Name})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list devices")
		return
	}
	views := make([]client.Device, 0, len(devices))
	for _, device := range devices {
		views = append(views, s.viewDevice(device))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) viewDevice(device registry.Device) client.Device {
	status := "missed"
	if s.isLive(device.ID) && (device.ExpiresAt == nil || device.ExpiresAt.After(time.Now())) {
		status = "alive"
	}
	var expiry *string
	if device.ExpiresAt != nil {
		formatted := device.ExpiresAt.UTC().Format(time.RFC3339)
		expiry = &formatted
	}
	return client.Device{Name: device.Name, Status: status, OS: device.OS, Arch: device.Arch, Shell: device.Shell, ExpiresAt: expiry, LastSeen: device.LastSeen.UTC().Format(time.RFC3339)}
}

func (s *Server) handleTTL(w http.ResponseWriter, r *http.Request) {
	var request struct {
		TTLSeconds *int64 `json:"ttl_seconds"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid TTL")
		return
	}
	expiry, ok := parseTTLSeconds(request.TTLSeconds)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid TTL")
		return
	}
	name := r.PathValue("name")
	if err := s.store.SetExpiry(r.Context(), name, expiry); err != nil {
		writeStoreError(w, err)
		return
	}
	s.notifyDeviceStateChange()
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := s.store.Delete(r.Context(), r.PathValue("name"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.closeDevice(id)
	w.WriteHeader(http.StatusNoContent)
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "registry failure")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
