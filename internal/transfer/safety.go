package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func hasDotDotSegment(raw string) bool {
	for _, seg := range strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

func ValidateRelPath(relSlash string) error {
	if relSlash == "" || strings.HasPrefix(relSlash, "/") || strings.ContainsRune(relSlash, '\\') {
		return fmt.Errorf("invalid archive path %q", relSlash)
	}
	if len(relSlash) >= 2 && relSlash[1] == ':' {
		return fmt.Errorf("invalid archive drive path %q", relSlash)
	}
	for _, seg := range strings.Split(relSlash, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("unsafe archive path segment in %q", relSlash)
		}
	}
	return nil
}

func inspectTargetRoot(targetDir string) (string, string, bool, error) {
	if strings.TrimSpace(targetDir) == "" || hasDotDotSegment(targetDir) {
		return "", "", false, fmt.Errorf("invalid target_dir %q", targetDir)
	}
	cleanTarget := filepath.Clean(targetDir)
	info, err := os.Lstat(cleanTarget)
	if errors.Is(err, os.ErrNotExist) {
		if ancestorErr := verifyExistingAncestors(cleanTarget); ancestorErr != nil {
			return "", "", false, ancestorErr
		}
		return cleanTarget, "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("stat target_dir %s: %w", targetDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", "", false, fmt.Errorf("target_dir %s cannot be a symbolic link", targetDir)
	}
	if !info.IsDir() {
		return "", "", false, fmt.Errorf("target_dir %s is not a directory", targetDir)
	}
	realTarget, evalErr := filepath.EvalSymlinks(cleanTarget)
	if evalErr != nil {
		return "", "", false, fmt.Errorf("resolve target_dir %s: %w", targetDir, evalErr)
	}
	return cleanTarget, realTarget, true, nil
}

func verifyExistingAncestors(cleanTarget string) error {
	cur := cleanTarget
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		info, err := os.Lstat(parent)
		if err == nil {
			return checkExistingAncestor(parent, info)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat target ancestor %s: %w", parent, err)
		}
		cur = parent
	}
}

func checkExistingAncestor(parent string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := os.Stat(parent)
		if err != nil || !resolved.IsDir() {
			return fmt.Errorf("target ancestor %s does not resolve to a directory", parent)
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("target ancestor %s is not a directory", parent)
	}
	return nil
}

func inspectCandidateTarget(ctx context.Context, root targetRoot, cand Candidate) (string, bool, error) {
	if err := verifyTargetSegments(root.real, root.clean, cand.RelPath); err != nil {
		return "", false, err
	}
	dest := filepath.Join(root.clean, filepath.FromSlash(cand.RelPath))
	info, err := os.Lstat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("stat target %s: %w", cand.RelPath, err)
	}
	if cand.IsDir {
		if !info.IsDir() {
			return "", false, fmt.Errorf("target %s exists and is not a directory", cand.RelPath)
		}
		return "", false, nil
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("target %s exists and is not a regular file", cand.RelPath)
	}
	if info.Size() != cand.Size {
		return "", false, nil
	}
	digest, hashErr := sha256File(ctx, dest)
	if hashErr != nil {
		return "", false, hashErr
	}
	return digest, true, nil
}

func verifyTargetSegments(realTarget, cleanTarget, relSlash string) error {
	segments := strings.Split(relSlash, "/")
	cur := cleanTarget
	for i, seg := range segments {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat target %s: %w", relSlash, err)
		}
		if i < len(segments)-1 {
			if err := checkDirSegment(cur, info); err != nil {
				return err
			}
			continue
		}
		if err := checkLeafSegment(realTarget, cur, info); err != nil {
			return err
		}
	}
	return nil
}

func checkDirSegment(cur string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("target directory segment %s is a symbolic link", cur)
	}
	if !info.IsDir() {
		return fmt.Errorf("target parent %s is not a directory", cur)
	}
	return nil
}

func checkLeafSegment(realTarget, cur string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(cur)
	if err != nil || !isWithinRoot(realTarget, resolved) {
		return fmt.Errorf("target symbolic link %s escapes target_dir", cur)
	}
	return fmt.Errorf("refusing to overwrite symbolic link %s", cur)
}

func isWithinRoot(realRoot, candidate string) bool {
	rel, err := filepath.Rel(realRoot, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func sha256File(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := file.Read(buf)
		if n > 0 {
			_, _ = hasher.Write(buf[:n])
		}
		if errors.Is(readErr, io.EOF) {
			return hex.EncodeToString(hasher.Sum(nil)), nil
		}
		if readErr != nil {
			return "", fmt.Errorf("hash %s: %w", path, readErr)
		}
	}
}
