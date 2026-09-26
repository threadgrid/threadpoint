// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteRootFileUsingUmaskPreservesExistingModeAndValidates(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := AtomicWriteRootFileUsingUmask(root, "AGENTS.md", []byte("new\n"), 0o666, 0o777, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("existing target mode = %04o, want 0600", info.Mode().Perm())
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "new\n" {
		t.Fatalf("body=%q err=%v", body, err)
	}

	wantErr := errors.New("target changed")
	err = AtomicWriteRootFileUsingUmask(root, "AGENTS.md", []byte("later\n"), 0o666, 0o777, func(*os.Root, string) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected validate error, got %v", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "new\n" {
		t.Fatalf("target changed despite failed validation: body=%q err=%v", body, err)
	}
}

func TestAtomicWriteFileIfMissingUsingUmaskUsesDirectCreateWhenHardLinksUnavailable(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "nested", "missing.txt")
	originalLinkFile := linkFile
	linkFile = func(string, string) error {
		return fmt.Errorf("operation not supported")
	}
	t.Cleanup(func() {
		linkFile = originalLinkFile
	})

	if err := AtomicWriteFileIfMissingUsingUmask(target, []byte("created"), 0o666, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFileIfMissingUsingUmask(target, []byte("updated"), 0o666, 0o777); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "created" {
		t.Fatalf("if-missing write should not replace existing file, got %q", body)
	}
}

func TestAtomicWriteUsingUmaskAndRootHelpersPreserveExpectedFilesystemState(t *testing.T) {
	rootPath := t.TempDir()
	target := filepath.Join(rootPath, "nested", "guide.md")
	if err := AtomicWriteFileUsingUmask(target, []byte("first"), 0o666, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFileUsingUmask(target, []byte("second"), 0o666, 0o777); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "second" {
		t.Fatalf("umask write body = %q, err=%v", body, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement mode = %v, err=%v", info.Mode(), err)
	}

	missing := filepath.Join(rootPath, "missing", "AGENTS.md")
	if err := AtomicWriteFileIfMissingUsingUmask(missing, []byte("created"), 0o666, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFileIfMissingUsingUmask(missing, []byte("ignored"), 0o666, 0o777); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(missing); err != nil || string(body) != "created" {
		t.Fatalf("missing-file umask body = %q, err=%v", body, err)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := AtomicWriteRootFile(nil, "guide.md", []byte("ignored"), 0o644, nil); err == nil {
		t.Fatal("nil rooted write should fail")
	}
	if err := AtomicWriteRootFile(root, "../guide.md", []byte("ignored"), 0o644, nil); err == nil {
		t.Fatal("rooted write outside root should fail")
	}
	validationErr := errors.New("reviewed state changed")
	if err := AtomicWriteRootFile(root, "rooted.md", []byte("ignored"), 0o644, func(*os.Root, string) error { return validationErr }); !errors.Is(err, validationErr) {
		t.Fatalf("rooted write validation error = %v", err)
	}
	if err := AtomicWriteRootFile(root, "rooted.md", []byte("written"), 0o644, nil); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(rootPath, "rooted.md")); err != nil || string(body) != "written" {
		t.Fatalf("rooted write body = %q, err=%v", body, err)
	}
}
