// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClearFileLockRetainsFreshLockUnlessForced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	created := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"pid":%d,"created":"%s"}`+"\n", os.Getpid(), created)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearFileLock(FileLockClearOptions{Path: path}); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("expected fresh lock refusal, got %v", err)
	}
	result, err := ClearFileLock(FileLockClearOptions{Path: path, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Cleared || !result.Forced || result.Stale {
		t.Fatalf("unexpected clear result: %+v", result)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected forced cleanup to remove lock, got %v", err)
	}
}

func TestClearFileLockRemovesStaleLockWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, []byte(`{"pid":2147483646,"created":"2026-01-01T00:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ClearFileLock(FileLockClearOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Cleared || !result.Stale || result.Forced {
		t.Fatalf("unexpected clear result: %+v", result)
	}
}

func TestClearFileLockRejectsUnsafeEntriesAndPropagatesStalePolicyErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(t *testing.T, path string)
	}{
		{
			name: "directory",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized metadata",
			create: func(t *testing.T, path string) {
				t.Helper()
				file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(maxClearableFileLockMetadataBytes + 1); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lock")
			test.create(t, path)
			_, err := ClearFileLock(FileLockClearOptions{Path: path})
			var lockErr FileLockError
			if !errors.As(err, &lockErr) || lockErr.Reason != FileLockErrorUnsafe {
				t.Fatalf("ClearFileLock(%s) error = %v, want unsafe file-lock classification", test.name, err)
			}
		})
	}

	policyErr := errors.New("stale policy stopped")
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, []byte(`{"pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearFileLock(FileLockClearOptions{
		Path: path,
		StalePolicy: func(FileLockSnapshot, time.Time, time.Duration) (bool, error) {
			return false, policyErr
		},
	}); !errors.Is(err, policyErr) {
		t.Fatalf("stale-policy error = %v", err)
	}

	if result, err := ClearFileLock(FileLockClearOptions{Path: filepath.Join(t.TempDir(), "missing", "lock")}); err != nil || result.Cleared {
		t.Fatalf("missing lock result = %+v, err=%v", result, err)
	}
}

func TestClearProjectLockRemovesTheCanonicalLock(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is required")
	}
	threadpointHome := t.TempDir()
	projectRoot := t.TempDir()
	roots, err := normalizedLockRoots([]string{projectRoot})
	if err != nil || len(roots) != 1 {
		t.Fatalf("normalized lock roots = %#v, err=%v", roots, err)
	}
	path := filepath.Join(threadpointHome, "locks", lockName(roots[0])+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ClearProjectLock(threadpointHome, projectRoot, true)
	if err != nil || !result.Cleared || !result.Forced {
		t.Fatalf("project clear result = %+v, err=%v", result, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project lock was not removed: %v", err)
	}
}

func TestClearFileLockRefusesUnsafeParentAndInvalidOptions(t *testing.T) {
	for _, failure := range []string{"empty-path", "parent-file", "parent-symlink", "unreadable-parent"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			parent := filepath.Join(dir, "locks")
			path := filepath.Join(parent, "lock")
			opts := FileLockClearOptions{Path: path, Force: true}
			switch failure {
			case "empty-path":
				opts.Path = ""
			case "parent-file":
				if err := os.WriteFile(parent, []byte("concurrent"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				if err := os.Symlink(t.TempDir(), parent); err != nil {
					t.Fatal(err)
				}
			case "unreadable-parent":
				if err := os.Mkdir(parent, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(parent, 0o700) }()
				if file, err := os.Open(parent); err == nil {
					_ = file.Close()
					t.Skip("directory permissions are not enforced")
				}
			}
			result, err := ClearFileLock(opts)
			if err == nil || result.Cleared {
				t.Fatalf("unsafe clear accepted: %+v %v", result, err)
			}
			if failure == "parent-file" {
				if body, err := os.ReadFile(parent); err != nil || string(body) != "concurrent" {
					t.Fatalf("parent changed: %q %v", body, err)
				}
			}
		})
	}
}
