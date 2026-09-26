// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

func TestRestoreRollsBackWhenDetachedOriginalChangesBeforeSnapshot(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{afterDetachRename: func(parent *os.Root, _ string, recovery string) {
		if err := parent.WriteFile(recovery, []byte("changed while detached"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "changed during atomic detach") || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("detached mutation error = %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "changed while detached" {
		t.Fatalf("changed detached original was not restored, body=%q err=%v", body, readErr)
	}
}

func TestRestoreRollsBackWhenAtomicInstallFailsWithoutAReplacement(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{renameNoReplace: func(parent *os.Root, oldName string, newName string) error {
		if strings.HasPrefix(oldName, ".threadpoint-restore-file-") {
			return errors.New("injected atomic install failure")
		}
		return safefs.RenameRootNoReplace(parent, oldName, newName)
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "injected atomic install failure") || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("atomic install rollback error = %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "after" {
		t.Fatalf("original target was not rolled back, body=%q err=%v", body, readErr)
	}
}

func TestRollbackDoesNotOverwriteReappearedTarget(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{beforeInstall: func(parent *os.Root, base string) {
		if err := parent.WriteFile(base, []byte("concurrent"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "did not overwrite a reappeared target") {
		t.Fatalf("expected no-replace rollback refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "concurrent" {
		t.Fatalf("rollback overwrote concurrent target, body=%q err=%v", body, readErr)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	foundDetached := false
	for _, entry := range entries {
		foundDetached = foundDetached || strings.HasPrefix(entry.Name(), ".threadpoint-restore-recovery-")
	}
	if !foundDetached {
		t.Fatal("original target was not retained after no-replace rollback refusal")
	}
}

func TestRestoreNeverRollsBackReplacementOfDetachedRecoveryItem(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	var replacedItem string
	hooks := &restoreHooks{afterDetachRename: func(parent *os.Root, _ string, itemPath string) {
		replacedItem = itemPath
		if err := parent.Remove(itemPath); err != nil {
			t.Fatal(err)
		}
		if err := parent.WriteFile(itemPath, []byte("untrusted recovery replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "could not be identity-verified") || !strings.Contains(err.Error(), "was not moved into the canonical target") {
		t.Fatalf("expected detached-item identity refusal, got %v", err)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		body, _ := os.ReadFile(target)
		t.Fatalf("recovery replacement reached canonical target, body=%q err=%v", body, statErr)
	}
	if replacedItem == "" {
		t.Fatal("detach hook did not run")
	}
	if body, readErr := os.ReadFile(filepath.Join(root, replacedItem)); readErr != nil || string(body) != "untrusted recovery replacement" {
		t.Fatalf("recovery replacement was not retained, body=%q err=%v", body, readErr)
	}
}

func TestRestoreNeverRollsBackReplacementOfDetachedRecoverySymlink(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unavailable")
	}
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "current")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupSymlinkTarget("current", "before", "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("after", target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := store.SetPostHashValue("current", "fixture-target", restoreSnapshotString("after")); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	var replacedItem string
	hooks := &restoreHooks{afterDetachRename: func(parent *os.Root, _ string, itemPath string) {
		replacedItem = itemPath
		if err := parent.Remove(itemPath); err != nil {
			t.Fatal(err)
		}
		if err := parent.Symlink("untrusted recovery replacement", itemPath); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "could not be identity-verified") || !strings.Contains(err.Error(), "was not moved into the canonical target") {
		t.Fatalf("expected detached-symlink identity refusal, got %v", err)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("recovery symlink replacement reached canonical target: %v", statErr)
	}
	if replacedItem == "" {
		t.Fatal("detach hook did not run")
	}
	if got, readErr := os.Readlink(filepath.Join(root, replacedItem)); readErr != nil || got != "untrusted recovery replacement" {
		t.Fatalf("recovery symlink replacement was not retained, target=%q err=%v", got, readErr)
	}
}

func TestFailedDetachRollbackRetainsDurableOverwriteBackup(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{
		afterDetachRename: func(parent *os.Root, base string, _ string) {
			if err := parent.WriteFile(base, []byte("concurrent"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "original restore target was retained") {
		t.Fatalf("expected retained detached target error, got %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "concurrent" {
		t.Fatalf("concurrent target was overwritten, body=%q err=%v", body, readErr)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	foundRecovery := false
	for _, run := range runs {
		if run.Operation == "restore" && len(run.Entries) == 1 && run.Entries[0].Role == "restore-overwrite" {
			foundRecovery = true
			if body, readErr := os.ReadFile(run.Entries[0].BackupPath); readErr != nil || string(body) != "after" {
				t.Fatalf("durable recovery payload body=%q err=%v", body, readErr)
			}
		}
	}
	if !foundRecovery {
		t.Fatal("durable overwrite backup was discarded after incomplete rollback")
	}
}

func TestRestoreRootRenameAfterDetachRollsBackWithoutStaleRecoveryPath(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	base := t.TempDir()
	root := filepath.Join(base, "project")
	moved := filepath.Join(base, "project-moved")
	home := t.TempDir()
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{afterDetachRename: func(_ *os.Root, _ string, _ string) {
		if err := os.Rename(root, moved); err != nil {
			t.Skipf("cannot rename an opened project root on this platform: %v", err)
		}
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "root changed") {
		t.Fatalf("expected post-detach root change refusal, got %v", err)
	}
	stalePrefix := filepath.Join(root, ".threadpoint-restore-recovery-")
	if strings.Contains(err.Error(), stalePrefix) {
		t.Fatalf("error reported stale recovery path %q: %v", stalePrefix, err)
	}
	if body, readErr := os.ReadFile(filepath.Join(moved, "AGENTS.md")); readErr != nil || string(body) != "after" {
		t.Fatalf("pinned original was not rolled back in moved root, body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "AGENTS.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replacement root was mutated: %v", statErr)
	}
	assertRestoreRecoveryPathsRunLocalOrCleared(t, root, home)
}

func TestRestoreParentRenameAfterDetachRollsBackWithoutStaleRecoveryPath(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	root := t.TempDir()
	home := t.TempDir()
	parentPath := filepath.Join(root, "nested")
	movedParent := filepath.Join(root, "nested-moved")
	if err := os.Mkdir(parentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parentPath, "AGENTS.md")
	mustWriteFile(t, target, "before")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("nested/AGENTS.md", target, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, target, "after")
	if err := store.SetPostHash("nested/AGENTS.md", target); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{afterDetachRename: func(_ *os.Root, _ string, _ string) {
		if err := os.Rename(parentPath, movedParent); err != nil {
			t.Skipf("cannot rename an opened restore parent on this platform: %v", err)
		}
		if err := os.Mkdir(parentPath, 0o755); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "parent changed") {
		t.Fatalf("expected post-detach parent change refusal, got %v", err)
	}
	stalePrefix := filepath.Join(parentPath, ".threadpoint-restore-recovery-")
	if strings.Contains(err.Error(), stalePrefix) {
		t.Fatalf("error reported stale recovery path %q: %v", stalePrefix, err)
	}
	if body, readErr := os.ReadFile(filepath.Join(movedParent, "AGENTS.md")); readErr != nil || string(body) != "after" {
		t.Fatalf("pinned original was not rolled back in moved parent, body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(parentPath, "AGENTS.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replacement parent was mutated: %v", statErr)
	}
	assertRestoreRecoveryPathsRunLocalOrCleared(t, root, home)
}

func assertRestoreRecoveryPathsRunLocalOrCleared(t *testing.T, projectRoot string, home string) {
	t.Helper()
	runs, err := backup.ListRuns(projectRoot, home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.Operation != "restore" {
			continue
		}
		for _, entry := range run.Entries {
			if entry.RecoveryPath == "" {
				continue
			}
			if entry.RecoveryPath != entry.BackupPath {
				t.Fatalf("manifest recovery intent does not name its run-local payload: %#v", entry)
			}
			rel, relErr := filepath.Rel(run.Path, entry.RecoveryPath)
			if relErr != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("manifest recovery payload escapes run %q: %q", run.Path, entry.RecoveryPath)
			}
			if _, err := os.Lstat(entry.RecoveryPath); err != nil {
				t.Fatalf("manifest recovery payload is not discoverable: %q: %v", entry.RecoveryPath, err)
			}
		}
	}
}

func TestRestoreTransactionRefusesLostAuthorityBeforePublication(t *testing.T) {
	for _, failure := range []string{"missing-payload-pin", "closed-project", "changed-target", "missing-store", "cannot-record-absence", "missing-before-detach", "appeared-before-install", "closed-before-install", "failed-discard"} {
		t.Run(failure, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			target := filepath.Join(project, "AGENTS.md")
			mustWriteFile(t, target, "before")
			runID := seedFileBackup(t, project, home, target, func() { mustWriteFile(t, target, "after") })
			absent := failure == "cannot-record-absence" || failure == "appeared-before-install" || failure == "closed-before-install"
			if absent {
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: project, HomeDir: home, BackupID: runID})
			if err != nil {
				t.Fatal(err)
			}
			candidate := plan.Candidates[0]
			root, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: project, HomeDir: home, Operation: restoreOperation})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			hooks := &restoreHooks{}
			retained, want := target, "after"
			switch failure {
			case "missing-payload-pin":
				candidate.backupPin = nil
			case "closed-project":
				_ = root.Close()
			case "changed-target":
				mustWriteFile(t, target, "concurrent")
				want = "concurrent"
			case "missing-store", "cannot-record-absence":
				store = nil
			case "missing-before-detach", "failed-discard":
				retained = target + ".retained"
				hooks.beforeDetach = func(parent *os.Root, name string) {
					if err := parent.Rename(name, name+".retained"); err != nil {
						t.Fatal(err)
					}
					if failure == "failed-discard" {
						store.RunDir = filepath.Join(t.TempDir(), "wrong-run")
					}
				}
			case "appeared-before-install":
				want = "concurrent"
				hooks.beforeDetach = func(parent *os.Root, name string) {
					if err := parent.WriteFile(name, []byte(want), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			case "closed-before-install":
				hooks.beforeDetach = func(parent *os.Root, _ string) {
					if err := parent.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, _, err = restoreBackup(root, plan.rootSnapshot, store, candidate, "", false, hooks)
			if err == nil {
				t.Fatal("unsafe restore transaction succeeded")
			}
			if absent && failure != "appeared-before-install" {
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed restore installed target: %v", err)
				}
			} else {
				body, err := os.ReadFile(retained)
				if err != nil || string(body) != want {
					t.Fatalf("failed restore lost current generation: %q %v", body, err)
				}
			}
		})
	}
}

func TestOverwriteBackupRefusesUnavailableCurrentTarget(t *testing.T) {
	for _, failure := range []string{"closed-parent", "missing-target", "unreadable-file", "unreadable-directory"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			snapshot, project, err := captureAndOpenRestoreRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer project.Close()
			candidate := Candidate{Path: "target", Root: dir, Target: filepath.Join(dir, "target")}
			parent, err := openRestoreTargetParent(project, snapshot, candidate)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.root.Close()
			switch failure {
			case "closed-parent":
				err = parent.root.Close()
			case "unreadable-file":
				err = parent.root.WriteFile("target", []byte("current"), 0)
				defer func() { _ = os.Chmod(candidate.Target, 0o600) }()
			case "unreadable-directory":
				err = parent.root.Mkdir("target", 0)
				defer func() { _ = os.Chmod(candidate.Target, 0o700) }()
			}
			if err != nil {
				t.Fatal(err)
			}
			entry, err := backupRootedCurrent(nil, parent, candidate)
			if err == nil || entry != nil {
				t.Fatalf("unavailable target backed up: %#v %v", entry, err)
			}
		})
	}
}
