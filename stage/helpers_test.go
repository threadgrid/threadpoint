// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func retargetSelectedProject(t *testing.T, logical, target string) {
	t.Helper()
	if err := os.Remove(logical); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, logical); err != nil {
		t.Fatal(err)
	}
}

func stageTransactionArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(dir)
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+base+".next-") || strings.HasPrefix(entry.Name(), "."+base+".old-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func replacePathWithSymlink(t *testing.T, path, replacement string) {
	t.Helper()
	moved := path + "-retained"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, path); err != nil {
		_ = os.Rename(moved, path)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove replacement symlink: %v", err)
			return
		}
		if err := os.Rename(moved, path); err != nil {
			t.Errorf("restore retained path: %v", err)
		}
	})
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "config", "user.email", "threadpoint@example.invalid")
	git(t, root, "config", "user.name", "Threadpoint Test")
	write(t, filepath.Join(root, "README.md"), "# test\n")
	git(t, root, "add", "README.md")
	git(t, root, "commit", "-m", "initial")
	return root
}

func git(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root, "-c", "commit.gpgSign=false"}, args...)...) //nolint:noctx // test git helper; no context cancellation needed.
	body, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, body)
	}
	return string(body)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
