//go:build darwin

// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func installMountIdentity(file *os.File) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(uint32(stat.Fsid.Val[0]))<<32 | uint64(uint32(stat.Fsid.Val[1])), nil
}
