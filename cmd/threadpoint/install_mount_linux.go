//go:build linux

// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func installMountIdentity(file *os.File) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(int(file.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		return 0, errors.New("install path mount identity is unavailable")
	}
	return stat.Mnt_id, nil
}
