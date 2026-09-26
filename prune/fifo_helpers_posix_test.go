// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package prune

import (
	"syscall"
)

func makePruneFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
