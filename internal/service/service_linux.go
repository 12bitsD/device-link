//go:build linux

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const systemdUnitName = "device-link.service"

func Run(ctx context.Context, runFn func(context.Context) error) error {
	return runFn(ctx)
}

func Install(ctx context.Context, cfg Config) (string, error) {
	unitPath, wantedBy, scopeArgs, err := systemdTarget()
	if err != nil {
		return "", err
	}
	content := renderSystemdUnit(cfg, wantedBy)
	if err := writeUnitAtomic(unitPath, []byte(content)); err != nil {
		return "", err
	}
	reloadArgs := append(append([]string{}, scopeArgs...), "daemon-reload")
	if err := runCommand(ctx, "systemctl", reloadArgs...); err != nil {
		return "", err
	}
	enableArgs := append(append([]string{}, scopeArgs...), "enable", "--now", systemdUnitName)
	if err := runCommand(ctx, "systemctl", enableArgs...); err != nil {
		return "", err
	}
	return unitPath, nil
}

func systemdTarget() (string, string, []string, error) {
	if os.Geteuid() == 0 {
		return filepath.Join("/etc/systemd/system", systemdUnitName), "multi-user.target", nil, nil
	}
	configRoot := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(configRoot) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", nil, fmt.Errorf("resolve user home: %w", err)
		}
		configRoot = filepath.Join(home, ".config")
	}
	unitPath := filepath.Join(configRoot, "systemd", "user", systemdUnitName)
	return unitPath, "default.target", []string{"--user"}, nil
}

func renderSystemdUnit(cfg Config, wantedBy string) string {
	argv := cfg.Command()
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, quoteSystemdArg(arg))
	}
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Device Link Node\nAfter=network-online.target\n\n")
	b.WriteString("[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + strings.Join(quoted, " ") + "\n")
	b.WriteString("Restart=always\nRestartSec=2\n\n")
	b.WriteString("[Install]\nWantedBy=" + wantedBy + "\n")
	return b.String()
}

func quoteSystemdArg(arg string) string {
	escaped := strings.ReplaceAll(arg, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	escaped = strings.ReplaceAll(escaped, "\r", `\r`)
	escaped = strings.ReplaceAll(escaped, "%", "%%")
	escaped = strings.ReplaceAll(escaped, "$", "$$")
	return `"` + escaped + `"`
}
