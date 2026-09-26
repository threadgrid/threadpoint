// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/stage"
)

func TestStageCommandsPlanApplyDiffAndDiscardDiscoveredArtifact(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("fixture guidance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = home
	classify := ".codex/AGENTS.md=project-shared"
	if err := runStage(context.Background(), app, []string{"--root", root, "--providers", "codex", "--classify", classify, "--plan"}); err != nil {
		t.Fatalf("stage plan: %v", err)
	}
	if !strings.Contains(stdout.String(), "stages: 1") {
		t.Fatalf("stage plan output = %q", stdout.String())
	}
	stdout.Reset()
	if err := runStage(context.Background(), app, []string{"--root", root, "--providers", "codex", "--classify", classify, "--apply", "--yes"}); err != nil {
		t.Fatalf("stage apply: %v", err)
	}
	records, err := stage.List(root, home)
	if err != nil || len(records) != 1 {
		t.Fatalf("staged records = %#v, %v", records, err)
	}
	id := records[0].ID
	rawPath := filepath.Join(root, "private-review")
	if err := stage.ReplaceContent(root, home, id, []byte("path="+rawPath+"\n")); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runStage(context.Background(), app, []string{"diff", id, "--root", root}); err != nil {
		t.Fatalf("stage diff: %v", err)
	}
	if !strings.Contains(stdout.String(), rawPath) {
		t.Fatalf("stage diff output = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "prints raw local artifact content") {
		t.Fatalf("stage diff warning = %q", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	app.nonInteractive = true
	for _, command := range []string{"difftool", "mergetool"} {
		if err := runStage(context.Background(), app, []string{command, id, "--root", root}); err == nil || !strings.Contains(err.Error(), "requires an interactive terminal") {
			t.Fatalf("noninteractive stage %s error = %v", command, err)
		}
	}
	app.nonInteractive = false
	if err := runStage(context.Background(), app, []string{"discard", id, "--root", root, "--yes"}); err != nil {
		t.Fatalf("stage discard: %v", err)
	}
	records, err = stage.List(root, home)
	if err != nil || len(records) != 0 {
		t.Fatalf("records after discard = %#v, %v", records, err)
	}
}

func TestStageMergetoolRebasesProvenanceBeforeCommit(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(root, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("base source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := stage.Create(context.Background(), stage.Options{
		Root: root, ThreadpointHome: home,
		Inputs: []stage.Input{{Provider: "codex", Source: ".codex/AGENTS.md", Kind: stage.KindInstruction, RequiredScope: stage.ScopeProjectShared}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, %v", report, err)
	}
	if err := os.WriteFile(source, []byte("remote source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(t.TempDir(), "merge.sh")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' 'merged review' > \"$4\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MERGE", tool)
	stdin, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = home
	app.stdin = stdin
	id := report.Stages[0].ID
	if err := runStage(context.Background(), app, []string{"mergetool", id, "--root", root}); err != nil {
		t.Fatalf("stage mergetool: %v", err)
	}
	if _, err := stage.Commit(context.Background(), stage.CommitOptions{Root: root, ThreadpointHome: home, ID: id}); err != nil {
		t.Fatalf("commit merged review: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil || !strings.Contains(string(body), "merged review") {
		t.Fatalf("committed merged review = %q, %v", body, err)
	}
}

func TestStageEditAndDifftoolUseExplicitConfiguredTools(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(project, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("original guidance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := stage.Create(context.Background(), stage.Options{
		Root: project, ThreadpointHome: home,
		Inputs: []stage.Input{{
			Provider: "codex", Source: ".codex/AGENTS.md", Kind: stage.KindInstruction,
			RequiredScope: stage.ScopeProjectShared,
		}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, err=%v", report, err)
	}
	id := report.Stages[0].ID

	toolDir := t.TempDir()
	editor := filepath.Join(toolDir, "editor.sh")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s\\n' 'edited guidance' > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	diff := filepath.Join(toolDir, "diff.sh")
	if err := os.WriteFile(diff, []byte("#!/bin/sh\ntest -f \"$LOCAL\"\ntest -f \"$REMOTE\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", editor)
	t.Setenv("DIFF", diff)
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if !interactiveInput(stdin) {
		t.Skip("platform null device is not reported as an interactive character device")
	}
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = home
	app.stdin = stdin

	if err := runStage(context.Background(), app, []string{"edit", id, "--root", project}); err != nil {
		t.Fatalf("stage edit: %v", err)
	}
	content, err := stage.ReadContent(project, home, id)
	if err != nil || !bytes.Equal(content, []byte("edited guidance\n")) {
		t.Fatalf("edited stage content = %q, err=%v", content, err)
	}
	if err := runStage(context.Background(), app, []string{"difftool", id, "--root", project}); err != nil {
		t.Fatalf("stage difftool: %v", err)
	}
}

func TestInteractiveStageToolsValidateFlagsRootsAndConfiguredTools(t *testing.T) {
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if !interactiveInput(stdin) {
		t.Skip("platform null device is not reported as an interactive character device")
	}
	root := t.TempDir()
	missingRoot := filepath.Join(t.TempDir(), "missing")
	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	app.threadpointHome = t.TempDir()
	app.stdin = stdin

	for _, tool := range []string{"edit", "difftool", "mergetool"} {
		t.Run(tool, func(t *testing.T) {
			if err := runStage(context.Background(), app, []string{tool, "stage-id", "--unknown"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("unknown flag error = %v", err)
			}
			if err := runStage(context.Background(), app, []string{tool, "--root", root, "stage-missing"}); err == nil || !strings.Contains(err.Error(), "requires exactly one") {
				t.Fatalf("trailing stage ID error = %v", err)
			}
			if err := runStage(context.Background(), app, []string{tool, "stage-id", "--root", missingRoot}); err == nil || !strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("missing root error = %v", err)
			}
		})
	}
}

func TestStageToolsRefuseMissingToolAndUnchangedMergeSource(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	source := filepath.Join(project, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("original guidance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := stage.Create(context.Background(), stage.Options{
		Root: project, ThreadpointHome: home,
		Inputs: []stage.Input{{
			Provider: "codex", Source: ".codex/AGENTS.md", Kind: stage.KindInstruction,
			RequiredScope: stage.ScopeProjectShared,
		}},
	})
	if err != nil || len(report.Stages) != 1 {
		t.Fatalf("stage report = %#v, err=%v", report, err)
	}
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if !interactiveInput(stdin) {
		t.Skip("platform null device is not reported as an interactive character device")
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv("DIFF", "")
	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	app.threadpointHome = home
	app.stdin = stdin
	id := report.Stages[0].ID

	if err := runStage(context.Background(), app, []string{"edit", id, "--root", project}); err == nil || !strings.Contains(err.Error(), "VISUAL or $EDITOR") {
		t.Fatalf("missing editor error = %v", err)
	}
	if err := runStage(context.Background(), app, []string{"difftool", id, "--root", project}); err == nil || !strings.Contains(err.Error(), "DIFF") {
		t.Fatalf("missing diff tool error = %v", err)
	}
	if err := runStage(context.Background(), app, []string{"mergetool", id, "--root", project}); err == nil || !strings.Contains(err.Error(), "source has not changed") {
		t.Fatalf("unchanged source error = %v", err)
	}
}

func TestStageToolsRejectMissingStageAfterInteractiveValidation(t *testing.T) {
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if !interactiveInput(stdin) {
		t.Skip("platform null device is not reported as an interactive character device")
	}
	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	app.stdin = stdin
	app.threadpointHome = t.TempDir()
	for _, args := range [][]string{{"edit", "stage-missing", "--root", t.TempDir()}, {"difftool", "stage-missing", "--root", t.TempDir()}} {
		if err := runStage(context.Background(), app, args); err == nil {
			t.Fatalf("stage tool invocation succeeded without staged state: %v", args)
		}
	}
}
