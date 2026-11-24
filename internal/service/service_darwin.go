//go:build darwin

package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
)

const launchdLabel = "io.devicelink.node"

func Run(ctx context.Context, runFn func(context.Context) error) error {
	return runFn(ctx)
}

func Install(ctx context.Context, cfg Config) (string, error) {
	domain, plistPath, err := launchdTarget()
	if err != nil {
		return "", err
	}
	if err := writeUnitAtomic(plistPath, renderLaunchdPlist(cfg)); err != nil {
		return "", err
	}
	_, _ = runCommandOutput(ctx, "launchctl", "bootout", domain, plistPath)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := runCommand(ctx, "launchctl", "bootstrap", domain, plistPath); err != nil {
		return "", err
	}
	if err := runCommand(ctx, "launchctl", "kickstart", "-k", domain+"/"+launchdLabel); err != nil {
		return "", err
	}
	return plistPath, nil
}

func launchdTarget() (string, string, error) {
	if os.Geteuid() == 0 {
		return "system", filepath.Join("/Library/LaunchDaemons", launchdLabel+".plist"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve user home: %w", err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	return domain, plistPath, nil
}

func renderLaunchdPlist(cfg Config) []byte {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	buf.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	buf.WriteString(`<plist version="1.0"><dict>` + "\n")
	buf.WriteString("  <key>Label</key><string>" + xmlEscape(launchdLabel) + "</string>\n")
	buf.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range cfg.Command() {
		buf.WriteString("    <string>" + xmlEscape(arg) + "</string>\n")
	}
	buf.WriteString("  </array>\n")
	buf.WriteString("  <key>RunAtLoad</key><true/>\n")
	buf.WriteString("  <key>KeepAlive</key><true/>\n")
	logPath := xmlEscape(filepath.Join(cfg.StateDir, "service.log"))
	buf.WriteString("  <key>StandardOutPath</key><string>" + logPath + "</string>\n")
	buf.WriteString("  <key>StandardErrorPath</key><string>" + logPath + "</string>\n")
	buf.WriteString("</dict></plist>\n")
	return buf.Bytes()
}

func xmlEscape(value string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}
