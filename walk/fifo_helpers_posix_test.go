// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package walk

import (
	"syscall"
)

func makeTraversalTestFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
