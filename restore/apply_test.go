// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

func TestRetainedRestoreErrorsKeepTheOriginalFailureAndLocationContext(t *testing.T) {
	cause := errors.New("replacement failed")
	target := &detachedRestoreTarget{itemPath: "memory.md", recoveryPath: "/recovery/memory.md"}
	retained := target.retainedError(cause)
	if !errors.Is(retained, cause) || !strings.Contains(retained.Error(), "/recovery/memory.md") {
		t.Fatalf("retained error = %v", retained)
	}
	for _, err := range []*retainedDetachedRestoreError{
		{cause: cause, rolledBack: true},
		{cause: cause, recoveryIdentityLost: true, recoveryRef: "recovery/item"},
		{cause: cause, recoveryRef: "pinned/item"},
	} {
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), "replacement failed") {
			t.Fatalf("retained error variant = %v", err)
		}
	}
}

func TestRestoreDiscardsTentativeOverwriteBackupWhenTargetChangesDuringCapture(t *testing.T) {
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
	hooks := &restoreHooks{beforeOverwriteBackup: func(parent *os.Root, base string) {
		if err := parent.WriteFile(base, []byte("changed during overwrite backup"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "changed while its overwrite backup was captured") {
		t.Fatalf("overwrite backup race error = %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "changed during overwrite backup" {
		t.Fatalf("overwrite backup race changed target, body=%q err=%v", body, readErr)
	}
}

func TestApplyRestoresMissingFileFromSelectedBackup(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, "")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "before" {
		t.Fatalf("expected restored file, got %q", body)
	}
	if len(report.Resolutions) != 1 || !report.Resolutions[0].Restored {
		t.Fatalf("expected restore resolution, got %#v", report.Resolutions)
	}
}

func TestApplyRemovesAFileRecordedAsAbsentBeforeTheOperation(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.RecordAbsent("generated.md", "fixture-target"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "generated.md")
	mustWriteFile(t, target, "created by operation\n")
	postHash, err := backup.FileSHA256(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPostHashValue("generated.md", "fixture-target", postHash); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil || len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusReady || !plan.Candidates[0].Absent {
		t.Fatalf("absent-target plan = %#v, err=%v", plan, err)
	}
	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true})
	if err != nil || len(report.Resolutions) != 1 || !report.Resolutions[0].Restored {
		t.Fatalf("absent-target restore report = %#v, err=%v", report, err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent-target restore did not remove generated file: %v", err)
	}
}

func TestApplyRestoresWhenCurrentMatchesPostHash(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})

	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "before" {
		t.Fatalf("expected rollback content, got %q", body)
	}
	if report.Resolutions[0].Backup == "" {
		t.Fatalf("expected overwrite backup, got %#v", report.Resolutions)
	}
}

func TestApplyRestoresOriginallyAbsentTargetByRemovingIt(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.RecordAbsent("AGENTS.md", "fixture-target"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, target, "created by commit")
	if err := store.SetPostHashValue("AGENTS.md", "fixture-target", mustFileSHA256(t, target)); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || !plan.Candidates[0].Absent || plan.Candidates[0].Status != StatusReady {
		t.Fatalf("expected ready absent-target candidate, got %#v", plan.Candidates)
	}
	if _, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("restore should remove originally absent target: %v", err)
	}
}

func TestApplyRestoresRecordedFileAndDirectoryPermissions(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	file := filepath.Join(root, "AGENTS.md")
	directory := filepath.Join(root, "native")
	child := filepath.Join(directory, "guide.md")
	mustWriteFile(t, file, "guide\n")
	mustWriteFile(t, child, "nested guide\n")
	for path, mode := range map[string]os.FileMode{
		file:      0o640,
		directory: 0o750,
		child:     0o600,
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("AGENTS.md", file, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackupDir("native", directory, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		file:      0o640,
		directory: 0o750,
		child:     0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode=%04o, want %04o", path, got, want)
		}
	}
}

func TestRestoreOverwriteBackupRecordsExactPostState(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	source, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root,
		HomeDir:     home,
		Operation:   "fixture",
		Now: func() time.Time {
			return time.Date(2026, 5, 30, 1, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := source.BackupFile("AGENTS.md", target, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, target, "after")
	if err := source.SetPostHash("AGENTS.md", target); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), Options{
		Root:     root,
		HomeDir:  home,
		BackupID: source.RunID,
		Yes:      true,
		Now: func() time.Time {
			return time.Date(2026, 5, 31, 1, 0, 0, 0, time.UTC)
		},
	}); err != nil {
		t.Fatal(err)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	var restoreRun string
	for _, run := range runs {
		if run.Operation == "restore" {
			restoreRun = run.RunID
			if len(run.Entries) != 1 || run.Entries[0].PostSHA256 != mustFileSHA256(t, target) {
				t.Fatalf("restore overwrite entry lacks exact post state: %#v", run.Entries)
			}
			break
		}
	}
	if restoreRun == "" {
		t.Fatalf("restore overwrite run not found: %#v", runs)
	}
	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: restoreRun})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusReady {
		t.Fatalf("exact post state should make inverse restore ready: %#v", plan.Candidates)
	}
	if _, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: restoreRun, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "after" {
		t.Fatalf("inverse restore body=%q err=%v", body, err)
	}
}

func TestDirectoryInstallDoesNotReplaceConcurrentEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "backup")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupDir("native", target, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{beforeInstall: func(parent *os.Root, base string) {
		if err := parent.Mkdir(base, 0o755); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "reappeared") {
		t.Fatalf("expected concurrent empty directory refusal, got %v", err)
	}
	entries, readErr := os.ReadDir(target)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("concurrent empty directory was replaced, entries=%v err=%v", entries, readErr)
	}
}

func TestRestoreFilesystemProbeFailurePrecedesCanonicalAndBackupMutation(t *testing.T) {
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
	wantErr := errors.New("runtime filesystem refuses no-replace")
	probed := false
	hooks := &restoreHooks{probeAtomicDetach: func(parent *os.Root) error {
		probed = true
		if body, err := parent.ReadFile("AGENTS.md"); err != nil || string(body) != "after" {
			t.Fatalf("probe did not receive actual target parent, body=%q err=%v", body, err)
		}
		return wantErr
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("probe failure = %v, want %v", err, wantErr)
	}
	if !probed {
		t.Fatal("actual target-parent filesystem was not probed")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "after" {
		t.Fatalf("probe failure changed canonical target, body=%q err=%v", body, err)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.Operation == "restore" {
			t.Fatalf("probe failure created restore backup state: %#v", run)
		}
	}
}

func TestMissingRegularRestoreProbesNearestExistingParentBeforeCreatingDirectories(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "source.md")
	mustWriteFile(t, source, "backup")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("missing/nested/AGENTS.md", source, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected absent-target probe failure")
	probes := 0
	hooks := &restoreHooks{probeAtomicDetach: func(parent *os.Root) error {
		probes++
		if _, err := parent.Stat("."); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preflight created a missing restore parent: %v", err)
		}
		return wantErr
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("probe failure = %v, want %v", err, wantErr)
	}
	if probes != 1 {
		t.Fatalf("nearest existing parent probe count = %d, want 1", probes)
	}
	if _, err := os.Lstat(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed preflight left a missing parent behind: %v", err)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.Operation == "restore" {
			t.Fatalf("failed preflight created restore state: %#v", run)
		}
	}
}

func TestMissingDirectoryRestoreProbesBeforeCreatingTargetParents(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "source-directory")
	mustWriteFile(t, filepath.Join(source, "guide.md"), "backup")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupDir("missing/nested/native", source, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected directory no-replace probe failure")
	hooks := &restoreHooks{probeAtomicDetach: func(*os.Root) error { return wantErr }}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("directory probe failure = %v, want %v", err, wantErr)
	}
	if _, err := os.Lstat(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory preflight created missing target parents: %v", err)
	}
}

func TestRestoreRegularInstallUsesNoReplaceRenameInsteadOfHardLink(t *testing.T) {
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
	var operations [][2]string
	hooks := &restoreHooks{renameNoReplace: func(root *os.Root, oldName string, newName string) error {
		operations = append(operations, [2]string{oldName, newName})
		return safefs.RenameRootNoReplace(root, oldName, newName)
	}}
	if _, err := applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}}); err != nil {
		t.Fatal(err)
	}
	foundInstall := false
	for _, operation := range operations {
		if strings.HasPrefix(filepath.Base(operation[0]), ".threadpoint-restore-file-") && operation[1] == "AGENTS.md" {
			foundInstall = true
		}
	}
	if !foundInstall {
		t.Fatalf("regular restore did not install its prepared file with no-replace rename: %#v", operations)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "before" {
		t.Fatalf("restored body=%q err=%v", body, err)
	}
}

func TestRestoreDirectoryPublishesPreparedSiblingWithNoReplaceRename(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "backup")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupDir("native", target, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	var operations [][2]string
	hooks := &restoreHooks{renameNoReplace: func(parent *os.Root, oldName string, newName string) error {
		operations = append(operations, [2]string{oldName, newName})
		return safefs.RenameRootNoReplace(parent, oldName, newName)
	}}
	if _, err := applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, operation := range operations {
		if strings.HasPrefix(filepath.Base(operation[0]), ".threadpoint-restore-directory-") && operation[1] == "native" {
			found = true
		}
	}
	if !found {
		t.Fatalf("prepared directory was not atomically published: %#v", operations)
	}
	if body, err := os.ReadFile(filepath.Join(target, "guide.md")); err != nil || string(body) != "backup" {
		t.Fatalf("restored directory body=%q err=%v", body, err)
	}
}

func TestPreparedDirectoryMutationFailsBeforeAtomicPublication(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "backup")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupDir("native", target, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{duringDirectoryInstall: func(preparedRoot *os.Root) {
		if err := preparedRoot.WriteFile("guide.md", []byte("concurrent"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "changed before atomic publication") {
		t.Fatalf("expected prepared-directory integrity refusal, got %v", err)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed prepared-directory validation exposed a partial canonical target: %v", statErr)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	foundAbsent := false
	for _, run := range runs {
		if run.Operation != "restore" {
			continue
		}
		for _, entry := range run.Entries {
			foundAbsent = foundAbsent || (entry.Path == "native" && entry.Role == "restore-overwrite" && entry.Absent)
		}
	}
	if foundAbsent {
		t.Fatal("failed pre-publication directory validation retained a tentative absent recovery entry")
	}
}

func TestApplyPreservesExactSymlinkTargetWhitespace(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	link := filepath.Join(root, "guide-link")
	want := " nested/guide.md "
	if err := os.Symlink(want, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("guide-link", link, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != want {
		t.Fatalf("restored symlink target=%q, want=%q, err=%v", got, want, err)
	}
}

func TestApplyRestoresOrdinarySymlinkSuffixedFile(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "native")
	ordinary := filepath.Join(source, "notes.symlink")
	mustWriteFile(t, ordinary, "ordinary backup file\n")
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root,
		HomeDir:     home,
		Operation:   "fixture",
		Now: func() time.Time {
			return time.Date(2026, 5, 30, 1, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupDir("native", source, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(ordinary)
	if err != nil {
		t.Fatalf("expected regular .symlink-suffixed file to be restored: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("ordinary .symlink-suffixed file was restored as a symlink")
	}
	body, err := os.ReadFile(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ordinary backup file\n" {
		t.Fatalf("restored file = %q, want original content", body)
	}
}

func TestAcceptedSymlinkRestoreReplacesRegularTarget(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	targetFile := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	mustWriteFile(t, targetFile, "target\n")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatal(err)
	}
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("link.txt", link, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, link, "regular replacement\n")

	opts := Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true}
	plan, err := buildRetainedRestorePlan(t, context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusConflict {
		t.Fatalf("expected regular target conflict for symlink backup, got %#v", plan.Candidates)
	}
	if _, err := applyResolutions(context.Background(), opts, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}}); err != nil {
		t.Fatalf("accepted symlink restore should replace a regular target: %v", err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected symlink to be restored: %v", err)
	}
	if got != "target.txt" {
		t.Fatalf("restored symlink target = %q, want target.txt", got)
	}
}

func TestAcceptedDirectoryRestoreReplacesRegularTarget(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	directory := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(directory, "notes.md"), "directory backup\n")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupDir("native", directory, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, directory, "regular replacement\n")

	opts := Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true}
	plan, err := buildRetainedRestorePlan(t, context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusConflict {
		t.Fatalf("expected regular target conflict for directory backup, got %#v", plan.Candidates)
	}
	if _, err := applyResolutions(context.Background(), opts, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}}); err != nil {
		t.Fatalf("accepted directory restore should replace a regular target: %v", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("restored target type = %v, want directory", info.Mode())
	}
	body, err := os.ReadFile(filepath.Join(directory, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "directory backup\n" {
		t.Fatalf("restored directory content = %q", body)
	}
}
