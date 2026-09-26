// SPDX-License-Identifier: Apache-2.0

package scan

import (
	"fmt"
	"strings"

	"github.com/threadgrid/threadpoint/knowledgebase"
)

// FormatText renders a catalog as concise human-readable text.
func FormatText(catalog *knowledgebase.Catalog) string {
	if catalog == nil {
		return "inventory: unavailable\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "inventory:\n")
	fmt.Fprintf(&out, "root: %s\n", catalog.Root)
	fmt.Fprintf(&out, "records: %d\n", len(catalog.Records))
	if len(catalog.Warnings) > 0 {
		fmt.Fprintf(&out, "warnings: %d\n", len(catalog.Warnings))
		for _, warning := range catalog.Warnings {
			fmt.Fprintf(&out, "  - %s\n", warning)
		}
	}
	if len(catalog.Records) > 0 {
		fmt.Fprintf(&out, "record summary:\n")
		for _, record := range catalog.Records {
			path := ""
			if len(record.Paths) > 0 {
				path = record.Paths[0]
			} else if len(record.Sources) > 0 {
				path = record.Sources[0].Path
			}
			if path == "" {
				fmt.Fprintf(&out, "  - %s %s\n", record.Kind, record.ID)
				continue
			}
			fmt.Fprintf(&out, "  - %s %s (%s)\n", record.Kind, record.ID, path)
		}
	}
	return out.String()
}
