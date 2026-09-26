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
	"github.com/threadgrid/threadpoint/walk"
)

func TestBuildPlanUsesLatestCommittedStageSnapshot(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "native.md")
	mustWriteFile(t, source, "first reviewed source")
	seedStageSourceSnapshotRel(t, root, home, "native.md", source)
	mustWriteFile(t, source, "second reviewed source")
	secondRunID := seedStageSourceSnapshotRel(t, root, home, "native.md", source)

	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, Now: func() time.Time {
		return time.Date(2026, 5, 30, 3, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusReady || plan.Candidates[0].BackupRun != secondRunID {
		t.Fatalf("latest stage snapshot was not selected: %#v", plan.Candidates)
	}
}

func TestBuildPlanIgnoresNonCommitSnapshots(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "native.md")
	mustWriteFile(t, source, "source")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "scan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("native.md", source, "stage-source"); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 0 {
		t.Fatalf("non-commit snapshot entered prune plan: %#v", plan.Candidates)
	}
}

func TestBuildPlanRequiresBoundCanonicalCommitGeneration(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	sourcePath := filepath.Join(root, "native.md")
	targetPath := filepath.Join(root, "AGENTS.md")
	sourceBody := []byte("reviewed source\n")
	targetBody := []byte("committed canonical\n")
	mustWriteFile(t, sourcePath, string(sourceBody))
	mustWriteFile(t, targetPath, string(targetBody))
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: backup.PendingCommitOperation, RunID: "commit-stage-bound"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration("stage-bound", "native.md", pruneSHA256(sourceBody), "AGENTS.md", pruneSHA256(targetBody), int64(len(targetBody))); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, sourceBody, os.FileMode(0o600), []byte("prior canonical\n"), 0o600, true)

	if err := store.FinalizePendingCommit(); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusReady {
		t.Fatalf("bound canonical transaction did not authorize prune: %+v", plan)
	}
	mustWriteFile(t, targetPath, "replaced canonical\n")
	plan, err = BuildPlan(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 0 || len(plan.Warnings) == 0 {
		t.Fatalf("mismatched canonical transaction still authorized prune: %+v", plan)
	}
}

func TestBuildPlanRejectsCommitTransactionWithExtraStageSource(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	sourceBody := []byte("reviewed source\n")
	extraBody := []byte("unrelated source\n")
	targetBody := []byte("committed canonical\n")
	mustWriteFile(t, filepath.Join(root, "native.md"), string(sourceBody))
	mustWriteFile(t, filepath.Join(root, "unrelated.md"), string(extraBody))
	mustWriteFile(t, filepath.Join(root, "AGENTS.md"), string(targetBody))
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root, HomeDir: home, Operation: backup.PendingCommitOperation, RunID: "commit-stage-bound",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration("stage-bound", "native.md", pruneSHA256(sourceBody), "AGENTS.md", pruneSHA256(targetBody), int64(len(targetBody))); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, sourceBody, os.FileMode(0o600), []byte("prior canonical\n"), 0o600, true)

	if err := store.FinalizePendingCommit(); err != nil {
		t.Fatal(err)
	}
	extra := store.Manifest.Entries[0]
	extra.Path = "unrelated.md"
	extra.SHA256 = pruneSHA256(extraBody)
	extra.Size = int64(len(extraBody))
	store.Manifest.Entries = append(store.Manifest.Entries, extra)
	writeMalformedBackupManifestFixture(t, store)

	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home})
	if err == nil || plan != nil || (!strings.Contains(err.Error(), "exactly one staged source") && !strings.Contains(err.Error(), "unsupported entry")) {
		t.Fatalf("transaction with extra stage source was not rejected visibly: plan=%+v err=%v", plan, err)
	}
}

func TestBuildPlanPreservesHistoricalAdditiveCommitAuthorization(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	firstSource := []byte("first native source\n")
	secondSource := []byte("second native source\n")
	mustWriteFile(t, filepath.Join(root, "first.md"), string(firstSource))
	mustWriteFile(t, filepath.Join(root, "second.md"), string(secondSource))
	firstTarget := []byte("first canonical generation\n")
	secondTarget := append(append([]byte(nil), firstTarget...), []byte("\nsecond canonical generation\n")...)
	mustWriteFile(t, filepath.Join(root, "AGENTS.md"), string(secondTarget))
	for index, commit := range []struct {
		id     string
		source string
		body   []byte
		target []byte
	}{
		{id: "stage-first", source: "first.md", body: firstSource, target: firstTarget},
		{id: "stage-second", source: "second.md", body: secondSource, target: secondTarget},
	} {
		store, err := backup.NewStore(backup.StoreOptions{
			ProjectRoot: root, HomeDir: home, Operation: backup.PendingCommitOperation, RunID: "commit-" + commit.id,
			Now: func() time.Time { return time.Date(2026, 8, 3, index+1, 0, 0, 0, time.UTC) },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}

		if err := store.ConfigurePendingCommitGeneration(commit.id, commit.source, pruneSHA256(commit.body), "AGENTS.md", pruneSHA256(commit.target), int64(len(commit.target))); err != nil {
			t.Fatal(err)
		}
		store = publishPendingFixture(t, store, commit.body, os.FileMode(0o600), []byte("prior canonical\n"), 0o600, true)

		if err := store.FinalizePendingCommit(); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 2 || plan.Candidates[0].Path != "first.md" || plan.Candidates[1].Path != "second.md" {
		t.Fatalf("historical additive commit was not preserved: %+v", plan)
	}
}

func TestBuildPlanRejectsOversizedArtifact(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	body := strings.Repeat("x", walk.DefaultMaxFileSize+1)
	mustWriteFile(t, source, body)
	seedStageSourceSnapshot(t, root, home, source)

	_, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home})
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("expected bounded-size refusal, got %v", err)
	}
}

func TestBuildPlanIgnoresUnrelatedAgentsImportsTree(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	mustWriteFile(t, filepath.Join(root, ".agents", "imports", "agent-artifacts", "manifest.json"), "{}")
	seedStageSourceSnapshot(t, root, home, source)

	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, Now: func() time.Time {
		return time.Date(2026, 5, 30, 2, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusReady {
		t.Fatalf("expected ready prune candidate, got %#v", plan.Candidates)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("unrelated imports tree changed the prune plan: %#v", plan.Warnings)
	}
}

func TestBuildCandidateClassifiesFailClosedStates(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "native.md"), "current")
	snapshot, projectRoot, err := captureAndOpenPruneRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer projectRoot.Close()

	candidate, err := buildCandidate(projectRoot, snapshot, root, "run", backup.Entry{Path: "native.md"})
	if err != nil || candidate.Status != StatusChanged || candidate.Code != "prune_backup_hash_missing" {
		t.Fatalf("missing digest classification=%+v err=%v", candidate, err)
	}
	candidate, err = buildCandidate(projectRoot, snapshot, root, "run", backup.Entry{Path: "missing.md", SHA256: pruneSHA256([]byte("missing"))})
	if err != nil || candidate.Status != StatusMissing {
		t.Fatalf("missing artifact classification=%+v err=%v", candidate, err)
	}
	candidate, err = buildCandidate(projectRoot, snapshot, root, "run", backup.Entry{Path: "native.md", SHA256: pruneSHA256([]byte("reviewed"))})
	if err != nil || candidate.Status != StatusChanged || candidate.Code != "prune_artifact_changed" {
		t.Fatalf("changed artifact classification=%+v err=%v", candidate, err)
	}
	if _, err := buildCandidate(projectRoot, snapshot, root, "run", backup.Entry{Path: "../outside", SHA256: pruneSHA256(nil)}); err == nil {
		t.Fatal("escaping backup entry was accepted")
	}
}

func TestPruneSourceVerificationRejectsUnreadableRetainedDescriptor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "native.md")
	mustWriteFile(t, path, "original")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := root.OpenFile("native.md", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyPinnedPruneRegular(root, "native.md", file, info, pruneSHA256([]byte("original"))); err == nil {
		t.Fatal("write-only source descriptor accepted")
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "original" {
		t.Fatalf("source changed: %q %v", body, err)
	}
}

func TestPruneAPIsRefuseUnresolvableRelativePaths(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"normalize-root", func() error { _, err := normalizeOptions(Options{Root: "relative", HomeDir: home}); return err }},
		{"normalize-default", func() error { _, err := normalizeOptions(Options{HomeDir: home}); return err }},
		{"normalize-home", func() error { _, err := normalizeOptions(Options{Root: project, HomeDir: "relative"}); return err }},
		{"plan", func() error {
			_, err := BuildPlan(context.Background(), Options{Root: "relative", HomeDir: home})
			return err
		}},
		{"apply", func() error {
			_, err := Apply(context.Background(), Options{Root: "relative", HomeDir: home, Yes: true})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("unresolvable prune path accepted")
			}
		})
	}
}
