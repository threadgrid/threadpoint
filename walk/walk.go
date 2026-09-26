// SPDX-License-Identifier: Apache-2.0

// Package walk walks directory trees with bounds: it is symlink-aware,
// caps the number of files visited, and reads only size-limited small text
// files, so scanning untrusted workspaces stays safe and predictable.
package walk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/threadgrid/threadpoint/internal/abspath"
)

// DefaultMaxWalkFiles through DefaultMaxFileSize bound untrusted directory
// scans when callers do not provide stricter limits.
const (
	DefaultMaxWalkFiles     = 500
	DefaultMaxWalkEntries   = 500
	DefaultMaxWalkDepth     = 64
	DefaultMaxWalkPathBytes = 1 << 20
	DefaultMaxFileSize      = 1024 * 1024
	walkReadBatchSize       = 128
)

var errStopWalk = errors.New("stop traversal")

var defaultSkipDirNames = []string{
	".angular",
	".astro",
	".aws-sam",
	".bundle",
	".bzr",
	".cache",
	".dart_tool",
	".docusaurus",
	".eggs",
	".fleet",
	".fossil",
	".git",
	".gradle",
	".hg",
	".hypothesis",
	".idea",
	".ipynb_checkpoints",
	".jj",
	".m2",
	".mypy_cache",
	".next",
	".nox",
	".npm",
	".nuxt",
	".output",
	".parcel-cache",
	".pijul",
	".pnpm-store",
	".pytest_cache",
	".ruff_cache",
	".serverless",
	".stack-work",
	".svelte-kit",
	".svn",
	".swiftpm",
	".terraform",
	".terragrunt-cache",
	".tox",
	".turbo",
	".venv",
	".vite",
	".vs",
	".vscode",
	".webpack",
	".yarn",
	"__pycache__",
	"bower_components",
	"build",
	"carthage",
	"coverage",
	"deriveddata",
	"dist",
	"node_modules",
	"out",
	"pods",
	"target",
	"vendor",
	"venv",
}

var defaultSkipDirSet = func() map[string]struct{} {
	values := make(map[string]struct{}, len(defaultSkipDirNames))
	for _, name := range defaultSkipDirNames {
		values[name] = struct{}{}
	}
	return values
}()

// Options configures a bounded workspace walk.
type Options struct {
	Root         string
	Start        string
	MaxFiles     int
	MaxEntries   int
	MaxDepth     int
	MaxPathBytes int
	SkipDirNames []string
}

// VisitFunc is called for each visited path with its root-relative path.
type VisitFunc func(path string, entry fs.DirEntry, rel string) error

// Dir walks opts.Start under opts.Root with threadpoint's safety bounds.
func Dir(ctx context.Context, opts Options, visit VisitFunc) ([]string, error) {
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultMaxWalkFiles
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxWalkEntries
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxWalkDepth
	}
	if opts.MaxPathBytes <= 0 {
		opts.MaxPathBytes = DefaultMaxWalkPathBytes
	}
	var err error
	opts.SkipDirNames, err = NormalizeSkipDirNames(opts.SkipDirNames)
	if err != nil {
		return nil, err
	}
	root, err := abspath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	start := opts.Start
	if start == "" {
		start = root
	}
	start, err = abspath.Abs(start)
	if err != nil {
		return nil, err
	}
	startInfo, err := os.Lstat(start)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return []string{fmt.Sprintf("skipped %s: %v", displayRel(root, start), err)}, nil
	}
	startRel, err := filepath.Rel(root, start)
	if err != nil || startRel == ".." || strings.HasPrefix(startRel, ".."+string(filepath.Separator)) {
		return nil, errors.Join(errors.New("walk start must remain beneath its selected root"), err)
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rootHandle.Close()
	startRoot := rootHandle
	if startRel != "." && startInfo.IsDir() {
		startRoot, err = rootHandle.OpenRoot(startRel)
		if err != nil {
			if startInfo.Mode()&fs.ModeSymlink != 0 {
				return []string{fmt.Sprintf("skipped %s: symlinks are not followed", displayRel(root, start))}, nil
			}
			return nil, err
		}
		defer startRoot.Close()
		opened, openErr := startRoot.Stat(".")
		if openErr != nil || !startInfo.IsDir() || !os.SameFile(startInfo, opened) {
			return nil, errors.Join(errors.New("walk start changed while it was pinned"), openErr)
		}
	}

	var warnings []string
	walkedFiles := 0
	walkedEntries := 0
	walkedPathBytes := 0
	visitEntry := func(path string, entry fs.DirEntry) error {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		rel := displayRel(root, path)
		if path != start && entry.Type()&fs.ModeSymlink != 0 {
			warnings = append(warnings, fmt.Sprintf("skipped %s: symlinks are not followed", rel))
			return filepath.SkipDir
		}
		if path != start && ShouldSkipEntry(entry.Name()) {
			warnings = append(warnings, fmt.Sprintf("skipped %s: ignored tool-managed entry", rel))
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path != start && entry.IsDir() && ShouldSkipDirWithNames(entry.Name(), opts.SkipDirNames) {
			reason := "ignored high-volume directory"
			if !ShouldSkipDir(entry.Name()) {
				reason = "ignored configured directory"
			}
			warnings = append(warnings, fmt.Sprintf("skipped %s: %s", rel, reason))
			return filepath.SkipDir
		}
		if !entry.IsDir() {
			walkedFiles++
			if walkedFiles > opts.MaxFiles {
				warnings = append(warnings, fmt.Sprintf("stopped scanning %s after %d files", displayRel(root, start), opts.MaxFiles))
				return errStopWalk
			}
		}
		if visit == nil {
			return nil
		}
		return visit(path, entry, rel)
	}
	startVisitErr := visitEntry(start, fs.FileInfoToDirEntry(startInfo))
	if startVisitErr != nil && !errors.Is(startVisitErr, filepath.SkipDir) {
		if errors.Is(startVisitErr, errStopWalk) {
			return warnings, nil
		}
		return warnings, startVisitErr
	}
	if !startInfo.IsDir() || startInfo.Mode()&fs.ModeSymlink != 0 || errors.Is(startVisitErr, filepath.SkipDir) {
		return warnings, nil
	}
	var walkDirectory func(*os.Root, string, int) error
	walkDirectory = func(directoryRoot *os.Root, logicalDir string, depth int) (returnErr error) {
		directory, err := directoryRoot.Open(".")
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped %s: %v", displayRel(root, logicalDir), err))
			return nil
		}
		defer func() { returnErr = errors.Join(returnErr, directory.Close()) }()
		remainingEntries := opts.MaxEntries - walkedEntries
		entries, overflow, readErr := readDirectoryEntriesBounded(directory, remainingEntries)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("skipped %s: %v", displayRel(root, logicalDir), readErr))
			return nil
		}
		if overflow {
			warnings = append(warnings, fmt.Sprintf("stopped scanning %s: directory %s exceeds the remaining %d-entry budget", displayRel(root, start), displayRel(root, logicalDir), remainingEntries))
			return errStopWalk
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, listed := range entries {
			name := listed.Name()
			path := filepath.Join(logicalDir, name)
			rel := displayRel(root, path)
			walkedEntries++
			walkedPathBytes += len(rel)
			if walkedEntries > opts.MaxEntries || walkedPathBytes > opts.MaxPathBytes {
				warnings = append(warnings, fmt.Sprintf("stopped scanning %s after %d entries or %d path bytes", displayRel(root, start), opts.MaxEntries, opts.MaxPathBytes))
				return errStopWalk
			}
			info, statErr := directoryRoot.Lstat(name)
			if statErr != nil {
				warnings = append(warnings, fmt.Sprintf("skipped %s: %v", rel, statErr))
				continue
			}
			entry := fs.FileInfoToDirEntry(info)
			if depth+1 > opts.MaxDepth && entry.IsDir() {
				warnings = append(warnings, fmt.Sprintf("skipped %s: traversal depth exceeds %d", rel, opts.MaxDepth))
				continue
			}
			visitErr := visitEntry(path, entry)
			if errors.Is(visitErr, errStopWalk) {
				return visitErr
			}
			if visitErr != nil && !errors.Is(visitErr, filepath.SkipDir) {
				return visitErr
			}
			if !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || errors.Is(visitErr, filepath.SkipDir) {
				continue
			}
			child, openErr := directoryRoot.OpenRoot(name)
			if openErr != nil {
				warnings = append(warnings, fmt.Sprintf("skipped %s: %v", rel, openErr))
				continue
			}
			opened, openErr := child.Stat(".")
			if openErr != nil || !opened.IsDir() || !os.SameFile(info, opened) {
				warnings = append(warnings, fmt.Sprintf("skipped %s: directory changed while it was pinned", rel))
				_ = child.Close()
				continue
			}
			childErr := walkDirectory(child, path, depth+1)
			closeErr := child.Close()
			if childErr != nil || closeErr != nil {
				return errors.Join(childErr, closeErr)
			}
		}
		return nil
	}
	err = walkDirectory(startRoot, start, 0)
	if errors.Is(err, errStopWalk) {
		err = nil
	}
	return warnings, err
}

func readDirectoryEntriesBounded(directory *os.File, maxEntries int) ([]fs.DirEntry, bool, error) {
	if directory == nil || maxEntries < 0 {
		return nil, false, errors.New("bounded directory read requires a file and non-negative limit")
	}
	entries := make([]fs.DirEntry, 0, min(maxEntries, walkReadBatchSize))
	for {
		remainingWithSentinel := maxEntries + 1 - len(entries)
		if remainingWithSentinel <= 0 {
			return entries, true, nil
		}
		batchSize := min(walkReadBatchSize, remainingWithSentinel)
		batch, err := directory.ReadDir(batchSize)
		entries = append(entries, batch...)
		if len(entries) > maxEntries {
			return entries[:maxEntries], true, nil
		}
		if errors.Is(err, io.EOF) {
			return entries, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
}

// DefaultSkipDirNames returns a copy of the directory basenames skipped by
// default because they are source-control metadata, dependencies, generated
// output, caches, infrastructure state, or IDE metadata.
func DefaultSkipDirNames() []string {
	return append([]string(nil), defaultSkipDirNames...)
}

// NormalizeSkipDirNames validates, case-normalizes, deduplicates, and sorts
// additional directory basenames for a traversal policy.
func NormalizeSkipDirNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(names))
	for _, raw := range names {
		name, err := normalizeSkipDirName(raw)
		if err != nil {
			return nil, err
		}
		seen[name] = struct{}{}
	}
	normalized := make([]string, 0, len(seen))
	for name := range seen {
		normalized = append(normalized, name)
	}
	sort.Strings(normalized)
	return normalized, nil
}

// ShouldSkipEntry reports whether name is a threadpoint-managed transient or
// recovery entry. It applies to both files and directories because an
// identity-raced no-replace capability probe can be retained fail-closed.
func ShouldSkipEntry(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(name, ".threadpoint-restore-recovery-") ||
		strings.HasPrefix(name, ".threadpoint-prune-recovery-") ||
		strings.HasPrefix(name, ".threadpoint-restore-file-") ||
		strings.HasPrefix(name, ".threadpoint-restore-directory-") ||
		strings.HasPrefix(name, ".threadpoint-noreplace-probe-source-") ||
		strings.HasPrefix(name, ".threadpoint-noreplace-probe-destination-")
}

// ShouldSkipDir reports whether name is a high-volume or tool-managed directory.
func ShouldSkipDir(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if ShouldSkipEntry(name) {
		return true
	}
	_, ok := defaultSkipDirSet[name]
	return ok
}

// ShouldSkipDirWithNames reports whether name is skipped by the default policy,
// a managed entry rule, or additional normalized directory basenames.
func ShouldSkipDirWithNames(name string, additional []string) bool {
	if ShouldSkipDir(name) {
		return true
	}
	name = strings.ToLower(strings.TrimSpace(name))
	for _, candidate := range additional {
		if name == candidate {
			return true
		}
	}
	return false
}

func normalizeSkipDirName(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		return "", errors.New("skip directory name cannot be empty")
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("skip directory name %q is not a basename", raw)
	}
	if strings.ContainsAny(name, "/\\*?[]{}") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("skip directory name %q must be a plain basename", raw)
	}
	return name, nil
}

// ReadSmallTextFile reads path when it is a small regular UTF-8 text file.
func ReadSmallTextFile(path string, maxSize int) ([]byte, string, bool, error) {
	return readSmallTextFile(path, maxSize, nil)
}

func readSmallTextFile(path string, maxSize int, afterLstat func()) ([]byte, string, bool, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxFileSize
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Sprintf("could not read %s: %v", path, err), false, nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Sprintf("skipped %s: symlinks are not followed", path), false, nil
	}
	if info.IsDir() {
		return nil, fmt.Sprintf("skipped %s: directories are not text files", path), false, nil
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Sprintf("skipped %s: non-regular files are not text files", path), false, nil
	}
	if info.Size() > int64(maxSize) {
		return nil, fmt.Sprintf("skipped %s larger than %d bytes", path, maxSize), false, nil
	}
	if afterLstat != nil {
		afterLstat()
	}
	file, err := openSmallRegularFile(path)
	if err != nil {
		return nil, fmt.Sprintf("could not read %s: %v", path, err), false, nil
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, fmt.Sprintf("skipped %s: file changed before it could be read", path), false, nil
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(maxSize)+1))
	if err != nil {
		return nil, fmt.Sprintf("could not read %s: %v", path, err), false, nil
	}
	if len(body) > maxSize {
		return nil, fmt.Sprintf("skipped %s larger than %d bytes", path, maxSize), false, nil
	}
	after, err := file.Stat()
	if err != nil {
		return nil, fmt.Sprintf("could not verify %s: %v", path, err), false, nil
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, after) || !os.SameFile(opened, current) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || int64(len(body)) != after.Size() {
		return nil, fmt.Sprintf("skipped %s: file changed while it was being read", path), false, nil
	}
	if bytes.IndexByte(body, 0) >= 0 || !utf8.Valid(body) {
		return nil, fmt.Sprintf("skipped %s: binary-like content", path), false, nil
	}
	return body, "", true, nil
}

func displayRel(root string, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return filepath.ToSlash(filepath.Clean(path))
	}
	return filepath.ToSlash(rel)
}
