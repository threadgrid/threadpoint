// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package safefs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestAtomicWriteFileUsingUmask(t *testing.T) {
	if os.Getenv(umaskHelperEnv) == "1" {
		runAtomicWriteFileUsingUmaskHelper(t)
		return
	}

	tests := []struct {
		name     string
		kind     string
		umask    int
		fileMode os.FileMode
		dirMode  os.FileMode
	}{
		{
			name:     "replacement creates files and parents with group-readable umask",
			kind:     "replace",
			umask:    0o002,
			fileMode: 0o664,
			dirMode:  0o775,
		},
		{
			name:     "missing-only creation honors owner-only umask",
			kind:     "missing",
			umask:    0o077,
			fileMode: 0o600,
			dirMode:  0o700,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			target := runAtomicWriteFileUsingUmaskSubprocess(t, root, test.kind, test.umask)
			assertMode(t, target, test.fileMode)
			assertMode(t, filepath.Dir(target), test.dirMode)
		})
	}

	t.Run("replacement preserves an existing target mode", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "nested", "agent.md")
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o600); err != nil {
			t.Fatal(err)
		}
		runAtomicWriteFileUsingUmaskSubprocess(t, root, "replace", 0o002)
		assertMode(t, target, 0o600)
	})
}

const (
	umaskHelperEnv      = "THREADPOINT_SAFEFS_UMASK_HELPER"
	umaskHelperRootEnv  = "THREADPOINT_SAFEFS_UMASK_ROOT"
	umaskHelperKindEnv  = "THREADPOINT_SAFEFS_UMASK_KIND"
	umaskHelperValueEnv = "THREADPOINT_SAFEFS_UMASK_VALUE"
)

func runAtomicWriteFileUsingUmaskHelper(t *testing.T) {
	t.Helper()
	value, err := strconv.ParseInt(os.Getenv(umaskHelperValueEnv), 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	previous := syscall.Umask(int(value))
	defer syscall.Umask(previous)

	target := filepath.Join(os.Getenv(umaskHelperRootEnv), "nested", "agent.md")
	switch os.Getenv(umaskHelperKindEnv) {
	case "replace":
		err = AtomicWriteFileUsingUmask(target, []byte("new\n"), 0o666, 0o777)
	case "missing":
		err = AtomicWriteFileIfMissingUsingUmask(target, []byte("new\n"), 0o666, 0o777)
	default:
		err = fmt.Errorf("unknown helper write kind %q", os.Getenv(umaskHelperKindEnv))
	}
	if err != nil {
		t.Fatal(err)
	}
}

func runAtomicWriteFileUsingUmaskSubprocess(t *testing.T, root, kind string, value int) string {
	t.Helper()
	// #nosec G702 -- the test executes its own fixed binary with a fixed test selector.
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAtomicWriteFileUsingUmask$")
	command.Env = append(os.Environ(),
		umaskHelperEnv+"=1",
		umaskHelperRootEnv+"="+root,
		umaskHelperKindEnv+"="+kind,
		umaskHelperValueEnv+"="+strconv.FormatInt(int64(value), 8),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("umask helper failed: %v\n%s", err, output)
	}
	return filepath.Join(root, "nested", "agent.md")
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
