// SPDX-License-Identifier: Apache-2.0

package prune

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupEmptyPruneRecoveryRetainsUnverifiableState(t *testing.T) {
	if err := cleanupEmptyPruneRecovery(nil, nil); err != nil {
		t.Fatal(err)
	}

	t.Run("raced content", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		addPruneRecoveryItem(t, fixture, "raced")
		if err := cleanupEmptyPruneRecovery(fixture.parent, fixture.recovery); err == nil || !strings.Contains(err.Error(), "raced content") {
			t.Fatalf("nonempty recovery container was removed: %v", err)
		}
	})

	t.Run("closed descriptor", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		if err := fixture.recovery.root.Close(); err != nil {
			t.Fatal(err)
		}
		if err := cleanupEmptyPruneRecovery(fixture.parent, fixture.recovery); err == nil || !strings.Contains(err.Error(), "could not be inspected") {
			t.Fatalf("closed recovery container was treated as clean: %v", err)
		}
	})

	t.Run("empty exact container", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		container := filepath.Join(fixture.root, fixture.parent.path, fixture.recovery.containerName)
		if err := cleanupEmptyPruneRecovery(fixture.parent, fixture.recovery); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(container); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("empty recovery container remains: %v", err)
		}
	})
}
