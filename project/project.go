// SPDX-License-Identifier: Apache-2.0

// Package project resolves the one project directory used by a command.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/threadgrid/threadpoint/layout"
	"github.com/threadgrid/threadpoint/provider"
	"github.com/threadgrid/threadpoint/safefs"
)

// Marker identifies a filesystem signal that can identify a project root.
type Marker string

const (
	// MarkerGit identifies a Git worktree, repository, or submodule root.
	MarkerGit Marker = "git"
	// MarkerAgentsGuide identifies the shared canonical AGENTS.md guide.
	MarkerAgentsGuide Marker = "agents-guide"
	// MarkerAgentsDirectory identifies the shared canonical .agents directory.
	MarkerAgentsDirectory Marker = "agents-directory"
	// MarkerLocalGuide identifies the canonical AGENTS.local.md guide.
	MarkerLocalGuide Marker = "local-guide"
	// MarkerLocalDirectory identifies the canonical .agents.local directory.
	MarkerLocalDirectory Marker = "local-directory"
	// MarkerProviderWorkspace identifies a provider's curated workspace marker.
	MarkerProviderWorkspace Marker = "provider-workspace"
)

// Candidate is one directory and the markers found there.
type Candidate struct {
	Path    string   `json:"path"`
	Markers []Marker `json:"markers"`
}

// Options configures project-root discovery.
type Options struct {
	// Root is an explicit root or the current-directory input for implicit discovery.
	Root string
	// Explicit disables implicit ancestor discovery and preserves Root as the project root.
	// An explicit Root must not be empty or whitespace-only.
	Explicit bool
	// AllowMissing permits an explicit non-existent root for commands that create it.
	AllowMissing bool
	// WorkDir resolves relative Root values. An empty value uses the current directory.
	WorkDir string
	// Registry supplies curated provider workspace markers. A zero value uses built-ins.
	Registry provider.Registry
}

// Selection records the supplied input root and the resulting selected project root.
type Selection struct {
	InputRoot   string `json:"inputRoot"`
	ProjectRoot string `json:"projectRoot"`
}

// AmbiguityError reports a native fallback beneath a stronger ancestor marker.
type AmbiguityError struct {
	Native   Candidate
	Ancestor Candidate
}

// Error implements error.
func (e *AmbiguityError) Error() string {
	return fmt.Sprintf("ambiguous project root: native marker at %s (%s) is below ancestor project marker at %s (%s); pass --root to select one", e.Native.Path, markerList(e.Native.Markers), e.Ancestor.Path, markerList(e.Ancestor.Markers))
}

// Discover resolves a single project root. Explicit roots remain exact logical
// absolute paths. Implicit discovery selects the nearest canonical or Git marker;
// a provider-native marker is a fallback only when no ancestor strong marker exists.
func Discover(opts Options) (Selection, error) {
	if opts.Explicit && strings.TrimSpace(opts.Root) == "" {
		return Selection{}, errors.New("explicit root cannot be empty")
	}
	input, err := normalizeInput(opts.Root, opts.WorkDir)
	if err != nil {
		return Selection{}, err
	}
	if err := validateRoot(input, opts.AllowMissing); err != nil {
		return Selection{}, err
	}
	if opts.Explicit {
		return Selection{InputRoot: input, ProjectRoot: input}, nil
	}

	home, hasHome := normalizedUserHome()
	workspaceMarkers := providerWorkspaceMarkers(opts.Registry)
	var native *Candidate
	for current := input; ; current = filepath.Dir(current) {
		if hasHome && sameResolvedPath(current, home) {
			break
		}
		strong := strongMarkers(current)
		if len(strong) > 0 {
			candidate := Candidate{Path: current, Markers: strong}
			if native != nil {
				return Selection{}, &AmbiguityError{Native: *native, Ancestor: candidate}
			}
			return Selection{InputRoot: input, ProjectRoot: current}, nil
		}
		if native == nil && hasWorkspaceMarker(current, workspaceMarkers) {
			native = &Candidate{Path: current, Markers: []Marker{MarkerProviderWorkspace}}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if native != nil {
		return Selection{InputRoot: input, ProjectRoot: native.Path}, nil
	}
	return Selection{InputRoot: input, ProjectRoot: input}, nil
}

func normalizeInput(root string, workDir string) (string, error) {
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	if strings.TrimSpace(workDir) == "" {
		cwd, err := logicalWorkingDirectory()
		if err != nil {
			return "", err
		}
		workDir = cwd
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(workDir, root)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func logicalWorkingDirectory() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if logical := strings.TrimSpace(os.Getenv("PWD")); filepath.IsAbs(logical) && sameResolvedPath(logical, cwd) {
		return filepath.Clean(logical), nil
	}
	return filepath.Clean(cwd), nil
}

func validateRoot(root string, allowMissing bool) error {
	// Resolve the longest existing ancestor and reattach any not-yet-created
	// suffix, so a symlinked ancestor (for example /tmp/link -> ~/.agents) cannot
	// pass the prohibited-root checks lexically and then have the actual mkdir
	// follow the symlink and escape into the prohibited tree.
	physical, err := safefs.ResolveRootForMutation(root)
	if err != nil {
		return fmt.Errorf("resolve root %s: %w", root, err)
	}
	if isFilesystemRoot(root) || isFilesystemRoot(physical) {
		return errors.New("root cannot be filesystem root")
	}
	if home, ok := normalizedUserHome(); ok && sameResolvedPath(physical, home) {
		return errors.New("root cannot be the home directory")
	}
	if userAgentsRoot, ok := normalizedUserAgentsRoot(); ok && pathIsUnder(physical, userAgentsRoot) {
		return errors.New("root cannot be under the user .agents directory")
	}
	info, err := os.Stat(root)
	if err != nil {
		if allowMissing && os.IsNotExist(err) {
			return nil
		}
		if os.IsNotExist(err) {
			return fmt.Errorf("root does not exist: %s", root)
		}
		return fmt.Errorf("inspect root %s: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("root is not a directory: %s", root)
	}
	return nil
}

func strongMarkers(root string) []Marker {
	markers := make([]Marker, 0, 5)
	if gitMarkerExists(root) {
		markers = append(markers, MarkerGit)
	}
	if regularFileExists(root, layout.GuideFile) {
		markers = append(markers, MarkerAgentsGuide)
	}
	if directoryExists(root, layout.AgentsDir) {
		markers = append(markers, MarkerAgentsDirectory)
	}
	if regularFileExists(root, "AGENTS.local.md") {
		markers = append(markers, MarkerLocalGuide)
	}
	if directoryExists(root, ".agents.local") {
		markers = append(markers, MarkerLocalDirectory)
	}
	return markers
}

func gitMarkerExists(root string) bool {
	info, err := os.Lstat(filepath.Join(root, ".git"))
	return err == nil && info.Mode()&os.ModeSymlink == 0 && (info.IsDir() || info.Mode().IsRegular())
}

func regularFileExists(root string, name string) bool {
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func directoryExists(root string, name string) bool {
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir()
}

func providerWorkspaceMarkers(registry provider.Registry) []string {
	seen := map[string]struct{}{}
	for _, definition := range registry.Definitions() {
		for _, marker := range definition.WorkspaceMarkers {
			clean := cleanRelativeMarker(marker)
			if clean != "" {
				seen[clean] = struct{}{}
			}
		}
	}
	markers := make([]string, 0, len(seen))
	for marker := range seen {
		markers = append(markers, marker)
	}
	sort.Strings(markers)
	return markers
}

func cleanRelativeMarker(marker string) string {
	marker = filepath.Clean(filepath.FromSlash(strings.TrimSpace(marker)))
	if marker == "." || marker == "" || filepath.IsAbs(marker) || marker == ".." || strings.HasPrefix(marker, ".."+string(filepath.Separator)) {
		return ""
	}
	return marker
}

func hasWorkspaceMarker(root string, markers []string) bool {
	for _, marker := range markers {
		info, err := os.Lstat(filepath.Join(root, marker))
		if err == nil && info.Mode()&os.ModeSymlink == 0 && (info.Mode().IsRegular() || info.IsDir()) {
			return true
		}
	}
	return false
}

func normalizedUserHome() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", false
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", false
	}
	return resolveExistingPath(abs), true
}

func normalizedUserAgentsRoot() (string, bool) {
	home, ok := normalizedUserHome()
	if !ok {
		return "", false
	}
	return filepath.Join(home, layout.AgentsDir), true
}

func resolveExistingPath(path string) string {
	clean := filepath.Clean(path)
	if evaluated, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(evaluated)
	}
	return clean
}

func sameResolvedPath(left string, right string) bool {
	return resolveExistingPath(left) == resolveExistingPath(right)
}

func pathIsUnder(path string, root string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func isFilesystemRoot(path string) bool {
	volume := filepath.VolumeName(path)
	return filepath.Clean(path) == volume+string(filepath.Separator)
}

func markerList(markers []Marker) string {
	parts := make([]string, len(markers))
	for i, marker := range markers {
		parts[i] = string(marker)
	}
	return strings.Join(parts, ",")
}
