//go:build darwin || linux

package service

import (
	"fmt"
	"os"
	"path/filepath"
)

func writeUnitAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create service directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".dl-unit-*")
	if err != nil {
		return fmt.Errorf("stage service file %s: %w", path, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("write service file %s: %w", path, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("chmod service file %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close service file %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit service file %s: %w", path, err)
	}
	committed = true
	return nil
}
