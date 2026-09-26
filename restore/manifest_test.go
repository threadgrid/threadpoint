// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/threadgrid/threadpoint/backup"
)

func TestRestoreValidationRequiresEveryCurrentRunField(t *testing.T) {
	project := filepath.Clean(t.TempDir())
	projectID := backup.ProjectID(project)
	runID := "fixture-run"
	runDir := filepath.Join(t.TempDir(), projectID, runID)
	valid := backup.Run{
		Version:     backup.ManifestVersion,
		ProjectID:   projectID,
		RunID:       runID,
		Operation:   "fixture",
		ProjectRoot: project,
		CreatedAt:   time.Date(2026, 8, 6, 1, 2, 3, 0, time.UTC),
		Path:        runDir,
		Entries: []backup.Entry{{
			Role: "fixture", Path: "memory.md", BackupPath: filepath.Join(runDir, "files", "payload"),
			SHA256: strings.Repeat("a", 64), Size: 1, Mode: 0o600, ModeKnown: true,
		}},
	}
	if err := validateBackupManifest(&valid); err != nil {
		t.Fatalf("valid current run: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*backup.Run)
	}{
		{name: "project id", mutate: func(run *backup.Run) { run.ProjectID = "" }},
		{name: "run id", mutate: func(run *backup.Run) { run.RunID = "" }},
		{name: "operation", mutate: func(run *backup.Run) { run.Operation = "" }},
		{name: "project root", mutate: func(run *backup.Run) { run.ProjectRoot = "" }},
		{name: "created at", mutate: func(run *backup.Run) { run.CreatedAt = time.Time{} }},
		{name: "path", mutate: func(run *backup.Run) { run.Path = "" }},
		{name: "entries", mutate: func(run *backup.Run) { run.Entries = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.Entries = append([]backup.Entry(nil), valid.Entries...)
			test.mutate(&candidate)
			if err := validateBackupManifest(&candidate); err == nil {
				t.Fatalf("incomplete current run was accepted: %+v", candidate)
			}
		})
	}
}

func TestRestoreManifestRejectsInvalidBackupEncodings(t *testing.T) {
	validDigest := strings.Repeat("a", 64)
	projectRoot := filepath.Clean(t.TempDir())
	projectID := backup.ProjectID(projectRoot)
	runID := "fixture"
	runPath := filepath.Join(t.TempDir(), projectID, runID)
	valid := backup.Run{
		Version: backup.ManifestVersion, ProjectID: projectID, RunID: runID,
		Operation: "fixture", ProjectRoot: projectRoot, CreatedAt: time.Now().UTC(), Path: runPath,
		Entries: []backup.Entry{{Role: "fixture", Path: "file.txt", BackupPath: filepath.Join(runPath, "files", "file"), SHA256: validDigest, Size: 1, Mode: 0o600, ModeKnown: true}},
	}
	if err := validateBackupManifest(&valid); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	for _, tc := range []backup.Run{
		{Version: backup.ManifestVersion - 1},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: ""}}},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: "../escape"}}},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: "same"}, {Path: "same"}}},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: "Case"}, {Path: "case"}}},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: "bad", Directory: true, Symlink: true}}},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: "bad", Size: -1}}},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: "bad", BackupPath: "/backup", SHA256: "not-a-digest", Size: 1}}},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: "absent", Absent: true, BackupPath: "/backup"}}},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: "file", BackupPath: "/backup", SHA256: validDigest, Size: 1}}},
		{Version: backup.ManifestVersion, Entries: []backup.Entry{{Path: "dir", BackupPath: "/backup", SHA256: validDigest, Directory: true}}},
	} {
		if err := validateBackupManifest(&tc); err == nil {
			t.Errorf("invalid manifest unexpectedly passed: %#v", tc)
		}
	}
}

func TestBuildPlanValidatesCurrentSchemaBeforePayloadReads(t *testing.T) {
	validHash := strings.Repeat("a", 64)
	tests := []struct {
		name   string
		mutate func(*backup.Entry)
		want   string
	}{
		{name: "missing payload hash", mutate: func(entry *backup.Entry) { entry.SHA256 = "" }, want: "invalid payload SHA-256"},
		{name: "malformed payload hash", mutate: func(entry *backup.Entry) { entry.SHA256 = "not-a-hash" }, want: "invalid payload SHA-256"},
		{name: "absent payload fields", mutate: func(entry *backup.Entry) {
			entry.Absent = true
			entry.BackupPath = "/missing/payload"
			entry.SHA256 = validHash
		}, want: "invalid absent-target encoding"},
		{name: "directory symlink flags", mutate: func(entry *backup.Entry) {
			entry.Directory = true
			entry.Symlink = true
		}, want: "both a directory and a symlink"},
		{name: "directory size", mutate: func(entry *backup.Entry) {
			entry.Directory = true
			entry.Size = 1
		}, want: "invalid directory size"},
		{name: "empty symlink marker", mutate: func(entry *backup.Entry) {
			entry.Symlink = true
			entry.Size = 0
		}, want: "invalid empty symlink marker"},
		{name: "invalid committed post hash", mutate: func(entry *backup.Entry) { entry.PostSHA256 = "bad" }, want: "invalid post-change SHA-256"},
		{name: "invalid pending post hash", mutate: func(entry *backup.Entry) { entry.PendingPostSHA256 = "bad" }, want: "invalid pending post-change SHA-256"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			entry, err := store.BackupBytesWithMode("AGENTS.md", []byte("payload"), "fixture-target", 0o600)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&store.Manifest.Entries[0])
			store.Manifest.Entries[0].BackupPath = filepath.Join(store.RunDir, "files", "definitely-missing-payload")
			if test.name == "absent payload fields" {
				store.Manifest.Entries[0].BackupPath = entry.BackupPath
			}
			writeStoreManifestFixtureUnchecked(t, store)
			_, err = BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q schema refusal before payload read, got %v", test.want, err)
			}
		})
	}
}

func TestBuildPlanRejectsDuplicateEffectiveVersionTwoTargets(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "restore"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupBytesWithMode("AGENTS.md", []byte("first"), "first-role", 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackupBytesWithMode("AGENTS.md", []byte("second"), "second-role", 0o600); err != nil {
		t.Fatal(err)
	}
	if len(store.Manifest.Entries) != 2 {
		t.Fatalf("test requires two role-scoped entries, got %#v", store.Manifest.Entries)
	}
	_, err = BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err == nil || !strings.Contains(err.Error(), "duplicate effective restore target") {
		t.Fatalf("expected duplicate target refusal, got %v", err)
	}
}

func TestBuildPlanRejectsCaseFoldEquivalentVersionTwoTargetsBeforePayloadReads(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "restore"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupBytesWithMode("Review.md", []byte("first"), "first-role", 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackupBytesWithMode("review.md", []byte("second"), "second-role", 0o600); err != nil {
		t.Fatal(err)
	}
	for index := range store.Manifest.Entries {
		store.Manifest.Entries[index].BackupPath = filepath.Join(store.RunDir, "files", fmt.Sprintf("missing-%d", index))
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	runs, err := backup.ListRuns(root, home, "")
	if err != nil || len(runs) != 1 {
		t.Fatalf("saved empty run list = %#v, err=%v", runs, err)
	}

	_, err = BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err == nil || !strings.Contains(err.Error(), "filesystem-equivalent restore target") {
		t.Fatalf("expected case-fold target refusal before payload reads, got %v", err)
	}
}

func TestBuildPlanRejectsUnsupportedBackupManifestVersions(t *testing.T) {
	for _, version := range []int{0, backup.ManifestVersion - 1, backup.ManifestVersion + 1} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			target := filepath.Join(root, "AGENTS.md")
			mustWriteFile(t, target, "backup\n")
			store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			if _, err := store.BackupFile("AGENTS.md", target, "fixture-target"); err != nil {
				t.Fatal(err)
			}
			store.Manifest.Version = version
			writeStoreManifestFixtureUnchecked(t, store)

			_, err = BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
			if err == nil || !strings.Contains(err.Error(), "manifest version") {
				t.Fatalf("expected unsupported manifest version refusal, got %v", err)
			}
		})
	}
}
