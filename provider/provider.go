// SPDX-License-Identifier: Apache-2.0

// Package provider defines data-driven agent-provider registries and artifact
// metadata. Custom registries extend discovery without changing threadpoint.
package provider

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/threadgrid/threadpoint/internal/layoutname"
)

// ID identifies an agent provider or the provider-neutral shared baseline.
type ID string

// Shared identifies provider-neutral artifacts; the remaining values
// identify the built-in provider integrations.
const (
	Shared      ID = "shared"
	Codex       ID = "codex"
	Claude      ID = "claude"
	Copilot     ID = "copilot"
	Antigravity ID = "antigravity"
	Kiro        ID = "kiro"
	OpenCode    ID = "opencode"
	Cursor      ID = "cursor"
	OpenClaw    ID = "openclaw"
)

// Class identifies the broad runtime shape of a provider.
type Class string

// CodingAgent runs directly against a workspace, while
// WorkspaceBackedAgent describes a workspace-hosted runtime.
const (
	CodingAgent          Class = "coding-agent"
	WorkspaceBackedAgent Class = "workspace-backed-agent"
)

// ArtifactScope identifies where an artifact lives. Only project-scoped
// artifacts are modeled today.
type ArtifactScope string

// ArtifactScopeProject identifies artifacts rooted in a project workspace.
const (
	ArtifactScopeProject ArtifactScope = "project"
)

// CanonicalScope optionally pins an artifact to a threadpoint canonical project
// lane. An empty scope leaves the lane to the normal Git-based classification.
type CanonicalScope string

// CanonicalScopeAuto defers lane selection to classification;
// CanonicalScopeProjectLocal selects the local project lane explicitly.
const (
	CanonicalScopeAuto         CanonicalScope = ""
	CanonicalScopeProjectLocal CanonicalScope = "project-local"
)

// ArtifactSpec describes a project-local artifact a provider contributes: its
// path, kind, read mode, canonical scope, and whether a directory is scanned
// recursively.
type ArtifactSpec struct {
	Path           string         `json:"path"`
	Kind           string         `json:"kind"`
	ReadMode       string         `json:"readMode"`
	CanonicalScope CanonicalScope `json:"canonicalScope,omitempty"`
	Recurse        bool           `json:"recurse,omitempty"`
}

// BridgeSpec describes a provider entrypoint file that bridges to the shared
// layout. Native marks a file the provider reads directly (rather than a
// generated pointer to AGENTS.md and .agents/).
type BridgeSpec struct {
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
	Native bool   `json:"native,omitempty"`
}

// NativePathSpec matches a provider's native workspace path. When Prefix is set
// the path and everything beneath it match.
type NativePathSpec struct {
	Path   string `json:"path"`
	Prefix bool   `json:"prefix,omitempty"`
}

// Definition describes one provider's identity, bridges, artifacts, and native
// workspace paths.
type Definition struct {
	ID                   ID               `json:"id"`
	DisplayName          string           `json:"displayName"`
	Class                Class            `json:"class,omitempty"`
	Bridges              []BridgeSpec     `json:"bridges,omitempty"`
	ProjectArtifacts     []ArtifactSpec   `json:"projectArtifacts,omitempty"`
	NativeWorkspacePaths []NativePathSpec `json:"nativeWorkspacePaths,omitempty"`
	// WorkspaceMarkers are specific relative paths used to detect whether the
	// provider is likely installed; NativeWorkspacePaths classify discovered paths.
	WorkspaceMarkers []string `json:"workspaceMarkers,omitempty"`
}

// Registry is an immutable, ordered set of provider definitions, safe for
// concurrent reads. Constructors copy inputs, and exported reads return copies.
type Registry struct {
	definitions []Definition
}

// Bridge is a provider bridge file discovered in a workspace: which provider it
// belongs to, its path, and whether it is the provider's native file.
type Bridge struct {
	ID     ID     `json:"provider"`
	Path   string `json:"path"`
	Native bool   `json:"native"`
}

// NewRegistry returns an immutable Registry built from a copy of definitions, so
// later mutation of the input slice does not affect the registry.
func NewRegistry(definitions []Definition) Registry {
	return Registry{definitions: cloneDefinitions(definitions)}
}

// BuiltinRegistry returns a Registry of threadpoint's built-in provider
// definitions.
func BuiltinRegistry() Registry {
	return NewRegistry(BuiltinDefinitions())
}

// BuiltinDefinitions returns a fresh copy of threadpoint's built-in provider
// definitions, suitable as a starting point for a custom Registry.
func BuiltinDefinitions() []Definition {
	return []Definition{
		{
			ID:          Shared,
			DisplayName: "Shared",
			Bridges: []BridgeSpec{
				{Path: layoutname.GuideFile},
				{Path: filepath.ToSlash(filepath.Join(layoutname.AgentsDir, layoutname.KnowledgeDir))},
			},
			ProjectArtifacts: []ArtifactSpec{
				{Path: layoutname.GuideFile, Kind: "instruction", ReadMode: "catalog-only"},
				{Path: filepath.ToSlash(filepath.Join(layoutname.AgentsDir, layoutname.KnowledgeDir)), Kind: "knowledge", ReadMode: "catalog-only", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(layoutname.AgentsDir, layoutname.RulesDir)), Kind: "rule", ReadMode: "catalog-only", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(layoutname.AgentsDir, layoutname.SkillsDir)), Kind: "skill", ReadMode: "catalog-only", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(layoutname.AgentsDir, layoutname.PluginsDir, layoutname.MarketplaceFile)), Kind: "plugin", ReadMode: "catalog-only"},
			},
		},
		{
			ID:          Codex,
			DisplayName: "Codex",
			Class:       CodingAgent,
			Bridges: []BridgeSpec{
				{
					Path:   filepath.ToSlash(filepath.Join(".codex", "AGENTS.md")),
					Body:   "# Codex Bridge\n\nUse ../AGENTS.md and ../.agents/ as the canonical shared agent setup. Keep this file as the provider entrypoint to the shared setup.\n",
					Native: true,
				},
			},
			ProjectArtifacts: []ArtifactSpec{
				{Path: filepath.ToSlash(filepath.Join(".codex", "AGENTS.md")), Kind: "instruction", ReadMode: "import"},
				{Path: filepath.ToSlash(filepath.Join(".codex", "config.toml")), Kind: "config", ReadMode: "manual-review"},
				{Path: filepath.ToSlash(filepath.Join(".codex", "hooks.json")), Kind: "hook", ReadMode: "manual-review"},
				{Path: filepath.ToSlash(filepath.Join(".codex", "hooks")), Kind: "hook", ReadMode: "manual-review", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".codex", "skills")), Kind: "skill", ReadMode: "import", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".codex", "plugins")), Kind: "plugin", ReadMode: "manual-review", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".codex-plugin", "plugin.json")), Kind: "plugin", ReadMode: "manual-review"},
			},
			NativeWorkspacePaths: []NativePathSpec{
				{Path: ".codex", Prefix: true},
			},
			WorkspaceMarkers: []string{".codex"},
		},
		{
			ID:          Claude,
			DisplayName: "Claude",
			Class:       CodingAgent,
			Bridges: []BridgeSpec{
				{
					Path:   "CLAUDE.md",
					Body:   "# Claude Bridge\n\nUse AGENTS.md and .agents/ as the canonical shared agent setup. Keep this file as the provider entrypoint to the shared setup.\n",
					Native: true,
				},
			},
			ProjectArtifacts: []ArtifactSpec{
				{Path: "CLAUDE.md", Kind: "instruction", ReadMode: "import"},
				{Path: "CLAUDE.local.md", Kind: "instruction", ReadMode: "import", CanonicalScope: CanonicalScopeProjectLocal},
				{Path: filepath.ToSlash(filepath.Join(".claude", "CLAUDE.md")), Kind: "instruction", ReadMode: "import"},
				{Path: filepath.ToSlash(filepath.Join(".claude", "settings.json")), Kind: "config", ReadMode: "manual-review"},
				{Path: filepath.ToSlash(filepath.Join(".claude", "settings.local.json")), Kind: "config", ReadMode: "manual-review"},
				{Path: filepath.ToSlash(filepath.Join(".claude", "commands")), Kind: "command", ReadMode: "import", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".claude", "skills")), Kind: "skill", ReadMode: "import", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".claude", "agents")), Kind: "agent", ReadMode: "import", Recurse: true},
			},
			NativeWorkspacePaths: []NativePathSpec{
				{Path: "CLAUDE.md"},
				{Path: "CLAUDE.local.md"},
				{Path: ".claude", Prefix: true},
			},
			WorkspaceMarkers: []string{"CLAUDE.md", ".claude"},
		},
		{
			ID:          Copilot,
			DisplayName: "Copilot",
			Class:       CodingAgent,
			Bridges: []BridgeSpec{
				{
					Path:   filepath.ToSlash(filepath.Join(".github", "copilot-instructions.md")),
					Body:   "# Copilot Bridge\n\nUse AGENTS.md and .agents/ as the canonical shared agent setup. Keep this file as the provider entrypoint to the shared setup.\n",
					Native: true,
				},
			},
			ProjectArtifacts: []ArtifactSpec{
				{Path: filepath.ToSlash(filepath.Join(".github", "copilot-instructions.md")), Kind: "instruction", ReadMode: "import"},
				{Path: filepath.ToSlash(filepath.Join(".github", "instructions")), Kind: "rule", ReadMode: "import", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".github", "prompts")), Kind: "prompt", ReadMode: "import", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".github", "skills")), Kind: "skill", ReadMode: "import", Recurse: true},
			},
			NativeWorkspacePaths: []NativePathSpec{
				{Path: filepath.ToSlash(filepath.Join(".github", "copilot-instructions.md"))},
				{Path: filepath.ToSlash(filepath.Join(".github", "instructions")), Prefix: true},
				{Path: filepath.ToSlash(filepath.Join(".github", "prompts")), Prefix: true},
				{Path: filepath.ToSlash(filepath.Join(".github", "skills")), Prefix: true},
			},
			WorkspaceMarkers: []string{
				filepath.ToSlash(filepath.Join(".github", "copilot-instructions.md")),
				filepath.ToSlash(filepath.Join(".github", "instructions")),
			},
		},
		{
			ID:          Antigravity,
			DisplayName: "Antigravity",
			Class:       CodingAgent,
			Bridges: []BridgeSpec{
				{
					Path:   "GEMINI.md",
					Body:   "# Antigravity Bridge\n\nUse AGENTS.md and .agents/ as the canonical shared agent setup. Keep this file as the provider entrypoint to the shared setup.\n",
					Native: true,
				},
			},
			ProjectArtifacts: []ArtifactSpec{
				{Path: "GEMINI.md", Kind: "instruction", ReadMode: "import"},
				{Path: filepath.ToSlash(filepath.Join(".agent", "rules")), Kind: "rule", ReadMode: "import", Recurse: true},
			},
			NativeWorkspacePaths: []NativePathSpec{
				{Path: "GEMINI.md"},
				{Path: ".agent", Prefix: true},
			},
			WorkspaceMarkers: []string{"GEMINI.md", ".agent"},
		},
		{
			ID:          Kiro,
			DisplayName: "Kiro",
			Class:       CodingAgent,
			Bridges: []BridgeSpec{
				{
					Path:   filepath.ToSlash(filepath.Join(".kiro", "steering", "threadpoint.md")),
					Body:   "# Kiro Bridge\n\nUse AGENTS.md and .agents/ as the canonical shared agent setup. Keep this file as the provider entrypoint to the shared setup.\n",
					Native: true,
				},
			},
			ProjectArtifacts: []ArtifactSpec{
				{Path: filepath.ToSlash(filepath.Join(".kiro", "steering")), Kind: "rule", ReadMode: "import", Recurse: true},
			},
			NativeWorkspacePaths: []NativePathSpec{
				{Path: filepath.ToSlash(filepath.Join(".kiro", "steering")), Prefix: true},
			},
			WorkspaceMarkers: []string{".kiro"},
		},
		{
			ID:          OpenCode,
			DisplayName: "OpenCode",
			Class:       CodingAgent,
			ProjectArtifacts: []ArtifactSpec{
				{Path: "opencode.json", Kind: "config", ReadMode: "manual-review"},
				{Path: "opencode.jsonc", Kind: "config", ReadMode: "manual-review"},
				{Path: filepath.ToSlash(filepath.Join(".opencode", "agents")), Kind: "agent", ReadMode: "import", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".opencode", "commands")), Kind: "command", ReadMode: "import", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".opencode", "skills")), Kind: "skill", ReadMode: "import", Recurse: true},
				{Path: filepath.ToSlash(filepath.Join(".opencode", "plugins")), Kind: "plugin", ReadMode: "manual-review", Recurse: true},
			},
			NativeWorkspacePaths: []NativePathSpec{
				{Path: "opencode.json"},
				{Path: "opencode.jsonc"},
				{Path: ".opencode", Prefix: true},
			},
			WorkspaceMarkers: []string{"opencode.json", "opencode.jsonc", ".opencode"},
		},
		{
			ID:          Cursor,
			DisplayName: "Cursor",
			Class:       CodingAgent,
			Bridges: []BridgeSpec{
				{
					Path:   filepath.ToSlash(filepath.Join(".cursor", "rules", "threadpoint.mdc")),
					Body:   "---\nalwaysApply: true\n---\n# Cursor Bridge\n\nUse AGENTS.md and .agents/ as the canonical shared agent setup. Keep this file as the provider entrypoint to the shared setup.\n",
					Native: true,
				},
			},
			ProjectArtifacts: []ArtifactSpec{
				{Path: filepath.ToSlash(filepath.Join(".cursor", "rules")), Kind: "rule", ReadMode: "import", Recurse: true},
				{Path: ".cursorrules", Kind: "instruction", ReadMode: "import"},
			},
			NativeWorkspacePaths: []NativePathSpec{
				{Path: filepath.ToSlash(filepath.Join(".cursor", "rules")), Prefix: true},
				{Path: ".cursorrules"},
			},
			WorkspaceMarkers: []string{".cursor", ".cursorrules"},
		},
		{
			ID:          OpenClaw,
			DisplayName: "OpenClaw",
			Class:       WorkspaceBackedAgent,
			ProjectArtifacts: []ArtifactSpec{
				{Path: "SOUL.md", Kind: "instruction", ReadMode: "import"},
				{Path: "TOOLS.md", Kind: "instruction", ReadMode: "manual-review"},
				{Path: "IDENTITY.md", Kind: "instruction", ReadMode: "manual-review"},
				{Path: "USER.md", Kind: "knowledge", ReadMode: "manual-review"},
				{Path: "HEARTBEAT.md", Kind: "instruction", ReadMode: "manual-review"},
				{Path: "BOOTSTRAP.md", Kind: "instruction", ReadMode: "manual-review"},
				{Path: "MEMORY.md", Kind: "knowledge", ReadMode: "import"},
				{Path: "memory", Kind: "knowledge", ReadMode: "import", Recurse: true},
				{Path: "skills", Kind: "skill", ReadMode: "import", Recurse: true},
			},
			NativeWorkspacePaths: []NativePathSpec{
				{Path: "AGENTS.md"},
				{Path: "SOUL.md"},
				{Path: "TOOLS.md"},
				{Path: "IDENTITY.md"},
				{Path: "USER.md"},
				{Path: "HEARTBEAT.md"},
				{Path: "BOOTSTRAP.md"},
				{Path: "MEMORY.md"},
				{Path: "memory", Prefix: true},
				{Path: "skills", Prefix: true},
			},
			WorkspaceMarkers: []string{"SOUL.md", "MEMORY.md", "skills"},
		},
	}
}

// Definitions returns a deep copy of the registry's provider definitions. The
// copy keeps the immutable-by-construction guarantee true from the outside, so
// callers may read or mutate the result freely; internal read-only callers use
// the cheaper non-cloning accessor, so this clone is never on a hot path.
func (r Registry) Definitions() []Definition {
	if len(r.definitions) == 0 {
		return BuiltinDefinitions()
	}
	return cloneDefinitions(r.definitions)
}

// builtinDefs is the immutable backing store for zero-value registries.
var builtinDefs = BuiltinDefinitions()

// definitionsForRead returns package-owned data that callers must not mutate.
func (r Registry) definitionsForRead() []Definition {
	if len(r.definitions) == 0 {
		return builtinDefs
	}
	return r.definitions
}

// Lookup returns the definition for provider.
func (r Registry) Lookup(provider ID) (Definition, bool) {
	for _, definition := range r.definitionsForRead() {
		if definition.ID == provider {
			return cloneDefinition(definition), true
		}
	}
	return Definition{}, false
}

// IDs returns the registry's providers in definition order.
func (r Registry) IDs() []ID {
	var out []ID
	for _, definition := range r.definitionsForRead() {
		out = append(out, definition.ID)
	}
	return out
}

// ListString returns provider IDs as a comma-separated list.
func (r Registry) ListString() string {
	ids := r.IDs()
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, string(id))
	}
	return strings.Join(values, ",")
}

// BridgePath returns the native bridge path for provider when one exists.
func (r Registry) BridgePath(provider ID) (string, bool) {
	definition, ok := r.Lookup(provider)
	if !ok {
		return "", false
	}
	for _, bridge := range definition.Bridges {
		if bridge.Native {
			return filepath.ToSlash(bridge.Path), true
		}
	}
	return "", false
}

// BridgeBody returns the native bridge body for provider when one exists.
func (r Registry) BridgeBody(provider ID) (string, bool) {
	definition, ok := r.Lookup(provider)
	if !ok {
		return "", false
	}
	for _, bridge := range definition.Bridges {
		if bridge.Native {
			return bridge.Body, true
		}
	}
	return "", false
}

// IsNativeWorkspacePath reports whether rel is a provider-native workspace path.
func (r Registry) IsNativeWorkspacePath(rel string) bool {
	rel = cleanRel(rel)
	if rel == "" {
		return false
	}
	for _, definition := range r.definitionsForRead() {
		for _, native := range definition.NativeWorkspacePaths {
			path := cleanRel(native.Path)
			if path == "" {
				continue
			}
			if rel == path || (native.Prefix && strings.HasPrefix(rel, path+"/")) {
				return true
			}
		}
	}
	return false
}

// DefaultManagedAssetPaths returns the shared canonical paths managed by
// threadpoint. Provider-native bridge files are excluded.
func (r Registry) DefaultManagedAssetPaths() []string {
	seen := map[string]bool{}
	var paths []string
	for _, definition := range r.definitionsForRead() {
		if definition.ID != Shared {
			continue
		}
		for _, bridge := range definition.Bridges {
			addPath(&paths, seen, bridge.Path)
		}
	}
	return paths
}

// DetectBridges returns provider bridge files present under root.
func (r Registry) DetectBridges(root string) []Bridge {
	var found []Bridge
	for _, definition := range r.definitionsForRead() {
		for _, candidate := range definition.Bridges {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(candidate.Path))); err == nil {
				found = append(found, Bridge{
					ID:     definition.ID,
					Path:   filepath.ToSlash(candidate.Path),
					Native: candidate.Native,
				})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].ID == found[j].ID {
			return found[i].Path < found[j].Path
		}
		return found[i].ID < found[j].ID
	})
	return found
}

// WorkspaceMarkers returns a copy of provider's detection markers, or nil when
// the provider is unknown or has none.
func (r Registry) WorkspaceMarkers(provider ID) []string {
	for _, definition := range r.definitionsForRead() {
		if definition.ID == provider {
			return append([]string(nil), definition.WorkspaceMarkers...)
		}
	}
	return nil
}

// Lookup returns the built-in definition for provider.
func Lookup(provider ID) (Definition, bool) {
	return BuiltinRegistry().Lookup(provider)
}

// Parse normalizes raw and returns a known provider ID when it exists.
func Parse(raw string) (ID, bool) {
	provider := ID(strings.ToLower(strings.TrimSpace(raw)))
	if provider == "" {
		return "", false
	}
	_, ok := Lookup(provider)
	return provider, ok
}

// IDs returns built-in provider IDs in definition order.
func IDs() []ID {
	return BuiltinRegistry().IDs()
}

// ListString returns built-in provider IDs as a comma-separated list.
func ListString() string {
	return BuiltinRegistry().ListString()
}

// BridgePath returns the built-in native bridge path for provider when one exists.
func BridgePath(provider ID) (string, bool) {
	return BuiltinRegistry().BridgePath(provider)
}

// BridgeBody returns the built-in native bridge body for provider when one exists.
func BridgeBody(provider ID) (string, bool) {
	return BuiltinRegistry().BridgeBody(provider)
}

// IsNativeWorkspacePath reports whether rel is native to any built-in provider.
func IsNativeWorkspacePath(rel string) bool {
	return BuiltinRegistry().IsNativeWorkspacePath(rel)
}

// WorkspaceMarkers returns the built-in detection markers for provider.
func WorkspaceMarkers(provider ID) []string {
	return BuiltinRegistry().WorkspaceMarkers(provider)
}

// DefaultManagedAssetPaths returns built-in shared and provider bridge paths.
func DefaultManagedAssetPaths() []string {
	return BuiltinRegistry().DefaultManagedAssetPaths()
}

// DetectBridges returns built-in provider bridge files present under root.
func DetectBridges(root string) []Bridge {
	return BuiltinRegistry().DetectBridges(root)
}

func cloneDefinition(definition Definition) Definition {
	clone := definition
	clone.Bridges = append([]BridgeSpec(nil), definition.Bridges...)
	clone.ProjectArtifacts = append([]ArtifactSpec(nil), definition.ProjectArtifacts...)
	clone.NativeWorkspacePaths = append([]NativePathSpec(nil), definition.NativeWorkspacePaths...)
	clone.WorkspaceMarkers = append([]string(nil), definition.WorkspaceMarkers...)
	return clone
}

func cloneDefinitions(definitions []Definition) []Definition {
	out := make([]Definition, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, cloneDefinition(definition))
	}
	return out
}

func addPath(paths *[]string, seen map[string]bool, rel string) {
	rel = cleanRel(rel)
	if rel == "" || seen[rel] {
		return
	}
	seen[rel] = true
	*paths = append(*paths, rel)
}

func cleanRel(rel string) string {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	rel = strings.TrimPrefix(rel, "./")
	rel = strings.TrimSuffix(rel, "/")
	if rel == "" || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	return rel
}
