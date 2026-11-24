package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	maxInMemoryStream = 4 << 20
	logChunkSize      = 256 << 10
)

type jobManager struct {
	dir       string
	shellPath string
	loginPath string
	mu        sync.Mutex
	closed    bool
	jobs      map[string]*managedJob
	notify    func(hubFrame)
}

type managedJob struct {
	id         string
	mu         sync.Mutex
	killReason string
	stdout     cappedBuffer
	stderr     cappedBuffer
	group      *procGroup
	done       chan struct{}
}

type jobMeta struct {
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
}

type cappedBuffer struct {
	buf bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := maxInMemoryStream - b.buf.Len()
	if remaining > 0 {
		_, _ = b.buf.Write(p[:min(len(p), remaining)])
	}
	return len(p), nil
}

type syncFileWriter struct {
	mu   sync.Mutex
	file *os.File
}

func (w *syncFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Write(p)
}

func newJobManager(ctx context.Context, stateDir, shellPath string) (*jobManager, error) {
	dir := filepath.Join(stateDir, "jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create jobs dir: %w", err)
	}
	return &jobManager{
		dir:       dir,
		shellPath: shellPath,
		loginPath: resolveLoginPath(ctx, shellPath),
		jobs:      make(map[string]*managedJob),
	}, nil
}

func (m *jobManager) logPath(jobID string) string  { return filepath.Join(m.dir, jobID+".log") }
func (m *jobManager) metaPath(jobID string) string { return filepath.Join(m.dir, jobID+".json") }

func (m *jobManager) setNotifier(fn func(hubFrame)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

func (m *jobManager) emit(frame hubFrame) {
	m.mu.Lock()
	fn := m.notify
	m.mu.Unlock()
	if fn != nil {
		fn(frame)
	}
}

func (m *jobManager) Close() {
	m.mu.Lock()
	m.closed = true
	active := make([]*managedJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		active = append(active, job)
	}
	m.mu.Unlock()
	for _, job := range active {
		job.abort("killed")
		<-job.done
	}
}

func validJobID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, ch := range id {
		ok := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-'
		if !ok {
			return false
		}
	}
	return true
}

func (m *jobManager) start(frame hubFrame) error {
	if !validJobID(frame.JobID) {
		return fmt.Errorf("invalid job id")
	}
	if len(frame.Command) == 0 {
		return fmt.Errorf("command is required")
	}
	if err := validateCwd(frame.Cwd); err != nil {
		return err
	}
	env, err := m.composeEnv(frame.Env)
	if err != nil {
		return err
	}
	cmd, err := buildCommand(m.shellPath, frame.Command, env)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(m.logPath(frame.JobID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open job log: %w", err)
	}
	if err := m.launchProcess(frame, cmd, env, logFile); err != nil {
		_ = logFile.Close()
		return err
	}
	return nil
}

func (m *jobManager) launchProcess(frame hubFrame, cmd *exec.Cmd, env []string, logFile *os.File) error {
	job := &managedJob{id: frame.JobID, done: make(chan struct{})}
	cmd.Dir = frame.Cwd
	cmd.Env = env
	cmd.Stdin = nil
	cmd.WaitDelay = 200 * time.Millisecond
	spool := &syncFileWriter{file: logFile}
	cmd.Stdout = io.MultiWriter(&job.stdout, spool)
	cmd.Stderr = io.MultiWriter(&job.stderr, spool)
	prepareJobCommand(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}
	group, err := attachProcessGroup(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	job.group = group
	if !m.registerJob(job) {
		_ = group.Close()
		_ = cmd.Wait()
		return fmt.Errorf("node is shutting down")
	}
	go m.supervise(job, cmd, logFile, time.Duration(frame.TimeoutMS)*time.Millisecond)
	return nil
}

func (m *jobManager) registerJob(job *managedJob) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.jobs[job.id] = job
	return true
}

func buildCommand(shellPath string, command, env []string) (*exec.Cmd, error) {
	if len(command) == 1 {
		return buildShellCommand(shellPath, command[0]), nil
	}
	resolved, err := resolveProgram(command[0], env)
	if err != nil {
		return nil, err
	}
	return &exec.Cmd{Path: resolved, Args: command}, nil
}

func resolveProgram(program string, env []string) (string, error) {
	if strings.ContainsAny(program, `/\`) {
		return program, nil
	}
	pathVal := envValue(env, "PATH")
	exts := executableExtensions(env)
	for _, dir := range strings.Split(pathVal, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		if match := matchExecutableInDir(dir, program, exts); match != "" {
			return match, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH", program)
}

func matchExecutableInDir(dir, program string, exts []string) string {
	for _, ext := range exts {
		candidate := filepath.Join(dir, program+ext)
		if info, err := os.Stat(candidate); err == nil && isExecutableCandidate(info) {
			return candidate
		}
	}
	return ""
}

func envValue(env []string, name string) string {
	for _, entry := range env {
		key, val, ok := strings.Cut(entry, "=")
		if ok && envKeysMatch(key, name) {
			return val
		}
	}
	return ""
}

func (m *jobManager) supervise(job *managedJob, cmd *exec.Cmd, logFile *os.File, timeout time.Duration) {
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			job.abort("timed_out")
		})
	}
	waitErr := cmd.Wait()
	if timer != nil {
		timer.Stop()
	}
	_ = logFile.Close()
	state, code := job.finish(waitErr)
	_ = writeJobMeta(m.metaPath(job.id), jobMeta{State: state, ExitCode: &code})
	size := fileSize(m.logPath(job.id))
	m.mu.Lock()
	delete(m.jobs, job.id)
	m.mu.Unlock()
	close(job.done)
	m.emit(hubFrame{
		Type:     "job_done",
		JobID:    job.id,
		State:    state,
		ExitCode: &code,
		Stdout:   job.stdout.buf.Bytes(),
		Stderr:   job.stderr.buf.Bytes(),
		Bytes:    size,
	})
}

func (j *managedJob) finish(waitErr error) (string, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.group != nil {
		_ = j.group.Close()
		j.group = nil
	}
	if j.killReason == "timed_out" {
		return "timed_out", 124
	}
	if j.killReason == "killed" {
		return "killed", 137
	}
	if waitErr == nil || errors.Is(waitErr, exec.ErrWaitDelay) {
		return "completed", 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.ExitCode() > 0 {
		return "failed", exitErr.ExitCode()
	}
	return "failed", 1
}

func (j *managedJob) abort(reason string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.group == nil {
		return
	}
	if j.killReason == "" {
		j.killReason = reason
	}
	_ = j.group.Kill()
}

func (m *jobManager) status(jobID string) (hubFrame, error) {
	if !validJobID(jobID) {
		return hubFrame{}, fmt.Errorf("job not found")
	}
	m.mu.Lock()
	job := m.jobs[jobID]
	m.mu.Unlock()
	logPath := m.logPath(jobID)
	if job != nil {
		return hubFrame{JobID: jobID, State: "running", Bytes: fileSize(logPath)}, nil
	}
	meta, err := readJobMeta(m.metaPath(jobID))
	if errors.Is(err, os.ErrNotExist) {
		return hubFrame{}, fmt.Errorf("job %s not found: %w", jobID, err)
	}
	if err != nil {
		return hubFrame{}, fmt.Errorf("read job %s metadata: %w", jobID, err)
	}
	return hubFrame{JobID: jobID, State: meta.State, ExitCode: meta.ExitCode, Bytes: fileSize(logPath)}, nil
}

func (m *jobManager) readLogs(jobID string, offset int64) ([]byte, bool, error) {
	if !validJobID(jobID) {
		return nil, false, fmt.Errorf("job not found")
	}
	file, err := os.Open(m.logPath(jobID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("job %s logs not found: %w", jobID, err)
	}
	if err != nil {
		return nil, false, fmt.Errorf("open job %s log: %w", jobID, err)
	}
	defer func() { _ = file.Close() }()
	buf := make([]byte, logChunkSize)
	n, readErr := file.ReadAt(buf, max(offset, int64(0)))
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, false, fmt.Errorf("read job log: %w", readErr)
	}
	eof := errors.Is(readErr, io.EOF) || n < len(buf)
	return buf[:n], eof, nil
}

func (m *jobManager) kill(jobID string) error {
	if !validJobID(jobID) {
		return fmt.Errorf("job not found")
	}
	m.mu.Lock()
	job := m.jobs[jobID]
	m.mu.Unlock()
	if job == nil {
		_, err := os.Stat(m.metaPath(jobID))
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("job %s not found: %w", jobID, err)
		}
		if err != nil {
			return fmt.Errorf("stat job %s: %w", jobID, err)
		}
		return nil
	}
	job.abort("killed")
	<-job.done
	return nil
}

func validateCwd(cwd string) error {
	if cwd == "" {
		return nil
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("invalid cwd %q: %w", cwd, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}
	return nil
}

func (m *jobManager) composeEnv(overrides []string) ([]string, error) {
	env := replaceOrAppendEnv(os.Environ(), "PATH", m.loginPath)
	for _, entry := range overrides {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid env %q (want KEY=VALUE)", entry)
		}
		env = replaceOrAppendEnv(env, key, val)
	}
	return env, nil
}

func replaceOrAppendEnv(env []string, key, val string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		existingKey, _, _ := strings.Cut(entry, "=")
		if envKeysMatch(existingKey, key) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, key+"="+val)
}

func envKeysMatch(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func writeJobMeta(path string, meta jobMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func readJobMeta(path string) (jobMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return jobMeta{}, err
	}
	var meta jobMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return jobMeta{}, err
	}
	return meta, nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
