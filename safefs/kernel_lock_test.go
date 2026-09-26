// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestKernelFileLockSerializesAndReleases(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	first, err := AcquireKernelFileLockRoot(context.Background(), root, "install.guard", filepath.Join(rootPath, "install.guard"), time.Second, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if second, err := AcquireKernelFileLockRoot(ctx, root, "install.guard", filepath.Join(rootPath, "install.guard"), time.Second, time.Millisecond); err == nil {
		_ = second.Release()
		t.Fatal("a second kernel lock entered while the first was held")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquisition error = %v, want context deadline", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireKernelFileLockRoot(context.Background(), root, "install.guard", filepath.Join(rootPath, "install.guard"), time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("kernel lock remained held after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestKernelFileLockRejectsSymlinkAndFIFOWithoutOpeningThem(t *testing.T) {
	tests := []struct {
		name  string
		setup func(string) error
	}{
		{
			name: "symlink",
			setup: func(path string) error {
				return os.Symlink(filepath.Join(filepath.Dir(path), "outside"), path)
			},
		},
		{
			name: "fifo",
			setup: func(path string) error {
				return unix.Mkfifo(path, 0o600)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			path := filepath.Join(rootPath, "install.guard")
			if err := test.setup(path); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			started := time.Now()
			if lock, err := AcquireKernelFileLockRoot(context.Background(), root, "install.guard", path, time.Second, time.Millisecond); err == nil {
				_ = lock.Release()
				t.Fatalf("kernel guard accepted a %s", test.name)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("kernel guard blocked on a %s for %s", test.name, elapsed)
			}
		})
	}
}

func TestKernelLockRefusesInvalidAuthorityAndReleasesAfterIdentityLoss(t *testing.T) {
	for _, change := range []string{"nil-root", "closed-root", "nested-name", "empty-name", "removed", "replaced", "public-mode", "closed-file"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			name := "guard"
			switch change {
			case "nil-root":
				root = nil
			case "closed-root":
				_ = root.Close()
			case "nested-name":
				name = "nested/guard"
			case "empty-name":
				name = ""
			}
			//nolint:staticcheck // Verify the documented nil-context default.
			lock, err := AcquireKernelFileLockRoot(nil, root, name, filepath.Join(dir, name), 0, 0)
			if change == "nil-root" || change == "closed-root" || change == "nested-name" || change == "empty-name" {
				if err == nil || lock != nil {
					t.Fatalf("invalid acquisition: %v %v", lock, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "removed":
				err = root.Remove(name)
			case "replaced":
				if err = root.Rename(name, "retained"); err == nil {
					err = root.WriteFile(name, []byte("concurrent"), 0o600)
				}
			case "public-mode":
				err = root.Chmod(name, 0o644)
			case "closed-file":
				err = lock.file.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := lock.Validate(); err == nil {
				t.Fatal("lost identity accepted")
			}
			if err := lock.Release(); err == nil {
				t.Fatal("lost identity hidden by release")
			}
			if err := lock.Release(); err != nil {
				t.Fatalf("release not idempotent: %v", err)
			}
			if change == "replaced" {
				body, err := root.ReadFile(name)
				if err != nil || string(body) != "concurrent" {
					t.Fatalf("release changed concurrent guard: %q %v", body, err)
				}
			}
			next, err := AcquireKernelFileLockRoot(context.Background(), root, name, filepath.Join(dir, name), 0, 0)
			if err != nil {
				t.Fatalf("kernel ownership not released: %v", err)
			}
			if err := next.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKernelLockWaitHonorsCancellation(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	holder, err := AcquireKernelFileLockRoot(context.Background(), root, "guard", "guard", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if lock, err := AcquireKernelFileLockRoot(ctx, root, "guard", "guard", time.Second, time.Millisecond); lock != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter=%v %v", lock, err)
	}
	if err := holder.Validate(); err != nil {
		t.Fatalf("waiter disturbed holder: %v", err)
	}
}
