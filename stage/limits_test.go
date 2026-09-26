// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"errors"
	"os"
	"os/exec"
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
	if script, args, err := descriptorBoundGitScript("windows", nil); err == nil || script != "" || args != nil {
		t.Fatalf("Windows descriptor wrapper unexpectedly built: script=%q args=%q err=%v", script, args, err)
	}
}

func TestDescriptorBoundGitWrapperEntersOnlyTheRetainedDirectory(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "android", "darwin":
	default:
		t.Skip("descriptor-bound Git wrapper is not exercised on this platform")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	retained := t.TempDir()
	directory, err := os.Open(retained)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	run := func(locate func() (string, string, error)) error {
		script, args, err := descriptorBoundGitScript(runtime.GOOS, locate)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), "sh", append(append([]string{"-c", script, "threadpoint-git"}, args...), "--version")...)
		command.ExtraFiles = []*os.File{directory}
		return command.Run()
	}

	if err := run(func() (string, string, error) { return descriptorLocation(directory) }); err != nil {
		t.Fatalf("wrapper refused the retained directory: %v", err)
	}
	if runtime.GOOS != "darwin" {
		return
	}
	_, identity, err := descriptorLocation(directory)
	if err != nil {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if err := run(func() (string, string, error) { return t.TempDir(), identity, nil }); !errors.As(err, &exitErr) || exitErr.ExitCode() != gitWorktreeEntryFailed {
		t.Fatalf("wrapper ran Git outside the retained directory: %v", err)
	}
}

func TestDescriptorBoundGitDarwinWrapperBindsLocatedIdentity(t *testing.T) {
	script, args, err := descriptorBoundGitScript("darwin", func() (string, string, error) { return "/retained", "16777232:42", nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, `[ "$(stat -f '%d:%i' .)" = "$2" ]`) || len(args) != 2 || args[0] != "/retained" || args[1] != "16777232:42" {
		t.Fatalf("darwin wrapper does not bind the located identity: script=%q args=%q", script, args)
	}
	lookup := errors.New("retained directory location unavailable")
	if _, _, err := descriptorBoundGitScript("darwin", func() (string, string, error) { return "", "", lookup }); !errors.Is(err, lookup) {
		t.Fatalf("descriptor wrapper hid the location lookup failure: %v", err)
	}
}

func TestDescriptorLocationFollowsRetainedDirectory(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "original")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}

	path, identity, err := descriptorLocation(directory)
	if runtime.GOOS != "darwin" {
		if err == nil {
			t.Fatalf("descriptor location unexpectedly available: path=%q identity=%q", path, identity)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(moved)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(current, want) {
		t.Fatalf("descriptor path %q does not name the retained directory", path)
	}
	reported, err := exec.CommandContext(t.Context(), "stat", "-f", "%d:%i", moved).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(reported)); got != identity {
		t.Fatalf("descriptor identity = %q, stat reports %q", identity, got)
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

func TestRetainedTargetGitValidationEnforcesScopeIgnoreRules(t *testing.T) {
	root := initGitRepo(t)
	write(t, filepath.Join(root, ".gitignore"), "ignored.md\n")
	write(t, filepath.Join(root, "tracked.md"), "shared\n")
	write(t, filepath.Join(root, "ignored.md"), "local\n")
	write(t, filepath.Join(root, "untracked.md"), "unreviewed\n")
	git(t, root, "add", ".gitignore", "tracked.md")
	git(t, root, "commit", "-m", "classify targets")
	retained, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()

	for _, test := range []struct {
		name   string
		target string
		scope  Scope
		valid  bool
	}{
		{"shared tracked", "tracked.md", ScopeProjectShared, true},
		{"shared ignored", "ignored.md", ScopeProjectShared, false},
		{"local ignored", "ignored.md", ScopeProjectLocal, true},
		{"local tracked", "tracked.md", ScopeProjectLocal, false},
		{"local unignored", "untracked.md", ScopeProjectLocal, false},
		{"unknown scope", "tracked.md", Scope("unknown"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateTargetGitFromRoot(retained, test.target, test.scope); (err == nil) != test.valid {
				t.Fatalf("validateTargetGitFromRoot(%q, %q) = %v, want valid=%t", test.target, test.scope, err, test.valid)
			}
		})
	}
	if err := validateTargetGitFromRoot(nil, "tracked.md", ScopeProjectShared); err == nil {
		t.Fatal("target validation ran without a retained project root")
	}
	if _, err := classifyFromRoot(nil, "tracked.md", "", nil); err == nil {
		t.Fatal("classification ran without a retained project root")
	}
}
