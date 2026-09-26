// SPDX-License-Identifier: Apache-2.0

package prune

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func writeMalformedBackupManifestFixture(t *testing.T, store *backup.Store) {
	t.Helper()
	body, err := json.MarshalIndent(store.Manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(store.RunDir, backup.ManifestName), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedStageSourceSnapshot(t *testing.T, root string, home string, source string) {
	t.Helper()
	seedStageSourceSnapshotRel(t, root, home, "CLAUDE.md", source)
}

func seedStageSourceSnapshotRel(t *testing.T, root string, home string, rel string, source string) string {
	t.Helper()
	sourceBody, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	stageID := "prune-" + pruneSHA256([]byte(filepath.ToSlash(rel) + "\x00" + pruneSHA256(sourceBody)))[:16]
	targetRel := filepath.ToSlash(filepath.Join(".agents", "prune-fixture", stageID+".md"))
	targetBody := []byte("canonical fixture " + stageID + "\n")
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root, HomeDir: home, Operation: backup.PendingCommitOperation, RunID: "commit-" + stageID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration(stageID, rel, pruneSHA256(sourceBody), targetRel, pruneSHA256(targetBody), int64(len(targetBody))); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, readPendingFixtureSource(t, source), pendingFixtureSourceMode(t, source), nil, os.FileMode(0), false)

	mustWriteFile(t, filepath.Join(root, filepath.FromSlash(targetRel)), string(targetBody))
	if err := store.FinalizePendingCommit(); err != nil {
		t.Fatal(err)
	}
	return store.RunID
}

func mustWriteFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRootFile(t *testing.T, root *os.Root, path string, body string) {
	t.Helper()
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(body); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

type pruneRecoveryFixture struct {
	root        string
	projectRoot *os.Root
	snapshot    pruneRootSnapshot
	parent      *pruneTargetParent
	recovery    *pruneRecovery
}

func newPruneRecoveryFixture(t *testing.T) *pruneRecoveryFixture {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "nested", "native.md"), "canonical")
	snapshot, projectRoot, err := captureAndOpenPruneRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := openPruneTargetParent(projectRoot, snapshot, Candidate{Path: "nested/native.md"})
	if err != nil {
		_ = projectRoot.Close()
		t.Fatal(err)
	}
	recovery, err := createPruneRecovery(parent, snapshot, Candidate{Path: "nested/native.md"})
	if err != nil {
		_ = parent.root.Close()
		_ = projectRoot.Close()
		t.Fatal(err)
	}
	fixture := &pruneRecoveryFixture{
		root:        root,
		projectRoot: projectRoot,
		snapshot:    snapshot,
		parent:      parent,
		recovery:    recovery,
	}
	t.Cleanup(func() {
		recovery.close()
		_ = parent.root.Close()
		_ = projectRoot.Close()
	})
	return fixture
}

func addPruneRecoveryItem(t *testing.T, fixture *pruneRecoveryFixture, body string) os.FileInfo {
	t.Helper()
	writeRootFile(t, fixture.recovery.root, fixture.recovery.itemName, body)
	info, err := fixture.recovery.root.Lstat(fixture.recovery.itemName)
	if err != nil {
		t.Fatal(err)
	}
	fixture.recovery.itemInfo = info
	return info
}
