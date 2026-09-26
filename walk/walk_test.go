// SPDX-License-Identifier: Apache-2.0

package walk

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWalkDirSkipsSymlinksIgnoredDirsAndStopsAtLimit(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "one.md"), "one")
	mustWrite(t, filepath.Join(root, ".agents", "rules", "two.md"), "two")
	mustWrite(t, filepath.Join(root, ".agents", "node_modules", "ignored.md"), "ignored")
	mustWrite(t, filepath.Join(root, "outside.md"), "outside")
	if err := os.Symlink(filepath.Join(root, "outside.md"), filepath.Join(root, ".agents", "knowledge", "outside.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	var visited []string
	warnings, err := Dir(context.Background(), Options{Root: root, Start: filepath.Join(root, ".agents"), MaxFiles: 1}, func(_ string, entry os.DirEntry, rel string) error {
		if !entry.IsDir() {
			visited = append(visited, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(visited) != 1 || visited[0] != ".agents/knowledge/one.md" {
		t.Fatalf("unexpected visited files: %#v", visited)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"node_modules", "symlinks are not followed", "stopped scanning .agents after 1 files"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected warning %q in %#v", want, warnings)
		}
	}
}

func TestShouldSkipEntryIncludesManagedRecoveryAndRestoreTemps(t *testing.T) {
	for _, name := range []string{
		".threadpoint-prune-recovery-deadbeef",
		".threadpoint-restore-file-deadbeef",
		".threadpoint-restore-directory-deadbeef",
	} {
		if !ShouldSkipEntry(name) {
			t.Fatalf("managed entry %q must not be rediscovered as provider content", name)
		}
	}
	if ShouldSkipEntry("threadpoint-prune-recovery-user-file") {
		t.Fatal("ordinary user files must not match the managed recovery prefix")
	}
}

func TestDirectorySkipPolicyIncludesExpandedDefaultsAndConfiguredNames(t *testing.T) {
	wantDefaults := ".angular,.astro,.aws-sam,.bundle,.bzr,.cache,.dart_tool,.docusaurus,.eggs,.fleet,.fossil,.git,.gradle,.hg,.hypothesis,.idea,.ipynb_checkpoints,.jj,.m2,.mypy_cache,.next,.nox,.npm,.nuxt,.output,.parcel-cache,.pijul,.pnpm-store,.pytest_cache,.ruff_cache,.serverless,.stack-work,.svelte-kit,.svn,.swiftpm,.terraform,.terragrunt-cache,.tox,.turbo,.venv,.vite,.vs,.vscode,.webpack,.yarn,__pycache__,bower_components,build,carthage,coverage,deriveddata,dist,node_modules,out,pods,target,vendor,venv"
	if got := strings.Join(DefaultSkipDirNames(), ","); got != wantDefaults {
		t.Fatalf("default directory policy = %q, want %q", got, wantDefaults)
	}
	for _, name := range DefaultSkipDirNames() {
		if !ShouldSkipDir(name) {
			t.Fatalf("default directory %q must be skipped", name)
		}
	}
	if ShouldSkipDir("team-artifacts") {
		t.Fatal("ordinary directory must not be skipped by default")
	}
	if !ShouldSkipDirWithNames("TEAM-ARTIFACTS", []string{"team-artifacts"}) {
		t.Fatal("configured directory must be skipped case-insensitively")
	}
	if !ShouldSkipDirWithNames(".threadpoint-prune-recovery-deadbeef", nil) {
		t.Fatal("managed recovery entries must remain skipped")
	}
}

func TestNormalizeSkipDirNamesRejectsPathsAndGlobs(t *testing.T) {
	normalized, err := NormalizeSkipDirNames([]string{" Generated ", "generated", "cache"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(normalized, ","), "cache,generated"; got != want {
		t.Fatalf("normalized names = %q, want %q", got, want)
	}
	for _, raw := range []string{"", ".", "..", "nested/cache", `nested\\cache`, "cache*"} {
		if _, err := NormalizeSkipDirNames([]string{raw}); err == nil {
			t.Fatalf("expected invalid directory name %q to fail", raw)
		}
	}
}

func TestWalkDirSkipsConfiguredDirectory(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "keep.md"), "keep")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "generated", "ignored.md"), "ignored")

	var visited []string
	warnings, err := Dir(context.Background(), Options{
		Root:         root,
		Start:        filepath.Join(root, ".agents"),
		SkipDirNames: []string{" Generated "},
	}, func(_ string, entry os.DirEntry, rel string) error {
		if !entry.IsDir() {
			visited = append(visited, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(visited, ","), ".agents/knowledge/keep.md"; got != want {
		t.Fatalf("visited = %q, want %q", got, want)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "ignored configured directory") {
		t.Fatalf("configured skip warning missing: %#v", warnings)
	}
}

func TestWalkDirChargesDirectoriesAndAggregatePathsAgainstBounds(t *testing.T) {
	root := t.TempDir()
	start := filepath.Join(root, ".agents")
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		if err := os.MkdirAll(filepath.Join(start, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	warnings, err := Dir(context.Background(), Options{
		Root: root, Start: start, MaxEntries: 3, MaxPathBytes: 1 << 20,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "remaining 3-entry budget") {
		t.Fatalf("directory budget warning = %#v", warnings)
	}

	deep := filepath.Join(start, "deep", "nested", "too-deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	warnings, err = Dir(context.Background(), Options{
		Root: root, Start: filepath.Join(start, "deep"), MaxEntries: 20, MaxDepth: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "traversal depth exceeds 1") {
		t.Fatalf("depth budget warning = %#v", warnings)
	}
}

func TestWalkDirSortsCompleteBoundedDirectoryBeforeVisiting(t *testing.T) {
	root := t.TempDir()
	start := filepath.Join(root, ".agents")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 149; index >= 0; index-- {
		name := fmt.Sprintf("%03d.md", index)
		mustWrite(t, filepath.Join(start, name), name)
	}
	var visited []string
	_, err := Dir(context.Background(), Options{
		Root: root, Start: start, MaxEntries: 200,
	}, func(_ string, entry fs.DirEntry, rel string) error {
		if !entry.IsDir() {
			visited = append(visited, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(visited) != 150 || visited[0] != ".agents/000.md" || visited[len(visited)-1] != ".agents/149.md" {
		t.Fatalf("bounded lexical traversal endpoints = %#v ... %#v", visited[:min(3, len(visited))], visited[max(0, len(visited)-3):])
	}
}

func TestReadSmallTextFileRejectsLargeBinaryAndSymlink(t *testing.T) {
	root := t.TempDir()
	large := filepath.Join(root, "large.md")
	binary := filepath.Join(root, "binary.md")
	target := filepath.Join(root, "target.md")
	link := filepath.Join(root, "link.md")
	mustWrite(t, large, "0123456789")
	mustWrite(t, binary, "abc\x00def")
	mustWrite(t, target, "target")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, warning, ok, err := ReadSmallTextFile(large, 4); err != nil || ok || !strings.Contains(warning, "larger than 4 bytes") {
		t.Fatalf("expected large-file warning, ok=%v warning=%q err=%v", ok, warning, err)
	}
	if _, warning, ok, err := ReadSmallTextFile(binary, DefaultMaxFileSize); err != nil || ok || !strings.Contains(warning, "binary-like") {
		t.Fatalf("expected binary warning, ok=%v warning=%q err=%v", ok, warning, err)
	}
	if _, warning, ok, err := ReadSmallTextFile(link, DefaultMaxFileSize); err != nil || ok || !strings.Contains(warning, "symlinks are not followed") {
		t.Fatalf("expected symlink warning, ok=%v warning=%q err=%v", ok, warning, err)
	}
}

func TestReadSmallTextFileRejectsGrowthAfterLstat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.md")
	mustWrite(t, path, "tiny")

	_, warning, ok, err := readSmallTextFile(path, 4, func() {
		mustWrite(t, path, "grew beyond the limit")
	})
	if err != nil || ok || !strings.Contains(warning, "larger than 4 bytes") {
		t.Fatalf("expected raced-growth warning, ok=%v warning=%q err=%v", ok, warning, err)
	}
}

func TestWalkDirPropagatesVisitorAndContextErrorsAndHandlesMissingStarts(t *testing.T) {
	root := t.TempDir()
	if warnings, err := Dir(context.Background(), Options{Root: root, Start: filepath.Join(root, "missing")}, nil); err != nil || warnings != nil {
		t.Fatalf("missing start = %#v, err=%v", warnings, err)
	}
	file := filepath.Join(root, "file.md")
	if err := os.WriteFile(file, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	visitorErr := errors.New("visitor stopped")
	if _, err := Dir(context.Background(), Options{Root: root}, func(path string, entry fs.DirEntry, _ string) error {
		if path == file && !entry.IsDir() {
			return visitorErr
		}
		return nil
	}); !errors.Is(err, visitorErr) {
		t.Fatalf("visitor error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Dir(canceled, Options{Root: root}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled walk error = %v", err)
	}
}

func TestReadDirectoryEntriesRejectsClosedDirectory(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readDirectoryEntriesBounded(directory, 1); err == nil || !strings.Contains(err.Error(), "closed file") {
		t.Fatalf("closed directory = %v", err)
	}
}
