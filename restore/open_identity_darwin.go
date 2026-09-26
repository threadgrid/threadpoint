// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package restore

import (
	"os"

	"golang.org/x/sys/unix"
)

func openRestoreIdentityPin(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	flags := os.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW
	if info.Mode()&os.ModeSymlink != 0 {
		// os.Root adds O_NOFOLLOW internally. Darwin's rooted open rejects that
		// flag combination for dangling symlinks even with O_SYMLINK, so callers
		// retain rooted Lstat snapshots without an entry handle for symlinks.
		return nil, nil
	}
	return root.OpenFile(name, flags, 0)
}

func openRestoreRegularPin(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
