// SPDX-License-Identifier: Apache-2.0

package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanReportsTraversalWarnings(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "shared")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "architecture.md"), "# Architecture")
	mustWrite(t, filepath.Join(root, ".agents", "node_modules", "ignored.md"), "ignored")
	mustWrite(t, filepath.Join(root, "outside.md"), "outside")
	if err := os.Symlink(filepath.Join(root, "outside.md"), filepath.Join(root, ".agents", "knowledge", "outside.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	kb, err := Run(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(kb.Warnings, "\n")
	for _, want := range []string{"node_modules", "symlinks are not followed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected warning %q in %#v", want, kb.Warnings)
		}
	}
}

func TestScanSkipsConfiguredDirectoryNames(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "shared")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "architecture.md"), "# Architecture")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "generated", "ignored.md"), "# Ignored")

	kb, err := Run(context.Background(), Options{Root: root, SkipDirNames: []string{"generated"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range kb.Records {
		if strings.Contains(strings.Join(record.Paths, ","), "generated") {
			t.Fatalf("configured directory was mapped: %#v", record)
		}
	}
	if !strings.Contains(strings.Join(kb.Warnings, "\n"), "ignored configured directory") {
		t.Fatalf("configured skip warning missing: %#v", kb.Warnings)
	}
}
