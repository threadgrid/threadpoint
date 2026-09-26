// SPDX-License-Identifier: Apache-2.0

package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/provider"
)

func TestDiscoverRejectsFilesystemRootMissingRootAndFileRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := Discover(Options{Root: string(filepath.Separator), Explicit: true}); err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("filesystem-root error = %v", err)
	}

	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := Discover(Options{Root: missing, Explicit: true}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing-root error = %v", err)
	}

	file := filepath.Join(t.TempDir(), "project.txt")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(Options{Root: file, Explicit: true}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file-root error = %v", err)
	}
}

func TestDiscoverWithoutHomeStopsAtFilesystemBoundary(t *testing.T) {
	t.Setenv("HOME", "")
	root := t.TempDir()
	selection, err := Discover(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if selection.InputRoot != root || selection.ProjectRoot != root {
		t.Fatalf("selection = %#v, want unmarked input root", selection)
	}
	if _, ok := normalizedUserAgentsRoot(); ok {
		t.Fatal("normalizedUserAgentsRoot unexpectedly succeeded without HOME")
	}
}

func TestDiscoverSelectsNearestStrongMarker(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	mustMkdir(t, filepath.Join(project, ".agents"))
	child := filepath.Join(project, "packages", "api")
	mustWrite(t, filepath.Join(child, "AGENTS.local.md"), "# Local\n")
	work := filepath.Join(child, "src")
	mustMkdir(t, work)

	selection, err := Discover(Options{Root: work, WorkDir: base})
	if err != nil {
		t.Fatal(err)
	}
	if selection.ProjectRoot != child {
		t.Fatalf("project root = %q, want nearest strong marker %q", selection.ProjectRoot, child)
	}
}

func TestDiscoverTreatsLocalDirectoryAsStrongMarker(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	mustMkdir(t, filepath.Join(project, ".agents.local"))
	work := filepath.Join(project, "src")
	mustMkdir(t, work)

	selection, err := Discover(Options{Root: work, WorkDir: base})
	if err != nil {
		t.Fatal(err)
	}
	if selection.ProjectRoot != project {
		t.Fatalf("project root = %q, want local marker root %q", selection.ProjectRoot, project)
	}
}

func TestDiscoverTreatsEveryStrongMarkerAsProjectRoot(t *testing.T) {
	for _, marker := range []struct {
		name string
		make func(*testing.T, string)
	}{
		{name: "git-directory", make: func(t *testing.T, root string) { t.Helper(); mustMkdir(t, filepath.Join(root, ".git")) }},
		{name: "git-file", make: func(t *testing.T, root string) {
			t.Helper()
			mustWrite(t, filepath.Join(root, ".git"), "gitdir: /tmp/worktree\n")
		}},
		{name: "agents-guide", make: func(t *testing.T, root string) {
			t.Helper()
			mustWrite(t, filepath.Join(root, "AGENTS.md"), "# Guide\n")
		}},
		{name: "agents-directory", make: func(t *testing.T, root string) { t.Helper(); mustMkdir(t, filepath.Join(root, ".agents")) }},
		{name: "local-guide", make: func(t *testing.T, root string) {
			t.Helper()
			mustWrite(t, filepath.Join(root, "AGENTS.local.md"), "# Local\n")
		}},
		{name: "local-directory", make: func(t *testing.T, root string) { t.Helper(); mustMkdir(t, filepath.Join(root, ".agents.local")) }},
	} {
		t.Run(marker.name, func(t *testing.T) {
			project := filepath.Join(t.TempDir(), "project")
			marker.make(t, project)
			work := filepath.Join(project, "src")
			mustMkdir(t, work)

			selection, err := Discover(Options{Root: work})
			if err != nil {
				t.Fatal(err)
			}
			if selection.ProjectRoot != project {
				t.Fatalf("project root = %q, want marker root %q", selection.ProjectRoot, project)
			}
		})
	}
}

func TestDiscoverSelectsGitDirectoryAndGitFile(t *testing.T) {
	for _, marker := range []struct {
		name string
		make func(*testing.T, string)
	}{
		{name: "directory", make: func(t *testing.T, root string) { t.Helper(); mustMkdir(t, filepath.Join(root, ".git")) }},
		{name: "file", make: func(t *testing.T, root string) {
			t.Helper()
			mustWrite(t, filepath.Join(root, ".git"), "gitdir: /tmp/worktree\n")
		}},
	} {
		t.Run(marker.name, func(t *testing.T) {
			project := filepath.Join(t.TempDir(), "project")
			marker.make(t, project)
			work := filepath.Join(project, "src")
			mustMkdir(t, work)

			selection, err := Discover(Options{Root: work})
			if err != nil {
				t.Fatal(err)
			}
			if selection.ProjectRoot != project {
				t.Fatalf("project root = %q, want Git root %q", selection.ProjectRoot, project)
			}
		})
	}
}

func TestDiscoverNativeFallbackRequiresNoStrongAncestor(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	mustMkdir(t, filepath.Join(project, ".git"))
	native := filepath.Join(project, "package")
	mustMkdir(t, filepath.Join(native, ".codex"))
	work := filepath.Join(native, "src")
	mustMkdir(t, work)

	_, err := Discover(Options{Root: work})
	var ambiguity *AmbiguityError
	if !errors.As(err, &ambiguity) {
		t.Fatalf("error = %v, want AmbiguityError", err)
	}
	if ambiguity.Native.Path != native || ambiguity.Ancestor.Path != project {
		t.Fatalf("ambiguity = %#v", ambiguity)
	}
	if !strings.Contains(err.Error(), "pass --root") {
		t.Fatalf("ambiguity error missing remediation: %v", err)
	}
}

func TestDiscoverNativeFallbackWithoutStrongAncestor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "workspace", "project")
	mustMkdir(t, filepath.Join(project, ".codex"))
	work := filepath.Join(project, "src")
	mustMkdir(t, work)

	selection, err := Discover(Options{Root: work})
	if err != nil {
		t.Fatal(err)
	}
	if selection.ProjectRoot != project {
		t.Fatalf("project root = %q, want native fallback %q", selection.ProjectRoot, project)
	}
}

func TestDiscoverDoesNotTreatUncuratedArtifactAsMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "project")
	mustWrite(t, filepath.Join(project, ".codex-plugin", "plugin.json"), "{}\n")
	work := filepath.Join(project, "src")
	mustMkdir(t, work)

	selection, err := Discover(Options{Root: work})
	if err != nil {
		t.Fatal(err)
	}
	if selection.ProjectRoot != work {
		t.Fatalf("project root = %q, want unmarked current directory %q", selection.ProjectRoot, work)
	}
}

func TestDiscoverNestedGitDoesNotInspectOuterMarkers(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "outer")
	mustMkdir(t, filepath.Join(outer, ".agents"))
	inner := filepath.Join(outer, "nested")
	mustMkdir(t, filepath.Join(inner, ".git"))
	work := filepath.Join(inner, "src")
	mustMkdir(t, work)

	selection, err := Discover(Options{Root: work})
	if err != nil {
		t.Fatal(err)
	}
	if selection.ProjectRoot != inner {
		t.Fatalf("project root = %q, want nested Git root %q", selection.ProjectRoot, inner)
	}
}

func TestDiscoverExplicitRootPreservesLogicalSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	mustMkdir(t, filepath.Join(target, ".agents"))
	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	selection, err := Discover(Options{Root: link, Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	if selection.InputRoot != link || selection.ProjectRoot != link {
		t.Fatalf("selection = %#v, want logical link %q", selection, link)
	}
}

func TestDiscoverRejectsSymlinkToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Discover(Options{Root: link, Explicit: true}); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("home symlink error = %v", err)
	}
}

func TestDiscoverAllowsMissingExplicitRootWhenRequested(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-project")
	selection, err := Discover(Options{Root: root, Explicit: true, AllowMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	if selection.ProjectRoot != root {
		t.Fatalf("project root = %q, want missing explicit root %q", selection.ProjectRoot, root)
	}
}

func TestDiscoverRejectsMissingRootUnderSymlinkedAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentsDir := filepath.Join(home, ".agents")
	mustMkdir(t, agentsDir)
	link := filepath.Join(t.TempDir(), "agents-link")
	if err := os.Symlink(agentsDir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	// The leaf does not exist yet (init creates it). A lexical check would miss
	// that the symlinked ancestor resolves into ~/.agents, letting the eventual
	// mkdir escape into the prohibited tree.
	missing := filepath.Join(link, "new-project")
	if _, err := Discover(Options{Root: missing, Explicit: true, AllowMissing: true}); err == nil || !strings.Contains(err.Error(), "under the user .agents directory") {
		t.Fatalf("symlinked-ancestor escape error = %v", err)
	}
}

func TestDiscoverSupportsCustomWorkspaceMarkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "project")
	mustMkdir(t, filepath.Join(project, ".example"))
	work := filepath.Join(project, "src")
	mustMkdir(t, work)
	registry := provider.NewRegistry([]provider.Definition{{ID: "example", WorkspaceMarkers: []string{".example"}}})

	selection, err := Discover(Options{Root: work, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if selection.ProjectRoot != project {
		t.Fatalf("project root = %q, want custom marker root %q", selection.ProjectRoot, project)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, body string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverRejectsEmptyExplicitRoot(t *testing.T) {
	for _, root := range []string{"", " ", "\t\n"} {
		t.Run(fmt.Sprintf("%q", root), func(t *testing.T) {
			_, err := Discover(Options{Root: root, Explicit: true, WorkDir: t.TempDir(), AllowMissing: true})
			if err == nil || !strings.Contains(err.Error(), "root cannot be empty") {
				t.Fatalf("explicit root %q error = %v", root, err)
			}
		})
	}
}
