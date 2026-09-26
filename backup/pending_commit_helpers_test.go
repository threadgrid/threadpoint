// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// publishPendingFixture creates complete current-format commit evidence through
// the production atomic publisher. Corrupt-fixture tests modify its result explicitly.
func publishPendingFixture(t *testing.T, store *Store, source []byte, sourceMode os.FileMode, prior []byte, priorMode os.FileMode, priorExists bool) *Store {
	t.Helper()
	backupBase := store.BackupRoot
	for filepath.Base(backupBase) != "backups" {
		parent := filepath.Dir(backupBase)
		if parent == backupBase {
			t.Fatal("fixture backup root has no backups ancestor")
		}
		backupBase = parent
	}
	home := filepath.Dir(backupBase)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	namespace, err := filepath.Rel(backupBase, store.BackupRoot)
	if err != nil {
		t.Fatal(err)
	}
	tx := store.Manifest.CommitTransaction
	result, err := PublishPendingCommitGenerationFromRoot(root, home, StoreOptions{
		ProjectRoot: store.Manifest.ProjectRoot, ThreadpointHome: home, BackupDir: namespace,
		Operation: PendingCommitOperation, RunID: store.RunID, Now: func() time.Time { return store.Manifest.CreatedAt },
	}, PendingCommitGeneration{
		Binding:    CommitBinding{StageID: tx.StageID, SourcePath: tx.SourcePath, SourceSHA256: tx.SourceSHA256, TargetPath: tx.TargetPath, TargetSHA256: tx.TargetSHA256, TargetSize: tx.TargetSize},
		SourceBody: source, SourceMode: sourceMode, PriorBody: prior, PriorMode: priorMode, PriorExists: priorExists,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
