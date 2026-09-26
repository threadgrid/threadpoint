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

func TestDiffPassesPathsAndTreatsDifferenceAsSuccess(t *testing.T) {
	dir := t.TempDir()
	result := filepath.Join(dir, "result")
	tool := writeTool(t, dir, "diff", "printf '%s|%s|%s|%s' \"$1\" \"$2\" \"$LOCAL\" \"$REMOTE\" > \"$REVIEW_RESULT\"\nexit 1\n")
	t.Setenv("REVIEW_RESULT", result)
	t.Setenv("DIFF", tool)

	if err := Diff(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "AGENTS.md", []byte("source"), []byte("staged")); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(readResult(t, result), "|")
	if len(parts) != 4 || parts[0] != parts[2] || parts[1] != parts[3] {
		t.Fatalf("diff arguments and environment = %#v", parts)
	}
	for _, path := range parts[:2] {
		if !strings.HasSuffix(path, ".md") {
			t.Fatalf("diff scratch path = %q, want .md", path)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("review directory still exists for %q: %v", path, err)
		}
	}
}
