package hub

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/12bitsD/device-link/internal/registry"
	"github.com/coder/websocket"
)

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
	if !s.register(device.ID, conn) {
		_ = conn.CloseNow()
		return
	}
	if err := s.store.Touch(r.Context(), device.ID); err != nil {
		s.unregister(device.ID, conn)
		_ = conn.CloseNow()
		return
	}
	s.readConnection(r.Context(), device.ID, conn)
}

func (s *Server) readConnection(ctx context.Context, id int64, conn *websocket.Conn) {
	conn.SetReadLimit(64)
	defer func() {
		s.unregister(id, conn)
		_ = conn.CloseNow()
	}()
	for {
		readCtx, readCancel := context.WithTimeout(ctx, 4*time.Second)
		_, message, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			return
		}
		if string(message) != "ping" {
			continue
		}
		if err := s.store.Touch(ctx, id); err != nil {
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte("pong")); err != nil {
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
