// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockSetBorrowProjectRootRetainsSelectedLogicalGeneration(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	selected := filepath.Join(parent, "selected")
	stateHome := filepath.Join(parent, "state")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(first, selected); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	locks, err := AcquireLocks(stateHome, []string{selected}, "project-root-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := locks.Release(); err != nil {
			t.Errorf("release locks: %v", err)
		}
	}()
	root, err := locks.BorrowProjectRoot(selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("retained", []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(selected); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, selected); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("after-retarget", []byte("still first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := locks.BorrowProjectRoot(selected); err == nil || !strings.Contains(err.Error(), "locked generation") {
		t.Fatalf("retargeted logical project remained borrowable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(first, "after-retarget")); err != nil {
		t.Fatalf("retained root did not remain on first generation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "after-retarget")); !os.IsNotExist(err) {
		t.Fatalf("retained root followed replacement generation: %v", err)
	}
}

func TestLockSetBorrowProjectRootRejectsUnselectedRoot(t *testing.T) {
	selected := t.TempDir()
	unselected := t.TempDir()
	locks, err := AcquireLocks(t.TempDir(), []string{selected}, "project-root-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := locks.Release(); err != nil {
			t.Errorf("release locks: %v", err)
		}
	}()
	if _, err := locks.BorrowProjectRoot(unselected); err == nil || !strings.Contains(err.Error(), "not locked") {
		t.Fatalf("unselected root borrow = %v", err)
	}
}

func TestAcquireLocksDeduplicatesRootsAndAllowsEmptySelection(t *testing.T) {
	root := t.TempDir()

	if locks, err := AcquireLocks(filepath.Join(root, "home"), nil, "test"); err != nil || len(locks.locks) != 0 {
		t.Fatalf("empty lock set = %#v, %v", locks, err)
	}
	second := t.TempDir()
	locks, err := AcquireLocks(filepath.Join(root, "home"), []string{root, second, root}, "test")
	if err != nil || len(locks.locks) != 2 {
		t.Fatalf("multi-root locks = %#v, %v", locks, err)
	}
	_ = locks.Release()
}

func TestAcquireLocksRejectsSecondHolderAndAllowsAfterRelease(t *testing.T) {
	stateHome := t.TempDir()
	root := t.TempDir()

	first, err := AcquireLocks(stateHome, []string{root}, "first")
	if err != nil {
		t.Fatal(err)
	}
	_, err = AcquireLocks(stateHome, []string{root}, "second")
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("expected lock contention, got %v", err)
	}
	if !strings.Contains(err.Error(), "command first") || !strings.Contains(err.Error(), "pid ") {
		t.Fatalf("expected owner details in lock error, got %v", err)
	}
	_ = first.Release()

	second, err := AcquireLocks(stateHome, []string{root}, "second")
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Release()
}

func TestLockSetBorrowProductRootRetainsLockedHomeGeneration(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "threadpoint")
	project := filepath.Join(parent, "project")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sentinel"), []byte("locked generation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	locks, err := AcquireLocks(home, []string{project}, "borrow-root-test")
	if err != nil {
		t.Fatal(err)
	}
	borrowed, err := locks.BorrowProductRoot()
	if err != nil {
		t.Fatal(err)
	}
	detached := home + ".detached"
	if err := os.Rename(home, detached); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sentinel"), []byte("replacement generation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := borrowed.ReadFile("sentinel")
	if err != nil || string(body) != "locked generation\n" {
		t.Fatalf("borrowed product root followed replacement: body=%q err=%v", body, err)
	}
	if _, err := locks.BorrowProductRoot(); err == nil || !strings.Contains(err.Error(), "no longer identifies the locked generation") {
		t.Fatalf("expected replacement product home to invalidate a later borrow, got %v", err)
	}
	if err := locks.Release(); err != nil {
		t.Fatalf("release through retained lock roots: %v", err)
	}
	if _, err := locks.BorrowProductRoot(); err == nil {
		t.Fatal("released lock set still exposed a product-home root")
	}
}

func TestLockSetBorrowProductRootRejectsRetargetedSelectedSymlink(t *testing.T) {
	parent := t.TempDir()
	firstHome := filepath.Join(parent, "first-home")
	secondHome := filepath.Join(parent, "second-home")
	selectedHome := filepath.Join(parent, "threadpoint-home")
	project := filepath.Join(parent, "project")
	for _, path := range []string{firstHome, secondHome, project} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	symlinkOrSkip(t, firstHome, selectedHome)
	locks, err := AcquireLocks(selectedHome, []string{project}, "selected-symlink-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locks.BorrowProductRoot(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(selectedHome); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondHome, selectedHome); err != nil {
		t.Fatal(err)
	}
	if _, err := locks.BorrowProductRoot(); err == nil || !strings.Contains(err.Error(), "no longer identifies the locked generation") {
		t.Fatalf("expected selected symlink retarget to invalidate the product root, got %v", err)
	}
	if err := locks.Release(); err != nil {
		t.Fatalf("release through retained lock roots: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(secondHome, "locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retargeted product home received lock state: %v", err)
	}
}

func TestAcquireLocksConcurrentFirstUseAdmitsOneHolder(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is required")
	}
	stateHome := filepath.Join(t.TempDir(), "threadpoint-home")
	projectRoot := t.TempDir()
	start := make(chan struct{})
	type result struct {
		set *LockSet
		err error
	}
	results := make(chan result, 8)
	for index := 0; index < cap(results); index++ {
		go func(index int) {
			<-start
			set, err := AcquireLocks(stateHome, []string{projectRoot}, fmt.Sprintf("holder-%d", index))
			results <- result{set: set, err: err}
		}(index)
	}
	close(start)

	var winner *LockSet
	for index := 0; index < cap(results); index++ {
		result := <-results
		if result.err == nil {
			if winner != nil {
				_ = result.set.Release()
				_ = winner.Release()
				t.Fatal("concurrent first use admitted more than one holder")
			}
			winner = result.set
			continue
		}
		if !errors.Is(result.err, ErrLockHeld) {
			if winner != nil {
				_ = winner.Release()
			}
			t.Fatalf("concurrent first-use error = %v", result.err)
		}
	}
	if winner == nil {
		t.Fatal("concurrent first use admitted no holder")
	}
	if err := winner.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireLocksFailsClosedWhenLocksDirectoryIsReplaced(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is required")
	}
	stateHome := t.TempDir()
	projectRoot := t.TempDir()

	first, err := AcquireLocks(stateHome, []string{projectRoot}, "first")
	if err != nil {
		t.Fatal(err)
	}
	detachedLocks := filepath.Join(stateHome, "locks-detached")
	if err := os.Rename(filepath.Join(stateHome, "locks"), detachedLocks); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(stateHome, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	replacementPath := lockPath(stateHome, projectRoot)
	replacementBody := []byte(`{"pid":2147483646,"created":"2026-01-01T00:00:00Z","token":"replacement"}` + "\n")
	if err := os.WriteFile(replacementPath, replacementBody, 0o600); err != nil {
		t.Fatal(err)
	}

	if second, err := AcquireLocks(stateHome, []string{projectRoot}, "second"); err == nil {
		_ = second.Release()
		t.Fatal("replacement locks directory admitted a second holder")
	} else if !strings.Contains(err.Error(), "identity is missing") {
		t.Fatalf("replacement locks directory error = %v", err)
	}
	if body, err := os.ReadFile(replacementPath); err != nil || !bytes.Equal(body, replacementBody) {
		t.Fatalf("failed acquisition reclaimed replacement-namespace entry: body=%q err=%v", body, err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release through retained locks root: %v", err)
	}
	oldPath := filepath.Join(detachedLocks, lockName(projectRoot)+".lock")
	if _, err := os.Lstat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release did not remove the held entry from its pinned root: %v", err)
	}
	if body, err := os.ReadFile(replacementPath); err != nil || !bytes.Equal(body, replacementBody) {
		t.Fatalf("release deleted or changed the replacement-namespace entry: body=%q err=%v", body, err)
	}
}

func TestAcquireLocksRejectsLocksSymlinkBeforeChmod(t *testing.T) {
	stateHome := t.TempDir()
	projectRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(stateHome, "locks"))

	if _, err := AcquireLocks(stateHome, []string{projectRoot}, "test"); !errors.Is(err, ErrUnsafeSymlink) {
		t.Fatalf("symlinked locks directory error = %v", err)
	}
	if info, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o755 {
		t.Fatalf("symlink target mode changed before rejection: %o", info.Mode().Perm())
	}
}

func TestAcquireLocksTreatsLogicalAndPhysicalRootsAsOneProject(t *testing.T) {
	stateHome := t.TempDir()
	physicalRoot := t.TempDir()
	logicalRoot := filepath.Join(t.TempDir(), "project-link")
	symlinkOrSkip(t, physicalRoot, logicalRoot)

	first, err := AcquireLocks(stateHome, []string{logicalRoot}, "logical")
	if err != nil {
		t.Fatal(err)
	}

	_, err = AcquireLocks(stateHome, []string{physicalRoot}, "physical")
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("physical root did not contend with logical root: %v", err)
	}

	_ = first.Release()
	second, err := AcquireLocks(stateHome, []string{physicalRoot}, "physical")
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Release()
}

func TestAcquireLocksReclaimsDeadOwner(t *testing.T) {
	stateHome := t.TempDir()
	root := t.TempDir()
	// PID 0x7ffffffe is far above any real PID, so signal-0 reports it as gone.
	seedLock(t, stateHome, root, lockOwner{Command: "stale", PID: 0x7ffffffe, Root: root, Created: time.Now().UTC()})

	set, err := AcquireLocks(stateHome, []string{root}, "fresh")
	if err != nil {
		t.Fatalf("expected a dead-owner lock to be reclaimed, got %v", err)
	}
	assertLockFileOwner(t, stateHome, root)
	_ = set.Release()
}

func TestAcquireLocksReclaimsAgedOwner(t *testing.T) {
	stateHome := t.TempDir()
	root := t.TempDir()
	seedLock(t, stateHome, root, lockOwner{Command: "old", PID: os.Getpid(), Root: root, Created: time.Now().UTC().Add(-48 * time.Hour)})

	set, err := AcquireLocks(stateHome, []string{root}, "fresh")
	if err != nil {
		t.Fatalf("expected an aged lock to be reclaimed, got %v", err)
	}
	assertLockFileOwner(t, stateHome, root)
	_ = set.Release()
}

func TestAcquireLocksRefusesLiveRecentOwner(t *testing.T) {
	stateHome := t.TempDir()
	root := t.TempDir()
	seedLock(t, stateHome, root, lockOwner{Command: "live", PID: os.Getpid(), Root: root, Created: time.Now().UTC()})

	if _, err := AcquireLocks(stateHome, []string{root}, "fresh"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("expected a live, recent lock to be refused, got %v", err)
	} else if !strings.Contains(err.Error(), "command live") || !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Fatalf("lock contention did not report the captured owner metadata: %v", err)
	}
}

func TestAcquireLocksRefusesFreshMalformedLockFile(t *testing.T) {
	stateHome := t.TempDir()
	root := t.TempDir()
	seedLockFile(t, stateHome, root, []byte("{"), 0)

	if _, err := AcquireLocks(stateHome, []string{root}, "fresh"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("expected a fresh malformed lock to be refused, got %v", err)
	}
}

func TestAcquireLocksRejectsDirectoryAtLockPath(t *testing.T) {
	stateHome := t.TempDir()
	root := t.TempDir()
	if err := os.MkdirAll(lockPath(stateHome, root), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := AcquireLocks(stateHome, []string{root}, "fresh")
	var lockErr FileLockError
	if !errors.As(err, &lockErr) || lockErr.Reason != FileLockErrorUnsafe {
		t.Fatalf("expected directory lock path to be rejected as unsafe, got %v", err)
	}
}

func TestAcquireLocksReclaimsOldMalformedLockFile(t *testing.T) {
	stateHome := t.TempDir()
	root := t.TempDir()
	seedLockFile(t, stateHome, root, []byte("{"), lockMetadataGrace+time.Second)

	set, err := AcquireLocks(stateHome, []string{root}, "fresh")
	if err != nil {
		t.Fatalf("expected an old malformed lock to be reclaimed, got %v", err)
	}
	assertLockFileOwner(t, stateHome, root)
	_ = set.Release()
}

func TestLockRootsDeduplicateLogicalAndPhysicalPaths(t *testing.T) {
	physical := t.TempDir()
	logical := filepath.Join(t.TempDir(), "logical")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	roots, err := normalizedLockRoots([]string{"", logical, physical, logical})
	if err != nil || len(roots) != 1 || roots[0] != physical {
		t.Fatalf("normalized roots = %#v, %v", roots, err)
	}
}

func seedLock(t *testing.T, stateHome string, root string, owner lockOwner) {
	t.Helper()
	dir := filepath.Dir(lockPath(stateHome, root))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(owner, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(stateHome, root), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedLockFile(t *testing.T, stateHome string, root string, body []byte, age time.Duration) {
	t.Helper()
	path := lockPath(stateHome, root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		older := time.Now().Add(-age)
		if err := os.Chtimes(path, older, older); err != nil {
			t.Fatal(err)
		}
	}
}

func assertLockFileOwner(t *testing.T, stateHome string, root string) {
	t.Helper()
	const command = "fresh"
	path := lockPath(stateHome, root)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("expected lock path to be a regular file, got mode %v", info.Mode())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var owner lockOwner
	if err := json.Unmarshal(body, &owner); err != nil {
		t.Fatal(err)
	}
	if owner.Command != command {
		t.Fatalf("expected command %q, got %q", command, owner.Command)
	}
	if owner.Token == "" {
		t.Fatal("expected lock owner token to be set")
	}
}

func TestProjectLockSyncFailureLeavesNamespaceRetryable(t *testing.T) {
	originalSync := syncFileLockParent
	t.Cleanup(func() { syncFileLockParent = originalSync })
	for stop := 1; stop <= 20; stop++ {
		home, project := t.TempDir(), t.TempDir()
		sentinel := errors.New("lock namespace sync failed")
		calls := 0
		syncFileLockParent = func(root *os.Root) error {
			calls++
			if calls == stop {
				return sentinel
			}
			return originalSync(root)
		}
		locks, err := AcquireLocks(home, []string{project}, "fixture")
		syncFileLockParent = originalSync
		if locks != nil {
			if releaseErr := locks.Release(); releaseErr != nil {
				t.Fatal(releaseErr)
			}
		}
		if calls >= stop && !errors.Is(err, sentinel) {
			t.Fatalf("sync %d failure lost: %v", stop, err)
		}
		if calls < stop && err != nil {
			t.Fatal(err)
		}
		retry, err := AcquireLocks(home, []string{project}, "retry")
		if err != nil {
			t.Fatalf("failed namespace publication blocked retry: %v", err)
		}
		if err := retry.Release(); err != nil {
			t.Fatal(err)
		}
		if calls < stop {
			break
		}
		if stop == 20 {
			t.Fatal("sync sweep did not finish")
		}
	}
}

func TestLockNamespaceRequiresMatchingPrivateIdentityPair(t *testing.T) {
	for _, scenario := range []string{"locks-half-only", "root-half-only", "mismatch", "invalid-root-json", "invalid-locks-json", "root-mode", "locks-mode"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			locksDir := filepath.Join(dir, "locks")
			if err := os.Mkdir(locksDir, 0o700); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			locks, err := os.OpenRoot(locksDir)
			if err != nil {
				t.Fatal(err)
			}
			defer locks.Close()
			rootPath, locksPath := filepath.Join(dir, productLocksRootIdentityName), filepath.Join(locksDir, locksRootIdentityName)
			body := []byte(`{"version":1,"token":"fixture"}`)
			if scenario != "locks-half-only" {
				if err := os.WriteFile(rootPath, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "root-half-only" {
				if err := os.WriteFile(locksPath, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "mismatch":
				if err := os.WriteFile(locksPath, []byte(`{"version":1,"token":"other"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "invalid-root-json":
				if err := os.WriteFile(rootPath, []byte("invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "invalid-locks-json":
				if err := os.WriteFile(locksPath, []byte("invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "root-mode":
				if err := os.Chmod(rootPath, 0o644); err != nil {
					t.Fatal(err)
				}
			case "locks-mode":
				if err := os.Chmod(locksPath, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			beforeRoot, _ := os.ReadFile(rootPath)
			beforeLocks, _ := os.ReadFile(locksPath)
			err = ensureLocksRootIdentity(root, locks)
			if scenario == "locks-half-only" {
				if err != nil {
					t.Fatalf("complete interrupted pair: %v", err)
				}
				current, err := os.ReadFile(rootPath)
				if err != nil || !bytes.Equal(current, body) {
					t.Fatalf("completed root identity=%q %v", current, err)
				}
			} else {
				if err == nil {
					t.Fatal("ambiguous identity pair accepted")
				}
				afterRoot, _ := os.ReadFile(rootPath)
				afterLocks, _ := os.ReadFile(locksPath)
				if !bytes.Equal(beforeRoot, afterRoot) || !bytes.Equal(beforeLocks, afterLocks) {
					t.Fatal("rejected pair was modified")
				}
			}
		})
	}
}

func TestLockOperationsFailWhenRequiredLocationsCannotBeResolved(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("THREADPOINT_HOME", "")
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"acquire-default-home", func() error {
			locks, err := AcquireLocks("", []string{project}, "test")
			if locks != nil {
				_ = locks.Release()
			}
			return err
		}},
		{"clear-default-home", func() error { _, err := ClearProjectLock("", project, true); return err }},
		{"clear-empty-project", func() error { _, err := ClearProjectLock(home, "", true); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); err == nil {
				t.Fatal("unresolved lock location accepted")
			}
		})
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"acquire-relative-home", func() error {
			locks, err := AcquireLocks(".", []string{project}, "test")
			if locks != nil {
				_ = locks.Release()
			}
			return err
		}},
		{"acquire-relative-project", func() error {
			locks, err := AcquireLocks(home, []string{"."}, "test")
			if locks != nil {
				_ = locks.Release()
			}
			return err
		}},
		{"clear-relative-home", func() error { _, err := ClearProjectLock(".", project, true); return err }},
		{"clear-relative-project", func() error { _, err := ClearProjectLock(home, ".", true); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); err == nil {
				t.Fatal("unresolved relative lock location accepted")
			}
		})
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("invalid lock request changed home: %v %v", entries, err)
	}
}

func TestLockRootPinningRefusesUnavailableNamespaces(t *testing.T) {
	for _, failure := range []string{"home-file", "home-unreadable", "home-unwritable", "locks-file", "locks-symlink", "locks-unreadable", "identity-directory", "relative-without-cwd"} {
		t.Run(failure, func(t *testing.T) {
			home := t.TempDir()
			switch failure {
			case "home-file":
				home = filepath.Join(home, "file")
				if err := os.WriteFile(home, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "home-unreadable":
				if err := os.Chmod(home, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(home, 0o700) }()
			case "home-unwritable":
				if err := os.Chmod(home, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(home, 0o700) }()
			case "locks-file":
				if err := os.WriteFile(filepath.Join(home, "locks"), []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "locks-symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(home, "locks")); err != nil {
					t.Fatal(err)
				}
			case "locks-unreadable":
				if err := os.Mkdir(filepath.Join(home, "locks"), 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(filepath.Join(home, "locks"), 0o700) }()
			case "identity-directory":
				if err := os.Mkdir(filepath.Join(home, productLocksRootIdentityName), 0o700); err != nil {
					t.Fatal(err)
				}
			case "relative-without-cwd":
				cwd := t.TempDir()
				t.Chdir(cwd)
				if err := os.Remove(cwd); err != nil {
					t.Fatal(err)
				}
				home = "relative"
			}
			product, locks, err := openPinnedLocksRoot(home)
			if product != nil {
				_ = product.Close()
			}
			if locks != nil {
				_ = locks.Close()
			}
			if err == nil || product != nil || locks != nil {
				t.Fatalf("unsafe namespace retained handles: %v %v %v", product, locks, err)
			}
			if failure == "home-file" || failure == "locks-file" {
				path := home
				if failure == "locks-file" {
					path = filepath.Join(home, "locks")
				}
				body, err := os.ReadFile(path)
				if err != nil || string(body) != "foreign" {
					t.Fatalf("foreign file changed: %q %v", body, err)
				}
			}
		})
	}
}
