// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package safefs

import (
	"os"
	"syscall"
)

func openAtomicCopySource(path string) (*os.File, error) {
	// O_NONBLOCK prevents a raced special-file replacement from hanging, while
	// O_NOFOLLOW refuses a raced final-component symlink replacement.
	// #nosec G304 -- path is the caller-selected source and is opened no-follow
	// and nonblocking before its descriptor identity is compared with Lstat.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
