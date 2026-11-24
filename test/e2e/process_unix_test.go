//go:build !windows

package e2e

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessTree(cmd *exec.Cmd) error {
	return signalProcessTree(cmd, syscall.SIGKILL)
}

func terminateProcessTree(cmd *exec.Cmd) error {
	return signalProcessTree(cmd, syscall.SIGTERM)
}

func signalProcessTree(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
