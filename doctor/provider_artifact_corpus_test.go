// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderArtifactCorpusDoctorDiagnostics(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project-local-artifacts")
	home := filepath.Join(base, "external-home")
	copyDoctorCorpusScenario(t, "project-local-artifacts", project)
	copyDoctorCorpusScenario(t, "external-home", home)

	report, err := Run(context.Background(), Options{
		Root:    project,
		HomeDir: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.DiscoveredArtifacts == 0 {
		t.Fatalf("expected project-local provider artifacts, got %#v", report.Summary)
	}
	for _, finding := range report.Findings {
		if finding.Code == "out_of_root_artifacts_detected" {
			t.Fatalf("out-of-root finding should not appear in project-scoped diagnostics: %#v", report.Findings)
		}
		if finding.Root == home ||
			finding.Path == ".codex/projects/project-local-artifacts/notes.md" ||
			finding.Path == ".claude/projects/project-local-artifacts/CLAUDE.md" {
			t.Fatalf("out-of-root fixture should not appear in project-scoped diagnostics: %#v", finding)
		}
	}
	if !hasSuggestedCommand(report, "threadpoint stage --root") {
		t.Fatalf("expected stage suggestion, got %#v", report.SuggestedCommands)
	}
}

func copyDoctorCorpusScenario(t *testing.T, name string, target string) {
	t.Helper()
	source := filepath.Join("..", "testdata", "provider-artifact-corpus", name)
	if err := copyDoctorCorpusTree(source, target); err != nil {
		t.Fatalf("copy provider corpus scenario %s: %v", name, err)
	}
}

func copyDoctorCorpusTree(source string, target string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(dst, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		// #nosec G122 -- tests copy a checked-in provider corpus tree and skip non-regular files.
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}
