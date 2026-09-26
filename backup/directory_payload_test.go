// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBackupRootDirPublishesSealedDigest(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	sourceParent := t.TempDir()
	sourceRel := "reviewed-imports"
	source := filepath.Join(sourceParent, sourceRel)
	mustWriteFile(t, filepath.Join(source, "nested", "guide.md"), "reviewed\n")
	if err := os.Symlink("nested/guide.md", filepath.Join(source, "guide-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sourceRoot, err := os.OpenRoot(sourceParent)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupRootDir(".agents/archive", sourceRoot, sourceRel, "removed-tree")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Directory || entry.SHA256 == "" {
		t.Fatalf("expected sealed directory digest, got %#v", entry)
	}
	digest, err := DirectorySHA256(entry.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if digest != entry.SHA256 {
		t.Fatalf("published directory digest = %s, manifest = %s", digest, entry.SHA256)
	}
	if body, err := os.ReadFile(filepath.Join(entry.BackupPath, "nested", "guide.md")); err != nil || string(body) != "reviewed\n" {
		t.Fatalf("sealed payload body = %q, err=%v", body, err)
	}
	if target, err := os.Readlink(filepath.Join(entry.BackupPath, "guide-link")); err != nil || target != "nested/guide.md" {
		t.Fatalf("sealed payload symlink = %q, err=%v", target, err)
	}
	mustWriteFile(t, filepath.Join(source, "nested", "guide.md"), "changed later\n")
	if digestAfter, err := DirectorySHA256(entry.BackupPath); err != nil || digestAfter != entry.SHA256 {
		t.Fatalf("published payload changed with source, digest=%s err=%v", digestAfter, err)
	}
}

func TestBackupRootDirReplacementUsesContentIdentityAndPreservesPriorPayload(t *testing.T) {
	root := t.TempDir()
	sourcePath := t.TempDir()
	guide := filepath.Join(sourcePath, "guide.md")
	mustWriteFile(t, guide, "first")
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: t.TempDir(), Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	first, err := store.BackupRootDir(".agents/archive", source, ".", "removed-tree")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, guide, "second")
	second, err := store.BackupRootDir(".agents/archive", source, ".", "removed-tree")
	if err != nil {
		t.Fatal(err)
	}
	if first.BackupPath == second.BackupPath || first.SHA256 == second.SHA256 {
		t.Fatalf("changed directory reused content identity: first=%#v second=%#v", first, second)
	}
	if body, err := os.ReadFile(filepath.Join(first.BackupPath, "guide.md")); err != nil || string(body) != "first" {
		t.Fatalf("prior directory payload changed, body=%q err=%v", body, err)
	}
	manifest, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].BackupPath != second.BackupPath || manifest.Entries[0].SHA256 != second.SHA256 {
		t.Fatalf("directory replacement manifest = %#v", manifest.Entries)
	}

	persistedBefore := manifest
	mustWriteFile(t, guide, "third")
	wantErr := errors.New("injected directory manifest save failure")
	store.beforeSave = func() error { return wantErr }
	if _, err := store.BackupRootDir(".agents/archive", source, ".", "removed-tree"); !errors.Is(err, wantErr) {
		t.Fatalf("directory replacement save error = %v, want %v", err, wantErr)
	}
	store.beforeSave = nil
	persistedAfter, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persistedAfter.Entries, persistedBefore.Entries) || !reflect.DeepEqual(store.Manifest.Entries, persistedBefore.Entries) {
		t.Fatalf("failed directory replacement changed manifest: persisted=%#v memory=%#v", persistedAfter.Entries, store.Manifest.Entries)
	}
	if body, err := os.ReadFile(filepath.Join(second.BackupPath, "guide.md")); err != nil || string(body) != "second" {
		t.Fatalf("failed directory replacement changed prior payload, body=%q err=%v", body, err)
	}
}

func TestBackupRootDirRejectsContentIdentityCollisionWithoutReplacement(t *testing.T) {
	root := t.TempDir()
	sourcePath := t.TempDir()
	mustWriteFile(t, filepath.Join(sourcePath, "guide.md"), "reviewed")
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: t.TempDir(), Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	digest, err := RootDirSHA256(source, ".")
	if err != nil {
		t.Fatal(err)
	}
	rel := backupPayloadRelWithIdentity("removed-tree", ".agents/archive", digest)
	collision := filepath.Join(store.RunDir, "dirs", rel)
	mustWriteFile(t, filepath.Join(collision, "attacker.md"), "collision")
	if _, err := store.BackupRootDir(".agents/archive", source, ".", "removed-tree"); err == nil || !strings.Contains(err.Error(), "content-addressed backup directory collision") {
		t.Fatalf("expected content identity collision refusal, got %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(collision, "attacker.md")); err != nil || string(body) != "collision" {
		t.Fatalf("collision payload was replaced, body=%q err=%v", body, err)
	}
	if len(store.Manifest.Entries) != 0 {
		t.Fatalf("collision published a manifest entry: %#v", store.Manifest.Entries)
	}
}

func TestBackupRootDirAllocatesThroughPinnedParentWhenPathIsReplaced(t *testing.T) {
	root := t.TempDir()
	sourcePath := t.TempDir()
	mustWriteFile(t, filepath.Join(sourcePath, "guide.md"), "reviewed")
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: t.TempDir(), Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	outside := t.TempDir()
	var movedParent string
	_, err = store.backupRootDirWithLimitsAndHooks(".agents/archive", source, ".", "removed-tree", DirectoryLimits{}, backupRootDirHooks{
		afterParentOpen: func(parent string) error {
			movedParent = parent + "-reviewed"
			if err := os.Rename(parent, movedParent); err != nil {
				return err
			}
			return os.Symlink(outside, parent)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "payload parent changed") {
		t.Fatalf("expected replaced payload-parent refusal, got %v", err)
	}
	if movedParent == "" {
		t.Fatal("parent replacement hook did not run")
	}
	outsideEntries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(outsideEntries) != 0 {
		t.Fatalf("private backup allocation escaped through replaced parent: %#v", outsideEntries)
	}
	if len(store.Manifest.Entries) != 0 {
		t.Fatalf("replaced parent published a manifest entry: %#v", store.Manifest.Entries)
	}
}

func TestBackupRootDirWithLimitsBoundsCopyAndDigest(t *testing.T) {
	tests := []struct {
		name       string
		populate   func(*testing.T, string)
		limits     DirectoryLimits
		want       string
		digestWant string
	}{
		{
			name: "filesystem entries",
			populate: func(t *testing.T, source string) {
				t.Helper()
				mustWriteFile(t, filepath.Join(source, "one.md"), "1")
				mustWriteFile(t, filepath.Join(source, "two.md"), "2")
			},
			limits:     DirectoryLimits{MaxEntries: 2, MaxBytes: 100, MaxFileBytes: 100},
			want:       "exceeds 2 filesystem entries",
			digestWant: "exceeds 2 filesystem entries",
		},
		{
			name: "aggregate bytes",
			populate: func(t *testing.T, source string) {
				t.Helper()
				mustWriteFile(t, filepath.Join(source, "one.md"), "12")
				mustWriteFile(t, filepath.Join(source, "two.md"), "34")
			},
			limits:     DirectoryLimits{MaxEntries: 10, MaxBytes: 3, MaxFileBytes: 10},
			want:       "exceeds 3 bytes of regular-file content",
			digestWant: "exceeds 3 bytes of regular-file content",
		},
		{
			name: "per-file bytes",
			populate: func(t *testing.T, source string) {
				t.Helper()
				mustWriteFile(t, filepath.Join(source, "large.md"), "1234")
			},
			limits:     DirectoryLimits{MaxEntries: 10, MaxBytes: 100, MaxFileBytes: 3},
			want:       "file exceeds 3 bytes",
			digestWant: "file exceeds 3 bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			source := filepath.Join(t.TempDir(), "source")
			test.populate(t, source)
			sourceRoot, err := os.OpenRoot(source)
			if err != nil {
				t.Fatal(err)
			}
			defer sourceRoot.Close()
			store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			_, err = store.BackupRootDirWithLimits(".agents/archive", sourceRoot, ".", "removed-tree", test.limits)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("bounded backup error = %v, want %q", err, test.want)
			}
			if len(store.Manifest.Entries) != 0 {
				t.Fatalf("failed bounded backup published a manifest entry: %#v", store.Manifest.Entries)
			}
			if err := filepath.WalkDir(store.RunDir, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					if errors.Is(walkErr, os.ErrNotExist) {
						return nil
					}
					return walkErr
				}
				if !entry.IsDir() {
					t.Fatalf("failed bounded backup retained payload entry %s", path)
				}
				return nil
			}); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			_, err = DirectorySHA256WithLimits(source, test.limits)
			if err == nil || !strings.Contains(err.Error(), test.digestWant) {
				t.Fatalf("bounded digest error = %v, want %q", err, test.digestWant)
			}
		})
	}
}

func TestBackupRootDirRejectsEarlierChildMutationFromLaterSibling(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	sourcePath := t.TempDir()
	earlier := filepath.Join(sourcePath, "a", "first.md")
	mustWriteFile(t, earlier, "reviewed")
	mustWriteFile(t, filepath.Join(sourcePath, "z.md"), "later")
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	mutated := false
	_, err = store.backupRootDirWithLimitsAndHooks(".agents/archive", source, ".", "removed-tree", DirectoryLimits{
		MaxEntries:       10,
		MaxBytes:         1024,
		MaxFileBytes:     1024,
		MaxDepth:         4,
		MaxPathBytes:     4096,
		MaxMetadataBytes: 4096,
	}, backupRootDirHooks{
		afterCopyEntry: func(walkPath string) error {
			if walkPath != "z.md" || mutated {
				return nil
			}
			mutated = true
			file, err := source.OpenFile(filepath.FromSlash("a/first.md"), os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				return err
			}
			if _, err := file.WriteString("mutated!"); err != nil {
				return errors.Join(err, file.Close())
			}
			return file.Close()
		},
	})
	if err == nil || !strings.Contains(err.Error(), "source changed while its sealed directory payload was created") {
		t.Fatalf("cross-sibling mutation error = %v", err)
	}
	if !mutated {
		t.Fatal("later-sibling copy hook did not mutate the earlier child")
	}
	if len(store.Manifest.Entries) != 0 {
		t.Fatalf("unstable source published a backup entry: %#v", store.Manifest.Entries)
	}
	if body, err := os.ReadFile(earlier); err != nil || string(body) != "mutated!" {
		t.Fatalf("earlier-child mutation did not occur, body=%q err=%v", body, err)
	}
}

func TestBackupRootDirRehashesPrivatePayloadAfterSourceDigest(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	sourcePath := t.TempDir()
	mustWriteFile(t, filepath.Join(sourcePath, "guide.md"), "reviewed")
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	mutated := false
	_, err = store.backupRootDirWithLimitsAndHooks(".agents/archive", source, ".", "removed-tree", DirectoryLimits{
		MaxEntries:       4,
		MaxBytes:         1024,
		MaxFileBytes:     1024,
		MaxDepth:         2,
		MaxPathBytes:     1024,
		MaxMetadataBytes: 2048,
	}, backupRootDirHooks{
		afterSourceDigest: func(temporary *os.Root) error {
			mutated = true
			return temporary.WriteFile("guide.md", []byte("tampered"), 0o644)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "private backup payload changed before publication") {
		t.Fatalf("private-payload mutation error = %v", err)
	}
	if !mutated {
		t.Fatal("after-source-digest mutation hook was not called")
	}
	if len(store.Manifest.Entries) != 0 {
		t.Fatalf("tampered private payload published an entry: %#v", store.Manifest.Entries)
	}
}

func TestRootDirSHA256RequiresMatchingWholeTreePasses(t *testing.T) {
	directoryPath := t.TempDir()
	filePath := filepath.Join(directoryPath, "a.md")
	mustWriteFile(t, filePath, "first")
	root, err := os.OpenRoot(directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, err = rootDirSHA256WithHooks(root, ".", DirectoryLimits{
		MaxEntries:       4,
		MaxBytes:         1024,
		MaxFileBytes:     1024,
		MaxDepth:         2,
		MaxPathBytes:     1024,
		MaxMetadataBytes: 2048,
	}, directoryDigestHooks{
		afterFirstPass: func() error {
			file, err := root.OpenFile("a.md", os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				return err
			}
			if _, err := file.WriteString("second"); err != nil {
				return errors.Join(err, file.Close())
			}
			return file.Close()
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed between bounded digest passes") {
		t.Fatalf("two-pass digest error = %v", err)
	}
}

func TestCopyDirPreservesNestedFilesAndSymlinks(t *testing.T) {
	root := t.TempDir()

	source := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(source, []byte("guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "nested", "item.txt"), []byte("item"), 0o644); err != nil {
		t.Fatal(err)
	}
	directoryTarget := filepath.Join(t.TempDir(), "copy")
	if err := CopyDir(directory, directoryTarget); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(directoryTarget, "nested", "item.txt")); err != nil || string(body) != "item" {
		t.Fatalf("copied directory file = %q, %v", body, err)
	}
	if err := os.Symlink(source, filepath.Join(directory, "link")); err == nil {
		symlinkTarget := filepath.Join(t.TempDir(), "copy-link")
		if err := CopyDir(directory, symlinkTarget); err != nil {
			t.Fatal(err)
		}
		if target, err := os.Readlink(filepath.Join(symlinkTarget, "link")); err != nil || target != source {
			t.Fatalf("copied symlink = %q, %v", target, err)
		}
	}
}

func TestRootedDirectoryBackupCleansPrivatePayloadAfterInjectedFailure(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = homeRoot.Close() }()
	sourcePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourcePath, "payload.txt"), []byte("payload\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceRoot.Close() }()

	wantErr := errors.New("injected rooted backup failure")
	tests := []struct {
		name  string
		runID string
		hooks backupRootDirHooks
	}{
		{
			name:  "after parent open",
			runID: "rooted-hook-failure-parent",
			hooks: backupRootDirHooks{afterParentOpen: func(string) error {
				return wantErr
			}},
		},
		{
			name:  "after copied file",
			runID: "rooted-hook-failure-copy",
			hooks: backupRootDirHooks{afterCopyEntry: func(sourcePath string) error {
				if sourcePath == "payload.txt" {
					return wantErr
				}
				return nil
			}},
		},
		{
			name:  "after source digest",
			runID: "rooted-hook-failure-digest",
			hooks: backupRootDirHooks{afterSourceDigest: func(*os.Root) error {
				return wantErr
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewStoreFromRoot(homeRoot, home, StoreOptions{
				ProjectRoot: project,
				Operation:   "fixture",
				RunID:       test.runID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			_, err = store.backupRootDirWithLimitsAndHooks(
				".agents/archive", sourceRoot, ".", "removed-tree", DirectoryLimits{}, test.hooks,
			)
			if !errors.Is(err, wantErr) {
				t.Fatalf("rooted backup error = %v, want %v", err, wantErr)
			}
			if len(store.Manifest.Entries) != 0 {
				t.Fatalf("failed rooted backup published entries: %#v", store.Manifest.Entries)
			}
			walkErr := filepath.WalkDir(store.RunDir, func(path string, _ os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if strings.Contains(filepath.Base(path), ".tmp-") {
					t.Fatalf("failed rooted backup retained private payload %s", path)
				}
				return nil
			})
			if walkErr != nil && !errors.Is(walkErr, os.ErrNotExist) {
				t.Fatal(walkErr)
			}
		})
	}
}

func TestRootedDirectoryBackupRejectsInvalidSourcesAndReusesExactPayload(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = homeRoot.Close() }()
	store, err := NewStoreFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project,
		Operation:   "fixture",
		RunID:       "rooted-source-validation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	sourcePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourcePath, "payload.txt"), []byte("payload\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(sourcePath, "directory-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sourceRoot, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceRoot.Close() }()

	for _, test := range []struct {
		name      string
		source    *os.Root
		sourceRel string
		limits    DirectoryLimits
	}{
		{name: "nil root", source: nil, sourceRel: "."},
		{name: "escaping path", source: sourceRoot, sourceRel: "../outside"},
		{name: "regular file", source: sourceRoot, sourceRel: "payload.txt"},
		{name: "symlink directory", source: sourceRoot, sourceRel: "directory-link"},
		{name: "negative limit", source: sourceRoot, sourceRel: ".", limits: DirectoryLimits{MaxEntries: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.BackupRootDirWithLimits(".agents/archive", test.source, test.sourceRel, "removed-tree", test.limits); err == nil {
				t.Fatal("invalid rooted backup source unexpectedly succeeded")
			}
		})
	}

	first, err := store.BackupRootDir(".agents/archive", sourceRoot, ".", "removed-tree")
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Lstat(first.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.BackupRootDir(".agents/archive", sourceRoot, ".", "removed-tree")
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Lstat(second.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if first.BackupPath != second.BackupPath || first.SHA256 != second.SHA256 || !os.SameFile(firstInfo, secondInfo) {
		t.Fatalf("exact rooted payload was replaced instead of reused: first=%#v second=%#v", first, second)
	}
	if len(store.Manifest.Entries) != 1 {
		t.Fatalf("exact rooted payload retry duplicated manifest entries: %#v", store.Manifest.Entries)
	}
}

func TestDirectoryBackupFailureDoesNotPublishManifestEntry(t *testing.T) {
	for _, failure := range []string{"parent-hook", "copy-hook", "digest-hook", "closed-source", "closed-temporary", "changed-source-mode", "temporary-moved"} {
		t.Run(failure, func(t *testing.T) {
			project, home, sourcePath := t.TempDir(), t.TempDir(), t.TempDir()
			mustWriteFile(t, filepath.Join(sourcePath, "guide.md"), "original")
			source, err := os.OpenRoot(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			store, err := NewStore(StoreOptions{ProjectRoot: project, ThreadpointHome: home, Operation: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			sentinel := errors.New("backup boundary failed")
			hooks := backupRootDirHooks{}
			switch failure {
			case "parent-hook":
				hooks.afterParentOpen = func(string) error { return sentinel }
			case "copy-hook":
				hooks.afterCopyEntry = func(string) error { return sentinel }
			case "digest-hook":
				hooks.afterSourceDigest = func(*os.Root) error { return sentinel }
			case "closed-source":
				hooks.afterCopyEntry = func(string) error { return source.Close() }
			case "closed-temporary":
				hooks.afterSourceDigest = func(root *os.Root) error { return root.Close() }
			case "changed-source-mode":
				hooks.afterCopyEntry = func(string) error { return os.Chmod(filepath.Join(sourcePath, "guide.md"), 0o600) }
			case "temporary-moved":
				hooks.afterSourceDigest = func(root *os.Root) error { return os.Rename(root.Name(), root.Name()+"-retained") }
			}
			entry, err := store.backupRootDirWithLimitsAndHooks("native", source, ".", "fixture", DirectoryLimits{}, hooks)
			if err == nil || entry != nil {
				t.Fatalf("failed backup published %+v %v", entry, err)
			}
			if strings.HasSuffix(failure, "hook") && !errors.Is(err, sentinel) {
				t.Fatalf("lost hook error: %v", err)
			}
			if len(store.Manifest.Entries) != 0 {
				t.Fatalf("failed payload registered: %+v", store.Manifest.Entries)
			}
			if _, err := os.Lstat(filepath.Join(store.RunDir, ManifestName)); !os.IsNotExist(err) {
				t.Fatalf("failed generation published manifest: %v", err)
			}
			body, err := os.ReadFile(filepath.Join(sourcePath, "guide.md"))
			if err != nil || string(body) != "original" {
				t.Fatalf("failed backup changed source: %q %v", body, err)
			}
		})
	}
}

func TestDirectoryBackupRejectsInvalidPayloadNamespaces(t *testing.T) {
	for _, rooted := range []bool{false, true} {
		for _, invalid := range []string{"entry-escape", "source-escape", "missing-source", "nil-source", "file-namespace", "symlink-namespace"} {
			t.Run(fmt.Sprintf("rooted=%v/%s", rooted, invalid), func(t *testing.T) {
				project, home := t.TempDir(), t.TempDir()
				homeRoot, err := os.OpenRoot(home)
				if err != nil {
					t.Fatal(err)
				}
				defer homeRoot.Close()
				opts := StoreOptions{ProjectRoot: project, ThreadpointHome: home, Operation: "fixture"}
				var store *Store
				if rooted {
					store, err = NewStoreFromRoot(homeRoot, home, opts)
				} else {
					store, err = NewStore(opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if _, err := store.BackupBytes("existing.md", []byte("existing"), "fixture"); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(filepath.Join(store.RunDir, ManifestName))
				if err != nil {
					t.Fatal(err)
				}
				source, err := os.OpenRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer source.Close()
				rel, sourceRel := "native", "."
				switch invalid {
				case "entry-escape":
					rel = "../escape"
				case "source-escape":
					sourceRel = "../escape"
				case "missing-source":
					sourceRel = "missing"
				case "nil-source":
					source = nil
				case "file-namespace":
					err = os.WriteFile(filepath.Join(store.RunDir, "dirs"), []byte("preserve"), 0o600)
				case "symlink-namespace":
					err = os.Symlink(t.TempDir(), filepath.Join(store.RunDir, "dirs"))
				}
				if err != nil {
					t.Fatal(err)
				}
				if entry, err := store.BackupRootDir(rel, source, sourceRel, "fixture"); err == nil || entry != nil {
					t.Fatalf("unsafe directory backup=%+v %v", entry, err)
				}
				after, err := os.ReadFile(filepath.Join(store.RunDir, ManifestName))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("refusal changed manifest: %v", err)
				}
			})
		}
	}
}

func TestPrivateNamespaceAllocationBoundsCollisions(t *testing.T) {
	for _, allocator := range []struct {
		name     string
		allocate func(*os.Root) (string, error)
	}{

		{"directory", func(root *os.Root) (string, error) { return makeBackupTempDirectory(root, ".prepared-") }},
	} {
		t.Run(allocator.name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			original := rand.Reader
			defer func() { rand.Reader = original }()
			reset := func() { rand.Reader = bytes.NewReader(bytes.Repeat([]byte{0x2a}, 4096)) }
			reset()
			name, err := allocator.allocate(root)
			if err != nil {
				t.Fatal(err)
			}
			marker := name
			if info, err := root.Lstat(name); err == nil && info.IsDir() {
				marker = filepath.Join(name, "sentinel")
			}
			if err := root.WriteFile(marker, []byte("retained"), 0o600); err != nil {
				t.Fatal(err)
			}
			reset()
			if got, err := allocator.allocate(root); err == nil || got != "" {
				t.Fatalf("colliding name accepted: %q %v", got, err)
			}
			if body, err := root.ReadFile(marker); err != nil || string(body) != "retained" {
				t.Fatalf("collision destroyed prior entry: %q %v", body, err)
			}
			if err := root.RemoveAll(name); err != nil {
				t.Fatal(err)
			}
			reset()
			if got, err := allocator.allocate(root); err != nil || got != name {
				t.Fatalf("allocation did not recover: %q %v", got, err)
			}
			_ = root.Close()
			reset()
			if _, err := allocator.allocate(root); err == nil {
				t.Fatal("closed namespace accepted")
			}
		})
	}
}

func TestRegularPayloadCopyRejectsChangedSourcesAndUnavailableTargets(t *testing.T) {
	for _, failure := range []string{"changed-identity", "shrunk-source", "grown-source", "closed-source", "missing-source", "unreadable-source", "closed-target", "occupied-target", "blocked-parent", "unwritable-target"} {
		t.Run(failure, func(t *testing.T) {
			sourceDir, targetDir := t.TempDir(), t.TempDir()
			source, err := os.OpenRoot(sourceDir)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			target, err := os.OpenRoot(targetDir)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			if err := source.WriteFile("payload", []byte("reviewed"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := source.Lstat("payload")
			if err != nil {
				t.Fatal(err)
			}
			targetName := "payload"
			switch failure {
			case "changed-identity":
				if err := source.Rename("payload", "retained"); err != nil {
					t.Fatal(err)
				}
				if err := source.WriteFile("payload", []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "shrunk-source", "grown-source":
				body := "x"
				if failure == "grown-source" {
					body = "much longer than reviewed"
				}
				if err := source.WriteFile("payload", []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			case "closed-source":
				_ = source.Close()
			case "missing-source":
				if err := source.Rename("payload", "retained"); err != nil {
					t.Fatal(err)
				}
			case "unreadable-source":
				if err := source.Chmod("payload", 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(filepath.Join(sourceDir, "payload"), 0o600) }()
			case "closed-target":
				_ = target.Close()
			case "occupied-target":
				if err := target.WriteFile("payload", []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "blocked-parent":
				targetName = "blocked/payload"
				if err := target.WriteFile("blocked", []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unwritable-target":
				if err := os.Chmod(targetDir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(targetDir, 0o700) }()
			}
			if _, err := copyRootRegularFile(source, "payload", info, target, targetName, -1); err == nil {
				t.Fatal("unsafe payload copy succeeded")
			}
			if failure == "occupied-target" || failure == "blocked-parent" {
				name := "payload"
				if failure == "blocked-parent" {
					name = "blocked"
				}
				body, err := os.ReadFile(filepath.Join(targetDir, name))
				if err != nil || string(body) != "foreign" {
					t.Fatalf("concurrent target changed: %q %v", body, err)
				}
			} else if _, err := os.Lstat(filepath.Join(targetDir, targetName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed copy left payload: %v", err)
			}
		})
	}
}

func TestDirectoryHashAndBackupRejectUnsafeSources(t *testing.T) {
	for _, operation := range []string{"hash", "backup"} {
		for _, failure := range []string{"missing", "regular-file", "symlink", "unreadable", "blocked-ancestor", "symlink-ancestor", "invalid-limits"} {
			if operation == "backup" && (failure == "invalid-limits" || failure == "symlink-ancestor") {
				continue
			}
			t.Run(operation+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				source := filepath.Join(dir, "source")
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
				limits := DirectoryLimits{}
				switch failure {
				case "missing":
					source = filepath.Join(dir, "missing")
				case "regular-file":
					source = filepath.Join(dir, "file")
					if err := os.WriteFile(source, []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Remove(source); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(t.TempDir(), source); err != nil {
						t.Fatal(err)
					}
				case "unreadable":
					if err := os.Chmod(source, 0); err != nil {
						t.Fatal(err)
					}
					defer func() { _ = os.Chmod(source, 0o700) }()
				case "blocked-ancestor":
					if err := os.WriteFile(filepath.Join(dir, "blocked"), []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
					source = filepath.Join(dir, "blocked", "source")
				case "symlink-ancestor":
					if err := os.Symlink(dir, filepath.Join(dir, "link")); err != nil {
						t.Fatal(err)
					}
					source = filepath.Join(dir, "link", "source")
				case "invalid-limits":
					limits.MaxDepth = -1
				}
				var err error
				if operation == "hash" {
					_, err = DirectorySHA256WithLimits(source, limits)
				} else {
					store, storeErr := NewStore(StoreOptions{ProjectRoot: t.TempDir(), HomeDir: t.TempDir(), Operation: "fixture"})
					if storeErr != nil {
						t.Fatal(storeErr)
					}
					if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
					_, err = store.BackupDir("source", source, "fixture")
					if len(store.Manifest.Entries) != 0 {
						t.Fatal("rejected backup recorded an entry")
					}
				}
				if err == nil {
					t.Fatal("unsafe directory source accepted")
				}
			})
		}
	}
}
