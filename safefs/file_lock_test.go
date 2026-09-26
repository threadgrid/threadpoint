// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileLockErrorUsesStableMessagesAndUnwrapsCauses(t *testing.T) {
	cause := errors.New("lock metadata could not be read")
	for _, test := range []struct {
		name string
		err  FileLockError
		want string
	}{
		{name: "explicit", err: FileLockError{Message: "custom lock message", Err: cause}, want: "custom lock message"},
		{name: "unsafe", err: FileLockError{Reason: FileLockErrorUnsafe}, want: "file lock path is unsafe"},
		{name: "held", err: FileLockError{Reason: FileLockErrorHeld}, want: "file lock is already held"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.err.Error(); got != test.want {
				t.Fatalf("message = %q, want %q", got, test.want)
			}
		})
	}
	if !errors.Is(FileLockError{Err: cause}, cause) {
		t.Fatal("file lock error should unwrap its explicit cause")
	}
}

func TestAcquireFileLockWritesPrivateModesAndReleasesOwnedLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "locks", "state-root.lock")
	body := []byte(`{"pid":1,"created_at":"2026-06-09T12:00:00Z","lock_id":"owned"}` + "\n")

	lock, err := AcquireFileLock(context.Background(), FileLockOptions{
		Path:           path,
		Body:           body,
		DirMode:        0o700,
		FileMode:       0o600,
		EnforceDirMode: true,
		ReleaseMatches: func(current []byte) bool {
			return strings.Contains(string(current), `"lock_id":"owned"`)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("lock dir mode = %o, want 700", info.Mode().Perm())
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %o, want 600", info.Mode().Perm())
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected release to remove lock, got %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second release = %v, want an idempotent no-op", err)
	}
	var unset *FileLock
	if err := unset.Release(); err != nil {
		t.Fatalf("nil lock release = %v", err)
	}
}

func TestFileLockReleaseSucceedsAfterLockDirectoryRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "state-root.lock")
	lock, err := AcquireFileLock(context.Background(), FileLockOptions{
		Path:     path,
		Body:     []byte(`{"pid":1,"created_at":"2026-06-09T12:00:00Z","lock_id":"owned"}` + "\n"),
		DirMode:  0o700,
		FileMode: 0o600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("release after lock directory removal = %v", err)
	}
}

func TestAcquireFileLockReportsHeldLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "held.lock")
	lock, err := AcquireFileLock(context.Background(), FileLockOptions{
		Path: path,
		Body: []byte(fmt.Sprintf(`{"pid":%d,"created":"%s","token":"holder"}`+"\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Fatal(err)
		}
	}()

	_, err = AcquireFileLock(context.Background(), FileLockOptions{
		Path:         path,
		Body:         []byte(`{"pid":1}` + "\n"),
		WaitTimeout:  5 * time.Millisecond,
		PollInterval: time.Millisecond,
	})
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("expected held lock classification, got %T %v", err, err)
	}
}

func TestAcquireFileLockReclaimsCreatedAtOwnerAfterStaleAge(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state-root.lock")
	old := time.Now().UTC().Add(-DefaultFileLockStaleAfter - time.Second).Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"pid":%d,"created_at":"%s","lock_id":"old"}`+"\n", os.Getpid(), old)), 0o644); err != nil {
		t.Fatal(err)
	}

	lock, err := AcquireFileLock(context.Background(), FileLockOptions{
		Path: path,
		Body: []byte(`{"pid":1,"created_at":"2099-01-01T00:00:00Z","lock_id":"fresh"}` + "\n"),
	})
	if err != nil {
		t.Fatalf("expected created_at stale lock to be reclaimed, got %v", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Fatal(err)
		}
	}()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"lock_id":"fresh"`) {
		t.Fatalf("expected fresh lock body, got %s", body)
	}
}

func TestAcquireFileLockRefusesFreshMalformedMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "malformed.lock")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := AcquireFileLock(context.Background(), FileLockOptions{
		Path: path,
		Body: []byte(`{"pid":1}` + "\n"),
	})
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("expected fresh malformed lock to be held, got %v", err)
	}
}

func TestFileLockReleaseRefusesDifferentOwner(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "release.lock")
	lock, err := AcquireFileLock(context.Background(), FileLockOptions{
		Path: path,
		Body: []byte(`{"token":"first"}` + "\n"),
		ReleaseMatches: func(body []byte) bool {
			return strings.Contains(string(body), `"token":"first"`)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"token":"second"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("expected release refusal for different owner, got %v", err)
	}
	if body, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(body), "second") {
		t.Fatalf("different owner lock was changed: %s", body)
	}
}

func TestRemoveReviewedFileLockRefusesReplacedLiveLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.lock")
	if err := os.WriteFile(path, []byte(`{"pid":1,"token":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	entry, err := reviewFileLockEntry(parent, "project.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Close()
	// Simulate another writer reclaiming the stale lock and installing a fresh
	// live lock (a new inode) between the staleness decision and the removal.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"pid":2,"token":"live"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeReviewedFileLock(parent, "project.lock", entry); !errors.Is(err, errFileLockChangedBeforeCleanup) {
		t.Fatalf("expected errFileLockChangedBeforeCleanup, got %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the freshly-installed live lock was deleted: %v", err)
	}
	if !strings.Contains(string(body), "live") {
		t.Fatalf("live lock body changed: %s", body)
	}
}

func TestFileLockStalePolicyHandlesMissingAndMalformedMetadata(t *testing.T) {
	now := time.Now().UTC()
	if stale, err := DefaultFileLockStalePolicy(FileLockSnapshot{}, now, time.Hour); err != nil || !stale {
		t.Fatalf("missing lock stale = %v, %v", stale, err)
	}
	if stale, err := DefaultFileLockStalePolicy(FileLockSnapshot{ModTime: now}, now, time.Hour); err != nil || stale {
		t.Fatalf("fresh unreadable metadata stale = %v, %v", stale, err)
	}
	if stale, err := DefaultFileLockStalePolicy(FileLockSnapshot{Body: []byte("not json"), ModTime: now.Add(-lockMetadataGrace - time.Second)}, now, time.Hour); err != nil || !stale {
		t.Fatalf("old malformed metadata stale = %v, %v", stale, err)
	}
	if stale, err := DefaultFileLockStalePolicy(FileLockSnapshot{Body: []byte(`{"pid":-1,"created_at":"bad"}`), ModTime: now}, now, time.Hour); err != nil || stale {
		t.Fatalf("fresh invalid owner stale = %v, %v", stale, err)
	}
}

func TestAcquireFileLockStopsWaitingOnCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waiting.lock")
	first, err := AcquireFileLock(context.Background(), FileLockOptions{Path: path, Body: []byte("owner")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Release(); err != nil {
			t.Errorf("release first lock: %v", err)
		}
	})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = AcquireFileLock(canceled, FileLockOptions{Path: path, Body: []byte("waiter"), WaitTimeout: time.Minute, PollInterval: time.Millisecond})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock wait error = %v", err)
	}
}

func TestReleaseDoesNotDeleteReacquiredLock(t *testing.T) {
	stateHome := t.TempDir()
	root := t.TempDir()

	first, err := AcquireLocks(stateHome, []string{root}, "first")
	if err != nil {
		t.Fatal(err)
	}
	makeLockFileAged(t, stateHome, root)

	second, err := AcquireLocks(stateHome, []string{root}, "second")
	if err != nil {
		t.Fatalf("expected aged lock to be reclaimed, got %v", err)
	}
	_ = first.Release()

	if _, err := AcquireLocks(stateHome, []string{root}, "third"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("expected reacquired lock to remain held, got %v", err)
	}
	_ = second.Release()
}

func makeLockFileAged(t *testing.T, stateHome string, root string) {
	t.Helper()
	path := lockPath(stateHome, root)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var owner lockOwner
	if err := json.Unmarshal(body, &owner); err != nil {
		t.Fatal(err)
	}
	owner.Created = time.Now().UTC().Add(-lockStaleAfter - time.Second)
	body, err = json.MarshalIndent(owner, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStaleLockReclamationPreservesChangedOwnerAndReportsPolicyFailure(t *testing.T) {
	for _, rooted := range []bool{false, true} {
		for _, outcome := range []string{"policy-error", "replacement", "removed", "read-only-parent", "directory-entry", "symlink-entry"} {
			t.Run(fmt.Sprintf("rooted=%v/%s", rooted, outcome), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "owner.lock")
				if err := os.WriteFile(path, []byte("old owner"), 0o600); err != nil {
					t.Fatal(err)
				}
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				sentinel := errors.New("cannot determine staleness")
				policy := func(FileLockSnapshot, time.Time, time.Duration) (bool, error) {
					switch outcome {
					case "policy-error":
						return false, sentinel
					case "replacement":
						if err := os.Rename(path, path+"-retained"); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte("new owner"), 0o600); err != nil {
							t.Fatal(err)
						}
					case "removed":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					case "read-only-parent":
						if err := os.Chmod(dir, 0o500); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
						if err := os.WriteFile(filepath.Join(dir, "permission-probe"), nil, 0o600); err == nil {
							t.Skip("filesystem does not enforce write permissions")
						}
					}
					return true, nil
				}
				if outcome == "directory-entry" || outcome == "symlink-entry" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if outcome == "directory-entry" {
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.Symlink("missing", path); err != nil {
							t.Fatal(err)
						}
					}
				}
				opts, err := normalizeFileLockOptions(FileLockOptions{Path: path, StalePolicy: policy})
				if err != nil {
					t.Fatal(err)
				}
				if rooted {
					err = reclaimStaleFileLockRoot(root, "owner.lock", opts)
				} else {
					err = reclaimStaleFileLock(opts)
				}
				switch outcome {
				case "replacement":
					if err != nil {
						t.Fatal(err)
					}
					if body, err := os.ReadFile(path); err != nil || string(body) != "new owner" {
						t.Fatalf("new owner lost: %q %v", body, err)
					}
				case "removed":
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("removed lock recreated: %v", err)
					}
				case "policy-error":
					if !errors.Is(err, sentinel) {
						t.Fatalf("policy error lost: %v", err)
					}
				default:
					if err == nil {
						t.Fatal("unsafe stale cleanup succeeded")
					}
					if _, err := os.Lstat(path); err != nil {
						t.Fatalf("unreviewed entry lost: %v", err)
					}
				}
			})
		}
	}
}

func TestRootedLockReleasePreservesUnownedOrUnavailableEntries(t *testing.T) {
	for _, failure := range []string{"nil-root", "closed-root", "directory", "foreign-owner", "unwritable", "changed-during-review", "missing"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.WriteFile("lock", []byte("owner"), 0o600); err != nil {
				t.Fatal(err)
			}
			lock := &FileLock{path: filepath.Join(dir, "lock")}
			switch failure {
			case "nil-root":
				root = nil
			case "closed-root":
				err = root.Close()
			case "directory":
				if err := root.Remove("lock"); err != nil {
					t.Fatal(err)
				}
				err = root.Mkdir("lock", 0o700)
			case "foreign-owner":
				lock.releaseMatches = func([]byte) bool { return false }
			case "unwritable":
				err = os.Chmod(dir, 0o500)
				defer func() { _ = os.Chmod(dir, 0o700) }()
			case "changed-during-review":
				lock.releaseMatches = func([]byte) bool {
					if err := root.Rename("lock", "retained"); err != nil {
						t.Fatal(err)
					}
					if err := root.WriteFile("lock", []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
					return true
				}
			case "missing":
				err = root.Remove("lock")
			}
			if err != nil {
				t.Fatal(err)
			}
			err = releaseFileLockFromRoot(lock, root, "lock")
			if failure == "missing" || failure == "changed-during-review" {
				if err != nil || !lock.released {
					t.Fatalf("release: %v completed=%v", err, lock.released)
				}
			} else if err == nil || lock.released {
				t.Fatalf("unsafe release: %v completed=%v", err, lock.released)
			}
			if failure != "directory" && failure != "missing" {
				want := "owner"
				if failure == "changed-during-review" {
					want = "foreign"
				}
				body, err := os.ReadFile(filepath.Join(dir, "lock"))
				if err != nil || string(body) != want {
					t.Fatalf("lock changed: %q %v", body, err)
				}
			}
		})
	}
}
