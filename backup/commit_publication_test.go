// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupTypeReplacementPublishesDistinctPayload(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: t.TempDir(), Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	regular, err := store.BackupBytes("guide", []byte("regular"), "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	link, err := store.BackupSymlinkTarget("guide", "target.md", "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if regular.BackupPath == link.BackupPath {
		t.Fatalf("type replacement reused payload path %q", regular.BackupPath)
	}
	manifest, err := ReadManifest(filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || !manifest.Entries[0].Symlink || manifest.Entries[0].BackupPath != link.BackupPath {
		t.Fatalf("type replacement manifest = %#v", manifest.Entries)
	}
	if body, err := os.ReadFile(regular.BackupPath); err != nil || string(body) != "regular" {
		t.Fatalf("type replacement destroyed regular payload, body=%q err=%v", body, err)
	}
}

func TestPublishPendingCommitGenerationFromRootPublishesCompleteGeneration(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()

	source := []byte("provider-native source\n")
	prior := []byte("# canonical before commit\n")
	proposed := append(append([]byte(nil), prior...), []byte("\ncommitted review\n")...)
	binding := pendingCommitTestBinding("stage-atomic-complete", source, proposed)
	store, err := PublishPendingCommitGenerationFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot:     project,
		ThreadpointHome: home,
	}, PendingCommitGeneration{
		Binding:     binding,
		SourceBody:  source,
		SourceMode:  0o640,
		PriorExists: true,
		PriorBody:   prior,
		PriorMode:   0o644,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.Manifest.Operation != PendingCommitOperation || store.Manifest.CommitTransaction == nil || store.Manifest.CommitTransaction.State != CommitTransactionPending {
		t.Fatalf("published manifest transaction = %+v", store.Manifest)
	}
	if len(store.Manifest.Entries) != 2 || store.Manifest.Entries[0].Role != "stage-source" || store.Manifest.Entries[1].Role != "canonical-prior" {
		t.Fatalf("published entries = %+v", store.Manifest.Entries)
	}

	bound, err := LoadBoundCommitTransactionFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home,
		Operation: PendingCommitOperation, RunID: "commit-" + binding.StageID,
	}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !bound.PriorKnown || !bound.PriorExists || !bytes.Equal(bound.PriorBody, prior) || bound.PriorMode != 0o644 {
		t.Fatalf("published prior generation = %+v", bound)
	}

	projectRuns := filepath.Join(home, "backups", ProjectID(project))
	entries, err := os.ReadDir(projectRuns)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "commit-"+binding.StageID {
		t.Fatalf("published project generations = %#v", entries)
	}
}

func TestPublishPendingCommitGenerationRejectsPriorAboveCanonicalCeiling(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()

	source := []byte("provider-native source\n")
	prior := bytes.Repeat([]byte("p"), MaxCanonicalGenerationBytes+1)
	proposed := []byte("canonical publication\n")
	binding := pendingCommitTestBinding("stage-canonical-prior-publish-limit", source, proposed)
	if _, err := PublishPendingCommitGenerationFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home,
	}, PendingCommitGeneration{
		Binding: binding, SourceBody: source, SourceMode: 0o600,
		PriorExists: true, PriorBody: prior, PriorMode: 0o600,
	}); err == nil || !strings.Contains(err.Error(), "sealed prior canonical generation exceeds") {
		t.Fatalf("oversized sealed-prior publication error = %v", err)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("rejected sealed-prior publication created state: entries=%v err=%v", entries, err)
	}
}

func TestPublishPendingCommitGenerationFromRootFailureLeavesNoCanonicalGeneration(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()

	source := []byte("provider-native source\n")
	proposed := []byte("canonical publication\n")
	binding := pendingCommitTestBinding("stage-atomic-failure", source, proposed)
	original := pendingCommitGenerationBeforePublish
	pendingCommitGenerationBeforePublish = func(_ *os.Root, _ string) error {
		return errors.New("injected publication failure")
	}
	t.Cleanup(func() { pendingCommitGenerationBeforePublish = original })

	store, err := PublishPendingCommitGenerationFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home,
	}, PendingCommitGeneration{
		Binding:    binding,
		SourceBody: source,
		SourceMode: 0o600,
	})
	if err == nil || !strings.Contains(err.Error(), "injected publication failure") || store != nil {
		t.Fatalf("publication result: store=%+v err=%v", store, err)
	}

	projectRuns := filepath.Join(home, "backups", ProjectID(project))
	entries, err := os.ReadDir(projectRuns)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed publication left observable generation residue: %#v", entries)
	}
}

func TestPublishPendingCommitGenerationFromRootDoesNotReplaceExistingGeneration(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()

	source := []byte("provider-native source\n")
	proposed := []byte("canonical publication\n")
	binding := pendingCommitTestBinding("stage-atomic-collision", source, proposed)
	spec := PendingCommitGeneration{Binding: binding, SourceBody: source, SourceMode: 0o600}
	first, err := PublishPendingCommitGenerationFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home,
	}, spec)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(first.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}

	second, err := PublishPendingCommitGenerationFromRoot(homeRoot, home, StoreOptions{
		ProjectRoot: project, ThreadpointHome: home,
	}, spec)
	if !errors.Is(err, os.ErrExist) || second != nil {
		t.Fatalf("collision result: store=%+v err=%v", second, err)
	}
	after, err := os.ReadFile(filepath.Join(first.RunDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("no-replace collision modified the published manifest")
	}
	entries, err := os.ReadDir(filepath.Dir(first.RunDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(first.RunDir) {
		t.Fatalf("collision left private publication residue: %#v", entries)
	}
}

func TestPendingCommitPublicationRejectsChangedPreparedEvidence(t *testing.T) {
	for _, change := range []string{"manifest-missing", "manifest-mode", "manifest-bytes", "payload-missing", "payload-mode", "payload-bytes", "temporary-replaced", "parent-replaced", "concurrent-run", "closed-parent"} {
		t.Run(change, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			homeRoot, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			defer homeRoot.Close()
			source, prior := []byte("native"), []byte("prior")
			binding := pendingCommitTestBinding("stage-prepared-evidence", source, []byte("published"))
			runID := "commit-" + binding.StageID
			original := pendingCommitGenerationBeforePublish
			defer func() { pendingCommitGenerationBeforePublish = original }()
			var concurrent string
			pendingCommitGenerationBeforePublish = func(parent *os.Root, name string) error {
				temp := filepath.Join(parent.Name(), name)
				manifestPath := filepath.Join(temp, ManifestName)
				manifest, err := ReadManifest(manifestPath)
				if err != nil {
					return err
				}
				payloadRel, err := filepath.Rel(filepath.Join(parent.Name(), runID), manifest.Entries[0].BackupPath)
				if err != nil {
					return err
				}
				payload := filepath.Join(temp, payloadRel)
				switch change {
				case "manifest-missing":
					return os.Remove(manifestPath)
				case "manifest-mode":
					return os.Chmod(manifestPath, 0o644)
				case "manifest-bytes":
					return os.WriteFile(manifestPath, []byte("changed"), 0o600)
				case "payload-missing":
					return os.Remove(payload)
				case "payload-mode":
					return os.Chmod(payload, 0o644)
				case "payload-bytes":
					return os.WriteFile(payload, []byte("changed"), 0o600)
				case "temporary-replaced":
					if err := os.Rename(temp, temp+".retained"); err != nil {
						return err
					}
					concurrent = temp
					if err := os.Mkdir(temp, 0o700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(temp, "sentinel"), []byte("concurrent"), 0o600)
				case "parent-replaced":
					path := parent.Name()
					if err := os.Rename(path, path+".retained"); err != nil {
						return err
					}
					concurrent = path
					if err := os.Mkdir(path, 0o700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(path, "sentinel"), []byte("concurrent"), 0o600)
				case "concurrent-run":
					concurrent = filepath.Join(parent.Name(), runID)
					if err := os.Mkdir(concurrent, 0o700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(concurrent, "sentinel"), []byte("concurrent"), 0o600)
				case "closed-parent":
					return parent.Close()
				}
				return nil
			}
			store, err := PublishPendingCommitGenerationFromRoot(homeRoot, home, StoreOptions{ProjectRoot: project, ThreadpointHome: home}, PendingCommitGeneration{Binding: binding, SourceBody: source, SourceMode: 0o600, PriorExists: true, PriorBody: prior, PriorMode: 0o600})
			if err == nil || store != nil {
				t.Fatalf("changed generation published: %v %v", store, err)
			}
			if concurrent != "" {
				if body, err := os.ReadFile(filepath.Join(concurrent, "sentinel")); err != nil || string(body) != "concurrent" {
					t.Fatalf("concurrent generation changed: %q %v", body, err)
				}
			}
			if change != "concurrent-run" {
				matches, err := filepath.Glob(filepath.Join(home, "backups", "*", runID))
				if err != nil || len(matches) != 0 {
					t.Fatalf("failed generation became visible: %v %v", matches, err)
				}
			}
		})
	}
}
