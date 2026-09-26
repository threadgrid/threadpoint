// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/knowledgebase"
	"github.com/threadgrid/threadpoint/layout"
	"github.com/threadgrid/threadpoint/stage"
)

func TestImplicitRootResolvesNearestAgentsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "pkg", "service")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	stdout, _, err := runTestCLI(t, "status", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var kb knowledgebase.Catalog
	if err := json.Unmarshal([]byte(stdout), &kb); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if kb.Root != root {
		t.Fatalf("status root = %q, want .agents root %q", kb.Root, root)
	}
}

func TestStageCommandsHandleEmptyWorkspaceAndValidation(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	app := newCLI(&stdout, &stderr, stdin)
	app.threadpointHome = home
	if err := runStage(context.Background(), app, []string{"--root", root, "--plan"}); err != nil {
		t.Fatalf("stage plan: %v", err)
	}
	if !strings.Contains(stdout.String(), "threadpoint stage --plan") {
		t.Fatalf("stage plan output = %q", stdout.String())
	}
	stdout.Reset()
	if err := runStage(context.Background(), app, []string{"list", "--root", root}); err != nil {
		t.Fatalf("stage list: %v", err)
	}
	if !strings.Contains(stdout.String(), "threadpoint stage list") {
		t.Fatalf("stage list output = %q", stdout.String())
	}
	for _, tc := range []struct {
		args []string
		want string
		run  func() error
	}{
		{args: nil, want: "requires exactly one"},
		{args: []string{"--plan", "--apply"}, want: "requires exactly one"},
		{args: []string{"--plan", "unexpected"}, want: "does not accept positional"},
		{args: []string{"--plan", "--providers", "unknown"}, want: "unknown provider"},
		{args: []string{"diff"}, want: "requires exactly one stage ID"},
		{args: []string{"discard", "id"}, want: "confirmation declined"},
		{args: []string{"edit", "id"}, want: "was not found"},
		{args: []string{"commit", "id"}, want: "requires exactly one", run: func() error { return runCommit(context.Background(), app, []string{"id"}) }},
	} {
		run := tc.run
		if run == nil {
			run = func() error { return runStage(context.Background(), app, tc.args) }
		}
		if err := run(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("runStage(%v) error = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestStageClassificationsValidatePathsAndScopes(t *testing.T) {
	classifications, err := parseClassifications([]string{" ./one=project-shared,two=project-local "})
	if err != nil || classifications["one"] != stage.ScopeProjectShared || classifications["two"] != stage.ScopeProjectLocal {
		t.Fatalf("classifications = %#v, %v", classifications, err)
	}
	for _, values := range [][]string{{"missing"}, {"=project-local"}, {"one=unknown"}, {"one=project-shared,one=project-local"}} {
		if _, err := parseClassifications(values); err == nil {
			t.Errorf("parseClassifications(%v) unexpectedly succeeded", values)
		}
	}
}

func TestLeadingStageIDSeparatesIDFromFlags(t *testing.T) {
	if id, rest := leadingStageID([]string{"id", "--root", "."}); id != "id" || len(rest) != 2 {
		t.Fatalf("leading stage ID = %q, %#v", id, rest)
	}
	if id, _ := leadingStageID([]string{"--root", "."}); id != "" {
		t.Fatalf("flag was incorrectly read as stage ID: %q", id)
	}
}

func TestStageReportRendersWarningsInTextAndJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	report := &stage.Report{Root: "/work", Warnings: []string{"review"}}
	if err := writeStageReport(app, outputFormatText, "threadpoint stage", report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "warning: review") {
		t.Fatalf("text stage report = %q", stdout.String())
	}
	stdout.Reset()
	if err := writeStageReport(app, outputFormatJSON, "threadpoint stage", report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"warnings":`) {
		t.Fatalf("JSON stage report = %q", stdout.String())
	}
}

func TestStageClassificationRequiredMapsToCLIError(t *testing.T) {
	if err := classifyStageError(stage.ErrClassificationRequired); err == nil {
		t.Fatal("classification error should be classified")
	}
}

func TestStageAndCommitPreserveGitIndex(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	initTestGitRepo(t, root)
	gitCommand(t, root, "config", "user.email", "threadpoint@example.invalid")
	gitCommand(t, root, "config", "user.name", "Threadpoint Test")
	mustWrite(t, filepath.Join(root, ".claude", "CLAUDE.md"), "review source")
	gitCommand(t, root, "add", ".claude/CLAUDE.md")
	gitCommand(t, root, "commit", "-m", "native artifact")

	stdout, stderr, err := runTestCLI(t, "--home", home, "stage", "--root", root, "--plan", "--format", "json")
	if err != nil {
		t.Fatalf("stage plan: %v\n%s", err, stderr)
	}
	var plan struct {
		Stages []struct {
			Scope string `json:"scope"`
		} `json:"stages"`
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil || len(plan.Stages) != 1 || plan.Stages[0].Scope != "project-shared" {
		t.Fatalf("unexpected stage plan %#v, err=%v\n%s", plan, err, stdout)
	}

	stdout, stderr, err = runTestCLI(t, "--home", home, "stage", "--root", root, "--apply", "--yes", "--format", "json")
	if err != nil {
		t.Fatalf("stage apply: %v\n%s", err, stderr)
	}
	var staged struct {
		Stages []struct {
			ID string `json:"id"`
		} `json:"stages"`
	}
	if err := json.Unmarshal([]byte(stdout), &staged); err != nil || len(staged.Stages) != 1 || staged.Stages[0].ID == "" {
		t.Fatalf("unexpected stage report %#v, err=%v\n%s", staged, err, stdout)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "imports")); !os.IsNotExist(err) {
		t.Fatalf("stage wrote review state into the project: %v", err)
	}

	_, stderr, err = runTestCLI(t, "--home", home, "commit", staged.Stages[0].ID, "--root", root, "--apply", "--yes")
	if err != nil {
		t.Fatalf("commit: %v\n%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatalf("shared commit did not write AGENTS.md: %v", err)
	}
	status := gitCommand(t, root, "status", "--porcelain")
	if strings.Contains(status, "A ") || strings.Contains(status, "M ") {
		t.Fatalf("stage/commit changed Git index: %q", status)
	}
}

func TestStageWarnsForIgnoredNativeArtifact(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	initTestGitRepo(t, root)
	gitCommand(t, root, "config", "user.email", "threadpoint@example.invalid")
	gitCommand(t, root, "config", "user.name", "Threadpoint Test")
	mustWrite(t, filepath.Join(root, ".codex", "AGENTS.md"), "native guidance")
	mustWrite(t, filepath.Join(root, ".gitignore"), "/.codex/\n")
	gitCommand(t, root, "add", ".gitignore")
	gitCommand(t, root, "commit", "-m", "ignored native artifact")

	stdout, stderr, err := runTestCLI(t, "--home", home, "stage", "--root", root, "--plan", "--format", "json")
	if err != nil {
		t.Fatalf("stage plan: %v\n%s", err, stderr)
	}
	var plan struct {
		Stages []struct {
			Scope string `json:"scope"`
		} `json:"stages"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("decode plan: %v\n%s", err, stdout)
	}
	if len(plan.Stages) != 1 || plan.Stages[0].Scope != "project-local" {
		t.Fatalf("ignored native artifact must stage locally: %#v", plan)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], ".codex/AGENTS.md") || !strings.Contains(plan.Warnings[0], "does not add or recommend native artifact ignore rules") {
		t.Fatalf("expected native-ignore classification advisory, got %#v", plan.Warnings)
	}

	stdout, stderr, err = runTestCLI(t, "--home", home, "stage", "--root", root, "--plan")
	if err != nil || !strings.Contains(stdout, "warning: provider-native source .codex/AGENTS.md") {
		t.Fatalf("text stage warning: err=%v stderr=%s stdout=%s", err, stderr, stdout)
	}
}

func TestStageClaudeLocalInstructionsUseLocalScope(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	initTestGitRepo(t, root)
	gitCommand(t, root, "config", "user.email", "threadpoint@example.invalid")
	gitCommand(t, root, "config", "user.name", "Threadpoint Test")
	mustWrite(t, filepath.Join(root, "CLAUDE.local.md"), "local guidance")
	gitCommand(t, root, "add", "CLAUDE.local.md")
	gitCommand(t, root, "commit", "-m", "tracked local artifacts")

	_, stderr, err := runTestCLI(t, "--home", home, "init", "--root", root, "--yes")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, stderr)
	}

	stdout, stderr, err := runTestCLI(t, "--home", home, "stage", "--root", root, "--plan", "--format", "json")
	if err != nil {
		t.Fatalf("stage plan: %v\n%s", err, stderr)
	}
	var plan struct {
		Stages []struct {
			Scope  string `json:"scope"`
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"stages"`
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("decode plan: %v\n%s", err, stdout)
	}
	if len(plan.Stages) != 1 || plan.Stages[0].Scope != "project-local" || plan.Stages[0].Source != "CLAUDE.local.md" || plan.Stages[0].Target != "AGENTS.local.md" {
		t.Fatalf("unexpected local stage plan: %#v", plan)
	}

	_, stderr, err = runTestCLI(t, "--home", home, "stage", "--root", root, "--plan", "--classify", "CLAUDE.local.md=project-shared")
	if err == nil || !strings.Contains(err.Error(), "declared project-local scope conflicts") {
		t.Fatalf("expected conflicting local scope refusal, err=%v stderr=%s", err, stderr)
	}

	stdout, stderr, err = runTestCLI(t, "--home", home, "stage", "--root", root, "--apply", "--yes", "--format", "json")
	if err != nil {
		t.Fatalf("stage apply: %v\n%s", err, stderr)
	}
	var staged struct {
		Stages []struct {
			ID string `json:"id"`
		} `json:"stages"`
	}
	if err := json.Unmarshal([]byte(stdout), &staged); err != nil || len(staged.Stages) != 1 || staged.Stages[0].ID == "" {
		t.Fatalf("unexpected local stage report %#v, err=%v\n%s", staged, err, stdout)
	}
	_, stderr, err = runTestCLI(t, "--home", home, "commit", staged.Stages[0].ID, "--root", root, "--apply", "--yes")
	if err != nil {
		t.Fatalf("local commit: %v\n%s", err, stderr)
	}
	body, err := os.ReadFile(filepath.Join(root, "AGENTS.local.md"))
	if err != nil || !strings.Contains(string(body), "local guidance") {
		t.Fatalf("local guide = %q, err=%v", body, err)
	}
}

func TestStageApplyRefusesProjectRetargetAfterLockRootBorrow(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	selected := filepath.Join(parent, "selected")
	for _, root := range []string{first, second} {
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		initTestGitRepo(t, root)
		gitCommand(t, root, "config", "user.email", "threadpoint@example.invalid")
		gitCommand(t, root, "config", "user.name", "Threadpoint Test")
		mustWrite(t, filepath.Join(root, ".claude", "CLAUDE.md"), filepath.Base(root)+" source\n")
		gitCommand(t, root, "add", ".claude/CLAUDE.md")
		gitCommand(t, root, "commit", "-m", "source")
	}
	if err := os.Symlink(first, selected); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	originalHook := stageCommandAfterProjectRootBorrow
	t.Cleanup(func() { stageCommandAfterProjectRootBorrow = originalHook })
	stageCommandAfterProjectRootBorrow = func() {
		stageCommandAfterProjectRootBorrow = nil
		if err := os.Remove(selected); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(second, selected); err != nil {
			t.Fatal(err)
		}
	}

	_, stderr, err := runTestCLI(t, "--home", filepath.Join(parent, "home"), "stage", "--root", selected, "--apply", "--yes")
	if err == nil || (!strings.Contains(err.Error(), "selected logical root") && !strings.Contains(err.Error(), "locked generation")) {
		t.Fatalf("retargeted stage error=%v stderr=%s", err, stderr)
	}
}

func TestCommitRefusesProjectRetargetAfterLockRootBorrow(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	selected := filepath.Join(parent, "selected")
	for _, root := range []string{first, second} {
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		initTestGitRepo(t, root)
	}
	gitCommand(t, first, "config", "user.email", "threadpoint@example.invalid")
	gitCommand(t, first, "config", "user.name", "Threadpoint Test")
	mustWrite(t, filepath.Join(first, ".claude", "CLAUDE.md"), "review source\n")
	gitCommand(t, first, "add", ".claude/CLAUDE.md")
	gitCommand(t, first, "commit", "-m", "source")
	if err := os.Symlink(first, selected); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	home := filepath.Join(parent, "home")
	stdout, stderr, err := runTestCLI(t, "--home", home, "stage", "--root", selected, "--apply", "--yes", "--format", "json")
	if err != nil {
		t.Fatalf("stage: %v\n%s", err, stderr)
	}
	var staged struct {
		Stages []struct {
			ID  string `json:"id"`
			Dir string `json:"dir"`
		} `json:"stages"`
	}
	if err := json.Unmarshal([]byte(stdout), &staged); err != nil || len(staged.Stages) != 1 {
		t.Fatalf("stage report=%#v err=%v", staged, err)
	}
	originalHook := stageCommandAfterProjectRootBorrow
	t.Cleanup(func() { stageCommandAfterProjectRootBorrow = originalHook })
	stageCommandAfterProjectRootBorrow = func() {
		stageCommandAfterProjectRootBorrow = nil
		if err := os.Remove(selected); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(second, selected); err != nil {
			t.Fatal(err)
		}
	}

	_, stderr, err = runTestCLI(t, "--home", home, "commit", staged.Stages[0].ID, "--root", selected, "--apply", "--yes")
	if err == nil || (!strings.Contains(err.Error(), "selected logical root") && !strings.Contains(err.Error(), "locked generation")) {
		t.Fatalf("retargeted commit error=%v stderr=%s", err, stderr)
	}
	if _, statErr := os.Stat(staged.Stages[0].Dir); statErr != nil {
		t.Fatalf("refused commit removed review stage: %v", statErr)
	}
	for _, root := range []string{first, second} {
		if _, statErr := os.Lstat(filepath.Join(root, "AGENTS.md")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("refused commit mutated %s: %v", root, statErr)
		}
	}
}

func TestStageCommandFlagAndEnvironmentValidation(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	missingRoot := filepath.Join(t.TempDir(), "missing")
	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	app.threadpointHome = home

	for _, test := range []struct {
		name string
		run  func() error
		want string
	}{
		{name: "stage unknown flag", run: func() error { return runStage(context.Background(), app, []string{"--unknown"}) }, want: "flag provided but not defined"},
		{name: "stage format", run: func() error { return runStage(context.Background(), app, []string{"--plan", "--format", "yaml"}) }, want: "unknown stage format"},
		{name: "stage root", run: func() error { return runStage(context.Background(), app, []string{"--plan", "--root", missingRoot}) }, want: "does not exist"},
		{name: "stage excluded provider", run: func() error {
			return runStage(context.Background(), app, []string{"--plan", "--root", root, "--exclude-providers", "unknown"})
		}, want: "unknown provider"},
		{name: "stage classification", run: func() error {
			return runStage(context.Background(), app, []string{"--plan", "--root", root, "--classify", "artifact=invalid"})
		}, want: "invalid --classify scope"},
		{name: "stage skip directory", run: func() error {
			return runStage(context.Background(), app, []string{"--plan", "--root", root, "--skip-dirs", ".."})
		}, want: "skip directory"},
		{name: "stage confirmation", run: func() error { return runStage(context.Background(), app, []string{"--apply", "--root", root}) }, want: "confirmation"},
		{name: "list unknown flag", run: func() error { return runStageList(app, []string{"--unknown"}) }, want: "flag provided but not defined"},
		{name: "list positional", run: func() error { return runStageList(app, []string{"unexpected"}) }, want: "does not accept positional"},
		{name: "list format", run: func() error { return runStageList(app, []string{"--root", root, "--format", "yaml"}) }, want: "unknown stage list format"},
		{name: "list root", run: func() error { return runStageList(app, []string{"--root", missingRoot}) }, want: "does not exist"},
		{name: "diff unknown flag", run: func() error { return runStageDiff(app, []string{"stage-id", "--unknown"}) }, want: "flag provided but not defined"},
		{name: "diff trailing id", run: func() error { return runStageDiff(app, []string{"--root", root, "stage-missing"}) }, want: "requires exactly one"},
		{name: "diff root", run: func() error { return runStageDiff(app, []string{"stage-id", "--root", missingRoot}) }, want: "does not exist"},
		{name: "discard unknown flag", run: func() error { return runStageDiscard(app, []string{"stage-id", "--unknown"}) }, want: "flag provided but not defined"},
		{name: "discard trailing id", run: func() error { return runStageDiscard(app, []string{"--yes", "--root", root, "stage-missing"}) }, want: "requires exactly one"},
		{name: "discard root", run: func() error { return runStageDiscard(app, []string{"stage-id", "--yes", "--root", missingRoot}) }, want: "does not exist"},
		{name: "commit unknown flag", run: func() error { return runCommit(context.Background(), app, []string{"stage-id", "--unknown"}) }, want: "flag provided but not defined"},
		{name: "commit trailing id", run: func() error {
			return runCommit(context.Background(), app, []string{"--plan", "--root", root, "stage-missing"})
		}, want: "requires exactly one"},
		{name: "commit format", run: func() error {
			return runCommit(context.Background(), app, []string{"stage-id", "--plan", "--format", "yaml"})
		}, want: "unknown commit format"},
		{name: "commit root", run: func() error {
			return runCommit(context.Background(), app, []string{"stage-id", "--plan", "--root", missingRoot})
		}, want: "does not exist"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestStageCommandsRejectUnknownFlagsBeforeOpeningState(t *testing.T) {
	for _, command := range []string{"stage", "stage list", "stage diff", "stage edit", "stage difftool", "stage mergetool", "stage discard", "commit"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			var output, diagnostics bytes.Buffer
			app := newCLI(&output, &diagnostics, strings.NewReader(""))
			app.threadpointHome = dir
			args := append(strings.Fields(command), "--not-a-supported-flag")
			err := app.run(context.Background(), args)
			if err == nil || exitCode(err) != ExitUsage {
				t.Fatalf("invalid flags: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid invocation changed state: %v %v", entries, err)
			}
		})
	}
}

func TestStageCommandsRefuseAnInvalidProductHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("THREADPOINT_HOME", "relative-home")
	terminal, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	for _, command := range []struct {
		name string
		args []string
	}{
		{"plan", []string{"--plan"}},
		{"list", []string{"list"}},
		{"diff", []string{"diff", "stage-id"}},
		{"edit", []string{"edit", "stage-id"}},
		{"difftool", []string{"difftool", "stage-id"}},
		{"mergetool", []string{"mergetool", "stage-id"}},
		{"discard", []string{"discard", "stage-id", "--yes"}},
		{"commit", []string{"stage-id", "--plan"}},
	} {
		t.Run(command.name, func(t *testing.T) {
			root := t.TempDir()
			var output, diagnostics bytes.Buffer
			app := newCLI(&output, &diagnostics, terminal)
			args := append(append([]string(nil), command.args...), "--root", root)
			var err error
			if command.name == "commit" {
				err = runCommit(context.Background(), app, args)
			} else {
				err = runStage(context.Background(), app, args)
			}
			if err == nil || !strings.Contains(err.Error(), "must be absolute") {
				t.Fatalf("invalid product home: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid namespace changed project: %v %v", entries, err)
			}
		})
	}
}
