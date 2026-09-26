// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package restore

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

func TestApplyRejectsRacedFIFOInDirectoryBackupWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := makeRestoreFIFO(probe); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "backup")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupDir("native", target, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(entry.BackupPath, "guide.md")
	var hookErr error
	hooks := &restoreHooks{beforeDirectoryCopy: func() {
		if err := os.Remove(payload); err != nil {
			hookErr = err
			return
		}
		if err := makeRestoreFIFO(payload); err != nil {
			hookErr = err
		}
	}}
	done := make(chan error, 1)
	go func() {
		_, err := applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
		done <- err
	}()
	select {
	case err := <-done:
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if err == nil || !strings.Contains(err.Error(), "payload entry guide.md identity changed") {
			t.Fatalf("expected raced directory FIFO refusal, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restore blocked while a directory backup payload raced to a FIFO")
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("failed directory preparation mutated target: %v", statErr)
	}
}
