// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"bytes"
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

func TestRestoreTargetRootDefaultsToSelectedRoot(t *testing.T) {
	if target, err := targetRootForEntry(t.TempDir(), ""); err != nil || target == "" {
		t.Fatalf("default target root = %q, %v", target, err)
	}
}

func TestApplyRejectsLogicalRootSymlinkRetargetAtTransactionBoundary(t *testing.T) {
	physical := t.TempDir()
	replacement := t.TempDir()
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
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: logical, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{beforeDetach: func(_ *os.Root, _ string) {
		if err := os.Remove(logical); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(replacement, logical); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: logical, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "now resolves") {
		t.Fatalf("expected logical root retarget refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(filepath.Join(physical, "AGENTS.md")); readErr != nil || string(body) != "after" {
		t.Fatalf("reviewed physical root was mutated, body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(replacement, "AGENTS.md")); !os.IsNotExist(statErr) {
		t.Fatalf("replacement root was mutated: %v", statErr)
	}
}

func TestApplyRejectsPhysicalRootReplacementAtTransactionBoundary(t *testing.T) {
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
	hooks := &restoreHooks{beforeDetach: func(_ *os.Root, _ string) {
		if err := os.Rename(root, moved); err != nil {
			t.Skipf("cannot replace project root on this platform: %v", err)
		}
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(root, "AGENTS.md"), "replacement")
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "root changed") {
		t.Fatalf("expected physical root replacement refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(filepath.Join(root, "AGENTS.md")); readErr != nil || string(body) != "replacement" {
		t.Fatalf("replacement physical path was mutated, body=%q err=%v", body, readErr)
	}
	if body, readErr := os.ReadFile(filepath.Join(moved, "AGENTS.md")); readErr != nil || string(body) != "after" {
		t.Fatalf("reviewed physical tree was mutated, body=%q err=%v", body, readErr)
	}
}

func TestApplyKeepsMutationInsidePinnedParentWhenAncestorIsReplaced(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "nested", "AGENTS.md")
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
	moved := filepath.Join(root, "nested-reviewed")
	outside := t.TempDir()
	hooks := &restoreHooks{beforeDetach: func(_ *os.Root, _ string) {
		if err := os.Rename(filepath.Join(root, "nested"), moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "nested")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil {
		t.Fatal("expected replaced target parent to abort restore")
	}
	if body, readErr := os.ReadFile(filepath.Join(moved, "AGENTS.md")); readErr != nil || string(body) != "after" {
		t.Fatalf("pinned original parent target changed, body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "AGENTS.md")); !os.IsNotExist(statErr) {
		t.Fatalf("restore escaped through replacement ancestor: %v", statErr)
	}
}

func TestApplyRejectsBackupEntryRootOutsideSelectedRoot(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	outsideRoot := t.TempDir()
	outsideTarget := filepath.Join(outsideRoot, "AGENTS.md")
	mustWriteFile(t, outsideTarget, "outside backup")
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

	if _, err := store.BackupFileFromRoot(backup.FileSpec{Rel: "AGENTS.md", Source: outsideTarget, Role: "fixture-target", Root: outsideRoot}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(outsideTarget); err != nil {
		t.Fatal(err)
	}

	_, err = Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true})
	if err == nil {
		t.Fatal("expected restore with out-of-root backup entry to fail safely")
	}
	if !strings.Contains(err.Error(), "outside selected root") {
		t.Fatalf("expected selected-root error, got %v", err)
	}
	if _, err := os.Stat(outsideTarget); !os.IsNotExist(err) {
		t.Fatalf("outside target should not be restored, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("root target should not be restored from out-of-root entry, stat err: %v", err)
	}
}

func TestApplyRejectsRestoreThroughSymlinkAncestor(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "source")
	mustWriteFile(t, filepath.Join(source, "file.txt"), "backup")
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

	if _, err := store.BackupDir("link/native", source, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	outsideRoot := t.TempDir()
	if err := os.Symlink(outsideRoot, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err = Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true})
	if !errors.Is(err, safefs.ErrUnsafeSymlink) {
		t.Fatalf("expected symlink ancestor refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outsideRoot, "native", "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("restore wrote through symlink ancestor, stat err: %v", err)
	}
}

func TestApplyPreflightsEveryRestoreTargetBeforeEarlierMutation(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("native no-replace rename is required for restore apply")
	}
	root := t.TempDir()
	home := t.TempDir()
	alpha := filepath.Join(root, "alpha.md")
	omega := filepath.Join(root, "omega.md")
	mustWriteFile(t, alpha, "before alpha\n")
	mustWriteFile(t, omega, "before omega\n")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("alpha.md", alpha, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackupFile("omega.md", omega, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, alpha, "after alpha\n")
	mustWriteFile(t, omega, "after omega\n")
	if err := store.SetPostHash("alpha.md", alpha); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPostHash("omega.md", omega); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil || len(plan.Candidates) != 2 {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	mustWriteFile(t, omega, "changed after plan\n")
	pending := make([]pendingResolution, 0, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		pending = append(pending, pendingResolution{candidate: candidate, decision: DecisionRestore})
	}

	report, err := applyResolutions(context.Background(), Options{Root: root, HomeDir: home}, plan, pending)
	if err == nil || !strings.Contains(err.Error(), "omega.md") {
		t.Fatalf("expected later-target preflight refusal, report=%#v err=%v", report, err)
	}
	if report != nil {
		t.Fatalf("later-target preflight created an apply report: %#v", report)
	}
	if body, readErr := os.ReadFile(alpha); readErr != nil || string(body) != "after alpha\n" {
		t.Fatalf("earlier restore candidate changed before later refusal, body=%q err=%v", body, readErr)
	}
	if runs, listErr := backup.ListRuns(root, home, ""); listErr != nil {
		t.Fatal(listErr)
	} else {
		for _, run := range runs {
			if run.Operation == restoreOperation {
				t.Fatalf("later-target preflight created restore backup state: %#v", run)
			}
		}
	}
}

func TestApplyRetainsExactRestoreTargetAndRootAcrossPlan(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("exact restore pins and native no-replace rename are required")
	}
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	moved := filepath.Join(root, "AGENTS.reviewed.md")
	mustWriteFile(t, target, "before\n")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after\n")
	})

	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, Yes: true, hooks: &restoreHooks{afterPlan: func(_ *Plan, _ *os.Root) error {
		if err := os.Rename(target, moved); err != nil {
			return err
		}
		mustWriteFile(t, target, "after\n")
		return nil
	}}})
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("expected exact restore-target refusal, report=%#v err=%v", report, err)
	}
	if report != nil {
		t.Fatalf("exact restore-target refusal created apply state: %#v", report)
	}
	for _, path := range []string{target, moved} {
		if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "after\n" {
			t.Fatalf("restore target refusal changed %s, body=%q err=%v", path, got, readErr)
		}
	}
}

func TestApplyRetainsExactSymlinkMarkerIdentityAcrossPlan(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "guide-link")
	if err := os.Symlink("before.md", target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupFile("guide-link", target, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	payload := entry.BackupPath
	movedPayload := payload + ".reviewed"
	body, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}

	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true, hooks: &restoreHooks{afterPlan: func(_ *Plan, _ *os.Root) error {
		if err := os.Rename(payload, movedPayload); err != nil {
			return err
		}
		return os.WriteFile(payload, body, 0o644)
	}}})
	if err == nil || !strings.Contains(err.Error(), "payload") || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("expected exact symlink-marker refusal, report=%#v err=%v", report, err)
	}
	if report != nil {
		t.Fatalf("exact symlink-marker refusal created apply state: %#v", report)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("symlink-marker refusal created target: %v", statErr)
	}
	for _, path := range []string{payload, movedPayload} {
		if got, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(got, body) {
			t.Fatalf("symlink-marker refusal changed %s, bytes=%d err=%v", path, len(got), readErr)
		}
	}
}

func TestRestorePlanPinsCloseOnPlanOnlyAndApplyErrorPaths(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before\n")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after\n")
	})
	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	assertRestorePlanPinsClosed(t, plan)

	var retainedPlan *Plan
	var retainedRoot *os.Root
	stop := errors.New("stop after plan")
	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, Yes: true, hooks: &restoreHooks{afterPlan: func(plan *Plan, pinnedRoot *os.Root) error {
		retainedPlan = plan
		retainedRoot = pinnedRoot
		return stop
	}}})
	if !errors.Is(err, stop) || report != nil {
		t.Fatalf("expected injected post-plan error without report, report=%#v err=%v", report, err)
	}
	assertRestorePlanPinsClosed(t, retainedPlan)
	if retainedRoot == nil {
		t.Fatal("post-plan hook did not retain the root handle for inspection")
	}
	if _, statErr := retainedRoot.Stat("."); statErr == nil {
		t.Fatal("restore root handle remained open after apply error")
	}

	report, err = Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	assertRestorePlanPinsClosed(t, report.Plan)
}

func TestListRunsAndSelectionUseRetainedProductRoot(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = homeRoot.Close() }()
	store, err := backup.NewStoreFromRoot(homeRoot, home, backup.StoreOptions{
		ProjectRoot:     project,
		ThreadpointHome: home,
		Operation:       "fixture",
		RunID:           "rooted-restore-list",
		Now: func() time.Time {
			return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.RecordAbsentFromRoot(homeRoot, home, "AGENTS.md", "fixture"); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		Root:            project,
		HomeDir:         filepath.Dir(home),
		ThreadpointHome: home,
		ProductRoot:     homeRoot,
		Now:             func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	}
	runs, err := ListRuns(opts)
	if err != nil || len(runs) != 1 || runs[0].RunID != store.RunID {
		t.Fatalf("rooted restoration runs = %#v, %v", runs, err)
	}
	latest, err := selectRun(opts)
	if err != nil || latest.RunID != store.RunID {
		t.Fatalf("rooted latest selection = %#v, %v", latest, err)
	}
	opts.BackupID = store.RunID
	selected, err := selectRun(opts)
	if err != nil || selected.RunID != store.RunID {
		t.Fatalf("rooted exact selection = %#v, %v", selected, err)
	}
	opts.BackupID = "missing-run"
	if _, err := selectRun(opts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing rooted selection = %v", err)
	}

	closedRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := closedRoot.Close(); err != nil {
		t.Fatal(err)
	}
	opts.ProductRoot = closedRoot
	if _, err := ListRuns(opts); err == nil {
		t.Fatal("rooted restoration listing accepted a closed product root")
	}
}

func TestCloseRestorePinHandlesNilAndOpenFiles(t *testing.T) {
	if err := closeRestorePin(nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pin")
	if err := os.WriteFile(path, []byte("pin"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeRestorePin(file); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); err == nil {
		t.Fatal("closed restore pin remained usable")
	}
}

func assertRestorePlanPinsClosed(t *testing.T, plan *Plan) {
	t.Helper()
	if plan == nil {
		t.Fatal("expected restore plan")
	}
	for _, candidate := range plan.Candidates {
		if candidate.currentPin != nil || candidate.backupPin != nil || candidate.backupTree != nil {
			t.Fatalf("restore plan retained runtime identity handles after return: %#v", candidate)
		}
	}
}

func TestRestoreParentSelectionRejectsEscapesAndLostRoots(t *testing.T) {
	for _, operation := range []string{"target", "probe"} {
		for _, invalid := range []string{"outside", "root-target", "symlink-parent", "file-parent", "closed-root", "replaced-root", "missing-snapshot"} {
			t.Run(operation+"/"+invalid, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "project")
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				snapshot, project, err := captureAndOpenRestoreRoot(root)
				if err != nil {
					t.Fatal(err)
				}
				defer project.Close()
				candidate := Candidate{Root: root, Target: filepath.Join(root, "nested", "guide.md")}
				switch invalid {
				case "outside":
					candidate.Target = filepath.Join(t.TempDir(), "guide.md")
				case "root-target":
					candidate.Target = root
				case "symlink-parent":
					err = project.Symlink(t.TempDir(), "nested")
				case "file-parent":
					err = project.WriteFile("nested", []byte("preserve"), 0o600)
				case "closed-root":
					err = project.Close()
				case "replaced-root":
					if err = os.Rename(root, root+"-retained"); err == nil {
						err = os.Mkdir(root, 0o700)
					}
				case "missing-snapshot":
					snapshot.info = nil
				}
				if err != nil {
					t.Fatal(err)
				}
				if operation == "target" {
					parent, openErr := openRestoreTargetParent(project, snapshot, candidate)
					err = openErr
					if parent != nil {
						_ = parent.root.Close()
					}
				} else {
					parent, openErr := openRestoreNoReplaceProbeParent(project, snapshot, candidate)
					err = openErr
					if parent != nil {
						_ = parent.root.Close()
					}
				}
				if err == nil {
					t.Fatal("unsafe parent accepted")
				}
				if _, err := os.Lstat(candidate.Target); err == nil && invalid != "root-target" {
					t.Fatal("parent selection created target")
				}
			})
		}
	}
}

func TestRestorePreflightRejectsIncompleteAndChangedCandidateAuthority(t *testing.T) {
	for _, failure := range []string{"nil-plan", "nil-root", "cancelled", "wrong-root", "invalid-path", "missing-payload-pin", "target-appeared", "target-missing", "closed-target-pin", "changed-target-bytes", "closed-root"} {
		t.Run(failure, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			target := filepath.Join(project, "AGENTS.md")
			mustWriteFile(t, target, "before")
			runID := seedFileBackup(t, project, home, target, func() { mustWriteFile(t, target, "after") })
			plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: project, HomeDir: home, BackupID: runID})
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			candidate := plan.Candidates[0]
			ctx := context.Background()
			want := "after"
			switch failure {
			case "nil-plan":
				plan = nil
			case "nil-root":
				root = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "wrong-root":
				candidate.Root = t.TempDir()
			case "invalid-path":
				candidate.Target = filepath.Join(t.TempDir(), "escape")
				candidate.Path = "../escape"
			case "missing-payload-pin":
				candidate.backupPin = nil
			case "target-appeared":
				candidate.currentSnapshot = ""
			case "target-missing":
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			case "closed-target-pin":
				if candidate.currentPin == nil {
					t.Skip("platform has no retained target descriptor")
				}
				_ = candidate.currentPin.Close()
			case "changed-target-bytes":
				want = "concurrent"
				mustWriteFile(t, target, want)
			case "closed-root":
				_ = root.Close()
			}
			if err := preflightRestoreCandidates(ctx, plan, []pendingResolution{{candidate: candidate, decision: DecisionRestore}}, root); err == nil {
				t.Fatal("invalid candidate passed preflight")
			}
			body, err := os.ReadFile(target)
			if failure == "target-missing" {
				if !os.IsNotExist(err) {
					t.Fatalf("preflight recreated missing target: %v", err)
				}
			} else if err != nil || string(body) != want {
				t.Fatalf("preflight changed target: %q %v", body, err)
			}
		})
	}
}

func TestApplyingReviewedPlanRefusesLostRootOrLockAuthority(t *testing.T) {
	for _, failure := range []string{"nil-root", "closed-root", "nil-plan", "wrong-snapshot", "changed-logical-root", "invalid-home", "closed-product", "changed-candidate"} {
		t.Run(failure, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			target := filepath.Join(project, "AGENTS.md")
			mustWriteFile(t, target, "before")
			runID := seedFileBackup(t, project, home, target, func() { mustWriteFile(t, target, "after") })
			opts := Options{Root: project, HomeDir: home, BackupID: runID}
			plan, err := buildRetainedRestorePlan(t, context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			pending := []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}}
			root, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			selected := root
			switch failure {
			case "nil-root":
				selected = nil
			case "closed-root":
				_ = root.Close()
			case "nil-plan":
				plan = nil
			case "wrong-snapshot":
				plan.rootSnapshot.info = nil
			case "changed-logical-root":
				plan.rootSnapshot.logicalRoot = t.TempDir()
			case "invalid-home":
				opts.ThreadpointHome = filepath.Join(home, "blocked")
				mustWriteFile(t, opts.ThreadpointHome, "foreign")
			case "closed-product":
				product, err := os.OpenRoot(home)
				if err != nil {
					t.Fatal(err)
				}
				_ = product.Close()
				opts.ProductRoot = product
			case "changed-candidate":
				pending[0].candidate.currentSnapshot = "invalid"
			}
			if _, err := applyResolutionsWithPinnedRoot(context.Background(), opts, plan, pending, selected); err == nil {
				t.Fatal("lost restore authority accepted")
			}
			body, err := os.ReadFile(target)
			if err != nil || string(body) != "after" {
				t.Fatalf("refused restore changed target: %q %v", body, err)
			}
		})
	}
}

func TestCandidatePinCaptureRefusesIncompleteOrChangedEvidence(t *testing.T) {
	for _, failure := range []string{"nil-root", "nil-candidate", "missing-backup", "missing-backup-pin", "invalid-target", "missing-target", "changed-target-identity", "closed-root"} {
		t.Run(failure, func(t *testing.T) {
			project := t.TempDir()
			target := filepath.Join(project, "memory.md")
			mustWriteFile(t, target, "current")
			root, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			candidate := Candidate{Root: project, Target: target, Path: "memory.md", Absent: true, currentSnapshot: "present"}
			current, err := root.Lstat("memory.md")
			if err != nil {
				t.Fatal(err)
			}
			selected, candidatePtr := root, &candidate
			switch failure {
			case "nil-root":
				selected = nil
			case "nil-candidate":
				candidatePtr = nil
			case "missing-backup":
				candidate.Absent = false
				candidate.BackupPath = filepath.Join(project, "missing-backup")
			case "missing-backup-pin":
				candidate.Absent = false
				candidate.backupInfo = current
				candidate.BackupPath = target
			case "invalid-target":
				candidate.Target = filepath.Join(t.TempDir(), "outside")
			case "missing-target":
				if err := root.Rename("memory.md", "retained"); err != nil {
					t.Fatal(err)
				}
			case "changed-target-identity":
				candidate.currentInfo, err = root.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
			case "closed-root":
				_ = root.Close()
			}
			if err := captureRestoreCandidatePins(selected, candidatePtr); err == nil {
				t.Fatal("untrusted candidate evidence accepted")
			}
			if err := closeRestoreCandidatePins(&candidate); err != nil {
				t.Fatal(err)
			}
			retained := target
			if failure == "missing-target" {
				retained = filepath.Join(project, "retained")
			}
			body, err := os.ReadFile(retained)
			if err != nil || string(body) != "current" {
				t.Fatalf("pin capture changed current file: %q %v", body, err)
			}
		})
	}
}

func TestCurrentTargetPinCaptureRejectsUnavailableOrForeignIdentity(t *testing.T) {
	for _, failure := range []string{"nil-root", "nil-candidate", "nil-identity", "closed-root", "missing-file", "wrong-identity"} {
		t.Run(failure, func(t *testing.T) {
			project := t.TempDir()
			mustWriteFile(t, filepath.Join(project, "memory.md"), "current")
			root, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			info, err := root.Lstat("memory.md")
			if err != nil {
				t.Fatal(err)
			}
			selected := root
			candidate := &Candidate{Path: "memory.md"}
			name := "memory.md"
			switch failure {
			case "nil-root":
				selected = nil
			case "nil-candidate":
				candidate = nil
			case "nil-identity":
				info = nil
			case "closed-root":
				_ = root.Close()
			case "missing-file":
				name = "missing"
			case "wrong-identity":
				info, err = root.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := captureRestoreCurrentPin(selected, name, candidate, info); err == nil {
				t.Fatal("invalid current target pin accepted")
			}
			if candidate != nil && candidate.currentPin != nil {
				_ = candidate.currentPin.Close()
				t.Fatal("failed capture retained a pin")
			}
		})
	}
}
