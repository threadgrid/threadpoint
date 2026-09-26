// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

func TestRestoreSymlinkMarkerRequiresTrailingDelimiter(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{body: "target", want: "trailing delimiter"},
		{body: "target\n", want: ""},
	} {
		got, err := decodeSymlinkMarker([]byte(tc.body))
		if tc.want == "" {
			if err != nil || got != "target" {
				t.Errorf("decoded marker = %q, %v", got, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("marker %q error = %v", tc.body, err)
		}
	}
}

func TestApplyKeepsPayloadReadsAndBackupWritesOnRetainedProductHome(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("native no-replace rename is required for restore apply")
	}
	root := t.TempDir()
	parent := t.TempDir()
	home := filepath.Join(parent, "threadpoint")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(target, []byte("original\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("AGENTS.md", target, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("post-change\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPostHash("AGENTS.md", target); err != nil {
		t.Fatal(err)
	}
	productRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer productRoot.Close()
	detached := home + ".detached"
	report, err := Apply(context.Background(), Options{
		Root: root, ThreadpointHome: home, BackupID: store.RunID, Yes: true, ProductRoot: productRoot,
		hooks: &restoreHooks{afterPlan: func(*Plan, *os.Root) error {
			if err := os.Rename(home, detached); err != nil {
				return err
			}
			if err := os.MkdirAll(home, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(home, "replacement-sentinel"), []byte("replacement\n"), 0o600)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Resolutions) != 1 || !report.Resolutions[0].Restored {
		t.Fatalf("restore report=%+v", report)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "original\n" {
		t.Fatalf("restore used the wrong payload generation: body=%q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(home, "backups")); !os.IsNotExist(err) {
		t.Fatalf("restore wrote through replacement product home: %v", err)
	}
	runs, err := backup.ListRunsFromRoot(root, productRoot, home, "")
	if err != nil {
		t.Fatal(err)
	}
	foundRestore := false
	for _, run := range runs {
		if run.Operation == restoreOperation {
			foundRestore = true
		}
	}
	if !foundRestore {
		t.Fatalf("retained product-home generation has no restore backup: %+v", runs)
	}
}

func TestBuildCandidateClassifiesDifferentFilesystemTypeAndInvalidPayloadSizes(t *testing.T) {
	rootPath := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(rootPath, "AGENTS.md")
	mustWriteFile(t, target, "backup")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: rootPath, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupFile("AGENTS.md", target, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	candidate, err := buildCandidate(root, Options{Root: rootPath, HomeDir: home}, store.RunID, *entry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeRestoreCandidatePins(&candidate); err != nil {
			t.Errorf("close restore candidate pins: %v", err)
		}
	})
	if candidate.Status != StatusConflict || candidate.Code != "restore_target_type_conflict" || !strings.Contains(candidate.Message, "different filesystem type") {
		t.Fatalf("different filesystem type candidate = %#v", candidate)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		size int64
		want string
	}{
		{name: "mismatched size", size: entry.Size + 1, want: "does not match manifest size"},
		{name: "oversized", size: maxRestoreRegularPayloadSize + 1, want: "larger than"},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := *entry
			invalid.Size = test.size
			_, err := buildCandidate(root, Options{Root: rootPath, HomeDir: home}, store.RunID, invalid)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("buildCandidate error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRestoreDirectoryBackupCopiesModesAndRefusesAppearingDestinations(t *testing.T) {
	for _, test := range []struct {
		name       string
		prepare    func(t *testing.T, sourcePath, targetPath string)
		wantError  bool
		wantTarget string
	}{
		{
			name: "copies directory regular file and symlink",
			prepare: func(t *testing.T, sourcePath, _ string) {
				t.Helper()
				mustWriteFile(t, filepath.Join(sourcePath, "nested", "guide.md"), "guide")
				if err := os.Chmod(filepath.Join(sourcePath, "nested", "guide.md"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("nested/guide.md", filepath.Join(sourcePath, "guide-link")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
			wantTarget: "guide",
		},
		{
			name: "refuses reappeared regular file",
			prepare: func(t *testing.T, sourcePath, targetPath string) {
				t.Helper()
				mustWriteFile(t, filepath.Join(sourcePath, "guide.md"), "source")
				mustWriteFile(t, filepath.Join(targetPath, "guide.md"), "target")
			},
			wantError: true,
		},
		{
			name: "refuses reappeared directory",
			prepare: func(t *testing.T, sourcePath, targetPath string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(sourcePath, "nested"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(targetPath, "nested"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantError: true,
		},
		{
			name: "refuses reappeared symlink",
			prepare: func(t *testing.T, sourcePath, targetPath string) {
				t.Helper()
				if err := os.Symlink("source", filepath.Join(sourcePath, "guide-link")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				if err := os.Symlink("target", filepath.Join(targetPath, "guide-link")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sourcePath := t.TempDir()
			targetPath := t.TempDir()
			test.prepare(t, sourcePath, targetPath)
			source, err := os.OpenRoot(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			target, err := os.OpenRoot(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			modes := map[string]uint32{}
			if err := filepath.Walk(sourcePath, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !info.IsDir() && !info.Mode().IsRegular() {
					return nil
				}
				rel, err := filepath.Rel(sourcePath, path)
				if err != nil {
					return err
				}
				modes[filepath.ToSlash(rel)] = uint32(info.Mode().Perm())
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			err = restoreDirectoryBackup(source, target, modes)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "appeared during copy") {
					t.Fatalf("restore directory collision error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := os.ReadFile(filepath.Join(targetPath, "nested", "guide.md"))
			if readErr != nil || string(body) != test.wantTarget {
				t.Fatalf("restored file = %q, err=%v", body, readErr)
			}
			if link, readErr := os.Readlink(filepath.Join(targetPath, "guide-link")); readErr != nil || link != "nested/guide.md" {
				t.Fatalf("restored symlink = %q, err=%v", link, readErr)
			}
		})
	}
}

func TestApplyRevalidatesRegularBackupPayloadAfterPlan(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupFile("AGENTS.md", target, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, target, "after")
	if err := store.SetPostHash("AGENTS.md", target); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, entry.BackupPath, "tampered after planning")

	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home}, plan, []pendingResolution{{
		candidate: plan.Candidates[0],
		decision:  DecisionRestore,
	}})
	if err == nil || !strings.Contains(err.Error(), "backup payload") || !strings.Contains(err.Error(), "changed since restore plan") {
		t.Fatalf("expected changed backup payload refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "after" {
		t.Fatalf("changed backup payload should not mutate target, body=%q err=%v", body, readErr)
	}
}

func TestOverwriteDirectoryBackupBoundsGrowthAfterTargetVerification(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "before")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupDir("native", target, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(target, "guide.md"), "after")
	postHash, err := backup.DirectorySHA256(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPostHashValue("native", "fixture-target", postHash); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusReady {
		t.Fatalf("expected ready directory restore, got %#v", plan.Candidates)
	}
	hooks := &restoreHooks{beforeOverwriteBackup: func(parent *os.Root, base string) {
		file, err := parent.OpenFile(filepath.Join(base, "growth.bin"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxRestoreDirectoryBytes + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || (!strings.Contains(err.Error(), "larger") && !strings.Contains(err.Error(), "limit") && !strings.Contains(err.Error(), "exceeds")) {
		t.Fatalf("expected bounded overwrite-directory backup refusal, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, "growth.bin")); statErr != nil {
		t.Fatalf("failed bounded backup should leave target intact: %v", statErr)
	}
}

func TestBuildPlanRejectsMismatchedBackupPayloadHash(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, "")
	runs, err := backup.ListRuns(root, home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != runID {
		t.Fatalf("expected seeded run %s, got %#v", runID, runs)
	}
	if err := os.WriteFile(runs[0].Entries[0].BackupPath, []byte("wrong"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err == nil || !strings.Contains(err.Error(), "does not match manifest hash") {
		t.Fatalf("expected manifest hash mismatch, got %v", err)
	}
}

func TestApplyRestoresDirectoryBackupSymlinks(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "native")
	targetFile := filepath.Join(source, "target.txt")
	link := filepath.Join(source, "link.txt")
	mustWriteFile(t, targetFile, "target")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatal(err)
	}
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root,
		HomeDir:     home,
		Operation:   "fixture",
		Now: func() time.Time {
			return time.Date(2026, 5, 30, 1, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupDir("native", source, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}

	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Resolutions) != 1 || !report.Resolutions[0].Restored {
		t.Fatalf("expected directory restore, got %#v", report.Resolutions)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected restored symlink: %v", err)
	}
	if got != "target.txt" {
		t.Fatalf("symlink target = %q, want target.txt", got)
	}
	if _, err := os.Stat(link + ".symlink"); !os.IsNotExist(err) {
		t.Fatalf("symlink marker should not be restored as a regular file, stat err=%v", err)
	}
}

func TestBuildPlanClassifiesMatchingDirectoryBackupAsAlreadyRestored(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "reviewed")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupDir("native", target, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusAlreadyRestored {
		t.Fatalf("matching directory should already be restored: %#v", plan.Candidates)
	}
}

func TestBuildPlanRejectsDirectoryPayloadWithWrongType(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(source, "guide.md"), "guide")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupDir("native", source, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(entry.BackupPath); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, entry.BackupPath, "not a directory")
	if _, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected wrong-type directory payload refusal, got %v", err)
	}
}

func TestBuildPlanBoundsDirectoryBackupPayload(t *testing.T) {
	t.Run("entry count", func(t *testing.T) {
		root := t.TempDir()
		home := t.TempDir()
		source := filepath.Join(root, "native")
		mustWriteFile(t, filepath.Join(source, "guide.md"), "guide")
		store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}

		entry, err := store.BackupDir("native", source, "fixture-target")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < maxRestoreDirectoryEntries; i++ {
			if err := os.Mkdir(filepath.Join(entry.BackupPath, fmt.Sprintf("extra-%04d", i)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		_, err = BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
		if err == nil || !strings.Contains(err.Error(), "exceeds 4097 filesystem entries") {
			t.Fatalf("expected bounded directory-entry refusal, got %v", err)
		}
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		root := t.TempDir()
		home := t.TempDir()
		source := filepath.Join(root, "native")
		mustWriteFile(t, filepath.Join(source, "guide.md"), "guide")
		store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}

		entry, err := store.BackupDir("native", source, "fixture-target")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(filepath.Join(entry.BackupPath, "guide.md"), maxRestoreDirectoryBytes+1); err != nil {
			t.Fatal(err)
		}
		_, err = BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
		if err == nil || !strings.Contains(err.Error(), "exceeds 67108864 bytes") {
			t.Fatalf("expected bounded directory-byte refusal, got %v", err)
		}
	})
}

func TestApplyRevalidatesDirectoryBackupPayloadAfterPlan(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "reviewed")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupDir("native", target, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(entry.BackupPath, "guide.md"), "tampered")
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home}, plan, []pendingResolution{{
		candidate: plan.Candidates[0],
		decision:  DecisionRestore,
	}})
	if err == nil || !strings.Contains(err.Error(), "backup payload") || !strings.Contains(err.Error(), "changed since restore plan") {
		t.Fatalf("expected changed directory payload refusal, got %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("changed directory payload should not create target: %v", statErr)
	}
}

func TestApplyRetainsExactBackupPayloadIdentityAcrossPlan(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("exact restore pins and native no-replace rename are required")
	}
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before\n")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after\n")
	})
	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil || len(plan.Candidates) != 1 {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	payload := plan.Candidates[0].BackupPath
	movedPayload := payload + ".reviewed"
	body, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}

	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, Yes: true, hooks: &restoreHooks{afterPlan: func(_ *Plan, _ *os.Root) error {
		if err := os.Rename(payload, movedPayload); err != nil {
			return err
		}
		return os.WriteFile(payload, body, 0o644)
	}}})
	if err == nil || !strings.Contains(err.Error(), "payload") || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("expected exact backup-payload refusal, report=%#v err=%v", report, err)
	}
	if report != nil {
		t.Fatalf("exact backup-payload refusal created apply state: %#v", report)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "after\n" {
		t.Fatalf("backup-payload refusal changed target, body=%q err=%v", got, readErr)
	}
	for _, path := range []string{payload, movedPayload} {
		if got, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(got, body) {
			t.Fatalf("backup-payload refusal changed %s, bytes=%d err=%v", path, len(got), readErr)
		}
	}
}

func TestApplyRetainsEveryDirectoryPayloadEntryIdentityAcrossPlan(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "reviewed\n")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	entry, err := store.BackupDir("native", target, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(entry.BackupPath, "guide.md")
	movedPayload := entry.BackupPath + ".guide-reviewed"
	body, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}

	report, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID, Yes: true, hooks: &restoreHooks{afterPlan: func(_ *Plan, _ *os.Root) error {
		if err := os.Rename(payload, movedPayload); err != nil {
			return err
		}
		return os.WriteFile(payload, body, 0o644)
	}}})
	if err == nil || !strings.Contains(err.Error(), "payload tree identity") {
		t.Fatalf("expected exact directory-entry refusal, report=%#v err=%v", report, err)
	}
	if report != nil {
		t.Fatalf("exact directory-entry refusal created apply state: %#v", report)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("directory-entry refusal created target: %v", statErr)
	}
	for _, path := range []string{payload, movedPayload} {
		if got, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(got, body) {
			t.Fatalf("directory-entry refusal changed %s, bytes=%d err=%v", path, len(got), readErr)
		}
	}
}

func TestCaptureRestorePayloadTreeRootPinsNestedPhysicalGeneration(t *testing.T) {
	parentPath := t.TempDir()
	payloadPath := filepath.Join(parentPath, "payload")
	if err := os.MkdirAll(filepath.Join(payloadPath, "nested", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(payloadPath, "root.txt"):                 "root\n",
		filepath.Join(payloadPath, "nested", "child.txt"):      "child\n",
		filepath.Join(payloadPath, "nested", "deeper", "leaf"): "leaf\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	tree, err := captureRestorePayloadTreeRoot(parent, "payload")
	if err != nil {
		t.Fatal(err)
	}
	if tree.root == nil || tree.info == nil || len(tree.entries) != 5 {
		t.Fatalf("unexpected rooted payload pin: root=%v info=%v entries=%v", tree.root, tree.info, tree.entries)
	}
	for _, expected := range []string{"root.txt", "nested", "nested/child.txt", "nested/deeper", "nested/deeper/leaf"} {
		if _, ok := tree.entries[expected]; !ok {
			t.Fatalf("missing retained payload entry %q: %v", expected, tree.entries)
		}
	}
	if err := closeRestorePayloadTree(tree); err != nil {
		t.Fatal(err)
	}
	if tree.root != nil {
		t.Fatal("closing the rooted payload retained its directory descriptor")
	}
	// Closing is intentionally idempotent so every plan/error path can use the
	// same cleanup helper.
	if err := closeRestorePayloadTree(tree); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureRestorePayloadTreeRootRejectsNonDirectoryAndSymlinkRoots(t *testing.T) {
	parentPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(parentPath, "regular"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parentPath, "physical"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("physical", filepath.Join(parentPath, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	for _, name := range []string{"regular", "linked"} {
		t.Run(name, func(t *testing.T) {
			tree, err := captureRestorePayloadTreeRoot(parent, name)
			if err == nil || !strings.Contains(err.Error(), "not a real directory") {
				t.Fatalf("expected rooted directory refusal, tree=%v err=%v", tree, err)
			}
			if tree != nil {
				t.Fatal("root refusal returned a partial payload pin")
			}
		})
	}
	if tree, err := captureRestorePayloadTreeRoot(parent, "missing"); tree != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing rooted payload = tree=%v err=%v", tree, err)
	}
}

func TestCaptureRestorePayloadTreePinsAmbientGenerationAndRejectsAliases(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.MkdirAll(filepath.Join(payload, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, "nested", "file"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := captureRestorePayloadTree(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.entries) != 2 {
		t.Fatalf("ambient payload entries = %v", tree.entries)
	}
	if err := closeRestorePayloadTree(tree); err != nil {
		t.Fatal(err)
	}

	regular := filepath.Join(filepath.Dir(payload), "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(filepath.Dir(payload), "linked")
	if err := os.Symlink("payload", linked); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, path := range []string{regular, linked} {
		if tree, err := captureRestorePayloadTree(path); err == nil || tree != nil || !strings.Contains(err.Error(), "not a directory or is a symlink") {
			t.Fatalf("ambient payload alias accepted: path=%s tree=%v err=%v", path, tree, err)
		}
	}
	if tree, err := captureRestorePayloadTree(filepath.Join(filepath.Dir(payload), "missing")); tree != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing ambient payload = tree=%v err=%v", tree, err)
	}
}

func TestPayloadPinningRejectsMissingWrongTypeAndUnreadableEntries(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		for _, rooted := range []bool{false, true} {
			for _, damage := range []string{"missing", "wrong-type", "symlink", "unreadable"} {
				t.Run(fmt.Sprintf("%s/rooted=%v/%s", kind, rooted, damage), func(t *testing.T) {
					dir := t.TempDir()
					path := filepath.Join(dir, "payload")
					if kind == "directory" {
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					} else {
						mustWriteFile(t, path, "sealed")
					}
					switch damage {
					case "missing":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					case "wrong-type":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						if kind == "file" {
							if err := os.Mkdir(path, 0o700); err != nil {
								t.Fatal(err)
							}
						} else {
							mustWriteFile(t, path, "not a directory")
						}
					case "symlink":
						if err := os.Rename(path, path+"-original"); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(path+"-original", path); err != nil {
							t.Fatal(err)
						}
					case "unreadable":
						if err := os.Chmod(path, 0); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
						probe, err := os.Open(path)
						if err == nil {
							_ = probe.Close()
							t.Skip("filesystem does not enforce read permissions")
						}
					}
					root, err := os.OpenRoot(dir)
					if err != nil {
						t.Fatal(err)
					}
					defer root.Close()
					candidate := Candidate{Path: "guide", BackupPath: path, Directory: kind == "directory"}
					if rooted {
						candidate.backupProductRoot, candidate.backupProductRel = root, "payload"
					}
					if err := captureRestoreBackupPins(&candidate); err == nil {
						_ = closeRestoreCandidatePins(&candidate)
						t.Fatal("untrusted payload accepted")
					}
				})
			}
		}
	}
}

func TestReadRestorePayloadRequiresExactLivePinAndBound(t *testing.T) {
	for _, damage := range []string{"negative-limit", "oversized", "closed-pin", "missing-info", "replaced-file", "removed-file", "closed-root"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "payload")
			mustWriteFile(t, path, "sealed")
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			candidate := Candidate{Path: "guide", BackupPath: path, backupProductRoot: root, backupProductRel: "payload"}
			if err := captureRestoreBackupPins(&candidate); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closeRestoreCandidatePins(&candidate) }()
			limit := int64(100)
			switch damage {
			case "negative-limit":
				limit = -1
			case "oversized":
				limit = 1
			case "closed-pin":
				if candidate.backupPin == nil {
					t.Skip("identity pins unavailable")
				}
				if err := candidate.backupPin.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing-info":
				candidate.backupInfo = nil
			case "replaced-file":
				if err := os.Rename(path, path+"-retained"); err != nil {
					t.Fatal(err)
				}
				mustWriteFile(t, path, "replacement")
			case "removed-file":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "closed-root":
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if body, err := readRestoreBackupFile(candidate, limit); err == nil {
				t.Fatalf("untrusted payload read: %q", body)
			}
		})
	}
}

func TestRestorePayloadReadsRequireRetainedCapabilities(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload")
	mustWriteFile(t, path, "sealed")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{Path: "guide", BackupPath: path, backupInfo: info}
	if body, err := readRestoreBackupFile(candidate, 100); err == nil {
		t.Fatalf("payload reopened without reviewed descriptor: %q", body)
	}
	directoryInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	candidate = Candidate{Path: "directory", BackupPath: dir, Directory: true, backupInfo: directoryInfo}
	if err := verifyRestorePayloadTree(candidate); err == nil {
		t.Fatal("directory payload accepted without retained tree")
	}
}

func TestDirectoryRestoreRefusesMissingModesAndOccupiedDestinations(t *testing.T) {
	for _, kind := range []string{"regular", "directory", "symlink"} {
		for _, invalid := range []string{"occupied", "closed-target", "missing-mode"} {
			if kind == "symlink" && invalid == "missing-mode" {
				continue
			}
			t.Run(kind+"/"+invalid, func(t *testing.T) {
				source, err := os.OpenRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer source.Close()
				targetPath := t.TempDir()
				target, err := os.OpenRoot(targetPath)
				if err != nil {
					t.Fatal(err)
				}
				defer target.Close()
				switch kind {
				case "regular":
					err = source.WriteFile("entry", []byte("source"), 0o600)
				case "directory":
					err = source.Mkdir("entry", 0o700)
				case "symlink":
					err = source.Symlink("elsewhere", "entry")
				}
				if err != nil {
					t.Fatal(err)
				}
				modes := map[string]uint32{"entry": 0o700}
				switch invalid {
				case "occupied":
					err = target.WriteFile("entry", []byte("concurrent"), 0o600)
				case "closed-target":
					err = target.Close()
				case "missing-mode":
					modes = nil
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := restoreDirectoryBackup(source, target, modes); err == nil {
					t.Fatal("unsafe copy accepted")
				}
				if invalid == "occupied" {
					body, err := os.ReadFile(filepath.Join(targetPath, "entry"))
					if err != nil || string(body) != "concurrent" {
						t.Fatalf("concurrent target changed: %q %v", body, err)
					}
				} else {
					entries, err := os.ReadDir(targetPath)
					if err != nil || len(entries) != 0 {
						t.Fatalf("failed copy wrote target: %v %v", entries, err)
					}
				}
			})
		}
	}
}

func TestDirectoryPreparationRefusesChangedPayloadAndUnavailableParent(t *testing.T) {
	for _, failure := range []string{"missing-tree", "closed-parent", "changed-before-copy", "missing-mode", "missing-tree-mode", "changed-digest"} {
		t.Run(failure, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			target := filepath.Join(project, "native")
			mustWriteFile(t, filepath.Join(target, "guide.md"), "original")
			store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: project, HomeDir: home, Operation: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if _, err := store.BackupDir("native", target, "fixture"); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: project, HomeDir: home, BackupID: store.RunID})
			if err != nil {
				t.Fatal(err)
			}
			candidate := plan.Candidates[0]
			root, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			parent, err := openRestoreTargetParent(root, plan.rootSnapshot, candidate)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.root.Close()
			hooks := &restoreHooks{}
			switch failure {
			case "missing-tree":
				candidate.backupTree = nil
			case "closed-parent":
				_ = parent.root.Close()
			case "changed-before-copy":
				hooks.beforeDirectoryCopy = func() {
					if err := os.Rename(candidate.BackupPath, candidate.BackupPath+".retained"); err != nil {
						t.Fatal(err)
					}
				}
			case "missing-mode":
				candidate.backupModeKnown = false
			case "missing-tree-mode":
				candidate.backupTreeModes = nil
			case "changed-digest":
				candidate.backupSnapshot = "invalid"
			}
			prepared, err := prepareRestoreDirectory(parent, candidate, hooks)
			if prepared != nil {
				_ = prepared.root.Close()
			}
			if err == nil {
				t.Fatal("untrusted directory preparation succeeded")
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed preparation installed target: %v", err)
			}
			entries, err := os.ReadDir(project)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".threadpoint-restore-directory-") {
					t.Fatalf("failed preparation leaked %s", entry.Name())
				}
			}
		})
	}
}

func TestDirectoryPayloadPreparationRequiresRetainedTree(t *testing.T) {
	path := t.TempDir()
	mustWriteFile(t, filepath.Join(path, "guide.md"), "ambient bytes")
	candidate := Candidate{Directory: true, BackupPath: path, Path: "native"}
	if _, err := prepareBackupPayload(candidate, "", false); err == nil {
		t.Fatal("ambient directory substituted for missing retained payload")
	}
	body, err := os.ReadFile(filepath.Join(path, "guide.md"))
	if err != nil || string(body) != "ambient bytes" {
		t.Fatalf("refused preparation changed ambient payload: %q %v", body, err)
	}
}

func TestPreparedDirectoryPublicationRequiresLiveIdentityAndDigest(t *testing.T) {
	for _, failure := range []string{"missing-prepared", "closed-prepared", "wrong-identity", "wrong-digest", "changed-parent", "rename-failure", "occupied", "residual-name"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			snapshot, project, err := captureAndOpenRestoreRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer project.Close()
			candidate := Candidate{Path: "target", Root: dir, Target: filepath.Join(dir, "target")}
			parent, err := openRestoreTargetParent(project, snapshot, candidate)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.root.Close()
			if err := parent.root.Mkdir("prepared", 0o700); err != nil {
				t.Fatal(err)
			}
			preparedRoot, err := parent.root.OpenRoot("prepared")
			if err != nil {
				t.Fatal(err)
			}
			defer preparedRoot.Close()
			if err := preparedRoot.WriteFile("entry", []byte("prepared"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := preparedRoot.Stat(".")
			if err != nil {
				t.Fatal(err)
			}
			digest, err := restoreDirectoryDigestRoot(preparedRoot, ".")
			if err != nil {
				t.Fatal(err)
			}
			prepared := &preparedRestoreDirectory{name: "prepared", root: preparedRoot, info: info}
			hooks := &restoreHooks{}
			switch failure {
			case "missing-prepared":
				prepared = nil
			case "closed-prepared":
				err = preparedRoot.Close()
			case "wrong-identity":
				prepared.info, err = project.Stat(".")
			case "wrong-digest":
				digest = "unreviewed"
			case "changed-parent":
				hooks.duringDirectoryInstall = func(*os.Root) { _ = parent.project.Close() }
			case "rename-failure":
				hooks.renameNoReplace = func(*os.Root, string, string) error { return errors.New("rename unavailable") }
			case "occupied":
				err = parent.root.WriteFile("target", []byte("foreign"), 0o600)
			case "residual-name":
				hooks.renameNoReplace = func(root *os.Root, oldName, newName string) error {
					if err := root.Rename(oldName, newName); err != nil {
						return err
					}
					return root.Mkdir(oldName, 0o700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := installRootDirectoryNoReplace(parent, prepared, candidate, digest, hooks); err == nil {
				t.Fatal("invalid publication accepted")
			}
			if failure == "occupied" {
				body, err := os.ReadFile(filepath.Join(dir, "target"))
				if err != nil || string(body) != "foreign" {
					t.Fatalf("concurrent target changed: %q %v", body, err)
				}
			} else if failure != "residual-name" {
				if _, err := os.Lstat(filepath.Join(dir, "target")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed publication created target: %v", err)
				}
			}
		})
	}
}

func TestPreparedPayloadInstallationRefusesIncompleteOrOccupiedTarget(t *testing.T) {
	for _, failure := range []string{"closed-parent", "missing-directory", "missing-mode", "occupied-symlink", "invalid-symlink"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			snapshot, project, err := captureAndOpenRestoreRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer project.Close()
			candidate := Candidate{Path: "target", Root: dir, Target: filepath.Join(dir, "target")}
			parent, err := openRestoreTargetParent(project, snapshot, candidate)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.root.Close()
			payload := preparedBackupPayload{body: []byte("restored"), mode: 0o600, modeKnown: true}
			switch failure {
			case "closed-parent":
				err = parent.root.Close()
			case "missing-directory":
				payload.directory = true
			case "missing-mode":
				payload.modeKnown = false
			case "occupied-symlink":
				candidate.Symlink = true
				payload.symlinkTarget = "destination"
				err = parent.root.WriteFile("target", []byte("foreign"), 0o600)
			case "invalid-symlink":
				candidate.Symlink = true
				payload.symlinkTarget = "invalid\x00target"
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := installPreparedBackup(parent, candidate, payload, nil, nil); err == nil {
				t.Fatal("incomplete installation accepted")
			}
			if failure == "occupied-symlink" {
				body, err := os.ReadFile(candidate.Target)
				if err != nil || string(body) != "foreign" {
					t.Fatalf("foreign target changed: %q %v", body, err)
				}
			} else if _, err := os.Lstat(candidate.Target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed installation became visible: %v", err)
			}
		})
	}
}

func TestRestoredPermissionValidationRequiresRecordedAndCurrentMode(t *testing.T) {
	for _, failure := range []string{"missing-mode", "missing-target", "wrong-mode"} {
		t.Run(failure, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			payload := preparedBackupPayload{mode: 0o600, modeKnown: true}
			if failure == "missing-mode" {
				payload.modeKnown = false
			}
			if failure == "wrong-mode" {
				if err := root.WriteFile("target", nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := root.Chmod("target", 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := verifyRestoredPermissions(&restoreTargetParent{root: root, base: "target"}, Candidate{Path: "target"}, payload); err == nil {
				t.Fatal("unverified permissions accepted")
			}
		})
	}
}
