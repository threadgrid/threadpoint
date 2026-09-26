// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestIsNotSymlink(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "file")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(regular); err == nil || !isNotSymlink(err) {
		t.Fatalf("expected a regular file to classify as not-a-symlink, got %v", err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Fatalf("reading a real symlink should succeed, got %v", err)
	}

	if _, err := os.Readlink(filepath.Join(dir, "missing")); err == nil || isNotSymlink(err) {
		t.Fatalf("a missing path must not classify as not-a-symlink, got %v", err)
	}
}

func TestBuildPlanRejectsIncompleteModeMetadataWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		seed   func(*backup.Store) error
		mutate func(*backup.Entry)
	}{
		{
			name: "regular file mode",
			seed: func(store *backup.Store) error {
				_, err := store.BackupBytesWithMode("AGENTS.md", []byte("payload"), "fixture", 0o640)
				return err
			},
			mutate: func(entry *backup.Entry) { entry.ModeKnown = false },
		},
		{
			name: "directory root mode",
			seed: func(store *backup.Store) error {
				source := t.TempDir()
				mustWriteFile(t, filepath.Join(source, "guide.md"), "guide")
				_, err := store.BackupDir("knowledge", source, "fixture")
				return err
			},
			mutate: func(entry *backup.Entry) { entry.ModeKnown = false },
		},
		{
			name: "directory tree modes",
			seed: func(store *backup.Store) error {
				source := t.TempDir()
				mustWriteFile(t, filepath.Join(source, "guide.md"), "guide")
				_, err := store.BackupDir("knowledge", source, "fixture")
				return err
			},
			mutate: func(entry *backup.Entry) { delete(entry.TreeModes, "guide.md") },
		},
	} {
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

			if err := test.seed(store); err != nil {
				t.Fatal(err)
			}
			test.mutate(&store.Manifest.Entries[0])
			writeStoreManifestFixtureUnchecked(t, store)
			manifestPath := filepath.Join(store.RunDir, backup.ManifestName)
			before, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID}); err == nil || !strings.Contains(err.Error(), "mode metadata") {
				t.Fatalf("incomplete mode metadata was accepted: %v", err)
			}
			after, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("rejected manifest was modified")
			}
		})
	}
}

func TestListRunsUsesTheSameNormalizedRestoreOptionsAsPlanning(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.RecordAbsent("generated.txt", "fixture"); err != nil {
		t.Fatal(err)
	}
	runs, err := ListRuns(Options{Root: root, HomeDir: home})
	if err != nil || len(runs) != 1 || runs[0].RunID != store.RunID {
		t.Fatalf("restoration runs = %#v, %v", runs, err)
	}
}

func TestBuildSymlinkCandidateClassifiesExistingTargetStates(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close project root: %v", err)
		}
	})

	if err := os.Symlink("same-target", filepath.Join(rootPath, "same")); err != nil {
		t.Fatal(err)
	}
	matched, err := buildSymlinkCandidate(root, Candidate{Path: "same"}, "same", []byte("same-target\n"))
	if err != nil || matched.Status != StatusAlreadyRestored || matched.CurrentSHA256 != restoreSnapshotString("same-target") {
		t.Fatalf("matching symlink = %#v, err=%v", matched, err)
	}

	if err := os.Symlink("post-target", filepath.Join(rootPath, "post")); err != nil {
		t.Fatal(err)
	}
	postHash := restoreSnapshotString("post-target")
	post, err := buildSymlinkCandidate(root, Candidate{Path: "post", PostSHA256: postHash}, "post", []byte("backup-target\n"))
	if err != nil || post.Status != StatusReady || post.CurrentSHA256 != postHash {
		t.Fatalf("post-change symlink = %#v, err=%v", post, err)
	}
	if err := os.Symlink("changed-target", filepath.Join(rootPath, "changed")); err != nil {
		t.Fatal(err)
	}
	changed, err := buildSymlinkCandidate(root, Candidate{Path: "changed"}, "changed", []byte("backup-target\n"))
	if err != nil || changed.Status != StatusConflict || changed.Code != "restore_target_symlink_changed" || !strings.Contains(changed.Message, "changed after the backup") {
		t.Fatalf("changed symlink = %#v, err=%v", changed, err)
	}

	if err := os.WriteFile(filepath.Join(rootPath, "regular"), []byte("not a symlink"), 0o644); err != nil {
		t.Fatal(err)
	}
	conflict, err := buildSymlinkCandidate(root, Candidate{Path: "regular"}, "regular", []byte("backup-target\n"))
	if err != nil || conflict.Status != StatusConflict || conflict.Code != "restore_target_symlink_conflict" || !strings.Contains(conflict.Message, "not the backed-up symlink") {
		t.Fatalf("regular target = %#v, err=%v", conflict, err)
	}
}

func TestBuildAbsentCandidateClassifiesMatchingAndChangedCreatedFiles(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(rootPath, "created.md"), []byte("expected"), 0o644); err != nil {
		t.Fatal(err)
	}
	matchingHash := bytesSHA256([]byte("expected"))
	matching, err := buildAbsentCandidate(root, Candidate{Path: "created.md", PostSHA256: matchingHash}, "created.md", nil)
	if err != nil || matching.Status != StatusReady || matching.CurrentSHA256 != matchingHash {
		t.Fatalf("matching created file = %#v, err=%v", matching, err)
	}
	changed, err := buildAbsentCandidate(root, Candidate{Path: "created.md", PostSHA256: bytesSHA256([]byte("other"))}, "created.md", nil)
	if err != nil || changed.Status != StatusConflict || changed.Code != "restore_created_target_changed" || !strings.Contains(changed.Message, "changed after") {
		t.Fatalf("changed created file = %#v, err=%v", changed, err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "created-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	directory, err := buildAbsentCandidate(root, Candidate{Path: "created-dir"}, "created-dir", nil)
	if err != nil || directory.Status != StatusConflict || !strings.Contains(directory.CurrentSHA256, "directory:") {
		t.Fatalf("created directory = %#v, err=%v", directory, err)
	}
}

func TestBuildPlanRejectsOversizedCurrentTarget(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, "")
	if err := os.Truncate(target, maxRestoreRegularPayloadSize+1); err != nil {
		t.Fatal(err)
	}
	_, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err == nil || !strings.Contains(err.Error(), "is larger than") {
		t.Fatalf("expected bounded current-target refusal, got %v", err)
	}
}

func TestBuildPlanAcceptsPendingThreadpointPostState(t *testing.T) {
	tests := []struct {
		name string
		seed func(*testing.T, string, string) string
	}{
		{
			name: "regular file",
			seed: func(t *testing.T, root string, home string) string {
				t.Helper()
				target := filepath.Join(root, "AGENTS.md")
				mustWriteFile(t, target, "before")
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
				mustWriteFile(t, target, "after")
				if err := store.SetPendingPostHashValue("AGENTS.md", "fixture-target", mustFileSHA256(t, target)); err != nil {
					t.Fatal(err)
				}
				return store.RunID
			},
		},
		{
			name: "directory",
			seed: func(t *testing.T, root string, home string) string {
				t.Helper()
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
				hash, err := backup.DirectorySHA256(target)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SetPendingPostHashValue("native", "fixture-target", hash); err != nil {
					t.Fatal(err)
				}
				return store.RunID
			},
		},
		{
			name: "symlink",
			seed: func(t *testing.T, root string, home string) string {
				t.Helper()
				store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}

				if _, err := store.BackupSymlinkTarget("current", "before", "fixture-target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("after", filepath.Join(root, "current")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				if err := store.SetPendingPostHashValue("current", "fixture-target", restoreSnapshotString("after")); err != nil {
					t.Fatal(err)
				}
				return store.RunID
			},
		},
		{
			name: "originally absent",
			seed: func(t *testing.T, root string, home string) string {
				t.Helper()
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
				target := filepath.Join(root, "AGENTS.md")
				mustWriteFile(t, target, "created")
				if err := store.SetPendingPostHashValue("AGENTS.md", "fixture-target", mustFileSHA256(t, target)); err != nil {
					t.Fatal(err)
				}
				return store.RunID
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			runID := test.seed(t, root, home)
			plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusReady || plan.Candidates[0].PendingPostSHA256 == "" {
				t.Fatalf("pending post state was not accepted: %#v", plan.Candidates)
			}
		})
	}
}

func TestRestoreAPIsRefuseUnresolvableRelativePaths(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"normalize-root", func() error { _, err := normalizeOptions(Options{Root: "relative", HomeDir: home}); return err }},
		{"normalize-default", func() error { _, err := normalizeOptions(Options{HomeDir: home}); return err }},
		{"normalize-home", func() error { _, err := normalizeOptions(Options{Root: project, HomeDir: "relative"}); return err }},
		{"list", func() error { _, err := ListRuns(Options{Root: "relative", HomeDir: home}); return err }},
		{"apply", func() error {
			_, err := Apply(context.Background(), Options{Root: "relative", HomeDir: home, Yes: true})
			return err
		}},
		{"interactive", func() error {
			_, err := RunInteractive(context.Background(), Options{Root: "relative", HomeDir: home}, strings.NewReader(""), io.Discard, io.Discard)
			return err
		}},
		{"entry-root", func() error { _, err := targetRootForEntry("relative", project); return err }},
		{"recorded-root", func() error { _, err := targetRootForEntry(project, "relative"); return err }},
		{"candidate-root", func() error {
			_, err := candidateRootName(Candidate{Root: "relative", Target: filepath.Join(project, "memory.md")})
			return err
		}},
		{"candidate-target", func() error { _, err := candidateRootName(Candidate{Root: project, Target: "relative"}); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("unresolvable restore path accepted")
			}
		})
	}
}

func TestCandidateInspectionRefusesUnavailableCurrentEvidence(t *testing.T) {
	for _, kind := range []string{"absent", "symlink"} {
		for _, failure := range []string{"closed-root", "unreadable-file", "unreadable-directory", "changed-before-read", "changed-before-comparison"} {
			if kind == "symlink" && strings.HasPrefix(failure, "changed-") {
				continue
			}
			t.Run(kind+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				if err := root.WriteFile("entry", []byte("current"), 0o600); err != nil {
					t.Fatal(err)
				}
				hooks := &restoreHooks{}
				switch failure {
				case "closed-root":
					err = root.Close()
				case "unreadable-file":
					err = root.Chmod("entry", 0)
					defer func() { _ = os.Chmod(filepath.Join(dir, "entry"), 0o600) }()
				case "unreadable-directory":
					if err := root.Remove("entry"); err != nil {
						t.Fatal(err)
					}
					err = root.Mkdir("entry", 0)
					defer func() { _ = os.Chmod(filepath.Join(dir, "entry"), 0o700) }()
				case "changed-before-read", "changed-before-comparison":
					calls := 0
					hooks.beforeCurrentRegularRead = func(r *os.Root, name string) {
						calls++
						if failure == "changed-before-read" || calls == 2 {
							if err := r.Remove(name); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				candidate := Candidate{Path: "entry", PostSHA256: bytesSHA256([]byte("current"))}
				if kind == "absent" {
					_, err = buildAbsentCandidate(root, candidate, "entry", hooks)
				} else {
					_, err = buildSymlinkCandidate(root, candidate, "entry", []byte("symlink:destination\n"))
				}
				if err == nil {
					t.Fatal("unavailable evidence produced a candidate")
				}
			})
		}
	}
}
