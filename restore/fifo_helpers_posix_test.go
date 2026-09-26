// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package restore

import (
	"syscall"
)

func makeRestoreFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
