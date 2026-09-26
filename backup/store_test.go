// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreUsesUserThreadpointBackupRoot(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	now := time.Date(2026, 5, 30, 1, 2, 3, 0, time.UTC)
	source := filepath.Join(root, "CLAUDE.md")
	mustWriteFile(t, source, "claude")

	store, err := NewStore(StoreOptions{
		ProjectRoot: root,
		HomeDir:     home,
		Operation:   "fixture",
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if store.Manifest.Version != ManifestVersion {
		t.Fatalf("new backup manifest version = %d, want %d", store.Manifest.Version, ManifestVersion)
	}
	entry, err := store.BackupFile("CLAUDE.md", source, "stage-source")
	if err != nil {
		t.Fatal(err)
	}
	physicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(entry.BackupPath, filepath.Join(physicalHome, ".threadpoint", "backups")) {
		t.Fatalf("expected threadpoint backup path, got %s", entry.BackupPath)
	}
	body, err := os.ReadFile(entry.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "claude" {
		t.Fatalf("expected backup content, got %q", body)
	}

	runs, err := ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != DefaultRunID("fixture", now) {
		t.Fatalf("expected one fixture run, got %#v", runs)
	}
	if runs[0].Version != ManifestVersion {
		t.Fatalf("listed backup run version = %d, want %d", runs[0].Version, ManifestVersion)
	}
}

func TestBorrowedStoreKeepsRetainedHomeForGenericOperations(t *testing.T) {
	project := t.TempDir()
	parent := t.TempDir()
	productHome := filepath.Join(parent, "product-home")
	if err := os.Mkdir(productHome, 0o700); err != nil {
		t.Fatal(err)
	}
	pinned, err := os.OpenRoot(productHome)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	store, err := NewStoreFromRoot(pinned, productHome, StoreOptions{ProjectRoot: project, ThreadpointHome: productHome, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	moved := productHome + ".moved"
	if err := os.Rename(productHome, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(productHome, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(source, []byte("reviewed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := store.BackupFile("CLAUDE.md", source, "stage-source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil {
		t.Fatalf("generic manifest load through retained home: %v", err)
	}
	rel, err := filepath.Rel(productHome, entry.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(moved, rel))
	if err != nil || string(body) != "reviewed\n" {
		t.Fatalf("retained backup payload = %q, err=%v", body, err)
	}
	replacementRoot, err := os.OpenRoot(productHome)
	if err != nil {
		t.Fatal(err)
	}
	defer replacementRoot.Close()
	if _, err := store.RecordAbsentFromRoot(replacementRoot, productHome, "AGENTS.md", "canonical-prior"); err == nil || !strings.Contains(err.Error(), "retained authority") {
		t.Fatalf("borrowed store accepted a replacement product root: %v", err)
	}
	if entries, err := os.ReadDir(productHome); err != nil || len(entries) != 0 {
		t.Fatalf("replacement product home was mutated: entries=%v err=%v", entries, err)
	}
}

func TestResolveBackupRootRejectsTraversal(t *testing.T) {
	t.Setenv("THREADPOINT_HOME", "")
	home := t.TempDir()
	if _, err := ResolveBackupRoot(home, "../../outside"); err == nil {
		t.Fatal("expected traversal backup directory to be rejected")
	}

	root, err := ResolveBackupRoot(home, filepath.ToSlash(filepath.Join("cli", "run")))
	if err != nil {
		t.Fatal(err)
	}
	physicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(physicalHome, ".threadpoint", "backups", "cli", "run")
	if root != want {
		t.Fatalf("expected backup root %q, got %q", want, root)
	}

	root, err = ResolveBackupRoot(home, ".")
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(home, ".threadpoint", "backups")
	if root != want {
		t.Fatalf("expected default backup root %q, got %q", want, root)
	}
}

func TestResolveBackupRootUsesThreadpointHome(t *testing.T) {
	home := t.TempDir()
	threadpointHome := t.TempDir()
	t.Setenv("THREADPOINT_HOME", threadpointHome)

	root, err := ResolveBackupRoot(home, "cli")
	if err != nil {
		t.Fatal(err)
	}
	physicalThreadpointHome, err := filepath.EvalSymlinks(threadpointHome)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(physicalThreadpointHome, "backups", "cli")
	if root != want {
		t.Fatalf("expected backup root %q, got %q", want, root)
	}
}

func TestResolveBackupRootPrefersExplicitThreadpointHome(t *testing.T) {
	home := t.TempDir()
	envThreadpointHome := filepath.Join(t.TempDir(), "env-home")
	explicitThreadpointHome := filepath.Join(t.TempDir(), "explicit-home")
	t.Setenv("THREADPOINT_HOME", envThreadpointHome)

	root, err := ResolveBackupRootWithOverride(home, explicitThreadpointHome, "cli")
	if err != nil {
		t.Fatal(err)
	}
	physicalExplicitParent, err := filepath.EvalSymlinks(filepath.Dir(explicitThreadpointHome))
	if err != nil {
		t.Fatal(err)
	}
	physicalExplicitHome := filepath.Join(physicalExplicitParent, filepath.Base(explicitThreadpointHome))
	want := filepath.Join(physicalExplicitHome, "backups", "cli")
	if root != want {
		t.Fatalf("expected explicit backup root %q, got %q", want, root)
	}

	store, err := NewStore(StoreOptions{
		ProjectRoot:     t.TempDir(),
		HomeDir:         home,
		ThreadpointHome: explicitThreadpointHome,
		Operation:       "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if !strings.HasPrefix(store.BackupRoot, filepath.Join(physicalExplicitHome, "backups")) {
		t.Fatalf("expected store under explicit home, got %s", store.BackupRoot)
	}
}

func TestBackupMutationAllowsTrustedConfiguredHomeAncestorSymlink(t *testing.T) {
	base := t.TempDir()
	physical := filepath.Join(base, "physical")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(base, "logical")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	configured := filepath.Join(logical, "threadpoint-home")
	store, err := NewStore(StoreOptions{
		ProjectRoot:     t.TempDir(),
		HomeDir:         t.TempDir(),
		ThreadpointHome: configured,
		Operation:       "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupBytes("AGENTS.md", []byte("guide"), "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(entry.BackupPath, filepath.Join(physical, "threadpoint-home")) {
		t.Fatalf("backup path %q did not use physical configured boundary", entry.BackupPath)
	}
}

func TestBackupStateUsesOwnerOnlyModes(t *testing.T) {
	store, err := NewStore(StoreOptions{ProjectRoot: t.TempDir(), HomeDir: t.TempDir(), Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupBytes("AGENTS.md", []byte("private memory"), "stage-source")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	assertMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s has mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
	assertMode(store.BackupRoot, 0o700)
	assertMode(filepath.Join(store.BackupRoot, store.ProjectID), 0o700)
	assertMode(store.RunDir, 0o700)
	assertMode(filepath.Join(store.RunDir, ManifestName), 0o600)
	assertMode(entry.BackupPath, 0o600)
}

func TestNewStoreRejectsUnsafeRunID(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	for _, runID := range []string{
		".",
		"..",
		filepath.Join("nested", "run"),
		"../outside",
		filepath.Join(string(filepath.Separator), "absolute"),
	} {
		t.Run(strings.ReplaceAll(runID, string(filepath.Separator), "_"), func(t *testing.T) {
			if _, err := NewStore(StoreOptions{
				ProjectRoot: root,
				HomeDir:     home,
				Operation:   "fixture",
				RunID:       runID,
			}); err == nil {
				t.Fatalf("expected unsafe run id %q to be rejected", runID)
			}
		})
	}
}

func TestResolveThreadpointHomeUsesConfiguredHome(t *testing.T) {
	home := t.TempDir()

	if got, err := ResolveThreadpointHome(home); err != nil || got != filepath.Join(home, ".threadpoint") {
		t.Fatalf("resolved home = %q, %v", got, err)
	}
}

func TestRootedStoreValidationRefusesLostOrChangedNamespace(t *testing.T) {
	for _, create := range []bool{false, true} {
		for _, failure := range []string{"nil-store", "nil-root", "closed-root", "wrong-home", "wrong-run", "outside-backup", "symlink-backups", "blocked-backups", "removed-cwd"} {
			if failure == "blocked-backups" && !create {
				continue
			}
			t.Run(fmt.Sprintf("create=%v/%s", create, failure), func(t *testing.T) {
				home, project := t.TempDir(), t.TempDir()
				root, err := os.OpenRoot(home)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				store, err := NewStoreFromRoot(root, home, StoreOptions{ProjectRoot: project, ThreadpointHome: home, Operation: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.validateRunDirRoot(root, home, false); err != nil {
					t.Fatalf("valid namespace rejected: %v", err)
				}
				selected := root
				switch failure {
				case "nil-store":
					store = nil
				case "nil-root":
					selected = nil
				case "closed-root":
					_ = root.Close()
				case "wrong-home":
					home = t.TempDir()
				case "wrong-run":
					store.RunDir = filepath.Join(home, "unrelated", store.RunID)
				case "outside-backup":
					store.BackupRoot = t.TempDir()
					store.RunDir = filepath.Join(store.BackupRoot, store.ProjectID, store.RunID)
				case "symlink-backups":
					if err := root.Symlink(t.TempDir(), "backups"); err != nil {
						t.Fatal(err)
					}
				case "blocked-backups":
					if err := root.WriteFile("backups", []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "removed-cwd":
					cwd := t.TempDir()
					t.Chdir(cwd)
					if err := os.Remove(cwd); err != nil {
						t.Fatal(err)
					}
					home = "relative"
				}
				err = store.validateRunDirRoot(selected, home, create)
				if err == nil {
					t.Fatal("invalid rooted store namespace accepted")
				}
				if failure == "blocked-backups" {
					body, err := root.ReadFile("backups")
					if err != nil || string(body) != "foreign" {
						t.Fatalf("foreign ancestor changed: %q %v", body, err)
					}
				}
			})
		}
	}
}

func TestBackupAPIsRefuseUnresolvableRelativeSelections(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"store-project", func() error { _, err := NewStore(StoreOptions{ProjectRoot: "relative", HomeDir: home}); return err }},
		{"store-home", func() error { _, err := NewStore(StoreOptions{ProjectRoot: project, HomeDir: "relative"}); return err }},
		{"retained-home", func() error {
			_, err := NewStoreFromRoot(root, "relative", StoreOptions{ProjectRoot: project, HomeDir: home})
			return err
		}},
		{"list", func() error { _, err := ListRunsWithOverride("relative", home, "", ""); return err }},
		{"product-relative", func() error {
			_, err := ProductRootRelativePath("relative", filepath.Join(home, "manifest.json"))
			return err
		}},
		{"target-relative", func() error { _, err := ProductRootRelativePath(home, "relative"); return err }},
		{"latest-retained", func() error { _, err := LatestRunFromRoot("relative", root, home, ""); return err }},
		{"find-retained", func() error { _, err := FindRunFromRoot("relative", root, home, "", "run-fixture"); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("unresolvable backup selection accepted")
			}
		})
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid selection wrote backup state: %v %v", entries, err)
	}
}
