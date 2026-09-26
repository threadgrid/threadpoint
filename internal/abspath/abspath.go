// SPDX-License-Identifier: Apache-2.0

// Package abspath resolves paths against the process working directory and
// fails closed when that directory no longer exists. Some platforms keep
// reporting the former path of a deleted working directory, which would let a
// relative selection bind to a location that has vanished.
package abspath

import (
	"fmt"
	"os"
	"path/filepath"
)

// Abs returns the clean absolute form of path, like filepath.Abs. A relative
// path resolves only while the reported working directory still names the
// current directory.
func Abs(path string) (string, error) {
	return resolve(path, os.Getwd)
}

// resolve takes the working-directory lookup as a parameter so the stale
// reports some platforms produce can be exercised everywhere.
func resolve(path string, getwd func() (string, error)) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	cwd, err := getwd()
	if err != nil {
		return "", err
	}
	current, err := os.Stat(".")
	if err != nil {
		return "", fmt.Errorf("inspect working directory: %w", err)
	}
	named, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("working directory %s is unavailable: %w", cwd, err)
	}
	if !os.SameFile(current, named) {
		return "", fmt.Errorf("working directory %s no longer names the current directory", cwd)
	}
	return filepath.Join(cwd, path), nil
}
