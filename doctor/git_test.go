// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/layout"
)

func TestCanonicalGitAssetMapsOnlyUserAgentArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	home := filepath.Join(t.TempDir(), "user")
	insideRel := ".agents/knowledge/note.md"
	mappedRoot, mappedRel := canonicalGitAsset(home, insideRel, home)
	if mappedRoot != filepath.Join(home, ".agents") || mappedRel != "knowledge/note.md" {
		t.Fatalf("mapped artifact = %q, %q", mappedRoot, mappedRel)
	}

	for _, test := range []struct {
		name string
		path string
		root string
		want bool
	}{
		{name: "same", path: root, root: root, want: true},
		{name: "descendant", path: filepath.Join(root, "nested", "file"), root: root, want: true},
		{name: "sibling", path: root + "-other", root: root, want: false},
		{name: "parent", path: filepath.Dir(root), root: root, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := insidePath(test.path, test.root); got != test.want {
				t.Fatalf("insidePath(%q, %q) = %v, want %v", test.path, test.root, got, test.want)
			}
		})
	}

	outsideRoot, outsideRel := canonicalGitAsset(root, "AGENTS.md", home)
	if outsideRoot != filepath.Clean(root) || outsideRel != "AGENTS.md" {
		t.Fatalf("ordinary project artifact = %q, %q", outsideRoot, outsideRel)
	}
	noHomeRoot, noHomeRel := canonicalGitAsset(root, "AGENTS.md", "")
	if noHomeRoot != filepath.Clean(root) || noHomeRel != "AGENTS.md" {
		t.Fatalf("artifact without home = %q, %q", noHomeRoot, noHomeRel)
	}
}

func TestSortGitPathsOrdersPathThenStatus(t *testing.T) {
	paths := []GitPath{{Path: "b", Status: " M"}, {Path: "a", Status: "??"}, {Path: "a", Status: " M"}}
	sortGitPaths(paths)
	if got := []GitPath{paths[0], paths[1], paths[2]}; got[0].Path != "a" || got[0].Status != " M" || got[1].Path != "a" || got[1].Status != "??" || got[2].Path != "b" {
		t.Fatalf("sorted paths = %#v", paths)
	}
}

func TestGitCheckIsScopedToManagedAssets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	home := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init")
	git(t, root, "add", "--", "AGENTS.md", ".agents")
	git(t, root, "-c", "user.name=Threadpoint", "-c", "user.email=threadpoint@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "init")

	// Neither an unrelated file nor a provider bridge / native artifact is a
	// threadpoint-managed asset. Threadpoint catalogs native files but no longer
	// authors bridge pointers, so the Git hygiene check must ignore both.
	mustWrite(t, filepath.Join(root, "notes.txt"), "not managed")
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "agent-authored bridge, not threadpoint-managed")
	report, err := Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.GitUntrackedPaths != 0 || report.Summary.GitDirtyPaths != 0 {
		t.Fatalf("expected unmanaged files, including provider bridges, to be ignored, got %#v", report.Summary)
	}

	// A file inside the shared .agents layout is threadpoint-managed and is flagged.
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "new.md"), "managed knowledge")
	report, err = Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.GitUntrackedPaths == 0 {
		t.Fatalf("expected untracked managed asset, got %#v", report.Summary)
	}
	if !hasFinding(report, "git_untracked_assets") {
		t.Fatalf("expected untracked Git finding, got %#v", report.Findings)
	}

	mustWrite(t, filepath.Join(root, "AGENTS.md"), "changed shared guide")
	report, err = Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.GitDirtyPaths == 0 || !hasFinding(report, "git_dirty_assets") {
		t.Fatalf("expected changed managed asset, got summary=%#v findings=%#v", report.Summary, report.Findings)
	}
}

func TestGitCheckIgnoresInheritedGitEnvironment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	home := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init")
	git(t, root, "add", "--", "AGENTS.md", ".agents")
	git(t, root, "-c", "user.name=Threadpoint", "-c", "user.email=threadpoint@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "init")

	badConfig := filepath.Join(t.TempDir(), "gitconfig")
	mustWrite(t, badConfig, "[core\n")
	t.Setenv("GIT_CONFIG_GLOBAL", badConfig)
	t.Setenv("GIT_CONFIG_SYSTEM", badConfig)
	t.Setenv("GIT_TRACE", "1")

	report, err := Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.GitWorktrees != 1 {
		t.Fatalf("expected Git worktree to be checked despite inherited config, got %#v", report.Summary)
	}
	if report.Summary.GitUntrackedPaths != 0 || report.Summary.GitDirtyPaths != 0 || report.Summary.GitConflictedPaths != 0 {
		t.Fatalf("expected clean managed Git status despite inherited trace, got %#v", report.Summary)
	}
	for _, gitReport := range report.Git {
		if gitReport.CheckFailed != "" {
			t.Fatalf("expected Git check to ignore inherited config, got %#v", gitReport)
		}
	}
}

func TestCheckGitClassifiesTrackedChangesAndUntrackedManagedAssets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init")
	git(t, root, "add", "--", "AGENTS.md", ".agents")
	git(t, root, "-c", "user.name=Threadpoint", "-c", "user.email=threadpoint@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "initial layout")
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "changed guide")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "untracked.md"), "untracked knowledge")
	report := checkGit(context.Background(), root, []string{"AGENTS.md", ".agents"})
	if !report.InWorktree || len(report.Dirty) != 1 || report.Dirty[0].Path != "AGENTS.md" || len(report.Untracked) != 1 || report.Untracked[0].Path != ".agents/knowledge/untracked.md" {
		t.Fatalf("Git state = %#v", report)
	}
	if paths := parseStatus("UU conflict.md\x00R  old.md\x00new.md\x00"); len(paths) != 2 || paths[0].status != "UU" || paths[1].status != "R " {
		t.Fatalf("parsed rename/conflict status = %#v", paths)
	}
	if !isConflicted("AA") || isConflicted(" M") {
		t.Fatal("conflict classification is incorrect")
	}
}

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, body)
	}
}
