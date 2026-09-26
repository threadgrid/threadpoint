// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

func TestRestoreTempCleanupRetainsConcurrentReplacement(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, "")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected install rename failure")
	var replacementName string
	hooks := &restoreHooks{renameNoReplace: func(parent *os.Root, oldName string, newName string) error {
		if !strings.HasPrefix(filepath.Base(oldName), ".threadpoint-restore-file-") {
			return safefs.RenameRootNoReplace(parent, oldName, newName)
		}
		replacementName = oldName
		if err := parent.Remove(oldName); err != nil {
			t.Fatal(err)
		}
		if err := parent.WriteFile(oldName, []byte("concurrent replacement"), 0o644); err != nil {
			t.Fatal(err)
		}
		return wantErr
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("install error = %v, want rename failure plus retained identity-changed residue", err)
	}
	if replacementName == "" {
		t.Fatal("regular install did not reach its no-replace rename")
	}
	if body, readErr := os.ReadFile(filepath.Join(root, replacementName)); readErr != nil || string(body) != "concurrent replacement" {
		t.Fatalf("cleanup deleted concurrent temp-name replacement, body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed installation unexpectedly created canonical target: %v", statErr)
	}
}

func TestPreparedDirectoryCleanupSurfacesIdentityChangedResidue(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "backup")
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
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	var replacement string
	var moved string
	hooks := &restoreHooks{beforePreparedCleanup: func(parent *os.Root, name string) {
		replacement = name
		moved = name + "-moved"
		if err := parent.Rename(name, moved); err != nil {
			t.Fatal(err)
		}
		if err := parent.Mkdir(name, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := parent.WriteFile(filepath.Join(name, "concurrent"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}, duringDirectoryInstall: func(prepared *os.Root) {
		if err := prepared.WriteFile("guide.md", []byte("tampered"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "prepared restore directory") || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("expected surfaced prepared cleanup residue, got %v", err)
	}
	if body, readErr := os.ReadFile(filepath.Join(root, replacement, "concurrent")); readErr != nil || string(body) != "keep" {
		t.Fatalf("replacement cleanup path changed, body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(root, moved)); statErr != nil {
		t.Fatalf("moved prepared residue was not retained: %v", statErr)
	}
}

func TestFailedDirectoryPreparationSurfacesCleanupResidue(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "native")
	mustWriteFile(t, filepath.Join(target, "guide.md"), "backup")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	_, err = store.BackupDir("native", target, "fixture-target")
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
	plan.Candidates[0].backupModeKnown = false
	var moved string
	hooks := &restoreHooks{
		beforePreparedCleanup: func(parent *os.Root, name string) {
			moved = name + "-moved"
			if err := parent.Rename(name, moved); err != nil {
				t.Fatal(err)
			}
			if err := parent.Mkdir(name, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "prepared restore directory") || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("expected failed preparation cleanup residue, got %v", err)
	}
	if moved == "" {
		t.Fatal("failed preparation did not run prepared cleanup hook")
	}
	if _, statErr := os.Lstat(filepath.Join(root, moved)); statErr != nil {
		t.Fatalf("failed-preparation residue was not retained: %v", statErr)
	}
}

func TestRestoreRetainsDetachedOriginalWhenItChangesBeforeDeletion(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	var detached string
	hooks := &restoreHooks{beforeDetachedDelete: func(parent *os.Root, name string) {
		detached = name
		if err := parent.WriteFile(name, []byte("changed while detached"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	report, err := applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "changed during restore") {
		t.Fatalf("expected detached snapshot refusal, got %v", err)
	}
	if detached == "" {
		t.Fatal("detached target hook was not called")
	}
	if report == nil || len(report.Resolutions) != 1 || report.Resolutions[0].Recovery == "" {
		t.Fatalf("changed detached target recovery was not reported: %#v", report)
	}
	recovery := report.Resolutions[0].Recovery
	if body, readErr := os.ReadFile(recovery); readErr != nil || string(body) != "changed while detached" {
		t.Fatalf("changed detached target was deleted, body=%q err=%v", body, readErr)
	}
}

func TestAbsentRestoreRechecksAbsenceAfterDetachedCleanup(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.RecordAbsent("AGENTS.md", "fixture-target"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, target, "created")
	if err := store.SetPostHashValue("AGENTS.md", "fixture-target", mustFileSHA256(t, target)); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &restoreHooks{afterDetachedDelete: func(parent *os.Root, base string) {
		if err := parent.WriteFile(base, []byte("concurrent"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, hooks: hooks}, plan, []pendingResolution{{candidate: plan.Candidates[0], decision: DecisionRestore}})
	if err == nil || !strings.Contains(err.Error(), "reappeared before removal completed") {
		t.Fatalf("expected final absence check refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "concurrent" {
		t.Fatalf("concurrent recreated target changed, body=%q err=%v", body, readErr)
	}
}

func TestPrivateNamespaceAllocationBoundsCollisions(t *testing.T) {
	for _, allocator := range []struct {
		name     string
		allocate func(*os.Root) (string, error)
	}{

		{"directory", func(root *os.Root) (string, error) { return makeRestoreTempDirectory(root, ".prepared-") }},
		{"recovery", makeRestoreRecoveryContainer},
		{"file", func(root *os.Root) (string, error) {
			name, file, err := createRestoreTempFile(root, ".prepared-", 0o600)
			if file != nil {
				err = errors.Join(err, file.Close())
			}
			return name, err
		}},
	} {
		t.Run(allocator.name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			original := rand.Reader
			defer func() { rand.Reader = original }()
			reset := func() { rand.Reader = bytes.NewReader(bytes.Repeat([]byte{0x2a}, 4096)) }
			reset()
			name, err := allocator.allocate(root)
			if err != nil {
				t.Fatal(err)
			}
			marker := name
			if info, err := root.Lstat(name); err == nil && info.IsDir() {
				marker = filepath.Join(name, "sentinel")
			}
			if err := root.WriteFile(marker, []byte("retained"), 0o600); err != nil {
				t.Fatal(err)
			}
			reset()
			if got, err := allocator.allocate(root); err == nil || got != "" {
				t.Fatalf("colliding name accepted: %q %v", got, err)
			}
			if body, err := root.ReadFile(marker); err != nil || string(body) != "retained" {
				t.Fatalf("collision destroyed prior entry: %q %v", body, err)
			}
			if err := root.RemoveAll(name); err != nil {
				t.Fatal(err)
			}
			reset()
			if got, err := allocator.allocate(root); err != nil || got != name {
				t.Fatalf("allocation did not recover: %q %v", got, err)
			}
			_ = root.Close()
			reset()
			if _, err := allocator.allocate(root); err == nil {
				t.Fatal("closed namespace accepted")
			}
		})
	}
}
