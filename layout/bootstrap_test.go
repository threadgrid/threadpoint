// SPDX-License-Identifier: Apache-2.0

package layout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureProjectBootstrapInRootUsesOnlyRetainedGeneration(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	detached := filepath.Join(parent, "detached")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(project, detached); err != nil {
		t.Skipf("platform does not permit renaming an opened root: %v", err)
	}
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := EnsureProjectBootstrapInRoot(root); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"AGENTS.md",
		filepath.Join(".agents", "adapters", "README.md"),
		filepath.Join("state", "skills"),
		".gitignore",
	} {
		if _, err := os.Stat(filepath.Join(detached, rel)); err != nil {
			t.Fatalf("retained generation missing %s: %v", rel, err)
		}
		if _, err := os.Stat(filepath.Join(project, rel)); !os.IsNotExist(err) {
			t.Fatalf("replacement generation received %s: %v", rel, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(detached, ".gitignore"))
	if err != nil || !strings.Contains(string(body), "/state/\n") {
		t.Fatalf("retained .gitignore = %q, err=%v", body, err)
	}
}

func TestEnsureSharedLayoutCreatesBootstrapLayout(t *testing.T) {
	root := t.TempDir()

	if err := EnsureShared(root); err != nil {
		t.Fatal(err)
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
	} {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("expected %s: %v", rel, err)
		}
		if !info.IsDir() {
			t.Fatalf("expected %s to be a directory", rel)
		}
	}
}

func TestEnsureSharedDirectoriesInRootUsesPinnedProject(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	moved := filepath.Join(base, "moved")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(project, moved); err != nil {
		t.Skipf("platform does not permit renaming an opened root: %v", err)
	}
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedDirectoriesInRoot(root); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(moved, ".agents", "knowledge")); err != nil || !info.IsDir() {
		t.Fatalf("pinned root should receive shared directories: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(project, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("replacement pathname must remain untouched: %v", err)
	}
}

func TestEnsureSharedLayoutPreservesExplicitLogicalSymlinkRoot(t *testing.T) {
	physicalRoot := t.TempDir()
	logicalRoot := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(physicalRoot, logicalRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := EnsureShared(logicalRoot); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"AGENTS.md", filepath.Join(".agents", "knowledge")} {
		if _, err := os.Stat(filepath.Join(logicalRoot, rel)); err != nil {
			t.Fatalf("expected %s under logical root: %v", rel, err)
		}
	}
}

func TestEnsureLayoutsPreserveExistingBootstrapContent(t *testing.T) {
	root := t.TempDir()
	if err := EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProjectLocal(root); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		"AGENTS.md": "# Custom shared guide\n",
		filepath.Join(".agents", "adapters", "README.md"): "# Custom adapter guide\n",
		"AGENTS.local.md": "# Custom local guide\n",
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProjectLocal(root); err != nil {
		t.Fatal(err)
	}
	for rel, want := range files {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Fatalf("%s changed on reinitialization: %q != %q", rel, body, want)
		}
	}
}

func TestEnsureProjectLocalLayoutInRootCreatesCompleteOverlay(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := EnsureProjectLocalInRoot(root); err != nil {
		t.Fatal(err)
	}
	for _, rel := range append(localDirectoryPaths(), "AGENTS.local.md", ".gitignore") {
		if _, err := os.Lstat(filepath.Join(rootPath, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("rooted local layout omitted %s: %v", rel, err)
		}
	}
	gitignore, err := os.ReadFile(filepath.Join(rootPath, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if body := string(gitignore); !strings.Contains(body, "/AGENTS.local.md") || !strings.Contains(body, "/.agents.local/") {
		t.Fatalf("rooted local ignore rules = %q", body)
	}
	if err := EnsureProjectLocalInRoot(nil); err == nil {
		t.Fatal("nil rooted local layout capability should fail")
	}
}
