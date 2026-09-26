// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package walk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadSmallTextFileDoesNotBlockOnRacedFIFO(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "probe")
	if err := makeTraversalTestFIFO(probe); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "raced.md")
	mustWrite(t, path, "text")
	type outcome struct {
		warning string
		ok      bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		var hookErr error
		_, warning, ok, err := readSmallTextFile(path, DefaultMaxFileSize, func() {
			if removeErr := os.Remove(path); removeErr != nil {
				hookErr = removeErr
				return
			}
			if fifoErr := makeTraversalTestFIFO(path); fifoErr != nil {
				hookErr = fifoErr
			}
		})
		if hookErr != nil {
			err = hookErr
		}
		done <- outcome{warning: warning, ok: ok, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.ok || (!strings.Contains(got.warning, "changed before") && !strings.Contains(got.warning, "could not read")) {
			t.Fatalf("expected raced-FIFO refusal, got %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded text read blocked on a raced FIFO")
	}
}
