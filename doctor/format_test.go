// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/layout"
)

func TestFormatTextIncludesKeySections(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	text := FormatText(report)
	for _, want := range []string{
		"Threadpoint status for ",
		"Errors:",
		"Warnings:",
		"Suggested next commands:",
		"State summary:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("doctor text missing %q:\n%s", want, text)
		}
	}
}

func TestDoctorTextRendersFailuresAndLatestBackup(t *testing.T) {
	formatted := FormatText(&Report{
		Root: "/work",
		OK:   false,
		Summary: Summary{
			BackupRuns:      2,
			LatestBackupRun: "backup-2",
		},
		Findings: []Finding{{Severity: SeverityError, Code: "fixture", Message: "failed"}},
	})
	for _, want := range []string{"Status: errors found", "[fixture] failed", "Suggested next commands: none", "latest backup-2"} {
		if !strings.Contains(formatted, want) {
			t.Fatalf("alternate report formatting omitted %q: %s", want, formatted)
		}
	}
}

func TestDoctorShellQuoteEscapesApostrophes(t *testing.T) {
	if got := shellQuote("it's spaced"); got != "'it'\\''s spaced'" {
		t.Fatalf("shellQuote apostrophe = %q", got)
	}
}
