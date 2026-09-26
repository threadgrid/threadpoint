// SPDX-License-Identifier: Apache-2.0

package discover

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/provider"
)

func TestScanCandidateRejectsEscapesAndSymlinkAncestors(t *testing.T) {
	project := t.TempDir()
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	_, warnings := scanCandidate(context.Background(), project, root, candidate{
		provider: "fixture", rel: "../outside.md", mode: ReadImport,
	})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "failed resolving") {
		t.Fatalf("escape warnings = %#v", warnings)
	}

	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "outside.md"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(project, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	artifacts, warnings := scanCandidate(context.Background(), project, root, candidate{
		provider: "fixture", rel: "linked/outside.md", mode: ReadImport,
	})
	if len(artifacts) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "failed reading") {
		t.Fatalf("symlink-ancestor result: artifacts=%#v warnings=%#v", artifacts, warnings)
	}
}

func TestArtifactReadFailsClosedWhenFileIdentityChanges(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, "input.md")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	artifact := artifactFromInfo(root, project, candidate{
		provider: "fixture", rel: "input.md", kind: KindKnowledge, mode: ReadImport,
	}, path, info, artifactOutcome{})
	if artifact.ReadMode != ReadDenied || artifact.Code != "discovery_file_changed" || artifact.SHA256 != "" {
		t.Fatalf("changed artifact = %#v", artifact)
	}

	if _, err := readArtifactRegularFileInRoot(nil, "input.md"); err == nil {
		t.Fatal("readArtifactRegularFileInRoot accepted a nil root")
	}
	if _, err := readArtifactRegularFileInRoot(root, "../outside.md"); err == nil {
		t.Fatal("readArtifactRegularFileInRoot accepted an escaping path")
	}
}

func TestUnsafeArtifactOutcomesCoverAuthenticationStateAndSpecialFiles(t *testing.T) {
	regular := boundaryFileInfo{mode: 0o600}
	for _, test := range []struct {
		path string
		info fs.FileInfo
		code string
	}{
		{path: "auth.json", info: regular, code: "discovery_authentication_file"},
		{path: "history.jsonl", info: regular, code: "discovery_runtime_state_file"},
		{path: "socket", info: boundaryFileInfo{mode: os.ModeSocket}, code: "discovery_special_file"},
	} {
		outcome := unsafeArtifactOutcome(test.path, test.info)
		if outcome.Code != test.code || outcome.Message == "" {
			t.Fatalf("unsafeArtifactOutcome(%q) = %#v", test.path, outcome)
		}
	}
}

func TestPinnedRootValidationRejectsAliasAndIdentityReplacement(t *testing.T) {
	t.Run("missing-capability", func(t *testing.T) {
		if err := validatePinnedRoot(rootSnapshot{}, nil); err == nil {
			t.Fatal("validatePinnedRoot accepted a missing capability")
		}
	})

	t.Run("alias-retargeted", func(t *testing.T) {
		base := t.TempDir()
		first := filepath.Join(base, "first")
		second := filepath.Join(base, "second")
		if err := os.Mkdir(first, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(second, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "selected")
		if err := os.Symlink(first, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		snapshot, root, err := captureAndOpenRoot(link)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(second, link); err != nil {
			t.Fatal(err)
		}
		if err := validatePinnedRoot(snapshot, root); err == nil {
			t.Fatal("validatePinnedRoot accepted a retargeted logical root")
		}
	})

	t.Run("path-identity-replaced", func(t *testing.T) {
		base := t.TempDir()
		selected := filepath.Join(base, "selected")
		moved := filepath.Join(base, "moved")
		if err := os.Mkdir(selected, 0o700); err != nil {
			t.Fatal(err)
		}
		snapshot, root, err := captureAndOpenRoot(selected)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := os.Rename(selected, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(selected, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := validatePinnedRoot(snapshot, root); err == nil {
			t.Fatal("validatePinnedRoot accepted a replacement directory")
		}
	})

	t.Run("closed-capability", func(t *testing.T) {
		selected := t.TempDir()
		snapshot, root, err := captureAndOpenRoot(selected)
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
		if err := validatePinnedRoot(snapshot, root); err == nil {
			t.Fatal("validatePinnedRoot accepted a closed capability")
		}
	})

	t.Run("unresolvable-logical-root", func(t *testing.T) {
		selected := t.TempDir()
		snapshot, root, err := captureAndOpenRoot(selected)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		snapshot.logical = selected + "\x00invalid"
		if err := validatePinnedRoot(snapshot, root); err == nil {
			t.Fatal("validatePinnedRoot accepted an unresolvable logical root")
		}
	})
}

func TestDiscoverRejectsNonDirectoryProjectRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project.txt")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Root: path}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Run non-directory error = %v", err)
	}
}

func TestDiscoverFromRootUsesRetainedGenerationAndRejectsInvalidCapabilities(t *testing.T) {
	project := t.TempDir()
	artifactPath := filepath.Join(project, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("retained project"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	report, err := RunFromRoot(context.Background(), Options{Root: project, Providers: []provider.ID{provider.Claude}}, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Artifacts) != 1 || report.Artifacts[0].Path != ".claude/CLAUDE.md" || report.Root != project {
		t.Fatalf("retained-root discovery report = %#v", report)
	}
	if _, err := RunFromRoot(context.Background(), Options{Root: project}, nil); err == nil {
		t.Fatal("nil retained root should fail")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunFromRoot(canceled, Options{Root: project}, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retained-root discovery error = %v", err)
	}
	if _, err := RunFromRoot(context.Background(), Options{Root: project, Providers: []provider.ID{"unknown"}}, root); err == nil {
		t.Fatal("invalid retained-root discovery options should fail")
	}

	closed, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFromRoot(context.Background(), Options{Root: project}, closed); err == nil {
		t.Fatal("closed retained root should fail")
	}
}

func TestDiscoverCatalogsUnsafeAndSymlinkArtifactsWithoutReadingThem(t *testing.T) {
	project := t.TempDir()
	for path, body := range map[string]string{
		".github/instructions/rule.md":         "safe rule",
		".github/instructions/auth-token.md":   "not imported",
		".github/instructions/.env.local":      "not imported",
		".github/instructions/sessions/log.md": "not imported",
	} {
		absolute := filepath.Join(project, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("rule.md", filepath.Join(project, ".github", "instructions", "linked.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("rule.md", filepath.Join(project, ".github", "instructions", ".env.link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	report, err := Run(context.Background(), Options{Root: project, Providers: []provider.ID{provider.Copilot}})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]Artifact{}
	for _, artifact := range report.Artifacts {
		found[artifact.Path] = artifact
	}
	if artifact := found[".github/instructions/rule.md"]; artifact.ReadMode != ReadImport || artifact.SHA256 == "" {
		t.Fatalf("safe artifact = %#v", artifact)
	}
	for _, path := range []string{
		".github/instructions/auth-token.md",
		".github/instructions/.env.local",
	} {
		artifact := found[path]
		if artifact.ReadMode != ReadDenied || artifact.Code == "" || artifact.Message == "" || artifact.SHA256 != "" {
			t.Fatalf("unsafe artifact %s = %#v", path, artifact)
		}
	}
	if artifact := found[".github/instructions/sessions/log.md"]; artifact.ReadMode != ReadDenied || artifact.Code != "discovery_runtime_path" || artifact.Message == "" {
		t.Fatalf("runtime nested artifact = %#v", artifact)
	}
	if artifact := found[".github/instructions/linked.md"]; !artifact.Symlink || artifact.ReadMode != ReadCatalog {
		t.Fatalf("symlink artifact = %#v", artifact)
	}
	if artifact := found[".github/instructions/.env.link"]; !artifact.Symlink || artifact.ReadMode != ReadDenied || artifact.Code != "discovery_environment_file" || artifact.Message == "" || artifact.SHA256 != "" {
		t.Fatalf("unsafe symlink artifact = %#v", artifact)
	}

	if _, err := Run(context.Background(), Options{Root: project, Providers: []provider.ID{"unknown"}}); err == nil || !errors.Is(err, err) {
		t.Fatal("unknown provider should fail discovery")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(canceled, Options{Root: project}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery error = %v", err)
	}
	missing := filepath.Join(project, "missing")
	missingReport, err := Run(context.Background(), Options{Root: missing})
	if err != nil || missingReport.Root != missing || len(missingReport.Artifacts) != 0 {
		t.Fatalf("missing root report = %#v, err=%v", missingReport, err)
	}
}
