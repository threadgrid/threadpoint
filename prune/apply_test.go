// SPDX-License-Identifier: Apache-2.0

package prune

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/threadgrid/threadpoint/backup"
)

func TestApplyRemovesMatchingBackedUpArtifact(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)

	report, err := Apply(context.Background(), Options{
		Root:    root,
		HomeDir: home,
		Yes:     true,
		Now: func() time.Time {
			return time.Date(2026, 5, 30, 2, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("expected native artifact to be removed, stat err: %v", err)
	}
	if len(report.Resolutions) != 1 || !report.Resolutions[0].Removed {
		t.Fatalf("expected removed resolution, got %#v", report.Resolutions)
	}
	if _, err := os.Stat(report.Resolutions[0].Backup); err != nil {
		t.Fatalf("expected prune backup for removed artifact: %v", err)
	}
	if report.Resolutions[0].Recovery == "" {
		t.Fatalf("expected retained original recovery path, got %#v", report.Resolutions[0])
	}
	if body, err := os.ReadFile(report.Resolutions[0].Recovery); err != nil || string(body) != "claude" {
		t.Fatalf("expected retained original at reported recovery path, body=%q err=%v", body, err)
	}
}

func TestApplyKeepsBackupWritesOnLockRetainedProductHome(t *testing.T) {
	root := t.TempDir()
	parent := t.TempDir()
	home := filepath.Join(parent, "threadpoint")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)
	detached := home + ".detached"
	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		afterPlan: func(*Plan, *os.Root) error {
			if err := os.Rename(home, detached); err != nil {
				return err
			}
			if err := os.MkdirAll(home, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(home, "replacement-sentinel"), []byte("replacement\n"), 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Resolutions) != 1 || !report.Resolutions[0].Removed {
		t.Fatalf("prune report=%+v", report)
	}
	if _, err := os.Stat(filepath.Join(home, ".threadpoint", "backups")); !os.IsNotExist(err) {
		t.Fatalf("prune wrote through replacement product home: %v", err)
	}
	retainedHome, err := os.OpenRoot(filepath.Join(detached, ".threadpoint"))
	if err != nil {
		t.Fatal(err)
	}
	defer retainedHome.Close()
	runs, err := backup.ListRunsFromRoot(root, retainedHome, filepath.Join(home, ".threadpoint"), "")
	if err != nil {
		t.Fatal(err)
	}
	foundPrune := false
	for _, run := range runs {
		if run.Operation == "prune" {
			foundPrune = true
		}
	}
	if !foundPrune {
		t.Fatalf("retained product-home generation has no prune run: %+v", runs)
	}
}

func TestApplyRejectsChangedArtifact(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)
	mustWriteFile(t, source, "changed")

	_, err := Apply(context.Background(), Options{Root: root, HomeDir: home, Yes: true})
	if err == nil {
		t.Fatal("expected changed artifact to block prune")
	}
	if !strings.Contains(err.Error(), "changed since its reviewed commit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyRecordsPrunedSourceMode(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	if err := os.Chmod(source, 0o600); err != nil {
		t.Fatal(err)
	}
	seedStageSourceSnapshot(t, root, home, source)
	if _, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{}); err != nil {
		t.Fatal(err)
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
			if !entry.ModeKnown {
				t.Fatal("pruned-native entry did not record the source mode")
			}
			if os.FileMode(entry.Mode).Perm() != 0o600 {
				t.Fatalf("pruned source mode = %o, want 0600", entry.Mode)
			}
			return
		}
	}
	t.Fatal("no pruned-native entry recorded")
}

func TestValidateCommitTransactionForPruneFailsClosed(t *testing.T) {
	root := t.TempDir()
	targetBody := []byte("committed canonical\n")
	writeTarget := func(body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTarget(targetBody)
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer projectRoot.Close()

	sourceBody := []byte("reviewed native source\n")
	validRun := func() backup.Run {
		return backup.Run{
			RunID:     "commit-bound",
			Operation: backup.CommittedCommitOperation,
			CommitTransaction: &backup.CommitTransaction{
				State:        backup.CommitTransactionCommitted,
				StageID:      "stage-bound",
				SourcePath:   "native.md",
				SourceSHA256: pruneSHA256(sourceBody),
				TargetPath:   "AGENTS.md",
				TargetSHA256: pruneSHA256(targetBody),
				TargetSize:   int64(len(targetBody)),
			},
			Entries: []backup.Entry{{
				Role: "stage-source", Path: "native.md", SHA256: pruneSHA256(sourceBody), Mode: 0o600, ModeKnown: true,
			}, {
				Role: "canonical-prior", Path: "AGENTS.md", Absent: true,
			}},
		}
	}

	tests := []struct {
		name string
		edit func(*backup.Run)
		want string
	}{
		{name: "unfinished transaction", edit: func(run *backup.Run) { run.CommitTransaction.State = backup.CommitTransactionPending }, want: "not finalized"},
		{name: "invalid digest", edit: func(run *backup.Run) { run.CommitTransaction.SourceSHA256 = "invalid" }, want: "digest is invalid"},
		{name: "missing staged source", edit: func(run *backup.Run) { run.Entries = run.Entries[1:] }, want: "source does not match"},
		{name: "duplicate staged source", edit: func(run *backup.Run) { run.Entries = append(run.Entries, run.Entries[0]) }, want: "unbound staged source"},
		{name: "missing prior canonical generation", edit: func(run *backup.Run) { run.Entries = run.Entries[:1] }, want: "exactly one prior canonical generation"},
		{name: "escaping canonical path", edit: func(run *backup.Run) {
			run.CommitTransaction.TargetPath = "../outside"
			run.Entries[1].Path = "../outside"
		}, want: "unsafe relative path"},
		{name: "missing canonical target", edit: func(run *backup.Run) {
			run.CommitTransaction.TargetPath = "missing.md"
			run.Entries[1].Path = "missing.md"
		}, want: "no such file"},
		{name: "short canonical target", edit: func(run *backup.Run) { run.CommitTransaction.TargetSize = int64(len(targetBody) + 1) }, want: "shorter"},
		{name: "different canonical generation", edit: func(run *backup.Run) { run.CommitTransaction.TargetSHA256 = strings.Repeat("0", 64) }, want: "no longer contains"},
		{name: "non-additive canonical extension", edit: func(run *backup.Run) {
			writeTarget(append(append([]byte(nil), targetBody...), []byte("tail")...))
		}, want: "does not extend"},
		{name: "missing canonical generation length", edit: func(run *backup.Run) {
			run.CommitTransaction.TargetSize = 0
		}, want: "generation length"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeTarget(targetBody)
			run := validRun()
			test.edit(&run)
			err := validateCommitTransactionForPrune(projectRoot, run)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q refusal, got %v", test.want, err)
			}
		})
	}
	if err := validateCommitTransactionForPrune(nil, validRun()); err == nil || !strings.Contains(err.Error(), "not finalized") {
		t.Fatalf("nil project root was not rejected: %v", err)
	}
}

func TestApplyRefusesWithoutConfirmationAndSkipsMissingCandidate(t *testing.T) {
	t.Run("confirmation required", func(t *testing.T) {
		root := t.TempDir()
		home := t.TempDir()
		source := filepath.Join(root, "native.md")
		mustWriteFile(t, source, "reviewed")
		seedStageSourceSnapshotRel(t, root, home, "native.md", source)
		if report, err := Apply(context.Background(), Options{Root: root, HomeDir: home}); err == nil || report != nil || !strings.Contains(err.Error(), "requires --confirm") {
			t.Fatalf("unconfirmed prune report=%+v err=%v", report, err)
		}
		if body, err := os.ReadFile(source); err != nil || string(body) != "reviewed" {
			t.Fatalf("unconfirmed prune changed source, body=%q err=%v", body, err)
		}
	})

	t.Run("already absent artifact", func(t *testing.T) {
		root := t.TempDir()
		home := t.TempDir()
		source := filepath.Join(root, "native.md")
		mustWriteFile(t, source, "reviewed")
		seedStageSourceSnapshotRel(t, root, home, "native.md", source)
		if err := os.Remove(source); err != nil {
			t.Fatal(err)
		}
		report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, Yes: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Resolutions) != 1 || !report.Resolutions[0].Skipped || report.Resolutions[0].Removed {
			t.Fatalf("missing prune resolution=%+v", report.Resolutions)
		}
	})

	t.Run("canceled planning", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if report, err := Apply(ctx, Options{Root: t.TempDir(), HomeDir: t.TempDir(), Yes: true}); report != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled apply report=%+v err=%v", report, err)
		}
	})
}
