// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

func TestCommitRollsBackAndTombstonesWhenBackupFinalizeFails(t *testing.T) {
	if !safefs.RenameRootExchangeSupported() {
		t.Skip("native atomic exchange is unavailable")
	}
	root := initGitRepo(t)
	home := t.TempDir()
	source := filepath.Join(root, "notes.md")
	const prior = "prior canonical\n"
	write(t, source, "project shared source\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectShared},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage: %v %#v", err, report)
	}
	stage := report.Stages[0]
	canonical := filepath.Join(root, filepath.FromSlash(stage.Target))
	write(t, canonical, prior)
	commitBackupBeforeFinalize = func() error {
		commitBackupBeforeFinalize = nil
		return errors.New("injected pre-finalize failure")
	}
	t.Cleanup(func() { commitBackupBeforeFinalize = nil })

	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err == nil || !strings.Contains(err.Error(), "injected pre-finalize failure") {
		t.Fatalf("expected backup-finalize transaction failure, got %v", err)
	}
	if body, readErr := os.ReadFile(canonical); readErr != nil || string(body) != prior {
		t.Fatalf("canonical was not rolled back after backup failure: body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Stat(stage.Dir); statErr != nil {
		t.Fatalf("stage was removed after rolled-back commit: %v", statErr)
	}
	if runs, listErr := backup.ListRunsWithOverride(root, "", home, ""); listErr != nil || len(runs) != 0 {
		t.Fatalf("failed transaction became visible to prune/restore: runs=%+v err=%v", runs, listErr)
	}
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, ThreadpointHome: home, Operation: backup.PendingCommitOperation, RunID: "commit-" + stage.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	manifest, err := backup.ReadManifest(filepath.Join(store.RunDir, backup.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Operation != backup.AbortedCommitOperation || manifest.CommitTransaction == nil || manifest.CommitTransaction.State != backup.CommitTransactionAborted {
		t.Fatalf("failed transaction was not tombstoned: %+v", manifest)
	}
}

func TestCommitKeepsStageWhenCanonicalContainsUnboundMatchingMarker(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "reviewed provider guidance\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Classify: map[string]Scope{"notes.md": ScopeProjectShared},
		Inputs:   []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage: %v %#v", err, report)
	}
	stage := report.Stages[0]
	canonical := filepath.Join(root, filepath.FromSlash(stage.Target))
	foreign := []byte("unrelated canonical text\n\n<!-- threadpoint:committed id=\"" + stage.ID + "\" -->\n")
	write(t, canonical, string(foreign))
	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err == nil || !strings.Contains(err.Error(), "unbound marker") {
		t.Fatalf("unbound canonical marker error = %v", err)
	}
	body, err := os.ReadFile(canonical)
	if err != nil || !bytes.Equal(body, foreign) {
		t.Fatalf("canonical collision changed: body=%q err=%v", body, err)
	}
	if _, err := os.Stat(stage.Dir); err != nil {
		t.Fatalf("review stage was not retained: %v", err)
	}
}

func TestCommitKeepsCanonicalAfterDurableBackupFinalize(t *testing.T) {
	if !safefs.RenameRootExchangeSupported() {
		t.Skip("native atomic exchange is unavailable")
	}
	root := initGitRepo(t)
	home := t.TempDir()
	source := filepath.Join(root, "notes.md")
	write(t, source, "project shared source\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectShared},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage: %v %#v", err, report)
	}
	stage := report.Stages[0]
	canonical := filepath.Join(root, filepath.FromSlash(stage.Target))
	write(t, canonical, "prior canonical\n")
	called := false
	commitBackupAfterFinalize = func() {
		called = true
		commitBackupAfterFinalize = nil
	}
	t.Cleanup(func() { commitBackupAfterFinalize = nil })

	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("post-finalize observation was not reached")
	}
	body, err := os.ReadFile(canonical)
	if err != nil || !strings.Contains(string(body), `threadpoint:committed id="`+stage.ID+`"`) {
		t.Fatalf("durably finalized canonical generation was not retained: body=%q err=%v", body, err)
	}
	runs, err := backup.ListRunsWithOverride(root, "", home, "")
	if err != nil || len(runs) != 1 || runs[0].Operation != backup.CommittedCommitOperation {
		t.Fatalf("durably finalized backup is not visible: runs=%+v err=%v", runs, err)
	}
}

func TestCommitRestoresCanonicalAndKeepsStageWhenSourceChangesAfterPublish(t *testing.T) {
	if !safefs.RenameRootExchangeSupported() {
		t.Skip("atomic rooted exchange is unsupported on this platform")
	}
	root := initGitRepo(t)
	home := t.TempDir()
	sourcePath := filepath.Join(root, "notes.md")
	write(t, sourcePath, "reviewed source\n")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectShared},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, %v", report, err)
	}
	stage := report.Stages[0]
	priorCanonical := []byte("prior canonical\n")
	targetPath := filepath.Join(root, filepath.FromSlash(stage.Target))
	write(t, targetPath, string(priorCanonical))
	commitBeforePostPublishValidation = func() {
		if err := os.WriteFile(sourcePath, []byte("changed after canonical publication\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { commitBeforePostPublishValidation = nil }()

	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("post-publication source change error = %v", err)
	}
	if body, err := os.ReadFile(targetPath); err != nil || !bytes.Equal(body, priorCanonical) {
		t.Fatalf("prior canonical generation was not restored: body=%q err=%v", body, err)
	}
	if _, _, content, err := ReadReview(root, home, stage.ID); err != nil || string(content) != "reviewed source\n" {
		t.Fatalf("stage was not retained after rollback: content=%q err=%v", content, err)
	}
}

func TestCommitRejectsContentOutsideManifestGeneration(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "original guidance")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home, Classify: map[string]Scope{"notes.md": ScopeProjectLocal},
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stage := report.Stages[0]
	if err := os.WriteFile(filepath.Join(stage.Dir, ContentFile), []byte("unmanifested edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err == nil || !strings.Contains(err.Error(), "content generation") {
		t.Fatalf("expected content-generation refusal, got %v", err)
	}
}

func TestCommitReviewedRejectsCanonicalGenerationOverReadLimit(t *testing.T) {
	root := initGitRepo(t)
	target, err := TargetFor(ScopeProjectShared, "fixture", "notes.md", KindKnowledge)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(root, filepath.FromSlash(target))
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatal(err)
	}
	prior := bytes.Repeat([]byte("p"), maxCanonicalBytes/2+1)
	if err := os.WriteFile(targetPath, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	input := CommitInput{
		Root: root, Scope: ScopeProjectShared, Provider: "fixture", Source: "notes.md",
		Kind: KindKnowledge, ID: "stage-canonical-limit",
		Content: bytes.Repeat([]byte("r"), maxCanonicalBytes/2),
	}
	if _, err := CommitReviewed(context.Background(), input); err == nil || !strings.Contains(err.Error(), "canonical generation") || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized canonical generation error = %v", err)
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, prior) {
		t.Fatal("oversized commit changed the prior canonical generation")
	}
}

func TestCommitReviewedRefusesNonRegularCanonicalTarget(t *testing.T) {
	root := initGitRepo(t)
	target, err := TargetFor(ScopeProjectShared, "fixture", "notes.md", KindKnowledge)
	if err != nil {
		t.Fatal(err)
	}
	// A directory (or any non-regular object) occupying the canonical target must
	// fail closed instead of being replaced through an ambient pathname.
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(target)), 0o755); err != nil {
		t.Fatal(err)
	}
	input := CommitInput{Root: root, Scope: ScopeProjectShared, Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, ID: "stage-nonregular", Content: []byte("reviewed block\n")}
	_, err = CommitReviewed(context.Background(), input)
	if err == nil {
		t.Fatal("expected CommitReviewed to refuse a non-regular canonical target")
	}
	if !strings.Contains(err.Error(), "non-regular canonical target") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRebaseMergedReviewUpdatesProvenanceAndCommitGuard(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, ".gitignore"), "/AGENTS.local.md\n/.agents.local/\n")
	write(t, filepath.Join(root, "notes.md"), "base source\n")
	git(t, root, "add", ".gitignore")
	git(t, root, "commit", "-m", "local target ignores")
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectLocal}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, %v", report, err)
	}
	id := report.Stages[0].ID
	remote := []byte("remote source\n")
	merged := []byte("merged review\n")
	write(t, filepath.Join(root, "notes.md"), string(remote))
	if err := RebaseMergedReview(root, home, id, remote, merged); err != nil {
		t.Fatal(err)
	}
	record, snapshot, review, err := ReadReview(root, home, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot) != string(remote) || string(review) != string(merged) || record.SourceSHA256 != digest(remote) {
		t.Fatalf("rebased stage = %#v, snapshot=%q, review=%q", record, snapshot, review)
	}
	write(t, filepath.Join(root, "notes.md"), "later source change\n")
	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: id}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("commit after source change error = %v", err)
	}
	write(t, filepath.Join(root, "notes.md"), string(remote))
	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: id}); err != nil {
		t.Fatalf("commit rebased review: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(report.Stages[0].Target)))
	if err != nil || !strings.Contains(string(body), "merged review") {
		t.Fatalf("committed rebased review = %q, %v", body, err)
	}
}

func TestCommitRejectsReplacedProjectAndKeepsRetainedStage(t *testing.T) {
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
	stage := report.Stages[0]
	if err := ReplaceContent(root, home, stage.ID, []byte("reviewed content\n")); err != nil {
		t.Fatal(err)
	}
	attackerRoot := initGitRepo(t)
	write(t, filepath.Join(attackerRoot, "source.md"), "attacker source\n")
	stageAfterOpen = func() {
		stageAfterOpen = nil
		replacePathWithSymlink(t, root, attackerRoot)
	}
	t.Cleanup(func() { stageAfterOpen = nil })
	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err == nil || !strings.Contains(err.Error(), "selected logical root changed") {
		t.Fatalf("replaced project commit error = %v", err)
	}
	target := filepath.FromSlash(stage.Target)
	if _, err := os.Lstat(filepath.Join(attackerRoot, target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement project received canonical content: %v", err)
	}
	if _, _, content, err := ReadReview(root, home, stage.ID); err != nil || string(content) != "reviewed content\n" {
		t.Fatalf("retained stage was not kept after project replacement: content=%q err=%v", content, err)
	}
}

func TestCommitRejectsReplacedLogicalHomeAndKeepsRetainedStage(t *testing.T) {
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
	stage := report.Stages[0]
	if err := ReplaceContent(root, home, stage.ID, []byte("reviewed content\n")); err != nil {
		t.Fatal(err)
	}
	attackerHome := t.TempDir()
	write(t, filepath.Join(attackerHome, "sentinel"), "replacement home\n")
	var retainedHome string
	stageAfterOpen = func() {
		stageAfterOpen = nil
		retainedHome = home + "-retained"
		replacePathWithSymlink(t, home, attackerHome)
	}
	t.Cleanup(func() { stageAfterOpen = nil })
	if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err == nil || !strings.Contains(err.Error(), "selected logical root changed") {
		t.Fatalf("replaced home commit error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(attackerHome, "backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement home received backup transaction: %v", err)
	}
	relStage, err := filepath.Rel(home, stage.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(retainedHome, relStage)); err != nil {
		t.Fatalf("retained stage generation was removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(stage.Target))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused commit published canonical content: %v", err)
	}
}

func TestCommitUsesScopeSpecificLayoutAndNeverMutatesGitIndex(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "shared.md"), "shared source")
	write(t, filepath.Join(root, "local.md"), "local source")
	write(t, filepath.Join(root, ".gitignore"), "local.md\nAGENTS.local.md\n.agents.local/\n")
	git(t, root, "add", "shared.md", ".gitignore")
	git(t, root, "commit", "-m", "source classification")

	report, err := Create(context.Background(), Options{
		Root:            root,
		ThreadpointHome: home,
		Inputs: []Input{
			{Provider: "fixture", Source: "shared.md", Kind: KindInstruction},
			{Provider: "fixture", Source: "local.md", Kind: KindInstruction},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Stages) != 2 {
		t.Fatalf("stages = %#v", report.Stages)
	}
	for _, stage := range report.Stages {
		if _, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: stage.ID}); err != nil {
			t.Fatalf("commit %s: %v", stage.ID, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatalf("shared guide missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.local.md")); err != nil {
		t.Fatalf("local guide missing: %v", err)
	}
	status := git(t, root, "status", "--porcelain")
	if strings.Contains(status, "A ") || strings.Contains(status, "M ") {
		t.Fatalf("threadpoint changed Git index: %q", status)
	}
}

func TestCommitRecordsTheReviewedSourceSnapshot(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "native.md"), "native guidance")

	staged, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Classify: map[string]Scope{"native.md": ScopeProjectShared},
		Inputs:   []Input{{Provider: "fixture", Source: "native.md", Kind: KindKnowledge}},
	})
	if err != nil || len(staged.Stages) != 1 {
		t.Fatalf("stage report = %#v, err = %v", staged, err)
	}
	report, err := Commit(context.Background(), CommitOptions{Root: root, ThreadpointHome: home, ID: staged.Stages[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if report.BackupRun == "" {
		t.Fatalf("commit report omitted its source snapshot: %#v", report)
	}
	runs, err := backup.ListRunsWithOverride(root, "", home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.RunID != report.BackupRun || run.Operation != "commit" {
			continue
		}
		if len(run.Entries) != 2 || run.Entries[0].Role != "stage-source" || run.Entries[0].Path != "native.md" || run.Entries[0].SHA256 != staged.Stages[0].SourceSHA256 ||
			run.Entries[1].Role != "canonical-prior" || run.Entries[1].Path != staged.Stages[0].Target || !run.Entries[1].Absent {
			t.Fatalf("commit source snapshot = %#v", run.Entries)
		}
		return
	}
	t.Fatalf("commit source snapshot run %q not found in %#v", report.BackupRun, runs)
}

func TestReviewedContentUsesStableTargetsForSparseSourceNames(t *testing.T) {
	for _, tc := range []struct {
		name             string
		scope            Scope
		provider, source string
		kind             Kind
		target, heading  string
	}{
		{"unnamed-provider-and-title", ScopeProjectShared, "", ".md", KindKnowledge, ".agents/knowledge/imported/imported/imported.md", "# Imported Knowledge"},
		{"local-rule", ScopeProjectLocal, "native", "preferences.md", KindRule, ".agents.local/rules/native-preferences.md", "# preferences"},
		{"local-instruction", ScopeProjectLocal, "native", "guide.md", KindInstruction, "AGENTS.local.md", "## Imported Agent Guidance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, body, err := NormalizeReviewedContent(CommitInput{Scope: tc.scope, Provider: tc.provider, Source: tc.source, Kind: tc.kind, ID: "stage-fixture", Content: []byte("  reviewed content\n")})
			if err != nil || target != tc.target || !strings.HasPrefix(string(body), tc.heading+"\n") || !strings.Contains(string(body), "reviewed content\n") {
				t.Fatalf("normalized result=%s %q %v", target, body, err)
			}
		})
	}
}

func TestReviewedContentRejectsInvalidScopePathAndSize(t *testing.T) {
	for _, damage := range []string{"scope", "source", "empty", "whitespace", "oversized"} {
		t.Run(damage, func(t *testing.T) {
			input := CommitInput{Scope: ScopeProjectShared, Provider: "native", Source: "notes.md", Kind: KindKnowledge, ID: "stage-fixture", Content: []byte("reviewed")}
			switch damage {
			case "scope":
				input.Scope = "invalid"
			case "source":
				input.Source = "../outside.md"
			case "empty":
				input.Content = nil
			case "whitespace":
				input.Content = []byte(" \n\t")
			case "oversized":
				input.Content = make([]byte, maxStageBytes+1)
			}
			if target, body, err := NormalizeReviewedContent(input); err == nil || target != "" || body != nil {
				t.Fatalf("invalid reviewed input accepted: %s %d bytes %v", target, len(body), err)
			}
		})
	}
}
