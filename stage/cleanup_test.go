// SPDX-License-Identifier: Apache-2.0

package stage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/safefs"
)

func TestPublicStageCleanupBoundsTraversalDepth(t *testing.T) {
	rootPath := t.TempDir()
	current := rootPath
	for depth := 0; depth <= 128; depth++ {
		current = filepath.Join(current, "d")
		if err := os.Mkdir(current, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(current, "sentinel"), "retain\n")
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := removePinnedRootContents(root); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("unbounded public cleanup depth error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(current, "sentinel")); err != nil {
		t.Fatalf("cleanup crossed its traversal budget: %v", err)
	}
}

func TestPublicStageCleanupRefusesReplacedLeaf(t *testing.T) {
	rootPath := t.TempDir()
	write(t, filepath.Join(rootPath, "review.md"), "reviewed\n")
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	original := removePinnedRootBeforeEntryRemove
	removePinnedRootBeforeEntryRemove = func(cleanupRoot *os.Root, name string) {
		removePinnedRootBeforeEntryRemove = nil
		if err := cleanupRoot.Remove(name); err != nil {
			t.Errorf("remove original leaf: %v", err)
			return
		}
		file, err := cleanupRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Errorf("create replacement leaf: %v", err)
			return
		}
		_, writeErr := file.WriteString("replacement\n")
		if err := errors.Join(writeErr, file.Close()); err != nil {
			t.Errorf("write replacement leaf: %v", err)
		}
	}
	t.Cleanup(func() { removePinnedRootBeforeEntryRemove = original })

	if err := removePinnedRootContents(root); err == nil || !strings.Contains(err.Error(), "changed before cleanup") {
		t.Fatalf("replacement cleanup error = %v", err)
	}
	body, err := os.ReadFile(filepath.Join(rootPath, "review.md"))
	if err != nil || string(body) != "replacement\n" {
		t.Fatalf("replacement leaf was removed: body=%q err=%v", body, err)
	}
}

func TestRecoveredCommitRollsBackWhenSelectedProjectEntryIsRetargetedBeforeStageCleanup(t *testing.T) {
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
	_, _, review, err := ReadReview(logical, home, stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, normalized, err := NormalizeReviewedContent(CommitInput{
		Root: logical, Scope: stage.Scope, Provider: stage.Provider, Source: stage.Source,
		Kind: stage.Kind, ID: stage.ID, Content: review,
	})
	if err != nil || target != stage.Target {
		t.Fatalf("normalize target=%q want=%q err=%v", target, stage.Target, err)
	}
	canonical := filepath.Join(original, filepath.FromSlash(target))
	prior := []byte("prior canonical\n")
	write(t, canonical, string(prior))
	priorInfo, err := os.Stat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	proposed := proposedCanonicalGeneration(normalized, prior)
	sourceBody, err := os.ReadFile(filepath.Join(original, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(filepath.Join(original, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer homeRoot.Close()
	transaction := newCommitBackupTransaction(stage, logical, home, homeRoot, sourceBody, sourceInfo.Mode())
	if err := transaction.prepare(CanonicalTransactionSnapshot{
		Target: target, Proposed: proposed, PriorExists: true, PriorBody: prior, PriorMode: priorInfo.Mode().Perm(),
	}); err != nil {
		t.Fatal(err)
	}
	write(t, canonical, string(proposed))
	if err := transaction.finalize(target, proposed); err != nil {
		t.Fatal(err)
	}

	replacement := initGitRepo(t)
	write(t, filepath.Join(replacement, "notes.md"), "replacement source\n")
	commitBeforeStageCleanupValidation = func() {
		commitBeforeStageCleanupValidation = nil
		retargetSelectedProject(t, logical, replacement)
	}
	t.Cleanup(func() { commitBeforeStageCleanupValidation = nil })

	_, err = Commit(context.Background(), CommitOptions{Root: logical, ThreadpointHome: home, ID: stage.ID})
	if err == nil || !strings.Contains(err.Error(), "selected logical root changed") {
		t.Fatalf("retargeted recovered commit error=%v", err)
	}
	if body, readErr := os.ReadFile(canonical); readErr != nil || !bytes.Equal(body, prior) {
		t.Fatalf("recovered detached canonical was not rolled back: body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Stat(stage.Dir); statErr != nil {
		t.Fatalf("review stage removed after recovered logical-root rollback: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(replacement, filepath.FromSlash(stage.Target))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replacement project was mutated: %v", statErr)
	}
}

func TestStageTempCleanupPreservesReplacementGeneration(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "notes.md"), "review bytes\n")
	injected := errors.New("forced failure after stage temp creation")
	previousHook := stageTempAfterCreate
	t.Cleanup(func() { stageTempAfterCreate = previousHook })
	var replacementPath string
	stageTempAfterCreate = func(parent *os.Root, name string, _ os.FileInfo) error {
		detached := name + ".detached"
		if err := os.Rename(filepath.Join(parent.Name(), name), filepath.Join(parent.Name(), detached)); err != nil {
			return err
		}
		if err := parent.Mkdir(name, 0o700); err != nil {
			return err
		}
		replacementPath = filepath.Join(parent.Name(), name, "sentinel")
		return errors.Join(injected, parent.WriteFile(filepath.Join(name, "sentinel"), []byte("replacement generation\n"), 0o600))
	}

	_, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "notes.md", Kind: KindKnowledge, RequiredScope: ScopeProjectShared}},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("stage error = %v", err)
	}
	body, readErr := os.ReadFile(replacementPath)
	if readErr != nil || string(body) != "replacement generation\n" {
		t.Fatalf("replacement stage temp was removed or changed: body=%q err=%v", body, readErr)
	}
}

func TestDiscardRecoversJournaledMissingStableStageUnderWriterPath(t *testing.T) {
	if !safefs.RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
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
	stage := report.Stages[0]
	parent, err := os.OpenRoot(filepath.Dir(stage.Dir))
	if err != nil {
		t.Fatal(err)
	}
	suffix := strings.Repeat("a", 20)
	base := filepath.Base(stage.Dir)
	oldName := "." + base + ".old-" + suffix
	nextName := "." + base + ".next-" + suffix
	nextRecord := stage
	nextRecord.SourceSHA256 = digest([]byte("remote\n"))
	nextRecord.ContentSHA256 = digest([]byte("merged\n"))
	if _, err := writeStageTransaction(parent, nextName, nextRecord, []byte("remote\n"), []byte("merged\n")); err != nil {
		_ = parent.Close()
		t.Fatal(err)
	}
	if err := safefs.RenameRootNoReplace(parent, base, oldName); err != nil {
		_ = parent.Close()
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journaled crash did not leave stable stage missing: %v", err)
	}

	artifacts := stageTransactionArtifacts(t, stage.Dir)
	if _, _, _, err := ReadReview(root, home, stage.ID); err == nil {
		t.Fatal("observational read unexpectedly recovered missing stable stage")
	}
	if got := stageTransactionArtifacts(t, stage.Dir); !slices.Equal(got, artifacts) {
		t.Fatalf("observational read changed recovery artifacts: before=%#v after=%#v", artifacts, got)
	}
	if err := Discard(root, home, stage.ID); err != nil {
		t.Fatalf("writer-path recovery and discard: %v", err)
	}
	if artifacts := stageTransactionArtifacts(t, stage.Dir); len(artifacts) != 0 {
		t.Fatalf("writer-path recovery left artifacts: %#v", artifacts)
	}
}

func TestStageReviewContentCanBeReadEditedDiffedAndDiscarded(t *testing.T) {
	root := initGitRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(root, "source.md"), "original\n")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("AGENTS.local.md\n.agents.local/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Create(context.Background(), Options{
		Root: root, ThreadpointHome: home,
		Inputs: []Input{{Provider: "fixture", Source: "source.md", Kind: KindKnowledge, RequiredScope: ScopeProjectLocal}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, err=%v", report, err)
	}
	id := report.Stages[0].ID
	if body, err := ReadContent(root, home, id); err != nil || string(body) != "original\n" {
		t.Fatalf("staged content = %q, err=%v", body, err)
	}
	record, snapshot, review, err := ReadReview(root, home, id)
	if err != nil || record.ID != id || string(snapshot) != "original\n" || string(review) != "original\n" {
		t.Fatalf("review = %#v, %q, %q, %v", record, snapshot, review, err)
	}
	write(t, filepath.Join(root, "source.md"), "current source\n")
	current, err := ReadCurrentSource(root, home, id)
	if err != nil || string(current) != "current source\n" {
		t.Fatalf("current source = %q, err=%v", current, err)
	}
	if diff, err := Diff(root, home, id); err != nil || diff != "" {
		t.Fatalf("initial diff = %q, err=%v", diff, err)
	}
	if err := ReplaceContent(root, home, id, []byte("reviewed\n")); err != nil {
		t.Fatal(err)
	}
	if diff, err := Diff(root, home, id); err != nil || !strings.Contains(diff, "-original") || !strings.Contains(diff, "+reviewed") {
		t.Fatalf("edited diff = %q, err=%v", diff, err)
	}
	for _, body := range [][]byte{nil, make([]byte, maxStageBytes+1)} {
		if err := ReplaceContent(root, home, id, body); err == nil {
			t.Errorf("ReplaceContent(%d bytes) unexpectedly succeeded", len(body))
		}
	}
	if err := Discard(root, home, id); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadContent(root, home, id); err == nil {
		t.Fatal("discarded stage should not remain readable")
	}
}

func TestStageDetachmentRefusesChangedGenerationAndOccupiedDestination(t *testing.T) {
	for _, failure := range []string{"nil-parent", "closed-parent", "missing-stage", "wrong-identity", "occupied-quarantine", "invalid-quarantine", "unwritable-parent"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "stage-fixture", "review"), "reviewed")
			parent, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			info, err := parent.Lstat("stage-fixture")
			if err != nil {
				t.Fatal(err)
			}
			selected := parent
			base, quarantine := "stage-fixture", "quarantine"
			switch failure {
			case "nil-parent":
				selected = nil
			case "closed-parent":
				_ = parent.Close()
			case "missing-stage":
				base = "missing"
			case "wrong-identity":
				info, err = parent.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
			case "occupied-quarantine":
				write(t, filepath.Join(dir, quarantine), "foreign")
			case "invalid-quarantine":
				quarantine = "\x00bad"
			case "unwritable-parent":
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(dir, 0o700) }()
			}
			if err := detachStageGenerationTo(selected, base, quarantine, info); err == nil {
				t.Fatal("unsafe detachment succeeded")
			}
			body, err := os.ReadFile(filepath.Join(dir, "stage-fixture", "review"))
			if err != nil || string(body) != "reviewed" {
				t.Fatalf("review lost: %q %v", body, err)
			}
		})
	}
}

func TestDetachedStageCleanupRequiresExactIdentityAndAvailableBudget(t *testing.T) {
	for _, failure := range []string{"nil-budget", "closed-parent", "missing-generation", "wrong-identity", "exhausted-entries", "exhausted-paths", "unreadable-generation"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "quarantine", "review"), "reviewed")
			parent, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			info, err := parent.Lstat("quarantine")
			if err != nil {
				t.Fatal(err)
			}
			budget := &stageEnumerationBudget{}
			name := "quarantine"
			switch failure {
			case "nil-budget":
				budget = nil
			case "closed-parent":
				_ = parent.Close()
			case "missing-generation":
				name = "missing"
			case "wrong-identity":
				info, err = parent.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
			case "exhausted-entries":
				budget.entries = maxEnumeratedStageEntries + 1
			case "exhausted-paths":
				budget.pathBytes = maxEnumeratedStagePathBytes
			case "unreadable-generation":
				if err := parent.Chmod(name, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(filepath.Join(dir, name), 0o700) }()
			}
			if err := removeDetachedStageGenerationFromParentWithBudget(parent, name, info, budget); err == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			if failure == "unreadable-generation" {
				if err := os.Chmod(filepath.Join(dir, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			body, err := os.ReadFile(filepath.Join(dir, "quarantine", "review"))
			if err != nil || string(body) != "reviewed" {
				t.Fatalf("review lost: %q %v", body, err)
			}
		})
	}
}

func TestHomeRootStageCleanupRefusesUntrustedScope(t *testing.T) {
	for _, failure := range []string{"nil-home", "closed-home", "invalid-id", "invalid-scope", "missing-scope", "file-scope", "symlink-scope", "unreadable-scope"} {
		t.Run(failure, func(t *testing.T) {
			home := t.TempDir()
			scope := filepath.Join("imports", "project", string(ScopeProjectShared))
			dir := filepath.Join(home, scope, "stage-fixture")
			write(t, filepath.Join(dir, "review"), "reviewed")
			root, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			info, err := root.Lstat(filepath.Join(scope, "stage-fixture"))
			if err != nil {
				t.Fatal(err)
			}
			record := Record{ID: "stage-fixture", ProjectID: "project", Scope: ScopeProjectShared}
			selected := root
			retained := dir
			switch failure {
			case "nil-home":
				selected = nil
			case "closed-home":
				_ = root.Close()
			case "invalid-id":
				record.ID = "../stage-fixture"
			case "invalid-scope":
				record.Scope = Scope("private")
			case "missing-scope", "file-scope", "symlink-scope":
				if err := root.Rename(scope, scope+".retained"); err != nil {
					t.Fatal(err)
				}
				retained = filepath.Join(home, scope+".retained", "stage-fixture")
				if failure == "file-scope" {
					if err := root.WriteFile(scope, []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "symlink-scope" {
					if err := root.Symlink(string(ScopeProjectShared)+".retained", scope); err != nil {
						t.Fatal(err)
					}
				}
			case "unreadable-scope":
				if err := root.Chmod(scope, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(filepath.Join(home, scope), 0o700) }()
			}
			if err := removeStageGenerationFromHomeRoot(selected, record, info); err == nil {
				t.Fatal("untrusted scope cleanup succeeded")
			}
			if failure == "unreadable-scope" {
				if err := os.Chmod(filepath.Join(home, scope), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			body, err := os.ReadFile(filepath.Join(retained, "review"))
			if err != nil || string(body) != "reviewed" {
				t.Fatalf("review lost: %q %v", body, err)
			}
		})
	}
}
