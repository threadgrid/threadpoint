// SPDX-License-Identifier: Apache-2.0

package layout

import (
	"fmt"
	"strings"
)

// FormatText renders a layout validation report as concise text.
func FormatText(report Report) string {
	var out strings.Builder
	fmt.Fprintf(&out, "layout validation:\n")
	fmt.Fprintf(&out, "root: %s\n", report.Root)
	fmt.Fprintf(&out, "ok: %v\n", report.OK)
	fmt.Fprintf(&out, "issues: %d\n", len(report.Issues))
	for _, issue := range report.Issues {
		fmt.Fprintf(&out, "  - %s: %s\n", issue.Path, issue.Message)
	}
	if len(report.Warnings) > 0 {
		fmt.Fprintf(&out, "warnings: %d\n", len(report.Warnings))
		for _, warning := range report.Warnings {
			fmt.Fprintf(&out, "  - %s: %s\n", warning.Path, warning.Message)
		}
	}
	return out.String()
}
