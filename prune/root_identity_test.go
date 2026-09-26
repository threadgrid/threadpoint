// SPDX-License-Identifier: Apache-2.0

package prune

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

func TestBuildPlanReturnsPinnedBackupListingError(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	projectRuns := filepath.Join(home, "backups", backup.ProjectID(root))
	if err := os.MkdirAll(filepath.Dir(projectRuns), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectRuns, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	productRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer productRoot.Close()

	_, err = BuildPlan(context.Background(), Options{
		Root:            root,
		ThreadpointHome: home,
		ProductRoot:     productRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "backup project run path is not a real directory") {
		t.Fatalf("expected pinned backup listing error, got %v", err)
	}
}

func TestApplyRejectsNestedSymlinkAncestorWithoutRemovingOutsideFile(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	home := t.TempDir()
	source := filepath.Join(root, "nested", "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshotRel(t, root, home, "nested/CLAUDE.md", source)
	if err := os.RemoveAll(filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(base, "outside")
	outside := filepath.Join(outsideDir, "CLAUDE.md")
	mustWriteFile(t, outside, "claude")
	if err := os.Symlink(outsideDir, filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}

	_, err := Apply(context.Background(), Options{Root: root, HomeDir: home, Yes: true})
	if err == nil || !errors.Is(err, safefs.ErrUnsafeSymlink) {
		t.Fatalf("expected unsafe-symlink refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(outside); readErr != nil || string(body) != "claude" {
		t.Fatalf("outside target changed, body=%q err=%v", body, readErr)
	}
}

func TestApplyFollowsSelectedLogicalRootButPinsPhysicalProject(t *testing.T) {
	base := t.TempDir()
	physical := filepath.Join(base, "physical")
	logical := filepath.Join(base, "selected-project")
	home := t.TempDir()
	source := filepath.Join(physical, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	seedStageSourceSnapshot(t, logical, home, filepath.Join(logical, "CLAUDE.md"))

	report, err := Apply(context.Background(), Options{Root: logical, HomeDir: home, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Root != logical {
		t.Fatalf("report root=%q, want selected logical root %q", report.Root, logical)
	}
	if len(report.Resolutions) != 1 || !strings.HasPrefix(report.Resolutions[0].Recovery, physical+string(filepath.Separator)) {
		t.Fatalf("recovery path was not bound to physical root %q: %#v", physical, report.Resolutions)
	}
	if strings.HasPrefix(report.Resolutions[0].Recovery, logical+string(filepath.Separator)) {
		t.Fatalf("recovery path depends on mutable logical symlink: %s", report.Resolutions[0].Recovery)
	}
}

func TestApplyRejectsSelectedLogicalRootRetargetAfterBackup(t *testing.T) {
	base := t.TempDir()
	physicalA := filepath.Join(base, "physical-a")
	physicalB := filepath.Join(base, "physical-b")
	logical := filepath.Join(base, "selected-project")
	home := t.TempDir()
	sourceA := filepath.Join(physicalA, "CLAUDE.md")
	sourceB := filepath.Join(physicalB, "CLAUDE.md")
	mustWriteFile(t, sourceA, "claude")
	mustWriteFile(t, sourceB, "other project")
	if err := os.Symlink(physicalA, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	seedStageSourceSnapshot(t, logical, home, filepath.Join(logical, "CLAUDE.md"))

	_, err := applyWithHooks(context.Background(), Options{Root: logical, HomeDir: home, Yes: true}, applyHooks{
		afterBackup: func(_ Candidate, _ *os.Root, _ string) {
			if removeErr := os.Remove(logical); removeErr != nil {
				t.Fatal(removeErr)
			}
			if linkErr := os.Symlink(physicalB, logical); linkErr != nil {
				t.Fatal(linkErr)
			}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "now resolves") {
		t.Fatalf("expected logical-root retarget refusal, got %v", err)
	}
	for path, want := range map[string]string{sourceA: "claude", sourceB: "other project"} {
		if body, readErr := os.ReadFile(path); readErr != nil || string(body) != want {
			t.Fatalf("root retarget changed %s, body=%q err=%v", path, body, readErr)
		}
	}
}

func TestApplyPinsSelectedRootFromPlanningThroughPreflight(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	reviewedRoot := filepath.Join(base, "reviewed-project")
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)

	_, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		afterPlan: func(plan *Plan, pinned *os.Root) error {
			if plan == nil || plan.root.info == nil || pinned == nil {
				t.Fatal("apply did not retain its planned project-root identity")
			}
			opened, statErr := pinned.Stat(".")
			if statErr != nil || !os.SameFile(plan.root.info, opened) {
				t.Fatalf("planned project root is not pinned: opened=%#v err=%v", opened, statErr)
			}
			if renameErr := os.Rename(root, reviewedRoot); renameErr != nil {
				return renameErr
			}
			mustWriteFile(t, filepath.Join(root, "CLAUDE.md"), "claude")
			return nil
		},
	})
	if err == nil || (!strings.Contains(err.Error(), "prune root changed") && !strings.Contains(err.Error(), "locked generation")) {
		t.Fatalf("expected selected-root replacement refusal, got %v", err)
	}
	for path := range map[string]struct{}{
		filepath.Join(root, "CLAUDE.md"):         {},
		filepath.Join(reviewedRoot, "CLAUDE.md"): {},
	} {
		if body, readErr := os.ReadFile(path); readErr != nil || string(body) != "claude" {
			t.Fatalf("selected-root race changed %s, body=%q err=%v", path, body, readErr)
		}
	}
}

func TestApplyPreservesSourceSwapInsideFinalCheckRenameGap(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)

	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		afterSourceBoundary: func(_ Candidate, parent *os.Root, base string, _ *os.Root, _ string) {
			if removeErr := parent.Remove(base); removeErr != nil {
				t.Fatal(removeErr)
			}
			writeRootFile(t, parent, base, "final-gap replacement")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed during atomic detach") {
		t.Fatalf("expected post-rename identity refusal, got report=%#v err=%v", report, err)
	}
	if _, statErr := os.Lstat(source); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unverified final-gap replacement was installed as canonical: %v", statErr)
	}
	if report == nil || len(report.Resolutions) != 1 || report.Resolutions[0].Recovery == "" || report.Resolutions[0].Backup == "" {
		t.Fatalf("final-gap replacement and sealed backup were not reported: %#v", report)
	}
	if body, readErr := os.ReadFile(report.Resolutions[0].Recovery); readErr != nil || string(body) != "final-gap replacement" {
		t.Fatalf("final-gap replacement was lost, body=%q err=%v", body, readErr)
	}
}

func TestApplyPreflightsEveryParentBeforeEarlierCandidateMutation(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	first := filepath.Join(root, "a", "CLAUDE.md")
	second := filepath.Join(root, "b", "AGENTS.md")
	mustWriteFile(t, first, "first")
	mustWriteFile(t, second, "second")
	seedStageSourceSnapshotRel(t, root, home, "a/CLAUDE.md", first)
	seedStageSourceSnapshotRel(t, root, home, "b/AGENTS.md", second)
	probes := 0

	_, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		probeNoReplace: func(_ *os.Root) error {
			probes++
			if probes == 2 {
				return safefs.ErrRenameNoReplaceUnsupported
			}
			return nil
		},
	})
	if err == nil || !errors.Is(err, safefs.ErrRenameNoReplaceUnsupported) {
		t.Fatalf("expected runtime no-replace preflight refusal, got %v", err)
	}
	if probes != 2 {
		t.Fatalf("preflight probes=%d, want 2", probes)
	}
	for path, want := range map[string]string{first: "first", second: "second"} {
		if body, readErr := os.ReadFile(path); readErr != nil || string(body) != want {
			t.Fatalf("preflight failure mutated %s, body=%q err=%v", path, body, readErr)
		}
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.Operation == "prune" {
			t.Fatalf("preflight failure created a prune backup run: %#v", run)
		}
	}
}

func TestReadPruneRegularReturnsHashForRootedBoundedFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "bounded")
	rooted, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rooted.Close()
	body, info, err := readPruneRegular(rooted, "CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "bounded" || info == nil || pruneSHA256(body) == strings.Repeat("0", 64) {
		t.Fatalf("unexpected rooted bounded read: body=%q info=%#v", body, info)
	}
}

func TestApplyRejectsEscapingBackupEntryWithoutRemovingOutsideFile(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	home := t.TempDir()
	outside := filepath.Join(base, "outside", "CLAUDE.md")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, outside, "outside claude")
	runID := seedStageSourceSnapshotRel(t, root, home, "CLAUDE.md", outside)
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: backup.PendingCommitOperation, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil {
		t.Fatal(err)
	}
	// Simulate a persisted manifest that was tampered with after the backup
	// store accepted and wrote a safe entry. New writes reject this path at the
	// API boundary, while readers must still fail safely on hostile old state.
	store.Manifest.Entries[0].Path = "../outside/CLAUDE.md"
	writeMalformedBackupManifestFixture(t, store)

	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, Yes: true})
	if err == nil || report != nil {
		t.Fatalf("escaping canonical backup entry was not reported before mutation: report=%#v err=%v", report, err)
	}
	body, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("outside file should remain untouched: %v", err)
	}
	if string(body) != "outside claude" {
		t.Fatalf("outside file was changed, got %q", body)
	}
}

func TestApplyPreflightsEveryReadyCandidateBeforeFirstBackup(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("native no-replace rename is required for prune apply")
	}
	root := t.TempDir()
	home := t.TempDir()
	alpha := filepath.Join(root, "alpha.md")
	omega := filepath.Join(root, "omega.md")
	mustWriteFile(t, alpha, "reviewed alpha\n")
	mustWriteFile(t, omega, "reviewed omega\n")
	seedStageSourceSnapshotRel(t, root, home, "alpha.md", alpha)
	seedStageSourceSnapshotRel(t, root, home, "omega.md", omega)

	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{afterPlan: func(_ *Plan, _ *os.Root) error {
		mustWriteFile(t, omega, "changed after plan\n")
		return nil
	}})
	if err == nil || !strings.Contains(err.Error(), "omega.md") {
		t.Fatalf("expected later-candidate preflight refusal, report=%#v err=%v", report, err)
	}
	if report != nil {
		t.Fatalf("later-candidate preflight created an apply report: %#v", report)
	}
	if body, readErr := os.ReadFile(alpha); readErr != nil || string(body) != "reviewed alpha\n" {
		t.Fatalf("earlier prune candidate changed before later refusal, body=%q err=%v", body, readErr)
	}
	if runs, listErr := backup.ListRuns(root, home, ""); listErr != nil {
		t.Fatal(listErr)
	} else {
		for _, run := range runs {
			if run.Operation == "prune" {
				t.Fatalf("later-candidate preflight created prune backup state: %#v", run)
			}
		}
	}
}

func TestApplyRetainsExactPruneSourceIdentityAcrossPlan(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("native no-replace rename is required for prune apply")
	}
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "native.md")
	moved := filepath.Join(root, "native.reviewed.md")
	body := "same reviewed bytes\n"
	mustWriteFile(t, source, body)
	seedStageSourceSnapshotRel(t, root, home, "native.md", source)

	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{afterPlan: func(_ *Plan, _ *os.Root) error {
		if err := os.Rename(source, moved); err != nil {
			return err
		}
		mustWriteFile(t, source, body)
		return nil
	}})
	if err == nil || !strings.Contains(err.Error(), "changed since the reviewed plan") {
		t.Fatalf("expected exact prune-source refusal, report=%#v err=%v", report, err)
	}
	if report != nil {
		t.Fatalf("exact prune-source refusal created an apply report: %#v", report)
	}
	for _, path := range []string{source, moved} {
		if got, readErr := os.ReadFile(path); readErr != nil || string(got) != body {
			t.Fatalf("prune identity refusal changed %s, body=%q err=%v", path, got, readErr)
		}
	}
	if runs, listErr := backup.ListRuns(root, home, ""); listErr != nil {
		t.Fatal(listErr)
	} else {
		for _, run := range runs {
			if run.Operation == "prune" {
				t.Fatalf("exact prune-source refusal created prune backup state: %#v", run)
			}
		}
	}
}

func TestPrunePlanPinsCloseOnPlanOnlyApplyErrorAndReport(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("native no-replace rename is required for prune apply")
	}
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "native.md")
	mustWriteFile(t, source, "reviewed bytes\n")
	seedStageSourceSnapshotRel(t, root, home, "native.md", source)

	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	assertPrunePlanPinsClosed(t, plan)

	var retainedPlan *Plan
	var retainedRoot *os.Root
	stop := errors.New("stop after pinned prune plan")
	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{afterPlan: func(plan *Plan, projectRoot *os.Root) error {
		retainedPlan = plan
		retainedRoot = projectRoot
		return stop
	}})
	if !errors.Is(err, stop) || report != nil {
		t.Fatalf("expected post-plan error without report, report=%#v err=%v", report, err)
	}
	assertPrunePlanPinsClosed(t, retainedPlan)
	if retainedRoot == nil {
		t.Fatal("post-plan hook did not expose prune root")
	}
	if _, statErr := retainedRoot.Stat("."); statErr == nil {
		t.Fatal("prune root remained open after apply error")
	}

	report, err = Apply(context.Background(), Options{Root: root, HomeDir: home, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	assertPrunePlanPinsClosed(t, report.Plan)
}

func TestPruneRootCapabilitiesRejectReplacementAndEscape(t *testing.T) {
	t.Run("missing selected root", func(t *testing.T) {
		_, root, err := captureAndOpenPruneRoot(filepath.Join(t.TempDir(), "missing"))
		if root != nil {
			_ = root.Close()
		}
		if err == nil {
			t.Fatal("missing prune root was accepted")
		}
	})

	t.Run("retargeted selected root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "project")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		snapshot, projectRoot, err := captureAndOpenPruneRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer projectRoot.Close()
		if err := os.Rename(root, root+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := validatePruneRootSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("replacement root was accepted: %v", err)
		}
	})

	t.Run("invalid candidate path", func(t *testing.T) {
		root := t.TempDir()
		info, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := pruneRootSnapshot{logical: root, physical: root, info: info}
		for _, candidate := range []Candidate{{Path: "../outside"}, {Path: "."}} {
			if _, err := candidateRootName(snapshot, candidate); err == nil {
				t.Fatalf("unsafe candidate path %q was accepted", candidate.Path)
			}
		}
	})

	t.Run("nil project capability", func(t *testing.T) {
		if _, err := openPruneTargetParent(nil, pruneRootSnapshot{}, Candidate{Path: "native.md"}); err == nil {
			t.Fatal("nil project capability was accepted")
		}
	})

	t.Run("symlinked target parent", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "nested")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		snapshot, projectRoot, err := captureAndOpenPruneRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer projectRoot.Close()
		if _, err := openPruneTargetParent(projectRoot, snapshot, Candidate{Path: "nested/native.md"}); err == nil || !errors.Is(err, safefs.ErrUnsafeSymlink) {
			t.Fatalf("symlinked target parent was accepted: %v", err)
		}
	})

	if err := (*pruneTargetParent)(nil).verify(); err == nil {
		t.Fatal("nil target-parent capability was accepted")
	}
}

func TestPinnedPruneReadRejectsUnsafeOrChangedIdentity(t *testing.T) {
	if _, _, _, err := readPinnedPruneRegular(nil, "native.md"); err == nil {
		t.Fatal("nil rooted read capability was accepted")
	}

	t.Run("special and missing paths", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(root, "target.md"), "target")
		if err := os.Symlink("target.md", filepath.Join(root, "link.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		rooted, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rooted.Close()
		for _, name := range []string{"missing.md", "directory", "link.md"} {
			if _, _, _, err := readPinnedPruneRegular(rooted, name); err == nil {
				t.Fatalf("unsafe rooted read %q was accepted", name)
			}
		}
	})

	t.Run("wrong expected identity", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "native.md"), "native")
		mustWriteFile(t, filepath.Join(root, "other.md"), "other")
		rooted, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rooted.Close()
		_, _, pin, err := readPinnedPruneRegular(rooted, "native.md")
		if err != nil {
			t.Fatal(err)
		}
		defer pin.Close()
		other, err := rooted.Lstat("other.md")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := verifyPinnedPruneRegular(rooted, "native.md", pin, other, pruneSHA256([]byte("native"))); err == nil || !strings.Contains(err.Error(), "changed identity") {
			t.Fatalf("mismatched expected identity was accepted: %v", err)
		}
	})

	t.Run("wrong expected digest", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "native.md"), "native")
		rooted, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rooted.Close()
		_, info, pin, err := readPinnedPruneRegular(rooted, "native.md")
		if err != nil {
			t.Fatal(err)
		}
		defer pin.Close()
		if _, _, err := verifyPinnedPruneRegular(rooted, "native.md", pin, info, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "atomic detach boundary") {
			t.Fatalf("wrong expected digest was accepted: %v", err)
		}
	})

	t.Run("unlinked pinned pathname", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "native.md"), "native")
		rooted, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rooted.Close()
		_, info, pin, err := readPinnedPruneRegular(rooted, "native.md")
		if err != nil {
			t.Fatal(err)
		}
		defer pin.Close()
		if err := rooted.Remove("native.md"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := verifyPinnedPruneRegular(rooted, "native.md", pin, info, pruneSHA256([]byte("native"))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unlinked pinned pathname returned %v", err)
		}
	})

	t.Run("closed source pin", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "native.md"), "native")
		rooted, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rooted.Close()
		_, info, pin, err := readPinnedPruneRegular(rooted, "native.md")
		if err != nil {
			t.Fatal(err)
		}
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := verifyPinnedPruneRegular(rooted, "native.md", pin, info, pruneSHA256([]byte("native"))); err == nil {
			t.Fatal("closed source pin was accepted")
		}
	})

	if _, _, err := verifyPinnedPruneRegular(nil, "native.md", nil, nil, ""); err == nil {
		t.Fatal("missing pinned identity was accepted")
	}
}

func TestPrunePreflightRejectsInvalidAndReappearedCandidates(t *testing.T) {
	if err := preflightPruneNoReplaceInRoot(nil, applyHooks{}, nil); err == nil {
		t.Fatal("nil no-replace plan was accepted")
	}
	if err := preflightPruneCandidates(context.Background(), nil, nil); err == nil {
		t.Fatal("nil candidate plan was accepted")
	}

	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "ready.md"), "ready")
	mustWriteFile(t, filepath.Join(root, "reappeared.md"), "reappeared")
	snapshot, projectRoot, err := captureAndOpenPruneRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer projectRoot.Close()
	readyInfo, err := projectRoot.Lstat("ready.md")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ready candidate can be re-read without a retained plan pin", func(t *testing.T) {
		plan := &Plan{root: snapshot, Candidates: []Candidate{{
			Path:        "ready.md",
			Status:      StatusReady,
			SHA256:      pruneSHA256([]byte("ready")),
			currentInfo: readyInfo,
		}}}
		if err := preflightPruneCandidates(context.Background(), plan, projectRoot); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("canceled preflight", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		plan := &Plan{root: snapshot, Candidates: []Candidate{{Path: "ready.md", Status: StatusReady}}}
		if err := preflightPruneCandidates(ctx, plan, projectRoot); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled preflight returned %v", err)
		}
	})

	t.Run("missing candidate remains absent", func(t *testing.T) {
		plan := &Plan{root: snapshot, Candidates: []Candidate{{Path: "missing.md", Status: StatusMissing}}}
		if err := preflightPruneCandidates(context.Background(), plan, projectRoot); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing candidate reappears", func(t *testing.T) {
		plan := &Plan{root: snapshot, Candidates: []Candidate{{Path: "reappeared.md", Status: StatusMissing}}}
		if err := preflightPruneCandidates(context.Background(), plan, projectRoot); err == nil || !strings.Contains(err.Error(), "reappeared") {
			t.Fatalf("reappeared candidate was accepted: %v", err)
		}
	})

	t.Run("unsafe candidate path", func(t *testing.T) {
		plan := &Plan{root: snapshot, Candidates: []Candidate{{Path: "../outside", Status: StatusMissing}}}
		if err := preflightPruneCandidates(context.Background(), plan, projectRoot); err == nil {
			t.Fatal("escaping candidate was accepted")
		}
	})

	if err := preflightPruneNoReplaceInRoot(&Plan{root: snapshot}, applyHooks{}, nil); err == nil {
		t.Fatal("nil pinned project root was accepted")
	}
}

func assertPrunePlanPinsClosed(t *testing.T, plan *Plan) {
	t.Helper()
	if plan == nil {
		t.Fatal("expected prune plan")
	}
	for _, candidate := range plan.Candidates {
		if candidate.currentPin != nil {
			t.Fatalf("prune candidate retained runtime pin: %#v", candidate)
		}
	}
}

func TestPruneParentSelectionRejectsUnusableRootsAndPaths(t *testing.T) {
	for _, damage := range []string{"nil-root", "closed-root", "missing-snapshot", "empty-path", "escaping-path", "missing-parent", "file-parent", "symlink-parent"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			snapshot, root, err := captureAndOpenPruneRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			candidate := Candidate{Path: "nested/file.md"}
			switch damage {
			case "nil-root":
				root = nil
			case "closed-root":
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing-snapshot":
				snapshot = pruneRootSnapshot{}
			case "empty-path":
				candidate.Path = ""
			case "escaping-path":
				candidate.Path = "../file.md"
			case "file-parent":
				mustWriteFile(t, filepath.Join(dir, "nested"), "original")
			case "symlink-parent":
				if err := os.Symlink(t.TempDir(), filepath.Join(dir, "nested")); err != nil {
					t.Fatal(err)
				}
			}
			if parent, err := openPruneTargetParent(root, snapshot, candidate); err == nil {
				_ = parent.root.Close()
				t.Fatal("unusable prune parent accepted")
			}
		})
	}
}

func TestPruneRetainedRootMustMatchSelectedGeneration(t *testing.T) {
	for _, damage := range []string{"nil", "closed", "different", "missing"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			selected := dir
			switch damage {
			case "nil":
				root = nil
			case "closed":
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			case "different":
				selected = t.TempDir()
			case "missing":
				selected = filepath.Join(dir, "missing")
			}
			if _, duplicate, err := capturePruneRootFromRetained(selected, root); err == nil {
				_ = duplicate.Close()
				t.Fatal("untrusted retained root accepted")
			}
		})
	}
}
