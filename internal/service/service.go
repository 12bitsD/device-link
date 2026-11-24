package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	commandTimeout   = 30 * time.Second
	commandWaitDelay = 3 * time.Second
)

type Config struct {
	BinaryPath string
	HubURL     string
	StateDir   string
	CAPath     string
}

func Resolve(hubURL, stateDir, caPath string) (Config, error) {
	if hasControlChar(hubURL) || hasControlChar(stateDir) || hasControlChar(caPath) {
		return Config{}, fmt.Errorf("service arguments cannot contain control characters")
	}
	exePath, err := os.Executable()
	if err != nil {
		return Config{}, fmt.Errorf("locate dl executable: %w", err)
	}
	absExe, err := filepath.Abs(exePath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve executable path: %w", err)
	}
	if hasControlChar(absExe) {
		return Config{}, fmt.Errorf("executable path contains control characters")
	}
	absState, err := filepath.Abs(stateDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve state-dir: %w", err)
	}
	absCA, err := resolveCAPath(caPath)
	if err != nil {
		return Config{}, err
	}
	return Config{BinaryPath: absExe, HubURL: hubURL, StateDir: absState, CAPath: absCA}, nil
}

func resolveCAPath(caPath string) (string, error) {
	if caPath == "" {
		return "", nil
	}
	absCA, err := filepath.Abs(caPath)
	if err != nil {
		return "", fmt.Errorf("resolve ca path: %w", err)
	}
	if _, statErr := os.Stat(absCA); statErr != nil {
		return "", fmt.Errorf("stat ca path: %w", statErr)
	}
	return absCA, nil
}

func hasControlChar(value string) bool {
	return strings.ContainsAny(value, "\x00\r\n")
}

func (c Config) Command() []string {
	argv := []string{c.BinaryPath, "join", "--hub", c.HubURL, "--state-dir", c.StateDir}
	if c.CAPath != "" {
		argv = append(argv, "--ca", c.CAPath)
	}
	return argv
}

func runCommand(ctx context.Context, name string, args ...string) error {
	_, err := runCommandOutput(ctx, name, args...)
	return err
}

func runCommandOutput(ctx context.Context, name string, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.WaitDelay = commandWaitDelay
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), text, err)
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}
