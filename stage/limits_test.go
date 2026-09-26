// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDescriptorBoundGitUsesRetainedHighDescriptorAcrossAmbientABA(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "android", "darwin", "dragonfly", "freebsd", "netbsd", "openbsd":
	default:
		t.Skip("descriptor-backed worktree paths are unavailable")
	}
	original := initGitRepo(t)
	write(t, filepath.Join(original, "notes.md"), "tracked original\n")
	git(t, original, "add", "notes.md")
	git(t, original, "commit", "-m", "track original")
	replacement := initGitRepo(t)
	write(t, filepath.Join(replacement, ".gitignore"), "notes.md\n")
	write(t, filepath.Join(replacement, "notes.md"), "ignored replacement\n")
	git(t, replacement, "add", ".gitignore")
	git(t, replacement, "commit", "-m", "ignore replacement")

	var held []*os.File
	for range 32 {
		file, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, file)
	}
	t.Cleanup(func() {
		for _, file := range held {
			_ = file.Close()
		}
	})
	retained, err := os.OpenRoot(original)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	parked := original + ".parked"
	var observed uintptr
	previousOpened := gitRootOpened
	previousBefore := gitRootBeforeRun
	previousAfter := gitRootAfterRun
	gitRootOpened = func(descriptor uintptr) { observed = descriptor }
	gitRootBeforeRun = func() {
		if err := os.Rename(original, parked); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, original); err != nil {
			t.Fatal(err)
		}
	}
	gitRootAfterRun = func() {
		if err := os.Rename(original, replacement); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(parked, original); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		gitRootOpened = previousOpened
		gitRootBeforeRun = previousBefore
		gitRootAfterRun = previousAfter
	})

	scope, err := ClassifyProjectSourceFromRoot(original, "notes.md", nil, retained)
	if err != nil {
		t.Fatal(err)
	}
	if observed == 0 || observed == 3 {
		t.Fatalf("descriptor-bound Git did not exercise a high inherited descriptor: %d", observed)
	}
	if scope != ScopeProjectShared {
		t.Fatalf("Git classified ambient replacement instead of retained worktree: %s", scope)
	}
}

func TestDescriptorBoundGitDirectoryFailsClosedOnWindows(t *testing.T) {
	if path, err := descriptorBoundGitDirectory("windows", 37); err == nil || path != "" {
		t.Fatalf("Windows descriptor binding unexpectedly fell back to an ambient path: path=%q err=%v", path, err)
	}
}

func TestDescriptorBoundGitTreatsMissingRepositoryAsNoMatch(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "android", "darwin", "dragonfly", "freebsd", "netbsd", "openbsd":
	default:
		t.Skip("descriptor-backed worktree paths are unavailable")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "explicitly classified source\n")
	retained, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()

	scope, err := ClassifyProjectSourceFromRoot(root, "notes.md", map[string]Scope{"notes.md": ScopeProjectShared}, retained)
	if err != nil {
		t.Fatalf("explicit classification outside a Git repository: %v", err)
	}
	if scope != ScopeProjectShared {
		t.Fatalf("explicit classification = %q, want %q", scope, ScopeProjectShared)
	}
	if err := validateTargetGitFromRoot(retained, "AGENTS.md", ScopeProjectShared); err != nil {
		t.Fatalf("project-shared target outside a Git repository: %v", err)
	}
}

func TestDescriptorBoundGitDoesNotHideInvalidRepositoryMetadata(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "android", "darwin", "dragonfly", "freebsd", "netbsd", "openbsd":
	default:
		t.Skip("descriptor-backed worktree paths are unavailable")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "explicitly classified source\n")
	write(t, filepath.Join(root, ".git"), "not a gitfile\n")
	retained, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()

	if _, err := ClassifyProjectSourceFromRoot(root, "notes.md", map[string]Scope{"notes.md": ScopeProjectShared}, retained); err == nil || !strings.Contains(err.Error(), "descriptor-bound Git query") {
		t.Fatalf("invalid repository metadata was treated as no Git match: %v", err)
	}
}

func TestDescriptorBoundGitRefusesUnsupportedOperatingSystem(t *testing.T) {
	if path, err := descriptorBoundGitDirectory("windows", 7); err == nil || path != "" {
		t.Fatalf("unsupported descriptor path = %q, %v", path, err)
	}
}
