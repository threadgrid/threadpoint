// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveRootFileMatchingBodyPreservesChangedGeneration(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := removeRootFileMatchingBody(root, "missing", []byte("expected")); err != nil {
		t.Fatalf("missing matching-body cleanup: %v", err)
	}
	if err := root.WriteFile("identity", []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeRootFileMatchingBody(root, "identity", []byte("expected")); !errors.Is(err, errFileLockChangedBeforeCleanup) {
		t.Fatalf("changed matching-body cleanup error = %v", err)
	}
	if body, err := root.ReadFile("identity"); err != nil || string(body) != "replacement" {
		t.Fatalf("changed identity file = %q, err=%v", body, err)
	}
	if err := removeRootFileMatchingBody(root, "identity", []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat("identity"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("matching identity file remains: %v", err)
	}
}

func TestRootExclusiveCreateNeverReplacesExistingDestination(t *testing.T) {
	if err := createRootExclusiveFile(nil, "identity", []byte("body"), 0o600); err == nil {
		t.Fatal("nil root accepted for exclusive create")
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := createRootExclusiveFile(root, "../identity", []byte("body"), 0o600); err == nil {
		t.Fatal("unsafe exclusive-create name was accepted")
	}
	if !RenameRootNoReplaceSupported() {
		t.Skip("no-replace rename unavailable")
	}
	if err := root.WriteFile("identity", []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createRootExclusiveFile(root, "identity", []byte("new"), 0o600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive create collision error = %v", err)
	}
	if body, err := root.ReadFile("identity"); err != nil || string(body) != "existing" {
		t.Fatalf("exclusive create replaced destination: %q, err=%v", body, err)
	}
	entries, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	dirEntries, readErr := entries.ReadDir(-1)
	closeErr := entries.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal(errors.Join(readErr, closeErr))
	}
	for _, entry := range dirEntries {
		if strings.Contains(entry.Name(), ".create-") {
			t.Fatalf("exclusive-create collision retained temporary %s", entry.Name())
		}
	}
}

func TestFailCreatedRootExclusiveFileRemovesOnlyCreatedGeneration(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := root.OpenFile("created", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	created, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("injected create failure")
	if err := failCreatedRootExclusiveFile(root, "created", file, created, cause); !errors.Is(err, cause) {
		t.Fatalf("exclusive-create cleanup error = %v", err)
	}
	if _, err := root.Lstat("created"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed exclusive create remains: %v", err)
	}
}

func TestExclusivePublicationRefusesNamespaceChangesAtDurabilityBoundary(t *testing.T) {
	for _, failure := range []string{"closed-root", "unwritable", "closed-before-rename", "removed-before-rename", "renamed-before-rename", "sync-before", "sync-after"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			original := syncFileLockParent
			defer func() { syncFileLockParent = original }()
			sentinel := errors.New("exclusive publication sync failed")
			calls := 0
			syncFileLockParent = func(parent *os.Root) error {
				calls++
				if failure == "sync-before" && calls == 1 || failure == "sync-after" && calls == 2 {
					return sentinel
				}
				if calls == 1 {
					switch failure {
					case "closed-before-rename":
						return parent.Close()
					case "removed-before-rename", "renamed-before-rename":
						entries, err := os.ReadDir(dir)
						if err != nil {
							return err
						}
						for _, entry := range entries {
							if strings.Contains(entry.Name(), ".create-") {
								if failure == "removed-before-rename" {
									return parent.Remove(entry.Name())
								}
								return parent.Rename(entry.Name(), "retained")
							}
						}
					}
				}
				return original(parent)
			}
			if failure == "closed-root" {
				_ = root.Close()
			}
			if failure == "unwritable" {
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(dir, 0o700) }()
			}
			err = createRootExclusiveFile(root, "identity", []byte("prepared"), 0o600)
			if err == nil {
				t.Fatal("interrupted exclusive publication succeeded")
			}
			if strings.HasPrefix(failure, "sync-") && !errors.Is(err, sentinel) {
				t.Fatalf("sync failure lost: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "identity")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed publication left canonical identity: %v", err)
			}
			if failure == "renamed-before-rename" {
				body, err := os.ReadFile(filepath.Join(dir, "retained"))
				if err != nil || string(body) != "prepared" {
					t.Fatalf("moved generation lost: %q %v", body, err)
				}
			}
		})
	}
}
