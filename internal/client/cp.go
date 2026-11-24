package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/12bitsD/device-link/internal/transfer"
)

type Endpoint struct {
	Device string
	Path   string
}

func ParseEndpoint(raw string) (Endpoint, error) {
	if strings.TrimSpace(raw) == "" {
		return Endpoint{}, fmt.Errorf("empty endpoint path")
	}
	if isWindowsDrivePath(raw) {
		return Endpoint{Device: "local", Path: raw}, nil
	}
	prefix, rest, hasColon := strings.Cut(raw, ":")
	if !hasColon || strings.ContainsAny(prefix, `/\`) {
		return Endpoint{Device: "local", Path: raw}, nil
	}
	if prefix == "" || strings.TrimSpace(rest) == "" {
		return Endpoint{}, fmt.Errorf("invalid endpoint %q", raw)
	}
	return Endpoint{Device: prefix, Path: rest}, nil
}

func isWindowsDrivePath(raw string) bool {
	if len(raw) < 3 || raw[1] != ':' {
		return false
	}
	ch := raw[0]
	isAlpha := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
	return isAlpha && (raw[2] == '\\' || raw[2] == '/')
}

type TransferScanRequest struct {
	Device   string   `json:"device"`
	Sources  []string `json:"sources"`
	Excludes []string `json:"excludes,omitempty"`
}

type TransferScanResponse struct {
	Candidates []transfer.Candidate `json:"candidates"`
	TotalBytes int64                `json:"total_bytes"`
}

type TransferStartRequest struct {
	From           string               `json:"from"`
	To             string               `json:"to"`
	Sources        []string             `json:"sources,omitempty"`
	Excludes       []string             `json:"excludes,omitempty"`
	TargetDir      string               `json:"target_dir,omitempty"`
	Candidates     []transfer.Candidate `json:"candidates,omitempty"`
	ExistingHashes map[string]string    `json:"existing_hashes,omitempty"`
	TotalBytes     int64                `json:"total_bytes"`
	WaitMS         int64                `json:"wait_ms"`
}

type TransferStatus struct {
	TransferID     string            `json:"transfer_id"`
	From           string            `json:"from"`
	To             string            `json:"to"`
	State          string            `json:"state"`
	Files          int               `json:"files"`
	Skipped        int               `json:"skipped"`
	Bytes          int64             `json:"bytes"`
	TotalBytes     int64             `json:"total_bytes"`
	ExistingHashes map[string]string `json:"existing_hashes,omitempty"`
}

func (c *Client) ScanRemoteSources(ctx context.Context, req TransferScanRequest) (TransferScanResponse, error) {
	reqCtx, cancel := context.WithTimeout(ctx, defaultControlTimeout)
	defer cancel()
	response, err := c.request(reqCtx, http.MethodPost, "/v1/transfers/scan", req)
	if err != nil {
		return TransferScanResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return TransferScanResponse{}, responseError("cp scan", response)
	}
	var out TransferScanResponse
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		return TransferScanResponse{}, fmt.Errorf("decode scan response: %w", err)
	}
	return out, nil
}

func (c *Client) StartTransfer(ctx context.Context, req TransferStartRequest) (TransferStatus, error) {
	timeout := defaultControlTimeout
	if req.WaitMS > 0 {
		timeout += time.Duration(req.WaitMS) * time.Millisecond
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := c.request(reqCtx, http.MethodPost, "/v1/transfers", req)
	if err != nil {
		return TransferStatus{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusCreated {
		return TransferStatus{}, responseError("cp", response)
	}
	var out TransferStatus
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		return TransferStatus{}, fmt.Errorf("decode transfer response: %w", err)
	}
	return out, nil
}

func (c *Client) UploadTransfer(ctx context.Context, transferID string, plan transfer.Plan) (TransferStatus, error) {
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
	request, err := c.newUploadRequest(ctx, transferID, plan, pipeR)
	if err != nil {
		return TransferStatus{}, err
	}
	response, doErr := c.http.Do(request)
	if doErr != nil {
		return TransferStatus{}, preferWriteErr(writeErr, writerDone, pipeR, fmt.Errorf("upload transfer: %w", doErr))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return TransferStatus{}, preferWriteErr(writeErr, writerDone, pipeR, responseError("cp upload", response))
	}
	var status TransferStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return TransferStatus{}, fmt.Errorf("decode upload status: %w", err)
	}
	return status, nil
}

func (c *Client) newUploadRequest(ctx context.Context, transferID string, plan transfer.Plan, body io.Reader) (*http.Request, error) {
	query := url.Values{}
	query.Set("files", strconv.Itoa(len(plan.Files)))
	query.Set("skipped", strconv.Itoa(plan.SkippedFiles))
	query.Set("transfer_bytes", strconv.FormatInt(plan.TransferBytes, 10))
	path := "/v1/transfers/" + url.PathEscape(transferID) + "/upload?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("create upload request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/x-tar")
	return request, nil
}

func preferWriteErr(writeErr error, writerDone <-chan struct{}, pipeR *io.PipeReader, fallback error) error {
	_ = pipeR.Close()
	<-writerDone
	if writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
		return writeErr
	}
	return fallback
}

func (c *Client) DownloadTransfer(ctx context.Context, transferID, targetDir string) (TransferStatus, error) {
	dlPath := "/v1/transfers/" + url.PathEscape(transferID) + "/download"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+dlPath, nil)
	if err != nil {
		return TransferStatus{}, fmt.Errorf("create download request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(request)
	if err != nil {
		return TransferStatus{}, fmt.Errorf("download transfer: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return TransferStatus{}, responseError("cp download", response)
	}
	limited := io.LimitReader(response.Body, transfer.MaxRawTarStreamBytes)
	extractErr := transfer.ExtractTar(ctx, limited, targetDir)
	if extractErr == nil {
		_, extractErr = io.Copy(io.Discard, limited)
	}
	errMsg := ""
	if extractErr != nil {
		errMsg = extractErr.Error()
	}
	status, completeErr := c.completeTransfer(ctx, transferID, errMsg)
	if extractErr != nil {
		return TransferStatus{}, extractErr
	}
	return status, completeErr
}

func (c *Client) FailTransfer(ctx context.Context, transferID, errMsg string) {
	reason := errMsg
	if reason == "" {
		reason = "controller transfer aborted"
	}
	_, _ = c.completeTransfer(ctx, transferID, reason)
}

func (c *Client) completeTransfer(ctx context.Context, transferID, errMsg string) (TransferStatus, error) {
	baseCtx := ctx
	if errMsg != "" {
		baseCtx = context.WithoutCancel(ctx)
	}
	reqCtx, cancel := context.WithTimeout(baseCtx, defaultControlTimeout)
	defer cancel()
	path := "/v1/transfers/" + url.PathEscape(transferID) + "/complete"
	response, err := c.request(reqCtx, http.MethodPost, path, map[string]string{"error": errMsg})
	if err != nil {
		return TransferStatus{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return TransferStatus{}, responseError("cp complete", response)
	}
	var status TransferStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return TransferStatus{}, fmt.Errorf("decode completion status: %w", err)
	}
	return status, nil
}

func (c *Client) TransferStatus(ctx context.Context, transferID string) (TransferStatus, error) {
	reqCtx, cancel := context.WithTimeout(ctx, defaultControlTimeout)
	defer cancel()
	response, err := c.request(reqCtx, http.MethodGet, "/v1/transfers/"+url.PathEscape(transferID), nil)
	if err != nil {
		return TransferStatus{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return TransferStatus{}, responseError("transfer status", response)
	}
	var status TransferStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return TransferStatus{}, fmt.Errorf("decode transfer status: %w", err)
	}
	return status, nil
}

func (c *Client) TransferCancel(ctx context.Context, transferID string) error {
	reqCtx, cancel := context.WithTimeout(ctx, defaultControlTimeout)
	defer cancel()
	response, err := c.request(reqCtx, http.MethodPost, "/v1/transfers/"+url.PathEscape(transferID)+"/cancel", nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return responseError("transfer cancel", response)
	}
	return nil
}
