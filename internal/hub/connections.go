package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/12bitsD/device-link/internal/registry"
	"github.com/12bitsD/device-link/internal/transfer"
	"github.com/coder/websocket"
)

const (
	frameTimeout  = 3 * time.Second
	maxFrameBytes = 16 << 20
)

type jobWaiterKey struct {
	deviceID int64
	jobID    string
}

type nodeFrame struct {
	Type           string               `json:"type"`
	ReqID          string               `json:"req_id,omitempty"`
	Error          string               `json:"error,omitempty"`
	Name           string               `json:"name,omitempty"`
	OS             string               `json:"os,omitempty"`
	Arch           string               `json:"arch,omitempty"`
	Shell          string               `json:"shell,omitempty"`
	ExpiresAt      string               `json:"expires_at,omitempty"`
	JobID          string               `json:"job_id,omitempty"`
	Command        []string             `json:"command,omitempty"`
	Cwd            string               `json:"cwd,omitempty"`
	Env            []string             `json:"env,omitempty"`
	TimeoutMS      int64                `json:"timeout_ms,omitempty"`
	State          string               `json:"state,omitempty"`
	ExitCode       *int                 `json:"exit_code,omitempty"`
	Stdout         []byte               `json:"stdout,omitempty"`
	Stderr         []byte               `json:"stderr,omitempty"`
	Bytes          int64                `json:"bytes,omitempty"`
	Offset         int64                `json:"offset,omitempty"`
	Chunk          []byte               `json:"chunk,omitempty"`
	EOF            bool                 `json:"eof,omitempty"`
	TransferID     string               `json:"transfer_id,omitempty"`
	Sources        []string             `json:"sources,omitempty"`
	Excludes       []string             `json:"excludes,omitempty"`
	TargetDir      string               `json:"target_dir,omitempty"`
	Candidates     []transfer.Candidate `json:"candidates,omitempty"`
	ExistingHashes map[string]string    `json:"existing_hashes,omitempty"`
	StreamToken    string               `json:"stream_token,omitempty"`
	Files          int                  `json:"files,omitempty"`
	Skipped        int                  `json:"skipped,omitempty"`
	TransferBytes  int64                `json:"transfer_bytes,omitempty"`
	TotalBytes     int64                `json:"total_bytes,omitempty"`
}

type nodeSession struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan nodeFrame
	done    chan struct{}
}

func newNodeSession(conn *websocket.Conn) *nodeSession {
	return &nodeSession{
		conn:    conn,
		pending: make(map[string]chan nodeFrame),
		done:    make(chan struct{}),
	}
}

func (ns *nodeSession) send(ctx context.Context, frame nodeFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	ns.writeMu.Lock()
	defer ns.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, frameTimeout)
	defer cancel()
	return ns.conn.Write(writeCtx, websocket.MessageText, payload)
}

func (ns *nodeSession) call(ctx context.Context, frame nodeFrame) (nodeFrame, error) {
	reqID, err := nextRequestID()
	if err != nil {
		return nodeFrame{}, err
	}
	frame.ReqID = reqID
	replyCh := make(chan nodeFrame, 1)
	ns.mu.Lock()
	ns.pending[reqID] = replyCh
	ns.mu.Unlock()
	defer func() {
		ns.mu.Lock()
		delete(ns.pending, reqID)
		ns.mu.Unlock()
	}()
	if err := ns.send(ctx, frame); err != nil {
		if ctx.Err() != nil {
			return nodeFrame{}, ctx.Err()
		}
		_ = ns.conn.CloseNow()
		return nodeFrame{}, fmt.Errorf("%w: %w", errDeviceOffline, err)
	}
	select {
	case reply := <-replyCh:
		return reply, nil
	case <-ns.done:
		return nodeFrame{}, errDeviceOffline
	case <-ctx.Done():
		return nodeFrame{}, ctx.Err()
	}
}

func (ns *nodeSession) deliver(frame nodeFrame) {
	ns.mu.Lock()
	ch := ns.pending[frame.ReqID]
	ns.mu.Unlock()
	if ch != nil {
		select {
		case ch <- frame:
		default:
		}
	}
}

func nextRequestID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("request id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
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
	session := newNodeSession(conn)
	defer func() {
		close(session.done)
		_ = conn.CloseNow()
	}()
	if err := s.store.Touch(r.Context(), device.ID); err != nil {
		return
	}
	if !s.register(device.ID, session) {
		return
	}
	defer s.unregister(device.ID, session)
	welcome := nodeFrame{
		Type:      "connected",
		Name:      device.Name,
		OS:        device.OS,
		Arch:      device.Arch,
		Shell:     device.Shell,
		ExpiresAt: formatDeviceExpiry(device.ExpiresAt),
	}
	if err := session.send(r.Context(), welcome); err != nil {
		return
	}
	s.readConnection(r.Context(), device.ID, session)
}

func formatDeviceExpiry(expiresAt *time.Time) string {
	if expiresAt == nil {
		return "never"
	}
	return expiresAt.UTC().Format(time.RFC3339)
}

func (s *Server) readConnection(ctx context.Context, id int64, session *nodeSession) {
	session.conn.SetReadLimit(maxFrameBytes)
	for {
		readCtx, readCancel := context.WithTimeout(ctx, frameTimeout)
		_, message, err := session.conn.Read(readCtx)
		readCancel()
		if err != nil {
			return
		}
		if !s.dispatchNodeFrame(ctx, id, session, message) {
			return
		}
	}
}

func (s *Server) dispatchNodeFrame(ctx context.Context, id int64, session *nodeSession, message []byte) bool {
	var frame nodeFrame
	if err := json.Unmarshal(message, &frame); err != nil {
		return false
	}
	switch frame.Type {
	case "ping":
		if err := s.store.Touch(ctx, id); err != nil {
			return false
		}
		return session.send(ctx, nodeFrame{Type: "pong"}) == nil
	case "rpc_reply":
		session.deliver(frame)
		return true
	case "job_done":
		s.deliverJobDone(id, frame)
		return true
	default:
		return false
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

func (s *Server) notifyStateChangeLocked() {
	if s.onlineNotify != nil {
		close(s.onlineNotify)
		s.onlineNotify = nil
	}
}

func (s *Server) register(id int64, session *nodeSession) bool {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return false
	}
	if s.live == nil {
		s.live = make(map[int64]*nodeSession)
	}
	previous := s.live[id]
	s.live[id] = session
	s.notifyStateChangeLocked()
	s.mu.Unlock()
	if previous != nil {
		_ = previous.conn.CloseNow()
	}
	return true
}

func (s *Server) unregister(id int64, session *nodeSession) {
	s.mu.Lock()
	if s.live[id] == session {
		delete(s.live, id)
		s.notifyStateChangeLocked()
	}
	s.mu.Unlock()
	s.failSessionTransfers(session)
}

func (s *Server) isLive(id int64) bool {
	s.mu.Lock()
	_, ok := s.live[id]
	s.mu.Unlock()
	return ok
}

func (s *Server) sessionFor(id int64) (*nodeSession, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onlineNotify == nil {
		s.onlineNotify = make(chan struct{})
	}
	return s.live[id], s.onlineNotify
}

func (s *Server) notifyDeviceStateChange() {
	s.mu.Lock()
	s.notifyStateChangeLocked()
	s.mu.Unlock()
}

func (s *Server) closeDevice(id int64) {
	s.mu.Lock()
	session := s.live[id]
	s.notifyStateChangeLocked()
	s.mu.Unlock()
	s.failDeviceTransfers(id)
	if session != nil {
		_ = session.conn.CloseNow()
	}
}

func (s *Server) closeConnections() {
	s.mu.Lock()
	s.closing = true
	sessions := make([]*nodeSession, 0, len(s.live))
	for _, session := range s.live {
		sessions = append(sessions, session)
	}
	s.notifyStateChangeLocked()
	s.mu.Unlock()
	for _, session := range sessions {
		_ = session.conn.CloseNow()
	}
	s.cancelAllTransfers()
}
