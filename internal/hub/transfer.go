package hub

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/12bitsD/device-link/internal/client"
	"github.com/12bitsD/device-link/internal/registry"
	"github.com/12bitsD/device-link/internal/transfer"
)

const (
	maxTransferMetaBytes = 8 << 20
	maxRetainedTransfers = 256
	streamAttachTimeout  = 15 * time.Second
)

var errTransferCancelled = errors.New("transfer cancelled")

type streamRole uint8

const (
	roleSend streamRole = 1
	roleRecv streamRole = 2
)

type hubTransfer struct {
	id           string
	from         string
	to           string
	sendDeviceID int64
	recvDeviceID int64
	sendSession  *nodeSession
	recvSession  *nodeSession
	sendToken    string
	recvToken    string
	pipeR        *io.PipeReader
	pipeW        *io.PipeWriter
	sendReady    chan struct{}
	recvReady    chan struct{}
	done         chan struct{}

	mu            sync.Mutex
	state         string
	files         int
	skipped       int
	rawStreamed   int64
	transferBytes int64
	totalBytes    int64
	errMsg        string
	sendClaimed   bool
	recvClaimed   bool
	sendDone      bool
	recvDone      bool
}

type transferEndpoints struct {
	from         string
	to           string
	sendDeviceID int64
	recvDeviceID int64
	sendSession  *nodeSession
	recvSession  *nodeSession
	totalBytes   int64
}

func newTransferToken(prefix string) (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate transfer token: %w", err)
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}

func (s *Server) registerTransfer(ep transferEndpoints) (*hubTransfer, error) {
	id, err := newTransferToken("tr_")
	if err != nil {
		return nil, err
	}
	sendTok, err := newTransferToken("st_")
	if err != nil {
		return nil, err
	}
	recvTok, err := newTransferToken("rt_")
	if err != nil {
		return nil, err
	}
	pipeR, pipeW := io.Pipe()
	tr := &hubTransfer{
		id:           id,
		from:         ep.from,
		to:           ep.to,
		sendDeviceID: ep.sendDeviceID,
		recvDeviceID: ep.recvDeviceID,
		sendSession:  ep.sendSession,
		recvSession:  ep.recvSession,
		sendToken:    sendTok,
		recvToken:    recvTok,
		pipeR:        pipeR,
		pipeW:        pipeW,
		sendReady:    make(chan struct{}),
		recvReady:    make(chan struct{}),
		done:         make(chan struct{}),
		state:        "running",
		totalBytes:   ep.totalBytes,
	}
	s.storeTransfer(tr)
	return tr, nil
}

func (s *Server) storeTransfer(tr *hubTransfer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transfers == nil {
		s.transfers = make(map[string]*hubTransfer)
	}
	s.evictTerminalTransfersLocked()
	s.transfers[tr.id] = tr
	s.transferOrder = append(s.transferOrder, tr.id)
}

func (s *Server) evictTerminalTransfersLocked() {
	if len(s.transfers) < maxRetainedTransfers {
		return
	}
	kept := s.transferOrder[:0]
	for _, id := range s.transferOrder {
		tr := s.transfers[id]
		if tr == nil {
			continue
		}
		if len(s.transfers) >= maxRetainedTransfers && !tr.isRunning() {
			delete(s.transfers, id)
			continue
		}
		kept = append(kept, id)
	}
	s.transferOrder = kept
}

func (s *Server) requireTransfer(w http.ResponseWriter, r *http.Request) *hubTransfer {
	s.mu.Lock()
	tr := s.transfers[r.PathValue("id")]
	s.mu.Unlock()
	if tr == nil {
		writeError(w, http.StatusNotFound, "transfer not found")
	}
	return tr
}

func (s *Server) cancelAllTransfers() {
	s.mu.Lock()
	list := make([]*hubTransfer, 0, len(s.transfers))
	for _, tr := range s.transfers {
		list = append(list, tr)
	}
	s.mu.Unlock()
	for _, tr := range list {
		tr.cancel()
	}
}

func (s *Server) failSessionTransfers(session *nodeSession) {
	s.mu.Lock()
	list := make([]*hubTransfer, 0)
	for _, tr := range s.transfers {
		if tr.sendSession == session || tr.recvSession == session {
			list = append(list, tr)
		}
	}
	s.mu.Unlock()
	for _, tr := range list {
		tr.failIfSessionActive(session)
	}
}

func (s *Server) failDeviceTransfers(deviceID int64) {
	s.mu.Lock()
	list := make([]*hubTransfer, 0)
	for _, tr := range s.transfers {
		if tr.sendDeviceID == deviceID || tr.recvDeviceID == deviceID {
			list = append(list, tr)
		}
	}
	s.mu.Unlock()
	for _, tr := range list {
		tr.fail("device removed during transfer")
	}
}

func (t *hubTransfer) isRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state == "running"
}

func (t *hubTransfer) failIfSessionActive(session *nodeSession) {
	t.mu.Lock()
	shouldFail := t.state == "running" &&
		((t.sendSession == session && !t.sendDone) || (t.recvSession == session && !t.recvDone))
	t.mu.Unlock()
	if shouldFail {
		t.fail("device disconnected during transfer")
	}
}

func (t *hubTransfer) claimStream(role streamRole) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != "running" {
		return false
	}
	if role == roleSend {
		if t.sendClaimed {
			return false
		}
		t.sendClaimed = true
		close(t.sendReady)
		return true
	}
	if t.recvClaimed {
		return false
	}
	t.recvClaimed = true
	close(t.recvReady)
	return true
}

func (t *hubTransfer) setPlanCounts(files, skipped int, transferBytes, totalBytes int64) {
	t.mu.Lock()
	if t.state == "running" {
		t.files = files
		t.skipped = skipped
		t.transferBytes = transferBytes
		if totalBytes >= 0 {
			t.totalBytes = totalBytes
		}
	}
	t.mu.Unlock()
}

func (t *hubTransfer) addStreamed(n int64) {
	t.mu.Lock()
	t.rawStreamed += n
	t.mu.Unlock()
}

func (t *hubTransfer) markDone(role streamRole) {
	t.mu.Lock()
	if t.state == "running" {
		if role == roleSend {
			t.sendDone = true
		} else {
			t.recvDone = true
		}
		if t.sendDone && t.recvDone {
			t.state = "completed"
			close(t.done)
		}
	}
	t.mu.Unlock()
}

func (t *hubTransfer) fail(msg string) {
	reason := msg
	if reason == "" {
		reason = "transfer stream interrupted"
	}
	t.finishWithError("failed", reason, errors.New(reason))
}

func (t *hubTransfer) cancel() client.TransferStatus {
	return t.finishWithError("cancelled", "", errTransferCancelled)
}

func (t *hubTransfer) finishWithError(state, msg string, pipeErr error) client.TransferStatus {
	t.mu.Lock()
	if t.state != "running" {
		snap := t.snapshotLocked()
		t.mu.Unlock()
		return snap
	}
	t.state = state
	t.errMsg = msg
	close(t.done)
	snap := t.snapshotLocked()
	t.mu.Unlock()
	_ = t.pipeR.CloseWithError(pipeErr)
	_ = t.pipeW.CloseWithError(pipeErr)
	return snap
}

func (t *hubTransfer) snapshot() client.TransferStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *hubTransfer) snapshotAndErr() (client.TransferStatus, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(), t.errMsg
}

func (t *hubTransfer) snapshotLocked() client.TransferStatus {
	transferred := max(min(t.rawStreamed, t.transferBytes), int64(0))
	if t.state == "completed" {
		transferred = t.transferBytes
	}
	return client.TransferStatus{
		TransferID: t.id,
		From:       t.from,
		To:         t.to,
		State:      t.state,
		Files:      t.files,
		Skipped:    t.skipped,
		Bytes:      transferred,
		TotalBytes: t.totalBytes,
	}
}

func decodeLargeJSON(w http.ResponseWriter, r *http.Request, dst any, invalidMsg string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxTransferMetaBytes)
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil {
		return true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "transfer metadata too large")
		return false
	}
	writeError(w, http.StatusBadRequest, invalidMsg)
	return false
}

func (s *Server) requireOnlineDevice(ctx context.Context, w http.ResponseWriter, name string) (registry.Device, *nodeSession, bool) {
	device, session, _, err := s.activeDeviceSession(ctx, name)
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "device "+name+" not found")
		return registry.Device{}, nil, false
	}
	if err != nil {
		writeStoreError(w, err)
		return registry.Device{}, nil, false
	}
	if session == nil {
		writeError(w, http.StatusServiceUnavailable, "device "+name+" is offline")
		return registry.Device{}, nil, false
	}
	return device, session, true
}

func (s *Server) handleTransferScan(w http.ResponseWriter, r *http.Request) {
	var req client.TransferScanRequest
	if !decodeLargeJSON(w, r, &req, "device and sources are required") {
		return
	}
	if req.Device == "" || len(req.Sources) == 0 {
		writeError(w, http.StatusBadRequest, "device and sources are required")
		return
	}
	_, session, ok := s.requireOnlineDevice(r.Context(), w, req.Device)
	if !ok {
		return
	}
	reply, err := session.call(r.Context(), nodeFrame{
		Type: "transfer_scan", Sources: req.Sources, Excludes: req.Excludes,
	})
	if !writeTransferRPCErr(w, reply, err) {
		return
	}
	writeJSON(w, http.StatusOK, client.TransferScanResponse{
		Candidates: reply.Candidates,
		TotalBytes: reply.TotalBytes,
	})
}

func (s *Server) handleStartTransfer(w http.ResponseWriter, r *http.Request) {
	var req client.TransferStartRequest
	if !decodeLargeJSON(w, r, &req, "from and to are required") {
		return
	}
	if req.From == "" || req.To == "" || (req.From == "local" && req.To == "local") {
		writeError(w, http.StatusBadRequest, "valid remote endpoint is required")
		return
	}
	if !validMillis(req.WaitMS) || req.TotalBytes < 0 || req.TotalBytes > transfer.MaxTransferBytes {
		writeError(w, http.StatusBadRequest, "invalid transfer size or wait duration")
		return
	}
	if req.From == "local" {
		s.startLocalToRemote(w, r, req)
		return
	}
	if req.To == "local" {
		s.startRemoteToLocal(w, r, req)
		return
	}
	s.startRemoteToRemote(w, r, req)
}

func (t *hubTransfer) startRecv(ctx context.Context, w http.ResponseWriter, session *nodeSession, targetDir string) bool {
	reply, err := session.call(ctx, nodeFrame{
		Type: "transfer_recv", TransferID: t.id, TargetDir: targetDir, StreamToken: t.recvToken,
	})
	if !writeTransferRPCErr(w, reply, err) {
		t.fail("failed to start target receiver")
		return false
	}
	return true
}

func (t *hubTransfer) startSend(ctx context.Context, w http.ResponseWriter, session *nodeSession, frame nodeFrame) bool {
	frame.Type = "transfer_send"
	frame.TransferID = t.id
	frame.StreamToken = t.sendToken
	reply, err := session.call(ctx, frame)
	if !writeTransferRPCErr(w, reply, err) {
		t.fail("failed to start source sender")
		return false
	}
	t.setPlanCounts(reply.Files, reply.Skipped, reply.TransferBytes, reply.TotalBytes)
	return true
}

func (s *Server) startLocalToRemote(w http.ResponseWriter, r *http.Request, req client.TransferStartRequest) {
	toDev, toSession, ok := s.requireOnlineDevice(r.Context(), w, req.To)
	if !ok {
		return
	}
	inspectReply, err := toSession.call(r.Context(), nodeFrame{
		Type: "transfer_inspect", TargetDir: req.TargetDir, Candidates: req.Candidates,
	})
	if !writeTransferRPCErr(w, inspectReply, err) {
		return
	}
	tr, regErr := s.registerTransfer(transferEndpoints{
		from: "local", to: toDev.Name, recvDeviceID: toDev.ID, recvSession: toSession, totalBytes: req.TotalBytes,
	})
	if regErr != nil {
		writeError(w, http.StatusInternalServerError, "could not create transfer")
		return
	}
	if !tr.startRecv(r.Context(), w, toSession, req.TargetDir) {
		return
	}
	snap := tr.snapshot()
	snap.ExistingHashes = inspectReply.ExistingHashes
	writeJSON(w, http.StatusCreated, snap)
}

func (s *Server) startRemoteToLocal(w http.ResponseWriter, r *http.Request, req client.TransferStartRequest) {
	fromDev, fromSession, ok := s.requireOnlineDevice(r.Context(), w, req.From)
	if !ok {
		return
	}
	tr, regErr := s.registerTransfer(transferEndpoints{
		from: fromDev.Name, to: "local", sendDeviceID: fromDev.ID, sendSession: fromSession, totalBytes: req.TotalBytes,
	})
	if regErr != nil {
		writeError(w, http.StatusInternalServerError, "could not create transfer")
		return
	}
	sendFrame := nodeFrame{Sources: req.Sources, Excludes: req.Excludes, ExistingHashes: req.ExistingHashes}
	if !tr.startSend(r.Context(), w, fromSession, sendFrame) {
		return
	}
	writeJSON(w, http.StatusCreated, tr.snapshot())
}

func (s *Server) startRemoteToRemote(w http.ResponseWriter, r *http.Request, req client.TransferStartRequest) {
	fromDev, fromSession, okFrom := s.requireOnlineDevice(r.Context(), w, req.From)
	if !okFrom {
		return
	}
	toDev, toSession, okTo := s.requireOnlineDevice(r.Context(), w, req.To)
	if !okTo {
		return
	}
	scanReply, scanErr := fromSession.call(r.Context(), nodeFrame{
		Type: "transfer_scan", Sources: req.Sources, Excludes: req.Excludes,
	})
	if !writeTransferRPCErr(w, scanReply, scanErr) {
		return
	}
	inspectReply, inspectErr := toSession.call(r.Context(), nodeFrame{
		Type: "transfer_inspect", TargetDir: req.TargetDir, Candidates: scanReply.Candidates,
	})
	if !writeTransferRPCErr(w, inspectReply, inspectErr) {
		return
	}
	tr, regErr := s.registerTransfer(transferEndpoints{
		from: fromDev.Name, to: toDev.Name,
		sendDeviceID: fromDev.ID, recvDeviceID: toDev.ID,
		sendSession: fromSession, recvSession: toSession,
		totalBytes: scanReply.TotalBytes,
	})
	if regErr != nil {
		writeError(w, http.StatusInternalServerError, "could not create transfer")
		return
	}
	if !tr.startRecv(r.Context(), w, toSession, req.TargetDir) {
		return
	}
	sendFrame := nodeFrame{Sources: req.Sources, Excludes: req.Excludes, ExistingHashes: inspectReply.ExistingHashes}
	if !tr.startSend(r.Context(), w, fromSession, sendFrame) {
		return
	}
	awaitRemoteTransfer(r.Context(), w, tr, time.Duration(req.WaitMS)*time.Millisecond)
}

func awaitRemoteTransfer(ctx context.Context, w http.ResponseWriter, tr *hubTransfer, wait time.Duration) {
	if wait <= 0 {
		writeJSON(w, http.StatusAccepted, tr.snapshot())
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-tr.done:
		writeFinishedTransfer(w, tr)
	case <-timer.C:
		writeTimeoutOrFinished(w, tr)
	case <-ctx.Done():
	}
}

func writeTimeoutOrFinished(w http.ResponseWriter, tr *hubTransfer) {
	select {
	case <-tr.done:
		writeFinishedTransfer(w, tr)
	default:
		writeJSON(w, http.StatusAccepted, tr.snapshot())
	}
}

func writeFinishedTransfer(w http.ResponseWriter, tr *hubTransfer) {
	snap, errMsg := tr.snapshotAndErr()
	if snap.State == "completed" {
		writeJSON(w, http.StatusOK, snap)
		return
	}
	msg := errMsg
	if msg == "" {
		msg = "transfer " + snap.State
	}
	writeError(w, http.StatusBadGateway, msg)
}

func parseUploadCounts(r *http.Request, maxBytes int64) (int, int, int64, bool) {
	q := r.URL.Query()
	files, err1 := strconv.Atoi(q.Get("files"))
	skipped, err2 := strconv.Atoi(q.Get("skipped"))
	transferBytes, err3 := strconv.ParseInt(q.Get("transfer_bytes"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	if files < 0 || skipped < 0 || transferBytes < 0 || transferBytes > maxBytes {
		return 0, 0, 0, false
	}
	return files, skipped, transferBytes, true
}

func (s *Server) handleControllerUpload(w http.ResponseWriter, r *http.Request) {
	tr := s.requireTransfer(w, r)
	if tr == nil {
		return
	}
	if tr.from != "local" {
		writeError(w, http.StatusBadRequest, "transfer source is not local")
		return
	}
	files, skipped, transferBytes, ok := parseUploadCounts(r, tr.snapshot().TotalBytes)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid upload counts")
		return
	}
	if !tr.claimStream(roleSend) {
		writeError(w, http.StatusConflict, "upload stream already claimed or closed")
		return
	}
	tr.setPlanCounts(files, skipped, transferBytes, -1)
	if !pipeUploadStream(r.Context(), tr, r.Body) {
		writeFinishedTransfer(w, tr)
		return
	}
	select {
	case <-tr.done:
		writeFinishedTransfer(w, tr)
	case <-r.Context().Done():
		tr.fail("controller disconnected")
	}
}

func (s *Server) handleControllerDownload(w http.ResponseWriter, r *http.Request) {
	tr := s.requireTransfer(w, r)
	if tr == nil {
		return
	}
	if tr.to != "local" {
		writeError(w, http.StatusBadRequest, "transfer target is not local")
		return
	}
	if !tr.claimStream(roleRecv) {
		writeError(w, http.StatusConflict, "download stream already claimed or closed")
		return
	}
	pipeDownloadStream(r.Context(), w, tr)
}

func (s *Server) handleControllerComplete(w http.ResponseWriter, r *http.Request) {
	tr := s.requireTransfer(w, r)
	if tr == nil {
		return
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := decodeJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid completion payload")
		return
	}
	if payload.Error != "" && (tr.from == "local" || tr.to == "local") {
		tr.fail(payload.Error)
		writeError(w, http.StatusBadRequest, payload.Error)
		return
	}
	if tr.to != "local" {
		writeError(w, http.StatusBadRequest, "transfer target is not local")
		return
	}
	tr.markDone(roleRecv)
	select {
	case <-tr.done:
		writeFinishedTransfer(w, tr)
	case <-r.Context().Done():
		tr.fail("controller disconnected")
	}
}

func (s *Server) handleTransferStatus(w http.ResponseWriter, r *http.Request) {
	tr := s.requireTransfer(w, r)
	if tr == nil {
		return
	}
	writeJSON(w, http.StatusOK, tr.snapshot())
}

func (s *Server) handleTransferCancel(w http.ResponseWriter, r *http.Request) {
	tr := s.requireTransfer(w, r)
	if tr == nil {
		return
	}
	writeJSON(w, http.StatusOK, tr.cancel())
}

func (s *Server) authorizeNodeStream(w http.ResponseWriter, r *http.Request) (*hubTransfer, streamRole, bool) {
	device, err := s.store.DeviceByToken(r.Context(), bearerToken(r))
	if errors.Is(err, registry.ErrUnauthorized) {
		writeError(w, http.StatusForbidden, "valid device token required")
		return nil, 0, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not verify device token")
		return nil, 0, false
	}
	s.mu.Lock()
	tr := s.transfers[r.PathValue("id")]
	s.mu.Unlock()
	streamTok := r.Header.Get("X-Transfer-Token")
	if tr != nil && device.ID == tr.sendDeviceID && tr.sendDeviceID != 0 && equalToken(streamTok, tr.sendToken) {
		return tr, roleSend, true
	}
	if tr != nil && device.ID == tr.recvDeviceID && tr.recvDeviceID != 0 && equalToken(streamTok, tr.recvToken) {
		return tr, roleRecv, true
	}
	writeError(w, http.StatusForbidden, "unauthorized transfer stream")
	return nil, 0, false
}

func equalToken(a, b string) bool {
	return len(a) > 0 && len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) handleNodeUpload(w http.ResponseWriter, r *http.Request) {
	tr, role, ok := s.authorizeNodeStream(w, r)
	if !ok {
		return
	}
	if role != roleSend {
		writeError(w, http.StatusForbidden, "upload token required")
		return
	}
	if !tr.claimStream(roleSend) {
		writeError(w, http.StatusConflict, "upload stream already claimed or closed")
		return
	}
	if !pipeUploadStream(r.Context(), tr, r.Body) {
		writeError(w, http.StatusBadGateway, "upload interrupted")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "uploaded"})
}

func (s *Server) handleNodeDownload(w http.ResponseWriter, r *http.Request) {
	tr, role, ok := s.authorizeNodeStream(w, r)
	if !ok {
		return
	}
	if role != roleRecv {
		writeError(w, http.StatusForbidden, "download token required")
		return
	}
	if !tr.claimStream(roleRecv) {
		writeError(w, http.StatusConflict, "download stream already claimed or closed")
		return
	}
	pipeDownloadStream(r.Context(), w, tr)
}

func (s *Server) handleNodeTransferDone(w http.ResponseWriter, r *http.Request) {
	tr, role, ok := s.authorizeNodeStream(w, r)
	if !ok {
		return
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := decodeJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}
	if payload.Error != "" {
		tr.fail(payload.Error)
		writeJSON(w, http.StatusOK, map[string]string{"state": "failed"})
		return
	}
	if role == roleRecv {
		tr.markDone(roleRecv)
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "ok"})
}

func waitPeerReady(ctx context.Context, tr *hubTransfer, peerReady <-chan struct{}) bool {
	timer := time.NewTimer(streamAttachTimeout)
	defer timer.Stop()
	select {
	case <-peerReady:
		return true
	case <-tr.done:
		return false
	case <-timer.C:
		tr.fail("timed out waiting for peer stream")
		return false
	case <-ctx.Done():
		tr.fail("transfer stream disconnected")
		return false
	}
}

func watchStreamContext(ctx context.Context, tr *hubTransfer) func() {
	stopCh := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-stopCh:
		case <-tr.done:
		case <-ctx.Done():
			tr.fail("transfer stream disconnected")
		}
	}()
	return func() {
		close(stopCh)
		<-joined
	}
}

func pipeUploadStream(ctx context.Context, tr *hubTransfer, body io.Reader) bool {
	if !waitPeerReady(ctx, tr, tr.recvReady) {
		return false
	}
	stopWatch := watchStreamContext(ctx, tr)
	defer stopWatch()
	buf := make([]byte, 16*1024)
	var rawTotal int64
	for {
		n, readErr := body.Read(buf)
		if !forwardUploadChunk(tr, buf[:n], &rawTotal) {
			return false
		}
		if errors.Is(readErr, io.EOF) {
			_ = tr.pipeW.Close()
			tr.markDone(roleSend)
			return true
		}
		if readErr != nil {
			tr.fail(readErr.Error())
			return false
		}
	}
}

func forwardUploadChunk(tr *hubTransfer, chunk []byte, rawTotal *int64) bool {
	if len(chunk) == 0 {
		return true
	}
	*rawTotal += int64(len(chunk))
	if *rawTotal > transfer.MaxRawTarStreamBytes {
		tr.fail("transfer stream exceeds 500MB limit")
		return false
	}
	written, writeErr := tr.pipeW.Write(chunk)
	tr.addStreamed(int64(written))
	if writeErr != nil {
		tr.fail(writeErr.Error())
		return false
	}
	return true
}

func pipeDownloadStream(ctx context.Context, w http.ResponseWriter, tr *hubTransfer) {
	if !waitPeerReady(ctx, tr, tr.sendReady) {
		writeError(w, http.StatusBadGateway, "sender stream failed to attach")
		return
	}
	stopWatch := watchStreamContext(ctx, tr)
	defer stopWatch()
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, readErr := tr.pipeR.Read(buf)
		if n > 0 && !flushDownloadChunk(w, flusher, tr, buf[:n]) {
			return
		}
		if errors.Is(readErr, io.EOF) {
			return
		}
		if readErr != nil {
			// bits:allow(GO001): net/http sentinel aborts chunked response on mid-stream error
			panic(http.ErrAbortHandler)
		}
	}
}

func flushDownloadChunk(w http.ResponseWriter, flusher http.Flusher, tr *hubTransfer, chunk []byte) bool {
	if _, err := w.Write(chunk); err != nil {
		tr.fail(err.Error())
		return false
	}
	if flusher != nil {
		flusher.Flush()
	}
	return true
}

func writeTransferRPCErr(w http.ResponseWriter, reply nodeFrame, err error) bool {
	if errors.Is(err, errDeviceOffline) {
		writeError(w, http.StatusServiceUnavailable, "device is offline")
		return false
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return false
	}
	if reply.Error != "" {
		writeError(w, http.StatusBadRequest, reply.Error)
		return false
	}
	return true
}
