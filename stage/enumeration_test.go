// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicStageRecoveryBoundsParentEnumeration(t *testing.T) {
	parentPath := t.TempDir()
	for index := 0; index <= maxEnumeratedStageEntries; index++ {
		write(t, filepath.Join(parentPath, fmt.Sprintf("noise-%04d", index)), "noise\n")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := recoverStageTransactions(parent, "stage-0123456789abcdefabcd"); err == nil || !strings.Contains(err.Error(), "enumeration") {
		t.Fatalf("unbounded public recovery scan error = %v", err)
	}
}

func TestListIgnoresNestedPhantomStage(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "source\n")
	report, err := Create(t.Context(), Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report=%#v err=%v", report, err)
	}
	phantom := report.Stages[0]
	phantom.ID = "stage-0123456789abcdefabcd"
	phantom.Dir = filepath.Join(home, "imports", phantom.ProjectID, string(phantom.Scope), "nested", "deeper", phantom.ID)
	if err := os.MkdirAll(phantom.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(phantom)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(phantom.Dir, ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}

	records, err := List(root, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != report.Stages[0].ID {
		t.Fatalf("nested phantom entered canonical listing: %#v", records)
	}
}

func TestListBoundsAggregateNoncanonicalStageNoise(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	scopeDir := filepath.Join(home, "imports", projectID(root), string(ScopeProjectShared))
	if err := os.MkdirAll(scopeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4097; index++ {
		if err := os.Mkdir(filepath.Join(scopeDir, fmt.Sprintf("noise-%04d", index)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if records, err := List(root, home); err == nil {
		t.Fatalf("unbounded stage noise was accepted: %d records", len(records))
	}
}

func TestListRejectsManifestReplacementAfterBudgetStat(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "source\n")
	report, err := Create(t.Context(), Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report=%#v err=%v", report, err)
	}
	original := stageEnumerationAfterManifestStat
	stageEnumerationAfterManifestStat = func(stageRoot *os.Root) error {
		stageEnumerationAfterManifestStat = nil
		body, err := os.ReadFile(filepath.Join(report.Stages[0].Dir, ManifestFile))
		if err != nil {
			return err
		}
		body = append(body, bytes.Repeat([]byte(" "), 1<<20)...)
		return atomicWriteRootStageFile(stageRoot, ManifestFile, body, 0o600, nil)
	}
	t.Cleanup(func() { stageEnumerationAfterManifestStat = original })

	if records, err := List(root, home); err == nil || len(records) != 0 {
		t.Fatalf("manifest replacement bypassed aggregate accounting: records=%#v err=%v", records, err)
	}
}

func TestListTreatsMissingLazyHomeAsEmpty(t *testing.T) {
	root := initGitRepo(t)
	home := filepath.Join(t.TempDir(), "not-created")

	records, err := List(root, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("missing home listed stages: %#v", records)
	}
}

func TestStageListAndCommitRejectChangedSourceButKeepReviewState(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "original guidance")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectLocal},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, %v", report, err)
	}
	stages, err := List(root, home)
	if err != nil || len(stages) != 1 || stages[0].ID != report.Stages[0].ID {
		t.Fatalf("listed stages = %#v, %v", stages, err)
	}
	if err := ReplaceContent(root, home, stages[0].ID, []byte("reviewed guidance")); err != nil {
		t.Fatal(err)
	}
	diff, err := Diff(root, home, stages[0].ID)
	if err != nil || !strings.Contains(diff, "reviewed guidance") {
		t.Fatalf("review diff = %q, %v", diff, err)
	}
	write(t, filepath.Join(root, "notes.md"), "changed outside review")
	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stages[0].ID}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed source commit error = %v", err)
	}
	if _, err := os.Stat(stages[0].Dir); err != nil {
		t.Fatalf("review state should remain after refused commit: %v", err)
	}
}

func TestStageListRejectsDamagedCanonicalGenerations(t *testing.T) {
	for _, damage := range []string{"scope-file", "scope-symlink", "generation-file", "generation-unreadable", "manifest-missing", "manifest-directory", "manifest-invalid", "wrong-scope", "closed-root", "missing-root"} {
		t.Run(damage, func(t *testing.T) {
			project, home := initGitRepo(t), t.TempDir()
			write(t, filepath.Join(project, "source.md"), "native")
			report, err := Create(context.Background(), Options{Root: project, ThreadpointHome: home, Inputs: []Input{{Source: "source.md", Provider: "fixture", Kind: KindKnowledge}}, Classify: map[string]Scope{"source.md": ScopeProjectShared}})
			if err != nil {
				t.Fatal(err)
			}
			record := report.Stages[0]
			scopeDir := filepath.Dir(record.Dir)
			manifest := filepath.Join(record.Dir, ManifestFile)
			homeRoot, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			defer homeRoot.Close()
			switch damage {
			case "scope-file", "scope-symlink":
				if err := os.Rename(scopeDir, scopeDir+".retained"); err != nil {
					t.Fatal(err)
				}
				if damage == "scope-file" {
					write(t, scopeDir, "foreign")
				} else if err := os.Symlink(scopeDir+".retained", scopeDir); err != nil {
					t.Fatal(err)
				}
			case "generation-file":
				if err := os.Rename(record.Dir, record.Dir+".retained"); err != nil {
					t.Fatal(err)
				}
				write(t, record.Dir, "foreign")
			case "generation-unreadable":
				if err := os.Chmod(record.Dir, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(record.Dir, 0o700) }()
				if f, err := os.Open(record.Dir); err == nil {
					_ = f.Close()
					t.Skip("directory permissions are not enforced")
				}
			case "manifest-missing":
				if err := os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
			case "manifest-directory":
				if err := os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(manifest, 0o700); err != nil {
					t.Fatal(err)
				}
			case "manifest-invalid":
				write(t, manifest, "invalid JSON")
			case "wrong-scope":
				record.Scope = ScopeProjectLocal
				body, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				write(t, manifest, string(body))
			case "closed-root":
				_ = homeRoot.Close()
			case "missing-root":
				homeRoot = nil
			}
			records, err := listStagesFromHomeRoot(homeRoot, home, project, record.ProjectID)
			if err == nil || len(records) != 0 {
				t.Fatalf("damaged stage listed: %v %v", records, err)
			}
			if body, err := os.ReadFile(filepath.Join(project, "source.md")); err != nil || string(body) != "native" {
				t.Fatalf("listing changed source: %q %v", body, err)
			}
		})
	}
}

func TestStageEnumerationRefusesUnavailableOrExhaustedAuthority(t *testing.T) {
	for _, operation := range []string{"scope", "directory"} {
		for _, failure := range []string{"nil-root", "nil-budget", "closed-root", "exhausted-entries", "exhausted-paths"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				write(t, filepath.Join(dir, "stage-fixture"), "foreign")
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				selected := root
				budget := &stageEnumerationBudget{}
				switch failure {
				case "nil-root":
					selected = nil
				case "nil-budget":
					budget = nil
				case "closed-root":
					_ = root.Close()
				case "exhausted-entries":
					budget.entries = maxEnumeratedStageEntries + 1
				case "exhausted-paths":
					budget.pathBytes = maxEnumeratedStagePathBytes
				}
				if operation == "scope" {
					_, err = listStagesInScopeRoot(selected, dir, dir, "fixture", ScopeProjectShared, budget)
				} else {
					_, err = readStageDirectoryEntries(selected, ".", 0, budget)
				}
				if err == nil {
					t.Fatal("enumeration accepted invalid authority or exceeded budget")
				}
				if body, err := os.ReadFile(filepath.Join(dir, "stage-fixture")); err != nil || string(body) != "foreign" {
					t.Fatalf("enumeration changed entry: %q %v", body, err)
				}
			})
		}
	}
}

func TestStagePathChargeRequiresEnumerationBudget(t *testing.T) {
	var budget *stageEnumerationBudget
	if err := budget.chargePath("stage-fixture/manifest.json"); err == nil {
		t.Fatal("unbounded stage path accepted")
	}
}
