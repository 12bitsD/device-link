//go:build !windows

package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type procGroup struct {
	pid int
}

func prepareJobCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachProcessGroup(cmd *exec.Cmd) (*procGroup, error) {
	return &procGroup{pid: cmd.Process.Pid}, nil
}

func (g *procGroup) Kill() error {
	if g == nil || g.pid <= 0 {
		return nil
	}
	err := syscall.Kill(-g.pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (g *procGroup) Close() error {
	return g.Kill()
}

func buildShellCommand(shellPath, script string) *exec.Cmd {
	if filepath.Base(shellPath) == "zsh" {
		return exec.Command(shellPath, "-f", "-c", script)
	}
	return exec.Command(shellPath, "-c", script)
}

func isExecutableCandidate(info os.FileInfo) bool {
	return !info.IsDir() && info.Mode()&0o111 != 0
}

func executableExtensions(_ []string) []string {
	return []string{""}
}

func resolveLoginPath(ctx context.Context, shellPath string) string {
	const fallback = "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, shellPath, "-l", "-c", `printf %s "$PATH"`)
	cmd.Env = replaceOrAppendEnv(os.Environ(), "PATH", fallback)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 200 * time.Millisecond
	out, err := cmd.Output()
	probed := strings.TrimSpace(string(out))
	if err != nil || probed == "" {
		return mergePathEntries(os.Getenv("PATH"), fallback)
	}
	return mergePathEntries(probed, fallback)
}

func mergePathEntries(primary, secondary string) string {
	seen := make(map[string]bool)
	var entries []string
	for _, list := range []string{primary, secondary} {
		for _, item := range strings.Split(list, string(os.PathListSeparator)) {
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			entries = append(entries, item)
		}
	}
	return strings.Join(entries, string(os.PathListSeparator))
}
