// SPDX-License-Identifier: Apache-2.0

package discover

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/provider"
)

func TestNormalizeOptionsRejectsUnsafeTraversalAndExcludedProvider(t *testing.T) {
	registry := provider.NewRegistry([]provider.Definition{
		{ID: "alpha"},
		{ID: "beta"},
	})

	if _, err := normalizeOptions(Options{
		Root:         t.TempDir(),
		SkipDirNames: []string{"../outside"},
		Registry:     registry,
	}); err == nil {
		t.Fatal("normalizeOptions accepted a skip-directory path")
	}
	if _, err := normalizeOptions(Options{
		Root:             t.TempDir(),
		ExcludeProviders: []provider.ID{"unknown"},
		Registry:         registry,
	}); err == nil {
		t.Fatal("normalizeOptions accepted an unknown excluded provider")
	}

	normalized, err := normalizeOptions(Options{
		Providers:        []provider.ID{"beta", "alpha"},
		ExcludeProviders: []provider.ID{"beta", "alpha"},
		Registry:         registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Root == "" || normalized.Providers[0] != "alpha" || normalized.ExcludeProviders[0] != "alpha" {
		t.Fatalf("normalized options = %#v", normalized)
	}
}

func TestDiscoveryBudgetRejectsMissingExhaustedAndOverLimitUse(t *testing.T) {
	var missing *discoveryBudget
	if err := missing.charge("entry"); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("nil budget error = %v", err)
	}

	exhausted := &discoveryBudget{exhausted: true}
	if err := exhausted.charge("entry"); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("exhausted budget error = %v", err)
	}

	overLimit := &discoveryBudget{entries: maxWalkEntries}
	if err := overLimit.charge("entry"); err == nil || !overLimit.exhausted {
		t.Fatalf("over-limit budget = %#v, error = %v", overLimit, err)
	}
}

func TestScanCandidateStopsOnCancellationAndFileLimit(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		project := t.TempDir()
		if err := os.Mkdir(filepath.Join(project, "inputs"), 0o700); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(project)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		artifacts, warnings := scanCandidate(ctx, project, root, candidate{
			provider: "fixture", rel: "inputs", mode: ReadImport, recurse: true,
		})
		if len(artifacts) != 1 || len(warnings) != 1 || !strings.Contains(warnings[0], context.Canceled.Error()) {
			t.Fatalf("canceled scan: artifacts=%#v warnings=%#v", artifacts, warnings)
		}
	})

	t.Run("file-limit", func(t *testing.T) {
		project := t.TempDir()
		inputs := filepath.Join(project, "inputs")
		if err := os.Mkdir(inputs, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= maxWalkFiles; i++ {
			// Use a stable, unique decimal suffix while keeping directory order deterministic.
			name := filepath.Join(inputs, formatBoundaryIndex(i))
			if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		root, err := os.OpenRoot(project)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		artifacts, warnings := scanCandidate(context.Background(), project, root, candidate{
			provider: "fixture", rel: "inputs", mode: ReadCatalog, recurse: true,
		})
		if len(artifacts) != 1 {
			t.Fatalf("artifact count = %d, want only the candidate directory when its complete listing exceeds the cap", len(artifacts))
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "stopped scanning") {
			t.Fatalf("file-limit warnings = %#v", warnings)
		}
	})

	t.Run("empty-directories-consume-entry-budget", func(t *testing.T) {
		project := t.TempDir()
		inputs := filepath.Join(project, "inputs")
		if err := os.Mkdir(inputs, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= maxWalkEntries; i++ {
			if err := os.Mkdir(filepath.Join(inputs, "dir-"+formatBoundaryIndex(i)), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		root, err := os.OpenRoot(project)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		artifacts, warnings := scanCandidate(context.Background(), project, root, candidate{
			provider: "fixture", rel: "inputs", mode: ReadCatalog, recurse: true,
		})
		if len(artifacts) != 1 {
			t.Fatalf("empty-directory artifacts = %d, want only the candidate directory", len(artifacts))
		}
		if !strings.Contains(strings.Join(warnings, "\n"), "after 500 entries") {
			t.Fatalf("empty-directory budget warnings = %#v", warnings)
		}
	})
}

func TestProviderExclusionAndArtifactSortBoundaries(t *testing.T) {
	if providerAllowed(Options{ExcludeProviders: []provider.ID{"fixture"}}, "fixture") {
		t.Fatal("excluded provider was allowed")
	}
	artifacts := []Artifact{
		{Root: "z", Provider: "a", Path: "a"},
		{Root: "a", Provider: "z", Path: "a"},
		{Root: "a", Provider: "a", Path: "z"},
	}
	sortArtifacts(artifacts)
	if artifacts[0].Root != "a" || artifacts[0].Provider != "a" || artifacts[1].Provider != "z" || artifacts[2].Root != "z" {
		t.Fatalf("sorted artifacts = %#v", artifacts)
	}
}

func TestDiscoverLimitsResultsToTheSelectedProject(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(filepath.Join(project, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sibling, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".claude", "CLAUDE.md"), []byte("project"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, ".claude", "CLAUDE.md"), []byte("sibling"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), Options{Root: project, Providers: []provider.ID{provider.Claude}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, artifact := range report.Artifacts {
		if artifact.Root != project || artifact.Root == sibling {
			t.Fatalf("discovery escaped selected project: %#v", report)
		}
		if artifact.Path == ".claude/CLAUDE.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected project artifact in %#v", report)
	}
}

func formatBoundaryIndex(index int) string {
	const digits = "0123456789"
	return string([]byte{
		digits[(index/100)%10],
		digits[(index/10)%10],
		digits[index%10],
	}) + ".md"
}
