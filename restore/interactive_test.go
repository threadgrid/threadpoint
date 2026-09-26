// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestRunInteractiveHandlesExplicitDecisions(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "backup")
	runID := seedFileBackup(t, root, home, target, "")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	var skippedOutput, skippedErrors bytes.Buffer
	skipped, err := RunInteractive(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID}, strings.NewReader("s\n"), &skippedOutput, &skippedErrors)
	if err != nil || len(skipped.Resolutions) != 1 || !skipped.Resolutions[0].Skipped {
		t.Fatalf("skipped interactive restore = %#v, err=%v", skipped, err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skipped restore changed target: %v", err)
	}

	var restoredOutput, restoredErrors bytes.Buffer
	restored, err := RunInteractive(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID}, strings.NewReader("r\n"), &restoredOutput, &restoredErrors)
	if err != nil || len(restored.Resolutions) != 1 || !restored.Resolutions[0].Restored {
		t.Fatalf("restored interactive report = %#v, err=%v", restored, err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "backup" {
		t.Fatalf("interactive restore target = %q, err=%v", body, err)
	}
}

func TestApplyRefusesConflictWithoutInteractiveMerge(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, "")
	mustWriteFile(t, target, "newer")

	_, err := Apply(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, Yes: true})
	if err == nil {
		t.Fatal("expected conflict to block noninteractive restore")
	}
	if !strings.Contains(err.Error(), "newer content") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyResolutionsRejectsTargetChangedAfterPlan(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != StatusReady {
		t.Fatalf("expected ready restore candidate, got %#v", plan.Candidates)
	}
	mustWriteFile(t, target, "raced")

	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID, Yes: true}, plan, []pendingResolution{{
		candidate: plan.Candidates[0],
		decision:  DecisionRestore,
	}})
	if err == nil || !strings.Contains(err.Error(), "changed since restore plan") {
		t.Fatalf("expected stale restore plan refusal, got %v", err)
	}
	body, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "raced" {
		t.Fatalf("stale restore plan overwrote target: %q", body)
	}
}

func TestApplyResolutionsRejectsReplacedPlannedRoot(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, func() {
		mustWriteFile(t, target, "after")
	})
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	moved := root + "-reviewed"
	if err := os.Rename(root, moved); err != nil {
		t.Skipf("cannot replace project root on this platform: %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "AGENTS.md"), "replacement")

	_, err = applyResolutions(context.Background(), Options{Root: root, HomeDir: home}, plan, []pendingResolution{{
		candidate: plan.Candidates[0],
		decision:  DecisionRestore,
	}})
	if err == nil || !strings.Contains(err.Error(), "root changed") {
		t.Fatalf("expected replaced root refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(filepath.Join(root, "AGENTS.md")); readErr != nil || string(body) != "replacement" {
		t.Fatalf("replacement root was mutated, body=%q err=%v", body, readErr)
	}
	if body, readErr := os.ReadFile(filepath.Join(moved, "AGENTS.md")); readErr != nil || string(body) != "after" {
		t.Fatalf("reviewed root was mutated, body=%q err=%v", body, readErr)
	}
}

func TestApplyCancellationReturnsCompletedResolutions(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	firstTarget := filepath.Join(root, "first.md")
	secondTarget := filepath.Join(root, "second.md")
	mustWriteFile(t, firstTarget, "first before")
	mustWriteFile(t, secondTarget, "second before")
	store, err := backup.NewStore(backup.StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, err := store.BackupFile("first.md", firstTarget, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackupFile("second.md", secondTarget, "fixture-target"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, firstTarget, "first after")
	mustWriteFile(t, secondTarget, "second after")
	if err := store.SetPostHash("first.md", firstTarget); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPostHash("second.md", secondTarget); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRetainedRestorePlan(t, context.Background(), Options{Root: root, HomeDir: home, BackupID: store.RunID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := false
	hooks := &restoreHooks{afterRecoveryValidation: func() {
		if !cancelled {
			cancelled = true
			cancel()
		}
	}}
	pending := []pendingResolution{
		{candidate: plan.Candidates[0], decision: DecisionRestore},
		{candidate: plan.Candidates[1], decision: DecisionRestore},
	}
	report, err := applyResolutions(ctx, Options{Root: root, HomeDir: home, hooks: hooks}, plan, pending)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("apply error = %v, want context cancellation", err)
	}
	if report == nil || len(report.Resolutions) != 1 || !report.Resolutions[0].Restored || report.Resolutions[0].Recovery == "" {
		t.Fatalf("cancellation dropped completed recovery metadata: %#v", report)
	}
	if body, err := os.ReadFile(firstTarget); err != nil || string(body) != "first before" {
		t.Fatalf("first candidate did not complete before cancellation, body=%q err=%v", body, err)
	}
	if body, err := os.ReadFile(secondTarget); err != nil || string(body) != "second after" {
		t.Fatalf("second candidate mutated after cancellation, body=%q err=%v", body, err)
	}
}

func TestRunInteractiveEditResolvesConflict(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, "")
	mustWriteFile(t, target, "newer")
	editor := filepath.Join(t.TempDir(), "editor.sh")
	mustWriteFile(t, editor, "#!/bin/sh\ncat > \"$1\" <<'EOF'\nmerged\nEOF\n")
	if err := os.Chmod(editor, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	_, err := RunInteractive(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID}, strings.NewReader("e\n"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "merged\n" {
		t.Fatalf("expected edited restore content, got %q", body)
	}
}

func TestRunInteractiveRejectsUnknownDecisionThenAllowsQuit(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, target, "before")
	runID := seedFileBackup(t, root, home, target, "")
	mustWriteFile(t, target, "newer")

	var stdout, stderr bytes.Buffer
	_, err := RunInteractive(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID}, strings.NewReader("unknown\nq\n"), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "restore aborted") {
		t.Fatalf("interactive quit error = %v", err)
	}
	if !strings.Contains(stderr.String(), "unknown decision") {
		t.Fatalf("unknown decision was not reported: %q", stderr.String())
	}
}

func TestInteractiveRestoreStopsOnInputAndPlanningFailures(t *testing.T) {
	for _, failure := range []string{"read", "after-plan", "cancel-after-plan", "editor"} {
		t.Run(failure, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			target := filepath.Join(root, "AGENTS.md")
			mustWriteFile(t, target, "original")
			runID := seedFileBackup(t, root, home, target, "")
			mustWriteFile(t, target, "newer")
			sentinel := errors.New("interactive fixture failure")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := Options{Root: root, HomeDir: home, BackupID: runID}
			var input io.Reader = strings.NewReader("s\n")
			switch failure {
			case "read":
				input = failingRestoreReader{sentinel}
			case "after-plan":
				opts.hooks = &restoreHooks{afterPlan: func(*Plan, *os.Root) error { return sentinel }}
			case "cancel-after-plan":
				opts.hooks = &restoreHooks{afterPlan: func(*Plan, *os.Root) error { cancel(); return nil }}
			case "editor":
				t.Setenv("VISUAL", "")
				t.Setenv("EDITOR", "nonexistent-restore-editor")
				input = strings.NewReader("e\ns\n")
			}
			var stdout, stderr bytes.Buffer
			report, err := RunInteractive(ctx, opts, input, &stdout, &stderr)
			switch failure {
			case "editor":
				if err != nil || report == nil || !report.Resolutions[0].Skipped || !strings.Contains(stderr.String(), "edit failed") {
					t.Fatalf("editor failure did not permit safe skip: %#v %v %s", report, err, &stderr)
				}
			case "cancel-after-plan":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			default:
				if !errors.Is(err, sentinel) {
					t.Fatalf("failure lost: %v", err)
				}
			}
			if body, err := os.ReadFile(target); err != nil || string(body) != "newer" {
				t.Fatalf("failed interaction mutated target: %q %v", body, err)
			}
		})
	}
}

type failingRestoreReader struct{ err error }

func (r failingRestoreReader) Read([]byte) (int, error) { return 0, r.err }

func TestInteractiveRestoreSkipsWithoutAnExplicitRestoreDecision(t *testing.T) {
	for _, scenario := range []string{"already-restored", "empty-input", "unknown-at-eof"} {
		t.Run(scenario, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			target := filepath.Join(root, "AGENTS.md")
			mustWriteFile(t, target, "original")
			runID := seedFileBackup(t, root, home, target, "")
			want := "original"
			if scenario == "empty-input" || scenario == "unknown-at-eof" {
				want = "newer"
				mustWriteFile(t, target, want)
			}
			input := ""
			if scenario == "unknown-at-eof" {
				input = "unknown"
			}
			var output, diagnostics bytes.Buffer
			report, err := RunInteractive(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID}, strings.NewReader(input), &output, &diagnostics)
			if err != nil || report == nil {
				t.Fatalf("safe skip: %#v %v", report, err)
			}
			for _, resolution := range report.Resolutions {
				if !resolution.Skipped {
					t.Fatalf("unexpected action: %#v", resolution)
				}
			}
			if scenario == "unknown-at-eof" && !strings.Contains(diagnostics.String(), "unknown decision") {
				t.Fatalf("missing decision diagnostic: %s", &diagnostics)
			}
			body, err := os.ReadFile(target)
			if err != nil || string(body) != want {
				t.Fatalf("no decision changed target: %q %v", body, err)
			}
		})
	}
}
