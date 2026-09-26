// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package safefs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestAtomicCopyFileDoesNotBlockWhenSourceBecomesFIFO(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(source, []byte("reviewed source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	atomicCopyBeforeSourceOpen = func(path string) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { atomicCopyBeforeSourceOpen = nil }()

	done := make(chan error, 1)
	go func() { done <- AtomicCopyFile(source, target, 0o644) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected raced FIFO source to be rejected")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("atomic copy blocked after the source became a FIFO")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("raced FIFO copy created a target: %v", err)
	}
}
