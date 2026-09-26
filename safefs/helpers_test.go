// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"os"
	"path/filepath"
	"testing"
)

func symlinkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

func lockPath(stateHome string, root string) string {
	return filepath.Join(stateHome, "locks", lockName(root)+".lock")
}
