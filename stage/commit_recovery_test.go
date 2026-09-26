// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

func TestCommitRecoversPendingBackupAfterCanonicalPublication(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	source := filepath.Join(root, "notes.md")
	write(t, source, "project shared source\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectShared},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage: %v %#v", err, report)
	}
	stage := report.Stages[0]
	_, _, review, err := ReadReview(root, home, stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, proposed, err := NormalizeReviewedContent(CommitInput{
		Root: root, Scope: stage.Scope, Provider: stage.Provider, Source: stage.Source, Kind: stage.Kind, ID: stage.ID, Content: review,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceBody, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	transaction := newCommitBackupTransaction(stage, root, home, homeRoot, sourceBody, sourceInfo.Mode())
	if err := transaction.prepare(CanonicalTransactionSnapshot{Target: target, Proposed: proposed}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after canonical publication but before the pending
	// manifest was finalized. The marker retry must finish the same transaction.
	write(t, filepath.Join(root, filepath.FromSlash(target)), string(proposed))

	commit, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID})
	if err != nil {
		t.Fatal(err)
	}
	if commit.BackupRun != transaction.runID() {
		t.Fatalf("recovered backup run=%q, want %q", commit.BackupRun, transaction.runID())
	}
	if _, statErr := os.Stat(stage.Dir); !os.IsNotExist(statErr) {
		t.Fatalf("recovered stage was not removed: %v", statErr)
	}
	runs, err := backup.ListRunsWithOverride(root, "", home, "")
	if err != nil || len(runs) != 1 || runs[0].Operation != backup.CommittedCommitOperation || runs[0].CommitTransaction == nil || runs[0].CommitTransaction.State != backup.CommitTransactionCommitted {
		t.Fatalf("pending backup was not finalized: runs=%+v err=%v", runs, err)
	}
}

func TestCommitRejectsInterruptedPrepareWithoutBackfill(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	source := filepath.Join(root, "notes.md")
	write(t, source, "project shared source\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectShared},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage: %v %#v", err, report)
	}
	stage := report.Stages[0]
	_, _, review, err := ReadReview(root, home, stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, normalized, err := NormalizeReviewedContent(CommitInput{
		Root: root, Scope: stage.Scope, Provider: stage.Provider, Source: stage.Source, Kind: stage.Kind, ID: stage.ID, Content: review,
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(root, filepath.FromSlash(target))
	const prior = "prior canonical\n"
	write(t, canonical, prior)
	if err := os.Chmod(canonical, 0o640); err != nil {
		t.Fatal(err)
	}
	proposed := proposedCanonicalGeneration(normalized, []byte(prior))
	sourceBody, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	store, err := backup.NewStoreFromRoot(homeRoot, home, backup.StoreOptions{
		ProjectRoot: root, ThreadpointHome: home,
		Operation: backup.PendingCommitOperation, RunID: "commit-" + stage.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration(stage.ID, stage.Source, digest(sourceBody), target, digest(proposed), int64(len(proposed))); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, sourceBody, sourceInfo.Mode(), nil, os.FileMode(0), false)
	// Deliberately remove required metadata to exercise rejection of malformed state.
	store.Manifest.Entries = store.Manifest.Entries[:1]
	manifestPath := filepath.Join(store.RunDir, backup.ManifestName)
	before := writeMalformedBackupManifestFixture(t, store)
	write(t, source, "provider changed after interrupted prepare\n")

	_, err = Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID})
	if err == nil || !strings.Contains(err.Error(), "exactly one prior canonical generation") {
		t.Fatalf("commit error=%v", err)
	}
	if body, err := os.ReadFile(canonical); err != nil || string(body) != prior {
		t.Fatalf("interrupted prepare changed canonical: body=%q err=%v", body, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("canonical mode=%v, want 0640", info.Mode().Perm())
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("incomplete current-schema generation was modified or backfilled")
	}
	if _, err := os.Stat(stage.Dir); err != nil {
		t.Fatalf("stage removed after recovered source refusal: %v", err)
	}
}

func TestStageGenerationRollbackPrimitivesRestoreExactDirectories(t *testing.T) {
	t.Run("journaled", func(t *testing.T) {
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		for name, marker := range map[string]string{"stage-id": "next", ".stage-id.old-token": "old"} {
			if err := root.Mkdir(name, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := root.WriteFile(filepath.Join(name, "marker"), []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		nextInfo, err := root.Lstat("stage-id")
		if err != nil {
			t.Fatal(err)
		}
		oldInfo, err := root.Lstat(".stage-id.old-token")
		if err != nil {
			t.Fatal(err)
		}
		rolledBack, err := rollbackJournaledStageGeneration(root, ".stage-id.next-token", ".stage-id.old-token", "stage-id", nextInfo, oldInfo)
		if err != nil || !rolledBack {
			t.Fatalf("journaled rollback = %v, err=%v", rolledBack, err)
		}
		if body, err := root.ReadFile(filepath.Join("stage-id", "marker")); err != nil || string(body) != "old" {
			t.Fatalf("restored journal generation = %q, err=%v", body, err)
		}
		if body, err := root.ReadFile(filepath.Join(".stage-id.next-token", "marker")); err != nil || string(body) != "next" {
			t.Fatalf("detached next generation = %q, err=%v", body, err)
		}
	})

	t.Run("detached", func(t *testing.T) {
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		if err := root.Mkdir(".stage-id.old-token", 0o700); err != nil {
			t.Fatal(err)
		}
		expected, err := root.Lstat(".stage-id.old-token")
		if err != nil {
			t.Fatal(err)
		}
		restored, err := restoreDetachedStageGeneration(root, ".stage-id.old-token", "stage-id", expected)
		if err != nil || !restored {
			t.Fatalf("detached restoration = %v, err=%v", restored, err)
		}
		current, err := root.Lstat("stage-id")
		if err != nil || !os.SameFile(expected, current) {
			t.Fatalf("restored generation identity changed: %v", err)
		}
	})

	t.Run("exchange", func(t *testing.T) {
		if !safefs.RenameRootExchangeSupported() {
			t.Skip("atomic directory exchange unavailable")
		}
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		for _, name := range []string{"stage-id", ".stage-id.next-token"} {
			if err := root.Mkdir(name, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		nextInfo, err := root.Lstat("stage-id")
		if err != nil {
			t.Fatal(err)
		}
		oldInfo, err := root.Lstat(".stage-id.next-token")
		if err != nil {
			t.Fatal(err)
		}
		if err := rollbackStageGenerationExchange(root, ".stage-id.next-token", "stage-id", nextInfo, oldInfo); err != nil {
			t.Fatal(err)
		}
		restored, err := root.Lstat("stage-id")
		if err != nil || !os.SameFile(oldInfo, restored) {
			t.Fatalf("exchange rollback did not restore prior generation: %v", err)
		}
	})
}

func TestStageTransactionRecoveryRejectsAmbiguousArtifacts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root *os.Root)
	}{
		{
			name: "stable generation is a file",
			setup: func(t *testing.T, root *os.Root) {
				t.Helper()
				if err := root.WriteFile("stage-id", []byte("file"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := root.Mkdir(".stage-id.next-01234567890123456789", 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "multiple prior generations",
			setup: func(t *testing.T, root *os.Root) {
				t.Helper()
				for _, name := range []string{".stage-id.old-01234567890123456789", ".stage-id.old-abcdefabcdefabcdefab"} {
					if err := root.Mkdir(name, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "prior generation is a file",
			setup: func(t *testing.T, root *os.Root) {
				t.Helper()
				if err := root.WriteFile(".stage-id.old-01234567890123456789", []byte("file"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "next generation is a file",
			setup: func(t *testing.T, root *os.Root) {
				t.Helper()
				if err := root.Mkdir("stage-id", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := root.WriteFile(".stage-id.next-01234567890123456789", []byte("file"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			test.setup(t, root)
			if err := recoverStageTransactions(root, "stage-id"); err == nil {
				t.Fatal("ambiguous stage transaction state was accepted")
			}
		})
	}
}

func TestCommitReviewedRetryPreservesExistingCanonicalContent(t *testing.T) {
	root := initGitRepo(t)
	target, err := TargetFor(ScopeProjectShared, "fixture", "notes.md", KindKnowledge)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, filepath.FromSlash(target)), "# Existing Knowledge\n\nkeep this\n")
	input := CommitInput{Root: root, Scope: ScopeProjectShared, Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, ID: "stage-retry", Content: []byte("first reviewed block\n")}
	if _, err := CommitReviewed(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target)))
	if err != nil {
		t.Fatal(err)
	}
	input.Content = []byte("replacement must not be written\n")
	if _, err := CommitReviewed(context.Background(), input); err == nil || !strings.Contains(err.Error(), "unbound marker") {
		t.Fatalf("unbound marker retry error = %v", err)
	}
	second, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target)))
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Fatalf("retry changed canonical content:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestCommitReviewedTransactionHooksRollbackAndRecoverMarker(t *testing.T) {
	if !safefs.RenameRootExchangeSupported() {
		t.Skip("atomic rooted exchange is unsupported on this platform")
	}
	newInput := func(root, id string) CommitInput {
		return CommitInput{
			Root: root, Scope: ScopeProjectShared, Provider: "fixture", Source: "notes.md", Kind: KindKnowledge,
			ID: id, Content: []byte("reviewed transaction content\n"),
		}
	}
	t.Run("successful publication orders prepare validation and finalize", func(t *testing.T) {
		root := initGitRepo(t)
		input := newInput(root, "stage-transaction-success")
		var calls []string
		input.PrepareTransaction = func(target string, proposed []byte) error {
			calls = append(calls, "prepare")
			if target == "" || !bytes.Contains(proposed, []byte(`threadpoint:committed id="stage-transaction-success"`)) {
				t.Fatalf("prepare target=%q proposed=%q", target, proposed)
			}
			return nil
		}
		input.ValidateBeforePublish = func() error { calls = append(calls, "before"); return nil }
		input.ValidateAfterPublish = func() error { calls = append(calls, "after"); return nil }
		input.FinalizeTransaction = func(string, []byte) error { calls = append(calls, "finalize"); return nil }
		input.AbortTransaction = func() error { calls = append(calls, "abort"); return nil }
		if _, err := CommitReviewed(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(calls, ","); got != "prepare,before,after,finalize" {
			t.Fatalf("transaction hook order = %s", got)
		}
	})
	t.Run("finalize failure restores canonical and aborts", func(t *testing.T) {
		root := initGitRepo(t)
		input := newInput(root, "stage-transaction-failure")
		target, err := TargetFor(input.Scope, input.Provider, input.Source, input.Kind)
		if err != nil {
			t.Fatal(err)
		}
		prior := []byte("prior canonical\n")
		write(t, filepath.Join(root, filepath.FromSlash(target)), string(prior))
		var calls []string
		input.PrepareTransaction = func(string, []byte) error { calls = append(calls, "prepare"); return nil }
		input.ValidateBeforePublish = func() error { calls = append(calls, "before"); return nil }
		input.ValidateAfterPublish = func() error { calls = append(calls, "after"); return nil }
		finalizeErr := errors.New("finalize failed")
		input.FinalizeTransaction = func(string, []byte) error { calls = append(calls, "finalize"); return finalizeErr }
		input.AbortTransaction = func() error { calls = append(calls, "abort"); return nil }
		if _, err := CommitReviewed(context.Background(), input); !errors.Is(err, finalizeErr) {
			t.Fatalf("finalize error = %v", err)
		}
		if got := strings.Join(calls, ","); got != "prepare,before,after,finalize,abort" {
			t.Fatalf("failed transaction hook order = %s", got)
		}
		if body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target))); err != nil || !bytes.Equal(body, prior) {
			t.Fatalf("canonical was not rolled back: body=%q err=%v", body, err)
		}
	})
	t.Run("existing marker without a durable binding is refused", func(t *testing.T) {
		root := initGitRepo(t)
		input := newInput(root, "stage-transaction-retry")
		report, err := CommitReviewed(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		targetPath := filepath.Join(root, filepath.FromSlash(report.Target))
		before, err := os.Lstat(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		var calls []string
		input.PrepareTransaction = func(target string, proposed []byte) error {
			calls = append(calls, "prepare")
			if target != report.Target || !bytes.Contains(proposed, []byte(`threadpoint:committed id="stage-transaction-retry"`)) {
				t.Fatalf("retry prepare target=%q proposed=%q", target, proposed)
			}
			return nil
		}
		input.ValidateAfterPublish = func() error { calls = append(calls, "after"); return nil }
		input.FinalizeTransaction = func(string, []byte) error { calls = append(calls, "finalize"); return nil }
		input.AbortTransaction = func() error { calls = append(calls, "abort"); return nil }
		if _, err := CommitReviewed(context.Background(), input); err == nil || !strings.Contains(err.Error(), "unbound marker") {
			t.Fatalf("unbound marker retry error = %v", err)
		}
		after, err := os.Lstat(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) {
			t.Fatal("marker recovery republished the canonical generation")
		}
		if got := strings.Join(calls, ","); got != "" {
			t.Fatalf("unbound marker invoked transaction hooks: %s", got)
		}
	})
}

func TestRebaseMergedReviewRecoversCrashAfterAtomicExchange(t *testing.T) {
	if !safefs.RenameRootExchangeSupported() {
		t.Skip("atomic rooted exchange is unsupported on this platform")
	}
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "source\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, %v", report, err)
	}
	stage := report.Stages[0]
	crash := errors.New("simulated crash after stage exchange")
	stageGenerationAfterExchange = func(*os.Root, string, string) error { return crash }
	err = RebaseMergedReview(root, home, stage.ID, []byte("remote\n"), []byte("merged\n"))
	stageGenerationAfterExchange = nil
	if !errors.Is(err, crash) {
		t.Fatalf("rebase crash error = %v", err)
	}
	if artifacts := stageTransactionArtifacts(t, stage.Dir); len(artifacts) != 1 || !strings.Contains(artifacts[0], ".next-") {
		t.Fatalf("post-exchange crash artifacts = %#v", artifacts)
	}
	record, source, content, err := ReadReview(root, home, stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(source) != "remote\n" || string(content) != "merged\n" || record.SourceSHA256 != digest(source) || record.ContentSHA256 != digest(content) {
		t.Fatalf("observed exchanged stage = %#v source=%q content=%q", record, source, content)
	}
	if artifacts := stageTransactionArtifacts(t, stage.Dir); len(artifacts) != 1 || !strings.Contains(artifacts[0], ".next-") {
		t.Fatalf("observational read changed exchange artifacts: %#v", artifacts)
	}
	if err := ReplaceContent(root, home, stage.ID, []byte("writer-cleaned review\n")); err != nil {
		t.Fatalf("writer-path recovery: %v", err)
	}
	if artifacts := stageTransactionArtifacts(t, stage.Dir); len(artifacts) != 0 {
		t.Fatalf("writer-path recovery left artifacts: %#v", artifacts)
	}
}

func TestRebaseMergedReviewJournalFallbackRecoversDetachedStage(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "source\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, %v", report, err)
	}
	stage := report.Stages[0]
	originalExchange := exchangeStageRoots
	exchangeStageRoots = func(*os.Root, string, string) error { return safefs.ErrRenameExchangeUnsupported }
	t.Cleanup(func() {
		exchangeStageRoots = originalExchange
		stageGenerationAfterDetach = nil
	})
	crash := errors.New("simulated crash after durable stage detach")
	stageGenerationAfterDetach = func(*os.Root, string, string) error { return crash }
	err = RebaseMergedReview(root, home, stage.ID, []byte("remote\n"), []byte("merged\n"))
	stageGenerationAfterDetach = nil
	if !errors.Is(err, crash) {
		t.Fatalf("journaled rebase crash error = %v", err)
	}
	if _, err := os.Lstat(stage.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("simulated fallback crash state kept stable stage: %v", err)
	}
	artifacts := stageTransactionArtifacts(t, stage.Dir)
	if len(artifacts) != 2 || !strings.Contains(strings.Join(artifacts, "\n"), ".old-") || !strings.Contains(strings.Join(artifacts, "\n"), ".next-") {
		t.Fatalf("journaled stage crash artifacts = %#v", artifacts)
	}
	listed, err := List(root, home)
	if err != nil || len(listed) != 0 {
		t.Fatalf("observational List exposed detached stage: records=%#v err=%v", listed, err)
	}
	if got := stageTransactionArtifacts(t, stage.Dir); !slices.Equal(got, artifacts) {
		t.Fatalf("observational List changed journal artifacts: before=%#v after=%#v", artifacts, got)
	}

	if _, _, _, err := ReadReview(root, home, stage.ID); err == nil {
		t.Fatal("observational stage read unexpectedly recovered a detached generation")
	}
	if got := stageTransactionArtifacts(t, stage.Dir); !slices.Equal(got, artifacts) {
		t.Fatalf("observational read changed journal artifacts: before=%#v after=%#v", artifacts, got)
	}
	if err := RebaseMergedReview(root, home, stage.ID, []byte("remote\n"), []byte("merged\n")); err != nil {
		t.Fatal(err)
	}
	_, source, content, err := ReadReview(root, home, stage.ID)
	if err != nil || string(source) != "remote\n" || string(content) != "merged\n" {
		t.Fatalf("journaled stage publication source=%q content=%q err=%v", source, content, err)
	}
}

func writeMalformedBackupManifestFixture(t *testing.T, store *backup.Store) []byte {
	t.Helper()
	body, err := json.MarshalIndent(store.Manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(store.RunDir, backup.ManifestName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCommitBackupRetryRequiresSameSealedPriorGeneration(t *testing.T) {
	for _, state := range []string{"pending", "aborted", "committed"} {
		for _, mismatch := range []bool{false, true} {
			name := state
			if mismatch {
				name += "/changed-prior"
			}
			t.Run(name, func(t *testing.T) {
				project, home := t.TempDir(), t.TempDir()
				retained, err := os.OpenRoot(home)
				if err != nil {
					t.Fatal(err)
				}
				defer retained.Close()
				record := Record{ID: "stage-retry", Source: "native.md"}
				makeTransaction := func() *commitBackupTransaction {
					return newCommitBackupTransaction(record, project, home, retained, []byte("native"), 0o600)
				}
				snapshot := CanonicalTransactionSnapshot{Target: "AGENTS.md", Proposed: []byte("proposed"), PriorExists: true, PriorBody: []byte("prior"), PriorMode: 0o640}
				transaction := makeTransaction()
				if err := transaction.prepare(snapshot); err != nil {
					t.Fatal(err)
				}
				switch state {
				case "aborted":
					if err := transaction.abort(); err != nil {
						t.Fatal(err)
					}
				case "committed":
					if err := transaction.finalize(snapshot.Target, snapshot.Proposed); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.ReadFile(filepath.Join(transaction.store.RunDir, backup.ManifestName))
				if err != nil {
					t.Fatal(err)
				}
				retry := makeTransaction()
				if mismatch {
					snapshot.PriorBody = []byte("different prior")
				}
				err = retry.prepare(snapshot)
				if mismatch && state != "committed" {
					if err == nil {
						t.Fatal("changed sealed prior accepted")
					}
					after, err := os.ReadFile(filepath.Join(transaction.store.RunDir, backup.ManifestName))
					if err != nil || !bytes.Equal(before, after) {
						t.Fatalf("refusal changed recovery evidence: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if retry.preexistingCommitted != (state == "committed") {
					t.Fatalf("retry committed=%v", retry.preexistingCommitted)
				}
				if err := retry.finalize(snapshot.Target, snapshot.Proposed); err != nil {
					t.Fatal(err)
				}
				if retry.store.Manifest.CommitTransaction.State != backup.CommitTransactionCommitted {
					t.Fatalf("retry state=%s", retry.store.Manifest.CommitTransaction.State)
				}
			})
		}
	}
}
