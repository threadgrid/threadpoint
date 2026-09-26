// SPDX-License-Identifier: Apache-2.0

package scan

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanMapsSharedKnowledgeAndSkills(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "shared")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "architecture.md"), "# Architecture")
	mustWrite(t, filepath.Join(root, ".agents", "skills", "example", "SKILL.md"), "---\nname: example\ndescription: Example skill\n---\n# Example")

	kb, err := Run(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	var foundSkill bool
	for _, record := range kb.Records {
		if record.Kind == "skill" && record.Title == "example" {
			if got, want := record.Sources[0].Provider, "shared"; got != want {
				t.Fatalf("skill source provider = %q, want %q", got, want)
			}
			foundSkill = true
		}
	}
	if !foundSkill {
		t.Fatalf("expected mapped example skill, got %#v", kb.Records)
	}
}

func TestScanIgnoresRecoveryAndProbeEntries(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "shared")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "architecture.md"), "# Architecture")
	for i := 0; i < 501; i++ {
		mustWrite(t, filepath.Join(root, ".agents", "rules", ".threadpoint-prune-recovery-test", fmt.Sprintf("recovery-%03d.md", i)), "# Recovery")
		mustWrite(t, filepath.Join(root, ".agents", "rules", ".threadpoint-restore-recovery-test", fmt.Sprintf("recovery-%03d.md", i)), "# Restore recovery")
	}

	kb, err := Run(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	for _, warning := range kb.Warnings {
		if strings.Contains(warning, "stopped scanning") {
			t.Fatalf("recovery tree exhausted scan budget: %#v", kb.Warnings)
		}
	}
}

func TestScanUsesCurrentDirectoryAndHonorsCancellation(t *testing.T) {
	t.Chdir(t.TempDir())
	mustWrite(t, filepath.Join(".agents", "knowledge", "note.md"), "# Note")
	catalog, err := Run(context.Background(), Options{})
	if err != nil || len(catalog.Records) != 2 || catalog.Records[1].Title != "note" {
		t.Fatalf("current directory scan = %+v, %v", catalog, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan = %v", err)
	}
}

func TestScanCatalogsLocalGuideAndStructuredCanonicalRecords(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"AGENTS.local.md", ".agents/knowledge/catalog.json", ".agents.local/rules/security.md",
		".agents.local/adapters/README.md", ".agents.local/plugins/example/plugin.json",
	} {
		mustWrite(t, filepath.Join(root, rel), "metadata inventory only")
	}
	catalog, err := Run(context.Background(), Options{Root: root})
	if err != nil || len(catalog.Warnings) != 0 {
		t.Fatalf("%+v %v", catalog, err)
	}
	for _, want := range []string{"bridge:AGENTS.local.md", "knowledge:.agents/knowledge/catalog.json", "rule:.agents.local/rules/security.md", "config:.agents.local/adapters/README.md", "plugin:.agents.local/plugins/example/plugin.json"} {
		found := false
		for _, record := range catalog.Records {
			if record.ID == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s: %+v", want, catalog.Records)
		}
	}
}

func TestScanWarnsForInvalidCanonicalPaths(t *testing.T) {
	for _, scenario := range []string{"local-guide-directory", "root-is-file"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			if scenario == "root-is-file" {
				root = filepath.Join(root, "file")
				mustWrite(t, root, "invalid root")
			} else {
				mustWrite(t, filepath.Join(root, "AGENTS.local.md", "child"), "invalid guide")
			}
			catalog, err := Run(context.Background(), Options{Root: root})
			if err != nil || len(catalog.Warnings) == 0 {
				t.Fatalf("invalid path hidden: %+v %v", catalog, err)
			}
			for _, record := range catalog.Records {
				if record.ID == "bridge:AGENTS.local.md" {
					t.Fatal("directory cataloged as a guide")
				}
			}
		})
	}
}

func TestScanPreservesSkillsNestedInsidePlugins(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".agents", "plugins", "example", "skills", "writer", "SKILL.md")
	mustWrite(t, path, `---
name: writer
description: Writes clearly
---
`)
	catalog, err := Run(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range catalog.Records {
		if record.Kind == "skill" && record.Title == "writer" && record.Summary == "Writes clearly" {
			return
		}
	}
	t.Fatalf("plugin skill lost metadata: %+v", catalog.Records)
}
