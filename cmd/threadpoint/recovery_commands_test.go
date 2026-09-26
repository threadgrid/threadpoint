// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/prune"
	"github.com/threadgrid/threadpoint/restore"
)

func TestWriteRestorationReportFormatsTextAndJSONWithoutSuppressingWriteErrors(t *testing.T) {
	report := &restore.Report{Root: "fixture", Applied: true}
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	if err := writeRestorationReport(app, outputFormatText, report); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() == 0 {
		t.Fatal("expected text restoration report")
	}
	stdout.Reset()
	if err := writeRestorationReport(app, outputFormatJSON, report); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() == 0 || stdout.Bytes()[0] != '{' {
		t.Fatalf("JSON restoration report = %q", stdout.String())
	}
	if err := writeRestorationReport(app, outputFormatText, nil); err != nil {
		t.Fatalf("nil restoration report should be ignored: %v", err)
	}
}

func TestPrunePlanAndRestoreListUseBackupNamespace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("THREADPOINT_HOME", t.TempDir())
	backupDir := "cli"
	source := filepath.Join(root, "CLAUDE.md")
	mustWrite(t, source, "claude")
	store := seedCLIPrunableCommit(t, root, backupDir, source)

	stdout, _, err := runTestCLI(t, "prune", "--root", root, "--backup-namespace", backupDir, "--plan", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var prunePlan prune.Plan
	if err := json.Unmarshal([]byte(stdout), &prunePlan); err != nil {
		t.Fatalf("prune emitted invalid JSON: %v\n%s", err, stdout)
	}
	if len(prunePlan.Candidates) != 1 || prunePlan.Candidates[0].Status != prune.StatusReady {
		t.Fatalf("expected ready prune candidate, got %#v", prunePlan.Candidates)
	}

	stdout, _, err = runTestCLI(t, "restore", "list", "--root", root, "--backup-namespace", backupDir, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var runs []backup.Run
	if err := json.Unmarshal([]byte(stdout), &runs); err != nil {
		t.Fatalf("restore list emitted invalid JSON: %v\n%s", err, stdout)
	}
	if len(runs) != 1 || runs[0].RunID != store.RunID {
		t.Fatalf("expected restore list to include seeded run, got %#v", runs)
	}

	stdout, _, err = runTestCLI(t, "restore", "--root", root, "--backup-namespace", backupDir, "--backup", store.RunID, "--plan", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var restorePlan restore.Plan
	if err := json.Unmarshal([]byte(stdout), &restorePlan); err != nil {
		t.Fatalf("restore plan emitted invalid JSON: %v\n%s", err, stdout)
	}
	if restorePlan.BackupRun != store.RunID {
		t.Fatalf("expected selected backup run %s, got %#v", store.RunID, restorePlan)
	}
}

func TestPruneRejectsExplicitPlanApply(t *testing.T) {
	_, _, err := runTestCLI(t, "prune", "--plan", "--apply", "--yes")
	if err == nil {
		t.Fatal("expected explicit --plan --apply to fail")
	}
	if !strings.Contains(err.Error(), "prune accepts only one of --plan or --apply") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRestoreJSONRendersPartialReportBeforeReturningApplyError(t *testing.T) {
	root := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = t.TempDir()
	wantErr := errors.New("injected restore transaction failure")
	app.restoreApply = func(context.Context, restore.Options) (*restore.Report, error) {
		return &restore.Report{
			Root:    root,
			Applied: true,
			Resolutions: []restore.Resolution{{
				Path:     "AGENTS.md",
				Decision: restore.DecisionRestore,
				Backup:   "/state/overwrite",
				Recovery: "/project/.threadpoint-restore-recovery/item",
			}},
		}, wantErr
	}
	err := app.run(context.Background(), []string{"restore", "--root", root, "--apply", "--yes", "--format", "json"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("restore error = %v, want %v", err, wantErr)
	}
	var report restore.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("partial restore report is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(report.Resolutions) != 1 || report.Resolutions[0].Backup == "" || report.Resolutions[0].Recovery == "" {
		t.Fatalf("partial JSON report lost recovery metadata: %#v", report)
	}
}

func TestRestoreJSONRendersPartialReportBeforeInteractiveError(t *testing.T) {
	root := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = t.TempDir()
	terminal, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	info, err := terminal.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		t.Skipf("%s is not a character device", os.DevNull)
	}
	app.stdin = terminal
	wantErr := errors.New("injected interactive restore failure")
	app.restoreRun = func(context.Context, restore.Options, io.Reader, io.Writer, io.Writer) (*restore.Report, error) {
		return &restore.Report{
			Root:    root,
			Applied: true,
			Resolutions: []restore.Resolution{{
				Path:     "AGENTS.md",
				Decision: restore.DecisionRestore,
				Backup:   "/state/overwrite",
				Recovery: "/project/.threadpoint-restore-recovery/item",
			}},
		}, wantErr
	}
	err = app.run(context.Background(), []string{"restore", "--root", root, "--format", "json"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("interactive restore error = %v, want %v", err, wantErr)
	}
	var report restore.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("partial interactive restore report is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(report.Resolutions) != 1 || report.Resolutions[0].Recovery == "" {
		t.Fatalf("partial interactive JSON report lost recovery metadata: %#v", report)
	}
}

func TestPruneApplyWorksWithDefaultPlanFlag(t *testing.T) {
	t.Setenv("THREADPOINT_HOME", t.TempDir())
	root := t.TempDir()
	backupDir := "cli-apply"
	source := filepath.Join(root, "CLAUDE.md")
	mustWrite(t, source, "claude")
	seedCLIPrunableCommit(t, root, backupDir, source)

	_, _, err := runTestCLI(t, "prune", "--root", root, "--backup-namespace", backupDir, "--apply", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("expected prune apply to remove source, stat err: %v", err)
	}

	root = t.TempDir()
	backupDir = "cli-apply-plan-false"
	source = filepath.Join(root, "CLAUDE.md")
	mustWrite(t, source, "claude")
	seedCLIPrunableCommit(t, root, backupDir, source)
	_, _, err = runTestCLI(t, "prune", "--root", root, "--backup-namespace", backupDir, "--plan=false", "--apply", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("expected prune apply with --plan=false to remove source, stat err: %v", err)
	}
}

func TestPruneApplyPrintsPartialRecoveryReportBeforeReturningError(t *testing.T) {
	t.Setenv("THREADPOINT_HOME", t.TempDir())
	original := applyPrune
	t.Cleanup(func() { applyPrune = original })
	wantErr := errors.New("simulated rollback collision")
	recovery := filepath.Join(t.TempDir(), ".threadpoint-prune-recovery-token", "original")
	applyPrune = func(_ context.Context, opts prune.Options) (*prune.Report, error) {
		return &prune.Report{
			Root:    opts.Root,
			Applied: true,
			Resolutions: []prune.Resolution{{
				Path:     "CLAUDE.md",
				Status:   prune.StatusReady,
				Backup:   "/state/prune-backup",
				Recovery: recovery,
			}},
		}, wantErr
	}

	stdout, _, err := runTestCLI(t, "prune", "--root", t.TempDir(), "--apply", "--yes", "--format", "json")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected apply error after report output, got %v", err)
	}
	var report prune.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("partial prune report was not valid JSON: %v\n%s", err, stdout)
	}
	if len(report.Resolutions) != 1 || report.Resolutions[0].Recovery != recovery {
		t.Fatalf("partial recovery metadata was lost: %#v", report)
	}
}

func seedCLIPrunableCommit(t *testing.T, root, backupDir, source string) *backup.Store {
	t.Helper()
	sourceBody, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	targetRel := filepath.ToSlash(filepath.Join(".agents", "prune-fixture.md"))
	targetBody := []byte("canonical fixture\n")
	sourceHash := sha256.Sum256(sourceBody)
	targetHash := sha256.Sum256(targetBody)
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root, BackupDir: backupDir, Operation: backup.PendingCommitOperation, RunID: "commit-cli-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration(
		"cli-fixture", "CLAUDE.md", hex.EncodeToString(sourceHash[:]), targetRel, hex.EncodeToString(targetHash[:]), int64(len(targetBody)),
	); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, readPendingFixtureSource(t, source), pendingFixtureSourceMode(t, source), nil, os.FileMode(0), false)

	mustWrite(t, filepath.Join(root, filepath.FromSlash(targetRel)), string(targetBody))
	if err := store.FinalizePendingCommit(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestRestoreListRejectsRestorationFlags(t *testing.T) {
	t.Setenv("THREADPOINT_HOME", t.TempDir())
	for _, args := range [][]string{
		{"--list"}, {"list", "--apply"}, {"list", "--plan"}, {"list", "--yes"},
		{"list", "--confirm", "restore threadpoint backup"}, {"list", "--backup", "run"},
		{"list", "--latest"}, {"list", "unexpected"}, {"list", "--format", "invalid"},
	} {
		_, _, err := runTestCLI(t, append([]string{"restore"}, args...)...)
		if err == nil {
			t.Errorf("restore %v unexpectedly succeeded", args)
		}
		if err != nil && exitCode(err) != 2 {
			t.Errorf("restore %v: expected usage error, got %v", args, err)
		}
	}
}

func TestRestoreListEmptyNamespace(t *testing.T) {
	t.Setenv("THREADPOINT_HOME", t.TempDir())
	root := t.TempDir()
	for _, format := range []string{"text", "json"} {
		output, _, err := runTestCLI(t, "restore", "list", "--root", root, "--backup-namespace", "empty", "--format", format)
		if err != nil {
			t.Fatal(err)
		}
		if format == "text" {
			if !strings.Contains(output, "backup runs: 0") {
				t.Fatalf("empty list = %q", output)
			}
		} else {
			var runs []backup.Run
			if err := json.Unmarshal([]byte(output), &runs); err != nil || len(runs) != 0 {
				t.Fatalf("empty list = %q, %v", output, err)
			}
		}
	}
}

func TestRestoreListRedactsRootsWithoutChangingBackups(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv("THREADPOINT_HOME", home)
	source := filepath.Join(root, "CLAUDE.md")
	mustWrite(t, source, "private guidance")
	seedCLIPrunableCommit(t, root, "redaction", source)
	output, _, err := runTestCLI(t, "restore", "list", "--root", root, "--backup-namespace", "redaction", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, root) || strings.Contains(output, home) {
		t.Fatalf("absolute paths leaked: %s", output)
	}
	if !strings.Contains(output, "$ROOT") || !strings.Contains(output, "$THREADPOINT_HOME") {
		t.Fatalf("missing path labels: %s", output)
	}
	var reports []backup.Run
	if err := json.Unmarshal([]byte(output), &reports); err != nil || len(reports) != 1 {
		t.Fatalf("invalid list: %v %s", err, output)
	}
	runs, err := restore.ListRuns(restore.Options{Root: root, ThreadpointHome: home, BackupDir: "redaction"})
	if err != nil || len(runs) != 1 || runs[0].ProjectRoot != root {
		t.Fatalf("listing changed stored roots: %#v %v", runs, err)
	}
}

func TestRestoreUsageErrorsSuggestListCommand(t *testing.T) {
	for _, args := range [][]string{
		{"--root", t.TempDir(), "list"},
		{"--list"}, {"-list"}, {"--list=true"},
		{"--unknown"}, {"--latest=invalid"}, {"--backup"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, err := runTestCLI(t, append([]string{"restore"}, args...)...)
			if err == nil || exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "threadpoint restore list --root PATH") {
				t.Fatalf("missing usage hint: %v", err)
			}
		})
	}
}
