// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestFormatTextRendersPlansReportsAndEmptyValues(t *testing.T) {
	if got := FormatPlanText(nil); got != "command: threadpoint restore\nplan: unavailable\n" {
		t.Fatalf("FormatPlanText(nil) = %q", got)
	}
	if got := FormatReportText(nil); got != "command: threadpoint restore\nreport: unavailable\n" {
		t.Fatalf("FormatReportText(nil) = %q", got)
	}

	plan := &Plan{
		Root: "/work", BackupRun: "run-1", Operation: "stage", Warnings: []string{"review first"},
		Candidates: []Candidate{{Path: "AGENTS.md", Status: StatusReady, Code: "restore_target_file_missing", Message: "Missing from workspace."}},
	}
	planText := FormatPlanText(plan)
	for _, want := range []string{"root: /work", "backup run: run-1", "operation: stage", "candidates: 1", "warnings: 1", "  - ready AGENTS.md", "    Missing from workspace. (code: restore_target_file_missing)"} {
		if !strings.Contains(planText, want) {
			t.Errorf("FormatPlanText() missing %q in %q", want, planText)
		}
	}

	reportText := FormatReportText(&Report{
		Root: "/work", Applied: true, Plan: plan, Warnings: []string{"restarted"},
		Resolutions: []Resolution{{Path: "AGENTS.md", Status: StatusReady, Restored: true, Backup: "/backup", Recovery: "/recovery", Messages: []string{"verified"}}},
	})
	for _, want := range []string{"applied: true", "resolutions: 1", "  - ready AGENTS.md restored=true", "    overwrite backup: /backup", "    retained recovery: /recovery", "    verified"} {
		if !strings.Contains(reportText, want) {
			t.Errorf("FormatReportText() missing %q in %q", want, reportText)
		}
	}
}

func TestFormatRunsTextRendersRunSummary(t *testing.T) {
	got := FormatRunsText([]backup.Run{{RunID: "run-1", Operation: "stage", Entries: make([]backup.Entry, 2)}})
	for _, want := range []string{"command: threadpoint restore list", "backup runs: 1", "  - run-1 stage entries=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatRunsText() missing %q in %q", want, got)
		}
	}
}

func TestFormatReportTextShowsBackupAndRetainedRecovery(t *testing.T) {
	text := FormatReportText(&Report{
		Root:    "/project",
		Applied: true,
		Resolutions: []Resolution{{
			Path:     "AGENTS.md",
			Status:   StatusReady,
			Restored: true,
			Backup:   "/backups/AGENTS.md",
			Recovery: "/project/.threadpoint-restore-recovery-test",
		}},
	})
	for _, want := range []string{
		"overwrite backup: /backups/AGENTS.md",
		"retained recovery: /project/.threadpoint-restore-recovery-test",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted report missing %q:\n%s", want, text)
		}
	}
}
