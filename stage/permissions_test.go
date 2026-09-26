// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestCommitRollsBackWhenSourcePermissionsChangeAfterPublication(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	source := filepath.Join(root, "notes.md")
	write(t, source, "project shared source\n")
	if err := os.Chmod(source, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectShared},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage: %v %#v", err, report)
	}
	stage := report.Stages[0]
	canonical := filepath.Join(root, filepath.FromSlash(stage.Target))
	const prior = "prior canonical\n"
	write(t, canonical, prior)
	commitBeforePostPublishValidation = func() {
		commitBeforePostPublishValidation = nil
		if err := os.Chmod(source, 0o600); err != nil {
			panic(err)
		}
	}
	t.Cleanup(func() { commitBeforePostPublishValidation = nil })

	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err == nil || !strings.Contains(err.Error(), "changed; create a new stage") {
		t.Fatalf("expected source permission freshness refusal, got %v", err)
	}
	if body, err := os.ReadFile(canonical); err != nil || string(body) != prior {
		t.Fatalf("canonical was not rolled back after source chmod: body=%q err=%v", body, err)
	}
	if _, err := os.Stat(stage.Dir); err != nil {
		t.Fatalf("stage removed after source chmod refusal: %v", err)
	}
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root, ThreadpointHome: home,
		Operation: backup.PendingCommitOperation, RunID: "commit-" + stage.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	manifest, err := backup.ReadManifest(filepath.Join(store.RunDir, backup.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Operation != backup.AbortedCommitOperation || manifest.CommitTransaction == nil || manifest.CommitTransaction.State != backup.CommitTransactionAborted {
		t.Fatalf("source chmod transaction was not tombstoned: %+v", manifest)
	}
}
