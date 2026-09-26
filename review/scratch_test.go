// SPDX-License-Identifier: Apache-2.0

package review

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditRejectsEditorReplacingScratchFileWithSymlink(t *testing.T) {
	dir := t.TempDir()
	tool := writeTool(t, dir, "symlink-editor", "rm -- \"$1\"\nln -s /dev/null \"$1\"\n")
	t.Setenv("VISUAL", tool)
	t.Setenv("EDITOR", "")

	if _, err := Edit(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "notes.md", []byte("private")); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink output error = %v", err)
	}
}

func TestScratchHelpersRejectUnsafeAndOversizedArtifacts(t *testing.T) {
	dir := t.TempDir()

	if _, err := writeScratch(filepath.Join(dir, "missing"), "edit", "notes.md", []byte("body")); err == nil {
		t.Fatal("writeScratch unexpectedly wrote below a missing directory")
	}
	if _, err := readScratch(filepath.Join(dir, "missing")); err == nil || !os.IsNotExist(err) {
		t.Fatalf("missing scratch error = %v", err)
	}
	if _, err := readScratch(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory scratch error = %v", err)
	}

	symlinkPath := filepath.Join(dir, "symlink")
	if err := os.Symlink("missing-target", symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readScratch(symlinkPath); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink scratch error = %v", err)
	}

	oversizedPath := filepath.Join(dir, "oversized")
	file, err := os.OpenFile(oversizedPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxContentBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readScratch(oversizedPath); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized scratch error = %v", err)
	}

	unreadablePath := filepath.Join(dir, "unreadable")
	if err := os.WriteFile(unreadablePath, []byte("private"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := readScratch(unreadablePath); err == nil || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable scratch error = %v", err)
	}
}

func TestExtensionUsesOnlyShortAlphanumericSuffixes(t *testing.T) {
	tests := map[string]string{
		"notes":                         ".md",
		"notes.":                        ".md",
		"notes.bad-name":                ".md",
		"notes.abcdefghijklmnopq":       ".md",
		"nested/path/CONFIG.JSON":       ".JSON",
		"nested/path/archive.tar.gz":    ".gz",
		"nested/path/numbers.thread123": ".thread123",
	}
	for source, want := range tests {
		if got := extension(source); got != want {
			t.Errorf("extension(%q) = %q, want %q", source, got, want)
		}
	}
}

func TestEditUsesSourceExtensionAndConfiguredEditor(t *testing.T) {
	dir := t.TempDir()
	result := filepath.Join(dir, "result")
	editor := writeTool(t, dir, "editor", "printf '%s' \"$1\" > \"$REVIEW_RESULT\"\nprintf 'edited' > \"$1\"\n")
	t.Setenv("REVIEW_RESULT", result)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", editor)

	got, err := Edit(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "notes/config.toml", []byte("initial"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "edited" {
		t.Fatalf("edited content = %q", got)
	}
	path, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(path), ".toml") {
		t.Fatalf("editor file = %q, want .toml", path)
	}
}
