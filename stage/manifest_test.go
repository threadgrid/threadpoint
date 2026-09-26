// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentStageManifestRequiresExactSchemaWithoutMutation(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "reviewed guidance\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectShared},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stage := report.Stages[0]
	manifestPath := filepath.Join(stage.Dir, ManifestFile)
	originalManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	originalContent, err := os.ReadFile(filepath.Join(stage.Dir, ContentFile))
	if err != nil {
		t.Fatal(err)
	}
	fields := []string{"schemaVersion", "id", "projectId", "root", "scope", "provider", "source", "kind", "target", "sourceSha256", "contentSha256", "createdAt", "dir"}
	tests := make([]struct {
		name   string
		mutate func(map[string]any)
	}, 0, len(fields)+2)
	for _, field := range fields {
		tests = append(tests, struct {
			name   string
			mutate func(map[string]any)
		}{name: "missing-" + field, mutate: func(manifest map[string]any) { delete(manifest, field) }})
	}
	tests = append(tests,
		struct {
			name   string
			mutate func(map[string]any)
		}{name: "unknown-field", mutate: func(manifest map[string]any) { manifest["futureField"] = true }},
		struct {
			name   string
			mutate func(map[string]any)
		}{name: "mismatched-dir", mutate: func(manifest map[string]any) { manifest["dir"] = filepath.Join(home, "other-stage") }},
	)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Cleanup(func() {
				// #nosec G703 -- manifestPath is the fixture-owned immutable stage path returned by Create.
				if err := os.WriteFile(manifestPath, originalManifest, 0o600); err != nil {
					t.Errorf("restore manifest: %v", err)
				}
			})
			var manifest map[string]any
			if err := json.Unmarshal(originalManifest, &manifest); err != nil {
				t.Fatal(err)
			}
			test.mutate(manifest)
			body, err := json.MarshalIndent(manifest, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			wantManifest := append([]byte(nil), body...)
			wantManifest = append(wantManifest, '\n')
			// #nosec G703 -- manifestPath is the fixture-owned immutable stage path returned by Create.
			if err := os.WriteFile(manifestPath, wantManifest, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := List(root, home); err == nil {
				t.Fatal("malformed stage was listed")
			}
			if _, _, _, err := ReadReview(root, home, stage.ID); err == nil {
				t.Fatal("malformed stage was read")
			}
			if err := ReplaceContent(root, home, stage.ID, []byte("edited\n")); err == nil {
				t.Fatal("malformed stage was edited")
			}
			if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err == nil {
				t.Fatal("malformed stage was committed")
			}
			if got, err := os.ReadFile(manifestPath); err != nil || !bytes.Equal(got, wantManifest) {
				t.Fatalf("rejected manifest was modified: body=%q err=%v", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(stage.Dir, ContentFile)); err != nil || !bytes.Equal(got, originalContent) {
				t.Fatalf("rejected content was modified: body=%q err=%v", got, err)
			}
		})
	}
}

func TestRebaseMergedReviewRestoresStageWhenManifestWriteFails(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "source\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, %v", report, err)
	}
	id := report.Stages[0].ID
	previousWrite := atomicWriteRootStageFile
	writes := 0
	atomicWriteRootStageFile = func(sr *os.Root, name string, body []byte, perm os.FileMode, validate func(*os.Root, string) error) error {
		writes++
		if writes == 3 {
			return errors.New("manifest write failed")
		}
		return previousWrite(sr, name, body, perm, validate)
	}
	defer func() { atomicWriteRootStageFile = previousWrite }()
	if err := RebaseMergedReview(root, home, id, []byte("remote source\n"), []byte("merged review\n")); err == nil || !strings.Contains(err.Error(), "manifest write failed") {
		t.Fatalf("rebase error = %v", err)
	}
	record, snapshot, review, err := ReadReview(root, home, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot) != "source\n" || string(review) != "source\n" || record.SourceSHA256 != digest([]byte("source\n")) {
		t.Fatalf("stage was not restored: %#v, snapshot=%q, review=%q", record, snapshot, review)
	}
}

func TestStageReadsStayOnRetainedHomeGeneration(t *testing.T) {
	for _, operation := range []string{"diff", "content", "review"} {
		t.Run(operation, func(t *testing.T) {
			root := initGitRepo(t)
			home := t.TempDir()
			write(t, filepath.Join(root, "source.md"), "original\n")
			report, err := Create(context.Background(), Options{
				Root: root, ThreadpointHome: home,
				Inputs: []Input{{Provider: "fixture", Source: "source.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
			})
			if err != nil || len(report.Stages) != 1 {
				t.Fatalf("stage report = %#v, err=%v", report, err)
			}
			stage := report.Stages[0]
			if err := ReplaceContent(root, home, stage.ID, []byte("reviewed\n")); err != nil {
				t.Fatal(err)
			}
			attackerHome := t.TempDir()
			relStage, err := filepath.Rel(home, stage.Dir)
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(attackerHome, relStage, SourceFile), "attacker source\n")
			write(t, filepath.Join(attackerHome, relStage, ContentFile), "attacker content\n")
			stageAfterOpen = func() {
				stageAfterOpen = nil
				replacePathWithSymlink(t, home, attackerHome)
			}
			t.Cleanup(func() { stageAfterOpen = nil })

			switch operation {
			case "diff":
				diff, err := Diff(root, home, stage.ID)
				if err != nil || !strings.Contains(diff, "-original") || !strings.Contains(diff, "+reviewed") || strings.Contains(diff, "attacker") {
					t.Fatalf("retained diff = %q, err=%v", diff, err)
				}
			case "content":
				body, err := ReadContent(root, home, stage.ID)
				if err != nil || string(body) != "reviewed\n" {
					t.Fatalf("retained content = %q, err=%v", body, err)
				}
			case "review":
				record, source, content, err := ReadReview(root, home, stage.ID)
				if err != nil || record.ID != stage.ID || string(source) != "original\n" || string(content) != "reviewed\n" {
					t.Fatalf("retained review = %#v source=%q content=%q err=%v", record, source, content, err)
				}
			}
		})
	}
}

func TestReadCurrentSourceRefusesReplacedSelectedProjectGeneration(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "source.md"), "original source\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "source.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, err=%v", report, err)
	}
	attackerRoot := initGitRepo(t)
	write(t, filepath.Join(attackerRoot, "source.md"), "attacker source\n")
	stageAfterOpen = func() {
		stageAfterOpen = nil
		replacePathWithSymlink(t, root, attackerRoot)
	}
	t.Cleanup(func() { stageAfterOpen = nil })
	body, err := ReadCurrentSource(root, home, report.Stages[0].ID)
	if err == nil || !strings.Contains(err.Error(), "selected logical root changed") {
		t.Fatalf("replaced selected source = %q, err=%v", body, err)
	}
}
