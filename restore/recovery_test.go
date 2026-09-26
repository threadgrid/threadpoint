// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestRetainedDetachedRestoreErrorRendersEveryRecoveryState(t *testing.T) {
	cause := errors.New("restore failed")
	for _, test := range []struct {
		name string
		err  retainedDetachedRestoreError
		want string
	}{
		{name: "rolled back", err: retainedDetachedRestoreError{cause: cause, rolledBack: true}, want: "rolled back"},
		{name: "identity lost", err: retainedDetachedRestoreError{cause: cause, recoveryIdentityLost: true, recoveryRef: "recovery/original"}, want: "no longer identifies"},
		{name: "ambient path", err: retainedDetachedRestoreError{cause: cause, detached: "/tmp/recovery"}, want: "retained at /tmp/recovery"},
		{name: "pinned only", err: retainedDetachedRestoreError{cause: cause, recoveryRef: "recovery/original"}, want: "no stable ambient pathname"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(test.err.Error(), test.want) || !errors.Is(&test.err, cause) {
				t.Fatalf("retained error = %v", &test.err)
			}
		})
	}
}

func TestRecoveryPathUsesReviewedPhysicalRootForSymlinkSelection(t *testing.T) {
	physical := t.TempDir()
	logicalParent := t.TempDir()
	logical := filepath.Join(logicalParent, "workspace")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	home := t.TempDir()
	target := filepath.Join(logical, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, logical, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	report, err := Apply(context.Background(), Options{Root: logical, HomeDir: home, BackupID: runID, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Resolutions) != 1 || report.Resolutions[0].Recovery == "" {
		t.Fatalf("missing physical recovery path: %#v", report.Resolutions)
	}
	recovery := filepath.Clean(report.Resolutions[0].Recovery)
	if !strings.HasPrefix(recovery, filepath.Clean(physical)+string(filepath.Separator)) {
		t.Fatalf("recovery path %q is not under reviewed physical root %q", recovery, physical)
	}
	if strings.HasPrefix(recovery, filepath.Clean(logical)+string(filepath.Separator)) {
		t.Fatalf("recovery path should not depend on logical symlink: %q", recovery)
	}
}

func TestApplyReturnsPartialReportWhenDetachedRecoveryIsRetained(t *testing.T) {
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
	report, err := applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil {
		t.Fatal("expected failed restore with retained detached original")
	}
	if report == nil || len(report.Resolutions) != 1 {
		t.Fatalf("failed restore dropped its partial report: %#v", report)
	}
	resolution := report.Resolutions[0]
	if resolution.Backup == "" || resolution.Recovery == "" || resolution.Restored {
		t.Fatalf("partial resolution omitted recovery metadata: %#v", resolution)
	}
	body, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	var decoded Report
	if err := json.Unmarshal(body, &decoded); err != nil || len(decoded.Resolutions) != 1 || decoded.Resolutions[0].Recovery == "" {
		t.Fatalf("partial report JSON lost recovery metadata: decoded=%#v err=%v", decoded, err)
	}
}

func TestRecoveryDetachDoesNotReplaceBoundaryDestination(t *testing.T) {
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
	hooks := &restoreHooks{beforeRecoveryRename: func(container *os.Root, item string) {
		if err := container.WriteFile(item, []byte("concurrent recovery item"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "no-replace rename boundary") {
		t.Fatalf("expected no-replace recovery refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "after" {
		t.Fatalf("boundary race detached the canonical target, body=%q err=%v", body, readErr)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".threadpoint-restore-recovery-") {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(root, entry.Name(), "original"))
		if readErr == nil && string(body) == "concurrent recovery item" {
			found = true
		}
	}
	if !found {
		t.Fatal("concurrent recovery destination was overwritten or removed")
	}
}

func TestPreDetachFailureCleansEmptyRecoveryContainer(t *testing.T) {
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
	moved := root + "-reviewed"
	hooks := &restoreHooks{beforeRecoveryRename: func(_ *os.Root, _ string) {
		if err := os.Rename(root, moved); err != nil {
			t.Skipf("cannot replace project root on this platform: %v", err)
		}
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "root changed") {
		t.Fatalf("expected pre-detach root replacement refusal, got %v", err)
	}
	entries, readErr := os.ReadDir(moved)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".threadpoint-restore-recovery-") {
			t.Fatalf("empty pre-detach recovery container was not cleaned: %s", entry.Name())
		}
	}
	if body, readErr := os.ReadFile(filepath.Join(moved, "AGENTS.md")); readErr != nil || string(body) != "after" {
		t.Fatalf("pre-detach failure mutated target, body=%q err=%v", body, readErr)
	}
}

func TestRestorePrejournalsRunLocalRecoveryPayloadAndClearsItOnPreDetachError(t *testing.T) {
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
	wantErr := errors.New("stop after recovery journal")
	journaled := ""
	hooks := &restoreHooks{afterRecoveryJournal: func(path string) error {
		journaled = path
		if filepath.Base(path) != "original" {
			t.Fatalf("journaled container instead of exact recovery item: %s", path)
		}
		runs, err := backup.ListRuns(root, home, "")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, run := range runs {
			if run.Operation == "restore" && len(run.Entries) == 1 && run.Entries[0].RecoveryPath == run.Entries[0].BackupPath && run.Entries[0].RecoveryPath != path {
				found = true
			}
		}
		if !found {
			t.Fatalf("run-local recovery payload was not persisted separately from quarantine path %s", path)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("target detached before journal hook, stat=%v", err)
		}
		if body, err := os.ReadFile(target); err != nil || string(body) != "after" {
			t.Fatalf("canonical target changed before detach, body=%q err=%v", body, err)
		}
		return wantErr
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("pre-detach error = %v, want %v", err, wantErr)
	}
	if journaled == "" {
		t.Fatal("recovery item was not pre-journaled")
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.Operation == "restore" && len(run.Entries) == 1 && run.Entries[0].RecoveryPath != "" {
			t.Fatalf("pre-detach error left stale recovery journal: %#v", run.Entries[0])
		}
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "after" {
		t.Fatalf("pre-detach error changed canonical target, body=%q err=%v", body, err)
	}
}

func TestChangedTargetVisibleBeforeDetachRemainsCanonical(t *testing.T) {
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
	hooks := &restoreHooks{beforeDetach: func(parent *os.Root, base string) {
		if err := parent.WriteFile(base, []byte("changed at detach"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "changed immediately before atomic detach") {
		t.Fatalf("expected last-boundary detach validation refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "changed at detach" {
		t.Fatalf("visible replacement did not remain canonical, body=%q err=%v", body, readErr)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	foundRestoreBackup := false
	for _, run := range runs {
		if run.Operation == "restore" && len(run.Entries) == 1 {
			foundRestoreBackup = true
		}
	}
	if foundRestoreBackup {
		t.Fatal("pre-detach refusal retained a tentative overwrite backup")
	}
}

func TestReplacementVisibleAtRecoveryRenameBoundaryIsNeverDetached(t *testing.T) {
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
	hooks := &restoreHooks{beforeRecoveryRename: func(_ *os.Root, _ string) {
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, target, "visible replacement")
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "changed immediately before atomic detach") {
		t.Fatalf("expected recovery-rename-boundary source refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "visible replacement" {
		t.Fatalf("visible recovery-boundary replacement was detached, body=%q err=%v", body, readErr)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".threadpoint-restore-recovery-") {
			t.Fatalf("pre-detach refusal retained a recovery container: %s", entry.Name())
		}
	}
}

func TestRestoreRetainsWritesThroughHandleAfterFinalRecoveryValidation(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	handle, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	hooks := &restoreHooks{afterRecoveryValidation: func() {
		if _, err := handle.WriteString("late handle write"); err != nil {
			t.Fatal(err)
		}
		if err := handle.Sync(); err != nil {
			t.Fatal(err)
		}
	}}
	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, Yes: true, hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Resolutions) != 1 || report.Resolutions[0].Recovery == "" {
		t.Fatalf("retained recovery was not reported: %#v", report.Resolutions)
	}
	if body, readErr := os.ReadFile(report.Resolutions[0].Recovery); readErr != nil || string(body) != "late handle write" {
		t.Fatalf("late handle write was lost, body=%q err=%v", body, readErr)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "before" {
		t.Fatalf("restored target body=%q err=%v", body, readErr)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	persistedRecovery := ""
	for _, run := range runs {
		if run.Operation == "restore" && len(run.Entries) == 1 && run.Entries[0].RecoveryPath == run.Entries[0].BackupPath {
			persistedRecovery = run.Entries[0].RecoveryPath
		}
	}
	if persistedRecovery == "" {
		t.Fatal("run-local recovery payload was not persisted in the restore-overwrite manifest")
	}
	if report.Resolutions[0].Recovery == persistedRecovery {
		t.Fatalf("report returned the run-local payload instead of the external quarantine path: %#v", report.Resolutions[0])
	}
}

func TestRefreshDetachedRecoveryPathClearsUnlocatableJournal(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: project,
		HomeDir:     home,
		Operation:   "restore",
		RunID:       "refresh-detached-recovery",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupBytesWithMode("AGENTS.md", []byte("original"), "restore-overwrite", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRecoveryPath("AGENTS.md", "restore-overwrite", filepath.Join(project, ".threadpoint-restore-recovery", "original")); err != nil {
		t.Fatal(err)
	}
	detached := &detachedRestoreTarget{recoveryPath: "stale-recovery-path"}
	err = refreshDetachedRecoveryPath(nil, detached, Candidate{Path: "AGENTS.md"}, store)
	if err == nil || !strings.Contains(err.Error(), "recovery container is not open") {
		t.Fatalf("unlocatable recovery error = %v", err)
	}
	if detached.recoveryPath != "" || store.Manifest.Entries[0].RecoveryPath != "" {
		t.Fatalf("unlocatable recovery journal was retained: detached=%q entry=%#v", detached.recoveryPath, store.Manifest.Entries[0])
	}
	persisted, err := backup.ReadManifest(filepath.Join(store.RunDir, backup.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Entries[0].RecoveryPath != "" {
		t.Fatalf("persisted unlocatable recovery journal was retained: %#v", persisted.Entries[0])
	}

	// A failed lookup with no previously journaled path must not require a
	// store merely to preserve the original lookup error.
	err = refreshDetachedRecoveryPath(nil, &detachedRestoreTarget{}, Candidate{}, nil)
	if err == nil || !strings.Contains(err.Error(), "recovery container is not open") {
		t.Fatalf("unjournaled recovery lookup error = %v", err)
	}
}

func TestRecoveryOperationsRefuseDamagedRetainedAuthority(t *testing.T) {
	for _, operation := range []string{"prospective-path", "rollback"} {
		for _, failure := range []string{"nil-container", "closed-container", "missing-container", "replaced-container", "closed-project", "closed-parent", "wrong-root-identity", "wrong-parent-identity", "occupied-item", "invalid-item-name", "occupied-target", "missing-item", "changed-item", "rename-failure", "changed-restored-bytes"} {
			if operation == "prospective-path" && (failure == "occupied-target" || failure == "missing-item" || failure == "changed-item" || failure == "rename-failure" || failure == "changed-restored-bytes") {
				continue
			}
			if operation == "rollback" && (failure == "occupied-item" || failure == "invalid-item-name") {
				continue
			}
			t.Run(operation+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				mustWriteFile(t, filepath.Join(dir, "nested", "fixture"), "untouched")
				project, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer project.Close()
				rootInfo, err := project.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
				root, err := project.OpenRoot("nested")
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				info, err := root.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
				parent := &restoreTargetParent{project: project, root: root, path: "nested", base: "target", parentInfo: info, rootSnapshot: restoreRootSnapshot{logicalRoot: dir, physicalRoot: dir, info: rootInfo}}
				if err := root.Mkdir("recovery", 0o700); err != nil {
					t.Fatal(err)
				}
				container, err := root.OpenRoot("recovery")
				if err != nil {
					t.Fatal(err)
				}
				defer container.Close()
				containerInfo, err := container.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
				detached := &detachedRestoreTarget{containerName: "recovery", itemName: "original", itemPath: filepath.Join("recovery", "original"), container: container, containerInfo: containerInfo}
				candidate := Candidate{Path: "nested/target"}
				if _, err := locateProspectiveDetachedRecoveryPath(parent, detached); err != nil {
					t.Fatalf("valid recovery rejected: %v", err)
				}
				if operation == "rollback" {
					if err := container.WriteFile("original", []byte("original"), 0o600); err != nil {
						t.Fatal(err)
					}
					detached.itemInfo, err = container.Lstat("original")
					if err != nil {
						t.Fatal(err)
					}
					candidate.currentSnapshot, err = snapshotRestoreTarget(container, "original", nil)
					if err != nil {
						t.Fatal(err)
					}
				}
				hooks := &restoreHooks{}
				sentinel := errors.New("rollback rename interrupted")
				switch failure {
				case "nil-container":
					detached.container = nil
				case "closed-container":
					_ = container.Close()
				case "missing-container", "replaced-container":
					if err := root.Rename("recovery", "retained"); err != nil {
						t.Fatal(err)
					}
					if failure == "replaced-container" {
						if err := root.Mkdir("recovery", 0o700); err != nil {
							t.Fatal(err)
						}
					}
				case "closed-project":
					_ = project.Close()
				case "closed-parent":
					_ = root.Close()
				case "wrong-root-identity":
					parent.rootSnapshot.info = info
				case "wrong-parent-identity":
					parent.parentInfo = rootInfo
				case "occupied-item":
					if err := container.WriteFile("original", []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "invalid-item-name":
					detached.itemName = "\x00invalid"
				case "occupied-target":
					if err := root.WriteFile("target", []byte("concurrent"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "missing-item", "changed-item":
					if err := container.Rename("original", "retained"); err != nil {
						t.Fatal(err)
					}
					if failure == "changed-item" {
						if err := container.WriteFile("original", []byte("foreign"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				case "rename-failure":
					hooks.renameNoReplace = func(*os.Root, string, string) error { return sentinel }
				case "changed-restored-bytes":
					hooks.renameNoReplace = func(root *os.Root, old, destination string) error {
						if err := root.Rename(old, destination); err != nil {
							return err
						}
						return root.WriteFile(destination, []byte("changed"), 0o600)
					}
				}
				if operation == "prospective-path" {
					_, err = locateProspectiveDetachedRecoveryPath(parent, detached)
				} else {
					err = rollbackDetachedNoReplace(parent, candidate, detached, hooks)
				}
				if err == nil {
					t.Fatal("damaged recovery authority accepted")
				}
				if failure == "rename-failure" && !errors.Is(err, sentinel) {
					t.Fatalf("rename failure lost: %v", err)
				}
				body, readErr := os.ReadFile(filepath.Join(dir, "nested", "fixture"))
				if readErr != nil || string(body) != "untouched" {
					t.Fatalf("unrelated entry changed: %q %v", body, readErr)
				}
				if failure == "occupied-target" {
					body, err := os.ReadFile(filepath.Join(dir, "nested", "target"))
					if err != nil || string(body) != "concurrent" {
						t.Fatalf("concurrent target overwritten: %q %v", body, err)
					}
				}
			})
		}
	}
}
