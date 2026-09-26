// SPDX-License-Identifier: Apache-2.0

package review

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergePassesGitLikeInputsAndReadsResult(t *testing.T) {
	dir := t.TempDir()
	result := filepath.Join(dir, "result")
	tool := writeTool(t, dir, "merge", "printf '%s|%s|%s|%s|%s|%s|%s|%s' \"$1\" \"$2\" \"$3\" \"$4\" \"$BASE\" \"$LOCAL\" \"$REMOTE\" \"$MERGED\" > \"$REVIEW_RESULT\"\nprintf 'merged' > \"$4\"\n")
	t.Setenv("REVIEW_RESULT", result)
	t.Setenv("MERGE", tool)

	got, err := Merge(context.Background(), Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, "rules/policy.yaml", []byte("base"), []byte("local"), []byte("remote"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "merged" {
		t.Fatalf("merged content = %q", got)
	}
	parts := strings.Split(readResult(t, result), "|")
	if len(parts) != 8 {
		t.Fatalf("merge inputs = %#v", parts)
	}
	for index := 0; index < 4; index++ {
		if parts[index] != parts[index+4] || !strings.HasSuffix(parts[index], ".yaml") {
			t.Fatalf("merge input %d = %q/%q", index, parts[index], parts[index+4])
		}
	}
}
