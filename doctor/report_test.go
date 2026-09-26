// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"testing"
)

func TestReportBuilderDeduplicatesAndRejectsEmptyInputs(t *testing.T) {
	builder := reportBuilder{
		report: &Report{OK: true},
		seen:   map[string]bool{},
		cmds:   map[string]bool{},
		assets: map[string]map[string]bool{},
	}
	builder.recordAsset("", "AGENTS.md")
	builder.recordAsset("project", "../outside")
	builder.recordAsset("project", ".agents/knowledge/note.md")
	builder.recordAsset("project", ".agents/knowledge/note.md")
	if len(builder.assets) != 1 || len(builder.assets["project"]) != 1 {
		t.Fatalf("deduplicated assets = %#v", builder.assets)
	}

	builder.addFinding(SeverityWarning, "fixture", "project", "", "message")
	builder.addFinding(SeverityWarning, "fixture", "project", "", "message")
	builder.addCommand("  ")
	builder.addCommand("threadpoint status")
	builder.addCommand("threadpoint status")
	if len(builder.report.Findings) != 1 || len(builder.report.SuggestedCommands) != 1 {
		t.Fatalf("deduplicated report = %#v", builder.report)
	}

	if report := checkGit(context.Background(), "project", nil); len(report.Checked) != 0 || report.InWorktree {
		t.Fatalf("empty Git check = %#v", report)
	}
	if command := gitPathCommand("project", "status --short", []GitPath{{}, {Path: "AGENTS.md"}, {Path: "AGENTS.md"}}); command == "" {
		t.Fatal("non-empty Git path command was omitted")
	}
	if command := gitPathCommand("project", "status --short", []GitPath{{}}); command != "" {
		t.Fatalf("empty Git path command = %q", command)
	}
	if got := shellQuote(""); got != "''" {
		t.Fatalf("empty shell argument = %q", got)
	}
	if rank := severityRank(Severity("other")); rank != 2 {
		t.Fatalf("unknown severity rank = %d", rank)
	}
}
