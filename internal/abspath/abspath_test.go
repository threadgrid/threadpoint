// SPDX-License-Identifier: Apache-2.0

package abspath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAbsResolvesAgainstWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	got, err := Abs("nested/../selected")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(wd, "selected"); got != want {
		t.Fatalf("Abs(relative) = %q, want %q", got, want)
	}
	absolute := filepath.Join(cwd, "a", "..", "b")
	if got, err := Abs(absolute); err != nil || got != filepath.Clean(absolute) {
		t.Fatalf("Abs(absolute) = %q, %v", got, err)
	}
}

func TestAbsRefusesRelativePathsAfterWorkingDirectoryRemoval(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}

	if got, err := Abs("relative"); err == nil {
		t.Fatalf("Abs resolved %q against a removed working directory", got)
	}
	absolute := filepath.Join(t.TempDir(), "kept")
	if got, err := Abs(absolute); err != nil || got != absolute {
		t.Fatalf("Abs(absolute) = %q, %v", got, err)
	}
}

func TestAbsRefusesStaleWorkingDirectoryReports(t *testing.T) {
	t.Chdir(t.TempDir())
	removed := filepath.Join(t.TempDir(), "removed")
	elsewhere := t.TempDir()
	for name, reported := range map[string]string{
		"path no longer exists":        removed,
		"path names another directory": elsewhere,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolve("relative", func() (string, error) { return reported, nil })
			if err == nil {
				t.Fatalf("resolved %q through stale working directory %q", got, reported)
			}
		})
	}
}

func TestAbsRefusesUninspectableWorkingDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory search permission")
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Chmod(cwd, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cwd, 0o700) })

	if got, err := resolve("relative", func() (string, error) { return cwd, nil }); err == nil {
		t.Fatalf("resolved %q in an uninspectable working directory", got)
	}
}
