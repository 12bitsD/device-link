package hub

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/12bitsD/device-link/internal/client"
	"github.com/12bitsD/device-link/internal/registry"
)

var errDeviceOffline = errors.New("device is offline")

func (s *Server) registerJobWaiter(key jobWaiterKey) chan nodeFrame {
	ch := make(chan nodeFrame, 1)
	s.mu.Lock()
	if s.jobWaiters == nil {
		s.jobWaiters = make(map[jobWaiterKey]chan nodeFrame)
	}
	s.jobWaiters[key] = ch
	s.mu.Unlock()
	return ch
}

func (s *Server) unregisterJobWaiter(key jobWaiterKey) {
	s.mu.Lock()
	delete(s.jobWaiters, key)
	s.mu.Unlock()
}

func (s *Server) deliverJobDone(deviceID int64, frame nodeFrame) {
	s.mu.Lock()
	ch := s.jobWaiters[jobWaiterKey{deviceID: deviceID, jobID: frame.JobID}]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- frame:
		default:
		}
	}
}

func validMillis(ms int64) bool {
	return ms >= 0 && ms <= math.MaxInt64/int64(time.Millisecond)
}

func (s *Server) activeDeviceSession(ctx context.Context, name string) (registry.Device, *nodeSession, <-chan struct{}, error) {
	device, err := s.store.DeviceByName(ctx, name)
	if err != nil {
		return registry.Device{}, nil, nil, err
	}
	session, notify := s.sessionFor(device.ID)
	if device.ExpiresAt != nil && !device.ExpiresAt.After(time.Now()) {
		return device, nil, notify, nil
	}
	return device, session, notify, nil
}

func (s *Server) waitDeviceSession(ctx context.Context, name string, waitOnline time.Duration) (registry.Device, *nodeSession, error) {
	device, session, notify, err := s.activeDeviceSession(ctx, name)
	if err != nil {
		return registry.Device{}, nil, err
	}
	if session != nil {
		return device, session, nil
	}
	if waitOnline <= 0 {
		return device, nil, errDeviceOffline
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitOnline)
	defer cancel()
	for {
		select {
		case <-waitCtx.Done():
			return device, nil, errDeviceOffline
		case <-notify:
		}
		device, session, notify, err = s.activeDeviceSession(waitCtx, name)
		if err != nil {
			return registry.Device{}, nil, err
		}
		if session != nil {
			return device, session, nil
		}
	}
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var request client.ExecRequest
	if err := decodeJSON(w, r, &request); err != nil || len(request.Command) == 0 {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	if !validMillis(request.WaitMS) || !validMillis(request.TimeoutMS) || !validMillis(request.WaitOnlineMS) {
		writeError(w, http.StatusBadRequest, "invalid duration")
		return
	}
	name := r.PathValue("name")
	device, session, err := s.waitDeviceSession(r.Context(), name, time.Duration(request.WaitOnlineMS)*time.Millisecond)
	if errors.Is(err, errDeviceOffline) {
		writeError(w, http.StatusServiceUnavailable, "device "+name+" is offline")
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	jobID, err := s.store.CreateJob(r.Context(), device.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.dispatchExec(w, r, execDispatch{
		deviceID: device.ID,
		session:  session,
		wait:     time.Duration(request.WaitMS) * time.Millisecond,
		start: nodeFrame{
			Type:      "exec_start",
			JobID:     jobID,
			Command:   request.Command,
			Cwd:       request.Cwd,
			Env:       request.Env,
			TimeoutMS: request.TimeoutMS,
		},
	})
}

type execDispatch struct {
	deviceID int64
	session  *nodeSession
	wait     time.Duration
	start    nodeFrame
}

func (s *Server) dispatchExec(w http.ResponseWriter, r *http.Request, d execDispatch) {
	key := jobWaiterKey{deviceID: d.deviceID, jobID: d.start.JobID}
	doneCh := s.registerJobWaiter(key)
	defer s.unregisterJobWaiter(key)
	reply, err := d.session.call(r.Context(), d.start)
	if errors.Is(err, errDeviceOffline) {
		writeError(w, http.StatusServiceUnavailable, "device is offline")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if reply.Error != "" {
		writeError(w, http.StatusBadRequest, reply.Error)
		return
	}
	if d.wait <= 0 {
		writeJSON(w, http.StatusAccepted, client.ExecResult{JobID: d.start.JobID, State: "running"})
		return
	}
	awaitJobCompletion(r.Context(), w, d, doneCh)
}

func awaitJobCompletion(ctx context.Context, w http.ResponseWriter, d execDispatch, doneCh <-chan nodeFrame) {
	timer := time.NewTimer(d.wait)
	defer timer.Stop()
	select {
	case done := <-doneCh:
		writeDoneResult(w, d.start.JobID, done)
	case <-d.session.done:
		writeDetachedOrDone(w, d.start.JobID, doneCh)
	case <-timer.C:
		writeJSON(w, http.StatusAccepted, client.ExecResult{JobID: d.start.JobID, State: "running"})
	case <-ctx.Done():
	}
}

func writeDetachedOrDone(w http.ResponseWriter, jobID string, doneCh <-chan nodeFrame) {
	select {
	case done := <-doneCh:
		writeDoneResult(w, jobID, done)
	default:
		writeJSON(w, http.StatusAccepted, client.ExecResult{JobID: jobID, State: "running"})
	}
}

func writeDoneResult(w http.ResponseWriter, jobID string, done nodeFrame) {
	writeJSON(w, http.StatusOK, client.ExecResult{
		JobID:    jobID,
		State:    done.State,
		ExitCode: done.ExitCode,
		Stdout:   done.Stdout,
		Stderr:   done.Stderr,
		Bytes:    done.Bytes,
	})
}

func (s *Server) resolveJobSession(w http.ResponseWriter, r *http.Request) (string, registry.Device, *nodeSession, bool) {
	jobID := r.PathValue("id")
	device, err := s.store.JobDevice(r.Context(), jobID)
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return "", registry.Device{}, nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not lookup job")
		return "", registry.Device{}, nil, false
	}
	if device.ExpiresAt != nil && !device.ExpiresAt.After(time.Now()) {
		writeError(w, http.StatusServiceUnavailable, "device "+device.Name+" is offline")
		return "", registry.Device{}, nil, false
	}
	session, _ := s.sessionFor(device.ID)
	if session == nil {
		writeError(w, http.StatusServiceUnavailable, "device "+device.Name+" is offline")
		return "", registry.Device{}, nil, false
	}
	return jobID, device, session, true
}

func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID, device, session, ok := s.resolveJobSession(w, r)
	if !ok {
		return
	}
	reply, err := session.call(r.Context(), nodeFrame{Type: "job_status", JobID: jobID})
	if !writeRPCErr(w, reply, err) {
		return
	}
	writeJSON(w, http.StatusOK, client.JobStatus{
		JobID:    jobID,
		Device:   device.Name,
		State:    reply.State,
		ExitCode: reply.ExitCode,
		Bytes:    reply.Bytes,
	})
}

func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	offset, err := parseOffsetQuery(r.URL.Query().Get("offset"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid offset")
		return
	}
	jobID, _, session, ok := s.resolveJobSession(w, r)
	if !ok {
		return
	}
	reply, callErr := session.call(r.Context(), nodeFrame{Type: "job_logs", JobID: jobID, Offset: offset})
	if !writeRPCErr(w, reply, callErr) {
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(reply.Chunk); err != nil {
		return
	}
	if !reply.EOF {
		streamRemainingLogs(r.Context(), w, session, nodeFrame{
			Type:   "job_logs",
			JobID:  jobID,
			Offset: offset + int64(len(reply.Chunk)),
		})
	}
}

func streamRemainingLogs(ctx context.Context, w http.ResponseWriter, session *nodeSession, next nodeFrame) {
	req := next
	for {
		reply, err := session.call(ctx, req)
		if err != nil || reply.Error != "" || (!reply.EOF && len(reply.Chunk) == 0) {
			// bits:allow(GO001): net/http sentinel aborts chunked response on mid-stream error
			panic(http.ErrAbortHandler)
		}
		if _, writeErr := w.Write(reply.Chunk); writeErr != nil || reply.EOF {
			return
		}
		req.Offset += int64(len(reply.Chunk))
	}
}

func parseOffsetQuery(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	offset, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid offset: %w", err)
	}
	if offset < 0 {
		return 0, errors.New("negative offset")
	}
	return offset, nil
}

func (s *Server) handleJobKill(w http.ResponseWriter, r *http.Request) {
	jobID, _, session, ok := s.resolveJobSession(w, r)
	if !ok {
		return
	}
	reply, err := session.call(r.Context(), nodeFrame{Type: "job_kill", JobID: jobID})
	if !writeRPCErr(w, reply, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"job_id": jobID, "state": "killed"})
}

func writeRPCErr(w http.ResponseWriter, reply nodeFrame, err error) bool {
	if errors.Is(err, errDeviceOffline) {
		writeError(w, http.StatusServiceUnavailable, "device is offline")
		return false
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return false
	}
	if reply.Error != "" {
		status := http.StatusBadGateway
		if reply.State == "not_found" {
			status = http.StatusNotFound
		}
		writeError(w, status, reply.Error)
		return false
	}
	return true
}
