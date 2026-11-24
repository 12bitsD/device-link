package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/12bitsD/device-link/internal/registry"
	"github.com/coder/websocket"
)

const frameTimeout = 3 * time.Second

type nodeFrame struct {
	Type      string `json:"type"`
	Name      string `json:"name,omitempty"`
	OS        string `json:"os,omitempty"`
	Arch      string `json:"arch,omitempty"`
	Shell     string `json:"shell,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	device, err := s.store.DeviceByToken(r.Context(), bearerToken(r))
	if errors.Is(err, registry.ErrUnauthorized) {
		writeError(w, http.StatusForbidden, "valid device token required")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not authenticate device")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	if !s.register(device.ID, conn) {
		return
	}
	defer s.unregister(device.ID, conn)
	if err := s.initNodeSession(r.Context(), device, conn); err != nil {
		return
	}
	s.readConnection(r.Context(), device.ID, conn)
}

func (s *Server) initNodeSession(ctx context.Context, device registry.Device, conn *websocket.Conn) error {
	if err := s.store.Touch(ctx, device.ID); err != nil {
		return err
	}
	welcome := nodeFrame{
		Type:      "connected",
		Name:      device.Name,
		OS:        device.OS,
		Arch:      device.Arch,
		Shell:     device.Shell,
		ExpiresAt: formatDeviceExpiry(device.ExpiresAt),
	}
	return writeNodeFrame(ctx, conn, welcome)
}

func formatDeviceExpiry(expiresAt *time.Time) string {
	if expiresAt == nil {
		return "never"
	}
	return expiresAt.UTC().Format(time.RFC3339)
}

func writeNodeFrame(ctx context.Context, conn *websocket.Conn, frame nodeFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, frameTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, payload)
}

func (s *Server) readConnection(ctx context.Context, id int64, conn *websocket.Conn) {
	conn.SetReadLimit(4096)
	for {
		readCtx, readCancel := context.WithTimeout(ctx, frameTimeout)
		_, message, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			return
		}
		var frame nodeFrame
		if err := json.Unmarshal(message, &frame); err != nil || frame.Type != "ping" {
			return
		}
		if err := s.store.Touch(ctx, id); err != nil {
			return
		}
		if err := writeNodeFrame(ctx, conn, nodeFrame{Type: "pong"}); err != nil {
			return
		}
	}
}

func (s *Server) beginRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.wg.Add(1)
	return true
}

func (s *Server) register(id int64, conn *websocket.Conn) bool {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return false
	}
	previous := s.live[id]
	s.live[id] = conn
	s.mu.Unlock()
	if previous != nil {
		_ = previous.CloseNow()
	}
	return true
}

func (s *Server) unregister(id int64, conn *websocket.Conn) {
	s.mu.Lock()
	if s.live[id] == conn {
		delete(s.live, id)
	}
	s.mu.Unlock()
}

func (s *Server) isLive(id int64) bool {
	s.mu.Lock()
	_, ok := s.live[id]
	s.mu.Unlock()
	return ok
}

func (s *Server) closeDevice(id int64) {
	s.mu.Lock()
	conn := s.live[id]
	s.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

func (s *Server) closeConnections() {
	s.mu.Lock()
	s.closing = true
	connections := make([]*websocket.Conn, 0, len(s.live))
	for _, conn := range s.live {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.CloseNow()
	}
}
