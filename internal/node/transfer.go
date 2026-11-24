package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/12bitsD/device-link/internal/transfer"
)

func (s *sessionConn) executeTransferRPC(ctx context.Context, frame hubFrame) (hubFrame, func()) {
	reply := hubFrame{Type: "rpc_reply", ReqID: frame.ReqID, TransferID: frame.TransferID}
	switch frame.Type {
	case "transfer_scan":
		scan, err := transfer.ScanSources(ctx, frame.Sources, frame.Excludes)
		if !setRPCError(&reply, err) {
			reply.Candidates = scan.Candidates()
			reply.TotalBytes = scan.TotalBytes
		}
		return reply, nil
	case "transfer_inspect":
		hashes, err := transfer.InspectTarget(ctx, frame.TargetDir, frame.Candidates)
		if !setRPCError(&reply, err) {
			reply.ExistingHashes = hashes
		}
		return reply, nil
	case "transfer_send":
		return reply, s.prepareSendLauncher(ctx, frame, &reply)
	case "transfer_recv":
		return reply, func() {
			s.rpcWG.Add(1)
			go func() {
				defer s.rpcWG.Done()
				s.streamDownloadFromHub(ctx, frame.TransferID, frame.StreamToken, frame.TargetDir)
			}()
		}
	default:
		return reply, nil
	}
}

func (s *sessionConn) prepareSendLauncher(ctx context.Context, frame hubFrame, reply *hubFrame) func() {
	scan, err := transfer.ScanSources(ctx, frame.Sources, frame.Excludes)
	if setRPCError(reply, err) {
		return nil
	}
	plan, planErr := transfer.FinalizePlan(ctx, scan, frame.ExistingHashes)
	if setRPCError(reply, planErr) {
		return nil
	}
	reply.Files = len(plan.Files)
	reply.Skipped = plan.SkippedFiles
	reply.TransferBytes = plan.TransferBytes
	reply.TotalBytes = plan.TotalBytes
	return func() {
		s.rpcWG.Add(1)
		go func() {
			defer s.rpcWG.Done()
			s.streamUploadToHub(ctx, frame.TransferID, frame.StreamToken, plan)
		}()
	}
}

func (s *sessionConn) streamUploadToHub(ctx context.Context, transferID, streamToken string, plan transfer.Plan) {
	pipeR, pipeW := io.Pipe()
	writerDone := make(chan struct{})
	var writeErr error
	go func() {
		defer close(writerDone)
		writeErr = transfer.WriteTar(ctx, pipeW, plan)
		_ = pipeW.CloseWithError(writeErr)
	}()
	defer func() {
		_ = pipeR.Close()
		<-writerDone
	}()
	targetURL := s.hubURL + "/v1/node-transfers/" + url.PathEscape(transferID) + "/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, targetURL, pipeR)
	if err != nil {
		s.reportTransferDone(ctx, transferID, streamToken, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("X-Transfer-Token", streamToken)
	req.Header.Set("Content-Type", "application/x-tar")
	resp, doErr := s.httpClient.Do(req)
	if doErr != nil {
		s.reportTransferDone(ctx, transferID, streamToken, s.resolveWriteErr(pipeR, writerDone, &writeErr, doErr))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		statusErr := fmt.Errorf("upload status %s", resp.Status)
		s.reportTransferDone(ctx, transferID, streamToken, s.resolveWriteErr(pipeR, writerDone, &writeErr, statusErr))
	}
}

func (s *sessionConn) resolveWriteErr(pipeR *io.PipeReader, writerDone <-chan struct{}, writeErr *error, fallback error) string {
	_ = pipeR.Close()
	<-writerDone
	if *writeErr != nil && !errors.Is(*writeErr, io.ErrClosedPipe) {
		return (*writeErr).Error()
	}
	return fallback.Error()
}

func (s *sessionConn) streamDownloadFromHub(ctx context.Context, transferID, streamToken, targetDir string) {
	targetURL := s.hubURL + "/v1/node-transfers/" + url.PathEscape(transferID) + "/download"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		s.reportTransferDone(ctx, transferID, streamToken, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("X-Transfer-Token", streamToken)
	resp, doErr := s.httpClient.Do(req)
	if doErr != nil {
		s.reportTransferDone(ctx, transferID, streamToken, doErr.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		s.reportTransferDone(ctx, transferID, streamToken, fmt.Sprintf("download status %s", resp.Status))
		return
	}
	limited := io.LimitReader(resp.Body, transfer.MaxRawTarStreamBytes)
	extractErr := transfer.ExtractTar(ctx, limited, targetDir)
	if extractErr == nil {
		_, extractErr = io.Copy(io.Discard, limited)
	}
	errMsg := ""
	if extractErr != nil {
		errMsg = extractErr.Error()
	}
	s.reportTransferDone(ctx, transferID, streamToken, errMsg)
}

func (s *sessionConn) reportTransferDone(ctx context.Context, transferID, streamToken, errMsg string) {
	reqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), frameTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"error": errMsg})
	if err != nil {
		return
	}
	targetURL := s.hubURL + "/v1/node-transfers/" + url.PathEscape(transferID) + "/done"
	req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if reqErr != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("X-Transfer-Token", streamToken)
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := s.httpClient.Do(req)
	if doErr != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
}
