//go:build windows

package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type procGroup struct {
	job windows.Handle
}

func prepareJobCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func attachProcessGroup(cmd *exec.Cmd) (*procGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create windows job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure windows job object: %w", err)
	}
	const access = windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE
	procHandle, err := windows.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return &procGroup{}, nil
	}
	defer func() { _ = windows.CloseHandle(procHandle) }()
	if err := windows.AssignProcessToJobObject(job, procHandle); err != nil {
		var code uint32
		if exitErr := windows.GetExitCodeProcess(procHandle, &code); exitErr == nil && code != uint32(windows.STATUS_PENDING) {
			_ = windows.CloseHandle(job)
			return &procGroup{}, nil
		}
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("assign process to job object: %w", err)
	}
	return &procGroup{job: job}, nil
}

func (g *procGroup) Kill() error {
	if g == nil || g.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(g.job, 1)
}

func (g *procGroup) Close() error {
	if g == nil || g.job == 0 {
		return nil
	}
	err := windows.CloseHandle(g.job)
	g.job = 0
	return err
}

func buildShellCommand(shellPath, script string) *exec.Cmd {
	base := strings.ToLower(filepath.Base(shellPath))
	if strings.Contains(base, "powershell") || strings.Contains(base, "pwsh") {
		return exec.Command(shellPath, "-NoProfile", "-NonInteractive", "-Command", script)
	}
	return exec.Command(shellPath, "/d", "/s", "/c", script)
}

func isExecutableCandidate(info os.FileInfo) bool {
	return !info.IsDir()
}

func executableExtensions(env []string) []string {
	raw := envValue(env, "PATHEXT")
	if raw == "" {
		raw = ".COM;.EXE;.BAT;.CMD"
	}
	exts := []string{""}
	for _, ext := range strings.Split(raw, ";") {
		if ext != "" {
			exts = append(exts, ext)
		}
	}
	return exts
}

func resolveLoginPath(_ context.Context, _ string) string {
	return os.Getenv("PATH")
}
