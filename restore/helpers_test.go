// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/threadgrid/threadpoint/backup"
)

// writeStoreManifestFixtureUnchecked deliberately persists malformed reader
// input. Production Store.Save rejects these current-schema violations.
func writeStoreManifestFixtureUnchecked(t *testing.T, store *backup.Store) {
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

func mustFileSHA256(t *testing.T, path string) string {
	t.Helper()
	hash, err := backup.FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func seedFileBackup(t *testing.T, root string, home string, target string, after any) string {
	t.Helper()
	now := time.Date(2026, 5, 30, 1, 0, 0, 0, time.UTC)
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root,
		HomeDir:     home,
		Operation:   "fixture",
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("AGENTS.md", target, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	switch fn := after.(type) {
	case func():
		fn()
		if err := store.SetPostHash("AGENTS.md", target); err != nil {
			t.Fatal(err)
		}
	case string:
		if fn != "" {
			t.Fatalf("unsupported seed marker %q", fn)
		}
	case nil:
	default:
		t.Fatalf("unsupported seed callback type %T", after)
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

// buildRetainedRestorePlan keeps the reviewed filesystem capabilities alive
// until the apply scenario has finished, as the production apply flow does.
func buildRetainedRestorePlan(t *testing.T, ctx context.Context, opts Options) (*Plan, error) {
	t.Helper()
	plan, root, err := buildRestorePlanWithPinnedRoot(ctx, opts)
	if root != nil {
		t.Cleanup(func() { _ = closeRestorePlanPins(plan); _ = root.Close() })
	}
	return plan, err
}

func applyResolutions(ctx context.Context, opts Options, plan *Plan, pending []pendingResolution) (*Report, error) {
	if err := validateRestoreRootSnapshot(plan.rootSnapshot); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(plan.rootSnapshot.physicalRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return applyResolutionsWithPinnedRoot(ctx, opts, plan, pending, root)
}
