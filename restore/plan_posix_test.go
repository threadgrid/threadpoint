// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package restore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildPlanRejectsRacedFIFOCurrentTargetWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := makeRestoreFIFO(probe); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	var hookErr error
	hooks := &restoreHooks{beforeCurrentRegularRead: func(_ *os.Root, _ string) {
		if err := os.Remove(target); err != nil {
			hookErr = err
			return
		}
		if err := makeRestoreFIFO(target); err != nil {
			hookErr = err
		}
	}}
	done := make(chan error, 1)
	go func() {
		_, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, hooks: hooks})
		done <- err
	}()
	select {
	case err := <-done:
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("expected raced FIFO refusal, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BuildPlan blocked while a current target raced to a FIFO")
	}
}
