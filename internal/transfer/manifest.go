package transfer

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	MaxTransferBytes     int64 = 500 * 1024 * 1024
	MaxRawTarStreamBytes int64 = MaxTransferBytes + (64 << 20)
)

type Candidate struct {
	RelPath string `json:"rel_path"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir,omitempty"`
}

type SourceFile struct {
	AbsPath    string
	RelPath    string
	Size       int64
	Executable bool
}

type ScanResult struct {
	Dirs       []string
	Files      []SourceFile
	TotalBytes int64
}

type Plan struct {
	Dirs          []string
	Files         []SourceFile
	SkippedFiles  int
	TransferBytes int64
	TotalBytes    int64
}

func (r ScanResult) Candidates() []Candidate {
	out := make([]Candidate, 0, len(r.Dirs)+len(r.Files))
	for _, dir := range r.Dirs {
		out = append(out, Candidate{RelPath: dir, IsDir: true})
	}
	for _, file := range r.Files {
		out = append(out, Candidate{RelPath: file.RelPath, Size: file.Size})
	}
	return out
}

type sourceScanner struct {
	excludes  []string
	seenBases map[string]struct{}
	res       ScanResult
}

func ScanSources(ctx context.Context, sources []string, excludes []string) (ScanResult, error) {
	if len(sources) == 0 {
		return ScanResult{}, errors.New("at least one source path is required")
	}
	scanner := &sourceScanner{
		excludes:  excludes,
		seenBases: make(map[string]struct{}, len(sources)),
	}
	for _, raw := range sources {
		if err := scanner.scanOne(ctx, raw); err != nil {
			return ScanResult{}, err
		}
	}
	if scanner.res.TotalBytes > MaxTransferBytes {
		return ScanResult{}, oversizedError(scanner.res)
	}
	return scanner.res, nil
}

func (sc *sourceScanner) scanOne(ctx context.Context, raw string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(raw) == "" || hasDotDotSegment(raw) {
		return fmt.Errorf("invalid source path %q", raw)
	}
	clean := filepath.Clean(raw)
	base := filepath.Base(clean)
	if base == "" || base == "." || base == ".." || strings.ContainsAny(base, `/\`) {
		return fmt.Errorf("invalid source path %q", raw)
	}
	if _, exists := sc.seenBases[base]; exists {
		return fmt.Errorf("duplicate source base name %q", base)
	}
	sc.seenBases[base] = struct{}{}
	return sc.statAndCollect(ctx, clean, base)
}

func (sc *sourceScanner) statAndCollect(ctx context.Context, clean, base string) error {
	info, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("stat source %s: %w", clean, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source %s is a symbolic link escaping source root", clean)
	}
	if MatchExclude(base, "", sc.excludes) {
		return nil
	}
	if info.Mode().IsRegular() {
		sc.addFile(clean, base, info)
		return nil
	}
	if info.IsDir() {
		return sc.walkDir(ctx, clean, base)
	}
	return fmt.Errorf("unsupported file mode for %s", clean)
}

func (sc *sourceScanner) addFile(absPath, relPath string, info fs.FileInfo) {
	sc.res.Files = append(sc.res.Files, SourceFile{
		AbsPath:    absPath,
		RelPath:    relPath,
		Size:       info.Size(),
		Executable: info.Mode().Perm()&0o111 != 0,
	})
	sc.res.TotalBytes += info.Size()
}

func (sc *sourceScanner) walkDir(ctx context.Context, rootDir, base string) error {
	return filepath.WalkDir(rootDir, func(cur string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return sc.visitEntry(rootDir, base, cur, entry)
	})
}

func (sc *sourceScanner) visitEntry(rootDir, base, cur string, entry fs.DirEntry) error {
	relSub, err := filepath.Rel(rootDir, cur)
	if err != nil {
		return fmt.Errorf("relative source path: %w", err)
	}
	subSlash := filepath.ToSlash(relSub)
	if subSlash == "." {
		sc.res.Dirs = append(sc.res.Dirs, base)
		return nil
	}
	relSlash := base + "/" + subSlash
	if MatchExclude(relSlash, subSlash, sc.excludes) {
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if entry.Type()&os.ModeSymlink != 0 {
		return fmt.Errorf("symbolic link %s is not permitted in transfer", relSlash)
	}
	if entry.IsDir() {
		sc.res.Dirs = append(sc.res.Dirs, relSlash)
		return nil
	}
	if !entry.Type().IsRegular() {
		return fmt.Errorf("unsupported source entry %s", relSlash)
	}
	info, infoErr := entry.Info()
	if infoErr != nil {
		return fmt.Errorf("stat %s: %w", relSlash, infoErr)
	}
	sc.addFile(cur, relSlash, info)
	return nil
}

func oversizedError(res ScanResult) error {
	sorted := slices.Clone(res.Files)
	slices.SortFunc(sorted, func(a, b SourceFile) int {
		return cmp.Compare(b.Size, a.Size)
	})
	limit := min(len(sorted), 5)
	parts := make([]string, 0, limit)
	for i := range limit {
		parts = append(parts, fmt.Sprintf("%s (%d bytes)", sorted[i].RelPath, sorted[i].Size))
	}
	return fmt.Errorf("transfer size %d bytes exceeds 500MB limit (%d bytes); largest files: %s",
		res.TotalBytes, MaxTransferBytes, strings.Join(parts, ", "))
}

func MatchExclude(relSlash, subSlash string, excludes []string) bool {
	for _, rawPattern := range excludes {
		pattern := strings.TrimSpace(strings.ReplaceAll(rawPattern, `\`, "/"))
		if pattern != "" && matchOnePattern(pattern, relSlash, subSlash) {
			return true
		}
	}
	return false
}

func matchOnePattern(pattern, relSlash, subSlash string) bool {
	base := path.Base(relSlash)
	if ok, err := path.Match(pattern, base); err == nil && ok {
		return true
	}
	if relSlash != base {
		if ok, err := path.Match(pattern, relSlash); err == nil && ok {
			return true
		}
	}
	if subSlash != "" && subSlash != "." {
		if ok, err := path.Match(pattern, subSlash); err == nil && ok {
			return true
		}
	}
	return false
}

func InspectTarget(ctx context.Context, targetDir string, candidates []Candidate) (map[string]string, error) {
	cleanTarget, realTarget, exists, err := inspectTargetRoot(targetDir)
	if err != nil {
		return nil, err
	}
	hashes := make(map[string]string)
	seen := make(map[string]struct{}, len(candidates))
	for _, cand := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := ValidateRelPath(cand.RelPath); err != nil {
			return nil, err
		}
		if _, dup := seen[cand.RelPath]; dup || !exists {
			continue
		}
		seen[cand.RelPath] = struct{}{}
		digest, ok, checkErr := inspectCandidateTarget(ctx, targetRoot{clean: cleanTarget, real: realTarget}, cand)
		if checkErr != nil {
			return nil, checkErr
		}
		if ok {
			hashes[cand.RelPath] = digest
		}
	}
	return hashes, nil
}

func FinalizePlan(ctx context.Context, scan ScanResult, existingHashes map[string]string) (Plan, error) {
	plan := Plan{
		Dirs:       scan.Dirs,
		TotalBytes: scan.TotalBytes,
	}
	for _, file := range scan.Files {
		skipped, err := shouldSkipFile(ctx, file, existingHashes)
		if err != nil {
			return Plan{}, err
		}
		if skipped {
			plan.SkippedFiles++
			continue
		}
		plan.Files = append(plan.Files, file)
		plan.TransferBytes += file.Size
	}
	return plan, nil
}

func shouldSkipFile(ctx context.Context, file SourceFile, existingHashes map[string]string) (bool, error) {
	targetDigest, ok := existingHashes[file.RelPath]
	if !ok {
		return false, nil
	}
	srcDigest, err := sha256File(ctx, file.AbsPath)
	if err != nil {
		return false, err
	}
	return srcDigest == targetDigest, nil
}
