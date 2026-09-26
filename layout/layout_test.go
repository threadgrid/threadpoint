// SPDX-License-Identifier: Apache-2.0

package layout

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureProjectSkillStateLayoutCreatesIgnoredRuntimeState(t *testing.T) {
	root := t.TempDir()
	gitignore := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("/existing/\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureProjectSkillState(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "state", "skills")
	if info, err := os.Stat(stateDir); err != nil || !info.IsDir() {
		t.Fatalf("expected skill state directory: info=%v err=%v", info, err)
	}
	body, err := os.ReadFile(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "/existing/\n/state/\n" {
		t.Fatalf(".gitignore = %q", body)
	}
	beforeInfo, err := os.Lstat(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if beforeInfo.Mode().Perm() != 0o600 {
		t.Fatalf(".gitignore mode = %o, want 0600", beforeInfo.Mode().Perm())
	}
	if err := EnsureProjectSkillState(root); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "/state/") != 1 {
		t.Fatalf(".gitignore has duplicate state entry: %s", body)
	}
	afterInfo, err := os.Lstat(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeInfo, afterInfo) || afterInfo.Mode().Perm() != 0o600 {
		t.Fatalf("idempotent init changed .gitignore identity or mode: before=%v after=%v", beforeInfo.Mode(), afterInfo.Mode())
	}
}

func TestEnsureGitignoreEntriesRetriesAndMergesConcurrentEdit(t *testing.T) {
	root := t.TempDir()
	gitignore := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("/existing/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalHook := beforeGitignoreCompare
	t.Cleanup(func() { beforeGitignoreCompare = originalHook })
	beforeGitignoreCompare = func() {
		beforeGitignoreCompare = nil
		if err := os.WriteFile(gitignore, []byte("/existing/\n/concurrent/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureGitignoreEntries(gitignore, []string{"/state/"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/existing/", "/concurrent/", "/state/"} {
		if !strings.Contains(string(body), want+"\n") {
			t.Fatalf("concurrent merge lost %q: %s", want, body)
		}
	}
}

func TestSharedSeedIsPortableAndAvoidsNativeLocations(t *testing.T) {
	for name, body := range map[string]string{
		"shared guide":     projectGuideSeed,
		"adapter guidance": projectAdaptersSeed,
	} {
		lower := strings.ToLower(body)
		for _, forbidden := range []string{"thread" + "grid", "threadpoint", "codex", "claude"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s seed mentions %q: %s", name, forbidden, body)
			}
		}
		if !strings.Contains(lower, "native") || !strings.Contains(body, ".agents/") {
			t.Fatalf("%s seed is missing portable-layout instructions: %s", name, body)
		}
	}
}

func TestEnsureSharedDirectoriesDoesNotCreateCanonicalFiles(t *testing.T) {
	root := t.TempDir()
	if err := EnsureSharedDirectories(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(root, ".agents", "adapters", "README.md"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("directory-only layout should not create %s: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(root, ".agents", "knowledge"),
	} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("expected shared directory %s: info=%v err=%v", path, info, err)
		}
	}
}

func TestEnsureSharedLayoutDoesNotCreateFeatureManifests(t *testing.T) {
	root := t.TempDir()

	if err := EnsureShared(root); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{
		filepath.Join(".agents", "knowledge", "feature-catalog.json"),
		filepath.Join(".agents", "knowledge", "requirements-manifest.json"),
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("expected %s not to be created, stat err: %v", rel, err)
		}
	}
}

func TestEnsureSharedLayoutPreservesExistingFeatureManifests(t *testing.T) {
	root := t.TempDir()
	featureCatalog := filepath.Join(root, ".agents", "knowledge", "feature-catalog.json")
	requirementsManifest := filepath.Join(root, ".agents", "knowledge", "requirements-manifest.json")
	if err := os.MkdirAll(filepath.Dir(featureCatalog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(featureCatalog, []byte("existing features"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requirementsManifest, []byte("existing requirements"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureShared(root); err != nil {
		t.Fatal(err)
	}

	featureBody, err := os.ReadFile(featureCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if string(featureBody) != "existing features" {
		t.Fatalf("feature catalog was changed: %q", featureBody)
	}
	requirementsBody, err := os.ReadFile(requirementsManifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(requirementsBody) != "existing requirements" {
		t.Fatalf("requirements manifest was changed: %q", requirementsBody)
	}
}

func TestEnsureProjectLocalLayoutCreatesIgnoredOverlay(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/.claude/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProjectLocal(root); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"AGENTS.local.md",
		filepath.Join(".agents.local", "knowledge"),
		filepath.Join(".agents.local", "rules"),
		filepath.Join(".agents.local", "skills"),
		filepath.Join(".agents.local", "plugins"),
		filepath.Join(".agents.local", "adapters"),
	} {
		if info, err := os.Stat(filepath.Join(root, rel)); err != nil || (strings.HasPrefix(rel, ".agents.local") && !info.IsDir()) {
			t.Fatalf("expected local overlay %s: info=%v err=%v", rel, info, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/.claude/", "/AGENTS.local.md", "/.agents.local/"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf(".gitignore missing %q: %s", want, body)
		}
	}
	for _, unwanted := range []string{"/CLAUDE.local.md"} {
		if strings.Contains(string(body), unwanted) {
			t.Fatalf(".gitignore must not add native artifact ignore %q: %s", unwanted, body)
		}
	}
	if err := EnsureProjectLocal(root); err != nil {
		t.Fatal(err)
	}
	gitignore := filepath.Join(root, ".gitignore")
	if err := os.Chmod(gitignore, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeBody, err := os.ReadFile(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Lstat(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureProjectLocal(root); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/AGENTS.local.md", "/.agents.local/"} {
		if count := strings.Count(string(body), want); count != 1 {
			t.Fatalf(".gitignore has %d copies of %q: %s", count, want, body)
		}
	}
	afterInfo, err := os.Lstat(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("reinitialization replaced an unchanged .gitignore")
	}
	if afterInfo.Mode().Perm() != 0o600 {
		t.Fatalf(".gitignore mode = %o, want 0600", afterInfo.Mode().Perm())
	}
	if string(body) != string(beforeBody) {
		t.Fatalf(".gitignore changed on reinitialization: %q != %q", body, beforeBody)
	}
}

func TestLayoutBoundaryHelpersRejectInvalidCapabilitiesAndPaths(t *testing.T) {
	for name, action := range map[string]func() error{
		"nil bootstrap root":        func() error { return EnsureProjectBootstrapInRoot(nil) },
		"nil shared-directory root": func() error { return EnsureSharedDirectoriesInRoot(nil) },
		"empty shared root":         func() error { return EnsureSharedDirectories("") },
		"empty skill-state root":    func() error { return EnsureProjectSkillState("") },
		"empty project-local root":  func() error { return EnsureProjectLocal("") },
		"nil gitignore root":        func() error { return ensureGitignoreEntriesInRoot(nil, ".gitignore", []string{"/state/"}) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := action(); err == nil {
				t.Fatal("invalid layout boundary was accepted")
			}
		})
	}

	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensureGitignoreEntriesInRoot(root, filepath.Join("nested", ".gitignore"), []string{"/state/"}); err == nil {
		t.Fatal("nested .gitignore path was accepted")
	}
	if err := writeRootIfMissingValidated(root, "../outside", "fixture", nil); err == nil {
		t.Fatal("escaping bootstrap path was accepted")
	}
	if err := root.Mkdir("directory", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readGitignoreGeneration(root, "directory"); err == nil {
		t.Fatal("directory was accepted as a .gitignore generation")
	}
	want := errors.New("generation changed")
	if err := validateRootMutation(func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("validator error = %v, want %v", err, want)
	}
}
