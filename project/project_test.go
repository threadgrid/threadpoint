// SPDX-License-Identifier: Apache-2.0

package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeInputDefaultsToWorkingDirectoryAndRejectsStalePWD(t *testing.T) {
	t.Setenv("PWD", filepath.Join(t.TempDir(), "not-the-working-directory"))
	want, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := normalizeInput("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(want) {
		t.Fatalf("normalizeInput default = %q, want %q", got, want)
	}
}

func TestProviderWorkspaceMarkersDiscardUnsafePaths(t *testing.T) {
	for _, marker := range []string{"", ".", "..", "../outside", string(filepath.Separator) + "absolute"} {
		if clean := cleanRelativeMarker(marker); clean != "" {
			t.Errorf("cleanRelativeMarker(%q) = %q, want empty", marker, clean)
		}
	}
}
