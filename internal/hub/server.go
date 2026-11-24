package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/12bitsD/device-link/internal/client"
	"github.com/12bitsD/device-link/internal/registry"
	"github.com/coder/websocket"
)

const heartbeatInterval = time.Second

type Config struct {
	Addr     string
	Host     string
	StateDir string
}

type Server struct {
	store   *registry.Store
	mu      sync.Mutex
	live    map[int64]*websocket.Conn
	closing bool
	wg      sync.WaitGroup
}

func Run(ctx context.Context, config Config) error {
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return fmt.Errorf("create hub state: %w", err)
	}
	store, err := registry.Open(ctx, filepath.Join(config.StateDir, "hub.db"))
	if err != nil {
		return err
	}
	if err := initializeState(config.StateDir, config.Host, store); err != nil {
		_ = store.Close()
		return err
	}
	listener, err := net.Listen("tcp", config.Addr)
	if err != nil {
		_ = store.Close()
		return fmt.Errorf("listen on %s: %w", config.Addr, err)
	}
	fmt.Printf("DL_ADDR=%s\n", listener.Addr())
	server := &Server{store: store, live: make(map[int64]*websocket.Conn)}
	return serve(ctx, listener, server, config.StateDir)
}

func initializeState(stateDir, host string, store *registry.Store) error {
	token, err := store.ControllerToken(context.Background())
	if err != nil {
		return err
	}
	tokenPath := filepath.Join(stateDir, "controller_token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write controller token: %w", err)
	}
	if err := os.Chmod(tokenPath, 0o600); err != nil {
		return fmt.Errorf("secure controller token: %w", err)
	}
	if err := ensureCertificate(stateDir, host); err != nil {
		return err
	}
	fmt.Printf("DL_CONTROLLER_TOKEN=%s\n", token)
	return nil
}

func serve(ctx context.Context, listener net.Listener, server *Server, stateDir string) error {
	httpServer := &http.Server{Handler: server.routes(), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		defer close(serveErr)
		serveErr <- httpServer.ServeTLS(listener, filepath.Join(stateDir, "cert.pem"), filepath.Join(stateDir, "key.pem"))
	}()
	runCtx, cancel := context.WithCancel(ctx)
	server.wg.Add(1)
	go server.expiryLoop(runCtx)
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve HTTPS: %w", err)
		}
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
	}
	server.closeConnections()
	server.wg.Wait()
	<-serveErr
	if err := server.store.Close(); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func (s *Server) expiryLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.disconnectExpired(ctx)
		}
	}
}

func (s *Server) disconnectExpired(ctx context.Context) {
	ids, err := s.store.ExpiredIDs(ctx)
	if err != nil {
		return
	}
	for _, id := range ids {
		s.closeDevice(id)
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/invites", s.handleInvites)
	mux.HandleFunc("/v1/join", s.handleJoin)
	mux.HandleFunc("/v1/devices", s.handleDevices)
	mux.HandleFunc("/v1/devices/", s.handleDevice)
	mux.HandleFunc("/v1/node", s.handleNode)
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
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (s *Server) handleInvites(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeController(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request struct {
		Name       string `json:"name"`
		TTLSeconds *int64 `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	var deviceExpiry *time.Time
	if request.TTLSeconds != nil {
		value := time.Now().Add(time.Duration(*request.TTLSeconds) * time.Second)
		deviceExpiry = &value
	}
	code, err := s.store.CreateInvite(r.Context(), request.Name, deviceExpiry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create invite")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"code": code, "name": request.Name})
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request struct {
		Code  string `json:"code"`
		OS    string `json:"os"`
		Arch  string `json:"arch"`
		Shell string `json:"shell"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
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
	if !s.authorizeController(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeController(w, r) {
		return
	}
	nameAction := strings.TrimPrefix(r.URL.Path, "/v1/devices/")
	parts := strings.Split(nameAction, "/")
	if len(parts) == 2 && parts[1] == "ttl" && r.Method == http.MethodPut {
		s.handleTTL(w, r, parts[0])
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		s.handleDelete(w, r, parts[0])
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

func (s *Server) handleTTL(w http.ResponseWriter, r *http.Request, name string) {
	var request struct {
		TTLSeconds *int64 `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || (request.TTLSeconds != nil && *request.TTLSeconds < 0) {
		writeError(w, http.StatusBadRequest, "invalid TTL")
		return
	}
	var expiry *time.Time
	if request.TTLSeconds != nil {
		value := time.Now().Add(time.Duration(*request.TTLSeconds) * time.Second)
		expiry = &value
	}
	if err := s.store.SetExpiry(r.Context(), name, expiry); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, name string) {
	id, err := s.store.Delete(r.Context(), name)
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
