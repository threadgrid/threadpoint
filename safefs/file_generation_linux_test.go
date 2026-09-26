// SPDX-License-Identifier: Apache-2.0

//go:build linux

package safefs

import (
	"os"
	"testing"
)

func TestSameFileGenerationRejectsReusedInode(t *testing.T) {
	rootPath := t.TempDir()
	path := rootPath + "/generation"
	if err := os.WriteFile(path, []byte("reviewed"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 10_000; attempt++ {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		current, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(expected, current) {
			continue
		}
		if SameFileGeneration(expected, current) {
			t.Fatal("generation comparison accepted an immediately reused inode")
		}
		return
	}
	t.Skip("filesystem did not reuse the original inode during the bounded probe")
}
