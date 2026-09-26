// SPDX-License-Identifier: Apache-2.0

//go:build linux

package restore

import (
	"os"

	"golang.org/x/sys/unix"
)

func openRestoreIdentityPin(root *os.Root, name string) (*os.File, error) {
	// O_PATH never opens a FIFO or device for I/O, and O_NOFOLLOW opens a final
	// symlink's inode rather than its target. The returned descriptor therefore
	// pins any entry type without blocking or following the final component.
	return root.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
}

func openRestoreRegularPin(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
