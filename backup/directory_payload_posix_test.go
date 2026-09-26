// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package backup

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCopyDirRejectsFIFOWithoutBlocking(t *testing.T) {
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "copy")
	fifo := filepath.Join(source, "notes.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- CopyDir(source, target) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected FIFO directory entry to be rejected")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("directory backup blocked while inspecting a FIFO")
	}
	if _, err := os.Lstat(filepath.Join(target, "notes.md")); !os.IsNotExist(err) {
		t.Fatalf("FIFO directory entry created a backup payload: %v", err)
	}
}
