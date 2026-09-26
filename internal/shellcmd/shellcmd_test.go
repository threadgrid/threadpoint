// SPDX-License-Identifier: Apache-2.0

package shellcmd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestCommandContextPassesTwoAndThreeArgumentsExactly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell behavior")
	}
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result")
	script := filepath.Join(dir, "argument recorder.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|%s|%s|%s' \"$#\" \"$1\" \"$2\" \"$3\" > \"$THREADPOINT_SHELLCMD_RESULT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("THREADPOINT_SHELLCMD_RESULT", resultPath)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "two", args: []string{"one", "two words"}, want: "2|one|two words|"},
		{name: "three", args: []string{"one", "two words", "three's"}, want: "3|one|two words|three's"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd, err := CommandContext(context.Background(), quote(script), test.args...)
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != test.want {
				t.Fatalf("recorded arguments = %q, want %q", body, test.want)
			}
		})
	}
}

func TestCommandContextRejectsEmptyCommand(t *testing.T) {
	if _, err := CommandContext(context.Background(), " \t ", "argument"); err == nil {
		t.Fatal("expected empty command to be rejected")
	}
	if _, err := CommandContext(context.Background(), "printf", "one", "two", "three", "four", "five"); err == nil {
		t.Fatal("expected too many positional arguments to be rejected")
	}
}

func TestCommandContextPassesMultipleArgumentsAfterTrailingComment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell behavior")
	}
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result")
	script := filepath.Join(dir, "tool script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|%s|%s|%s' \"$1\" \"$2\" \"$3\" \"$4\" > \"$THREADPOINT_SHELLCMD_RESULT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("THREADPOINT_SHELLCMD_RESULT", resultPath)

	cmd, err := CommandContext(context.Background(), quote(script)+" # comment", "one", "two words", "three", "four")
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one|two words|three|four" {
		t.Fatalf("arguments = %q", got)
	}
}

func TestCommandContextBuildsDirectShellCommandWithoutAnArgument(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell behavior")
	}
	cmd, err := CommandContext(context.Background(), "printf threadpoint")
	if err != nil {
		t.Fatal(err)
	}
	if got := cmd.Args; len(got) != 3 || got[0] != "sh" || got[1] != "-c" || got[2] != "printf threadpoint" {
		t.Fatalf("direct shell command = %#v", got)
	}
}

func TestCommandContextPreservesArgumentAfterTrailingComment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell behavior")
	}
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result")
	script := filepath.Join(dir, "editor script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$1\" > \"$THREADPOINT_SHELLCMD_RESULT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("THREADPOINT_SHELLCMD_RESULT", resultPath)

	cmd, err := CommandContext(context.Background(), quote(script)+" # comment", "path with spaces")
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "path with spaces" {
		t.Fatalf("editor argument = %q, want preserved argument", got)
	}
}

func TestWindowsCommandQuotesConfiguredArguments(t *testing.T) {
	cmd, err := commandContextForOS(context.Background(), "windows", "editor", "two words", `quoted"value`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd", "/C", `editor "two words" "quoted\"value"`}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("Windows command = %#v, want %#v", cmd.Args, want)
	}
}
