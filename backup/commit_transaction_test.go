// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPendingPostHashRetainsCommittedStateUntilCommit(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: t.TempDir(), Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupBytes("AGENTS.md", []byte("original"), "fixture-target"); err != nil {
		t.Fatal(err)
	}
	committed := strings.Repeat("1", 64)
	pending := strings.Repeat("2", 64)
	if err := store.SetPostHashValue("AGENTS.md", "fixture-target", committed); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPendingPostHashValue("AGENTS.md", "fixture-target", pending); err != nil {
		t.Fatal(err)
	}
	entry := store.Manifest.Entries[0]
	if entry.PostSHA256 != committed || entry.PendingPostSHA256 != pending {
		t.Fatalf("pending state lost committed hash: %#v", entry)
	}
	if err := store.CommitPendingPostHashValue("AGENTS.md", "fixture-target", pending); err != nil {
		t.Fatal(err)
	}
	entry = store.Manifest.Entries[0]
	if entry.PostSHA256 != pending || entry.PendingPostSHA256 != "" {
		t.Fatalf("pending state was not committed atomically: %#v", entry)
	}
}

func TestPendingCommitTransactionIsInvisibleUntilFinalized(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := []byte("native source\n")
	sourceHash := sha256.Sum256(source)
	target := []byte("canonical publication\n")
	targetHash := sha256.Sum256(target)
	store, err := NewStore(StoreOptions{
		ProjectRoot: root,
		HomeDir:     home,
		Operation:   PendingCommitOperation,
		RunID:       "commit-stage-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration("stage-fixture", "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:]), int64(len(target))); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, source, 0o640, nil, os.FileMode(0), false)

	runs, err := ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("pending commit became visible: %+v", runs)
	}
	manifestPath := filepath.Join(store.RunDir, ManifestName)
	pending, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Operation != PendingCommitOperation || !CommitTransactionMatches(pending, CommitTransactionPending, "stage-fixture", "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:])) {
		t.Fatalf("pending transaction was not durably bound: %+v", pending)
	}
	if err := store.FinalizePendingCommit(); err != nil {
		t.Fatal(err)
	}
	runs, err = ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Operation != CommittedCommitOperation || runs[0].CommitTransaction == nil || runs[0].CommitTransaction.State != CommitTransactionCommitted {
		t.Fatalf("finalized commit run=%+v", runs)
	}
	if err := store.AbortPendingCommit(); err != nil {
		t.Fatal(err)
	}
	if runs, err := ListRuns(root, home, ""); err != nil || len(runs) != 0 {
		t.Fatalf("aborted commit remained visible: runs=%+v err=%v", runs, err)
	}
}

func TestPendingCommitFinalizeFailureKeepsPruneInvisibleManifest(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := []byte("native source\n")
	sourceHash := sha256.Sum256(source)
	target := []byte("canonical publication\n")
	targetHash := sha256.Sum256(target)
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: PendingCommitOperation, RunID: "commit-stage-failure"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration("stage-failure", "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:]), int64(len(target))); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, source, os.FileMode(0o600), nil, os.FileMode(0), false)

	store.beforeSave = func() error { return errors.New("injected finalize failure") }
	if err := store.FinalizePendingCommit(); err == nil || !strings.Contains(err.Error(), "injected finalize failure") {
		t.Fatalf("expected finalize failure, got %v", err)
	}
	persisted, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Operation != PendingCommitOperation || persisted.CommitTransaction == nil || persisted.CommitTransaction.State != CommitTransactionPending {
		t.Fatalf("failed finalize exposed commit: %+v", persisted)
	}
}

func TestPendingCommitTransactionFinalizesThroughPinnedHome(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	source := []byte("native source\n")
	sourceHash := sha256.Sum256(source)
	target := []byte("canonical publication\n")
	targetHash := sha256.Sum256(target)
	store, err := NewStore(StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: PendingCommitOperation, RunID: "commit-stage-rooted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration("stage-rooted", "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:]), int64(len(target))); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, source, 0o600, nil, os.FileMode(0), false)

	if err := store.FinalizePendingCommitFromRoot(homeRoot, home); err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadManifestFromRoot(homeRoot, home, filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Operation != CommittedCommitOperation || manifest.CommitTransaction == nil || manifest.CommitTransaction.State != CommitTransactionCommitted {
		t.Fatalf("rooted finalized manifest=%+v", manifest)
	}
}

func TestCommitManifestTransitionsRejectReplacementGeneration(t *testing.T) {
	for _, transition := range []struct {
		name string
		run  func(*Store) error
	}{
		{name: "finalize", run: func(store *Store) error { return store.FinalizePendingCommit() }},
		{name: "abort", run: func(store *Store) error { return store.AbortPendingCommit() }},
	} {
		t.Run(transition.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			store := preparedCommitStore(t, root, home, "commit-stage-"+transition.name)
			manifestPath := filepath.Join(store.RunDir, ManifestName)
			persisted, err := ReadManifest(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			persisted.Operation = "replacement-generation"
			replacement, err := json.MarshalIndent(persisted, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			replacement = append(replacement, '\n')
			store.beforeSave = func() error {
				store.beforeSave = nil
				temporary := manifestPath + ".replacement"
				if err := os.WriteFile(temporary, replacement, 0o600); err != nil {
					return err
				}
				return os.Rename(temporary, manifestPath)
			}

			err = transition.run(store)
			if err == nil || !strings.Contains(err.Error(), "changed after its generation was reviewed") {
				t.Fatalf("transition accepted a replacement manifest: %v", err)
			}
			body, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != string(replacement) {
				t.Fatalf("transition overwrote replacement manifest:\n%s", body)
			}
			if store.Manifest.Operation != PendingCommitOperation || store.Manifest.CommitTransaction == nil || store.Manifest.CommitTransaction.State != CommitTransactionPending {
				t.Fatalf("failed transition did not restore in-memory pending state: %+v", store.Manifest)
			}
		})
	}
}

func TestRootedPendingCommitRetryReusesExactContentAddressedPayload(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	source := []byte("native source\n")
	sourceHash := sha256.Sum256(source)
	target := []byte("canonical publication\n")
	targetHash := sha256.Sum256(target)
	options := StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: PendingCommitOperation, RunID: "commit-stage-rooted-retry"}
	first, err := NewStore(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := first.LoadManifestGenerationFromRoot(homeRoot, home); !os.IsNotExist(err) {
		t.Fatalf("fresh manifest load = %v, want missing", err)
	}
	if err := first.ConfigurePendingCommitGeneration("stage-rooted-retry", "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:]), int64(len(target))); err != nil {
		t.Fatal(err)
	}
	first = publishPendingFixture(t, first, source, 0o600, nil, os.FileMode(0), false)
	entry := &first.Manifest.Entries[0]

	payloadInfo, err := os.Lstat(entry.BackupPath)
	if err != nil {
		t.Fatal(err)
	}

	retry, err := NewStore(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retry.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	manifest, err := retry.LoadManifestGenerationFromRoot(homeRoot, home)
	if err != nil {
		t.Fatal(err)
	}
	if !CommitTransactionMatches(manifest, CommitTransactionPending, "stage-rooted-retry", "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:])) {
		t.Fatalf("loaded pending transaction mismatch: %+v", manifest)
	}
	bound, err := LoadBoundCommitTransactionFromRoot(homeRoot, home, options, CommitBinding{
		StageID: "stage-rooted-retry", SourcePath: "native.md", SourceSHA256: hex.EncodeToString(sourceHash[:]), TargetPath: "AGENTS.md", TargetSHA256: hex.EncodeToString(targetHash[:]), TargetSize: int64(len(target)),
	})
	if err != nil {
		t.Fatalf("load exact payload retry: %v", err)
	}
	retry = bound.Store

	after, err := os.Lstat(entry.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(payloadInfo, after) {
		t.Fatal("exact payload retry replaced the content-addressed generation")
	}
	if err := retry.FinalizePendingCommitFromRoot(homeRoot, home); err != nil {
		t.Fatal(err)
	}
}

func TestRootedPendingCommitReaderRejectsInterruptedPriorAbsentPreparation(t *testing.T) {
	root := t.TempDir()
	parent := t.TempDir()
	home := filepath.Join(parent, "product-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	source := []byte("native source\n")
	sourceHash := sha256.Sum256(source)
	target := []byte("canonical publication\n")
	targetHash := sha256.Sum256(target)
	options := StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: PendingCommitOperation, RunID: "commit-stage-interrupted-prepare"}
	first, err := NewStoreFromRoot(homeRoot, home, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := first.LoadManifestGenerationFromRoot(homeRoot, home); !os.IsNotExist(err) {
		t.Fatalf("fresh manifest load = %v, want missing", err)
	}
	if err := first.ConfigurePendingCommitGeneration("stage-interrupted-prepare", "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:]), int64(len(target))); err != nil {
		t.Fatal(err)
	}
	first = publishPendingFixture(t, first, source, 0o600, nil, os.FileMode(0), false)
	// Deliberately remove required metadata to exercise rejection of malformed state.
	first.Manifest.Entries = first.Manifest.Entries[:1]
	if len(first.Manifest.Entries) != 1 || first.Manifest.Entries[0].Role != "stage-source" {
		t.Fatalf("interrupted prepare manifest=%+v", first.Manifest)
	}
	before := writeManifestFixtureUnchecked(t, first)

	// Simulate a restart after the source entry was durable but before the
	// canonical-prior absence marker was saved, and replace the ambient home
	// pathname. The retained reader must reject the incomplete generation and
	// must not infer or backfill its missing prior state.
	if err := os.Rename(home, home+".selected"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	bound, err := LoadBoundCommitTransactionFromRoot(homeRoot, home, options, CommitBinding{
		StageID: "stage-interrupted-prepare", SourcePath: "native.md", SourceSHA256: hex.EncodeToString(sourceHash[:]),
		TargetPath: "AGENTS.md", TargetSHA256: hex.EncodeToString(targetHash[:]), TargetSize: int64(len(target)),
	})
	if err == nil || bound != nil || !strings.Contains(err.Error(), "exactly one prior canonical generation") {
		t.Fatalf("incomplete generation load: bound=%+v err=%v", bound, err)
	}
	after, err := os.ReadFile(filepath.Join(home+".selected", "backups", ProjectID(root), options.RunID, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected incomplete generation was modified")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rooted retry wrote into replacement home: %#v", entries)
	}
}

func TestLoadBoundCommitTransactionReturnsSealedPriorCanonical(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	source := []byte("native source\n")
	prior := []byte("prior canonical\n")
	proposed := []byte("prior canonical\n\nnew committed block\n")
	sourceHash := sha256.Sum256(source)
	targetHash := sha256.Sum256(proposed)
	options := StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: PendingCommitOperation, RunID: "commit-stage-recovery"}
	store, err := NewStoreFromRoot(homeRoot, home, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGenerationFromRoot(homeRoot, home); !os.IsNotExist(err) {
		t.Fatalf("fresh manifest load = %v, want missing", err)
	}
	if err := store.ConfigurePendingCommitGeneration("stage-recovery", "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:]), int64(len(proposed))); err != nil {
		t.Fatal(err)
	}
	publishPendingFixture(t, store, source, 0o640, prior, 0o644, true)

	bound, err := LoadBoundCommitTransactionFromRoot(homeRoot, home, options, CommitBinding{
		StageID: "stage-recovery", SourcePath: "native.md", SourceSHA256: hex.EncodeToString(sourceHash[:]),
		TargetPath: "AGENTS.md", TargetSHA256: hex.EncodeToString(targetHash[:]), TargetSize: int64(len(proposed)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.State != CommitTransactionPending || !bound.PriorKnown || !bound.PriorExists || string(bound.PriorBody) != string(prior) || bound.PriorMode != 0o644 {
		t.Fatalf("bound recovery transaction=%+v", bound)
	}
	discovered, err := LoadBoundCommitTransactionFromRoot(homeRoot, home, options, CommitBinding{
		StageID: "stage-recovery", SourcePath: "native.md", SourceSHA256: hex.EncodeToString(sourceHash[:]), TargetPath: "AGENTS.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	if discovered.TargetSHA256 != hex.EncodeToString(targetHash[:]) || discovered.TargetSize != int64(len(proposed)) {
		t.Fatalf("discovered canonical generation=%+v", discovered)
	}
}

func TestLoadBoundCommitTransactionRejectsRunWithoutPrior(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	source := []byte("native source\n")
	target := []byte("canonical publication\n")
	sourceHash := sha256.Sum256(source)
	targetHash := sha256.Sum256(target)
	options := StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: PendingCommitOperation, RunID: "commit-stage-incomplete-committed"}
	store, err := NewStoreFromRoot(homeRoot, home, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGenerationFromRoot(homeRoot, home); !os.IsNotExist(err) {
		t.Fatalf("fresh manifest load = %v, want missing", err)
	}
	if err := store.ConfigurePendingCommitGeneration("stage-incomplete-committed", "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:]), int64(len(target))); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, source, 0o600, nil, os.FileMode(0), false)
	// Deliberately remove required metadata to exercise rejection of malformed state.
	store.Manifest.Entries = store.Manifest.Entries[:1]
	manifestPath := filepath.Join(store.RunDir, ManifestName)
	before := writeManifestFixtureUnchecked(t, store)
	bound, err := LoadBoundCommitTransactionFromRoot(homeRoot, home, options, CommitBinding{
		StageID: "stage-incomplete-committed", SourcePath: "native.md", SourceSHA256: hex.EncodeToString(sourceHash[:]),
		TargetPath: "AGENTS.md", TargetSHA256: hex.EncodeToString(targetHash[:]), TargetSize: int64(len(target)),
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one prior canonical generation") || bound != nil {
		t.Fatalf("incomplete committed manifest was accepted: bound=%+v err=%v", bound, err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected committed manifest was modified")
	}
}

func TestLoadBoundCommitTransactionRejectsPayloadFromSiblingRunWithoutMutation(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	store := preparedCommitStore(t, root, home, "commit-stage-cross-run")
	if err := store.FinalizePendingCommit(); err != nil {
		t.Fatal(err)
	}
	other, err := NewStore(StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: "fixture", RunID: "other-run"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	otherEntry, err := other.BackupBytesWithMode("native.md", []byte("native source\n"), "stage-source", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for index := range store.Manifest.Entries {
		if store.Manifest.Entries[index].Role == "stage-source" {
			store.Manifest.Entries[index].BackupPath = otherEntry.BackupPath
		}
	}
	writeManifestFixtureUnchecked(t, store)
	manifestPath := filepath.Join(store.RunDir, ManifestName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	transaction := store.Manifest.CommitTransaction
	bound, err := LoadBoundCommitTransactionFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: root, ThreadpointHome: home, Operation: PendingCommitOperation, RunID: store.RunID,
	}, CommitBinding{
		StageID: transaction.StageID, SourcePath: transaction.SourcePath, SourceSHA256: transaction.SourceSHA256,
		TargetPath: transaction.TargetPath, TargetSHA256: transaction.TargetSHA256, TargetSize: transaction.TargetSize,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the selected project run") || bound != nil {
		t.Fatalf("sibling-run payload was accepted: bound=%+v err=%v", bound, err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected cross-run manifest was modified")
	}
}

func TestLoadBoundCommitTransactionRejectsLegacyPriorAboveCanonicalCeiling(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()

	source := []byte("provider-native source\n")
	proposed := []byte("canonical publication\n")
	binding := pendingCommitTestBinding("stage-canonical-prior-load-limit", source, proposed)
	store, err := PublishPendingCommitGenerationFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home,
	}, PendingCommitGeneration{
		Binding: binding, SourceBody: source, SourceMode: 0o600,
		PriorExists: true, PriorBody: []byte("prior\n"), PriorMode: 0o600,
	})
	if err != nil {
		t.Fatal(err)
	}
	oversized := bytes.Repeat([]byte("p"), MaxCanonicalGenerationBytes+1)
	for index := range store.Manifest.Entries {
		entry := &store.Manifest.Entries[index]
		if entry.Role != "canonical-prior" {
			continue
		}
		if err := os.WriteFile(entry.BackupPath, oversized, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(oversized)
		entry.Size = int64(len(oversized))
		entry.SHA256 = hex.EncodeToString(sum[:])
	}
	writeManifestFixtureUnchecked(t, store)

	bound, err := LoadBoundCommitTransactionFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home,
		Operation: PendingCommitOperation, RunID: "commit-" + binding.StageID,
	}, binding)
	if err == nil || bound != nil || !strings.Contains(err.Error(), "sealed prior canonical generation exceeds") {
		t.Fatalf("oversized sealed-prior load: bound-present=%t err=%v", bound != nil, err)
	}
}

func TestLoadBoundCommitTransactionMissingRunDoesNotCreateGeneration(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	source := []byte("provider-native source\n")
	target := []byte("canonical publication\n")
	binding := pendingCommitTestBinding("stage-missing-read", source, target)

	bound, err := LoadBoundCommitTransactionFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home,
		Operation: PendingCommitOperation, RunID: "commit-" + binding.StageID,
	}, binding)
	if !errors.Is(err, os.ErrNotExist) || bound != nil {
		t.Fatalf("missing transaction read: bound=%+v err=%v", bound, err)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("missing transaction read created backup state: entries=%#v err=%v", entries, err)
	}
}

// writeManifestFixtureUnchecked deliberately manufactures corrupt or legacy
// on-disk state for reader/recovery tests. Production Save rejects these
// manifests before publication.
func writeManifestFixtureUnchecked(t *testing.T, store *Store) []byte {
	t.Helper()
	body, err := marshalManifest(store.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.RunDir, ManifestName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCommitRecoveryRejectsMismatchedBindingAndDamagedEvidence(t *testing.T) {
	for _, damage := range []string{"invalid-stage", "wrong-run", "negative-size", "size-without-hash", "hash-without-size", "wrong-size", "wrong-source", "wrong-target", "nil-root", "closed-root", "run-symlink", "run-file", "missing-manifest", "source-missing", "source-mode", "source-bytes", "source-size", "unknown-role", "extra-entry"} {
		t.Run(damage, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			store := preparedCommitStore(t, project, home, "commit-stage-contract")
			root, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			tx := store.Manifest.CommitTransaction
			binding := CommitBinding{StageID: tx.StageID, SourcePath: tx.SourcePath, SourceSHA256: tx.SourceSHA256, TargetPath: tx.TargetPath, TargetSHA256: tx.TargetSHA256, TargetSize: tx.TargetSize}
			opts := StoreOptions{ProjectRoot: project, ThreadpointHome: home, Operation: PendingCommitOperation, RunID: store.RunID}
			manifestPath := filepath.Join(store.RunDir, ManifestName)
			sourceIndex := -1
			for i, e := range store.Manifest.Entries {
				if e.Role == "stage-source" {
					sourceIndex = i
				}
			}
			if sourceIndex < 0 {
				t.Fatal("source entry missing")
			}
			source := &store.Manifest.Entries[sourceIndex]
			rewrite := false
			switch damage {
			case "invalid-stage":
				binding.StageID = "../outside"
			case "wrong-run":
				opts.RunID = "commit-another"
			case "negative-size":
				binding.TargetSize = -1
			case "size-without-hash":
				binding.TargetSHA256 = ""
			case "hash-without-size":
				binding.TargetSize = 0
			case "wrong-size":
				binding.TargetSize++
			case "wrong-source":
				binding.SourcePath = "different.md"
			case "wrong-target":
				binding.TargetPath = "different.md"
			case "nil-root":
				root = nil
			case "closed-root":
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			case "run-symlink", "run-file":
				moved := store.RunDir + "-retained"
				if err := os.Rename(store.RunDir, moved); err != nil {
					t.Fatal(err)
				}
				manifestPath = filepath.Join(moved, ManifestName)
				if damage == "run-file" {
					mustWriteFile(t, store.RunDir, "unrelated")
				} else if err := os.Symlink(moved, store.RunDir); err != nil {
					t.Fatal(err)
				}
			case "missing-manifest":
				if err := os.Remove(manifestPath); err != nil {
					t.Fatal(err)
				}
			case "source-missing":
				if err := os.Remove(source.BackupPath); err != nil {
					t.Fatal(err)
				}
			case "source-mode":
				if err := os.Chmod(source.BackupPath, 0o644); err != nil {
					t.Fatal(err)
				}
			case "source-bytes":
				if err := os.WriteFile(source.BackupPath, []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "source-size":
				source.Size++
				rewrite = true
			case "unknown-role":
				source.Role = "unknown"
				rewrite = true
			case "extra-entry":
				extra := *source
				extra.Role = "extra"
				store.Manifest.Entries = append(store.Manifest.Entries, extra)
				rewrite = true
			}
			if rewrite {
				body, err := json.Marshal(store.Manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(manifestPath, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := os.ReadFile(manifestPath)
			if transaction, err := LoadBoundCommitTransactionFromRoot(root, home, opts, binding); err == nil {
				t.Fatalf("damaged recovery accepted: %#v", transaction)
			}
			after, afterErr := os.ReadFile(manifestPath)
			if !bytes.Equal(before, after) || (beforeErr == nil) != (afterErr == nil) {
				t.Fatal("recovery rejection changed durable evidence")
			}
		})
	}
}

func TestInternalEntryPublicationRejectsIncrementalCommitGenerations(t *testing.T) {
	for _, rooted := range []bool{false, true} {
		t.Run(fmt.Sprintf("rooted=%v", rooted), func(t *testing.T) {
			home := t.TempDir()
			store := preparedCommitStore(t, t.TempDir(), home, "commit-stage-binding")
			before, err := os.ReadFile(filepath.Join(store.RunDir, ManifestName))
			if err != nil {
				t.Fatal(err)
			}
			entry := store.Manifest.Entries[0]
			if rooted {
				root, openErr := os.OpenRoot(home)
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer root.Close()
				_, err = store.commitEntryFromRoot(root, home, entry)
			} else {
				_, err = store.commitEntry(entry)
			}
			if err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("incremental commit entry accepted: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(store.RunDir, ManifestName))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("refusal changed manifest: %v", err)
			}
		})
	}
}

func TestCommitTransitionPreservesPendingGenerationOnRejectedFinalization(t *testing.T) {
	for _, failure := range []string{"missing-store", "missing-transaction", "missing-writer", "wrong-state", "unreviewed-generation", "invalid-manifest", "save-failure"} {
		t.Run(failure, func(t *testing.T) {
			store := preparedCommitStore(t, t.TempDir(), t.TempDir(), "commit-stage-fixture")
			path := filepath.Join(store.RunDir, ManifestName)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			selected := store
			sentinel := errors.New("transition persistence failed")
			writes := 0
			save := func([]byte) error { writes++; return sentinel }
			switch failure {
			case "missing-store":
				selected = nil
			case "missing-transaction":
				store.Manifest.CommitTransaction = nil
			case "missing-writer":
				save = nil
			case "wrong-state":
				store.Manifest.CommitTransaction.State = CommitTransactionAborted
			case "unreviewed-generation":
				store.manifestGenerationKnown = false
			case "invalid-manifest":
				store.Manifest.Version = -1
			}
			prior, err := json.Marshal(store.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			err = selected.transitionCommitTransaction(CommitTransactionCommitted, CommittedCommitOperation, save)
			if err == nil {
				t.Fatal("invalid transition succeeded")
			}
			if failure == "save-failure" {
				if !errors.Is(err, sentinel) || writes != 1 {
					t.Fatalf("publication failure lost: %v calls=%d", err, writes)
				}
			} else if writes != 0 {
				t.Fatal("invalid transition reached persistence")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected transition changed persisted generation: %q %v", after, err)
			}
			current, err := json.Marshal(store.Manifest)
			if err != nil || !bytes.Equal(prior, current) {
				t.Fatalf("rejected transition changed in-memory generation: %q %v", current, err)
			}
		})
	}
}
