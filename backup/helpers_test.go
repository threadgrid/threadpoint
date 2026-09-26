// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustWriteFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func preparedCommitStore(t *testing.T, root, home, runID string) *Store {
	t.Helper()
	source := []byte("native source\n")
	sourceHash := sha256.Sum256(source)
	target := []byte("canonical publication\n")
	targetHash := sha256.Sum256(target)
	store, err := NewStore(StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: PendingCommitOperation, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration(strings.TrimPrefix(runID, "commit-"), "native.md", hex.EncodeToString(sourceHash[:]), "AGENTS.md", hex.EncodeToString(targetHash[:]), int64(len(target))); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, source, 0o600, nil, os.FileMode(0), false)

	return store
}

func pendingCommitTestBinding(stageID string, source, target []byte) CommitBinding {
	sourceHash := sha256.Sum256(source)
	targetHash := sha256.Sum256(target)
	return CommitBinding{
		StageID:      stageID,
		SourcePath:   "providers/native.md",
		SourceSHA256: hex.EncodeToString(sourceHash[:]),
		TargetPath:   "AGENTS.md",
		TargetSHA256: hex.EncodeToString(targetHash[:]),
		TargetSize:   int64(len(target)),
	}
}
