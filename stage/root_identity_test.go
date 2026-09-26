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

	"github.com/threadgrid/threadpoint/safefs"
)

func TestPrivateStageMutationsRestorePriorReviewAfterProjectRetarget(t *testing.T) {
	for _, operation := range []string{"edit", "rebase", "discard"} {
		t.Run(operation, func(t *testing.T) {
			original := initGitRepo(t)
			write(t, filepath.Join(original, "notes.md"), "reviewed source\n")
			logical := filepath.Join(t.TempDir(), "selected-project")
			if err := os.Symlink(original, logical); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			home := t.TempDir()
			report, err := Create(context.Background(), Options{
				Root: logical, ThreadpointHome: home,
				Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
			})
			if err != nil || len(report.Stages) != 1 {
				t.Fatalf("stage report=%#v err=%v", report, err)
			}
			stage := report.Stages[0]
			priorRecord, priorSource, priorContent, err := ReadReview(logical, home, stage.ID)
			if err != nil {
				t.Fatal(err)
			}
			projectRoot, err := os.OpenRoot(logical)
			if err != nil {
				t.Fatal(err)
			}
			defer projectRoot.Close()
			homeRoot, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			defer homeRoot.Close()
			replacement := initGitRepo(t)
			write(t, filepath.Join(replacement, "notes.md"), "replacement source\n")

			if operation == "discard" {
				previous := discardStageAfterDetach
				discardStageAfterDetach = func() {
					discardStageAfterDetach = nil
					retargetSelectedProject(t, logical, replacement)
				}
				t.Cleanup(func() { discardStageAfterDetach = previous })
				err = DiscardFromRoots(logical, home, stage.ID, homeRoot, projectRoot)
			} else {
				previous := stageGenerationAfterExchange
				stageGenerationAfterExchange = func(*os.Root, string, string) error {
					stageGenerationAfterExchange = nil
					retargetSelectedProject(t, logical, replacement)
					return nil
				}
				t.Cleanup(func() { stageGenerationAfterExchange = previous })
				if operation == "edit" {
					err = ReplaceContentFromRoots(logical, home, stage.ID, []byte("edited review\n"), homeRoot, projectRoot)
				} else {
					err = RebaseMergedReviewFromRoots(logical, home, stage.ID, []byte("remote source\n"), []byte("merged review\n"), homeRoot, projectRoot)
				}
			}
			if err == nil || !strings.Contains(err.Error(), "selected") {
				t.Fatalf("retargeted %s error=%v", operation, err)
			}
			gotRecord, gotSource, gotContent, readErr := ReadReview(logical, home, stage.ID)
			if readErr != nil {
				t.Fatalf("prior review was not restored after %s: %v", operation, readErr)
			}
			if gotRecord != priorRecord || !bytes.Equal(gotSource, priorSource) || !bytes.Equal(gotContent, priorContent) {
				t.Fatalf("%s changed prior review after retarget: record=%#v source=%q content=%q", operation, gotRecord, gotSource, gotContent)
			}
		})
	}
}

func TestStageClassifiesPinnedSelectedWorktree(t *testing.T) {
	original := initGitRepo(t)
	write(t, filepath.Join(original, ".gitignore"), "notes.md\n")
	write(t, filepath.Join(original, "notes.md"), "private original bytes\n")
	git(t, original, "add", ".gitignore")
	git(t, original, "commit", "-m", "ignore private source")

	replacement := initGitRepo(t)
	write(t, filepath.Join(replacement, "notes.md"), "tracked replacement bytes\n")
	git(t, replacement, "add", "notes.md")
	git(t, replacement, "commit", "-m", "track replacement source")

	logical := filepath.Join(t.TempDir(), "selected-project")
	if err := os.Symlink(original, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	home := t.TempDir()
	stageBeforeClassify = func() {
		stageBeforeClassify = nil
		retargetSelectedProject(t, logical, replacement)
	}
	t.Cleanup(func() { stageBeforeClassify = nil })

	report, err := Create(context.Background(), Options{
		Root: logical, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err == nil || !strings.Contains(err.Error(), "selected logical root changed") {
		t.Fatalf("retargeted stage report=%#v error=%v", report, err)
	}
	if records, listErr := List(logical, home); listErr != nil || len(records) != 0 {
		t.Fatalf("retargeted classification published review state: records=%#v err=%v", records, listErr)
	}
}

func TestCommitRollsBackWhenSelectedProjectEntryIsRetargetedAfterPublication(t *testing.T) {
	original := initGitRepo(t)
	write(t, filepath.Join(original, "notes.md"), "reviewed source\n")
	logical := filepath.Join(t.TempDir(), "selected-project")
	if err := os.Symlink(original, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	home := t.TempDir()
	report, err := Create(context.Background(), Options{
		Root: logical, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report=%#v err=%v", report, err)
	}
	stage := report.Stages[0]
	canonical := filepath.Join(original, filepath.FromSlash(stage.Target))
	prior := []byte("prior canonical\n")
	write(t, canonical, string(prior))

	replacement := initGitRepo(t)
	write(t, filepath.Join(replacement, "notes.md"), "replacement source\n")
	commitBeforeStageCleanupValidation = func() {
		commitBeforeStageCleanupValidation = nil
		retargetSelectedProject(t, logical, replacement)
	}
	t.Cleanup(func() { commitBeforeStageCleanupValidation = nil })

	_, err = Commit(context.Background(), CommitOptions{Root: logical, ThreadpointHome: home, ID: stage.ID})
	if err == nil || !strings.Contains(err.Error(), "selected logical root changed") {
		t.Fatalf("retargeted commit error=%v", err)
	}
	if body, readErr := os.ReadFile(canonical); readErr != nil || !bytes.Equal(body, prior) {
		t.Fatalf("detached canonical was not rolled back: body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Stat(stage.Dir); statErr != nil {
		t.Fatalf("review stage removed after logical-root rollback: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(replacement, filepath.FromSlash(stage.Target))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replacement project was mutated: %v", statErr)
	}
}

func TestCommitRestoresDetachedStageWhenSelectedProjectEntryIsRetargetedAfterStageDetach(t *testing.T) {
	original := initGitRepo(t)
	write(t, filepath.Join(original, "notes.md"), "reviewed source\n")
	logical := filepath.Join(t.TempDir(), "selected-project")
	if err := os.Symlink(original, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	home := t.TempDir()
	report, err := Create(context.Background(), Options{
		Root: logical, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report=%#v err=%v", report, err)
	}
	stage := report.Stages[0]
	canonical := filepath.Join(original, filepath.FromSlash(stage.Target))
	prior := []byte("prior canonical\n")
	write(t, canonical, string(prior))
	replacement := initGitRepo(t)
	write(t, filepath.Join(replacement, "notes.md"), "replacement source\n")
	observedDetached := false
	commitStageCleanupAfterDetach = func() {
		commitStageCleanupAfterDetach = nil
		if _, statErr := os.Lstat(stage.Dir); errors.Is(statErr, os.ErrNotExist) {
			entries, readErr := os.ReadDir(filepath.Dir(stage.Dir))
			if readErr != nil {
				t.Fatal(readErr)
			}
			prefix := "." + filepath.Base(stage.Dir) + ".old-"
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
					observedDetached = true
					break
				}
			}
		}
		retargetSelectedProject(t, logical, replacement)
	}
	t.Cleanup(func() { commitStageCleanupAfterDetach = nil })

	_, err = Commit(context.Background(), CommitOptions{Root: logical, ThreadpointHome: home, ID: stage.ID})
	if err == nil || !strings.Contains(err.Error(), "selected logical root changed") {
		t.Fatalf("post-detach retargeted commit error=%v", err)
	}
	if !observedDetached {
		t.Fatal("cleanup guard did not run after reversible stage detachment")
	}
	if body, readErr := os.ReadFile(canonical); readErr != nil || !bytes.Equal(body, prior) {
		t.Fatalf("detached canonical was not rolled back: body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Stat(stage.Dir); statErr != nil {
		t.Fatalf("review stage was not restored after logical-root rollback: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(replacement, filepath.FromSlash(stage.Target))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replacement project was mutated: %v", statErr)
	}
}

func TestRetainedHomeRootSupportsCompleteReviewLifecycle(t *testing.T) {
	project := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(project, "notes.md"), "original guidance\n")
	write(t, filepath.Join(project, "discard.md"), "discard guidance\n")

	locks, err := safefs.AcquireLocks(home, []string{project}, "rooted-review-lifecycle-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := locks.Release(); err != nil {
			t.Errorf("release locks: %v", err)
		}
	})
	homeRoot, err := locks.BorrowProductRoot()
	if err != nil {
		t.Fatal(err)
	}

	options := Options{
		Root: project, ThreadpointHome: home,
		Inputs: []Input{{
			Provider: "fixture", Source: "notes.md", Kind: KindKnowledge,
			RequiredScope: ScopeProjectShared,
		}},
	}
	report, err := CreateFromRoot(context.Background(), options, homeRoot)
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("rooted stage report = %#v, err=%v", report, err)
	}
	record := report.Stages[0]

	// Restaging through the retained root must locate the immutable existing
	// generation instead of publishing a duplicate review directory.
	again, err := CreateFromRoot(context.Background(), options, homeRoot)
	if !errors.Is(err, ErrAlreadyStaged) || again != nil || !strings.Contains(err.Error(), record.ID) {
		t.Fatalf("rooted restage report = %#v, err=%v", again, err)
	}

	content, err := ReadContentFromRoot(project, home, record.ID, homeRoot)
	if err != nil || !bytes.Equal(content, []byte("original guidance\n")) {
		t.Fatalf("rooted content = %q, err=%v", content, err)
	}
	if err := ReplaceContentFromRoot(project, home, record.ID, []byte("reviewed guidance\n"), homeRoot); err != nil {
		t.Fatal(err)
	}
	if err := RebaseMergedReviewFromRoot(project, home, record.ID, []byte("original guidance\n"), []byte("merged guidance\n"), homeRoot); err != nil {
		t.Fatal(err)
	}
	current, source, merged, err := ReadReviewFromRoot(project, home, record.ID, homeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != record.ID || !bytes.Equal(source, []byte("original guidance\n")) || !bytes.Equal(merged, []byte("merged guidance\n")) {
		t.Fatalf("rooted review = %#v source=%q content=%q", current, source, merged)
	}

	committed, err := CommitFromRoot(context.Background(), CommitOptions{
		Root: project, ThreadpointHome: home, ID: record.ID,
	}, homeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if committed.ID != record.ID || committed.BackupRun == "" {
		t.Fatalf("rooted commit report = %#v", committed)
	}
	if body, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(record.Target))); err != nil || !bytes.Contains(body, []byte("merged guidance")) {
		t.Fatalf("committed canonical content = %q, err=%v", body, err)
	}
	if _, err := ReadContentFromRoot(project, home, record.ID, homeRoot); err == nil {
		t.Fatal("committed stage remains visible through retained root")
	}

	discarded, err := CreateFromRoot(context.Background(), Options{
		Root: project, ThreadpointHome: home,
		Inputs: []Input{{
			Provider: "fixture", Source: "discard.md", Kind: KindKnowledge,
			RequiredScope: ScopeProjectLocal,
		}},
	}, homeRoot)
	if err != nil || len(discarded.Stages) != 1 {
		t.Fatalf("discard stage report = %#v, err=%v", discarded, err)
	}
	if err := DiscardFromRoot(project, home, discarded.Stages[0].ID, homeRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadContentFromRoot(project, home, discarded.Stages[0].ID, homeRoot); err == nil {
		t.Fatal("discarded stage remains visible through retained root")
	}
}

func TestCommitReviewedFromRootUsesBorrowedCanonicalGeneration(t *testing.T) {
	project := initGitRepo(t)
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	input := CommitInput{
		Root: project, Scope: ScopeProjectShared, Provider: "fixture",
		Source: "native.md", Kind: KindKnowledge, ID: "stage-rooted-reviewed",
		Content: []byte("reviewed through retained project root\n"),
	}
	first, err := CommitReviewedFromRoot(context.Background(), input, root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Target == "" {
		t.Fatalf("rooted commit report = %#v", first)
	}
	// A bare marker has no durable commit binding at this lower-level API, so a
	// retry must fail visibly rather than treating marker text as publication
	// authority.
	if _, err := CommitReviewedFromRoot(context.Background(), input, root); err == nil || !strings.Contains(err.Error(), "unbound marker") {
		t.Fatalf("unbound rooted marker retry error = %v", err)
	}
	body, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(first.Target)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(body, []byte(`threadpoint:committed id="stage-rooted-reviewed"`)) != 1 {
		t.Fatalf("rooted retry duplicated committed content: %q", body)
	}
}

func TestStageFromRootRollsBackEarlierBatchItem(t *testing.T) {
	project := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(project, "a-valid.md"), "valid review\n")
	locks, err := safefs.AcquireLocks(home, []string{project}, "rooted-stage-batch-rollback-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := locks.Release(); err != nil {
			t.Errorf("release locks: %v", err)
		}
	}()
	homeRoot, err := locks.BorrowProductRoot()
	if err != nil {
		t.Fatal(err)
	}

	report, err := CreateFromRoot(context.Background(), Options{
		Root: project, ThreadpointHome: home,
		Inputs: []Input{
			{Provider: "fixture", Source: "a-valid.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared},
			{Provider: "fixture", Source: "z-missing.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared},
		},
	}, homeRoot)
	if err == nil || report != nil {
		t.Fatalf("rooted batch with missing source succeeded: report=%#v err=%v", report, err)
	}
	stages, err := List(project, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("rooted batch rollback retained stages: %#v", stages)
	}
}

func TestRemovePinnedStageContentsRecursesWithoutFollowingLinks(t *testing.T) {
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "sentinel")
	write(t, outsideFile, "outside\n")
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "nested", "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(rootPath, "top"), "top\n")
	write(t, filepath.Join(rootPath, "nested", "file"), "nested\n")
	write(t, filepath.Join(rootPath, "nested", "deeper", "file"), "deep\n")
	if err := os.Symlink(outside, filepath.Join(rootPath, "nested", "outside-link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := removePinnedRootContents(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rooted recursive cleanup entries=%v err=%v", entries, err)
	}
	if body, err := os.ReadFile(outsideFile); err != nil || string(body) != "outside\n" {
		t.Fatalf("rooted recursive cleanup followed symlink: %q err=%v", body, err)
	}
}

func TestRootedStageReadersRejectUntrustedStateShapes(t *testing.T) {
	project := initGitRepo(t)
	projectID := projectID(project)
	tests := []struct {
		name  string
		id    string
		setup func(t *testing.T, home string)
	}{
		{name: "invalid id", id: "not-a-stage", setup: func(*testing.T, string) {}},
		{
			name: "scope is a file", id: "stage-file-scope",
			setup: func(t *testing.T, home string) {
				t.Helper()
				write(t, filepath.Join(home, "imports", projectID, string(ScopeProjectShared)), "not a directory")
			},
		},
		{
			name: "stage is a file", id: "stage-file",
			setup: func(t *testing.T, home string) {
				t.Helper()
				write(t, filepath.Join(home, "imports", projectID, string(ScopeProjectShared), "stage-file"), "not a directory")
			},
		},
		{
			name: "manifest location mismatch", id: "stage-mismatch",
			setup: func(t *testing.T, home string) {
				t.Helper()
				dir := filepath.Join(home, "imports", projectID, string(ScopeProjectShared), "stage-mismatch")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(Record{ID: "stage-other", Scope: ScopeProjectShared, ProjectID: projectID})
				if err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(dir, ManifestFile), string(body))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			test.setup(t, home)
			homeRoot, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			defer homeRoot.Close()
			stage, err := openStageUnderRoot(project, home, test.id, homeRoot, false)
			if stage != nil {
				_ = stage.Close()
			}
			if err == nil {
				t.Fatal("untrusted stage state was accepted")
			}
		})
	}
}

func TestRootedStageMutationsRejectWholeHomeReplacement(t *testing.T) {
	operations := []struct {
		name          string
		requiresStage bool
		mutate        func(root, home string, stage Record, productRoot *os.Root) error
	}{
		{
			name: "stage",
			mutate: func(root, home string, _ Record, productRoot *os.Root) error {
				_, err := CreateFromRoot(context.Background(), Options{
					Root: root, ThreadpointHome: home,
					Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
				}, productRoot)
				return err
			},
		},
		{
			name:          "edit",
			requiresStage: true,
			mutate: func(root, home string, stage Record, productRoot *os.Root) error {
				return ReplaceContentFromRoot(root, home, stage.ID, []byte("replacement review\n"), productRoot)
			},
		},
		{
			name:          "mergetool rebase",
			requiresStage: true,
			mutate: func(root, home string, stage Record, productRoot *os.Root) error {
				return RebaseMergedReviewFromRoot(root, home, stage.ID, []byte("remote source\n"), []byte("merged review\n"), productRoot)
			},
		},
		{
			name:          "discard",
			requiresStage: true,
			mutate: func(root, home string, stage Record, productRoot *os.Root) error {
				return DiscardFromRoot(root, home, stage.ID, productRoot)
			},
		},
		{
			name:          "commit",
			requiresStage: true,
			mutate: func(root, home string, stage Record, productRoot *os.Root) error {
				_, err := CommitFromRoot(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}, productRoot)
				return err
			},
		},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			parent := t.TempDir()
			root := initGitRepo(t)
			home := filepath.Join(parent, "threadpoint-home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(root, "notes.md"), "review bytes\n")
			var stage Record
			if operation.requiresStage {
				report, err := Create(context.Background(), Options{
					Root: root, ThreadpointHome: home,
					Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
				})
				if err != nil || len(report.Stages) != 1 {
					t.Fatalf("stage report = %#v, err=%v", report, err)
				}
				stage = report.Stages[0]
			}
			locks, err := safefs.AcquireLocks(home, []string{root}, "rooted-stage-replacement-test")
			if err != nil {
				t.Fatal(err)
			}
			productRoot, err := locks.BorrowProductRoot()
			if err != nil {
				t.Fatal(err)
			}
			detached := home + ".detached"
			if err := os.Rename(home, detached); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(home, 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(home, "sentinel"), "replacement home\n")

			err = operation.mutate(root, home, stage, productRoot)
			if err == nil || !strings.Contains(err.Error(), "mutation root path no longer identifies") {
				t.Fatalf("whole-home replacement mutation error = %v", err)
			}
			if releaseErr := locks.Release(); releaseErr != nil {
				t.Fatalf("release through retained lock roots: %v", releaseErr)
			}
			if body, err := os.ReadFile(filepath.Join(home, "sentinel")); err != nil || string(body) != "replacement home\n" {
				t.Fatalf("replacement home sentinel = %q, err=%v", body, err)
			}
			if _, err := os.Lstat(filepath.Join(home, "imports")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("replacement home received stage state: %v", err)
			}
			if operation.requiresStage {
				detachedStage := filepath.Join(detached, "imports", stage.ProjectID, string(stage.Scope), stage.ID)
				if body, err := os.ReadFile(filepath.Join(detachedStage, ContentFile)); err != nil || string(body) != "review bytes\n" {
					t.Fatalf("detached stage was mutated: body=%q err=%v", body, err)
				}
				if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(stage.Target))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused rooted mutation published canonical content: %v", err)
				}
			}
		})
	}
}

func TestStageFromRootHandlesEmptyRetainedScope(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "review bytes\n")
	opts := Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	}
	first, err := Create(context.Background(), opts)
	if err != nil || len(first.Stages) != 1 {
		t.Fatalf("first stage report = %#v, err=%v", first, err)
	}
	if err := Discard(root, home, first.Stages[0].ID); err != nil {
		t.Fatal(err)
	}
	locks, err := safefs.AcquireLocks(home, []string{root}, "empty-scope-stage-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := locks.Release(); err != nil {
			t.Errorf("release locks: %v", err)
		}
	})
	productRoot, err := locks.BorrowProductRoot()
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateFromRoot(context.Background(), opts, productRoot)
	if err != nil || len(second.Stages) != 1 {
		t.Fatalf("rooted restage report = %#v, err=%v", second, err)
	}
}

func TestOpenMutationRootRejectsReplacementBetweenResolutionAndOpen(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	fired := false
	openMutationRootAfterResolve = func(selected string) {
		if fired || filepath.Clean(selected) != root {
			return
		}
		fired = true
		if err := os.Rename(root, root+".selected"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { openMutationRootAfterResolve = nil })
	opened, err := openMutationRoot(root)
	if opened != nil {
		_ = opened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "changed identity") || !fired {
		t.Fatalf("expected mutation-root replacement refusal, fired=%v err=%v", fired, err)
	}
}

func TestStageOperationsRejectInvalidRootAndHomeBeforeWriting(t *testing.T) {
	for _, operation := range []string{"preview", "create", "list", "edit", "discard", "commit", "read", "source"} {
		for _, invalid := range []string{"missing-root", "file-root", "empty-home", "filesystem-home"} {
			t.Run(operation+"/"+invalid, func(t *testing.T) {
				root, home := t.TempDir(), t.TempDir()
				originalHome := home
				switch invalid {
				case "missing-root":
					root = filepath.Join(root, "missing")
				case "file-root":
					root = filepath.Join(root, "file")
					write(t, root, "preserve")
				case "empty-home":
					home = ""
				case "filesystem-home":
					home = string(filepath.Separator)
				}
				var err error
				switch operation {
				case "preview":
					_, err = Preview(context.Background(), Options{Root: root, ThreadpointHome: home})
				case "create":
					_, err = Create(context.Background(), Options{Root: root, ThreadpointHome: home})
				case "list":
					_, err = List(root, home)
				case "edit":
					err = ReplaceContent(root, home, "stage", []byte("review"))
				case "discard":
					err = Discard(root, home, "stage")
				case "commit":
					_, err = Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: "stage"})
				case "read":
					_, err = ReadContent(root, home, "stage")
				case "source":
					_, err = ReadCurrentSource(root, home, "stage")
				}
				if err == nil {
					t.Fatal("invalid location accepted")
				}
				entries, readErr := os.ReadDir(originalHome)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("invalid operation wrote home: %v %v", entries, readErr)
				}
			})
		}
	}
}

func TestStageOperationsReturnErrorsWhenRelativeRootsCannotBeResolved(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"preview", func() error {
			_, err := Preview(context.Background(), Options{Root: ".", ThreadpointHome: home})
			return err
		}},
		{"create", func() error {
			_, err := Create(context.Background(), Options{Root: ".", ThreadpointHome: home})
			return err
		}},
		{"commit", func() error {
			_, err := Commit(context.Background(), CommitOptions{Root: ".", ThreadpointHome: home})
			return err
		}},
		{"relative-home", func() error {
			_, err := Preview(context.Background(), Options{Root: root, ThreadpointHome: "."})
			return err
		}},
		{"reviewed-commit", func() error { _, err := CommitReviewed(context.Background(), CommitInput{Root: "."}); return err }},
		{"source-classification", func() error { _, err := ClassifyProjectSourceFromRoot(".", "native.md", nil, nil); return err }},
		{"selected-root", func() error {
			retained, err := os.OpenRoot(root)
			if err != nil {
				return err
			}
			defer retained.Close()
			_, err = retainSelectedRoot(".", retained)
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); err == nil {
				t.Fatal("unresolvable relative location accepted")
			}
		})
	}
}

func TestRootedStageDirectoryCreationRefusesUnsafeComponents(t *testing.T) {
	for _, failure := range []string{"nil-root", "closed-root", "absolute", "escape", "dot", "nested-dot", "file", "symlink", "nested-file", "unreadable", "unwritable", "invalid-name"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			selected := root
			relative := "stages/project"
			switch failure {
			case "nil-root":
				selected = nil
			case "closed-root":
				_ = root.Close()
			case "absolute":
				relative = filepath.Join(dir, "outside")
			case "escape":
				relative = "../outside"
			case "dot":
				relative = "."
			case "nested-dot":
				relative = `stages\..\outside`
			case "file", "nested-file":
				name := "stages"
				if failure == "nested-file" {
					name = relative
				}
				write(t, filepath.Join(dir, name), "foreign")
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(dir, "stages")); err != nil {
					t.Fatal(err)
				}
			case "unreadable", "unwritable":
				mode := os.FileMode(0)
				if failure == "unwritable" {
					mode = 0o500
				}
				if err := os.Mkdir(filepath.Join(dir, "stages"), mode); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(filepath.Join(dir, "stages"), 0o700) }()
			case "invalid-name":
				relative = "stages/\x00bad"
			}
			opened, err := openOrCreateRootDirectory(selected, relative, 0o700)
			if opened != nil {
				_ = opened.Close()
			}
			if err == nil {
				t.Fatal("unsafe directory creation succeeded")
			}
			if failure == "file" || failure == "nested-file" {
				name := "stages"
				if failure == "nested-file" {
					name = relative
				}
				body, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(body) != "foreign" {
					t.Fatalf("foreign entry changed: %q %v", body, err)
				}
			}
		})
	}
}

func TestStageMutationsRefuseUnusableRetainedProject(t *testing.T) {
	for _, operation := range []string{"create", "commit"} {
		for _, failure := range []string{"closed", "wrong", "missing-logical"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				project, home := t.TempDir(), t.TempDir()
				retained, err := os.OpenRoot(project)
				if err != nil {
					t.Fatal(err)
				}
				defer retained.Close()
				logical := project
				switch failure {
				case "closed":
					_ = retained.Close()
				case "wrong":
					logical = t.TempDir()
				case "missing-logical":
					logical = filepath.Join(project, "missing")
				}
				if operation == "create" {
					_, err = stage(context.Background(), Options{Root: logical, ThreadpointHome: home}, nil, retained)
				} else {
					_, err = commit(context.Background(), CommitOptions{Root: logical, ThreadpointHome: home, ID: "stage-fixture"}, nil, retained)
				}
				if err == nil {
					t.Fatal("mutation accepted unusable retained project")
				}
				entries, err := os.ReadDir(home)
				if err != nil || len(entries) != 0 {
					t.Fatalf("rejected mutation wrote home: %v %v", entries, err)
				}
			})
		}
	}
}

func TestRetainedRootAPIsRefuseMissingAuthority(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	ctx := context.Background()
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"classify", func() error { _, err := ClassifyProjectSourceFromRoot(project, "memory.md", nil, nil); return err }},
		{"create", func() error {
			_, err := CreateFromRoot(ctx, Options{Root: project, ThreadpointHome: home}, nil)
			return err
		}},
		{"create-roots", func() error {
			_, err := CreateFromRoots(ctx, Options{Root: project, ThreadpointHome: home}, nil, nil)
			return err
		}},
		{"replace", func() error { return ReplaceContentFromRoot(project, home, "stage-fixture", nil, nil) }},
		{"replace-roots", func() error { return ReplaceContentFromRoots(project, home, "stage-fixture", nil, nil, nil) }},
		{"rebase", func() error { return RebaseMergedReviewFromRoot(project, home, "stage-fixture", nil, nil, nil) }},
		{"rebase-roots", func() error { return RebaseMergedReviewFromRoots(project, home, "stage-fixture", nil, nil, nil, nil) }},
		{"read-content", func() error { _, err := ReadContentFromRoot(project, home, "stage-fixture", nil); return err }},
		{"read-review", func() error { _, _, _, err := ReadReviewFromRoot(project, home, "stage-fixture", nil); return err }},
		{"read-source", func() error {
			_, err := ReadCurrentSourceFromRoots(project, home, "stage-fixture", nil, nil)
			return err
		}},
		{"discard", func() error { return DiscardFromRoot(project, home, "stage-fixture", nil) }},
		{"discard-roots", func() error { return DiscardFromRoots(project, home, "stage-fixture", nil, nil) }},
		{"commit", func() error {
			_, err := CommitFromRoot(ctx, CommitOptions{Root: project, ThreadpointHome: home, ID: "stage-fixture"}, nil)
			return err
		}},
		{"commit-roots", func() error {
			_, err := CommitFromRoots(ctx, CommitOptions{Root: project, ThreadpointHome: home, ID: "stage-fixture"}, nil, nil)
			return err
		}},
		{"commit-reviewed", func() error { _, err := CommitReviewedFromRoot(ctx, CommitInput{}, nil); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("missing authority accepted")
			}
			for _, path := range []string{project, home} {
				entries, err := os.ReadDir(path)
				if err != nil || len(entries) != 0 {
					t.Fatalf("missing authority caused ambient writes: %v %v", entries, err)
				}
			}
		})
	}
}

func TestStageAPIsRefuseUnresolvableRelativeSelections(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"relative-project", func() error { _, _, err := normalizeOptions("relative", home); return err }},
		{"relative-home", func() error { _, _, err := normalizeOptions(project, "relative"); return err }},
		{"default-project", func() error { _, _, err := normalizeOptions("", home); return err }},
		{"retained-selection", func() error { _, err := retainSelectedRoot("relative", root); return err }},
		{"classify", func() error { _, err := ClassifyProjectSource("relative", "memory.md", nil); return err }},
		{"classify-retained", func() error { _, err := ClassifyProjectSourceFromRoot("relative", "memory.md", nil, root); return err }},
		{"reviewed-commit", func() error {
			_, err := CommitReviewed(context.Background(), CommitInput{Root: "relative"})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("unresolvable selection accepted")
			}
		})
	}
	for _, path := range []string{project, home} {
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			t.Fatalf("invalid selection caused writes: %v %v", entries, err)
		}
	}
}

func TestReviewedCommitRefusesUnavailableRetainedProject(t *testing.T) {
	for _, failure := range []string{"closed-root", "wrong-root", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			project := t.TempDir()
			root, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			input := CommitInput{Root: project, Scope: ScopeProjectShared, Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, ID: "stage-fixture", Content: []byte("reviewed")}
			ctx := context.Background()
			switch failure {
			case "closed-root":
				_ = root.Close()
			case "wrong-root":
				input.Root = t.TempDir()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := CommitReviewedFromRoot(ctx, input, root); err == nil {
				t.Fatal("unavailable project accepted")
			}
			entries, err := os.ReadDir(project)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected commit wrote project: %v %v", entries, err)
			}
		})
	}
}

func TestSelectedRootGuardHandlesFilesystemRootWithoutParent(t *testing.T) {
	path := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	guard, err := retainSelectedRoot(path, root)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if guard.parent != nil {
		t.Fatal("filesystem root unexpectedly retained a distinct parent")
	}
	if err := guard.Verify(); err != nil {
		t.Fatalf("valid filesystem root refused: %v", err)
	}
	if _, err := retainSelectedRoot(path, nil); err == nil {
		t.Fatal("missing physical root accepted")
	}
	_ = root.Close()
	if err := guard.Verify(); err == nil {
		t.Fatal("closed filesystem root accepted")
	}
}
