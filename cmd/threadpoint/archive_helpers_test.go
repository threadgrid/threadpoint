// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func FuzzArchiveEntryName(f *testing.F) {
	for _, seed := range []string{"threadpoint", "./threadpoint", "../evil", "/abs", "a\\b", "..", "dir/threadpoint", "", "./..", "x/../y"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		name, err := archiveEntryName(raw)
		if err != nil {
			return
		}
		// Any accepted entry name must be safe to extract under a directory.
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.Contains(name, "\\") {
			t.Fatalf("archiveEntryName(%q) accepted unsafe name %q", raw, name)
		}
	})
}
