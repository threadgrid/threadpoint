// SPDX-License-Identifier: Apache-2.0

//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package safefs

import (
	"os"
	"syscall"
)

// openReviewedLockFile opens a lock file beneath a pinned root with no-follow
// and nonblocking semantics, so a raced final-component replacement of the lock
// with a symlink or FIFO cannot redirect the read or block lock review. The
// caller validates the opened descriptor's identity before trusting it.
func openReviewedLockFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
