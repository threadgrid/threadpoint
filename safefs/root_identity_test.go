// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMutationRootResolutionNormalizesSymlinkedMissingPaths(t *testing.T) {
	physical := t.TempDir()
	logical := filepath.Join(t.TempDir(), "logical")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolvedRoot, err := ResolveRootForMutation(filepath.Join(logical, "missing", "child"))
	if err != nil || resolvedRoot != filepath.Join(physical, "missing", "child") {
		t.Fatalf("resolved mutation root = %q, %v", resolvedRoot, err)
	}
	if _, err := ResolveRootForMutation(""); err == nil {
		t.Fatal("empty root unexpectedly resolved")
	}
}
