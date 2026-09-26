// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCommandArgumentScanningRecognizesFormatAndCommandName(t *testing.T) {
	for _, args := range [][]string{{"fixture-one", "--format=json"}, {"fixture-two", "-format=json"}} {
		if !shouldEmitJSONError(newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader("")), args) {
			t.Errorf("JSON error output not detected for %v", args)
		}
	}
	if got := commandNameFromArgs([]string{"--quiet", "fixture-command"}); got != "fixture-command" {
		t.Fatalf("command after boolean flag = %q", got)
	}
	if got := commandNameFromArgs([]string{"--quiet"}); got != "" {
		t.Fatalf("flags-only command name = %q", got)
	}
}

func TestTopLevelHelp(t *testing.T) {
	stdout, stderr, err := runTestCLI(t)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	for _, want := range []string{
		"Usage:",
		"threadpoint [global flags] <command> [flags]",
		"Global flags:",
		"--home path",
		"--format text|json",
		"stage",
		"update",
		"doctor",
		"status",
		"init",
		"uninstall",
		"commit",
		"prune",
		"restore",
		"version",
		"Reference:",
		"Project commands discover their root from the current directory.",
		"optional command flag --root PATH",
		"docs/glossary.md",
		"docs/canonical-layout-guide.md",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("help missing %q:\n%s", want, stderr)
		}
	}
}

func TestUpdateCommandHelp(t *testing.T) {
	stdout, stderr, err := runTestCLI(t, "help", "update")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	for _, want := range []string{
		"threadpoint update [flags]",
		"threadpoint update <check|rollback|reminder> [flags]",
		`--max-age duration      cache freshness window (default "15m")`,
		"--yes                   confirm update for noninteractive use",
		"check       inspect stable or preview release metadata",
		"rollback    install the latest stable release",
		"reminder    manage reminders: status, enable, disable, dismiss",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("update help missing %q:\n%s", want, stderr)
		}
	}
}

func TestCommandHelp(t *testing.T) {
	for _, args := range [][]string{
		{"help", "status"},
		{"status", "--help"},
	} {
		stdout, stderr, err := runTestCLI(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		if stdout != "" {
			t.Fatalf("expected empty stdout, got %q", stdout)
		}
		if !strings.Contains(stderr, "threadpoint status [--root path]") {
			t.Fatalf("command help missing usage:\n%s", stderr)
		}
		if !strings.Contains(stderr, `project root override (optional; otherwise discover from current directory)`) {
			t.Fatalf("command help missing root flag:\n%s", stderr)
		}
		if !strings.Contains(stderr, "--skip-dirs list") {
			t.Fatalf("command help missing skip directory flag:\n%s", stderr)
		}
		if !strings.Contains(stderr, "Glossary: https://github.com/threadgrid/threadpoint/blob/main/docs/glossary.md") {
			t.Fatalf("command help missing glossary reference:\n%s", stderr)
		}
	}
}

func TestHelpFlagScannerSkipsStringFlagValues(t *testing.T) {
	if hasHelpFlag([]string{"--version", "--help"}) {
		t.Fatal("string flag value --help should not trigger command help")
	}
	if hasHelpFlag([]string{"--transform-command", "tool --help"}) {
		t.Fatal("quoted command containing --help should not trigger command help")
	}
	if hasHelpFlag([]string{"--backup", "--help"}) {
		t.Fatal("backup value --help should not trigger command help")
	}
	if hasHelpFlag([]string{"--confirm", "--help"}) {
		t.Fatal("confirmation phrase --help should not trigger command help")
	}
	if !hasHelpFlag([]string{"--root", ".", "--help"}) {
		t.Fatal("help flag after another option should still trigger command help")
	}
	if !hasHelpFlag([]string{"--help"}) {
		t.Fatal("direct help flag should trigger command help")
	}
}

func TestUninstallHelp(t *testing.T) {
	stdout, stderr, err := runTestCLI(t, "help", "uninstall")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	for _, want := range []string{
		"threadpoint uninstall [--dir path] [--plan|--apply] [--yes]",
		"--dir path  command directory",
		"--plan      print the uninstall plan",
		"--apply     remove the installer-managed bundle and metadata",
		"--yes       required with --apply",
		"text uninstall plan or apply report",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("uninstall help missing %q:\n%s", want, stderr)
		}
	}
}

func TestUpdateRejectsRemovedApplySubcommand(t *testing.T) {
	stdout, _, err := runTestCLI(t, "update", "apply", "--help")
	if err == nil || !strings.Contains(err.Error(), `unknown update subcommand "apply"`) {
		t.Fatalf("expected removed apply subcommand to fail, got %v", err)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
}

func TestSelfUpdateCommandIsNotSupported(t *testing.T) {
	removedCommand := "self-" + "update"
	for _, args := range [][]string{
		{removedCommand},
		{"help", removedCommand},
	} {
		stdout, _, err := runTestCLI(t, args...)
		want := "unknown command "
		if args[0] == "help" {
			want = "unknown help topic "
		}
		if err == nil || !strings.Contains(err.Error(), want+`"`+removedCommand+`"`) {
			t.Fatalf("expected removed update command to be unsupported for %v, got %v", args, err)
		}
		if stdout != "" {
			t.Fatalf("expected empty stdout, got %q", stdout)
		}
	}
}

func TestPruneRestoreAndDoctorHelp(t *testing.T) {
	for _, item := range []struct {
		command string
		want    string
	}{
		{"prune", "threadpoint prune [--root path] [--plan|--apply] [flags]"},
		{"restore", "threadpoint restore [--root path] [--plan|--apply] [flags]"},
		{"status", "threadpoint status [--root path] [flags]"},
	} {
		stdout, stderr, err := runTestCLI(t, "help", item.command)
		if err != nil {
			t.Fatal(err)
		}
		if stdout != "" {
			t.Fatalf("expected empty stdout, got %q", stdout)
		}
		if !strings.Contains(stderr, item.want) {
			t.Fatalf("%s help missing usage:\n%s", item.command, stderr)
		}
		if !strings.Contains(stderr, "--backup-namespace") {
			t.Fatalf("%s help missing backup namespace flag:\n%s", item.command, stderr)
		}
	}
}

func TestMapAndExportAreNotCommands(t *testing.T) {
	for _, name := range []string{"map", "export"} {
		_, _, err := runTestCLI(t, name)
		if err == nil {
			t.Fatalf("expected %s to fail", name)
		}
		if !strings.Contains(err.Error(), `unknown command "`+name+`"`) {
			t.Fatalf("unexpected error for %s: %v", name, err)
		}
	}
}

func TestBackupDirFlagIsRejected(t *testing.T) {
	removedBackupFlag := "--backup" + "-dir"
	_, _, err := runTestCLI(t, "restore", removedBackupFlag, "invalid", "--list")
	if err == nil {
		t.Fatalf("expected %s to fail", removedBackupFlag)
	}
	if !strings.Contains(err.Error(), "flag provided but not defined: "+removedBackupFlag[1:]) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCommandsRejectPositionalArguments(t *testing.T) {
	_, _, err := runTestCLI(t, "status", "unexpected")
	if err == nil {
		t.Fatal("expected positional argument to fail")
	}
	if !strings.Contains(err.Error(), "status does not accept positional arguments") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSelfUpdateHelpAndPresentation(t *testing.T) {
	for _, operation := range []updateOperation{updateOperationUpdate, updateOperationRollback} {
		var stdout, stderr bytes.Buffer
		app := newCLI(&stdout, &stderr, strings.NewReader(""))
		runUpdateApplyHelp(app, operation)
		if !strings.Contains(stderr.String(), "Usage:") || !strings.Contains(stderr.String(), "threadpoint update") {
			t.Fatalf("help for %q = %q", operation, stderr.String())
		}
	}

	var text bytes.Buffer
	report := selfUpdateReport{
		Command: "threadpoint update", Repo: "owner/repo", InstallDir: "/bin", BinaryPath: "/bundle/bin/threadpoint", LinkPath: "/bin/threadpoint", MetadataPath: "/state/install.json",
		CurrentVersion: "v1.0.0", LatestVersion: "v1.2.3", Source: "network", CheckedAt: "2026-07-01T00:00:00Z", UpdateAvailable: true, Updated: true,
		Archive: "threadpoint.tar.gz", ArchiveSHA256: "sum", Reason: "updated",
	}
	if err := printSelfUpdateText(&text, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"link: /bin/threadpoint", "archive: threadpoint.tar.gz", "archive sha256: sum", "code: update_applied"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report missing %q: %q", want, text.String())
		}
	}
	var jsonOut bytes.Buffer
	if err := printSelfUpdateReport(newCLI(&jsonOut, &bytes.Buffer{}, strings.NewReader("")), outputFormatJSON, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOut.String(), `"archive_sha256": "sum"`) {
		t.Fatalf("json report = %q", jsonOut.String())
	}
	if err := printSelfUpdateReport(newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader("")), "invalid", report); err == nil {
		t.Fatal("invalid self-update format should fail")
	}
}

func TestProjectFlagErrorUsageExplainsRootDiscovery(t *testing.T) {
	for _, command := range []string{"init", "status", "prune", "restore", "stage", "stage list", "stage diff", "stage edit", "stage difftool", "stage mergetool", "stage discard", "commit", "lock clear"} {
		t.Run(command, func(t *testing.T) {
			args := append(strings.Fields(command), "--bogus")
			_, stderr, err := runTestCLI(t, args...)
			if err == nil || !strings.Contains(stderr, "project root override (optional") || strings.Contains(stderr, `project root (default ".")`) {
				t.Fatalf("usage does not explain discovery: %q err=%v", stderr, err)
			}
		})
	}
}
