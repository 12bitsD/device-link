package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type ExecRequest struct {
	Command      []string `json:"command"`
	Cwd          string   `json:"cwd,omitempty"`
	Env          []string `json:"env,omitempty"`
	WaitMS       int64    `json:"wait_ms"`
	TimeoutMS    int64    `json:"timeout_ms"`
	WaitOnlineMS int64    `json:"wait_online_ms"`
}

type ExecResult struct {
	JobID    string `json:"job_id"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	Bytes    int64  `json:"bytes"`
}

type JobStatus struct {
	JobID    string `json:"job_id"`
	Device   string `json:"device"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
	Bytes    int64  `json:"bytes"`
}

func DurationMillis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	ms := d.Milliseconds()
	if ms == 0 {
		return 1
	}
	return ms
}

func (c *Client) Exec(ctx context.Context, device string, req ExecRequest) (ExecResult, error) {
	budget := defaultControlTimeout +
		time.Duration(max(req.WaitOnlineMS, 0))*time.Millisecond +
		time.Duration(max(req.WaitMS, 0))*time.Millisecond
	reqCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	path := "/v1/devices/" + url.PathEscape(device) + "/exec"
	response, err := c.request(reqCtx, http.MethodPost, path, req)
	if err != nil {
		return ExecResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		return ExecResult{}, responseError("exec", response)
	}
	var result ExecResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return ExecResult{}, fmt.Errorf("decode exec result: %w", err)
	}
	return result, nil
}

func (c *Client) JobStatus(ctx context.Context, jobID string) (JobStatus, error) {
	reqCtx, cancel := context.WithTimeout(ctx, defaultControlTimeout)
	defer cancel()
	response, err := c.request(reqCtx, http.MethodGet, "/v1/jobs/"+url.PathEscape(jobID), nil)
	if err != nil {
		return JobStatus{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return JobStatus{}, responseError("job status", response)
	}
	var status JobStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return JobStatus{}, fmt.Errorf("decode job status: %w", err)
	}
	return status, nil
}

func (c *Client) JobLogs(ctx context.Context, jobID string, offset int64, dst io.Writer) error {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	path := "/v1/jobs/" + url.PathEscape(jobID) + "/logs?offset=" + strconv.FormatInt(offset, 10)
	response, err := c.request(reqCtx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return responseError("job logs", response)
	}
	if _, err := io.Copy(dst, response.Body); err != nil {
		return fmt.Errorf("stream job logs: %w", err)
	}
	return nil
}

func (c *Client) JobKill(ctx context.Context, jobID string) error {
	reqCtx, cancel := context.WithTimeout(ctx, defaultControlTimeout)
	defer cancel()
	response, err := c.request(reqCtx, http.MethodPost, "/v1/jobs/"+url.PathEscape(jobID)+"/kill", nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return responseError("job kill", response)
	}
	return nil
}
