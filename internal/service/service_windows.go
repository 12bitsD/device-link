//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "DeviceLink"

func Run(ctx context.Context, runFn func(context.Context) error) error {
	inService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect windows service: %w", err)
	}
	if !inService {
		return runFn(ctx)
	}
	ws := &windowsService{run: runFn}
	if err := svc.Run(windowsServiceName, ws); err != nil {
		return fmt.Errorf("run windows service: %w", err)
	}
	return ws.err
}

type windowsService struct {
	run func(context.Context) error
	err error
}

func (w *windowsService) Execute(_ []string, changes <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- w.run(runCtx)
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	w.err = waitServiceStop(cancel, errCh, changes, status)
	if w.err != nil {
		return true, 1
	}
	return false, 0
}

func waitServiceStop(cancel context.CancelFunc, errCh <-chan error, changes <-chan svc.ChangeRequest, status chan<- svc.Status) error {
	stopping := false
	for {
		req, stopped, runErr := nextServiceEvent(errCh, changes)
		if stopped {
			if !stopping {
				status <- svc.Status{State: svc.StopPending}
			}
			if stopping && errors.Is(runErr, context.Canceled) {
				return nil
			}
			return runErr
		}
		if handleControlCmd(req, cancel, status) {
			stopping = true
		}
	}
}

func nextServiceEvent(errCh <-chan error, changes <-chan svc.ChangeRequest) (svc.ChangeRequest, bool, error) {
	select {
	case err := <-errCh:
		return svc.ChangeRequest{}, true, err
	case req := <-changes:
		return req, false, nil
	}
}

func handleControlCmd(req svc.ChangeRequest, cancel context.CancelFunc, status chan<- svc.Status) bool {
	switch req.Cmd {
	case svc.Interrogate:
		status <- req.CurrentStatus
		return false
	case svc.Stop, svc.Shutdown:
		status <- svc.Status{State: svc.StopPending}
		cancel()
		return true
	default:
		return false
	}
}

func Install(ctx context.Context, cfg Config) (string, error) {
	binPath := formatWindowsBinPath(cfg.Command())
	stopOut, _ := runCommandOutput(ctx, "sc", "stop", windowsServiceName)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.Contains(stopOut, "STOP_PENDING") {
		if err := waitWindowsStopped(ctx); err != nil {
			return "", err
		}
	}
	if err := runCommand(ctx, "sc", "create", windowsServiceName, "binPath=", binPath, "start=", "auto"); err != nil {
		if cfgErr := runCommand(ctx, "sc", "config", windowsServiceName, "binPath=", binPath, "start=", "auto"); cfgErr != nil {
			return "", errors.Join(err, cfgErr)
		}
	}
	if err := runCommand(ctx, "sc", "start", windowsServiceName); err != nil {
		return "", err
	}
	return windowsServiceName, nil
}

func waitWindowsStopped(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, _ := runCommandOutput(waitCtx, "sc", "query", windowsServiceName)
		if !strings.Contains(out, "STOP_PENDING") {
			return waitCtx.Err()
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for %s to stop: %w", windowsServiceName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func formatWindowsBinPath(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, quoteWindowsArg(arg))
	}
	return strings.Join(quoted, " ")
}

func quoteWindowsArg(arg string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(arg); i++ {
		switch arg[i] {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes*2+1))
			b.WriteByte('"')
			slashes = 0
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteByte(arg[i])
			slashes = 0
		}
	}
	b.WriteString(strings.Repeat(`\`, slashes*2))
	b.WriteByte('"')
	return b.String()
}
