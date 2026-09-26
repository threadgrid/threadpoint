// SPDX-License-Identifier: Apache-2.0

package prune

import (
	"strings"
	"testing"
)

func TestFormatTextRendersPlansReportsAndEmptyValues(t *testing.T) {
	if got := FormatPlanText(nil); got != "command: threadpoint prune\nplan: unavailable\n" {
		t.Fatalf("FormatPlanText(nil) = %q", got)
	}
	if got := FormatReportText(nil); got != "command: threadpoint prune\nreport: unavailable\n" {
		t.Fatalf("FormatReportText(nil) = %q", got)
	}

	plan := &Plan{
		Root: "/work", Warnings: []string{"review first"},
		Candidates: []Candidate{{Path: ".agents/rules/old.md", Status: StatusChanged, Code: "prune_artifact_changed", Message: "Content changed."}},
	}
	planText := FormatPlanText(plan)
	for _, want := range []string{"root: /work", "candidates: 1", "warnings: 1", "  - changed .agents/rules/old.md", "    Content changed. (code: prune_artifact_changed)"} {
		if !strings.Contains(planText, want) {
			t.Errorf("FormatPlanText() missing %q in %q", want, planText)
		}
	}

	reportText := FormatReportText(&Report{
		Root: "/work", Applied: true, Plan: plan, Warnings: []string{"restarted"},
		Resolutions: []Resolution{{Path: ".agents/rules/old.md", Status: StatusReady, Removed: true, Backup: "/backup", Recovery: "/recovery", Messages: []string{"removed"}}},
	})
	for _, want := range []string{"applied: true", "resolutions: 1", "  - ready .agents/rules/old.md removed=true", "    backup: /backup", "    recovery: /recovery", "    removed"} {
		if !strings.Contains(reportText, want) {
			t.Errorf("FormatReportText() missing %q in %q", want, reportText)
		}
	}
}

func TestFormatReportTextIncludesBackupAndRecoveryPaths(t *testing.T) {
	text := FormatReportText(&Report{
		Root:    "/project",
		Applied: true,
		Resolutions: []Resolution{{
			Path:     "CLAUDE.md",
			Status:   StatusReady,
			Backup:   "/state/backup",
			Recovery: "/project/.threadpoint-prune-recovery-token/original",
			Removed:  true,
		}},
	})
	for _, want := range []string{"backup: /state/backup", "recovery: /project/.threadpoint-prune-recovery-token/original"} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted report missing %q:\n%s", want, text)
		}
	}
}
