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

func TestApplyDoesNotDetachReplacementAfterSealedBackup(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)
	var replacementInfo os.FileInfo

	_, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		afterBackup: func(_ Candidate, parent *os.Root, base string) {
			if removeErr := parent.Remove(base); removeErr != nil {
				t.Fatal(removeErr)
			}
			file, createErr := parent.OpenFile(base, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if createErr != nil {
				t.Fatal(createErr)
			}
			if _, writeErr := file.WriteString("claude"); writeErr != nil {
				t.Fatal(writeErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			replacementInfo, createErr = parent.Lstat(base)
			if createErr != nil {
				t.Fatal(createErr)
			}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed after its prune backup was sealed") {
		t.Fatalf("expected post-backup replacement refusal, got %v", err)
	}
	current, statErr := os.Lstat(source)
	if statErr != nil || replacementInfo == nil || !os.SameFile(replacementInfo, current) {
		t.Fatalf("concurrent identical-content replacement identity was lost: current=%#v err=%v", current, statErr)
	}
	if body, readErr := os.ReadFile(source); readErr != nil || string(body) != "claude" {
		t.Fatalf("concurrent replacement was removed, body=%q err=%v", body, readErr)
	}
}

func TestApplyRefusesReplacementAtDetachBoundaryWithoutRemovingIt(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)

	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		beforeRecoveryRename: func(_ Candidate, parent *os.Root, base string, _ *os.Root, _ string) {
			if removeErr := parent.Remove(base); removeErr != nil {
				t.Fatal(removeErr)
			}
			writeRootFile(t, parent, base, "boundary replacement")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed at the atomic detach boundary") {
		t.Fatalf("expected detach-boundary replacement refusal, got report=%#v err=%v", report, err)
	}
	if body, readErr := os.ReadFile(source); readErr != nil || string(body) != "boundary replacement" {
		t.Fatalf("detach-boundary replacement was removed, body=%q err=%v", body, readErr)
	}
	if report == nil || len(report.Resolutions) != 1 {
		t.Fatalf("expected a partial refusal report, got %#v", report)
	}
	resolution := report.Resolutions[0]
	if resolution.Removed || resolution.Recovery != "" || resolution.Backup != "" {
		t.Fatalf("pre-detach refusal reported stale mutation artifacts: %#v", resolution)
	}
}

func TestApplyRefusesOccupiedRecoveryDestinationWithoutRemovingTarget(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)

	_, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		beforeRecoveryRename: func(_ Candidate, _ *os.Root, _ string, recovery *os.Root, item string) {
			writeRootFile(t, recovery, item, "raced recovery content")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "recovery destination appeared") {
		t.Fatalf("expected occupied-recovery refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(source); readErr != nil || string(body) != "claude" {
		t.Fatalf("canonical target changed, body=%q err=%v", body, readErr)
	}
}

func TestApplyNeverRollsRacedRecoveryReplacementIntoCanonicalPath(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)

	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		afterDetach: func(_ Candidate, _ *os.Root, _ string, recovery *os.Root, item string) {
			if removeErr := recovery.Remove(item); removeErr != nil {
				t.Fatal(removeErr)
			}
			writeRootFile(t, recovery, item, "raced recovery replacement")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed identity before rollback") {
		t.Fatalf("expected raced recovery identity refusal, got report=%#v err=%v", report, err)
	}
	if _, statErr := os.Lstat(source); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("raced recovery replacement was installed as canonical content: %v", statErr)
	}
	if report == nil || len(report.Resolutions) != 1 || report.Resolutions[0].Recovery == "" || report.Resolutions[0].Backup == "" {
		t.Fatalf("expected sealed backup and truthfully labeled raced recovery, got %#v", report)
	}
	if body, readErr := os.ReadFile(report.Resolutions[0].Recovery); readErr != nil || string(body) != "raced recovery replacement" {
		t.Fatalf("raced recovery content was not retained truthfully, body=%q err=%v", body, readErr)
	}
}

func TestApplyPrejournalsRunLocalRecoveryPayloadBeforeDetach(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)
	observed := false
	stop := errors.New("stop after recovery journal")

	_, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		afterRecoveryJournal: func(candidate Candidate, recoveryPath string) error {
			observed = true
			if filepath.Dir(filepath.Dir(recoveryPath)) != filepath.Dir(source) {
				t.Fatalf("recovery is not adjacent to target parent: %s", recoveryPath)
			}
			runs, listErr := backup.ListRuns(root, home, "")
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, run := range runs {
				for _, entry := range run.Entries {
					if entry.Role == "pruned-native" && entry.Path == candidate.Path && entry.RecoveryPath == entry.BackupPath && entry.RecoveryPath != recoveryPath {
						return stop
					}
				}
			}
			t.Fatalf("run-local recovery payload was not persisted separately from quarantine path %s", recoveryPath)
			return stop
		},
	})
	if !errors.Is(err, stop) || !observed {
		t.Fatalf("expected pre-journal hook refusal, got observed=%v err=%v", observed, err)
	}
	if body, readErr := os.ReadFile(source); readErr != nil || string(body) != "claude" {
		t.Fatalf("pre-journal refusal mutated target, body=%q err=%v", body, readErr)
	}
}

func TestApplyInterruptionAfterDetachLeavesExactJournaledRecovery(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)
	interrupted := errors.New("simulated process interruption")

	func() {
		defer func() {
			recovered, _ := recover().(error)
			if !errors.Is(recovered, interrupted) {
				t.Fatalf("unexpected interruption value: %v", recovered)
			}
		}()
		_, _ = applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
			afterDetach: func(_ Candidate, _ *os.Root, _ string, _ *os.Root, _ string) {
				panic(interrupted)
			},
		})
	}()
	if _, statErr := os.Lstat(source); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected canonical target to be detached at interruption, stat err=%v", statErr)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		for _, entry := range run.Entries {
			if entry.Role != "pruned-native" || entry.Path != "CLAUDE.md" {
				continue
			}
			if entry.RecoveryPath == "" {
				t.Fatal("interrupted detach had no pre-journaled recovery path")
			}
			if body, readErr := os.ReadFile(entry.RecoveryPath); readErr != nil || string(body) != "claude" {
				t.Fatalf("journal does not name detached original, body=%q err=%v", body, readErr)
			}
			return
		}
	}
	t.Fatal("interrupted prune backup entry was not persisted")
}

func TestApplyInterruptionAfterJournalBeforeDetachLeavesCanonicalAndIntent(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)
	interrupted := errors.New("simulated pre-detach interruption")

	func() {
		defer func() {
			recovered, _ := recover().(error)
			if !errors.Is(recovered, interrupted) {
				t.Fatalf("unexpected interruption value: %v", recovered)
			}
		}()
		_, _ = applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
			afterRecoveryJournal: func(_ Candidate, _ string) error {
				panic(interrupted)
			},
		})
	}()
	if body, err := os.ReadFile(source); err != nil || string(body) != "claude" {
		t.Fatalf("pre-detach interruption changed canonical target, body=%q err=%v", body, err)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		for _, entry := range run.Entries {
			if entry.Role != "pruned-native" || entry.Path != "CLAUDE.md" {
				continue
			}
			if entry.RecoveryPath == "" {
				t.Fatal("pre-detach interruption had no exact recovery intent")
			}
			if body, readErr := os.ReadFile(entry.RecoveryPath); readErr != nil || string(body) != "claude" {
				t.Fatalf("pre-detach run-local recovery payload = %q, err=%v", body, readErr)
			}
			return
		}
	}
	t.Fatal("pre-detach interrupted prune entry was not persisted")
}

func TestApplyRetainsLateHandleWritesAtReportedRecoveryPath(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)
	var openHandle *os.File

	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		beforeRecoveryRename: func(_ Candidate, parent *os.Root, base string, _ *os.Root, _ string) {
			var openErr error
			openHandle, openErr = parent.OpenFile(base, os.O_WRONLY, 0)
			if openErr != nil {
				t.Fatal(openErr)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openHandle.WriteAt([]byte("CLAUDE"), 0); err != nil {
		t.Fatal(err)
	}
	if err := openHandle.Close(); err != nil {
		t.Fatal(err)
	}
	recovery := report.Resolutions[0].Recovery
	if body, readErr := os.ReadFile(recovery); readErr != nil || string(body) != "CLAUDE" {
		t.Fatalf("late handle write was not retained at reported recovery, body=%q err=%v", body, readErr)
	}
	runs, listErr := backup.ListRuns(root, home, "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	persistedRecovery := ""
	for _, run := range runs {
		for _, entry := range run.Entries {
			if entry.Role == "pruned-native" && entry.Path == "CLAUDE.md" && entry.RecoveryPath == entry.BackupPath {
				persistedRecovery = entry.RecoveryPath
			}
		}
	}
	if persistedRecovery == "" {
		t.Fatal("prune manifest did not retain its run-local recovery payload")
	}
	if recovery == persistedRecovery {
		t.Fatalf("report returned the run-local payload instead of the external quarantine path: %#v", report.Resolutions[0])
	}
}

func TestApplyFinalReportClearsRecoveryPathWhoseInodeChanged(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	first := filepath.Join(root, "a", "CLAUDE.md")
	second := filepath.Join(root, "b", "AGENTS.md")
	mustWriteFile(t, first, "first")
	mustWriteFile(t, second, "second")
	seedStageSourceSnapshotRel(t, root, home, "a/CLAUDE.md", first)
	seedStageSourceSnapshotRel(t, root, home, "b/AGENTS.md", second)
	var firstRecovery string

	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		afterRecoveryJournal: func(_ Candidate, recoveryPath string) error {
			if firstRecovery == "" {
				firstRecovery = recoveryPath
				return nil
			}
			if removeErr := os.Remove(firstRecovery); removeErr != nil {
				t.Fatal(removeErr)
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "finalizing prune recovery claims") {
		t.Fatalf("expected stale final recovery claim error, got report=%#v err=%v", report, err)
	}
	if report == nil || len(report.Resolutions) != 2 || report.Resolutions[0].Recovery != "" || report.Resolutions[1].Recovery == "" {
		t.Fatalf("final report retained a stale recovery path: %#v", report)
	}
}

func TestPruneRecoveryCapabilityRejectsMissingAndReplacedState(t *testing.T) {
	t.Run("nil recovery", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		if err := (*pruneRecovery)(nil).verify(fixture.parent); err == nil {
			t.Fatal("nil recovery capability was accepted")
		}
	})

	t.Run("missing recovery item", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		if err := verifyPruneRecoveryPath(fixture.parent, fixture.recovery); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing recovery item returned %v", err)
		}
		if _, pin, err := pinCurrentPruneRecoveryPath(fixture.parent, fixture.recovery); !errors.Is(err, os.ErrNotExist) || pin != nil {
			t.Fatalf("missing recovery item info pin=%v err=%v", pin, err)
		}
	})

	t.Run("valid recovery item remains rooted", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		addPruneRecoveryItem(t, fixture, "original")
		if err := verifyPruneRecoveryPath(fixture.parent, fixture.recovery); err != nil {
			t.Fatal(err)
		}
		info, pin, err := pinCurrentPruneRecoveryPath(fixture.parent, fixture.recovery)
		if err != nil {
			t.Fatal(err)
		}
		if info == nil || pin == nil {
			t.Fatalf("rooted recovery pin info=%v pin=%v", info, pin)
		}
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("replaced recovery item", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		original := addPruneRecoveryItem(t, fixture, "original")
		if err := fixture.recovery.root.Remove(fixture.recovery.itemName); err != nil {
			t.Fatal(err)
		}
		writeRootFile(t, fixture.recovery.root, fixture.recovery.itemName, "replacement")
		if err := verifyPruneRecoveryPath(fixture.parent, fixture.recovery); err == nil || !strings.Contains(err.Error(), "changed identity") {
			t.Fatalf("replacement recovery item was accepted: %v", err)
		}
		current, pin, err := pinCurrentPruneRecoveryPath(fixture.parent, fixture.recovery)
		if err != nil {
			t.Fatal(err)
		}
		defer pin.Close()
		if safefs.SameFileGeneration(original, current) {
			t.Fatal("replacement recovery item retained original identity")
		}
	})

	t.Run("replaced recovery container", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		container := filepath.Join(fixture.root, fixture.parent.path, fixture.recovery.containerName)
		if err := os.Rename(container, container+".detached"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(container, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := fixture.recovery.verify(fixture.parent); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("replacement recovery container was accepted: %v", err)
		}
	})

	t.Run("closed recovery descriptor", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		if err := fixture.recovery.root.Close(); err != nil {
			t.Fatal(err)
		}
		if err := fixture.recovery.verify(fixture.parent); err == nil {
			t.Fatal("closed recovery descriptor was accepted")
		}
	})
}

func TestRecoveryInspectionRejectsChangedRetainedPaths(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(*pruneRecoveryFixture) error
	}{
		{"verify", func(f *pruneRecoveryFixture) error { return verifyPruneRecoveryPath(f.parent, f.recovery) }},
		{"pin", func(f *pruneRecoveryFixture) error {
			_, pin, err := pinCurrentPruneRecoveryPath(f.parent, f.recovery)
			if pin != nil {
				_ = pin.Close()
			}
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			for _, mutation := range []struct {
				name   string
				change func(*testing.T, *pruneRecoveryFixture)
			}{
				{"closed recovery", func(t *testing.T, f *pruneRecoveryFixture) {
					t.Helper()
					if err := f.recovery.root.Close(); err != nil {
						t.Fatal(err)
					}
				}},
				{"closed parent", func(t *testing.T, f *pruneRecoveryFixture) {
					t.Helper()
					if err := f.parent.root.Close(); err != nil {
						t.Fatal(err)
					}
				}},
				{"closed project", func(t *testing.T, f *pruneRecoveryFixture) {
					t.Helper()
					if err := f.projectRoot.Close(); err != nil {
						t.Fatal(err)
					}
				}},
				{"missing container", func(t *testing.T, f *pruneRecoveryFixture) {
					t.Helper()
					old := filepath.Join(f.root, f.parent.path, f.recovery.containerName)
					if err := os.Rename(old, old+"-moved"); err != nil {
						t.Fatal(err)
					}
				}},
				{"missing item", func(t *testing.T, f *pruneRecoveryFixture) {
					t.Helper()
					if err := f.recovery.root.Remove(f.recovery.itemName); err != nil {
						t.Fatal(err)
					}
				}},
				{"wrong logical item", func(_ *testing.T, f *pruneRecoveryFixture) { f.recovery.itemRel = "missing/item" }},
				{"different logical inode", func(_ *testing.T, f *pruneRecoveryFixture) { f.recovery.itemRel = "native.md" }},
				{"symlinked logical item", func(t *testing.T, f *pruneRecoveryFixture) {
					t.Helper()
					if err := f.parent.root.Symlink(f.recovery.containerName, "alias"); err != nil {
						t.Skip(err)
					}
					f.recovery.itemRel = filepath.Join("alias", f.recovery.itemName)
				}},
			} {
				t.Run(mutation.name, func(t *testing.T) {
					f := newPruneRecoveryFixture(t)
					addPruneRecoveryItem(t, f, "recovery")
					mutation.change(t, f)
					if err := operation.run(f); err == nil {
						t.Fatal("changed recovery authority accepted")
					}
					body, err := os.ReadFile(filepath.Join(f.root, "nested", "native.md"))
					if err != nil || string(body) != "canonical" {
						t.Fatalf("inspection changed native file: %q %v", body, err)
					}
				})
			}
		})
	}
}

func TestEmptyRecoveryCleanupRetainsUnverifiableOrUnremovableContainer(t *testing.T) {
	for _, damage := range []string{"closed-container", "closed-parent", "raced-content", "read-only-parent"} {
		t.Run(damage, func(t *testing.T) {
			f := newPruneRecoveryFixture(t)
			container := filepath.Dir(f.recovery.path)
			switch damage {
			case "closed-container":
				if err := f.recovery.root.Close(); err != nil {
					t.Fatal(err)
				}
			case "closed-parent":
				if err := f.parent.root.Close(); err != nil {
					t.Fatal(err)
				}
			case "raced-content":
				mustWriteFile(t, filepath.Join(container, "third-party"), "retain")
			case "read-only-parent":
				parent := filepath.Dir(container)
				if err := os.Chmod(parent, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
				if err := os.WriteFile(filepath.Join(parent, "permission-probe"), nil, 0o600); err == nil {
					t.Skip("filesystem does not enforce write permissions")
				}
			}
			if err := cleanupEmptyPruneRecovery(f.parent, f.recovery); err == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			if info, err := os.Lstat(container); err != nil || !info.IsDir() {
				t.Fatalf("recovery container lost: %v", err)
			}
			if damage == "raced-content" {
				if body, err := os.ReadFile(filepath.Join(container, "third-party")); err != nil || string(body) != "retain" {
					t.Fatalf("third-party content lost: %q %v", body, err)
				}
			}
		})
	}
}
