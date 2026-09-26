// SPDX-License-Identifier: Apache-2.0

// Package home resolves the threadpoint home directory (~/.threadpoint, or
// $THREADPOINT_HOME when set) used for locks, backups, and update caches.
package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve returns the absolute threadpoint home directory. It honors
// $THREADPOINT_HOME when set, otherwise uses homeDir (or the current user's home
// directory when homeDir is empty) joined with ".threadpoint".
func Resolve(homeDir string) (string, error) {
	return ResolveWithOverride(homeDir, "")
}

// ResolveWithOverride returns the absolute threadpoint home directory. It
// prefers a non-empty explicit threadpointHome value, then non-empty
// $THREADPOINT_HOME, then homeDir/.threadpoint. Product home values must already
// be absolute paths and must not target unsafe broad filesystem roots.
func ResolveWithOverride(homeDir string, threadpointHome string) (string, error) {
	home, err := ResolveUserHome(homeDir)
	if err != nil {
		return "", err
	}
	if explicit := strings.TrimSpace(threadpointHome); explicit != "" {
		return validateThreadpointHome(home, explicit)
	}
	if env := strings.TrimSpace(os.Getenv("THREADPOINT_HOME")); env != "" {
		return validateThreadpointHome(home, env)
	}
	return validateThreadpointHome(home, filepath.Join(home, ".threadpoint"))
}

// ResolveUserHome returns a cleaned, absolute home directory, using the current
// user's home when homeDir is empty.
func ResolveUserHome(homeDir string) (string, error) {
	if homeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		homeDir = home
	}
	abs, err := filepath.Abs(homeDir)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func validateThreadpointHome(userHome string, threadpointHome string) (string, error) {
	if !filepath.IsAbs(threadpointHome) {
		return "", fmt.Errorf("threadpoint home must be absolute: %s", threadpointHome)
	}
	root, err := filepath.Abs(threadpointHome)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return "", errors.New("threadpoint home cannot be filesystem root")
	}
	home, err := filepath.Abs(userHome)
	if err != nil {
		return "", err
	}
	home = filepath.Clean(home)
	if root == home {
		return "", errors.New("threadpoint home cannot be the home directory")
	}
	if root == filepath.Join(home, ".agents") {
		return "", errors.New("threadpoint home cannot be the entire .agents directory")
	}
	if info, err := os.Lstat(root); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("threadpoint home cannot be a symlink: %s", root)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("threadpoint home is not a directory: %s", root)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect threadpoint home %s: %w", root, err)
	}
	resolved, err := resolveTrustedBoundary(root)
	if err != nil {
		return "", fmt.Errorf("resolve threadpoint home %s: %w", root, err)
	}
	resolvedUserHome, err := resolveTrustedBoundary(home)
	if err != nil {
		return "", fmt.Errorf("resolve user home %s: %w", home, err)
	}
	switch resolved {
	case string(filepath.Separator):
		return "", errors.New("threadpoint home cannot resolve to filesystem root")
	case resolvedUserHome:
		return "", errors.New("threadpoint home cannot resolve to the home directory")
	case filepath.Join(resolvedUserHome, ".agents"):
		return "", errors.New("threadpoint home cannot resolve to the entire .agents directory")
	}
	return resolved, nil
}

// resolveTrustedBoundary resolves symlinks in the caller-selected boundary's
// existing ancestors while preserving a not-yet-created suffix. Descendants of
// the returned physical boundary remain subject to the ordinary no-symlink
// state-write checks.
func resolveTrustedBoundary(root string) (string, error) {
	current := filepath.Clean(root)
	var suffix []string
	for {
		_, err := os.Lstat(current)
		switch {
		case err == nil:
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(root), nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
