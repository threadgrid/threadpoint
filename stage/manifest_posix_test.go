// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package stage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadRegularRejectsFifoAndSymlinkWithoutHanging(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "source.md")
	if err := makeStageFIFO(fifo); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readRegular(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected readRegular to reject a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readRegular blocked on a FIFO instead of failing closed")
	}

	regular := filepath.Join(dir, "real.md")
	write(t, regular, "content\n")
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(link); err == nil {
		t.Fatal("expected readRegular to reject a final-component symlink")
	}
	if body, err := readRegular(regular); err != nil || string(body) != "content\n" {
		t.Fatalf("readRegular of a regular file failed: body=%q err=%v", body, err)
	}
}
