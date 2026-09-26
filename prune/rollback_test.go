// SPDX-License-Identifier: Apache-2.0

package prune

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyRollsBackPostDetachHashMismatchWithoutReplacement(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)

	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		afterDetach: func(_ Candidate, _ *os.Root, _ string, recovery *os.Root, item string) {
			file, openErr := recovery.OpenFile(item, os.O_WRONLY|os.O_TRUNC, 0)
			if openErr != nil {
				t.Fatal(openErr)
			}
			if _, writeErr := file.WriteString("late write"); writeErr != nil {
				t.Fatal(writeErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed during atomic detach") || !strings.Contains(err.Error(), "rolled back without replacement") {
		t.Fatalf("expected verified no-replace rollback, got report=%#v err=%v", report, err)
	}
	if body, readErr := os.ReadFile(source); readErr != nil || string(body) != "late write" {
		t.Fatalf("detached inode was not rolled back, body=%q err=%v", body, readErr)
	}
	if report != nil && len(report.Resolutions) != 0 && report.Resolutions[len(report.Resolutions)-1].Recovery != "" {
		t.Fatalf("successful rollback reported a stale recovery path: %#v", report.Resolutions)
	}
}

func TestApplyRollbackCollisionPreservesReplacementAndReportsRecovery(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")
	seedStageSourceSnapshot(t, root, home, source)

	report, err := applyWithHooks(context.Background(), Options{Root: root, HomeDir: home, Yes: true}, applyHooks{
		afterDetach: func(_ Candidate, _ *os.Root, _ string, recovery *os.Root, item string) {
			file, openErr := recovery.OpenFile(item, os.O_WRONLY|os.O_TRUNC, 0)
			if openErr != nil {
				t.Fatal(openErr)
			}
			_, _ = file.WriteString("late write")
			_ = file.Close()
		},
		beforeRollback: func(_ Candidate, parent *os.Root, base string, _ *os.Root, _ string) {
			writeRootFile(t, parent, base, "concurrent replacement")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "rollback refused") {
		t.Fatalf("expected rollback collision, got report=%#v err=%v", report, err)
	}
	if body, readErr := os.ReadFile(source); readErr != nil || string(body) != "concurrent replacement" {
		t.Fatalf("rollback overwrote replacement, body=%q err=%v", body, readErr)
	}
	if report == nil || len(report.Resolutions) != 1 || report.Resolutions[0].Recovery == "" {
		t.Fatalf("expected exact retained recovery report, got %#v", report)
	}
	if body, readErr := os.ReadFile(report.Resolutions[0].Recovery); readErr != nil || string(body) != "late write" {
		t.Fatalf("reported recovery did not retain detached inode, body=%q err=%v", body, readErr)
	}
}

func TestRollbackPruneNoReplaceFailsClosedAndRestoresExactItem(t *testing.T) {
	t.Run("canonical target reappeared", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		addPruneRecoveryItem(t, fixture, "original")
		if err := rollbackPruneNoReplace(fixture.parent, fixture.recovery); err == nil || !strings.Contains(err.Error(), "reappeared") {
			t.Fatalf("rollback overwrote canonical target: %v", err)
		}
	})

	t.Run("recovery item missing", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		if err := fixture.parent.root.Remove(fixture.parent.base); err != nil {
			t.Fatal(err)
		}
		if err := rollbackPruneNoReplace(fixture.parent, fixture.recovery); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing rollback item returned %v", err)
		}
	})

	t.Run("recovery item is not regular", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		if err := fixture.parent.root.Remove(fixture.parent.base); err != nil {
			t.Fatal(err)
		}
		if err := fixture.recovery.root.Mkdir(fixture.recovery.itemName, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := rollbackPruneNoReplace(fixture.parent, fixture.recovery); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("directory rollback item was accepted: %v", err)
		}
	})

	t.Run("recovery identity changed", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		if err := fixture.parent.root.Remove(fixture.parent.base); err != nil {
			t.Fatal(err)
		}
		addPruneRecoveryItem(t, fixture, "original")
		fixture.recovery.itemInfo = nil
		if err := rollbackPruneNoReplace(fixture.parent, fixture.recovery); err == nil || !strings.Contains(err.Error(), "changed identity") {
			t.Fatalf("unbound rollback item was accepted: %v", err)
		}
	})

	t.Run("exact item restored without replacement", func(t *testing.T) {
		fixture := newPruneRecoveryFixture(t)
		if err := fixture.parent.root.Remove(fixture.parent.base); err != nil {
			t.Fatal(err)
		}
		original := addPruneRecoveryItem(t, fixture, "original")
		if err := rollbackPruneNoReplace(fixture.parent, fixture.recovery); err != nil {
			t.Fatal(err)
		}
		current, err := fixture.parent.root.Lstat(fixture.parent.base)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(original, current) {
			t.Fatal("rollback did not restore the exact recovery inode")
		}
		if _, err := fixture.recovery.root.Lstat(fixture.recovery.itemName); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery source remained after rollback: %v", err)
		}
	})
}
