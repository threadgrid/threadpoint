// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinRegistryExposesCurrentProviders(t *testing.T) {
	registry := BuiltinRegistry()
	if got, want := registry.ListString(), "shared,codex,claude,copilot,antigravity,kiro,opencode,cursor,openclaw"; got != want {
		t.Fatalf("provider list = %q, want %q", got, want)
	}
	for _, provider := range []ID{Shared, Codex, Claude, Copilot, Antigravity, Kiro, OpenCode, Cursor, OpenClaw} {
		if _, ok := registry.Lookup(provider); !ok {
			t.Fatalf("missing provider %q", provider)
		}
	}
	if path, ok := registry.BridgePath(Codex); !ok || path != filepath.ToSlash(filepath.Join(".codex", "AGENTS.md")) {
		t.Fatalf("unexpected codex bridge: %q %v", path, ok)
	}
	if body, ok := registry.BridgeBody(Claude); !ok || !strings.Contains(body, "Claude Bridge") {
		t.Fatalf("unexpected claude bridge body: %q %v", body, ok)
	}
	if body, ok := registry.BridgeBody(Antigravity); !ok || !strings.Contains(body, "Antigravity Bridge") {
		t.Fatalf("unexpected antigravity bridge body: %q %v", body, ok)
	}
	if body, ok := registry.BridgeBody(Kiro); !ok || !strings.Contains(body, "Kiro Bridge") {
		t.Fatalf("unexpected kiro bridge body: %q %v", body, ok)
	}
	if _, ok := registry.BridgeBody(OpenCode); ok {
		t.Fatal("opencode should use AGENTS.md natively instead of a managed project bridge")
	}
	if body, ok := registry.BridgeBody(Cursor); !ok || !strings.Contains(body, "Cursor Bridge") {
		t.Fatalf("unexpected cursor bridge body: %q %v", body, ok)
	}
	openclaw, ok := registry.Lookup(OpenClaw)
	if !ok {
		t.Fatal("missing openclaw definition")
	}
	if openclaw.Class != WorkspaceBackedAgent {
		t.Fatalf("OpenClaw class = %q", openclaw.Class)
	}
	if _, ok := registry.BridgeBody(OpenClaw); ok {
		t.Fatal("openclaw should use AGENTS.md natively instead of a managed project bridge")
	}
}

func TestBuiltinRegistryWorkspaceMarkers(t *testing.T) {
	registry := BuiltinRegistry()
	want := map[ID][]string{
		Codex:       {".codex"},
		Claude:      {"CLAUDE.md", ".claude"},
		Copilot:     {".github/copilot-instructions.md", ".github/instructions"},
		Antigravity: {"GEMINI.md", ".agent"},
		Kiro:        {".kiro"},
		OpenCode:    {"opencode.json", "opencode.jsonc", ".opencode"},
		Cursor:      {".cursor", ".cursorrules"},
		OpenClaw:    {"SOUL.md", "MEMORY.md", "skills"},
	}
	for provider, markers := range want {
		got := registry.WorkspaceMarkers(provider)
		if len(got) != len(markers) {
			t.Fatalf("%s markers = %v, want %v", provider, got, markers)
		}
		for i := range markers {
			if got[i] != markers[i] {
				t.Fatalf("%s markers = %v, want %v", provider, got, markers)
			}
		}
	}
	if got := registry.WorkspaceMarkers(Shared); len(got) != 0 {
		t.Fatalf("shared markers = %v, want none", got)
	}
	if got := registry.WorkspaceMarkers("nope"); got != nil {
		t.Fatalf("unknown provider markers = %v, want nil", got)
	}
	first := registry.WorkspaceMarkers(Claude)
	first[0] = "mutated"
	if again := registry.WorkspaceMarkers(Claude); again[0] != "CLAUDE.md" {
		t.Fatalf("WorkspaceMarkers leaked internal state: %v", again)
	}
}

func TestBuiltinProviderDefinitionsAreComplete(t *testing.T) {
	readModes := map[string]bool{
		"catalog-only":  true,
		"import":        true,
		"manual-review": true,
	}
	canonicalScopes := map[CanonicalScope]bool{
		CanonicalScopeAuto:         true,
		CanonicalScopeProjectLocal: true,
	}
	seen := map[ID]bool{}
	sharedGuideNative := map[ID]bool{
		OpenCode: true,
		OpenClaw: true,
	}

	for _, definition := range BuiltinDefinitions() {
		if definition.ID == "" {
			t.Fatal("provider definition missing ID")
		}
		if seen[definition.ID] {
			t.Fatalf("duplicate provider definition for %q", definition.ID)
		}
		seen[definition.ID] = true
		if strings.TrimSpace(definition.DisplayName) == "" {
			t.Fatalf("provider %q missing display name", definition.ID)
		}
		if definition.ID != Shared && definition.Class == "" {
			t.Fatalf("provider %q missing class", definition.ID)
		}
		if len(definition.Bridges) == 0 && !sharedGuideNative[definition.ID] {
			t.Fatalf("provider %q missing bridge metadata", definition.ID)
		}

		nativeBridge := false
		for _, bridge := range definition.Bridges {
			if strings.TrimSpace(bridge.Path) == "" {
				t.Fatalf("provider %q has bridge with empty path", definition.ID)
			}
			if bridge.Native {
				nativeBridge = true
				if strings.TrimSpace(bridge.Body) == "" {
					t.Fatalf("provider %q has native bridge %q without body", definition.ID, bridge.Path)
				}
			}
		}

		checkArtifacts := func(label string, artifacts []ArtifactSpec) {
			for _, artifact := range artifacts {
				if strings.TrimSpace(artifact.Path) == "" {
					t.Fatalf("provider %q %s artifact missing path", definition.ID, label)
				}
				if strings.TrimSpace(artifact.Kind) == "" {
					t.Fatalf("provider %q %s artifact %q missing kind", definition.ID, label, artifact.Path)
				}
				if !readModes[artifact.ReadMode] {
					t.Fatalf("provider %q %s artifact %q has unsupported read mode %q", definition.ID, label, artifact.Path, artifact.ReadMode)
				}
				if !canonicalScopes[artifact.CanonicalScope] {
					t.Fatalf("provider %q %s artifact %q has unsupported canonical scope %q", definition.ID, label, artifact.Path, artifact.CanonicalScope)
				}
			}
		}
		checkArtifacts("project", definition.ProjectArtifacts)

		if definition.ID == Shared {
			continue
		}
		if !nativeBridge && !sharedGuideNative[definition.ID] {
			t.Fatalf("provider %q missing native bridge", definition.ID)
		}
		if len(definition.ProjectArtifacts) == 0 {
			t.Fatalf("provider %q missing artifact specs", definition.ID)
		}
		if len(definition.NativeWorkspacePaths) == 0 {
			t.Fatalf("provider %q missing native path markers", definition.ID)
		}
	}
}

func TestClaudeLocalInstructionArtifactsAreProjectLocal(t *testing.T) {
	registry := BuiltinRegistry()
	claude, ok := registry.Lookup(Claude)
	if !ok {
		t.Fatal("missing Claude definition")
	}
	wanted := map[string]bool{
		"CLAUDE.local.md": false,
	}
	for _, artifact := range claude.ProjectArtifacts {
		if _, ok := wanted[artifact.Path]; !ok {
			continue
		}
		if artifact.Kind != "instruction" || artifact.ReadMode != "import" || artifact.CanonicalScope != CanonicalScopeProjectLocal {
			t.Fatalf("local Claude artifact %q = %#v", artifact.Path, artifact)
		}
		wanted[artifact.Path] = true
	}
	for path, found := range wanted {
		if !found {
			t.Fatalf("missing local Claude artifact %q", path)
		}
		if !registry.IsNativeWorkspacePath(path) {
			t.Fatalf("expected %q to be a native Claude workspace path", path)
		}
	}
}

func TestDetectBridgesPreservesProviderMetadata(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "shared")
	mustWrite(t, filepath.Join(root, ".github", "copilot-instructions.md"), "copilot")

	bridges := DetectBridges(root)
	if len(bridges) != 2 {
		t.Fatalf("expected 2 bridges, got %d", len(bridges))
	}
	if bridges[0].ID != Copilot || !bridges[0].Native {
		t.Fatalf("expected native copilot bridge first, got %#v", bridges[0])
	}
	if bridges[1].ID != Shared || bridges[1].Native {
		t.Fatalf("expected shared bridge second, got %#v", bridges[1])
	}
}

func TestRegistrySupportsFutureAgentDefinitions(t *testing.T) {
	registry := NewRegistry([]Definition{
		{
			ID:          Shared,
			DisplayName: "Shared",
			Bridges:     []BridgeSpec{{Path: "AGENTS.md"}},
		},
		{
			ID:          ID("future"),
			DisplayName: "Future",
			Bridges: []BridgeSpec{{
				Path:   filepath.ToSlash(filepath.Join(".future", "AGENTS.md")),
				Body:   "# Future Bridge\n",
				Native: true,
			}},
			ProjectArtifacts: []ArtifactSpec{{
				Path:     filepath.ToSlash(filepath.Join(".future", "memory.md")),
				Kind:     "instruction",
				ReadMode: "import",
			}},
			NativeWorkspacePaths: []NativePathSpec{{Path: ".future", Prefix: true}},
		},
	})

	if got, want := registry.ListString(), "shared,future"; got != want {
		t.Fatalf("provider list = %q, want %q", got, want)
	}
	if path, ok := registry.BridgePath(ID("future")); !ok || path != ".future/AGENTS.md" {
		t.Fatalf("unexpected future bridge: %q %v", path, ok)
	}
	if !registry.IsNativeWorkspacePath(".future/cache.json") {
		t.Fatalf("expected future workspace native path")
	}
}

func TestBuiltinRegistryConvenienceFunctionsExposeDefensiveCopies(t *testing.T) {
	if got, want := ListString(), BuiltinRegistry().ListString(); got != want {
		t.Fatalf("ListString() = %q, want %q", got, want)
	}
	if got, want := IDs(), BuiltinRegistry().IDs(); len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("IDs() = %#v, want %#v", got, want)
	}
	if provider, found := Parse(" CODEX "); !found || provider != Codex {
		t.Fatalf("Parse(CODEX) = %q, %v", provider, found)
	}
	if _, found := Parse("unknown"); found {
		t.Fatal("unknown provider should not resolve")
	}
	if path, found := BridgePath(Codex); !found || path != ".codex/AGENTS.md" {
		t.Fatalf("BridgePath(codex) = %q, %v", path, found)
	}
	if body, found := BridgeBody(Claude); !found || !strings.Contains(body, "Claude Bridge") {
		t.Fatalf("BridgeBody(claude) = %q, %v", body, found)
	}
	if !IsNativeWorkspacePath(".codex/config.toml") || IsNativeWorkspacePath("notes.md") {
		t.Fatal("native workspace lookup did not preserve provider boundaries")
	}
	markers := WorkspaceMarkers(Claude)
	markers[0] = "changed"
	if WorkspaceMarkers(Claude)[0] == "changed" {
		t.Fatal("WorkspaceMarkers should return a defensive copy")
	}
	paths := DefaultManagedAssetPaths()
	if len(paths) == 0 || paths[0] == "" {
		t.Fatalf("DefaultManagedAssetPaths() = %#v", paths)
	}
}

func TestRegistryBoundaryInputsRemainProviderNeutral(t *testing.T) {
	var zero Registry
	if definitions := zero.Definitions(); len(definitions) != len(BuiltinDefinitions()) {
		t.Fatalf("zero-value registry definitions = %d, want %d", len(definitions), len(BuiltinDefinitions()))
	}
	if _, found := zero.BridgePath("unknown"); found {
		t.Fatal("unknown provider should not have a bridge path")
	}
	if _, found := zero.BridgeBody("unknown"); found {
		t.Fatal("unknown provider should not have a bridge body")
	}
	if _, found := Parse(" \t "); found {
		t.Fatal("blank provider ID should not resolve")
	}

	registry := NewRegistry([]Definition{
		{
			ID: Shared,
			Bridges: []BridgeSpec{
				{Path: ""},
				{Path: "."},
				{Path: "../outside"},
				{Path: "./AGENTS.md"},
				{Path: "AGENTS.md/"},
			},
		},
		{
			ID: ID("future"),
			Bridges: []BridgeSpec{
				{Path: ".future/b.md"},
				{Path: ".future/a.md"},
			},
			NativeWorkspacePaths: []NativePathSpec{
				{Path: "../outside", Prefix: true},
				{Path: ".future/config.json"},
				{Path: ".future/cache", Prefix: true},
			},
		},
	})
	if _, found := registry.BridgePath(Shared); found {
		t.Fatal("provider without a native bridge should not have a bridge path")
	}
	if _, found := registry.BridgeBody(Shared); found {
		t.Fatal("provider without a native bridge should not have a bridge body")
	}
	for _, rel := range []string{"", ".", "..", "../outside", string(filepath.Separator) + "absolute"} {
		if registry.IsNativeWorkspacePath(rel) {
			t.Fatalf("unsafe native workspace path %q was accepted", rel)
		}
	}
	if !registry.IsNativeWorkspacePath(".future/config.json") {
		t.Fatal("exact native workspace path was not recognized")
	}
	if !registry.IsNativeWorkspacePath(".future/cache/entry.json") {
		t.Fatal("prefixed native workspace path was not recognized")
	}
	if registry.IsNativeWorkspacePath(".future/cache-adjacent") {
		t.Fatal("adjacent native workspace path should not match a prefix")
	}
	if got := registry.DefaultManagedAssetPaths(); len(got) != 1 || got[0] != "AGENTS.md" {
		t.Fatalf("managed paths = %#v, want [AGENTS.md]", got)
	}

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".future", "a.md"), "a")
	mustWrite(t, filepath.Join(root, ".future", "b.md"), "b")
	detectionRegistry := NewRegistry([]Definition{{
		ID: ID("future"),
		Bridges: []BridgeSpec{
			{Path: ".future/b.md"},
			{Path: ".future/a.md"},
		},
	}})
	bridges := detectionRegistry.DetectBridges(root)
	if len(bridges) != 2 || bridges[0].Path != ".future/a.md" || bridges[1].Path != ".future/b.md" {
		t.Fatalf("same-provider bridges were not sorted by path: %#v", bridges)
	}
}

func mustWrite(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
