// SPDX-License-Identifier: Apache-2.0

package scan

import (
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/knowledgebase"
)

func TestFormatTextRendersAllRecordPathForms(t *testing.T) {
	if got := FormatText(nil); got != "inventory: unavailable\n" {
		t.Fatalf("FormatText(nil) = %q", got)
	}

	got := FormatText(&knowledgebase.Catalog{
		Root:     "/work",
		Warnings: []string{"incomplete source"},
		Records: []knowledgebase.Record{
			{ID: "with-path", Kind: knowledgebase.RecordKnowledge, Paths: []string{".agents/knowledge/example.md"}},
			{ID: "with-source", Kind: knowledgebase.RecordRule, Sources: []knowledgebase.Source{{Path: ".agents/rules/example.md"}}},
			{ID: "without-path", Kind: knowledgebase.RecordSkill},
		},
	})
	for _, want := range []string{
		"root: /work", "records: 3", "warnings: 1", "  - incomplete source",
		"  - knowledge with-path (.agents/knowledge/example.md)",
		"  - rule with-source (.agents/rules/example.md)",
		"  - skill without-path",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatText() missing %q in %q", want, got)
		}
	}
}
