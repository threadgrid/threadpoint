// SPDX-License-Identifier: Apache-2.0

package review

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestReviewSubprocessUsesInjectedStreams(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	var stdout, stderr bytes.Buffer
	streams := Streams{Stdin: strings.NewReader("private input\n"), Stdout: &stdout, Stderr: &stderr}
	if err := run(context.Background(), streams, "sh -c 'cat; printf warning >&2'", nil, nil); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "private input\n" || stderr.String() != "warning" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
