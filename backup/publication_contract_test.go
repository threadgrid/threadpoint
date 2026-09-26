// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestUnreviewedStoreCannotPublish(t *testing.T) {
	store, err := NewStore(StoreOptions{ProjectRoot: t.TempDir(), HomeDir: t.TempDir(), Operation: "prune"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackupBytes("native.md", []byte("private"), "native"); err == nil {
		t.Fatal("unreviewed store published a payload")
	}
	if _, err := os.Lstat(store.RunDir); !os.IsNotExist(err) {
		t.Fatalf("unreviewed store wrote its run directory: %v", err)
	}
}

func TestPendingCommitRequiresAtomicGeneration(t *testing.T) {
	store, err := NewStore(StoreOptions{ProjectRoot: t.TempDir(), HomeDir: t.TempDir(), Operation: PendingCommitOperation, RunID: "commit-stage-contract"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); !os.IsNotExist(err) {
		t.Fatalf("review absent manifest: %v", err)
	}
	body := []byte("private")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if err := store.ConfigurePendingCommitGeneration("stage-contract", "native.md", digest, "AGENTS.md", digest, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackupBytes("native.md", body, "stage-source"); err == nil {
		t.Fatal("incremental pending generation was accepted")
	}
	if _, err := os.Lstat(store.RunDir); !os.IsNotExist(err) {
		t.Fatalf("incomplete generation wrote its run directory: %v", err)
	}
}

func TestReadManifestRejectsSupersededVersionWithoutMutation(t *testing.T) {
	store := preparedCommitStore(t, t.TempDir(), t.TempDir(), "commit-stage-schema")
	store.Manifest.Version = 2
	before := writeManifestFixtureUnchecked(t, store)
	path := filepath.Join(store.RunDir, ManifestName)
	if _, err := ReadManifest(path); err == nil {
		t.Fatal("superseded backup version was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected manifest was modified: %v", err)
	}
}

func TestPendingPublicationRejectsInvalidBindingsWithoutCreatingState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*StoreOptions, *PendingCommitGeneration)
	}{
		{"invalid stage", func(_ *StoreOptions, g *PendingCommitGeneration) { g.Binding.StageID = "../outside" }},
		{"wrong operation", func(o *StoreOptions, _ *PendingCommitGeneration) { o.Operation = "prune" }},
		{"wrong run", func(o *StoreOptions, _ *PendingCommitGeneration) { o.RunID = "commit-other" }},
		{"oversized target", func(_ *StoreOptions, g *PendingCommitGeneration) {
			g.Binding.TargetSize = MaxCanonicalGenerationBytes + 1
		}},
		{"absent prior body", func(_ *StoreOptions, g *PendingCommitGeneration) { g.PriorBody = []byte("hidden") }},
		{"absent prior mode", func(_ *StoreOptions, g *PendingCommitGeneration) { g.PriorMode = 0o600 }},
		{"nonregular source mode", func(_ *StoreOptions, g *PendingCommitGeneration) { g.SourceMode = os.ModeDir }},
		{"zero target size", func(_ *StoreOptions, g *PendingCommitGeneration) { g.Binding.TargetSize = 0 }},
		{"escaping source", func(_ *StoreOptions, g *PendingCommitGeneration) { g.Binding.SourcePath = "../native.md" }},
		{"escaping target", func(_ *StoreOptions, g *PendingCommitGeneration) { g.Binding.TargetPath = "../AGENTS.md" }},
		{"bad source digest", func(_ *StoreOptions, g *PendingCommitGeneration) { g.Binding.SourceSHA256 = "not-a-digest" }},
		{"bad target digest", func(_ *StoreOptions, g *PendingCommitGeneration) { g.Binding.TargetSHA256 = "not-a-digest" }},
		{"source mismatch", func(_ *StoreOptions, g *PendingCommitGeneration) { g.SourceBody = []byte("changed") }},
		{"wrong configured home", func(o *StoreOptions, _ *PendingCommitGeneration) { o.ThreadpointHome = o.ProjectRoot }},
		{"escaping backup namespace", func(o *StoreOptions, _ *PendingCommitGeneration) { o.BackupDir = "../outside" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			source := []byte("native")
			sum := sha256.Sum256(source)
			digest := hex.EncodeToString(sum[:])
			opts := StoreOptions{ProjectRoot: t.TempDir(), ThreadpointHome: home}
			generation := PendingCommitGeneration{Binding: CommitBinding{StageID: "stage-contract", SourcePath: "native.md", SourceSHA256: digest, TargetPath: "AGENTS.md", TargetSHA256: digest, TargetSize: int64(len(source))}, SourceBody: source, SourceMode: 0o600}
			tc.change(&opts, &generation)
			if _, err := PublishPendingCommitGenerationFromRoot(root, home, opts, generation); err == nil {
				t.Fatal("invalid publication accepted")
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected publication wrote state: %v, %v", entries, err)
			}
		})
	}
}

func TestEntryAPIsRequireReviewedOrdinaryStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Store) error
	}{
		{"file", func(s *Store) error { _, err := s.BackupFile("native.md", "missing", "native"); return err }},
		{"bytes", func(s *Store) error { _, err := s.BackupBytes("native.md", []byte("native"), "native"); return err }},
		{"rooted bytes", func(s *Store) error {
			_, err := s.BackupBytesWithModeFromRoot(nil, "", "native.md", []byte("native"), "native", 0o600)
			return err
		}},
		{"symlink", func(s *Store) error { _, err := s.BackupSymlinkTarget("native.md", "target", "native"); return err }},
		{"directory", func(s *Store) error { _, err := s.BackupDir("directory", "missing", "native"); return err }},
		{"absence", func(s *Store) error { _, err := s.RecordAbsent("native.md", "native"); return err }},
		{"rooted absence", func(s *Store) error { _, err := s.RecordAbsentFromRoot(nil, "", "native.md", "native"); return err }},
		{"save", func(s *Store) error { return s.Save() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(nil); err == nil {
				t.Fatal("nil store accepted")
			}
			store, err := NewStore(StoreOptions{ProjectRoot: t.TempDir(), HomeDir: t.TempDir(), Operation: "prune"})
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.run(store); err == nil {
				t.Fatal("unreviewed store accepted")
			}
			if _, err := os.Lstat(store.RunDir); !os.IsNotExist(err) {
				t.Fatalf("unreviewed operation wrote state: %v", err)
			}
		})
	}
}
