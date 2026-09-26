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

func TestRestoreDetachIdentityPinRejectsRacedFIFOWithoutBlocking(t *testing.T) {
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
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	var hookErr error
	hooks := &restoreHooks{beforeDetachIdentityPin: func(_ *os.Root, _ string) {
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
		_, applyErr := applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
		done <- applyErr
	}()
	select {
	case applyErr := <-done:
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if applyErr == nil || !strings.Contains(applyErr.Error(), "non-regular") {
			t.Fatalf("expected raced FIFO identity-pin refusal, got %v", applyErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restore blocked while its detach identity pin raced to a FIFO")
	}
	if info, statErr := os.Lstat(target); statErr != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("raced FIFO should remain canonical, info=%v err=%v", info, statErr)
	}
}
