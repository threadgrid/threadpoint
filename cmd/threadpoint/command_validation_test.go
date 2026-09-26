// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestCommandValidationRejectsInvalidArgumentsBeforeMutation(t *testing.T) {
	home := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("THREADPOINT_HOME", "")
	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	commands := append([]command(nil), app.commands...)
	commands = append(commands,
		command{name: "update-check", run: runUpdateCheck},
		command{name: "update-dismiss", run: func(_ context.Context, app *cli, args []string) error { return runUpdateDismiss(app, args) }},
		command{name: "stage-list", run: func(_ context.Context, app *cli, args []string) error { return runStageList(app, args) }},
		command{name: "stage-diff", run: func(_ context.Context, app *cli, args []string) error { return runStageDiff(app, args) }},
		command{name: "stage-edit", run: runStageEdit},
		command{name: "stage-difftool", run: runStageDifftool},
		command{name: "stage-mergetool", run: runStageMergetool},
		command{name: "stage-discard", run: func(_ context.Context, app *cli, args []string) error { return runStageDiscard(app, args) }},
	)
	for _, action := range []string{"status", "enable", "disable"} {
		commands = append(commands, command{name: "reminders-" + action, run: func(_ context.Context, app *cli, args []string) error {
			return runUpdateReminders(app, append([]string{action}, args...))
		}})
	}
	for _, command := range commands {
		for _, invalid := range []struct {
			name string
			args []string
		}{
			{"unknown-flag", []string{"--definitely-not-a-real-flag"}},
			{"invalid-format", []string{"--format", "unsupported-format"}},
			{"extra-positionals", []string{"unexpected-one", "unexpected-two"}},
		} {
			t.Run(command.name+"/"+invalid.name, func(t *testing.T) {
				if err := command.run(context.Background(), app, invalid.args); err == nil {
					t.Fatal("invalid arguments accepted")
				}
			})
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid flags wrote state: %v %v", entries, err)
	}
}
