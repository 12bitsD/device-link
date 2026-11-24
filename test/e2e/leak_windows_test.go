//go:build windows

package e2e

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

const (
	windowsProcessQueryLimitedInfo = 0x1000
	windowsStillActive             = 259
)

func waitProcessGroupDead(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	rootPID := uint32(cmd.Process.Pid)
	deadline := time.Now().Add(timeout)
	for {
		alivePID, alive, err := windowsProcessTreeAlive(rootPID)
		if err != nil {
			return fmt.Errorf("snapshot windows process tree: %w", err)
		}
		if !alive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("windows process %d (root %d) still alive", alivePID, rootPID)
		}
		// bits:allow(E2E_SLEEP): poll briefly for windows process tree exit
		time.Sleep(20 * time.Millisecond)
	}
}

func windowsProcessTreeAlive(rootPID uint32) (uint32, bool, error) {
	if windowsPIDAlive(rootPID) {
		return rootPID, true, nil
	}
	children, err := snapshotProcessChildren()
	if err != nil {
		return 0, false, err
	}
	queue := append([]uint32{}, children[rootPID]...)
	seen := map[uint32]bool{rootPID: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if windowsPIDAlive(pid) {
			return pid, true, nil
		}
		queue = append(queue, children[pid]...)
	}
	return 0, false, nil
}

func snapshotProcessChildren() (map[uint32][]uint32, error) {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.CloseHandle(snapshot) }()
	children := make(map[uint32][]uint32)
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for stepErr := syscall.Process32First(snapshot, &entry); stepErr == nil; stepErr = syscall.Process32Next(snapshot, &entry) {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
	}
	return children, nil
}

func windowsPIDAlive(pid uint32) bool {
	handle, err := syscall.OpenProcess(windowsProcessQueryLimitedInfo, false, pid)
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	var code uint32
	if exitErr := syscall.GetExitCodeProcess(handle, &code); exitErr != nil {
		return false
	}
	return code == windowsStillActive
}
