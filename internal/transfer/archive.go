package transfer

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func WriteTar(ctx context.Context, w io.Writer, plan Plan) error {
	tw := tar.NewWriter(w)
	now := time.Now().UTC()
	for _, dir := range plan.Dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     dir + "/",
			Typeflag: tar.TypeDir,
			Mode:     0o755,
			ModTime:  now,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write tar dir %s: %w", dir, err)
		}
	}
	for _, file := range plan.Files {
		if err := writeTarFile(ctx, tw, file, now); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}
	return nil
}

func writeTarFile(ctx context.Context, tw *tar.Writer, file SourceFile, modTime time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := os.Open(file.AbsPath)
	if err != nil {
		return fmt.Errorf("open source %s: %w", file.RelPath, err)
	}
	defer func() { _ = src.Close() }()
	info, statErr := src.Stat()
	if statErr != nil {
		return fmt.Errorf("stat open source %s: %w", file.RelPath, statErr)
	}
	if !info.Mode().IsRegular() || info.Size() != file.Size {
		return fmt.Errorf("source %s changed during transfer", file.RelPath)
	}
	mode := int64(0o644)
	if file.Executable {
		mode = 0o755
	}
	hdr := &tar.Header{
		Name:     file.RelPath,
		Typeflag: tar.TypeReg,
		Mode:     mode,
		Size:     file.Size,
		ModTime:  modTime,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header %s: %w", file.RelPath, err)
	}
	return streamExactSource(ctx, tw, src, file)
}

func streamExactSource(ctx context.Context, tw *tar.Writer, src *os.File, file SourceFile) error {
	if err := copyWithContext(ctx, tw, io.LimitReader(src, file.Size), file.Size); err != nil {
		return fmt.Errorf("stream source %s: %w", file.RelPath, err)
	}
	var extra [1]byte
	if n, _ := src.Read(extra[:]); n > 0 {
		return fmt.Errorf("source %s grew during transfer", file.RelPath)
	}
	return nil
}

type targetRoot struct {
	clean string
	real  string
}

func ExtractTar(ctx context.Context, r io.Reader, targetDir string) error {
	root, err := prepareTargetRoot(targetDir)
	if err != nil {
		return err
	}
	capped := &streamCapReader{r: r, limit: MaxRawTarStreamBytes}
	tr := tar.NewReader(capped)
	remaining := MaxTransferBytes
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			return nil
		}
		if nextErr != nil {
			return fmt.Errorf("read tar stream: %w", nextErr)
		}
		if err := extractBoundedEntry(ctx, tr, extractContext{root: root, remaining: &remaining}, hdr); err != nil {
			return err
		}
	}
}

type streamCapReader struct {
	r     io.Reader
	read  int64
	limit int64
}

func (c *streamCapReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.limit {
		return n, errors.New("transfer stream exceeds 500MB limit")
	}
	return n, err
}

type extractContext struct {
	root      targetRoot
	remaining *int64
}

func extractBoundedEntry(ctx context.Context, tr *tar.Reader, ec extractContext, hdr *tar.Header) error {
	relName := strings.TrimRight(hdr.Name, "/")
	if err := ValidateRelPath(relName); err != nil {
		return err
	}
	if err := verifyTargetSegments(ec.root.real, ec.root.clean, relName); err != nil {
		return err
	}
	dest := filepath.Join(ec.root.clean, filepath.FromSlash(relName))
	switch hdr.Typeflag {
	case tar.TypeDir:
		return ensureSafeSubdir(ec.root, relName, dest)
	case tar.TypeReg:
		return extractRegularFile(ctx, tr, ec, hdr)
	default:
		return fmt.Errorf("unsupported tar entry type %d for %s", hdr.Typeflag, relName)
	}
}

func prepareTargetRoot(targetDir string) (targetRoot, error) {
	cleanTarget, realTarget, exists, err := inspectTargetRoot(targetDir)
	if err != nil {
		return targetRoot{}, err
	}
	if exists {
		return targetRoot{clean: cleanTarget, real: realTarget}, nil
	}
	if err := os.MkdirAll(cleanTarget, 0o755); err != nil {
		return targetRoot{}, fmt.Errorf("create target_dir %s: %w", targetDir, err)
	}
	resolved, evalErr := filepath.EvalSymlinks(cleanTarget)
	if evalErr != nil {
		return targetRoot{}, fmt.Errorf("resolve target_dir %s: %w", targetDir, evalErr)
	}
	return targetRoot{clean: cleanTarget, real: resolved}, nil
}

func ensureSafeSubdir(root targetRoot, relName, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", relName, err)
	}
	realDest, err := filepath.EvalSymlinks(dest)
	if err != nil || !isWithinRoot(root.real, realDest) {
		return fmt.Errorf("directory %s escapes target_dir", relName)
	}
	return nil
}

func extractRegularFile(ctx context.Context, tr *tar.Reader, ec extractContext, hdr *tar.Header) error {
	if hdr.Size < 0 {
		return fmt.Errorf("negative tar entry size %d for %s", hdr.Size, hdr.Name)
	}
	if hdr.Size > *ec.remaining {
		return fmt.Errorf("tar payload for %s exceeds 500MB limit", hdr.Name)
	}
	*ec.remaining -= hdr.Size
	relName := strings.TrimRight(hdr.Name, "/")
	dest := filepath.Join(ec.root.clean, filepath.FromSlash(relName))
	parentRel := path.Dir(relName)
	if parentRel != "." {
		if err := ensureSafeSubdir(ec.root, parentRel, filepath.Dir(dest)); err != nil {
			return err
		}
	}
	return writeAtomicFile(ctx, tr, dest, hdr)
}

func writeAtomicFile(ctx context.Context, r io.Reader, dest string, hdr *tar.Header) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(dest), ".dl-cp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", dest, err)
	}
	tmpPath := tmpFile.Name()
	committed := false
	defer func() {
		_ = tmpFile.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := copyWithContext(ctx, tmpFile, r, hdr.Size); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	perm := os.FileMode(0o644)
	if hdr.Mode&0o111 != 0 {
		perm = 0o755
	}
	if err := tmpFile.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", dest, err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dest, err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("commit %s: %w", dest, err)
	}
	committed = true
	return nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader, expected int64) error {
	buf := make([]byte, 32*1024)
	var written int64
	for written < expected {
		if err := ctx.Err(); err != nil {
			return err
		}
		step := min(int64(len(buf)), expected-written)
		n, readErr := src.Read(buf[:step])
		if err := writeFullChunk(dst, buf[:n], &written); err != nil {
			return err
		}
		if readErr != nil {
			return classifyCopyEOF(readErr, written, expected)
		}
	}
	return nil
}

func writeFullChunk(dst io.Writer, chunk []byte, written *int64) error {
	if len(chunk) == 0 {
		return nil
	}
	w, err := dst.Write(chunk)
	*written += int64(w)
	if err != nil {
		return err
	}
	if w != len(chunk) {
		return io.ErrShortWrite
	}
	return nil
}

func classifyCopyEOF(readErr error, written, expected int64) error {
	if errors.Is(readErr, io.EOF) {
		if written == expected {
			return nil
		}
		return io.ErrUnexpectedEOF
	}
	return readErr
}
