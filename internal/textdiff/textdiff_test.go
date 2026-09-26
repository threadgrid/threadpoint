// SPDX-License-Identifier: Apache-2.0

package textdiff

import (
	"strings"
	"testing"
)

func TestUnifiedEmptyWhenEqual(t *testing.T) {
	if got := Unified("AGENTS.md", "current", "proposed", "same\n", "same\n"); got != "" {
		t.Fatalf("expected empty diff for equal inputs, got %q", got)
	}
}

func TestUnifiedShowsAppendedLinesAsAdditions(t *testing.T) {
	// Appending to an existing file should surface the
	// appended lines as '+', keeping existing lines as unchanged context rather
	// than churning the whole file.
	current := "# Agent Guide\n\nExisting line.\n"
	proposed := current + "\nAppended block.\n"
	diff := Unified("AGENTS.md", "current", "proposed", current, proposed)

	for _, want := range []string{
		"--- AGENTS.md current",
		"+++ AGENTS.md proposed",
		" # Agent Guide",
		" Existing line.",
		"+Appended block.",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "-# Agent Guide") || strings.Contains(diff, "-Existing line.") {
		t.Fatalf("existing lines should be context, not removals:\n%s", diff)
	}
}

func TestUnifiedReplacedLine(t *testing.T) {
	diff := Unified("f", "current", "backup", "alpha\nbeta\n", "alpha\ngamma\n")
	if !strings.Contains(diff, " alpha") {
		t.Fatalf("expected unchanged context line:\n%s", diff)
	}
	if !strings.Contains(diff, "-beta") || !strings.Contains(diff, "+gamma") {
		t.Fatalf("expected -beta/+gamma:\n%s", diff)
	}
}
