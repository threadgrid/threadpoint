// SPDX-License-Identifier: Apache-2.0

package layout

import (
	"strings"
	"testing"
)

func TestFormatTextRendersIssuesAndWarnings(t *testing.T) {
	got := FormatText(Report{
		Root:     "/work",
		Issues:   []Issue{{Path: "AGENTS.md", Message: "missing"}},
		Warnings: []Issue{{Path: ".agents/skills", Message: "optional"}},
	})
	for _, want := range []string{
		"root: /work", "ok: false", "issues: 1", "  - AGENTS.md: missing",
		"warnings: 1", "  - .agents/skills: optional",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatText() missing %q in %q", want, got)
		}
	}
}
