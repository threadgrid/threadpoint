// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// KernelFileLock is a crash-safe advisory lock on a stable, owner-private
// coordination file. The file is intentionally retained after release: all
// cooperating processes must flock the same inode, including while a stale
// pathname lock is reviewed, reclaimed, or released.
type KernelFileLock struct {
	file     *os.File
	root     *os.Root
	name     string
	path     string
	info     fs.FileInfo
	released bool
}

// AcquireKernelFileLockRoot opens and exclusively flocks one stable file below
// an already-pinned root. Kernel ownership disappears automatically when the
// last inherited file description closes, so a crashed owner cannot leave a
// transition guard that itself needs a racy stale-reclaim protocol.
func AcquireKernelFileLockRoot(ctx context.Context, root *os.Root, name, path string, waitTimeout, pollInterval time.Duration) (*KernelFileLock, error) {
	if root == nil {
		return nil, errors.New("kernel file lock root is required")
	}
	base, err := cleanPinnedRenameBase(name)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if pollInterval <= 0 {
		pollInterval = defaultFileLockPoll
	}
	// O_NOFOLLOW rejects a raced final-component symlink, and O_NONBLOCK keeps
	// an accidental FIFO from hanging before its type can be validated.
	file, err := root.OpenFile(base, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*KernelFileLock, error) {
		return nil, errors.Join(cause, file.Close())
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(err)
	}
	info, err := validateRootedKernelLockFile(root, base, file, nil)
	if err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}

	deadline := time.Now().Add(waitTimeout)
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fail(err)
		}
		if waitTimeout <= 0 || !time.Now().Before(deadline) {
			return fail(FileLockError{Reason: FileLockErrorHeld, Path: path, Message: "kernel file lock is already held"})
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fail(ctx.Err())
		case <-timer.C:
		}
	}
	if _, err := validateRootedKernelLockFile(root, base, file, info); err != nil {
		unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return fail(errors.Join(err, unlockErr))
	}
	return &KernelFileLock{file: file, root: root, name: base, path: path, info: info}, nil
}

func validateRootedKernelLockFile(root *os.Root, name string, file *os.File, expected fs.FileInfo) (fs.FileInfo, error) {
	opened, openErr := file.Stat()
	current, currentErr := root.Lstat(name)
	if openErr != nil || currentErr != nil || !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(opened, current) || opened.Mode().Perm()&0o077 != 0 || current.Mode().Perm()&0o077 != 0 ||
		(expected != nil && (!os.SameFile(expected, opened) || !os.SameFile(expected, current))) {
		return nil, errors.Join(errors.New("kernel file lock identity changed"), openErr, currentErr)
	}
	return opened, nil
}

// Validate confirms that the stable coordination pathname still identifies
// the exact flocked inode retained by this capability.
func (lock *KernelFileLock) Validate() error {
	if lock == nil || lock.file == nil || lock.root == nil || lock.info == nil || lock.released {
		return errors.New("kernel file lock is not held")
	}
	_, err := validateRootedKernelLockFile(lock.root, lock.name, lock.file, lock.info)
	return err
}

// Release drops kernel ownership and closes the retained file description. The
// stable guard file remains so waiters never split across different inodes.
func (lock *KernelFileLock) Release() error {
	if lock == nil || lock.released {
		return nil
	}
	validationErr := lock.Validate()
	lock.released = true
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(validationErr, unlockErr, closeErr)
}
