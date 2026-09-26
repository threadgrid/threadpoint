// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/doctor"
	"github.com/threadgrid/threadpoint/knowledgebase"
	"github.com/threadgrid/threadpoint/layout"
)

func TestRootDefaultsToAbsoluteCurrentDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	stdout, _, err := runTestCLI(t, "status", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var kb knowledgebase.Catalog
	if err := json.Unmarshal([]byte(stdout), &kb); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if kb.Root != root {
		t.Fatalf("status root = %q, want absolute cwd %q", kb.Root, root)
	}
}

func TestImplicitAgentsRootBeatsGitTopLevel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	initTestGitRepo(t, repo)
	project := filepath.Join(repo, "services", "api")
	if err := layout.EnsureShared(project); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(project, "src")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	stdout, _, err := runTestCLI(t, "status", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var kb knowledgebase.Catalog
	if err := json.Unmarshal([]byte(stdout), &kb); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if kb.Root != project {
		t.Fatalf("status root = %q, want nearest .agents root %q", kb.Root, project)
	}
}

func TestImplicitRootIgnoresHomeGitTopLevel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	initTestGitRepo(t, home)
	root := filepath.Join(home, "workspace", "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	stdout, _, err := runTestCLI(t, "status", "--format", "json")
	if err == nil {
		t.Fatal("expected invalid shared layout")
	}
	var kb knowledgebase.Catalog
	if err := json.Unmarshal([]byte(stdout), &kb); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if kb.Root != root {
		t.Fatalf("status root = %q, want cwd %q because home Git root is invalid", kb.Root, root)
	}
}

func TestExplicitRootDoesNotResolveAgentsParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := t.TempDir()
	if err := layout.EnsureShared(parent); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runTestCLI(t, "status", "--root", child, "--format", "json")
	if err == nil {
		t.Fatal("expected invalid shared layout")
	}
	var kb knowledgebase.Catalog
	if err := json.Unmarshal([]byte(stdout), &kb); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if kb.Root != child {
		t.Fatalf("status root = %q, want explicit child root %q", kb.Root, child)
	}
}

func TestRootFlagNormalizesRelativePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	stdout, _, err := runTestCLI(t, "status", "--root", "project", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var report layout.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if report.Root != root {
		t.Fatalf("status root = %q, want %q", report.Root, root)
	}
}

func TestRootFlagPreservesSymlinkRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	stdout, _, err := runTestCLI(t, "status", "--root", link, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var report doctor.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("doctor emitted invalid JSON: %v\n%s", err, stdout)
	}
	if report.Root != link {
		t.Fatalf("doctor root = %q, want logical symlink root %q", report.Root, link)
	}
}

func TestImplicitRootRejectsNativeMarkerBelowStrongAncestor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(project, "package")
	if err := os.MkdirAll(filepath.Join(native, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(native, "src")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	_, _, err := runTestCLI(t, "status", "--format", "json")
	if err == nil || !strings.Contains(err.Error(), "pass --root") {
		t.Fatalf("implicit native root error = %v, want --root remediation", err)
	}
}

func TestEmptyExplicitRootFailsBeforeMutation(t *testing.T) {
	for _, root := range []string{"", "   "} {
		for _, command := range []string{"status", "init", "stage"} {
			t.Run(command+"/"+root, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				cwd := t.TempDir()
				t.Chdir(cwd)
				args := []string{command, "--root", root}
				if command == "init" {
					args = append(args, "--yes")
				}
				if command == "stage" {
					args = append(args, "--apply", "--yes")
				}
				_, _, err := runTestCLI(t, args...)
				if err == nil || exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "--root cannot be empty") {
					t.Fatalf("empty root error = %v", err)
				}
				entries, err := os.ReadDir(cwd)
				if err != nil || len(entries) != 0 {
					t.Fatalf("root mutated: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestGlobalRootFlagExplainsCommandPosition(t *testing.T) {
	_, _, err := runTestCLI(t, "--root", ".", "status")
	if err == nil || exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "threadpoint <command> --root PATH") {
		t.Fatalf("misplaced root error = %v", err)
	}
}
