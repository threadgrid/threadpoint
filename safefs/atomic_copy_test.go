// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicCopyFileCopiesRegularContentAndRejectsSymlinksAndDirectories(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	if err := os.WriteFile(source, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCopyFile(source, filepath.Join(root, "copied.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(root, "copied.txt")); err != nil || string(body) != "source" {
		t.Fatalf("copied body = %q, err=%v", body, err)
	}
	link := filepath.Join(root, "source-link")
	if err := os.Symlink(source, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := AtomicCopyFile(link, filepath.Join(root, "link-copy"), 0o644); !errors.Is(err, ErrUnsafeSymlink) {
		t.Fatalf("symlink copy error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "source-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCopyFile(filepath.Join(root, "source-dir"), filepath.Join(root, "directory-copy"), 0o644); err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("directory copy error = %v", err)
	}
}
