//go:build !windows

package e2e

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

func waitProcessGroupDead(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if time.Now().After(deadline) {
			return killAndFormatGroupError(pid, err)
		}
		// bits:allow(E2E_SLEEP): poll briefly for kernel process group reap
		time.Sleep(20 * time.Millisecond)
	}
}

func killAndFormatGroupError(pid int, err error) error {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if err == nil {
		return fmt.Errorf("process group %d still alive", pid)
	}
	return fmt.Errorf("process group %d check: %w", pid, err)
}
