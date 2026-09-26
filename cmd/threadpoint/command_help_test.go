// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type helpOnlyIO struct{ t *testing.T }

func (h helpOnlyIO) Read([]byte) (int, error) {
	h.t.Error("help tried to read input")
	return 0, io.EOF
}
func (h helpOnlyIO) RoundTrip(*http.Request) (*http.Response, error) {
	h.t.Error("help tried to use the network")
	return nil, errors.New("help must be offline")
}

func TestCommandPathHelpIsConsistentAndHasNoSideEffects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: helpOnlyIO{t}}
	t.Cleanup(func() { http.DefaultClient = original })
	productHome := filepath.Join(t.TempDir(), "not-created")

	paths := []string{"init", "stage", "stage list", "stage diff", "stage edit", "stage difftool", "stage mergetool", "stage discard", "commit", "prune", "restore", "restore list", "status", "doctor", "version", "update", "update check", "update rollback", "update reminder", "update reminder status", "update reminder enable", "update reminder disable", "update reminder dismiss", "uninstall", "lock", "lock clear"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			words := strings.Fields(path)
			forms := [][]string{append([]string{"help"}, words...), append(append([]string{}, words...), "--help"), append(append([]string{}, words...), "-h")}
			forms = append(forms, append(append([]string{}, words...), "help"))
			if path == "lock" || path == "update reminder" {
				forms = append(forms, words)
			}
			var expected string
			for _, args := range forms {
				var stdout, stderr bytes.Buffer
				app := newCLI(&stdout, &stderr, helpOnlyIO{t})
				err := app.run(context.Background(), append([]string{"--home", productHome}, args...))
				if err != nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "threadpoint") {
					t.Fatalf("%v: stdout=%q stderr=%q error=%v", args, stdout.String(), stderr.String(), err)
				}
				if expected == "" {
					expected = stderr.String()
				} else if stderr.String() != expected {
					t.Fatalf("%v help differs from help path:\n%s\nwant:\n%s", args, stderr.String(), expected)
				}
				if _, err := os.Stat(productHome); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("help created product state: %v", err)
				}
			}
		})
	}
}

func TestRetiredCommandPathsAndUnknownHelpAreRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, path := range []string{"locks clear", "update reminders status", "update dismiss", "stage missing", "restore missing", "update reminder missing"} {
		for _, args := range [][]string{strings.Fields(path), append(strings.Fields(path), "--help"), append([]string{"help"}, strings.Fields(path)...)} {
			stdout, _, err := runTestCLI(t, args...)
			if err == nil || !strings.Contains(err.Error(), "unknown") || stdout != "" {
				t.Fatalf("%v: stdout=%q error=%v", args, stdout, err)
			}
		}
	}
}

func TestHelpScannerDoesNotInterpretOptionValuesOrTerminatedArguments(t *testing.T) {
	for _, args := range [][]string{{"--", "--help"}, {"--version", "--help"}, {"--backup", "-h"}, {"--classify", "--help"}, {"--skip-dirs", "-h"}} {
		if hasHelpFlag(args) {
			t.Errorf("mistook option value or terminated argument for help: %v", args)
		}
	}
	if !hasHelpFlag([]string{"--format", "json", "--help"}) {
		t.Fatal("missed help following option value")
	}
}

func TestBareGroupsInJSONModeReturnStructuredErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, path := range []string{"lock", "update reminder"} {
		var stdout, stderr bytes.Buffer
		args := append([]string{"--format", "json"}, strings.Fields(path)...)
		code := execute(context.Background(), &stdout, &stderr, strings.NewReader(""), args)
		want := ExitUsage

		var report errorReport
		if err := json.Unmarshal(stderr.Bytes(), &report); err != nil {
			t.Fatalf("%s: invalid error JSON: %q: %v", path, stderr.String(), err)
		}
		if code != want || report.OK || report.ExitCode != want || stdout.Len() != 0 {
			t.Fatalf("%s: code=%d report=%#v stdout=%q", path, code, report, stdout.String())
		}
		stderr.Reset()
		code = execute(context.Background(), &stdout, &stderr, strings.NewReader(""), append(args, "--help"))
		if code != ExitOK || !strings.Contains(stderr.String(), "usage:") && !strings.Contains(stderr.String(), "Usage:") {
			t.Fatalf("%s explicit help: code=%d output=%q", path, code, stderr.String())
		}
	}
}

func TestLockLeafHelpAndUsageFormatting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	help := func(path string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args := append([]string{"help"}, strings.Fields(path)...)
		if code := execute(context.Background(), &stdout, &stderr, strings.NewReader(""), args); code != ExitOK {
			t.Fatalf("help %s: %d %s", path, code, stderr.String())
		}
		return stderr.String()
	}
	parent, leaf := help("lock"), help("lock clear")
	if parent == leaf || !strings.Contains(parent, "<clear>") || !strings.Contains(leaf, "lock clear") || !strings.Contains(leaf, "--force") {
		t.Fatalf("parent=%q leaf=%q", parent, leaf)
	}
	for _, path := range []string{"restore list", "stage diff", "stage edit", "stage difftool", "stage mergetool"} {
		output := help(path)
		for _, line := range strings.Split(output, "\n") {
			if strings.TrimRight(line, " \t") != line {
				t.Errorf("%s has trailing whitespace: %q", path, line)
			}
		}
		if strings.HasSuffix(output, "\n\n") {
			t.Errorf("%s has extra final blank lines", path)
		}
	}
}

func TestCommandHelpTopicsRejectMissingParent(t *testing.T) {
	topics := map[string]commandHelpTopic{
		"update":                  {},
		"update reminder dismiss": {},
	}
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Error("missing parent was silently accepted")
		}
		if _, exists := topics["update reminder"]; exists {
			t.Error("normalization created an unregistered parent")
		}
	}()
	normalizeCommandHelpTopics(topics)
}

func TestCommandHelpTopicsNormalizeRegisteredParents(t *testing.T) {
	topics := map[string]commandHelpTopic{
		"update":                  {},
		"update reminder":         {},
		"update reminder dismiss": {},
	}
	normalizeCommandHelpTopics(topics)
	for path, topic := range topics {
		if topic.path != path {
			t.Errorf("topic %q normalized to %q", path, topic.path)
		}
		if want := path != "update reminder dismiss"; topic.children != want {
			t.Errorf("topic %q children=%v, want %v", path, topic.children, want)
		}
	}
}

func TestHelpFlagGeneratorEnforcesFlagSetName(t *testing.T) {
	for _, tc := range []struct {
		name, declaration string
		valid             bool
	}{
		{"short declaration", "fs := flags.NewFlagSet(\"test\", flags.ContinueOnError)", true},
		{"var declaration", "var fs = flags.NewFlagSet(\"test\", flags.ContinueOnError)", true},
		{"renamed short declaration", "options := flags.NewFlagSet(\"test\", flags.ContinueOnError)", false},
		{"renamed var declaration", "var options = flags.NewFlagSet(\"test\", flags.ContinueOnError)", false},
		{"unassigned constructor", "consume(flags.NewFlagSet(\"test\", flags.ContinueOnError))", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := "package main\nimport flags \"flag\"\nfunc declare() {\n" + tc.declaration + "\nfs.String(\"root\", \"\", \"\")\n}\n"
			if err := os.WriteFile(filepath.Join(dir, "flags.go"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, check := range []bool{false, true} {
				args := []string{"run", "../../scripts/cli-help-flags.go", "-dir", dir}
				if check {
					args = append(args, "-check")
				}
				output, err := exec.CommandContext(t.Context(), "go", args...).CombinedOutput()
				if tc.valid {
					if err != nil {
						t.Fatalf("check=%v: %v\n%s", check, err, output)
					}
				} else if err == nil || !strings.Contains(string(output), "flag.NewFlagSet must be assigned directly to fs") {
					t.Fatalf("check=%v: expected receiver diagnostic, got %v\n%s", check, err, output)
				}
			}
		})
	}
}

func TestHelpFlagGeneratorEnforcesHelperParameterName(t *testing.T) {
	for _, tc := range []struct {
		name, importName, parameter, body string
		valid                             bool
	}{
		{"named fs", "flag", "fs *flag.FlagSet", "fs.String(\"gamma\", \"\", \"\")", true},
		{"renamed parameter", "flag", "set *flag.FlagSet", "set.String(\"gamma\", \"\", \"\")", false},
		{"aliased import", "flags", "set *flags.FlagSet", "set.String(\"gamma\", \"\", \"\")", false},
		{"dot import", ".", "set *FlagSet", "set.String(\"gamma\", \"\", \"\")", false},
		{"grouped parameters", "flag", "fs, set *flag.FlagSet", "set.String(\"gamma\", \"\", \"\")", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := "package main\nimport " + tc.importName + " \"flag\"\nfunc addFlags(" + tc.parameter + ") { " + tc.body + " }\n"
			// Keep another value flag so a skipped helper still generates valid Go.
			flagType := tc.importName + ".FlagSet"
			if tc.importName == "." {
				flagType = "FlagSet"
			}
			source += "func baseline(fs *" + flagType + ") { fs.String(\"root\", \"\", \"\") }\n"
			if err := os.WriteFile(filepath.Join(dir, "flags.go"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, check := range []bool{false, true} {
				args := []string{"run", "../../scripts/cli-help-flags.go", "-dir", dir}
				if check {
					args = append(args, "-check")
				}
				output, err := exec.CommandContext(t.Context(), "go", args...).CombinedOutput()
				if !tc.valid {
					if err == nil || !strings.Contains(string(output), "*flag.FlagSet parameter must be named fs") {
						t.Fatalf("check=%v: expected parameter diagnostic, got %v\n%s", check, err, output)
					}
					continue
				}
				if err != nil {
					t.Fatalf("check=%v: %v\n%s", check, err, output)
				}
				generated, err := os.ReadFile(filepath.Join(dir, "help_flags_generated.go"))
				if err != nil || !strings.Contains(string(generated), "\"gamma\"") {
					t.Fatalf("helper flag missing: %v\n%s", err, generated)
				}
			}
		})
	}
}

func TestGeneratedHelpFlagsMatchDeclarations(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "run", "../../scripts/cli-help-flags.go", "-dir", ".", "-check", "-spdx", "Apache-2.0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("help flag metadata: %v\n%s", err, output)
	}
}
