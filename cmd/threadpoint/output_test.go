// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestExampleFixturePathsDocumentedAndPresent(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	docPath := filepath.Join(repoRoot, "docs", "threadpoint-examples.md")
	body, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}

	matches := regexp.MustCompile("`(docs/example-fixtures/[^`]+)`").FindAllStringSubmatch(string(body), -1)
	seen := map[string]bool{}
	for _, match := range matches {
		rel := strings.TrimSuffix(match[1], "/")
		seen[rel] = true
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("documented fixture path %s is not present: %v", rel, err)
		}
	}

	for _, rel := range []string{
		"docs/example-fixtures/bare-init",
		"docs/example-fixtures/v0-baseline",
	} {
		if !seen[rel] {
			t.Fatalf("threadpoint-examples.md does not document fixture path %s", rel)
		}
	}
}
