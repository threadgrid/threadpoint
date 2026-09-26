// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package walk

import (
	"os"
	"syscall"
)

func openSmallRegularFile(path string) (*os.File, error) {
	// O_NONBLOCK prevents a raced FIFO replacement from hanging, while
	// O_NOFOLLOW refuses a raced final-component symlink replacement.
	// #nosec G304 -- the caller bounds and verifies the walked project file.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
