// SPDX-License-Identifier: Apache-2.0

package prune

import (
	"fmt"
	"strings"
)

// FormatPlanText renders a pruning plan as concise human-readable text.
func FormatPlanText(plan *Plan) string {
	if plan == nil {
		return "command: threadpoint prune\nplan: unavailable\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "command: threadpoint prune\n")
	fmt.Fprintf(&out, "root: %s\n", plan.Root)
	fmt.Fprintf(&out, "candidates: %d\n", len(plan.Candidates))
	formatWarnings(&out, plan.Warnings)
	for _, candidate := range plan.Candidates {
		fmt.Fprintf(&out, "  - %s %s\n", candidate.Status, candidate.Path)
		if candidate.Message != "" {
			fmt.Fprintf(&out, "    %s", candidate.Message)
			if candidate.Code != "" {
				fmt.Fprintf(&out, " (code: %s)", candidate.Code)
			}
			fmt.Fprintln(&out)
		}
	}
	return out.String()
}

// FormatReportText renders a pruning apply report as concise human-readable text.
func FormatReportText(report *Report) string {
	if report == nil {
		return "command: threadpoint prune\nreport: unavailable\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "command: threadpoint prune\n")
	fmt.Fprintf(&out, "root: %s\n", report.Root)
	fmt.Fprintf(&out, "applied: %v\n", report.Applied)
	if report.Plan != nil {
		fmt.Fprintf(&out, "candidates: %d\n", len(report.Plan.Candidates))
	}
	fmt.Fprintf(&out, "resolutions: %d\n", len(report.Resolutions))
	formatWarnings(&out, report.Warnings)
	for _, resolution := range report.Resolutions {
		fmt.Fprintf(&out, "  - %s %s removed=%v\n", resolution.Status, resolution.Path, resolution.Removed)
		if resolution.Backup != "" {
			fmt.Fprintf(&out, "    backup: %s\n", resolution.Backup)
		}
		if resolution.Recovery != "" {
			fmt.Fprintf(&out, "    recovery: %s\n", resolution.Recovery)
		}
		for _, message := range resolution.Messages {
			fmt.Fprintf(&out, "    %s\n", message)
		}
	}
	return out.String()
}

func formatWarnings(out *strings.Builder, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintf(out, "warnings: %d\n", len(warnings))
	for _, warning := range warnings {
		fmt.Fprintf(out, "  - %s\n", warning)
	}
}
