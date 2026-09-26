// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJournaledStagePublicationRefusesExistingJournalName(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"stage-id", ".stage-id.next-token", ".stage-id.old-token"} {
		if err := root.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	nextInfo, err := root.Lstat(".stage-id.next-token")
	if err != nil {
		t.Fatal(err)
	}
	oldInfo, err := root.Lstat("stage-id")
	if err != nil {
		t.Fatal(err)
	}
	retained, err := publishStageGenerationWithJournal(root, ".stage-id.next-token", "stage-id", "token", nextInfo, oldInfo, nil)
	if err == nil || retained || !strings.Contains(err.Error(), "journal already exists") {
		t.Fatalf("existing stage journal publication retained=%v err=%v", retained, err)
	}
}

func TestStageRollsBackEarlierStagesWhenBatchFails(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "a.md"), "valid")
	_, err := Create(context.Background(), Options{
		Root:            root,
		ThreadpointHome: home,
		Classify: map[string]Scope{
			"a.md":         ScopeProjectShared,
			"z-missing.md": ScopeProjectShared,
		},
		Inputs: []Input{
			{Provider: "fixture", Source: "a.md", Kind: KindKnowledge},
			{Provider: "fixture", Source: "z-missing.md", Kind: KindKnowledge},
		},
	})
	if err == nil {
		t.Fatal("expected second stage input to fail")
	}
	records, listErr := List(root, home)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(records) != 0 {
		t.Fatalf("failed batch retained partial stages: %#v", records)
	}
}

func TestStageRollsBackGenerationPublishedBeforeParentSyncFailure(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "review bytes\n")
	originalSync := syncStageParent
	t.Cleanup(func() { syncStageParent = originalSync })
	syncCalls := 0
	syncStageParent = func(path string) error {
		syncCalls++
		if syncCalls == 2 {
			return errors.New("forced post-rename sync failure")
		}
		return originalSync(path)
	}

	_, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectShared},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err == nil || !strings.Contains(err.Error(), "forced post-rename sync failure") {
		t.Fatalf("stage error = %v", err)
	}
	records, listErr := List(root, home)
	if listErr != nil || len(records) != 0 {
		t.Fatalf("post-rename failure retained visible stage: %#v, %v", records, listErr)
	}
}

func TestStageStoresReviewCopiesOutsideProjectAndRequiresClassification(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "native guidance")

	_, err := Create(context.Background(), Options{
		Root:            root,
		ThreadpointHome: home,
		Inputs: []Input{{
			Provider: "fixture",
			Source:   "notes.md",
			Kind:     KindKnowledge,
		}},
	})
	if !errors.Is(err, ErrClassificationRequired) {
		t.Fatalf("Create error = %v, want ErrClassificationRequired", err)
	}

	report, err := Create(context.Background(), Options{
		Root:            root,
		ThreadpointHome: home,
		Classify:        map[string]Scope{"notes.md": ScopeProjectLocal},
		Inputs: []Input{{
			Provider: "fixture",
			Source:   "notes.md",
			Kind:     KindKnowledge,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Stages) != 1 || report.Stages[0].Scope != ScopeProjectLocal {
		t.Fatalf("stages = %#v", report.Stages)
	}
	stage := report.Stages[0]
	if !strings.HasPrefix(stage.Dir, filepath.Join(home, "imports")+string(filepath.Separator)) {
		t.Fatalf("stage dir = %s, want under threadpoint home", stage.Dir)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "imports")); !os.IsNotExist(err) {
		t.Fatalf("stage wrote project review state: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(stage.Dir, ContentFile)); err != nil || string(body) != "native guidance" {
		t.Fatalf("staged content = %q, err=%v", body, err)
	}
}

func TestRebaseMergedReviewRejectsInvalidAndUnwritableStages(t *testing.T) {
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
	if err := RebaseMergedReview(root, home, "stage-missing", []byte("source\n"), []byte("review\n")); err == nil {
		t.Fatal("missing stage rebase unexpectedly succeeded")
	}
	if err := RebaseMergedReview(root, home, id, nil, []byte("review\n")); err == nil {
		t.Fatal("empty source rebase unexpectedly succeeded")
	}
	if err := RebaseMergedReview(root, home, id, []byte("source\n"), nil); err == nil {
		t.Fatal("empty review rebase unexpectedly succeeded")
	}
	dir := report.Stages[0].Dir
	if err := os.Remove(filepath.Join(dir, SourceFile)); err != nil {
		t.Fatal(err)
	}
	if err := RebaseMergedReview(root, home, id, []byte("source\n"), []byte("review\n")); err == nil {
		t.Fatal("missing source snapshot rebase unexpectedly succeeded")
	}
	write(t, filepath.Join(dir, SourceFile), "source\n")
	if err := os.Remove(filepath.Join(dir, ContentFile)); err != nil {
		t.Fatal(err)
	}
	if err := RebaseMergedReview(root, home, id, []byte("source\n"), []byte("review\n")); err == nil {
		t.Fatal("missing review copy rebase unexpectedly succeeded")
	}
	write(t, filepath.Join(dir, ContentFile), "source\n")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore stage permissions: %v", err)
		}
	}()
	if err := RebaseMergedReview(root, home, id, []byte("source\n"), []byte("review\n")); err == nil {
		t.Fatal("unwritable stage rebase unexpectedly succeeded")
	}
}

func TestClassifyProjectSourceUsesGitStateAndExplicitUntrackedScope(t *testing.T) {
	root := initGitRepo(t)
	write(t, filepath.Join(root, "tracked.md"), "shared\n")
	write(t, filepath.Join(root, ".gitignore"), "ignored.md\n")
	write(t, filepath.Join(root, "ignored.md"), "local\n")
	write(t, filepath.Join(root, "untracked.md"), "reviewed\n")
	git(t, root, "add", "tracked.md", ".gitignore")
	git(t, root, "commit", "-m", "classify sources")

	tests := []struct {
		source   string
		explicit map[string]Scope
		want     Scope
	}{
		{source: "tracked.md", want: ScopeProjectShared},
		{source: "ignored.md", want: ScopeProjectLocal},
		{source: "untracked.md", explicit: map[string]Scope{"untracked.md": ScopeProjectLocal}, want: ScopeProjectLocal},
		{source: filepath.Join(root, "tracked.md"), want: ScopeProjectShared},
		{source: filepath.Join(root, "untracked.md"), explicit: map[string]Scope{"untracked.md": ScopeProjectShared}, want: ScopeProjectShared},
	}
	for _, test := range tests {
		scope, err := ClassifyProjectSource(root, test.source, test.explicit)
		if err != nil || scope != test.want {
			t.Fatalf("ClassifyProjectSource(%q) = %q, %v; want %q", test.source, scope, err, test.want)
		}
	}
	if _, err := ClassifyProjectSource(root, filepath.Join(filepath.Dir(root), "outside.md"), nil); err == nil {
		t.Fatal("absolute source outside the project should be refused")
	}
	if _, err := ClassifyProjectSource(root, "../outside.md", nil); err == nil {
		t.Fatal("source outside the project should be refused")
	}
}

func TestNativeArtifactIgnoreWarningUsesSelectedGitWorktree(t *testing.T) {
	root := initGitRepo(t)
	write(t, filepath.Join(root, ".gitignore"), "ignored.md\n")
	write(t, filepath.Join(root, "ignored.md"), "private\n")
	write(t, filepath.Join(root, "visible.md"), "shared\n")
	git(t, root, "add", ".gitignore", "visible.md")
	git(t, root, "commit", "-m", "ignore native source")

	warning, err := NativeArtifactIgnoreWarning(root, "ignored.md")
	if err != nil || !strings.Contains(warning, "ignored by Git") || !strings.Contains(warning, "ignored.md") {
		t.Fatalf("ignored native warning = %q, %v", warning, err)
	}
	warning, err = NativeArtifactIgnoreWarning(root, "visible.md")
	if err != nil || warning != "" {
		t.Fatalf("visible native warning = %q, %v", warning, err)
	}
	if _, err := NativeArtifactIgnoreWarning("", "ignored.md"); err == nil {
		t.Fatal("empty project root should fail")
	}
	if _, err := NativeArtifactIgnoreWarning(root, "../outside.md"); err == nil {
		t.Fatal("source outside selected project should fail")
	}
	if _, err := NativeArtifactIgnoreWarning(filepath.Join(root, "missing"), "ignored.md"); err == nil {
		t.Fatal("missing Git worktree should fail")
	}
}

func TestDeclaredLocalScopeOverridesGitAndRejectsConflictingClassification(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "CLAUDE.local.md"), "private guidance")
	git(t, root, "add", "CLAUDE.local.md")
	git(t, root, "commit", "-m", "tracked local guidance")
	input := Input{Provider: "claude", Source: "CLAUDE.local.md", Kind: KindInstruction, RequiredScope: ScopeProjectLocal}

	report, err := Preview(context.Background(), Options{Root: root, ThreadpointHome: home, Inputs: []Input{input}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Stages) != 1 || report.Stages[0].Scope != ScopeProjectLocal || report.Stages[0].Target != "AGENTS.local.md" {
		t.Fatalf("unexpected declared-local preview: %#v", report)
	}

	_, err = Preview(context.Background(), Options{
		Root: root, ThreadpointHome: home, Inputs: []Input{input},
		Classify: map[string]Scope{"CLAUDE.local.md": ScopeProjectShared},
	})
	if !errors.Is(err, ErrClassificationRequired) {
		t.Fatalf("conflicting classification error = %v, want ErrClassificationRequired", err)
	}
}

func TestIgnoredNativeSourceStagesLocallyWithClassificationAdvisory(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, ".codex", "AGENTS.md"), "native guidance")
	write(t, filepath.Join(root, ".gitignore"), "/.codex/\n")
	git(t, root, "add", ".gitignore")
	git(t, root, "commit", "-m", "ignored native artifact")
	input := Input{Provider: "codex", Source: ".codex/AGENTS.md", Kind: KindInstruction}

	preview, err := Preview(context.Background(), Options{Root: root, ThreadpointHome: home, Inputs: []Input{input}})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Stages) != 1 || preview.Stages[0].Scope != ScopeProjectLocal {
		t.Fatalf("ignored native preview = %#v", preview)
	}
	if len(preview.Warnings) != 1 || !strings.Contains(preview.Warnings[0], "does not add or recommend native artifact ignore rules") {
		t.Fatalf("native-ignore preview warnings = %#v", preview.Warnings)
	}

	report, err := Create(context.Background(), Options{Root: root, ThreadpointHome: home, Inputs: []Input{input}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 1 || report.Warnings[0] != preview.Warnings[0] {
		t.Fatalf("native-ignore stage warnings = %#v, preview = %#v", report.Warnings, preview.Warnings)
	}
}

func TestStageLeavesUnrelatedAgentsImportsTreeUntouched(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "source.md"), "source")
	imports := filepath.Join(root, ".agents", "imports", "unrelated.md")
	write(t, imports, "unrelated")

	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Classify: map[string]Scope{"source.md": ScopeProjectShared},
		Inputs:   []Input{{Provider: "fixture", Source: "source.md", Kind: KindKnowledge}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, err = %v", report, err)
	}
	if body, err := os.ReadFile(imports); err != nil || string(body) != "unrelated" {
		t.Fatalf("unrelated imports tree changed: %q, err = %v", body, err)
	}
}

func TestPreviewRejectsInvalidSourcesWithoutCreatingStages(t *testing.T) {
	for _, invalid := range []string{"empty", "escape", "missing", "directory", "symlink", "unclassified", "invalid-scope", "cancelled", "already-staged", "damaged-home"} {
		t.Run(invalid, func(t *testing.T) {
			root, home := initGitRepo(t), t.TempDir()
			write(t, filepath.Join(root, "source.md"), "native")
			opts := Options{Root: root, ThreadpointHome: home, Inputs: []Input{{Provider: "fixture", Source: "source.md", Kind: KindKnowledge}}, Classify: map[string]Scope{"source.md": ScopeProjectShared}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch invalid {
			case "empty":
				opts.Inputs[0].Source = ""
			case "escape":
				opts.Inputs[0].Source = "../outside"
			case "missing":
				if err := os.Remove(filepath.Join(root, "source.md")); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(filepath.Join(root, "source.md")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "source.md"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(filepath.Join(root, "source.md")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("elsewhere", filepath.Join(root, "source.md")); err != nil {
					t.Fatal(err)
				}
			case "unclassified":
				opts.Classify = nil
			case "invalid-scope":
				opts.Classify["source.md"] = Scope("invalid")
			case "cancelled":
				cancel()
			case "already-staged":
				if _, err := Create(ctx, opts); err != nil {
					t.Fatal(err)
				}
			case "damaged-home":
				home = filepath.Join(home, "file")
				write(t, home, "preserve")
				opts.ThreadpointHome = home
			}
			report, err := Preview(ctx, opts)
			if err == nil || report != nil {
				t.Fatalf("invalid preview returned %+v, %v", report, err)
			}
			if invalid != "already-staged" && invalid != "damaged-home" {
				entries, err := os.ReadDir(home)
				if err != nil || len(entries) != 0 {
					t.Fatalf("preview wrote state: %v %v", entries, err)
				}
			}
		})
	}
}

func TestStageGenerationWriteFailuresPreservePublicationContract(t *testing.T) {
	for _, failure := range []string{"source", "content", "manifest", "before-publish-sync", "after-publish-sync", "invalid-record", "closed-parent", "missing-parent", "missing-sync", "existing-stage"} {
		t.Run(failure, func(t *testing.T) {
			project, home := initGitRepo(t), t.TempDir()
			write(t, filepath.Join(project, "source.md"), "native")
			report, err := Create(context.Background(), Options{Root: project, ThreadpointHome: home, Inputs: []Input{{Source: "source.md", Provider: "fixture", Kind: KindKnowledge}}, Classify: map[string]Scope{"source.md": ScopeProjectShared}})
			if err != nil {
				t.Fatal(err)
			}
			record := report.Stages[0]
			dir := t.TempDir()
			parent, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			original := atomicWriteRootStageFile
			t.Cleanup(func() { atomicWriteRootStageFile = original })
			sentinel := errors.New("stage write failed")
			atomicWriteRootStageFile = func(root *os.Root, name string, body []byte, mode os.FileMode, validate func(*os.Root, string) error) error {
				if name == map[string]string{"source": SourceFile, "content": ContentFile, "manifest": ManifestFile}[failure] {
					return sentinel
				}
				return original(root, name, body, mode, validate)
			}
			syncCalls := 0
			syncParent := func() error {
				syncCalls++
				if failure == "before-publish-sync" && syncCalls == 1 || failure == "after-publish-sync" && syncCalls == 2 {
					return sentinel
				}
				return nil
			}
			switch failure {
			case "invalid-record":
				record.SchemaVersion = "invalid"
			case "closed-parent":
				_ = parent.Close()
			case "missing-parent":
				parent = nil
			case "missing-sync":
				syncParent = nil
			case "existing-stage":
				if err := os.Mkdir(filepath.Join(dir, record.ID), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			published, err := writeStageToParent(parent, record.ID, record, []byte("native"), syncParent)
			if err == nil {
				t.Fatal("failed generation write succeeded")
			}
			if failure == "after-publish-sync" {
				if published == nil || !errors.Is(err, sentinel) {
					t.Fatalf("published generation lost: %v %v", published, err)
				}
				for _, name := range []string{SourceFile, ContentFile, ManifestFile} {
					if data, err := os.ReadFile(filepath.Join(dir, record.ID, name)); err != nil || len(data) == 0 {
						t.Fatalf("partial generation %s: %q %v", name, data, err)
					}
				}
			} else if failure != "existing-stage" {
				entries, readErr := os.ReadDir(dir)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("failed write left residue: %v %v", entries, readErr)
				}
			}
		})
	}
}
