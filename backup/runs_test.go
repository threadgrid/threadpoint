// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStaleWarningsUseManifestCreatedAt(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	old := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	source := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, source, "guide")

	store, err := NewStore(StoreOptions{
		ProjectRoot: root,
		HomeDir:     home,
		Operation:   "fixture",
		Now:         func() time.Time { return old },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("AGENTS.md", source, "fixture-secondary"); err != nil {
		t.Fatal(err)
	}

	warnings := StaleWarnings(root, home, "", old.Add(31*24*time.Hour))
	if len(warnings) != 1 || !strings.Contains(warnings[0], "older than 30 days") {
		t.Fatalf("expected stale backup warning, got %#v", warnings)
	}
}

func TestListRunsReportsCorruptCanonicalManifest(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, source, "guide")

	good := time.Date(2026, 5, 30, 1, 2, 3, 0, time.UTC)
	bad := time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC)
	for _, now := range []time.Time{good, bad} {
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

		if _, err := store.BackupFile("AGENTS.md", source, "fixture-target"); err != nil {
			t.Fatal(err)
		}
	}

	badRunDir := filepath.Join(home, ".threadpoint", "backups", ProjectID(root), DefaultRunID("fixture", bad))
	if err := os.WriteFile(filepath.Join(badRunDir, ManifestName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	runs, err := ListRuns(root, home, "")
	if err == nil {
		t.Fatalf("corrupt canonical manifest was hidden by the good run: %#v", runs)
	}
}

func TestRunLookupFindsSavedBackup(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(source, []byte("guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if latest, err := LatestRun(root, home, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("latest run before any backup = %#v, %v; want not-exist", latest, err)
	}
	store, err := NewStore(StoreOptions{
		ProjectRoot: root, HomeDir: home, Operation: "fixture", RunID: "fixture-20260731", Now: func() time.Time { return time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("AGENTS.md", source, "fixture"); err != nil {
		t.Fatal(err)
	}
	latest, err := LatestRun(root, home, "")
	if err != nil || latest.RunID != store.RunID {
		t.Fatalf("latest run = %#v, %v", latest, err)
	}
	found, err := FindRun(root, home, "", store.RunID)
	if err != nil || found.RunID != store.RunID {
		t.Fatalf("found run = %#v, %v", found, err)
	}
	if _, err := FindRun(root, home, "", "missing"); !os.IsNotExist(err) {
		t.Fatalf("missing run error = %v", err)
	}
}

func TestListRunsRejectsManifestOutsideSelectedNamespace(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Manifest)
	}{
		{name: "project id", edit: func(manifest *Manifest) { manifest.ProjectID = "other-project" }},
		{name: "project root", edit: func(manifest *Manifest) { manifest.ProjectRoot = t.TempDir() }},
		{name: "run id", edit: func(manifest *Manifest) { manifest.RunID = "other-run" }},
		{name: "sibling payload", edit: func(manifest *Manifest) {
			manifest.Entries[0].BackupPath = filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(manifest.Entries[0].BackupPath))), "other-run", "files", "payload")
		}},
		{name: "absent entry with outside payload", edit: func(manifest *Manifest) {
			manifest.Entries[0].Absent = true
			manifest.Entries[0].BackupPath = filepath.Join(t.TempDir(), "payload")
		}},
		{name: "sibling recovery payload", edit: func(manifest *Manifest) {
			manifest.Entries[0].RecoveryPath = filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(manifest.Entries[0].BackupPath))), "other-run", "files", "recovery")
		}},
		{name: "outside recovery payload", edit: func(manifest *Manifest) {
			manifest.Entries[0].RecoveryPath = filepath.Join(t.TempDir(), "recovery")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture", RunID: "selected-run"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			if _, err := store.BackupBytesWithMode("AGENTS.md", []byte("payload"), "fixture", 0o600); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(store.RunDir, ManifestName)
			body, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest Manifest
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
			test.edit(&manifest)
			body, err = json.MarshalIndent(manifest, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, append(body, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			tampered, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}

			for _, rooted := range []bool{false, true} {
				var runs []Run
				if rooted {
					homePath := filepath.Join(home, ".threadpoint")
					homeRoot, openErr := os.OpenRoot(homePath)
					if openErr != nil {
						t.Fatal(openErr)
					}
					runs, err = ListRunsFromRoot(root, homeRoot, homePath, "")
					if closeErr := homeRoot.Close(); err == nil {
						err = closeErr
					}
				} else {
					runs, err = ListRuns(root, home, "")
				}
				if err == nil {
					t.Fatalf("rooted=%t hid namespace-confused canonical manifest: %#v", rooted, runs)
				}
			}
			after, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(tampered) {
				t.Fatal("rejected namespace-confused manifest was modified")
			}
		})
	}
}

func TestReadManifestFromRootRequiresPinnedHomeAndContainedPath(t *testing.T) {
	home := t.TempDir()
	if _, err := ReadManifestFromRoot(nil, home, filepath.Join(home, ManifestName)); err == nil {
		t.Fatal("manifest read without a pinned home root")
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	if _, err := ReadManifestFromRoot(homeRoot, home, filepath.Join(t.TempDir(), ManifestName)); err == nil {
		t.Fatal("manifest read outside the pinned home")
	}
	if _, err := ReadManifestFromRoot(homeRoot, home, filepath.Join(home, "missing", ManifestName)); err == nil {
		t.Fatal("missing manifest read succeeded")
	}
}

func TestRunEnumerationBudgetsFailClosed(t *testing.T) {
	type listFixture struct {
		root string
		home string
	}
	newFixture := func(t *testing.T, entryCounts ...int) listFixture {
		t.Helper()
		fixture := listFixture{root: t.TempDir(), home: t.TempDir()}
		for runIndex, entryCount := range entryCounts {
			store, err := NewStore(StoreOptions{
				ProjectRoot: fixture.root,
				HomeDir:     fixture.home,
				Operation:   "fixture",
				RunID:       "run-" + string(rune('a'+runIndex)),
				Now:         func() time.Time { return time.Unix(int64(runIndex+1), 0) },
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			for entryIndex := 0; entryIndex < entryCount; entryIndex++ {
				name := "entry-" + string(rune('a'+entryIndex))
				if _, err := store.BackupBytesWithMode(name, []byte(name), "fixture", 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return fixture
	}
	list := func(t *testing.T, fixture listFixture, rooted bool, limits runEnumerationLimits) error {
		t.Helper()
		if !rooted {
			_, err := listRunsWithOverrideLimits(fixture.root, fixture.home, "", "", limits)
			return err
		}
		homePath := filepath.Join(fixture.home, ".threadpoint")
		homeRoot, err := os.OpenRoot(homePath)
		if err != nil {
			return err
		}
		defer homeRoot.Close()
		_, err = listRunsFromRootLimits(fixture.root, homeRoot, homePath, "", limits)
		return err
	}

	for _, rooted := range []bool{false, true} {
		t.Run("non-positive limits", func(t *testing.T) {
			fixture := newFixture(t, 1)
			for _, limits := range []runEnumerationLimits{
				{MaxRuns: 0, MaxEntries: 1, MaxBytes: 1},
				{MaxRuns: 1, MaxEntries: -1, MaxBytes: 1},
				{MaxRuns: 1, MaxEntries: 1, MaxBytes: 0},
			} {
				if err := list(t, fixture, rooted, limits); err == nil || !strings.Contains(err.Error(), "must all be positive") {
					t.Fatalf("rooted=%t limits=%+v error = %v", rooted, limits, err)
				}
			}
		})
		t.Run("run count", func(t *testing.T) {
			fixture := newFixture(t, 1, 1)
			err := list(t, fixture, rooted, runEnumerationLimits{MaxRuns: 1, MaxEntries: 10, MaxBytes: maxManifestBytes * 2})
			if err == nil || !strings.Contains(err.Error(), "run enumeration limit") {
				t.Fatalf("rooted=%t run-budget error = %v", rooted, err)
			}
		})
		t.Run("entry count", func(t *testing.T) {
			fixture := newFixture(t, 2)
			err := list(t, fixture, rooted, runEnumerationLimits{MaxRuns: 2, MaxEntries: 1, MaxBytes: maxManifestBytes})
			if err == nil || !strings.Contains(err.Error(), "entry enumeration limit") {
				t.Fatalf("rooted=%t entry-budget error = %v", rooted, err)
			}
		})
		t.Run("manifest bytes", func(t *testing.T) {
			fixture := newFixture(t, 1)
			projectDir := filepath.Join(fixture.home, ".threadpoint", "backups", ProjectID(fixture.root))
			entries, err := os.ReadDir(projectDir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("run directory: entries=%d err=%v", len(entries), err)
			}
			body, err := os.ReadFile(filepath.Join(projectDir, entries[0].Name(), ManifestName))
			if err != nil {
				t.Fatal(err)
			}
			err = list(t, fixture, rooted, runEnumerationLimits{MaxRuns: 2, MaxEntries: 2, MaxBytes: int64(len(body) - 1)})
			if err == nil || !strings.Contains(err.Error(), "manifest byte limit") {
				t.Fatalf("rooted=%t byte-budget error = %v", rooted, err)
			}
		})
	}
}

func TestListRunsFailsVisiblyForCorruptCanonicalRun(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, source, "guide")

	old := time.Date(2026, 8, 1, 1, 2, 3, 0, time.UTC)
	newer := old.Add(time.Hour)
	for _, now := range []time.Time{old, newer} {
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

		if _, err := store.BackupFile("AGENTS.md", source, "fixture-target"); err != nil {
			t.Fatal(err)
		}
	}

	newerRun := DefaultRunID("fixture", newer)
	manifestPath := filepath.Join(home, ".threadpoint", "backups", ProjectID(root), newerRun, ManifestName)
	if err := os.WriteFile(manifestPath, []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if runs, err := ListRuns(root, home, ""); err == nil {
		t.Fatalf("corrupt canonical run was hidden by older state: %#v", runs)
	}
}

func TestListRunsRejectsNewestCanonicalZeroEntryManifestWithoutFallback(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, source, "guide")
	older := time.Date(2026, 8, 1, 1, 2, 3, 0, time.UTC)
	store, err := NewStore(StoreOptions{
		ProjectRoot: root, HomeDir: home, Operation: "fixture",
		Now: func() time.Time { return older },
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

	newer := older.Add(time.Hour)
	runID := DefaultRunID("fixture", newer)
	runDir := filepath.Join(home, ".threadpoint", "backups", ProjectID(root), runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(Manifest{
		Version: ManifestVersion, ProjectID: ProjectID(root), RunID: runID,
		Operation: "fixture", ProjectRoot: filepath.Clean(root), CreatedAt: newer,
		Entries: []Entry{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, ManifestName), body, 0o600); err != nil {
		t.Fatal(err)
	}

	if runs, err := ListRuns(root, home, ""); err == nil || !strings.Contains(err.Error(), "at least one entry") {
		t.Fatalf("zero-entry canonical run was hidden by older state: runs=%#v err=%v", runs, err)
	}
}

func TestListRunsIgnoresNoncanonicalDirectoryNoise(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, source, "guide")
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("AGENTS.md", source, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	noise := filepath.Join(store.BackupRoot, store.ProjectID, ".unrelated")
	if err := os.Mkdir(noise, 0o700); err != nil {
		t.Fatal(err)
	}

	runs, err := ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != store.RunID {
		t.Fatalf("noncanonical noise changed run selection: %#v", runs)
	}
}

func TestRecoveryCommitTransitionsStayInvisibleToRootedLatestRun(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = homeRoot.Close() }()

	for _, fixture := range []struct {
		runID string
		when  time.Time
	}{
		{runID: "ordinary-old", when: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{runID: "ordinary-new", when: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
	} {
		store, err := NewStoreFromRoot(homeRoot, home, StoreOptions{
			ProjectRoot: project, ThreadpointHome: home, Operation: "fixture", RunID: fixture.runID,
			Now: func() time.Time { return fixture.when },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}

		if _, err := store.BackupBytesWithMode("AGENTS.md", []byte(fixture.runID), "fixture", 0o600); err != nil {
			t.Fatal(err)
		}
	}

	latest, err := LatestRunFromRoot(project, homeRoot, home, "")
	if err != nil || latest.RunID != "ordinary-new" {
		t.Fatalf("rooted latest run = %#v, %v", latest, err)
	}
	found, err := FindRunFromRoot(project, homeRoot, home, "", "ordinary-old")
	if err != nil || found.RunID != "ordinary-old" {
		t.Fatalf("rooted selected run = %#v, %v", found, err)
	}
	if _, err := FindRunFromRoot(project, homeRoot, home, "", "missing-run"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing rooted selected run = %v", err)
	}
	if _, err := LatestRunFromRoot(filepath.Join(project, "missing-project"), homeRoot, home, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing rooted latest run = %v", err)
	}
	warnings := StaleWarningsFromRoot(project, homeRoot, home, "", time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC))
	if len(warnings) != 2 || !strings.Contains(warnings[0], "older than 30 days") {
		t.Fatalf("rooted stale warnings = %#v", warnings)
	}
	if warnings := StaleWarningsFromRoot(project, nil, home, "", time.Time{}); len(warnings) != 1 || !strings.Contains(warnings[0], "failed checking") {
		t.Fatalf("rooted stale-warning error = %#v", warnings)
	}
	if rel, err := ProductRootRelativePath(home, filepath.Join(home, "backups", "fixture")); err != nil || rel != filepath.Join("backups", "fixture") {
		t.Fatalf("product-root relative path = %q, %v", rel, err)
	}
	if _, err := ProductRootRelativePath(home, home); err == nil {
		t.Fatal("product-root relative path accepted the root itself")
	}

	source := []byte("native source\n")
	proposed := []byte("canonical publication\n")
	sourceHash := sha256.Sum256(source)
	proposedHash := sha256.Sum256(proposed)
	recovery, err := NewStoreFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home, Operation: PendingCommitOperation,
		RunID: "commit-rooted-recovery", Now: func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := recovery.ConfigurePendingCommitGeneration(
		"rooted-recovery", "native.md", hex.EncodeToString(sourceHash[:]),
		"AGENTS.md", hex.EncodeToString(proposedHash[:]), int64(len(proposed)),
	); err != nil {
		t.Fatal(err)
	}
	recovery = publishPendingFixture(t, recovery, source, 0o600, nil, os.FileMode(0), false)

	if err := recovery.FinalizePendingCommitRecoveryFromRoot(homeRoot, home); err != nil {
		t.Fatal(err)
	}
	// Repeating an already-completed transition is intentionally idempotent.
	if err := recovery.FinalizePendingCommitRecoveryFromRoot(homeRoot, home); err != nil {
		t.Fatal(err)
	}
	persisted, err := ReadManifestFromRoot(homeRoot, home, filepath.Join(recovery.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Operation != RecoveryCommitOperation || persisted.CommitTransaction == nil || persisted.CommitTransaction.State != CommitTransactionRecoveryCommitted {
		t.Fatalf("rooted recovery commit manifest = %#v", persisted)
	}
	latest, err = LatestRunFromRoot(project, homeRoot, home, "")
	if err != nil || latest.RunID != "ordinary-new" {
		t.Fatalf("recovery-only run became an ordinary latest run: %#v, %v", latest, err)
	}

	ambient := preparedCommitStore(t, project, home, "commit-ambient-recovery")
	if err := ambient.FinalizePendingCommitRecovery(); err != nil {
		t.Fatal(err)
	}
	if ambient.Manifest.Operation != RecoveryCommitOperation || ambient.Manifest.CommitTransaction.State != CommitTransactionRecoveryCommitted {
		t.Fatalf("ambient recovery commit manifest = %#v", ambient.Manifest)
	}
	aborted := preparedCommitStore(t, project, home, "commit-rooted-abort")
	if err := aborted.AbortPendingCommitFromRoot(homeRoot, home); err != nil {
		t.Fatal(err)
	}
	if aborted.Manifest.Operation != AbortedCommitOperation || aborted.Manifest.CommitTransaction.State != CommitTransactionAborted {
		t.Fatalf("rooted aborted commit manifest = %#v", aborted.Manifest)
	}
	persistedAbort, err := ReadManifestFromRoot(homeRoot, home, filepath.Join(aborted.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if persistedAbort.Operation != AbortedCommitOperation || persistedAbort.CommitTransaction == nil || persistedAbort.CommitTransaction.State != CommitTransactionAborted {
		t.Fatalf("persisted rooted aborted commit manifest = %#v", persistedAbort)
	}
}

func TestPinnedRunEnumerationRejectsDamagedCanonicalEntries(t *testing.T) {
	for _, failure := range []string{"nil-root", "closed-root", "run-file", "run-symlink", "run-unreadable", "missing-manifest", "directory-manifest", "oversized-manifest", "unreadable-manifest", "invalid-manifest"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			run := filepath.Join(dir, "run-fixture")
			if err := os.Mkdir(run, 0o700); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			selected := root
			manifest := filepath.Join(run, ManifestName)
			switch failure {
			case "nil-root":
				selected = nil
			case "closed-root":
				_ = root.Close()
			case "run-file", "run-symlink":
				if err := os.Remove(run); err != nil {
					t.Fatal(err)
				}
				if failure == "run-file" {
					if err := os.WriteFile(run, []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink(t.TempDir(), run); err != nil {
						t.Fatal(err)
					}
				}
			case "run-unreadable":
				if err := os.Chmod(run, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(run, 0o700) }()
			case "directory-manifest":
				if err := os.Mkdir(manifest, 0o700); err != nil {
					t.Fatal(err)
				}
			case "oversized-manifest":
				file, err := os.Create(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(maxManifestBytes + 1); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			case "unreadable-manifest", "invalid-manifest":
				if err := os.WriteFile(manifest, []byte("invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
				if failure == "unreadable-manifest" {
					if err := os.Chmod(manifest, 0); err != nil {
						t.Fatal(err)
					}
					defer func() { _ = os.Chmod(manifest, 0o600) }()
				}
			}
			runs, err := listRunsFromPinnedProjectDir(t.TempDir(), "fixture", dir, selected, defaultRunEnumerationLimits())
			if err == nil || len(runs) != 0 {
				t.Fatalf("damaged canonical run returned choices: %v %v", runs, err)
			}
			if failure == "run-file" {
				body, err := os.ReadFile(run)
				if err != nil || string(body) != "foreign" {
					t.Fatalf("enumeration changed file: %q %v", body, err)
				}
			}
		})
	}
}
