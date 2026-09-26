//go:build !linux && !darwin

// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
)

func installMountIdentity(_ *os.File) (uint64, error) {
	return 0, errors.New("install path mount-boundary validation is unsupported on this platform")
}
