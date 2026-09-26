// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/threadgrid/threadpoint/safefs"
)

func TestReviewPublicationRestoresPriorGenerationWhenValidationFails(t *testing.T) {
	for _, mechanism := range []string{"exchange", "journal"} {
		t.Run(mechanism, func(t *testing.T) {
			if !safefs.RenameRootNoReplaceSupported() || (mechanism == "exchange" && !safefs.RenameRootExchangeSupported()) {
				t.Skip("required rename unavailable")
			}
			previous := exchangeStageRoots
			t.Cleanup(func() { exchangeStageRoots = previous })
			if mechanism == "journal" {
				exchangeStageRoots = func(*os.Root, string, string) error { return safefs.ErrRenameExchangeUnsupported }
			}
			for stop := 1; stop <= 4; stop++ {
				root, home := initGitRepo(t), t.TempDir()
				write(t, filepath.Join(root, "notes.md"), "original source\n")
				report, err := Create(context.Background(), Options{Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectShared}, Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}}})
				if err != nil {
					t.Fatal(err)
				}
				record := report.Stages[0]
				opened, err := openStageForMutationRoot(root, home, record.ID, nil)
				if err != nil {
					t.Fatal(err)
				}
				original, err := os.ReadFile(filepath.Join(record.Dir, ContentFile))
				if err != nil {
					t.Fatal(err)
				}
				sentinel := errors.New("selected project changed")
				calls := 0
				record.SourceSHA256, record.ContentSHA256 = digest([]byte("new source")), digest([]byte("new review"))
				err = replaceStageGeneration(opened, record, []byte("new source"), []byte("new review"), func() error {
					calls++
					if calls == stop {
						return sentinel
					}
					return nil
				})
				if closeErr := opened.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				body, readErr := os.ReadFile(filepath.Join(record.Dir, ContentFile))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if calls < stop {
					if err != nil || string(body) != "new review" {
						t.Fatalf("complete publication: %q %v", body, err)
					}
					break
				}
				if !errors.Is(err, sentinel) || string(body) != string(original) {
					t.Fatalf("validation %d did not restore original: %q %v", stop, body, err)
				}
				if artifacts := stageTransactionArtifacts(t, record.Dir); len(artifacts) != 0 {
					t.Fatalf("rollback left artifacts: %v", artifacts)
				}
			}
		})
	}
}

func TestJournaledReviewPublicationRetainsEvidenceOnNamespaceFailure(t *testing.T) {
	for _, failure := range []string{"existing-journal", "closed-parent", "missing-active", "wrong-active-identity", "detach-hook", "missing-next", "replaced-next", "exchange-hook", "replaced-old"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			base, nextName, suffix := "stage-reviewed", ".stage-reviewed.next-new", "journal"
			oldName := "." + base + ".old-" + suffix
			write(t, filepath.Join(dir, base, "review"), "prior")
			write(t, filepath.Join(dir, nextName, "review"), "prepared")
			parent, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			oldInfo, err := parent.Lstat(base)
			if err != nil {
				t.Fatal(err)
			}
			nextInfo, err := parent.Lstat(nextName)
			if err != nil {
				t.Fatal(err)
			}
			originalDetach, originalExchange := stageGenerationAfterDetach, stageGenerationAfterExchange
			defer func() { stageGenerationAfterDetach = originalDetach; stageGenerationAfterExchange = originalExchange }()
			sentinel := errors.New("review publication stopped")
			switch failure {
			case "existing-journal":
				write(t, filepath.Join(dir, oldName, "foreign"), "concurrent")
			case "closed-parent":
				_ = parent.Close()
			case "missing-active":
				if err := parent.Rename(base, base+".retained"); err != nil {
					t.Fatal(err)
				}
			case "wrong-active-identity":
				oldInfo = nextInfo
			case "detach-hook":
				stageGenerationAfterDetach = func(*os.Root, string, string) error { return sentinel }
			case "missing-next", "replaced-next":
				stageGenerationAfterDetach = func(root *os.Root, _ string, _ string) error {
					if err := root.Rename(nextName, nextName+".retained"); err != nil {
						return err
					}
					if failure == "replaced-next" {
						return root.Mkdir(nextName, 0o700)
					}
					return nil
				}
			case "exchange-hook":
				stageGenerationAfterExchange = func(*os.Root, string, string) error { return sentinel }
			case "replaced-old":
				stageGenerationAfterExchange = func(root *os.Root, _ string, old string) error {
					if err := root.Rename(old, old+".retained"); err != nil {
						return err
					}
					return root.Mkdir(old, 0o700)
				}
			}
			_, err = publishStageGenerationWithJournal(parent, nextName, base, suffix, nextInfo, oldInfo, nil)
			if err == nil {
				t.Fatal("damaged publication succeeded")
			}
			if failure == "detach-hook" || failure == "exchange-hook" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("hook failure lost: %v", err)
				}
			}
			found := map[string]bool{}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if body, err := os.ReadFile(filepath.Join(dir, entry.Name(), "review")); err == nil {
					found[string(body)] = true
				}
			}
			if !found["prior"] || !found["prepared"] {
				t.Fatalf("publication lost recoverable review generations: %v", found)
			}
		})
	}
}

func TestReviewTransactionPreparationRefusesIncompleteGenerations(t *testing.T) {
	project, home := initGitRepo(t), t.TempDir()
	write(t, filepath.Join(project, "source.md"), "native")
	report, err := Create(context.Background(), Options{Root: project, ThreadpointHome: home, Inputs: []Input{{Source: "source.md", Provider: "fixture", Kind: KindKnowledge}}, Classify: map[string]Scope{"source.md": ScopeProjectShared}})
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"nil-parent", "closed-parent", "occupied", "invalid-record", "source", "content", "manifest", "closed-transaction", "replaced-transaction"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			parent, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			record := report.Stages[0]
			original := atomicWriteRootStageFile
			defer func() { atomicWriteRootStageFile = original }()
			sentinel := errors.New("transaction write interrupted")
			atomicWriteRootStageFile = func(root *os.Root, name string, body []byte, mode os.FileMode, validate func(*os.Root, string) error) error {
				if name == map[string]string{"source": SourceFile, "content": ContentFile, "manifest": ManifestFile}[failure] {
					return sentinel
				}
				if err := original(root, name, body, mode, validate); err != nil {
					return err
				}
				if name == ManifestFile {
					if failure == "closed-transaction" {
						return root.Close()
					}
					if failure == "replaced-transaction" {
						if err := parent.Rename("pending", "retained"); err != nil {
							return err
						}
						return parent.Mkdir("pending", 0o700)
					}
				}
				return nil
			}
			selected := parent
			switch failure {
			case "nil-parent":
				selected = nil
			case "closed-parent":
				_ = parent.Close()
			case "occupied":
				write(t, filepath.Join(dir, "pending", "foreign"), "preserved")
			case "invalid-record":
				record.SchemaVersion = "invalid"
			}
			_, err = writeStageTransaction(selected, "pending", record, []byte("native"), []byte("reviewed"))
			if err == nil {
				t.Fatal("incomplete transaction succeeded")
			}
			if failure == "source" || failure == "content" || failure == "manifest" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("write error lost: %v", err)
				}
			}
			if failure == "occupied" {
				body, readErr := os.ReadFile(filepath.Join(dir, "pending", "foreign"))
				if readErr != nil || string(body) != "preserved" {
					t.Fatalf("foreign generation changed: %q %v", body, readErr)
				}
			} else if failure == "replaced-transaction" {
				body, readErr := os.ReadFile(filepath.Join(dir, "retained", ContentFile))
				if readErr != nil || string(body) != "reviewed" {
					t.Fatalf("retained review lost: %q %v", body, readErr)
				}
				if _, err := os.Stat(filepath.Join(dir, "pending")); err != nil {
					t.Fatalf("replacement removed: %v", err)
				}
			} else if _, err := os.Lstat(filepath.Join(dir, "pending")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incomplete transaction remains: %v", err)
			}
		})
	}
}
