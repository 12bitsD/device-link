package transfer

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTarRejectsCumulativeOver500MB(t *testing.T) {
	pipeR, pipeW := io.Pipe()
	writerDone := make(chan struct{})
	const chunkFileBytes = 300 * 1024 * 1024
	go func() {
		defer close(writerDone)
		tw := tar.NewWriter(pipeW)
		_ = tw.WriteHeader(&tar.Header{Name: "part1.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: chunkFileBytes})
		_, _ = io.CopyN(tw, zeroReader{}, chunkFileBytes)
		_ = tw.WriteHeader(&tar.Header{Name: "part2.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: chunkFileBytes})
		_, _ = io.CopyN(tw, zeroReader{}, 1024)
		_ = pipeW.Close()
	}()
	defer func() {
		_ = pipeR.Close()
		<-writerDone
	}()

	targetDir := t.TempDir()
	err := ExtractTar(context.Background(), pipeR, targetDir)
	if err == nil || !strings.Contains(err.Error(), "exceeds 500MB limit") {
		t.Fatalf("expected cumulative >500MB rejection, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(targetDir, "part2.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("part2.bin should not be committed: %v", statErr)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestInspectTargetRejectsFileDirCollision(t *testing.T) {
	targetDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(targetDir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := InspectTarget(context.Background(), targetDir, []Candidate{{RelPath: "app", Size: 5}})
	if err == nil {
		t.Fatal("expected pre-flight collision error when target leaf is an existing directory")
	}
}
