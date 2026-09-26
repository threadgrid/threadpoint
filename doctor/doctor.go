// SPDX-License-Identifier: Apache-2.0

// Package doctor runs read-only diagnostics across the threadpoint subsystems —
// layout, discovery, staged review, pruning, and restoration — and reports the
// workspace's overall health without mutating anything.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/discover"
	"github.com/threadgrid/threadpoint/layout"
	"github.com/threadgrid/threadpoint/provider"
	"github.com/threadgrid/threadpoint/prune"
	"github.com/threadgrid/threadpoint/restore"
	"github.com/threadgrid/threadpoint/walk"
)

// Severity classifies the impact of a doctor finding.
type Severity string

// SeverityError, SeverityWarning, and SeverityInfo order findings by impact.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Options configures a doctor run: the project Root, the user HomeDir and
// backup directory, the provider selection, the clock (Now), and the provider
// Registry the diagnostics run against.
type Options struct {
	Root                      string
	SkipDirNames              []string
	HomeDir                   string
	ThreadpointHome           string
	Providers                 []provider.ID
	ExcludeProviders          []provider.ID
	ProviderSelectionExplicit bool
	BackupDir                 string
	Now                       func() time.Time
	Registry                  provider.Registry
	// StageReader supplies project-only stage observations for an embedding product.
	// A nil reader uses the selected threadpoint stage store.
	StageReader func(context.Context, Options) ([]StageObservation, error)
}

// Finding is one diagnostic issue or informational result.
type Finding struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Root     string   `json:"root,omitempty"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
}

// Summary counts the major diagnostic categories in a report.
type Summary struct {
	ValidationIssues    int    `json:"validationIssues"`
	ValidationWarnings  int    `json:"validationWarnings"`
	DiscoveredArtifacts int    `json:"discoveredArtifacts"`
	ImportableArtifacts int    `json:"importableArtifacts"`
	ManualReviewItems   int    `json:"manualReviewItems"`
	ActiveStages        int    `json:"activeStages"`
	PruneCandidates     int    `json:"pruneCandidates"`
	PruneReady          int    `json:"pruneReady"`
	PruneChanged        int    `json:"pruneChanged"`
	PruneMissing        int    `json:"pruneMissing"`
	BackupRuns          int    `json:"backupRuns"`
	LatestBackupRun     string `json:"latestBackupRun,omitempty"`
	GitWorktrees        int    `json:"gitWorktrees"`
	GitMissingWorktrees int    `json:"gitMissingWorktrees"`
	GitCheckedPaths     int    `json:"gitCheckedPaths"`
	GitDirtyPaths       int    `json:"gitDirtyPaths"`
	GitUntrackedPaths   int    `json:"gitUntrackedPaths"`
	GitConflictedPaths  int    `json:"gitConflictedPaths"`
}

// GitPath describes one scoped Git path with status.
type GitPath struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// GitReport summarizes scoped Git status for one worktree.
type GitReport struct {
	Root        string    `json:"root"`
	InWorktree  bool      `json:"inWorktree"`
	GitRoot     string    `json:"gitRoot,omitempty"`
	Checked     []string  `json:"checked,omitempty"`
	Dirty       []GitPath `json:"dirty,omitempty"`
	Untracked   []GitPath `json:"untracked,omitempty"`
	Conflicted  []GitPath `json:"conflicted,omitempty"`
	CheckFailed string    `json:"checkFailed,omitempty"`
}

// Report is the full doctor diagnostic result.
type Report struct {
	Readiness         string            `json:"readiness"`
	Inventory         InventorySection  `json:"inventory"`
	Validation        ValidationSection `json:"validation"`
	Workflow          WorkflowSection   `json:"workflow"`
	Root              string            `json:"root"`
	OK                bool              `json:"ok"`
	Summary           Summary           `json:"summary"`
	Findings          []Finding         `json:"findings,omitempty"`
	SuggestedCommands []string          `json:"suggestedCommands,omitempty"`
	Git               []GitReport       `json:"git,omitempty"`
}

// Run runs the read-only diagnostics for opts across the threadpoint
// subsystems and returns a Report of findings and an overall summary. It does
// not mutate the workspace.
func Run(ctx context.Context, opts Options) (*Report, error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	builder := reportBuilder{
		report: &Report{Root: normalized.Root, OK: true},
		seen:   map[string]bool{},
		cmds:   map[string]bool{},
		assets: map[string]map[string]bool{},
	}
	builder.report.Workflow.Complete = true
	builder.addInventory(ctx, normalized)
	builder.addDefaultAssets(normalized.Root, normalized.Registry)
	builder.addValidation(normalized)
	builder.addDiscovery(ctx, normalized)
	builder.addStages(ctx, normalized)
	builder.addPruning(ctx, normalized)
	builder.addBackups(normalized)
	builder.addGit(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	builder.finish()
	return builder.report, nil
}

type reportBuilder struct {
	report        *Report
	seen          map[string]bool
	cmds          map[string]bool
	assets        map[string]map[string]bool
	backupRuns    []backup.Run
	backupsLoaded bool
}

func (b *reportBuilder) addValidation(opts Options) {
	validation := layout.Validate(opts.Root)
	b.report.Validation = ValidationSection{Complete: true, Report: validation}
	b.report.Summary.ValidationIssues = len(validation.Issues)
	b.report.Summary.ValidationWarnings = len(validation.Warnings)
	for _, issue := range validation.Issues {
		b.addFinding(SeverityError, "layout_invalid", opts.Root, issue.Path, issue.Message)
	}
	for _, warning := range validation.Warnings {
		b.addFinding(SeverityInfo, "layout_warning", opts.Root, warning.Path, warning.Message)
	}
	if len(validation.Issues) > 0 {
		b.addCommand(fmt.Sprintf("threadpoint init --root %s", shellQuote(opts.Root)))
	}
}

func (b *reportBuilder) addDiscovery(ctx context.Context, opts Options) {
	report, err := discover.Run(ctx, discover.Options{
		Root:                      opts.Root,
		SkipDirNames:              append([]string(nil), opts.SkipDirNames...),
		Providers:                 append([]provider.ID(nil), opts.Providers...),
		ExcludeProviders:          append([]provider.ID(nil), opts.ExcludeProviders...),
		ProviderSelectionExplicit: opts.ProviderSelectionExplicit,
		Registry:                  opts.Registry,
	})
	if err != nil {
		b.addFinding(SeverityError, "discovery_failed", opts.Root, "", fmt.Sprintf("failed discovering provider artifacts: %v", err))
		return
	}
	b.report.Workflow.Sources = report.Artifacts
	b.report.Summary.DiscoveredArtifacts = len(report.Artifacts)
	for _, warning := range report.Warnings {
		b.addFinding(SeverityWarning, "discovery_warning", opts.Root, "", warning)
	}
	for _, artifact := range report.Artifacts {
		b.addAsset(artifact.Root, artifact.Path)
		switch artifact.ReadMode {
		case discover.ReadImport:
			b.report.Summary.ImportableArtifacts++
		case discover.ReadManual:
			b.report.Summary.ManualReviewItems++
		}
	}
	if b.report.Summary.ImportableArtifacts > 0 || b.report.Summary.ManualReviewItems > 0 {
		b.addFinding(SeverityInfo, "provider_artifacts_detected", opts.Root, "", fmt.Sprintf("%d importable and %d manual-review provider artifacts were found", b.report.Summary.ImportableArtifacts, b.report.Summary.ManualReviewItems))
		b.addCommand(fmt.Sprintf("threadpoint stage --root %s --plan", shellQuote(opts.Root)))
	}
}

func (b *reportBuilder) addStages(ctx context.Context, opts Options) {
	reader := opts.StageReader
	if reader == nil {
		reader = inspectProjectStages
	}
	observations, err := reader(ctx, opts)
	if err != nil {
		b.addFinding(SeverityError, "stage_list_failed", opts.Root, "", fmt.Sprintf("failed inspecting active stages: %v", err))
		return
	}
	b.report.Workflow.Stages = observations
	b.report.Summary.ActiveStages = len(observations)
	for _, item := range observations {
		if item.Code != "" {
			severity := SeverityWarning
			if item.SourceState == "unavailable" {
				severity = SeverityError
			}
			b.addFinding(severity, item.Code, opts.Root, item.Source, item.Message)
		}
	}
	if len(observations) > 0 {
		b.addFinding(SeverityInfo, "review_stages_pending", opts.Root, "", fmt.Sprintf("%d staged artifacts are awaiting review and commit", len(observations)))
		b.addCommand(fmt.Sprintf("threadpoint stage list --root %s", shellQuote(opts.Root)))
	}
}

func (b *reportBuilder) addPruning(ctx context.Context, opts Options) {
	plan, err := prune.BuildPlan(ctx, prune.Options{
		Root:            opts.Root,
		HomeDir:         opts.HomeDir,
		ThreadpointHome: opts.ThreadpointHome,
		BackupDir:       opts.BackupDir,
		PlanOnly:        true,
		Now:             opts.Now,
	})
	if err != nil {
		b.addFinding(SeverityError, "prune_plan_failed", opts.Root, "", fmt.Sprintf("failed building prune plan: %v", err))
		return
	}
	b.backupRuns, b.backupsLoaded = plan.BackupRuns(), true
	for _, run := range b.backupRuns {
		if opts.Now().Sub(run.CreatedAt) > 30*24*time.Hour {
			b.addFinding(SeverityInfo, "backup_review_due", opts.Root, "", "backup "+run.RunID+" is older than 30 days; review retention")
		}
	}
	b.report.Workflow.Prune = plan.Candidates
	b.report.Summary.PruneCandidates = len(plan.Candidates)
	for _, warning := range plan.Warnings {
		b.addFinding(SeverityWarning, "prune_warning", opts.Root, "", warning)
	}
	for _, candidate := range plan.Candidates {
		if filepath.Clean(candidate.Root) == filepath.Clean(opts.Root) {
			b.addAsset(candidate.Root, candidate.Path)
		}
		switch candidate.Status {
		case prune.StatusReady:
			b.report.Summary.PruneReady++
		case prune.StatusChanged:
			b.report.Summary.PruneChanged++
		case prune.StatusMissing:
			b.report.Summary.PruneMissing++
		}
	}
	if b.report.Summary.PruneReady > 0 {
		b.addFinding(SeverityInfo, "prune_ready", opts.Root, "", fmt.Sprintf("%d native artifacts can be pruned after review", b.report.Summary.PruneReady))
		b.addCommand(fmt.Sprintf("threadpoint prune --root %s --plan", shellQuote(opts.Root)))
	}
	if b.report.Summary.PruneChanged > 0 {
		b.addFinding(SeverityInfo, "prune_changed", opts.Root, "", fmt.Sprintf("%d native artifacts changed since their reviewed commit and need a new stage", b.report.Summary.PruneChanged))
		b.addCommand(fmt.Sprintf("threadpoint stage --root %s --plan", shellQuote(opts.Root)))
	}
}

func (b *reportBuilder) addBackups(opts Options) {
	runs := b.backupRuns
	var err error
	if !b.backupsLoaded {
		runs, err = restore.ListRuns(restore.Options{
			Root:            opts.Root,
			HomeDir:         opts.HomeDir,
			ThreadpointHome: opts.ThreadpointHome,
			BackupDir:       opts.BackupDir,
		})
	}
	if err != nil {
		b.addFinding(SeverityError, "backup_list_failed", opts.Root, "", fmt.Sprintf("failed listing threadpoint backups: %v", err))
		return
	}
	for _, run := range runs {
		b.report.Workflow.Backups = append(b.report.Workflow.Backups, BackupObservation{ID: run.RunID, Operation: run.Operation, CreatedAt: run.CreatedAt})
	}
	b.report.Summary.BackupRuns = len(runs)
	if len(runs) > 0 {
		b.report.Summary.LatestBackupRun = runs[0].RunID
		b.addCommand(fmt.Sprintf("threadpoint restore list --root %s", shellQuote(opts.Root)))
	}
}

func (b *reportBuilder) addGit(ctx context.Context) {
	roots := sortedAssetRoots(b.assets)
	for _, root := range roots {
		report := checkGit(ctx, root, sortedAssets(b.assets[root]))
		b.report.Git = append(b.report.Git, report)
		b.report.Summary.GitCheckedPaths += len(report.Checked)
		if report.InWorktree {
			b.report.Summary.GitWorktrees++
		} else {
			b.report.Summary.GitMissingWorktrees++
			b.addFinding(SeverityWarning, "git_worktree_missing", root, "", "Threadpoint-managed agent assets should be protected by version control before destructive workflows")
			b.addCommand(fmt.Sprintf("git -C %s init", shellQuote(root)))
		}
		if report.CheckFailed != "" {
			b.addFinding(SeverityWarning, "git_check_failed", root, "", report.CheckFailed)
		}
		b.report.Summary.GitDirtyPaths += len(report.Dirty)
		b.report.Summary.GitUntrackedPaths += len(report.Untracked)
		b.report.Summary.GitConflictedPaths += len(report.Conflicted)
		if len(report.Untracked) > 0 {
			b.addFinding(SeverityWarning, "git_untracked_assets", root, "", fmt.Sprintf("%d threadpoint-managed agent assets are not tracked by Git", len(report.Untracked)))
			b.addCommand(gitPathCommand(root, "add", report.Untracked))
		}
		if len(report.Conflicted) > 0 {
			b.addFinding(SeverityError, "git_conflicted_assets", root, "", "managed agent assets have unresolved Git conflicts")
		}
		if len(report.Dirty) > 0 || len(report.Conflicted) > 0 {
			total := len(report.Dirty) + len(report.Conflicted)
			b.addFinding(SeverityInfo, "git_dirty_assets", root, "", fmt.Sprintf("%d threadpoint-managed agent assets have staged, modified, deleted, or conflicted worktree state", total))
			paths := append([]GitPath{}, report.Dirty...)
			paths = append(paths, report.Conflicted...)
			b.addCommand(gitPathCommand(root, "status --short", paths))
		}
	}
}

func (b *reportBuilder) finish() {
	b.report.Readiness = "healthy"
	b.report.Workflow.Summary = b.report.Summary
	b.report.Workflow.Git = b.report.Git
	for _, finding := range b.report.Findings {
		if finding.Severity == SeverityWarning && b.report.Readiness != "blocked" {
			b.report.Readiness = "degraded"
		}
		if finding.Severity == SeverityError {
			b.report.Readiness = "blocked"
		}
	}
	for _, finding := range b.report.Findings {
		if finding.Severity == SeverityError {
			b.report.OK = false
			break
		}
	}
	sort.SliceStable(b.report.Findings, func(i, j int) bool {
		if b.report.Findings[i].Severity != b.report.Findings[j].Severity {
			return severityRank(b.report.Findings[i].Severity) < severityRank(b.report.Findings[j].Severity)
		}
		if b.report.Findings[i].Code != b.report.Findings[j].Code {
			return b.report.Findings[i].Code < b.report.Findings[j].Code
		}
		return b.report.Findings[i].Path < b.report.Findings[j].Path
	})
	sort.Strings(b.report.SuggestedCommands)
}

func (b *reportBuilder) addDefaultAssets(root string, registry provider.Registry) {
	for _, rel := range registry.DefaultManagedAssetPaths() {
		b.addAsset(root, rel)
	}
	b.addAsset(root, ".agents")
}

// addAsset records a project-root path for the Git hygiene check, but only when
// it belongs to the shared layout threadpoint actually writes (AGENTS.md or
// .agents/*). Detected provider bridges and other native artifacts are cataloged
// through other findings; threadpoint no longer authors them, so the Git check
// must not claim them as threadpoint-managed assets.
func (b *reportBuilder) addAsset(root string, rel string) {
	if !isManagedLayoutPath(rel) {
		return
	}
	b.recordAsset(root, rel)
}

// recordAsset inserts a resolved (root, rel) asset into the Git-check set.
func (b *reportBuilder) recordAsset(root string, rel string) {
	root = strings.TrimSpace(root)
	rel = cleanRel(rel)
	if root == "" || rel == "" {
		return
	}
	if b.assets[root] == nil {
		b.assets[root] = map[string]bool{}
	}
	b.assets[root][rel] = true
}

// isManagedLayoutPath reports whether rel belongs to the shared agent layout
// threadpoint writes and version-controls: the AGENTS.md guide or anything under
// .agents/. Provider bridges (CLAUDE.md, .codex/AGENTS.md, …) and other detected
// native artifacts live outside this layout and are not threadpoint-managed.
func isManagedLayoutPath(rel string) bool {
	rel = cleanRel(rel)
	return rel == "AGENTS.md" || rel == ".agents" || strings.HasPrefix(rel, ".agents/")
}

func (b *reportBuilder) addFinding(severity Severity, code string, root string, path string, message string) {
	if (strings.HasSuffix(code, "_failed") && code != "inventory_failed") || code == "discovery_warning" || code == "prune_warning" || code == "stage_source_unavailable" {
		b.report.Workflow.Complete = false
	}

	key := string(severity) + "\x00" + code + "\x00" + root + "\x00" + path + "\x00" + message
	if b.seen[key] {
		return
	}
	b.seen[key] = true
	b.report.Findings = append(b.report.Findings, Finding{
		Severity: severity,
		Code:     code,
		Root:     root,
		Path:     cleanRel(path),
		Message:  message,
	})
}

func (b *reportBuilder) addCommand(command string) {
	command = strings.TrimSpace(command)
	if command == "" || b.cmds[command] {
		return
	}
	b.cmds[command] = true
	b.report.SuggestedCommands = append(b.report.SuggestedCommands, command)
}

func checkGit(ctx context.Context, root string, paths []string) GitReport {
	report := GitReport{Root: root, Checked: paths}
	if len(paths) == 0 {
		return report
	}
	gitRoot, err := gitOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			report.CheckFailed = "git executable was not found; cannot verify version-control state for managed agent assets"
		}
		return report
	}
	report.InWorktree = true
	report.GitRoot = strings.TrimSpace(gitRoot)
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--"}
	args = append(args, paths...)
	status, err := gitOutput(ctx, root, args...)
	if err != nil {
		report.CheckFailed = fmt.Sprintf("failed checking managed asset Git status: %v", err)
		return report
	}
	for _, entry := range parseStatus(status) {
		path := cleanRel(entry.path)
		if path == "" {
			continue
		}
		gitPath := GitPath{Path: path, Status: entry.status}
		switch {
		case isConflicted(entry.status):
			gitPath.Code = "git_path_conflicted"
			gitPath.Message = "Git path has conflicted worktree state."
			report.Conflicted = append(report.Conflicted, gitPath)
		case entry.status == "??":
			gitPath.Code = "git_path_untracked"
			gitPath.Message = "Git path is untracked."
			report.Untracked = append(report.Untracked, gitPath)
		default:
			gitPath.Code = "git_path_dirty"
			gitPath.Message = "Git path has staged, modified, or deleted worktree state."
			report.Dirty = append(report.Dirty, gitPath)
		}
	}
	sortGitPaths(report.Dirty)
	sortGitPaths(report.Untracked)
	sortGitPaths(report.Conflicted)
	return report
}

type statusEntry struct {
	status string
	path   string
}

func parseStatus(raw string) []statusEntry {
	parts := strings.Split(raw, "\x00")
	entries := []statusEntry{}
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if len(part) < 4 {
			continue
		}
		status := part[:2]
		path := part[3:]
		entries = append(entries, statusEntry{status: status, path: path})
		if strings.ContainsAny(status, "RC") && i+1 < len(parts) {
			i++
		}
	}
	return entries
}

func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	// #nosec G204 -- executable is fixed to git; root and args are passed as argv, not through a shell.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(gitSafeEnvironment(),
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
	body, err := cmd.Output()
	if err != nil {
		text := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			text = strings.TrimSpace(string(exitErr.Stderr))
		}
		if text == "" {
			text = strings.TrimSpace(string(body))
		}
		if text != "" {
			return "", fmt.Errorf("%w: %s", err, text)
		}
		return "", err
	}
	return string(body), nil
}

func gitSafeEnvironment() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && (key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_SYSTEM" || key == "GIT_CONFIG_NOSYSTEM") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func isConflicted(status string) bool {
	if strings.Contains(status, "U") {
		return true
	}
	switch status {
	case "AA", "DD":
		return true
	default:
		return false
	}
}

func normalizeOptions(opts Options) (Options, error) {
	if opts.Root == "" {
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
	if opts.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return opts, err
		}
		opts.HomeDir = home
	}
	if opts.HomeDir != "" {
		home, err := filepath.Abs(opts.HomeDir)
		if err != nil {
			return opts, err
		}
		opts.HomeDir = filepath.Clean(home)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := validateSelectedProviders(opts.Registry, opts.Providers); err != nil {
		return opts, err
	}
	if err := validateSelectedProviders(opts.Registry, opts.ExcludeProviders); err != nil {
		return opts, err
	}
	sort.Slice(opts.Providers, func(i, j int) bool { return opts.Providers[i] < opts.Providers[j] })
	sort.Slice(opts.ExcludeProviders, func(i, j int) bool { return opts.ExcludeProviders[i] < opts.ExcludeProviders[j] })
	return opts, nil
}

func validateSelectedProviders(registry provider.Registry, providers []provider.ID) error {
	for _, providerID := range providers {
		if _, ok := registry.Lookup(providerID); !ok {
			return fmt.Errorf("unknown provider %q", providerID)
		}
	}
	return nil
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

func canonicalGitAsset(root string, rel string, homeDir string) (string, string) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return root, rel
	}
	rootAbs = filepath.Clean(rootAbs)
	targetAbs := filepath.Join(rootAbs, filepath.FromSlash(rel))
	if strings.TrimSpace(homeDir) == "" {
		return rootAbs, rel
	}
	homeAbs, err := filepath.Abs(homeDir)
	if err != nil {
		return rootAbs, rel
	}
	agentsRoot := filepath.Join(filepath.Clean(homeAbs), ".agents")
	if insidePath(targetAbs, agentsRoot) {
		mappedRel, err := filepath.Rel(agentsRoot, targetAbs)
		if err != nil {
			return rootAbs, rel
		}
		return agentsRoot, filepath.ToSlash(mappedRel)
	}
	return rootAbs, rel
}

func insidePath(path string, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sortedAssetRoots(assets map[string]map[string]bool) []string {
	roots := make([]string, 0, len(assets))
	for root := range assets {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

func sortedAssets(paths map[string]bool) []string {
	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func sortGitPaths(paths []GitPath) {
	sort.Slice(paths, func(i, j int) bool {
		if paths[i].Path != paths[j].Path {
			return paths[i].Path < paths[j].Path
		}
		return paths[i].Status < paths[j].Status
	})
}

func severityRank(severity Severity) int {
	switch severity {
	case SeverityError:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}

func gitPathCommand(root string, subcommand string, paths []GitPath) string {
	seen := map[string]bool{}
	quoted := []string{}
	for _, path := range paths {
		if path.Path == "" || seen[path.Path] {
			continue
		}
		seen[path.Path] = true
		quoted = append(quoted, shellQuote(path.Path))
	}
	sort.Strings(quoted)
	if len(quoted) == 0 {
		return ""
	}
	return fmt.Sprintf("git -C %s %s -- %s", shellQuote(root), subcommand, strings.Join(quoted, " "))
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_@%+=:,./-", r) {
			continue
		}
		return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
	}
	return value
}

// FormatText renders a Report as human-readable text for the CLI.
func FormatText(report *Report) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Threadpoint status for %s\n", report.Root)
	if report.OK {
		fmt.Fprintln(&out, "Status: OK")
	} else {
		fmt.Fprintln(&out, "Status: errors found")
	}
	fmt.Fprintln(&out)
	writeFindings(&out, "Errors", report.Findings, SeverityError)
	writeFindings(&out, "Warnings", report.Findings, SeverityWarning)
	if len(report.SuggestedCommands) == 0 {
		fmt.Fprintln(&out, "Suggested next commands: none")
	} else {
		fmt.Fprintln(&out, "Suggested next commands:")
		for _, command := range report.SuggestedCommands {
			fmt.Fprintf(&out, "  %s\n", command)
		}
	}
	fmt.Fprintln(&out)
	fmt.Fprint(&out, formatInventory(report.Inventory))
	fmt.Fprintf(&out, "Validation: valid %v, complete %v\n", report.Validation.OK, report.Validation.Complete)
	fmt.Fprintf(&out, "Workflow: complete %v\n", report.Workflow.Complete)
	for _, item := range report.Workflow.Stages {
		fmt.Fprintf(&out, "  %s %s: source %s, edited %v\n", item.Scope, item.ID, item.SourceState, item.Edited)
	}
	fmt.Fprintln(&out, "State summary:")
	fmt.Fprintf(&out, "  validation: %d errors, %d warnings\n", report.Summary.ValidationIssues, report.Summary.ValidationWarnings)
	fmt.Fprintf(&out, "  discovery: %d artifacts (%d importable, %d manual-review)\n", report.Summary.DiscoveredArtifacts, report.Summary.ImportableArtifacts, report.Summary.ManualReviewItems)
	fmt.Fprintf(&out, "  review stages: %d active\n", report.Summary.ActiveStages)
	fmt.Fprintf(&out, "  prune: %d candidates (%d ready, %d changed, %d missing)\n", report.Summary.PruneCandidates, report.Summary.PruneReady, report.Summary.PruneChanged, report.Summary.PruneMissing)
	if report.Summary.LatestBackupRun == "" {
		fmt.Fprintf(&out, "  backups: %d runs\n", report.Summary.BackupRuns)
	} else {
		fmt.Fprintf(&out, "  backups: %d runs, latest %s\n", report.Summary.BackupRuns, report.Summary.LatestBackupRun)
	}
	fmt.Fprintf(&out, "  git assets: %d checked, %d untracked, %d dirty, %d conflicted\n", report.Summary.GitCheckedPaths, report.Summary.GitUntrackedPaths, report.Summary.GitDirtyPaths, report.Summary.GitConflictedPaths)
	return out.String()
}

func writeFindings(out *strings.Builder, title string, findings []Finding, severity Severity) {
	wrote := false
	for _, finding := range findings {
		if finding.Severity != severity {
			continue
		}
		if !wrote {
			fmt.Fprintf(out, "%s:\n", title)
			wrote = true
		}
		location := finding.Path
		if location == "" {
			location = finding.Root
		}
		if location == "" {
			fmt.Fprintf(out, "  - [%s] %s\n", finding.Code, finding.Message)
		} else {
			fmt.Fprintf(out, "  - [%s] %s: %s\n", finding.Code, location, finding.Message)
		}
	}
	if !wrote {
		fmt.Fprintf(out, "%s: none\n", title)
	}
}
