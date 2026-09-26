// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"path/filepath"
	"testing"
)

func TestHomeResolutionHonorsConfiguredOverrides(t *testing.T) {
	home := t.TempDir()
	override := filepath.Join(home, "custom-threadpoint")
	resolvedOverride, err := ResolveThreadpointHomeWithOverride(home, override)
	if err != nil || resolvedOverride != override {
		t.Fatalf("threadpoint home override = %q, %v", resolvedOverride, err)
	}
	resolvedHome, err := ResolveThreadpointHome(home)
	if err != nil || resolvedHome != filepath.Join(home, ".threadpoint") {
		t.Fatalf("threadpoint home = %q, %v", resolvedHome, err)
	}
	userHome, err := ResolveUserHome(home)
	if err != nil || userHome != home {
		t.Fatalf("user home = %q, %v", userHome, err)
	}
}
