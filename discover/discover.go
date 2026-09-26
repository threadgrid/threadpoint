// SPDX-License-Identifier: Apache-2.0

// Package discover safely inventories provider-native project artifacts for
// the explicit stage/commit workflow. It never writes project content.
package discover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/provider"
	"github.com/threadgrid/threadpoint/safefs"
	"github.com/threadgrid/threadpoint/walk"
)

const (
	maxImportFileSize = walk.DefaultMaxFileSize
	maxWalkFiles      = walk.DefaultMaxWalkFiles
	maxWalkEntries    = walk.DefaultMaxWalkEntries
	maxWalkDepth      = walk.DefaultMaxWalkDepth
	maxWalkPathBytes  = walk.DefaultMaxWalkPathBytes
	walkReadBatchSize = 128
)

// Kind classifies a provider-native artifact.
type Kind string

// KindInstruction through KindDirectory identify the artifact shapes emitted
// by discovery.
const (
	KindInstruction Kind = "instruction"
	KindKnowledge   Kind = "knowledge"
	KindRule        Kind = "rule"
	KindSkill       Kind = "skill"
	KindPlugin      Kind = "plugin"
	KindConfig      Kind = "config"
	KindHook        Kind = "hook"
	KindPrompt      Kind = "prompt"
	KindCommand     Kind = "command"
	KindAgent       Kind = "agent"
	KindDirectory   Kind = "directory"
)

// ReadMode reports an artifact's staging eligibility. Only ReadImport can enter
// a review stage; the other modes are catalog and diagnostic results.
type ReadMode string

// ReadImport is eligible for staging. ReadManual, ReadCatalog, and ReadDenied
// require progressively more restrictive handling by the caller.
const (
	ReadImport  ReadMode = "import"
	ReadManual  ReadMode = "manual-review"
	ReadCatalog ReadMode = "catalog-only"
	ReadDenied  ReadMode = "denied"
)

// Options limits discovery to one selected project root and a provider set.
type Options struct {
	Root                      string
	SkipDirNames              []string
	Providers                 []provider.ID
	ExcludeProviders          []provider.ID
	ProviderSelectionExplicit bool
	Registry                  provider.Registry
}

// Artifact describes one provider-native artifact under the selected project.
type Artifact struct {
	Provider       provider.ID             `json:"provider"`
	Root           string                  `json:"root"`
	Path           string                  `json:"path"`
	AbsPath        string                  `json:"absPath,omitempty"`
	Kind           Kind                    `json:"kind"`
	ReadMode       ReadMode                `json:"readMode"`
	CanonicalScope provider.CanonicalScope `json:"canonicalScope,omitempty"`
	Size           int64                   `json:"size,omitempty"`
	SHA256         string                  `json:"sha256,omitempty"`
	Directory      bool                    `json:"directory,omitempty"`
	Symlink        bool                    `json:"symlink,omitempty"`
	Code           string                  `json:"code,omitempty"`
	Message        string                  `json:"message,omitempty"`
}

// Report lists artifacts and read-only scan warnings.
type Report struct {
	Root      string     `json:"root"`
	Artifacts []Artifact `json:"artifacts"`
	Warnings  []string   `json:"warnings,omitempty"`
}

type candidate struct {
	provider       provider.ID
	rel            string
	kind           Kind
	mode           ReadMode
	canonicalScope provider.CanonicalScope
	recurse        bool
}

type rootSnapshot struct {
	logical  string
	physical string
	info     fs.FileInfo
}

type discoveryBudget struct {
	entries   int
	pathBytes int
	exhausted bool
}

func (budget *discoveryBudget) charge(path string) error {
	if budget == nil {
		return errors.New("discovery traversal budget is required")
	}
	if budget.exhausted {
		return errors.New("discovery traversal budget is exhausted")
	}
	budget.entries++
	budget.pathBytes += len(filepath.ToSlash(path))
	if budget.entries > maxWalkEntries || budget.pathBytes > maxWalkPathBytes {
		budget.exhausted = true
		return fmt.Errorf("discovery traversal exceeded %d entries or %d path bytes", maxWalkEntries, maxWalkPathBytes)
	}
	return nil
}

// Run inventories provider artifacts beneath opts.Root. It does not
// traverse parent directories, a user home, or any other project.
func Run(ctx context.Context, opts Options) (*Report, error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, root, err := captureAndOpenRoot(normalized.Root)
	if err != nil {
		return nil, err
	}
	report := &Report{Root: normalized.Root}
	if root == nil {
		return report, nil
	}
	artifacts, warnings := discoverProjectRoot(ctx, normalized, root)
	report.Artifacts = artifacts
	report.Warnings = warnings
	sortArtifacts(report.Artifacts)
	validationErr := validatePinnedRoot(snapshot, root)
	closeErr := root.Close()
	if validationErr != nil || closeErr != nil {
		return nil, errors.Join(validationErr, closeErr)
	}
	return report, nil
}

// RunFromRoot inventories provider artifacts through a caller-retained
// project root. The root is borrowed and remains owned by the caller. This
// variant never resolves or reopens opts.Root, so a lock-owning caller can keep
// discovery bound to the same filesystem generation as its mutation decision.
func RunFromRoot(ctx context.Context, opts Options, root *os.Root) (*Report, error) {
	if root == nil {
		return nil, errors.New("retained discovery project root is required")
	}
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := root.Stat(".")
	if err != nil || !before.IsDir() {
		return nil, errors.Join(errors.New("retained discovery project root is not a directory"), err)
	}
	artifacts, warnings := discoverProjectRoot(ctx, normalized, root)
	after, err := root.Stat(".")
	if err != nil || !after.IsDir() || !os.SameFile(before, after) {
		return nil, errors.Join(errors.New("retained discovery project root changed during discovery"), err)
	}
	report := &Report{Root: normalized.Root, Artifacts: artifacts, Warnings: warnings}
	sortArtifacts(report.Artifacts)
	return report, nil
}

func normalizeOptions(opts Options) (Options, error) {
	if strings.TrimSpace(opts.Root) == "" {
		opts.Root = "."
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return opts, err
	}
	opts.Root = filepath.Clean(root)
	normalizedSkipDirs, err := walk.NormalizeSkipDirNames(opts.SkipDirNames)
	if err != nil {
		return opts, err
	}
	opts.SkipDirNames = normalizedSkipDirs
	if err := validateRegistryReadModes(opts.Registry); err != nil {
		return opts, err
	}
	if err := validateProviders(opts.Registry, opts.Providers); err != nil {
		return opts, err
	}
	if err := validateProviders(opts.Registry, opts.ExcludeProviders); err != nil {
		return opts, err
	}
	sort.Slice(opts.Providers, func(i, j int) bool { return opts.Providers[i] < opts.Providers[j] })
	sort.Slice(opts.ExcludeProviders, func(i, j int) bool { return opts.ExcludeProviders[i] < opts.ExcludeProviders[j] })
	return opts, nil
}

func validateRegistryReadModes(registry provider.Registry) error {
	for _, definition := range registry.Definitions() {
		for _, artifact := range definition.ProjectArtifacts {
			if !validReadMode(ReadMode(artifact.ReadMode)) {
				return fmt.Errorf("provider %q project artifact %q has unsupported read mode %q", definition.ID, artifact.Path, artifact.ReadMode)
			}
		}
	}
	return nil
}

func validReadMode(mode ReadMode) bool {
	switch mode {
	case ReadImport, ReadManual, ReadCatalog, ReadDenied:
		return true
	default:
		return false
	}
}

func validateProviders(registry provider.Registry, providers []provider.ID) error {
	for _, providerID := range providers {
		if _, ok := registry.Lookup(providerID); !ok {
			return fmt.Errorf("unknown provider %q", providerID)
		}
	}
	return nil
}

func discoverProjectRoot(ctx context.Context, opts Options, root *os.Root) ([]Artifact, []string) {
	var artifacts []Artifact
	var warnings []string
	budget := &discoveryBudget{}
	for _, spec := range projectCandidates(opts.Registry) {
		if !providerAllowed(opts, spec.provider) {
			continue
		}
		if budget.exhausted {
			break
		}
		found, foundWarnings := scanCandidateWithBudget(ctx, opts.Root, root, spec, opts.SkipDirNames, budget)
		artifacts = append(artifacts, found...)
		warnings = append(warnings, foundWarnings...)
	}
	sortArtifacts(artifacts)
	return artifacts, warnings
}

func projectCandidates(registry provider.Registry) []candidate {
	var out []candidate
	for _, definition := range registry.Definitions() {
		for _, spec := range definition.ProjectArtifacts {
			out = append(out, candidate{
				provider: definition.ID, rel: filepath.ToSlash(spec.Path), kind: Kind(spec.Kind),
				mode: ReadMode(spec.ReadMode), canonicalScope: spec.CanonicalScope, recurse: spec.Recurse,
			})
		}
	}
	return out
}

func scanCandidate(ctx context.Context, logicalRoot string, root *os.Root, spec candidate) ([]Artifact, []string) {
	return scanCandidateWithBudget(ctx, logicalRoot, root, spec, nil, &discoveryBudget{})
}

func scanCandidateWithBudget(ctx context.Context, logicalRoot string, root *os.Root, spec candidate, skipDirNames []string, budget *discoveryBudget) ([]Artifact, []string) {
	name, err := cleanRootName(spec.rel)
	if err != nil {
		return nil, []string{fmt.Sprintf("failed resolving %s: %v", spec.rel, err)}
	}
	if err := safefs.RejectRootSymlinkAncestors(root, filepath.Dir(name)); err != nil {
		return nil, []string{fmt.Sprintf("failed reading %s: %v", spec.rel, err)}
	}
	abs := filepath.Join(logicalRoot, name)
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("failed reading %s: %v", spec.rel, err)}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return []Artifact{artifactFromInfo(root, logicalRoot, spec, abs, info, artifactOutcome{})}, nil
	}

	directory := artifactFromInfo(root, logicalRoot, spec, abs, info, artifactOutcome{Code: "discovery_directory_cataloged", Message: "Directory cataloged; safe files are listed separately when applicable."})
	directory.Kind = KindDirectory
	directory.Directory = true
	if !spec.recurse || directory.ReadMode == ReadDenied {
		return []Artifact{directory}, nil
	}

	artifacts := []Artifact{directory}
	var warnings []string
	candidateRoot, err := root.OpenRoot(name)
	if err != nil {
		return artifacts, append(warnings, fmt.Sprintf("failed opening %s from the pinned project root: %v", spec.rel, err))
	}
	opened, err := candidateRoot.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
		return artifacts, append(warnings, fmt.Sprintf("candidate %s changed while its directory was pinned: %v", spec.rel, errors.Join(err, candidateRoot.Close())))
	}
	defer func() {
		if err := candidateRoot.Close(); err != nil {
			warnings = append(warnings, fmt.Sprintf("failed closing pinned candidate %s: %v", spec.rel, err))
		}
	}()

	walkedFiles := 0
	stopped := false
	var visitDirectory func(*os.Root, string, int) error
	visitDirectory = func(directoryRoot *os.Root, walkPrefix string, depth int) (returnErr error) {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory, err := directoryRoot.Open(".")
		if err != nil {
			return err
		}
		defer func() { returnErr = errors.Join(returnErr, directory.Close()) }()
		if err := ctx.Err(); err != nil {
			return err
		}
		remainingEntries := maxWalkEntries - budget.entries
		entries, overflow, readErr := readDiscoveryDirectoryEntries(directory, remainingEntries)
		if readErr != nil {
			return readErr
		}
		if overflow {
			budget.exhausted = true
			stopped = true
			return nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			walkPath := entry.Name()
			if walkPrefix != "." {
				walkPath = filepath.Join(walkPrefix, entry.Name())
			}
			if err := budget.charge(walkPath); err != nil {
				stopped = true
				return nil
			}
			displayPath := filepath.Join(abs, filepath.FromSlash(walkPath))
			info, statErr := directoryRoot.Lstat(entry.Name())
			if statErr != nil {
				warnings = append(warnings, fmt.Sprintf("failed stat %s: %v", displayPath, statErr))
				continue
			}
			if info.IsDir() {
				if walk.ShouldSkipDirWithNames(entry.Name(), skipDirNames) {
					continue
				}
				if depth+1 > maxWalkDepth {
					warnings = append(warnings, fmt.Sprintf("stopped descending %s after depth %d", displayPath, maxWalkDepth))
					continue
				}
				child, openErr := directoryRoot.OpenRoot(entry.Name())
				if openErr != nil {
					warnings = append(warnings, fmt.Sprintf("failed opening %s: %v", displayPath, openErr))
					continue
				}
				opened, openErr := child.Stat(".")
				if openErr != nil || !opened.IsDir() || !os.SameFile(info, opened) {
					warnings = append(warnings, fmt.Sprintf("directory %s changed while it was pinned", displayPath))
					_ = child.Close()
					continue
				}
				childErr := visitDirectory(child, walkPath, depth+1)
				closeErr := child.Close()
				if childErr != nil || closeErr != nil {
					return errors.Join(childErr, closeErr)
				}
				if stopped {
					return nil
				}
				continue
			}
			walkedFiles++
			if walkedFiles > maxWalkFiles {
				stopped = true
				return nil
			}
			nested := spec
			nested.rel = filepath.ToSlash(filepath.Join(name, filepath.FromSlash(walkPath)))
			artifacts = append(artifacts, artifactFromInfo(root, logicalRoot, nested, displayPath, info, artifactOutcome{}))
		}
		return nil
	}
	err = visitDirectory(candidateRoot, ".", 0)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("failed walking %s: %v", spec.rel, err))
	}
	if stopped || budget.exhausted || walkedFiles > maxWalkFiles {
		warnings = append(warnings, fmt.Sprintf("stopped scanning %s after %d entries or %d path bytes", spec.rel, maxWalkEntries, maxWalkPathBytes))
	}
	return artifacts, warnings
}

func readDiscoveryDirectoryEntries(directory *os.File, maxEntries int) ([]fs.DirEntry, bool, error) {
	if directory == nil || maxEntries < 0 {
		return nil, false, errors.New("bounded discovery directory read requires a file and non-negative limit")
	}
	entries := make([]fs.DirEntry, 0, min(maxEntries, walkReadBatchSize))
	for {
		remainingWithSentinel := maxEntries + 1 - len(entries)
		if remainingWithSentinel <= 0 {
			return entries, true, nil
		}
		batchSize := min(walkReadBatchSize, remainingWithSentinel)
		batch, err := directory.ReadDir(batchSize)
		entries = append(entries, batch...)
		if len(entries) > maxEntries {
			return entries[:maxEntries], true, nil
		}
		if errors.Is(err, io.EOF) {
			return entries, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
}

type artifactOutcome struct {
	Code    string
	Message string
}

func artifactFromInfo(root *os.Root, logicalRoot string, spec candidate, abs string, info fs.FileInfo, outcome artifactOutcome) Artifact {
	mode := spec.mode
	if unsafeOutcome := unsafeArtifactOutcome(spec.rel, info); unsafeOutcome.Code != "" {
		mode, outcome = ReadDenied, unsafeOutcome
	} else if info.Mode()&os.ModeSymlink != 0 {
		mode = ReadCatalog
		outcome = artifactOutcome{Code: "discovery_symlink_cataloged", Message: "Symlink cataloged but not followed."}
	} else if info.IsDir() && spec.mode == ReadImport {
		mode = ReadCatalog
	}
	artifact := Artifact{Provider: spec.provider, Root: logicalRoot, Path: filepath.ToSlash(spec.rel), AbsPath: abs, Kind: spec.kind, ReadMode: mode, CanonicalScope: spec.canonicalScope, Size: info.Size(), Directory: info.IsDir(), Symlink: info.Mode()&os.ModeSymlink != 0, Code: outcome.Code, Message: outcome.Message}
	if !artifact.Directory && info.Mode().IsRegular() && artifact.ReadMode == ReadImport && artifact.Size <= maxImportFileSize {
		body, err := readArtifactRegularFileInRoot(root, spec.rel)
		if err != nil {
			artifact.ReadMode = ReadDenied
			artifact.Code = "discovery_file_changed"
			artifact.Message = "Regular file changed or could not be read safely during discovery."
		} else {
			sum := sha256.Sum256(body)
			artifact.Size = int64(len(body))
			artifact.SHA256 = hex.EncodeToString(sum[:])
		}
	}
	return artifact
}

func readArtifactRegularFileInRoot(root *os.Root, sourceName string) ([]byte, error) {
	if root == nil {
		return nil, errors.New("pinned project root is required")
	}
	name, err := cleanRootName(sourceName)
	if err != nil {
		return nil, err
	}
	return backup.ReadRootRegularFileBounded(root, name, maxImportFileSize)
}

func captureAndOpenRoot(root string) (rootSnapshot, *os.Root, error) {
	logical, err := filepath.Abs(root)
	if err != nil {
		return rootSnapshot{}, nil, err
	}
	logical = filepath.Clean(logical)
	physical, err := safefs.ResolveRootForMutation(logical)
	if err != nil {
		return rootSnapshot{}, nil, err
	}
	snapshot := rootSnapshot{logical: logical, physical: filepath.Clean(physical)}
	info, err := os.Stat(snapshot.physical)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil, nil
	}
	if err != nil {
		return rootSnapshot{}, nil, err
	}
	if !info.IsDir() {
		return rootSnapshot{}, nil, fmt.Errorf("project root is not a directory: %s", logical)
	}
	pinned, err := os.OpenRoot(snapshot.physical)
	if err != nil {
		return rootSnapshot{}, nil, err
	}
	opened, err := pinned.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
		return rootSnapshot{}, nil, errors.Join(errors.New("project root changed while discovery pinned it"), err, pinned.Close())
	}
	snapshot.info = opened
	if err := validatePinnedRoot(snapshot, pinned); err != nil {
		return rootSnapshot{}, nil, errors.Join(err, pinned.Close())
	}
	return snapshot, pinned, nil
}

func validatePinnedRoot(snapshot rootSnapshot, root *os.Root) error {
	if root == nil || snapshot.info == nil {
		return errors.New("selected project root is unavailable")
	}
	physical, err := safefs.ResolveRootForMutation(snapshot.logical)
	if err != nil {
		return fmt.Errorf("selected project root changed during discovery: %w", err)
	}
	if filepath.Clean(physical) != snapshot.physical {
		return fmt.Errorf("selected project root changed during discovery")
	}
	current, err := os.Stat(snapshot.physical)
	if err != nil || !current.IsDir() || !os.SameFile(snapshot.info, current) {
		return errors.Join(errors.New("selected project root changed during discovery"), err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(snapshot.info, opened) {
		return errors.Join(errors.New("selected project root changed during discovery"), err)
	}
	return nil
}

func providerAllowed(opts Options, id provider.ID) bool {
	if len(opts.Providers) > 0 {
		found := false
		for _, allowed := range opts.Providers {
			if allowed == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, excluded := range opts.ExcludeProviders {
		if excluded == id {
			return false
		}
	}
	return true
}

func unsafeArtifactOutcome(rel string, info fs.FileInfo) artifactOutcome {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		lower := strings.ToLower(part)
		switch lower {
		case ".git", "node_modules", ".cache", "cache", ".tmp", "tmp", "sessions", "session", "logs", "log", "shell_snapshots", "backups":
			return artifactOutcome{Code: "discovery_runtime_path", Message: "Unsafe or runtime state path is cataloged but not staged."}
		}
		if lower == ".env" || strings.HasPrefix(lower, ".env.") {
			return artifactOutcome{Code: "discovery_environment_file", Message: "Environment files are not staged."}
		}
		if lower == "auth" || strings.HasPrefix(lower, "auth.") || strings.Contains(lower, "auth-profile") || strings.Contains(lower, "oauth") {
			return artifactOutcome{Code: "discovery_authentication_file", Message: "Authentication files are not staged."}
		}
		for _, needle := range []string{"secret", "credential", "token", "keychain", "cookie"} {
			if strings.Contains(lower, needle) {
				return artifactOutcome{Code: "discovery_sensitive_path", Message: "Potentially sensitive path is not staged."}
			}
		}
		if strings.HasPrefix(lower, "state_") || strings.HasPrefix(lower, "logs_") || strings.HasPrefix(lower, "history") {
			return artifactOutcome{Code: "discovery_runtime_state_file", Message: "Runtime state files are not staged."}
		}
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().IsRegular() {
		return artifactOutcome{}
	}
	return artifactOutcome{Code: "discovery_special_file", Message: "Special filesystem input is cataloged but not staged."}
}

func cleanRootName(name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes selected project: %s", name)
	}
	return clean, nil
}

func sortArtifacts(artifacts []Artifact) {
	sort.Slice(artifacts, func(i, j int) bool {
		left, right := artifacts[i], artifacts[j]
		if left.Root != right.Root {
			return left.Root < right.Root
		}
		if left.Provider != right.Provider {
			return left.Provider < right.Provider
		}
		return left.Path < right.Path
	})
}
