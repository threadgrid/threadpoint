// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecordAbsentRejectsBackupSymlinkAncestorBeforeCreatingOutside(t *testing.T) {
	t.Setenv("THREADPOINT_HOME", "")
	root := t.TempDir()
	home := t.TempDir()
	outside := t.TempDir()
	threadpointHome := filepath.Join(home, ".threadpoint")
	if err := os.MkdirAll(threadpointHome, 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, filepath.Join(threadpointHome, "backups")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := store.RecordAbsent("AGENTS.md", "fixture-target"); err == nil {
		t.Fatal("expected backup symlink ancestor to be rejected")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("backup write created content outside the backup root: %#v", entries)
	}
}

func TestRecordAbsentPersistsRestorablePreRunState(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.RecordAbsent("AGENTS.md", "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Absent || entry.BackupPath != "" {
		t.Fatalf("unexpected absent-target entry: %#v", entry)
	}
	manifest, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || !manifest.Entries[0].Absent {
		t.Fatalf("absent-target entry was not persisted: %#v", manifest.Entries)
	}
}

func TestClearRecoveryPathPersistsWithoutDiscardingEntry(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupBytes(".agents/archive", []byte("sealed"), "removed-tree")
	if err != nil {
		t.Fatal(err)
	}
	recovery := filepath.Join(root, ".agents", ".threadpoint-imports-recovery-test")
	if err := store.SetRecoveryPath(entry.Path, entry.Role, recovery); err != nil {
		t.Fatal(err)
	}
	journaled, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if len(journaled.Entries) != 1 || journaled.Entries[0].RecoveryPath != entry.BackupPath || journaled.Entries[0].RecoveryPath == recovery {
		t.Fatalf("recovery intent did not retain only the run-local payload: %#v", journaled.Entries)
	}
	if err := store.ClearRecoveryPath(entry.Path, entry.Role); err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].RecoveryPath != "" || manifest.Entries[0].BackupPath != entry.BackupPath {
		t.Fatalf("cleared recovery path changed the backup entry: %#v", manifest.Entries)
	}
}

func TestDiscardRemovesTentativeEntryAndPayload(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "AGENTS.md")
	directory := filepath.Join(root, ".agents", "imports")
	mustWriteFile(t, source, "original")
	mustWriteFile(t, filepath.Join(directory, "staged.md"), "staged")
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("AGENTS.md", source, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	tentative, err := store.BackupDir(".agents/archive", directory, "removed-tree")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Discard("removed-tree", ".agents/archive"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tentative.BackupPath); !os.IsNotExist(err) {
		t.Fatalf("tentative payload should be removed, err=%v", err)
	}
	manifest, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].Role != "fixture-target" {
		t.Fatalf("discard should preserve unrelated backup entries: %#v", manifest.Entries)
	}
	if err := store.Discard("fixture-target", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(store.RunDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarding the final entry retained a canonical empty run: %v", err)
	}
	runs, err := ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("empty discard tombstone was exposed as a restore run: %#v", runs)
	}
}

func TestDiscardFinalEntrySaveFailurePreservesManifestPayloadAndMemory(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: t.TempDir(), Operation: "restore"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupBytes("AGENTS.md", []byte("original"), "restore-overwrite")
	if err != nil {
		t.Fatal(err)
	}
	persistedBefore, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected discard manifest save failure")
	store.beforeSave = func() error { return wantErr }
	if err := store.Discard("restore-overwrite", "AGENTS.md"); !errors.Is(err, wantErr) {
		t.Fatalf("discard error = %v, want %v", err, wantErr)
	}
	store.beforeSave = nil
	persistedAfter, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persistedAfter.Entries, persistedBefore.Entries) || !reflect.DeepEqual(store.Manifest.Entries, persistedBefore.Entries) {
		t.Fatalf("failed final discard changed manifest: persisted=%#v memory=%#v", persistedAfter.Entries, store.Manifest.Entries)
	}
	if body, err := os.ReadFile(entry.BackupPath); err != nil || string(body) != "original" {
		t.Fatalf("failed final discard changed payload, body=%q err=%v", body, err)
	}
}

func TestDifferentRolesUseIndependentPayloadsAndDiscardPreservesTheOther(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, source, "shared original")
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	first, err := store.BackupFile("AGENTS.md", source, "stage-source")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, source, "updated for second role")
	second, err := store.BackupFile("AGENTS.md", source, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if first.BackupPath == second.BackupPath {
		t.Fatalf("different roles must not share a mutable payload: %q", first.BackupPath)
	}
	if body, err := os.ReadFile(first.BackupPath); err != nil || string(body) != "shared original" {
		t.Fatalf("first role payload was overwritten, body=%q err=%v", body, err)
	}
	if body, err := os.ReadFile(second.BackupPath); err != nil || string(body) != "updated for second role" {
		t.Fatalf("second role payload body=%q err=%v", body, err)
	}
	if err := store.Discard("fixture-target", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(first.BackupPath); err != nil || string(body) != "shared original" {
		t.Fatalf("remaining entry lost its shared payload, body=%q err=%v", body, err)
	}
	manifest, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].Role != "stage-source" {
		t.Fatalf("unexpected remaining entries: %#v", manifest.Entries)
	}
}

func TestDiscardPreservesPayloadReferencedByAnotherRole(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: t.TempDir(), Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	first, err := store.BackupBytes("AGENTS.md", []byte("previous shared payload"), "stage-source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackupBytes("AGENTS.md", []byte("second payload"), "fixture-target"); err != nil {
		t.Fatal(err)
	}
	// Simulate an older manifest whose distinct role entries intentionally
	// referenced the same payload path.
	store.Manifest.Entries[1].BackupPath = first.BackupPath
	store.Manifest.Entries[1].SHA256 = first.SHA256
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Discard("fixture-target", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(first.BackupPath); err != nil || string(body) != "previous shared payload" {
		t.Fatalf("remaining previous entry lost its shared payload, body=%q err=%v", body, err)
	}
}

func TestRecordAbsentFromRootRejectsReplacementManifestGeneration(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	source := []byte("native source\n")
	options := StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: "fixture", RunID: "absent-cas"}
	store, err := NewStoreFromRoot(homeRoot, home, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGenerationFromRoot(homeRoot, home); !os.IsNotExist(err) {
		t.Fatalf("fresh manifest load = %v, want missing", err)
	}
	if _, err := store.BackupBytesWithModeFromRoot(homeRoot, home, "native.md", source, "fixture", 0o600); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(store.RunDir, ManifestName)
	replacementManifest := store.Manifest
	replacementManifest.Operation = "concurrent-replacement"
	replacement, err := marshalManifest(replacementManifest)
	if err != nil {
		t.Fatal(err)
	}
	store.beforeSave = func() error {
		store.beforeSave = nil
		temporary := manifestPath + ".replacement"
		if err := os.WriteFile(temporary, replacement, 0o600); err != nil {
			return err
		}
		return os.Rename(temporary, manifestPath)
	}

	if _, err := store.RecordAbsentFromRoot(homeRoot, home, "AGENTS.md", "fixture-target"); err == nil {
		t.Fatal("record-absent replaced a concurrent manifest")
	}

	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(replacement) {
		t.Fatalf("prior-absent save overwrote replacement manifest:\n%s", body)
	}
	for _, entry := range store.Manifest.Entries {
		if entry.Role == "fixture-target" {
			t.Fatalf("failed CAS retained an unsaved prior marker in memory: %+v", store.Manifest)
		}
	}
}

func TestByteBackupsRecordPostHashesAndDiscardTheirSealedPayload(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	store, err := NewStore(StoreOptions{ProjectRoot: project, HomeDir: home, Operation: "fixture", Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupBytesWithMode("generated/file.txt", []byte("original"), "fixture", 0o640)
	if err != nil || !entry.ModeKnown || entry.Mode != 0o640 {
		t.Fatalf("byte backup = %#v, %v", entry, err)
	}
	created := filepath.Join(project, "generated", "file.txt")
	if err := os.MkdirAll(filepath.Dir(created), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("created"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPostHash("generated/file.txt", created); err != nil {
		t.Fatal(err)
	}
	if store.Manifest.Entries[0].PostSHA256 == "" {
		t.Fatal("post-change hash was not stored")
	}
	if _, err := FileSHA256(filepath.Join(project, "missing")); err == nil {
		t.Fatal("hashing a missing file unexpectedly succeeded")
	}
	payload := entry.BackupPath
	if err := store.Discard("fixture", "generated/file.txt"); err != nil {
		t.Fatal(err)
	}
	if len(store.Manifest.Entries) != 0 {
		t.Fatalf("discarded entries = %#v", store.Manifest.Entries)
	}
	if _, err := os.Lstat(payload); !os.IsNotExist(err) {
		t.Fatalf("discarded payload remains: %v", err)
	}
	if err := store.Discard("fixture", "missing.txt"); err != nil {
		t.Fatalf("discard missing entry: %v", err)
	}
}

func TestBorrowedStoreDiscardRemovesExactDirectoryPayload(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = homeRoot.Close() }()
	source := filepath.Join(project, "source-tree")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "payload.txt"), []byte("payload\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := NewStoreFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home, Operation: "fixture", RunID: "rooted-directory-discard",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupDir(".agents/archive", source, "removed-tree")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(entry.BackupPath); err != nil || !info.IsDir() {
		t.Fatalf("rooted directory backup = %v, %v", info, err)
	}
	if err := store.Discard("removed-tree", ".agents/archive"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(entry.BackupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded rooted directory payload remains: %v", err)
	}
	runRel, err := rootRelativePath(home, store.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := homeRoot.Lstat(runRel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rooted final discard retained a canonical empty run: %v", err)
	}
}

func TestRootedDiscardRefusesReplacementAndSpecialPayloads(t *testing.T) {
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = homeRoot.Close() }()
	if err := os.Mkdir(filepath.Join(home, "payloads"), 0o700); err != nil {
		t.Fatal(err)
	}

	regular := filepath.Join(home, "payloads", "regular")
	if err := os.WriteFile(regular, []byte("reviewed"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Lstat(regular)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(regular); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regular, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeDiscardedBackupPayloadRoot(homeRoot, home, regular, expected); err == nil || !strings.Contains(err.Error(), "changed identity") {
		t.Fatalf("replacement discard error = %v", err)
	}
	if body, err := os.ReadFile(regular); err != nil || string(body) != "replacement" {
		t.Fatalf("replacement payload was removed: body=%q err=%v", body, err)
	}

	link := filepath.Join(home, "payloads", "link")
	if err := os.Symlink("regular", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeDiscardedBackupPayloadRoot(homeRoot, home, link, linkInfo); err == nil || !strings.Contains(err.Error(), "not a regular file or real directory") {
		t.Fatalf("special payload discard error = %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("refused special payload was removed: %v", err)
	}

	if err := removeDiscardedBackupPayloadRoot(homeRoot, home, regular, nil); err != nil {
		t.Fatalf("missing rooted payload cleanup = %v", err)
	}
	outside := filepath.Join(filepath.Dir(home), "outside")
	if err := removeDiscardedBackupPayloadRoot(homeRoot, home, outside, expected); err == nil {
		t.Fatal("rooted payload cleanup accepted a path outside product home")
	}
}

func TestDiscardedPayloadCleanupRequiresExactLiveIdentity(t *testing.T) {
	for _, rooted := range []bool{false, true} {
		for _, change := range []string{"missing", "replaced", "symlink", "missing-parent", "parent-symlink", "closed-root"} {
			if !rooted && change == "closed-root" {
				continue
			}
			t.Run(fmt.Sprintf("rooted=%v/%s", rooted, change), func(t *testing.T) {
				home := t.TempDir()
				dir := filepath.Join(home, "payloads")
				path := filepath.Join(dir, "payload")
				mustWriteFile(t, path, "original")
				expected, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				root, err := os.OpenRoot(home)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				switch change {
				case "missing":
					err = os.Remove(path)
				case "replaced":
					if err = os.Rename(path, path+"-retained"); err == nil {
						err = os.WriteFile(path, []byte("concurrent"), 0o600)
					}
				case "symlink":
					if err = os.Remove(path); err == nil {
						err = os.Symlink("elsewhere", path)
					}
					if err == nil {
						expected, err = os.Lstat(path)
					}
				case "missing-parent":
					err = os.Rename(dir, dir+"-retained")
				case "parent-symlink":
					if err = os.Rename(dir, dir+"-retained"); err == nil {
						err = os.Symlink(dir+"-retained", dir)
					}
				case "closed-root":
					err = root.Close()
				}
				if err != nil {
					t.Fatal(err)
				}
				if rooted {
					err = removeDiscardedBackupPayloadRoot(root, home, path, expected)
				} else {
					err = removeDiscardedBackupPayload(path, expected)
				}
				if err == nil {
					t.Fatal("unverifiable cleanup succeeded")
				}
				if change == "replaced" {
					body, err := os.ReadFile(path)
					if err != nil || string(body) != "concurrent" {
						t.Fatalf("concurrent payload removed: %q %v", body, err)
					}
				}
			})
		}
	}
}

func TestFinalEntryDiscardPreservesUnreviewedNamespaceChanges(t *testing.T) {
	for _, rooted := range []bool{false, true} {
		for _, change := range []string{"parent-file", "parent-symlink", "parent-missing", "run-file", "run-missing", "manifest-changed", "run-unreadable"} {
			t.Run(fmt.Sprintf("rooted=%v/%s", rooted, change), func(t *testing.T) {
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
				if _, err := store.LoadManifestGeneration(); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				entry, err := store.BackupBytes("guide.md", []byte("original"), "fixture")
				if err != nil {
					t.Fatal(err)
				}
				payloadRel, err := filepath.Rel(store.RunDir, entry.BackupPath)
				if err != nil {
					t.Fatal(err)
				}
				retainedRun := store.RunDir
				store.beforeSave = func() error {
					parent := filepath.Dir(store.RunDir)
					switch change {
					case "parent-file", "parent-symlink", "parent-missing":
						if err := os.Rename(parent, parent+".retained"); err != nil {
							return err
						}
						retainedRun = filepath.Join(parent+".retained", store.RunID)
						if change == "parent-file" {
							return os.WriteFile(parent, []byte("concurrent"), 0o600)
						}
						if change == "parent-symlink" {
							return os.Symlink(parent+".retained", parent)
						}
					case "run-file", "run-missing":
						if err := os.Rename(store.RunDir, store.RunDir+".retained"); err != nil {
							return err
						}
						retainedRun = store.RunDir + ".retained"
						if change == "run-file" {
							return os.WriteFile(store.RunDir, []byte("concurrent"), 0o600)
						}
					case "manifest-changed":
						return os.WriteFile(filepath.Join(store.RunDir, ManifestName), []byte("concurrent"), 0o600)
					case "run-unreadable":
						if err := os.Chmod(store.RunDir, 0); err != nil {
							return err
						}
						t.Cleanup(func() { _ = os.Chmod(store.RunDir, 0o700) })
						if file, err := os.Open(store.RunDir); err == nil {
							_ = file.Close()
							t.Skip("directory permissions are not enforced")
						}
					}
					return nil
				}
				if err := store.Discard("fixture", "guide.md"); err == nil {
					t.Fatal("discard accepted changed namespace")
				}
				if change == "run-unreadable" {
					if err := os.Chmod(store.RunDir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if len(store.Manifest.Entries) != 1 {
					t.Fatal("failed discard forgot original entry")
				}
				if body, err := os.ReadFile(filepath.Join(retainedRun, payloadRel)); err != nil || string(body) != "original" {
					t.Fatalf("original payload lost: %q %v", body, err)
				}
				if change == "manifest-changed" {
					if body, err := os.ReadFile(filepath.Join(store.RunDir, ManifestName)); err != nil || string(body) != "concurrent" {
						t.Fatalf("concurrent manifest removed: %q %v", body, err)
					}
				}
			})
		}
	}
}

func TestEntryStateMutationsPreserveManifestWhenRejected(t *testing.T) {
	for _, operation := range []string{"post-hash", "pending-hash", "commit-hash", "set-recovery", "clear-recovery"} {
		for _, failure := range []string{"invalid-path", "missing-entry", "save-failure", "wrong-pending", "missing-payload", "relative-recovery"} {
			if failure == "wrong-pending" && operation != "commit-hash" {
				continue
			}
			if (failure == "missing-payload" || failure == "relative-recovery") && operation != "set-recovery" {
				continue
			}
			t.Run(operation+"/"+failure, func(t *testing.T) {
				project := t.TempDir()
				store, err := NewStore(StoreOptions{ProjectRoot: project, HomeDir: t.TempDir(), Operation: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if _, err := store.BackupBytes("memory.md", []byte("original"), "fixture"); err != nil {
					t.Fatal(err)
				}
				hash := strings.Repeat("1", 64)
				if err := store.SetPendingPostHashValue("memory.md", "fixture", hash); err != nil {
					t.Fatal(err)
				}
				manifestPath := filepath.Join(store.RunDir, ManifestName)
				persisted, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				rel, recovery := "memory.md", filepath.Join(project, "quarantine")
				sentinel := errors.New("manifest publication failed")
				switch failure {
				case "invalid-path":
					rel = "../outside"
				case "missing-entry":
					rel = "missing.md"
				case "save-failure":
					store.beforeSave = func() error { return sentinel }
				case "wrong-pending":
					hash = strings.Repeat("2", 64)
				case "missing-payload":
					store.Manifest.Entries[0].BackupPath = ""
				case "relative-recovery":
					recovery = "relative"
				}
				before := append([]Entry(nil), store.Manifest.Entries...)
				switch operation {
				case "post-hash":
					err = store.SetPostHashValue(rel, "fixture", hash)
				case "pending-hash":
					err = store.SetPendingPostHashValue(rel, "fixture", hash)
				case "commit-hash":
					err = store.CommitPendingPostHashValue(rel, "fixture", hash)
				case "set-recovery":
					err = store.SetRecoveryPath(rel, "fixture", recovery)
				case "clear-recovery":
					err = store.ClearRecoveryPath(rel, "fixture")
				}
				if err == nil {
					t.Fatal("invalid mutation accepted")
				}
				if failure == "save-failure" && !errors.Is(err, sentinel) {
					t.Fatalf("publication error lost: %v", err)
				}
				if !reflect.DeepEqual(before, store.Manifest.Entries) {
					t.Fatalf("failed mutation changed memory: %#v", store.Manifest.Entries)
				}
				after, err := os.ReadFile(manifestPath)
				if err != nil || string(after) != string(persisted) {
					t.Fatalf("failed mutation changed disk: %q %v", after, err)
				}
			})
		}
	}
}
