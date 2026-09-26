// SPDX-License-Identifier: Apache-2.0

package skill

import (
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	meta, err := ParseFrontmatter([]byte("---\nname: memory-maintenance\ndescription: Maintain memory\n---\n# Body\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "memory-maintenance" {
		t.Fatalf("unexpected name %q", meta.Name)
	}
	if meta.Description != "Maintain memory" {
		t.Fatalf("unexpected description %q", meta.Description)
	}
}

func TestParseFrontmatterRejectsMissingDescription(t *testing.T) {
	_, err := ParseFrontmatter([]byte("---\nname: broken\n---\n"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseFrontmatterParsesMultilineDescription(t *testing.T) {
	meta, err := ParseFrontmatter([]byte("---\nname: review\ndescription: >-\n  Review changes for correctness\n  and explain concrete risk.\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "Review changes for correctness and explain concrete risk." {
		t.Fatalf("description = %q", meta.Description)
	}
}

func TestParseFrontmatterParsesBlockScalarHeaderVariants(t *testing.T) {
	for _, header := range []string{"|+", "|2", "| # literal comment", ">-", ">2- # folded comment"} {
		t.Run(header, func(t *testing.T) {
			meta, err := ParseFrontmatter([]byte("---\nname: review\ndescription: " + header + "\n  Review changes\n  and explain risk.\n---\n"))
			if err != nil {
				t.Fatal(err)
			}
			if meta.Description == header || meta.Description == "" {
				t.Fatalf("description for %q = %q, want parsed block content", header, meta.Description)
			}
		})
	}
}

func TestParseFrontmatterKeepsIndentedClosingMarkerInLiteralDescription(t *testing.T) {
	meta, err := ParseFrontmatter([]byte("---\nname: review\ndescription: |\n  Review the release\n  ---\n  before deployment.\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "Review the release\n---\nbefore deployment." {
		t.Fatalf("description = %q", meta.Description)
	}
}

func TestParseFrontmatterPreservesFoldedDescriptionParagraphs(t *testing.T) {
	meta, err := ParseFrontmatter([]byte("---\nname: review\ndescription: >\n  Review the release carefully.\n\n  Escalate deployment risks.\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "Review the release carefully.\nEscalate deployment risks." {
		t.Fatalf("description = %q", meta.Description)
	}
}

func TestParseFrontmatterPreservesExplicitIndentationAndFoldedDetailLines(t *testing.T) {
	meta, err := ParseFrontmatter([]byte("---\nname: review\ndescription: >2\n  First\n    detail\n  Last\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "First\n  detail\nLast" {
		t.Fatalf("description = %q", meta.Description)
	}
}

func TestParseFrontmatterPreservesRepeatedFoldedBlankLines(t *testing.T) {
	meta, err := ParseFrontmatter([]byte("---\nname: review\ndescription: >\n  First\n\n\n  Last\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "First\n\nLast" {
		t.Fatalf("description = %q", meta.Description)
	}
}

func TestParseFrontmatterPreservesFoldedBlankLinesBesideIndentedContent(t *testing.T) {
	meta, err := ParseFrontmatter([]byte("---\nname: review\ndescription: >\n  First\n\n    detail\n\n  Last\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "First\n\n  detail\n\nLast" {
		t.Fatalf("description = %q", meta.Description)
	}
}

func TestParseFrontmatterPreservesLiteralWhitespaceOnlyLines(t *testing.T) {
	meta, err := ParseFrontmatter([]byte("---\nname: review\ndescription: |\n  First\n    \n  Last\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "First\n  \nLast" {
		t.Fatalf("description = %q", meta.Description)
	}
}

func TestParseFrontmatterPreservesFoldedWhitespaceOnlyIndentedLines(t *testing.T) {
	meta, err := ParseFrontmatter([]byte("---\nname: review\ndescription: >2-\n  First\n    \n  Last\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "First\n  \nLast" {
		t.Fatalf("description = %q", meta.Description)
	}
}

func FuzzParseFrontmatter(f *testing.F) {
	for _, seed := range []string{
		"---\nname: x\ndescription: y\n---\n",
		"---\n---\n",
		"no frontmatter here",
		"---\nname: only\n",
		"---\nname: a\ndescription: b: c\n---\n",
		"",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		// Must never panic; a successful parse must carry both required fields.
		meta, err := ParseFrontmatter(body)
		if err == nil && (meta.Name == "" || meta.Description == "") {
			t.Fatalf("ParseFrontmatter accepted incomplete metadata: %#v", meta)
		}
	})
}
