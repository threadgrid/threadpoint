// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

var (
	commitBackupBeforeFinalize func() error
	commitBackupAfterFinalize  func()
)

type commitBackupTransaction struct {
	record               Record
	projectRoot          string
	threadpointHome      string
	homeRoot             *os.Root
	sourceBody           []byte
	sourceMode           os.FileMode
	store                *backup.Store
	preexistingCommitted bool
	snapshot             *CanonicalTransactionSnapshot
}

func newCommitBackupTransaction(record Record, projectRoot, threadpointHome string, homeRoot *os.Root, sourceBody []byte, sourceMode os.FileMode) *commitBackupTransaction {
	return &commitBackupTransaction{
		record:          record,
		projectRoot:     projectRoot,
		threadpointHome: threadpointHome,
		homeRoot:        homeRoot,
		sourceBody:      append([]byte(nil), sourceBody...),
		sourceMode:      sourceMode,
	}
}

func (transaction *commitBackupTransaction) runID() string {
	return "commit-" + transaction.record.ID
}

func (transaction *commitBackupTransaction) prepare(snapshot CanonicalTransactionSnapshot) error {
	if transaction == nil || transaction.store != nil {
		return errors.New("commit backup transaction was prepared more than once")
	}
	target := snapshot.Target
	proposed := snapshot.Proposed
	if target == "" || len(proposed) == 0 {
		return errors.New("commit backup transaction requires a canonical generation")
	}
	sealed := snapshot
	sealed.Proposed = append([]byte(nil), snapshot.Proposed...)
	sealed.PriorBody = append([]byte(nil), snapshot.PriorBody...)
	transaction.snapshot = &sealed
	sourceSHA256 := digest(transaction.sourceBody)
	targetSHA256 := digest(proposed)
	options := backup.StoreOptions{
		ProjectRoot:     transaction.projectRoot,
		ThreadpointHome: transaction.threadpointHome,
		Operation:       backup.PendingCommitOperation,
		RunID:           transaction.runID(),
	}
	binding := backup.CommitBinding{
		StageID: transaction.record.ID, SourcePath: transaction.record.Source, SourceSHA256: sourceSHA256,
		TargetPath: target, TargetSHA256: targetSHA256, TargetSize: int64(len(proposed)),
	}
	bound, err := backup.LoadBoundCommitTransactionFromRoot(transaction.homeRoot, transaction.threadpointHome, options, binding)
	if err == nil {
		switch bound.State {
		case backup.CommitTransactionCommitted:
			transaction.store = bound.Store
			transaction.preexistingCommitted = true
			return nil
		case backup.CommitTransactionPending:
			if !boundPriorMatchesSnapshot(bound, snapshot) {
				return errors.New("existing pending commit backup does not match the reviewed prior canonical generation")
			}
			transaction.store = bound.Store
			return nil
		case backup.CommitTransactionAborted:
			if !boundPriorMatchesSnapshot(bound, snapshot) {
				return errors.New("existing aborted commit backup does not match the reviewed prior canonical generation")
			}
			if err := bound.Store.ConfigurePendingCommitGeneration(binding.StageID, binding.SourcePath, binding.SourceSHA256, binding.TargetPath, binding.TargetSHA256, binding.TargetSize); err != nil {
				return err
			}
			if err := bound.Store.SaveFromRoot(transaction.homeRoot, transaction.threadpointHome); err != nil {
				return fmt.Errorf("restart exact aborted commit backup transaction: %w", err)
			}
			transaction.store = bound.Store
			return nil
		default:
			return errors.New("existing commit backup run has an unsupported transaction state")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load existing commit backup generation: %w", err)
	}
	store, err := backup.PublishPendingCommitGenerationFromRoot(transaction.homeRoot, transaction.threadpointHome, options, backup.PendingCommitGeneration{
		Binding: binding, SourceBody: transaction.sourceBody, SourceMode: transaction.sourceMode,
		PriorExists: snapshot.PriorExists, PriorBody: snapshot.PriorBody, PriorMode: snapshot.PriorMode,
	})
	if err != nil {
		return err
	}
	transaction.store = store
	return nil
}

func boundPriorMatchesSnapshot(bound *backup.BoundCommitTransaction, snapshot CanonicalTransactionSnapshot) bool {
	if bound == nil || !bound.PriorKnown || bound.PriorExists != snapshot.PriorExists {
		return false
	}
	if !snapshot.PriorExists {
		return len(bound.PriorBody) == 0
	}
	return bound.PriorMode.Perm() == snapshot.PriorMode.Perm() && bytes.Equal(bound.PriorBody, snapshot.PriorBody)
}

// rollbackCommitted restores the sealed prior canonical generation when a
// logical-root mismatch is discovered after durable finalization but before
// review-stage cleanup. The exact published bytes are the CAS precondition, so
// a later canonical edit is never overwritten by this rollback.
func (transaction *commitBackupTransaction) rollbackCommitted(projectRoot *os.Root) error {
	if transaction == nil || transaction.preexistingCommitted {
		return nil
	}
	if transaction.store == nil || transaction.snapshot == nil {
		return errors.New("committed canonical rollback requires a sealed transaction snapshot")
	}
	published := safefs.RootFileGeneration{Exists: true, Body: append([]byte(nil), transaction.snapshot.Proposed...)}
	prior := safefs.RootFileGeneration{
		Exists: transaction.snapshot.PriorExists,
		Body:   append([]byte(nil), transaction.snapshot.PriorBody...),
		Mode:   transaction.snapshot.PriorMode,
	}
	if err := safefs.RestoreRootFileTransaction(projectRoot, transaction.snapshot.Target, published, prior); err != nil {
		return err
	}
	if err := transaction.store.AbortPendingCommitFromRoot(transaction.homeRoot, transaction.threadpointHome); err != nil {
		return err
	}
	return safefs.CleanupRootFileTransaction(projectRoot, transaction.snapshot.Target, prior)
}

func (transaction *commitBackupTransaction) finalize(target string, proposed []byte) error {
	if transaction == nil || transaction.store == nil {
		return errors.New("commit backup transaction was not prepared")
	}
	if transaction.preexistingCommitted {
		return nil
	}
	if !backup.CommitTransactionMatches(&transaction.store.Manifest, backup.CommitTransactionPending, transaction.record.ID, transaction.record.Source, digest(transaction.sourceBody), target, digest(proposed)) {
		return errors.New("commit backup transaction no longer matches the canonical publication")
	}
	if transaction.store.Manifest.CommitTransaction.TargetSize != int64(len(proposed)) {
		return errors.New("commit backup transaction target size no longer matches the canonical publication")
	}
	if commitBackupBeforeFinalize != nil {
		if err := commitBackupBeforeFinalize(); err != nil {
			return err
		}
	}
	if err := transaction.store.FinalizePendingCommitFromRoot(transaction.homeRoot, transaction.threadpointHome); err != nil {
		return err
	}
	if commitBackupAfterFinalize != nil {
		// The manifest transition above is the durable cross-file commit point.
		// Nothing after it may return a fallible result that would make safefs
		// roll the already-committed canonical generation back.
		commitBackupAfterFinalize()
	}
	return nil
}

func (transaction *commitBackupTransaction) abort() error {
	if transaction == nil || transaction.store == nil || transaction.preexistingCommitted {
		return nil
	}
	return transaction.store.AbortPendingCommitFromRoot(transaction.homeRoot, transaction.threadpointHome)
}

// recoverCommitBackupTransaction resolves durable state before source freshness
// checks. It accepts committed work only while its exact target and marker remain
// canonical; other transactions restore their sealed prior generation.
func recoverCommitBackupTransaction(record Record, projectRootPath, threadpointHome string, homeRoot, projectRoot *os.Root, reviewed []byte) (*commitBackupTransaction, error) {
	target, normalized, err := NormalizeReviewedContent(CommitInput{
		Root: projectRootPath, Scope: record.Scope, Provider: record.Provider,
		Source: record.Source, Kind: record.Kind, ID: record.ID, Content: reviewed,
	})
	if err != nil {
		return nil, err
	}
	options := backup.StoreOptions{
		ProjectRoot: projectRootPath, ThreadpointHome: threadpointHome,
		Operation: backup.PendingCommitOperation, RunID: "commit-" + record.ID,
	}
	bound, err := backup.LoadBoundCommitTransactionFromRoot(homeRoot, threadpointHome, options, backup.CommitBinding{
		StageID: record.ID, SourcePath: record.Source, SourceSHA256: record.SourceSHA256,
		TargetPath: target,
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // no bound transaction is the expected fresh-commit path.
	}
	if err != nil {
		return nil, fmt.Errorf("load bound commit recovery transaction: %w", err)
	}
	published := safefs.RootFileGeneration{Exists: true}
	if bound.PriorKnown {
		published.Body = proposedCanonicalGeneration(normalized, bound.PriorBody)
		if err := validateCanonicalGenerationSize(target, published.Body); err != nil {
			return nil, err
		}
		if digest(published.Body) != bound.TargetSHA256 || int64(len(published.Body)) != bound.TargetSize {
			return nil, errors.New("commit recovery target does not match the sealed prior and reviewed content")
		}
	}

	switch bound.State {
	case backup.CommitTransactionPending, backup.CommitTransactionAborted:
		if !bound.PriorKnown {
			return nil, errors.New("unfinished commit recovery transaction has no sealed prior canonical generation")
		}
		prior := safefs.RootFileGeneration{
			Exists: bound.PriorExists, Body: append([]byte(nil), bound.PriorBody...), Mode: bound.PriorMode,
		}
		if err := safefs.RestoreRootFileTransaction(projectRoot, target, published, prior); err != nil {
			return nil, fmt.Errorf("restore prior canonical generation: %w", err)
		}
		if bound.State == backup.CommitTransactionPending {
			if err := bound.Store.AbortPendingCommitFromRoot(homeRoot, threadpointHome); err != nil {
				return nil, fmt.Errorf("tombstone rolled-back commit transaction: %w", err)
			}
		}
		if err := safefs.CleanupRootFileTransaction(projectRoot, target, prior); err != nil {
			return nil, fmt.Errorf("clean rolled-back canonical transaction residue: %w", err)
		}
		return nil, nil //nolint:nilnil // rolled-back recovery continues as a fresh commit attempt.

	case backup.CommitTransactionCommitted:
		current, currentInfo, exists, err := readCanonicalGenerationUnderRoot(projectRoot, filepath.FromSlash(target), target)
		if err != nil {
			return nil, err
		}
		marker := []byte(`threadpoint:committed id="` + safeID(record.ID) + `"`)
		if !exists || digest(current) != bound.TargetSHA256 || int64(len(current)) != bound.TargetSize || !bytes.Contains(current, marker) {
			return nil, errors.New("committed backup transaction does not match the current canonical generation")
		}
		if bound.PriorKnown && !bytes.Equal(current, published.Body) {
			return nil, errors.New("committed canonical generation does not match the sealed prior and reviewed content")
		}
		published.Body = append([]byte(nil), current...)
		published.Mode = currentInfo.Mode().Perm()
		if err := safefs.CleanupRootFileTransaction(projectRoot, target, published); err != nil {
			return nil, fmt.Errorf("clean committed canonical transaction residue: %w", err)
		}
		return &commitBackupTransaction{
			record: record, projectRoot: projectRootPath, threadpointHome: threadpointHome,
			homeRoot: homeRoot, store: bound.Store,
			snapshot: &CanonicalTransactionSnapshot{
				Target: target, Proposed: append([]byte(nil), current...), PriorExists: bound.PriorExists,
				PriorBody: append([]byte(nil), bound.PriorBody...), PriorMode: bound.PriorMode,
			},
		}, nil
	default:
		return nil, errors.New("commit recovery transaction has an unsupported state")
	}
}

func proposedCanonicalGeneration(normalized, prior []byte) []byte {
	if len(prior) == 0 {
		return append([]byte(nil), normalized...)
	}
	proposed := make([]byte, 0, len(prior)+1+len(normalized))
	proposed = append(proposed, prior...)
	proposed = append(proposed, '\n')
	return append(proposed, normalized...)
}
