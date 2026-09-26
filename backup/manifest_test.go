// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBackupReplacementSaveFailurePreservesManifestPayloadAndMemory(t *testing.T) {
	tests := []struct {
		name   string
		first  func(*Store) (*Entry, error)
		second func(*Store) (*Entry, error)
		body   string
	}{
		{
			name: "regular to regular",
			first: func(store *Store) (*Entry, error) {
				return store.BackupBytes("AGENTS.md", []byte("v1"), "fixture-target")
			},
			second: func(store *Store) (*Entry, error) {
				return store.BackupBytes("AGENTS.md", []byte("v2"), "fixture-target")
			},
			body: "v1",
		},
		{
			name: "symlink to symlink",
			first: func(store *Store) (*Entry, error) {
				return store.BackupSymlinkTarget("guide", "first.md", "fixture-target")
			},
			second: func(store *Store) (*Entry, error) {
				return store.BackupSymlinkTarget("guide", "second.md", "fixture-target")
			},
			body: "first.md\n",
		},
		{
			name: "regular to symlink",
			first: func(store *Store) (*Entry, error) {
				return store.BackupBytes("guide", []byte("regular"), "fixture-target")
			},
			second: func(store *Store) (*Entry, error) {
				return store.BackupSymlinkTarget("guide", "target.md", "fixture-target")
			},
			body: "regular",
		},
		{
			name: "symlink to regular",
			first: func(store *Store) (*Entry, error) {
				return store.BackupSymlinkTarget("guide", "target.md", "fixture-target")
			},
			second: func(store *Store) (*Entry, error) {
				return store.BackupBytes("guide", []byte("regular"), "fixture-target")
			},
			body: "target.md\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: t.TempDir(), Operation: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			first, err := test.first(store)
			if err != nil {
				t.Fatal(err)
			}
			persistedBefore, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
			if err != nil {
				t.Fatal(err)
			}
			wantErr := errors.New("injected manifest save failure")
			store.beforeSave = func() error { return wantErr }
			if _, err := test.second(store); !errors.Is(err, wantErr) {
				t.Fatalf("replacement error = %v, want %v", err, wantErr)
			}
			store.beforeSave = nil
			persistedAfter, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persistedAfter.Entries, persistedBefore.Entries) {
				t.Fatalf("persisted manifest changed after failed replacement:\nbefore=%#v\nafter=%#v", persistedBefore.Entries, persistedAfter.Entries)
			}
			if !reflect.DeepEqual(store.Manifest.Entries, persistedBefore.Entries) {
				t.Fatalf("in-memory manifest was not rolled back: %#v", store.Manifest.Entries)
			}
			if body, err := os.ReadFile(first.BackupPath); err != nil || string(body) != test.body {
				t.Fatalf("prior payload changed after failed replacement, body=%q err=%v", body, err)
			}
		})
	}
}

func TestReadManifestRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	realManifest := filepath.Join(dir, "real.json")
	mustWriteFile(t, realManifest, `{}`)
	link := filepath.Join(dir, ManifestName)
	if err := os.Symlink(realManifest, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ReadManifest(link); err == nil {
		t.Fatal("expected symlinked backup manifest to be rejected")
	}
}

func TestSaveVariantsRejectSemanticallyInvalidCurrentManifestBeforePublication(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Store)
		want   string
	}{
		{
			name: "missing operation",
			mutate: func(store *Store) {
				store.Manifest.Operation = ""
			},
			want: "canonical operation",
		},
		{
			name: "missing created at",
			mutate: func(store *Store) {
				store.Manifest.CreatedAt = time.Time{}
			},
			want: "createdAt",
		},
		{
			name: "project identity mismatch",
			mutate: func(store *Store) {
				store.Manifest.ProjectID = strings.Repeat("0", 16)
			},
			want: "projectId",
		},
		{
			name: "invalid entry encoding",
			mutate: func(store *Store) {
				store.Manifest.Entries[0].Absent = false
			},
			want: "payload SHA-256",
		},
		{
			name: "payload namespace escape",
			mutate: func(store *Store) {
				store.Manifest.Entries[0] = Entry{
					Role: "fixture", Path: "memory.md", BackupPath: filepath.Join(t.TempDir(), "payload"),
					SHA256: strings.Repeat("a", 64), ModeKnown: true, Mode: 0o600,
				}
			},
			want: "outside the selected project run",
		},
	}

	for _, rooted := range []bool{false, true} {
		for _, test := range tests {
			name := "ambient/" + test.name
			if rooted {
				name = "rooted/" + test.name
			}
			t.Run(name, func(t *testing.T) {
				project := t.TempDir()
				state := t.TempDir()
				now := time.Date(2026, 8, 6, 1, 2, 3, 0, time.UTC)
				opts := StoreOptions{ProjectRoot: project, Operation: "fixture", Now: func() time.Time { return now }}
				var (
					store     *Store
					stateRoot *os.Root
					err       error
				)
				if rooted {
					opts.ThreadpointHome = state
					stateRoot, err = os.OpenRoot(state)
					if err != nil {
						t.Fatal(err)
					}
					defer stateRoot.Close()
					store, err = NewStoreFromRoot(stateRoot, state, opts)
				} else {
					opts.HomeDir = state
					store, err = NewStore(opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				store.Manifest.Entries = []Entry{{Role: "fixture", Path: "memory.md", Absent: true}}
				test.mutate(store)

				if rooted {
					err = store.SaveFromRoot(stateRoot, state)
				} else {
					err = store.Save()
				}
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("invalid manifest publication error = %v, want %q", err, test.want)
				}
				if _, statErr := os.Lstat(filepath.Join(store.RunDir, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("invalid manifest became visible: %v", statErr)
				}
			})
		}
	}
}

func TestReadManifestRequiresExactCompleteCurrentSchema(t *testing.T) {
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
	manifestPath := filepath.Join(store.RunDir, ManifestName)
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var valid map[string]any
	if err := json.Unmarshal(body, &valid); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing-version", mutate: func(value map[string]any) { delete(value, "version") }},
		{name: "missing-operation", mutate: func(value map[string]any) { delete(value, "operation") }},
		{name: "missing-created-at", mutate: func(value map[string]any) { delete(value, "createdAt") }},
		{name: "unknown-field", mutate: func(value map[string]any) { value["futureField"] = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := make(map[string]any, len(valid))
			for key, item := range valid {
				value[key] = item
			}
			test.mutate(value)
			invalid, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), ManifestName)
			if err := os.WriteFile(path, invalid, 0o600); err != nil {
				t.Fatal(err)
			}
			if manifest, err := ReadManifest(path); err == nil {
				t.Fatalf("invalid manifest was accepted: %#v", manifest)
			}
		})
	}

	trailing := filepath.Join(t.TempDir(), ManifestName)
	if err := os.WriteFile(trailing, append(append([]byte(nil), body...), []byte("\n{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if manifest, err := ReadManifest(trailing); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("trailing manifest = %#v, %v", manifest, err)
	}
}

func TestBackupWriterRejectsOperationItsStrictReaderCannotDecode(t *testing.T) {
	for _, operation := range []string{"My Backup", "foo/bar", "UPPER"} {
		t.Run(operation, func(t *testing.T) {
			if store, err := NewStore(StoreOptions{ProjectRoot: t.TempDir(), HomeDir: t.TempDir(), Operation: operation}); err == nil || store != nil {
				t.Fatalf("noncanonical writer operation was accepted: store=%+v err=%v", store, err)
			}
		})
	}
}

func TestBackupWriterRefusesZeroEntryManifestPublication(t *testing.T) {
	store, err := NewStore(StoreOptions{ProjectRoot: t.TempDir(), HomeDir: t.TempDir(), Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.Save(); err == nil || !strings.Contains(err.Error(), "zero-entry") {
		t.Fatalf("zero-entry manifest publication error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(store.RunDir, ManifestName)); !os.IsNotExist(err) {
		t.Fatalf("zero-entry manifest became visible: %v", err)
	}
}

func TestReadManifestRequiresExplicitZeroDirectoryRootMode(t *testing.T) {
	manifest := Manifest{
		Version: ManifestVersion, Operation: "fixture", ProjectRoot: filepath.Clean(t.TempDir()),
		RunID: "fixture-run", CreatedAt: time.Now().UTC(),
		Entries: []Entry{{Role: "fixture", Path: "directory", BackupPath: filepath.Join(t.TempDir(), "payload"), SHA256: strings.Repeat("0", 64), Directory: true, ModeKnown: true}},
	}
	manifest.ProjectID = ProjectID(manifest.ProjectRoot)
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ManifestName)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if decoded, err := ReadManifest(path); err == nil || decoded != nil {
		t.Fatalf("directory without explicit root mode was accepted: manifest=%+v err=%v", decoded, err)
	}
}

func TestConditionalManifestSavePreservesReviewedGeneration(t *testing.T) {
	for _, outcome := range []string{"success", "stale", "invalid-manifest", "before-save-failure", "run-replaced"} {
		t.Run(outcome, func(t *testing.T) {
			store, err := NewStore(StoreOptions{ProjectRoot: t.TempDir(), HomeDir: t.TempDir(), Operation: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadManifestGeneration(); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if _, err := store.BackupBytes("guide.md", []byte("guide"), "fixture"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.RunDir, ManifestName)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			expected := append([]byte(nil), before...)
			sentinel := errors.New("manifest publication denied")
			switch outcome {
			case "stale":
				expected = []byte("unreviewed")
			case "invalid-manifest":
				store.Manifest.Entries[0].Role = ""
			case "before-save-failure":
				store.beforeSave = func() error { return sentinel }
			case "run-replaced":
				moved := store.RunDir + "-retained"
				if err := os.Rename(store.RunDir, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, store.RunDir); err != nil {
					t.Fatal(err)
				}
			}
			err = store.saveExpected(expected)
			if outcome == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unreviewed manifest publication accepted")
			}
			if outcome == "before-save-failure" && !errors.Is(err, sentinel) {
				t.Fatalf("save failure lost: %v", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != string(before) {
				t.Fatalf("reviewed bytes changed: %q %v", after, readErr)
			}
		})
	}
}
