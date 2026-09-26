// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package backup

import (
	"os"
	"syscall"
)

func openBoundedRegularRootFile(root *os.Root, name string) (*os.File, error) {
	// O_NONBLOCK prevents a raced special-file replacement from hanging, while
	// O_NOFOLLOW refuses a raced final-component symlink replacement.
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
