// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package prune

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildPlanRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := makePruneFIFO(source); err != nil {
		t.Skipf("FIFO unsupported: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("expected regular-file refusal, got %v", err)
		}
	case <-time.After(time.Second):
		// Release an implementation that incorrectly opened the FIFO so the test
		// process does not retain a blocked goroutine after reporting the failure.
		go func() {
			file, _ := os.OpenFile(source, os.O_WRONLY, 0)
			if file != nil {
				_ = file.Close()
			}
		}()
		t.Fatal("BuildPlan blocked while reading a FIFO")
	}
}
