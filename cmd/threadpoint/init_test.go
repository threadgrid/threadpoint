// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCreatesSharedLayout(t *testing.T) {
	root := t.TempDir()

	stdout, _, err := runTestCLI(t, "init", "--root", root, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}

	for _, rel := range []string{
		"AGENTS.md",
		filepath.Join(".agents", "adapters", "README.md"),
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("expected %s: %v", rel, err)
		}
	}
	for _, rel := range []string{
		filepath.Join(".agents", "knowledge"),
		filepath.Join(".agents", "rules"),
		filepath.Join(".agents", "skills"),
		filepath.Join(".agents", "plugins"),
		filepath.Join("state", "skills"),
	} {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("expected %s: %v", rel, err)
		}
		if !info.IsDir() {
			t.Fatalf("expected %s to be a directory", rel)
		}
	}
	for _, rel := range []string{
		filepath.Join(".agents", "knowledge", "feature-catalog.json"),
		filepath.Join(".agents", "knowledge", "requirements-manifest.json"),
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("expected %s not to be created, stat err: %v", rel, err)
		}
	}

	for _, rel := range []string{"AGENTS.local.md", ".agents.local"} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("shared init should not create %s, stat err=%v", rel, err)
		}
	}
	gitignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("shared init should create .gitignore: %v", err)
	}
	if strings.Count(string(gitignore), "/state/") != 1 {
		t.Fatalf(".gitignore = %q, want one /state/ entry", gitignore)
	}
	stdout, _, err = runTestCLI(t, "init", "--root", root, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("expected repeated init to be quiet, got %q", stdout)
	}
	gitignore, err = os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(gitignore), "/state/") != 1 {
		t.Fatalf("repeated init duplicated state ignore: %q", gitignore)
	}
}

func TestInitUsesNormalizedRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := t.TempDir()
	t.Chdir(base)
	root := filepath.Join(base, "new-project")

	stdout, _, err := runTestCLI(t, "init", "--root", "new-project", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("init stdout = %q, want empty", stdout)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatalf("init did not create layout under normalized root: %v", err)
	}
}

func TestInitKeepsBootstrapOnLockedProjectGeneration(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	selected := filepath.Join(parent, "selected")
	for _, root := range []string{first, second} {
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(first, selected); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	originalHook := initAfterProjectRootBorrow
	t.Cleanup(func() { initAfterProjectRootBorrow = originalHook })
	initAfterProjectRootBorrow = func() {
		initAfterProjectRootBorrow = nil
		if err := os.Remove(selected); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(second, selected); err != nil {
			t.Fatal(err)
		}
	}

	app := &cli{stdin: strings.NewReader(""), stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	t.Setenv("THREADPOINT_HOME", filepath.Join(parent, "home"))
	err := runInit(context.Background(), app, []string{"--root", selected, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "locked generation") {
		t.Fatalf("init retarget error = %v, want locked-generation refusal", err)
	}
	if _, err := os.Stat(filepath.Join(first, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retained generation was mutated after its logical selection changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement generation received bootstrap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement generation received state layout: %v", err)
	}
}

func TestInitDoesNotCreateProjectLocalOverlay(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	initTestGitRepo(t, root)
	_, stderr, err := runTestCLI(t, "--home", home, "init", "--root", root, "--yes")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, stderr)
	}
	for _, rel := range []string{"AGENTS.local.md", ".agents.local"} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("shared init should not create %s, stat err=%v", rel, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil || !strings.Contains(string(body), "/state/\n") {
		t.Fatalf("shared init .gitignore = %q, err=%v", body, err)
	}
	gitCommand(t, root, "check-ignore", "-q", "state/skills/example/cache.json")
}

func TestInitReportsDiscoveredAncestor(t *testing.T) {
	for _, mode := range []string{"text", "json", "quiet"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			initTestGitRepo(t, root)
			child := filepath.Join(root, "newthing")
			if err := os.Mkdir(child, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(child)
			args := []string{"init", "--yes"}
			if mode == "json" {
				args = append(args, "--format", "json")
			}
			if mode == "quiet" {
				args = append([]string{"--quiet"}, args...)
			}
			stdout, stderr, err := runTestCLI(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "text" && (!strings.Contains(stderr, root) || strings.Contains(stderr, child) || stdout != "") {
				t.Fatalf("missing selected root: stdout=%q stderr=%q", stdout, stderr)
			}
			if mode == "quiet" && (stdout != "" || stderr != "") {
				t.Fatalf("quiet output: %q %q", stdout, stderr)
			}
			if mode == "json" {
				var report initReport
				if err := json.Unmarshal([]byte(stdout), &report); err != nil || report.Root != root || stderr != "" {
					t.Fatalf("JSON report = %q, stderr=%q err=%v", stdout, stderr, err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(child, "AGENTS.md")); !os.IsNotExist(err) {
				t.Fatalf("child was initialized: %v", err)
			}
		})
	}
}

func TestInitPromptNamesSelectedRootBeforeMutation(t *testing.T) {
	root := t.TempDir()
	terminal, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if !interactiveInput(terminal) {
		t.Skip("no character device")
	}
	var stdout, stderr strings.Builder
	app := newCLI(&stdout, &stderr, terminal)
	err = runInit(context.Background(), app, []string{"--root", root})
	if err == nil || !strings.Contains(stderr.String(), root) {
		t.Fatalf("prompt=%q err=%v", stderr.String(), err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("unconfirmed init mutated root: %v", err)
	}
}
