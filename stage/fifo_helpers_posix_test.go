// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package stage

import (
	"syscall"
)

func makeStageFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
