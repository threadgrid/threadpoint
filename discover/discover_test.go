// SPDX-License-Identifier: Apache-2.0

package discover

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/threadgrid/threadpoint/provider"
)

func TestDiscoverSkipsConfiguredNestedDirectory(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, ".claude", "commands", "generated", "ignored.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), Options{Root: project, Providers: []provider.ID{provider.Claude}, SkipDirNames: []string{"generated"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range report.Artifacts {
		if artifact.Path == ".claude/commands/generated/ignored.md" {
			t.Fatalf("configured directory was discovered: %#v", artifact)
		}
	}
}

func TestDiscoverDoesNotTraverseDeniedDirectory(t *testing.T) {
	project := t.TempDir()
	child := filepath.Join(project, "sessions", "entry.md")
	if err := os.MkdirAll(filepath.Dir(child), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, []byte("not imported"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), Options{
		Root: project,
		Registry: provider.NewRegistry([]provider.Definition{{
			ID: "fixture",
			ProjectArtifacts: []provider.ArtifactSpec{{
				Path: "sessions", Kind: "knowledge", ReadMode: string(ReadImport), Recurse: true,
			}},
		}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Artifacts) != 1 {
		t.Fatalf("artifacts = %#v, want only denied directory", report.Artifacts)
	}
	artifact := report.Artifacts[0]
	if !artifact.Directory || artifact.ReadMode != ReadDenied || artifact.Code != "discovery_runtime_path" || artifact.SHA256 != "" {
		t.Fatalf("denied directory = %#v", artifact)
	}
}

func TestDiscoverRejectsInvalidRegistryReadModes(t *testing.T) {
	for _, mode := range []string{"", "review-later"} {
		t.Run(mode, func(t *testing.T) {
			_, err := Run(context.Background(), Options{
				Root: t.TempDir(),
				Registry: provider.NewRegistry([]provider.Definition{{
					ID: "fixture",
					ProjectArtifacts: []provider.ArtifactSpec{{
						Path: "memory.md", Kind: "knowledge", ReadMode: mode,
					}},
				}}),
			})
			if err == nil {
				t.Fatalf("Run accepted read mode %q", mode)
			}
		})
	}
}

func TestArtifactJSONAlwaysIncludesReadMode(t *testing.T) {
	body, err := json.Marshal(Artifact{})
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if got, found := value["readMode"]; !found || string(got) != `""` {
		t.Fatalf("readMode JSON field = %s, found = %v", got, found)
	}
}
