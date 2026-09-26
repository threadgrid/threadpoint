// SPDX-License-Identifier: Apache-2.0

package editor

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/review"
)

func TestOpenPropagatesMissingEditorConfiguration(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")

	got, err := Open(context.Background(), review.Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "private stage content")
	if err == nil || !strings.Contains(err.Error(), "$VISUAL or $EDITOR") {
		t.Fatalf("missing editor error = %v", err)
	}
	if got != "" {
		t.Fatalf("content returned after editor failure = %q", got)
	}
}

func TestOpenUsesShellQuotingForEditorCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "editor dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "edit script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", "'"+strings.ReplaceAll(script, "'", "'\\''")+"' 'two words'")
	t.Setenv("VISUAL", "")

	got, err := Open(context.Background(), review.Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if got != "two words\n" {
		t.Fatalf("edited content = %q, want quoted editor arg", got)
	}
}
