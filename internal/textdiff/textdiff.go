// SPDX-License-Identifier: Apache-2.0

// Package textdiff renders positional line-oriented diffs for local review.
package textdiff

import (
	"fmt"
	"strings"
)

// Unified renders a positional diff with labeled headers and standard line
// prefixes. It returns an empty string for equal inputs and does not compute a
// minimal-edit (LCS) diff.
func Unified(name, oldLabel, newLabel, oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s %s\n", name, oldLabel)
	fmt.Fprintf(&b, "+++ %s %s\n", name, newLabel)
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	n := len(oldLines)
	if len(newLines) > n {
		n = len(newLines)
	}
	for i := 0; i < n; i++ {
		oldOK := i < len(oldLines)
		newOK := i < len(newLines)
		var oldLine, newLine string
		if oldOK {
			oldLine = oldLines[i]
		}
		if newOK {
			newLine = newLines[i]
		}
		if oldOK && newOK && oldLine == newLine {
			fmt.Fprintf(&b, " %s\n", oldLine)
			continue
		}
		if oldOK {
			fmt.Fprintf(&b, "-%s\n", oldLine)
		}
		if newOK {
			fmt.Fprintf(&b, "+%s\n", newLine)
		}
	}
	return b.String()
}

func splitLines(text string) []string {
	text = strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
