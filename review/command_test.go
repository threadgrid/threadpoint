// SPDX-License-Identifier: Apache-2.0

package review

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewToolsPropagateTemporaryDirectoryFailure(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(tempFile, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tempFile)
	t.Setenv("VISUAL", "true")
	t.Setenv("EDITOR", "")
	t.Setenv("DIFF", "true")
	t.Setenv("MERGE", "true")

	if _, err := Edit(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "notes.md", []byte("body")); err == nil {
		t.Fatal("Edit unexpectedly created a scratch directory under a file")
	}
	if err := Diff(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "notes.md", []byte("one"), []byte("two")); err == nil {
		t.Fatal("Diff unexpectedly created a scratch directory under a file")
	}
	if _, err := Merge(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "notes.md", []byte("base"), []byte("local"), []byte("remote")); err == nil {
		t.Fatal("Merge unexpectedly created a scratch directory under a file")
	}
}

func TestReviewToolsPropagateConfiguredCommandFailure(t *testing.T) {
	dir := t.TempDir()
	failingTool := writeTool(t, dir, "failure", "exit 23\n")
	t.Setenv("VISUAL", failingTool)
	t.Setenv("EDITOR", "")
	t.Setenv("DIFF", failingTool)
	t.Setenv("MERGE", failingTool)

	if _, err := Edit(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "notes.md", []byte("body")); err == nil {
		t.Fatal("Edit unexpectedly ignored editor failure")
	}
	if err := Diff(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "notes.md", []byte("one"), []byte("two")); err == nil {
		t.Fatal("Diff unexpectedly treated exit status 23 as a normal difference")
	}
	if _, err := Merge(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "notes.md", []byte("base"), []byte("local"), []byte("remote")); err == nil {
		t.Fatal("Merge unexpectedly ignored merge-tool failure")
	}
}

func TestRunRejectsEmptyCommandBeforeExecution(t *testing.T) {
	err := run(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, " ", []string{"artifact"}, map[string]string{"LOCAL": "artifact"})
	if err == nil || !strings.Contains(err.Error(), "command must not be empty") {
		t.Fatalf("empty command error = %v", err)
	}
}

func TestReviewToolsRequireExplicitConfiguration(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv("DIFF", "")
	t.Setenv("MERGE", "")
	if _, err := Edit(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "source", []byte("body")); err == nil {
		t.Fatal("Edit unexpectedly succeeded")
	}
	if err := Diff(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "source", []byte("one"), []byte("two")); err == nil {
		t.Fatal("Diff unexpectedly succeeded")
	}
	if _, err := Merge(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "source", []byte("base"), []byte("local"), []byte("remote")); err == nil {
		t.Fatal("Merge unexpectedly succeeded")
	}
}

func TestEnvironmentOverridesInheritedReviewVariables(t *testing.T) {
	t.Setenv("LOCAL", "inherited")
	values := environment(map[string]string{"LOCAL": "review-copy", "REMOTE": "source-copy"})
	joined := "\n" + strings.Join(values, "\n") + "\n"
	if strings.Count(joined, "\nLOCAL=") != 1 || !strings.Contains(joined, "\nLOCAL=review-copy\n") {
		t.Fatalf("LOCAL environment = %q", joined)
	}
	if !strings.Contains(joined, "\nREMOTE=source-copy\n") {
		t.Fatalf("REMOTE environment = %q", joined)
	}
}
