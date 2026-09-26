// SPDX-License-Identifier: Apache-2.0

package layout

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

func TestValidateLayoutReportsMissingFiles(t *testing.T) {
	root := t.TempDir()

	report := Validate(root)
	if report.OK {
		t.Fatalf("expected invalid layout")
	}
	if len(report.Issues) != 4 {
		t.Fatalf("expected 4 issues, got %d", len(report.Issues))
	}
	if len(report.Warnings) != 2 {
		t.Fatalf("expected 2 optional directory warnings, got %d", len(report.Warnings))
	}
}

func TestValidateLayoutRejectsDirectoryAtRequiredFile(t *testing.T) {
	root := t.TempDir()
	if err := EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(root, "AGENTS.md")
	if err := os.Remove(agents); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	report := Validate(root)
	if report.OK {
		t.Fatal("expected Validate to fail when a required file is a directory")
	}
	found := false
	for _, issue := range report.Issues {
		if issue.Path == "AGENTS.md" && strings.Contains(issue.Message, "not a regular file") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a not-a-regular-file issue for AGENTS.md, got %+v", report.Issues)
	}
}

func TestEnsureProjectSkillStateLayoutRejectsStateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "state")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := EnsureProjectSkillState(root)
	if !errors.Is(err, safefs.ErrUnsafeSymlink) {
		t.Fatalf("expected unsafe state symlink refusal, got %v", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("outside symlink target was mutated: %#v", entries)
	}
}

func TestEnsureSharedLayoutStillRejectsProjectControlledAgentsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".agents")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	err := EnsureShared(root)
	if !errors.Is(err, safefs.ErrUnsafeSymlink) {
		t.Fatalf("expected project-controlled .agents symlink refusal, got %v", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("outside symlink target was mutated: %#v", entries)
	}
}

func TestValidateLayoutAllowsMissingFeatureManifests(t *testing.T) {
	root := t.TempDir()
	if err := EnsureShared(root); err != nil {
		t.Fatal(err)
	}

	report := Validate(root)
	if !report.OK {
		t.Fatalf("expected valid layout without feature manifests, got %#v", report)
	}
}

func TestValidateLayoutTreatsSkillsAndPluginsAsOptional(t *testing.T) {
	root := t.TempDir()
	if err := EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		filepath.Join(".agents", "skills"),
		filepath.Join(".agents", "plugins"),
	} {
		if err := os.RemoveAll(filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}

	report := Validate(root)
	if !report.OK {
		t.Fatalf("expected valid layout without optional skills/plugins, got %#v", report)
	}
	if len(report.Warnings) != 2 {
		t.Fatalf("expected optional directory warnings, got %#v", report.Warnings)
	}
}

func TestValidateLayoutDoesNotInspectBackupState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("THREADPOINT_HOME", t.TempDir())
	if err := EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "AGENTS.md")
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root,
		Operation:   "fixture",
		Now: func() time.Time {
			return time.Now().Add(-31 * 24 * time.Hour)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("AGENTS.md", source, "fixture-target"); err != nil {
		t.Fatal(err)
	}

	report := Validate(root)
	if len(report.Warnings) != 0 {
		t.Fatalf("structural validation inspected backup state: %#v", report.Warnings)
	}
}

func TestValidatedProjectLocalLayoutChecksLockGenerationAtEachBoundary(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	validations := 0
	err = EnsureProjectLocalInRootValidated(root, func() error {
		validations++
		if validations == 3 {
			return errors.New("selected generation changed")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "selected generation changed") {
		t.Fatalf("validated layout error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "AGENTS.local.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("layout continued to a later file commit after validation failed: %v", err)
	}
}

func TestBootstrapStopsAtEveryRejectedMutationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*os.Root, func() error) error
	}{
		{"project bootstrap", EnsureProjectBootstrapInRootValidated},
		{"project local", EnsureProjectLocalInRootValidated},
		{"shared directories", EnsureSharedDirectoriesInRootValidated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sentinel := errors.New("ownership lost")
			completed := false
			for stop := 1; stop < 100; stop++ {
				dir := t.TempDir()
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				err = tc.run(root, func() error {
					calls++
					if calls == stop {
						return sentinel
					}
					return nil
				})
				if closeErr := root.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				if calls < stop {
					if err != nil {
						t.Fatal(err)
					}
					completed = true
					break
				}
				if !errors.Is(err, sentinel) || calls != stop {
					t.Fatalf("boundary %d: calls=%d err=%v", stop, calls, err)
				}
			}
			if !completed {
				t.Fatal("bootstrap never completed without a rejected boundary")
			}
		})
	}
}

func TestIsProjectLocalPath(t *testing.T) {
	for _, tc := range []struct {
		path  string
		local bool
	}{
		{"AGENTS.local.md", true}, {".agents.local", true}, {"./.agents.local/knowledge/a.md", true},
		{".agents.localish/a.md", false}, {".agents/knowledge/a.md", false}, {"nested/AGENTS.local.md", false},
	} {
		if got := IsProjectLocalPath(tc.path); got != tc.local {
			t.Errorf("%q: %v, want %v", tc.path, got, tc.local)
		}
	}
}
