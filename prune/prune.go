// SPDX-License-Identifier: Apache-2.0

// Package prune removes native agent artifacts whose latest committed stage
// snapshot still matches their current content. BuildPlan returns a pure plan
// value; Apply performs the effects.
package prune

import (
	"context"
	"crypto/rand"
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
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/clierror"
	"github.com/threadgrid/threadpoint/internal/abspath"
	"github.com/threadgrid/threadpoint/safefs"
	"github.com/threadgrid/threadpoint/safepath"
	"github.com/threadgrid/threadpoint/walk"
)

const pruneConfirmation = "remove replaced agent artifacts"

const maxPruneArtifactSize = int64(walk.DefaultMaxFileSize)

// Status describes whether a native artifact can be pruned.
type Status string

// StatusReady can be pruned safely; StatusChanged and StatusMissing describe
// sources that no longer match the committed stage snapshot.
const (
	StatusReady   Status = "ready"
	StatusChanged Status = "changed"
	StatusMissing Status = "missing"
)

// Options configures a pruning run: the project Root, the user HomeDir and
// backup directory, the confirmation, and the clock (Now).
type Options struct {
	Root            string
	HomeDir         string
	ThreadpointHome string
	BackupDir       string
	PlanOnly        bool
	Apply           bool
	Yes             bool
	Confirm         string
	Now             func() time.Time
	// ProductRoot is a borrowed, caller-retained ThreadpointHome capability.
	// When set, every backup read and write resolves beneath this exact root.
	ProductRoot *os.Root
	// ProjectRoot is a borrowed, caller-retained selected-project capability.
	// Apply fills it from its lock set before planning or mutating.
	ProjectRoot *os.Root
}

// Candidate is one native artifact considered for pruning.
type Candidate struct {
	Path          string `json:"path"`
	Root          string `json:"root"`
	BackupRun     string `json:"backupRun"`
	BackupPath    string `json:"backupPath"`
	SHA256        string `json:"sha256,omitempty"`
	CurrentSHA256 string `json:"currentSha256,omitempty"`
	Status        Status `json:"status"`
	Code          string `json:"code,omitempty"`
	Message       string `json:"message,omitempty"`
	currentInfo   fs.FileInfo
	currentPin    *os.File
}

// Plan is the dry-run result of pruning: the native artifacts eligible for
// removal plus any warnings. It is a pure value with no effects.
type Plan struct {
	backupRuns []backup.Run
	Root       string      `json:"root"`
	Candidates []Candidate `json:"candidates"`
	Warnings   []string    `json:"warnings,omitempty"`
	root       pruneRootSnapshot
}

// BackupRuns returns the backup metadata inspected while building this plan.
// Callers must treat the returned snapshot as read-only.
func (plan *Plan) BackupRuns() []backup.Run { return append([]backup.Run(nil), plan.backupRuns...) }

// Resolution reports the pruning outcome for one candidate.
type Resolution struct {
	Path         string   `json:"path"`
	Status       Status   `json:"status"`
	Backup       string   `json:"backup,omitempty"`
	Recovery     string   `json:"recovery,omitempty"`
	Removed      bool     `json:"removed,omitempty"`
	Skipped      bool     `json:"skipped,omitempty"`
	Messages     []string `json:"messages,omitempty"`
	recoveryInfo fs.FileInfo
	recoveryPin  *os.File
}

type pruneRootSnapshot struct {
	logical  string
	physical string
	info     fs.FileInfo
}

type applyHooks struct {
	afterPlan            func(*Plan, *os.Root) error
	probeNoReplace       func(*os.Root) error
	afterBackup          func(Candidate, *os.Root, string)
	afterRecoveryJournal func(Candidate, string) error
	beforeRecoveryRename func(Candidate, *os.Root, string, *os.Root, string)
	afterSourceBoundary  func(Candidate, *os.Root, string, *os.Root, string)
	afterDetach          func(Candidate, *os.Root, string, *os.Root, string)
	beforeRollback       func(Candidate, *os.Root, string, *os.Root, string)
}

type pruneTargetParent struct {
	project  *os.Root
	root     *os.Root
	path     string
	base     string
	info     fs.FileInfo
	snapshot pruneRootSnapshot
}

type pruneRecovery struct {
	containerName string
	itemName      string
	itemRel       string
	path          string
	root          *os.Root
	containerInfo fs.FileInfo
	itemInfo      fs.FileInfo
}

// Report is the result of applying a pruning Plan: the resolution for each
// candidate.
type Report struct {
	Root        string       `json:"root"`
	Applied     bool         `json:"applied"`
	Plan        *Plan        `json:"plan"`
	Resolutions []Resolution `json:"resolutions,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
}

// BuildPlan returns a pure pruning plan for opts — the verified-removable native
// artifacts and any warnings — without performing any effects. Apply executes it.
func BuildPlan(ctx context.Context, opts Options) (*Plan, error) {
	plan, projectRoot, err := buildPlanWithPinnedRoot(ctx, opts)
	if projectRoot != nil {
		err = errors.Join(err, projectRoot.Close())
	}
	closePrunePlanPins(plan)
	return plan, err
}

// buildPlanWithPinnedRoot returns the project-root descriptor used to build the
// plan. Apply keeps that descriptor open across confirmation, filesystem
// preflight, lock acquisition, mutation, and final recovery reporting. A stale
// FileInfo alone cannot distinguish a reviewed directory from a later inode
// that reuses the same device/inode tuple after the reviewed root is unlinked.
func buildPlanWithPinnedRoot(ctx context.Context, opts Options) (resultPlan *Plan, resultRoot *os.Root, resultErr error) {
	return buildPlanWithPinnedRootHooks(ctx, opts)
}

func buildPlanWithPinnedRootHooks(ctx context.Context, opts Options) (resultPlan *Plan, resultRoot *os.Root, resultErr error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var rootSnapshot pruneRootSnapshot
	var projectRoot *os.Root
	if normalized.ProjectRoot != nil {
		rootSnapshot, projectRoot, err = capturePruneRootFromRetained(normalized.Root, normalized.ProjectRoot)
	} else {
		rootSnapshot, projectRoot, err = captureAndOpenPruneRoot(normalized.Root)
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		closePrunePlanPins(resultPlan)
		resultErr = errors.Join(resultErr, projectRoot.Close())
		resultRoot = nil
	}()

	plan := &Plan{Root: normalized.Root, root: rootSnapshot}
	var runs []backup.Run
	if normalized.ProductRoot != nil {
		var homePath string
		homePath, err = productHomePath(normalized)
		if err != nil {
			return nil, nil, err
		}
		runs, err = backup.ListRunsFromRoot(normalized.Root, normalized.ProductRoot, homePath, normalized.BackupDir)
	} else {
		runs, err = backup.ListRunsWithOverride(normalized.Root, normalized.HomeDir, normalized.ThreadpointHome, normalized.BackupDir)
	}
	if err != nil {
		return nil, nil, err
	}
	plan.backupRuns = runs
	seen := map[string]bool{}
	for _, run := range runs {
		if run.Operation != backup.CommittedCommitOperation {
			continue
		}
		if err := validateCommitTransactionForPrune(projectRoot, run); err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("ignored commit backup %s because its canonical transaction is not current: %v", run.RunID, err))
			continue
		}
		for _, entry := range run.Entries {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if entry.Role != "stage-source" || entry.Directory || entry.Symlink {
				continue
			}
			seenKey := filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry.Path)))
			if seen[seenKey] {
				continue
			}
			seen[seenKey] = true
			candidate, err := buildCandidate(projectRoot, rootSnapshot, normalized.Root, run.RunID, entry)
			if err != nil {
				return nil, nil, err
			}
			plan.Candidates = append(plan.Candidates, candidate)
		}
	}
	sort.Slice(plan.Candidates, func(i, j int) bool {
		return plan.Candidates[i].Path < plan.Candidates[j].Path
	})
	if err := validatePruneRootSnapshot(rootSnapshot); err != nil {
		return nil, nil, fmt.Errorf("project root changed while the prune plan was built: %w", err)
	}
	return plan, projectRoot, nil
}

func validateCommitTransactionForPrune(projectRoot *os.Root, run backup.Run) error {
	transaction := run.CommitTransaction
	if projectRoot == nil || transaction == nil || transaction.State != backup.CommitTransactionCommitted || strings.TrimSpace(transaction.StageID) == "" {
		return errors.New("commit transaction is not finalized")
	}
	if !validPruneSHA256(transaction.SourceSHA256) || !validPruneSHA256(transaction.TargetSHA256) {
		return errors.New("commit transaction digest is invalid")
	}
	if transaction.TargetSize <= 0 {
		return errors.New("commit transaction is missing its canonical generation length")
	}
	foundSource := false
	for _, entry := range run.Entries {
		if entry.Role != "stage-source" {
			continue
		}
		if foundSource || entry.Path != transaction.SourcePath || entry.SHA256 != transaction.SourceSHA256 || entry.Absent || entry.Directory || entry.Symlink || !entry.ModeKnown {
			return errors.New("commit transaction contains an unbound staged source")
		}
		foundSource = true
	}
	if !foundSource {
		return errors.New("commit transaction source does not match its backup entry")
	}
	priorCount := 0
	for _, entry := range run.Entries {
		if entry.Role != "canonical-prior" {
			continue
		}
		priorCount++
		if entry.Path != transaction.TargetPath {
			return errors.New("commit transaction contains an unbound prior canonical generation")
		}
		if entry.Absent {
			if entry.BackupPath != "" || entry.SHA256 != "" || entry.Size != 0 || entry.Directory || entry.Symlink {
				return errors.New("commit transaction prior-absent marker is malformed")
			}
			continue
		}
		if entry.Directory || entry.Symlink || !entry.ModeKnown || entry.BackupPath == "" || entry.Size < 0 || !validPruneSHA256(entry.SHA256) {
			return errors.New("commit transaction prior canonical entry is malformed")
		}
	}
	if priorCount != 1 {
		return errors.New("commit transaction must contain exactly one prior canonical generation")
	}
	target, err := safepath.Join(".", transaction.TargetPath)
	if err != nil {
		return err
	}
	body, err := backup.ReadRootRegularFileBounded(projectRoot, target, maxPruneArtifactSize)
	if err != nil {
		return err
	}
	if transaction.TargetSize > int64(len(body)) {
		return errors.New("canonical target is shorter than the committed generation")
	}
	generation := body[:transaction.TargetSize]
	if pruneSHA256(generation) != transaction.TargetSHA256 {
		return errors.New("canonical target no longer contains the committed generation")
	}
	if transaction.TargetSize < int64(len(body)) && body[transaction.TargetSize] != '\n' {
		return errors.New("canonical target does not extend the committed generation additively")
	}
	return nil
}

func validPruneSHA256(raw string) bool {
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == sha256.Size
}

// Apply builds the pruning plan for opts and removes the verified artifacts
// after backing them up, and returns a Report.
func Apply(ctx context.Context, opts Options) (*Report, error) {
	return applyWithHooks(ctx, opts, applyHooks{})
}

func applyWithHooks(ctx context.Context, opts Options, hooks applyHooks) (resultReport *Report, resultErr error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	locks, err := acquireApplyLocks(normalized)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, locks.Release()) }()
	productRoot := normalized.ProductRoot
	if productRoot == nil {
		productRoot, err = locks.BorrowProductRoot()
		if err != nil {
			return nil, err
		}
		normalized.ProductRoot = productRoot
	}
	if normalized.ProjectRoot == nil {
		normalized.ProjectRoot, err = locks.BorrowProjectRoot(normalized.Root)
		if err != nil {
			return nil, err
		}
	}
	plan, projectRoot, err := buildPlanWithPinnedRoot(ctx, normalized)
	if err != nil {
		return nil, err
	}
	defer projectRoot.Close()
	defer closePrunePlanPins(plan)
	if hooks.afterPlan != nil {
		if err := hooks.afterPlan(plan, projectRoot); err != nil {
			return nil, err
		}
	}
	if !confirmed(normalized) {
		return nil, clierror.Refused(fmt.Errorf("prune --apply requires --confirm %q or --yes", pruneConfirmation))
	}
	if err := verifyPruneLockRoot(locks, normalized.Root, projectRoot); err != nil {
		return nil, err
	}
	for _, candidate := range plan.Candidates {
		if candidate.Status == StatusChanged {
			return nil, clierror.Refused(fmt.Errorf("cannot prune %s because it changed since its reviewed commit; stage and commit it again before pruning", candidate.Path))
		}
	}
	if err := preflightPruneCandidates(ctx, plan, projectRoot); err != nil {
		return nil, err
	}
	if err := preflightPruneNoReplaceInRoot(plan, hooks, projectRoot); err != nil {
		return nil, err
	}
	storeOptions := backup.StoreOptions{
		ProjectRoot:     normalized.Root,
		HomeDir:         normalized.HomeDir,
		ThreadpointHome: normalized.ThreadpointHome,
		BackupDir:       normalized.BackupDir,
		Operation:       "prune",
		Now:             normalized.Now,
	}
	homePath, err := productHomePath(normalized)
	if err != nil {
		return nil, err
	}
	store, err := backup.NewStoreFromRoot(productRoot, homePath, storeOptions)
	if err != nil {
		return nil, err
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	report := &Report{Root: normalized.Root, Applied: true, Plan: plan}
	defer func() {
		if err := finalizePruneRecoveryClaims(projectRoot, plan.root, store, report); err != nil {
			if resultReport == nil {
				resultReport = report
			}
			resultErr = errors.Join(resultErr, fmt.Errorf("finalizing prune recovery claims: %w", err))
		}
		if err := closePruneRecoveryPins(report); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("closing prune recovery pins: %w", err))
		}
	}()
	for _, candidate := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := verifyPruneLockRoot(locks, normalized.Root, projectRoot); err != nil {
			return report, err
		}
		resolution := Resolution{Path: candidate.Path, Status: candidate.Status}
		if candidate.Status == StatusMissing {
			resolution.Skipped = true
			resolution.Messages = append(resolution.Messages, "native artifact is already absent")
			report.Resolutions = append(report.Resolutions, resolution)
			continue
		}
		resolution, err = applyReadyCandidate(projectRoot, plan.root, store, candidate, hooks)
		report.Resolutions = append(report.Resolutions, resolution)
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

func productHomePath(opts Options) (string, error) {
	return safefs.ResolveThreadpointHomeWithOverride(opts.HomeDir, opts.ThreadpointHome)
}

func acquireApplyLocks(opts Options) (*safefs.LockSet, error) {
	threadpointHome, err := safefs.ResolveThreadpointHomeWithOverride(opts.HomeDir, opts.ThreadpointHome)
	if err != nil {
		return nil, err
	}
	backupRoot, err := backup.ResolveBackupRootWithOverride(opts.HomeDir, opts.ThreadpointHome, opts.BackupDir)
	if err != nil {
		return nil, err
	}
	return safefs.AcquireLocks(threadpointHome, []string{opts.Root, backupRoot}, "prune")
}

func normalizeOptions(opts Options) (Options, error) {
	if opts.Root == "" {
		opts.Root = "."
	}
	root, err := abspath.Abs(opts.Root)
	if err != nil {
		return opts, err
	}
	opts.Root = filepath.Clean(root)
	resolvedHome, err := safefs.ResolveUserHome(opts.HomeDir)
	if err != nil {
		return opts, err
	}
	opts.HomeDir = resolvedHome
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return opts, nil
}

// captureAndOpenPruneRoot establishes the root identity from an already-open
// descriptor and then binds the selected logical and physical pathnames back to
// that descriptor. Keeping the descriptor open prevents device/inode reuse from
// making a later replacement directory compare equal to the selected root.
func captureAndOpenPruneRoot(logicalRoot string) (pruneRootSnapshot, *os.Root, error) {
	physical, err := safefs.ResolveRootForMutation(logicalRoot)
	if err != nil {
		return pruneRootSnapshot{}, nil, err
	}
	root, err := os.OpenRoot(physical)
	if err != nil {
		return pruneRootSnapshot{}, nil, err
	}
	opened, err := root.Stat(".")
	if err != nil {
		return pruneRootSnapshot{}, nil, errors.Join(err, root.Close())
	}
	snapshot := pruneRootSnapshot{
		logical:  filepath.Clean(logicalRoot),
		physical: filepath.Clean(physical),
		info:     opened,
	}
	if !opened.IsDir() || opened.Mode()&os.ModeSymlink != 0 {
		return pruneRootSnapshot{}, nil, errors.Join(fmt.Errorf("prune root is not a real directory: %s", logicalRoot), root.Close())
	}
	if err := validatePruneRootSnapshot(snapshot); err != nil {
		return pruneRootSnapshot{}, nil, errors.Join(err, root.Close())
	}
	return snapshot, root, nil
}

func capturePruneRootFromRetained(logicalRoot string, retained *os.Root) (pruneRootSnapshot, *os.Root, error) {
	if retained == nil {
		return pruneRootSnapshot{}, nil, errors.New("retained prune project root is required")
	}
	physical, err := safefs.ResolveRootForMutation(logicalRoot)
	if err != nil {
		return pruneRootSnapshot{}, nil, err
	}
	opened, err := retained.Stat(".")
	if err != nil || !opened.IsDir() {
		return pruneRootSnapshot{}, nil, errors.Join(errors.New("retained prune project root is not a directory"), err)
	}
	current, err := os.Lstat(physical)
	if err != nil || !current.IsDir() || !os.SameFile(opened, current) {
		return pruneRootSnapshot{}, nil, errors.Join(errors.New("retained prune project root does not match the selected generation"), err)
	}
	duplicate, err := retained.OpenRoot(".")
	if err != nil {
		return pruneRootSnapshot{}, nil, err
	}
	duplicateInfo, err := duplicate.Stat(".")
	if err != nil || !os.SameFile(opened, duplicateInfo) {
		return pruneRootSnapshot{}, nil, errors.Join(errors.New("retained prune project root changed while duplicated"), err, duplicate.Close())
	}
	snapshot := pruneRootSnapshot{logical: filepath.Clean(logicalRoot), physical: filepath.Clean(physical), info: duplicateInfo}
	if err := validatePruneRootSnapshot(snapshot); err != nil {
		return pruneRootSnapshot{}, nil, errors.Join(err, duplicate.Close())
	}
	return snapshot, duplicate, nil
}

func verifyPruneLockRoot(locks *safefs.LockSet, logicalRoot string, projectRoot *os.Root) error {
	if locks == nil || projectRoot == nil {
		return errors.New("prune lock and project root are required")
	}
	locked, err := locks.BorrowProjectRoot(logicalRoot)
	if err != nil {
		return err
	}
	lockedInfo, lockedErr := locked.Stat(".")
	projectInfo, projectErr := projectRoot.Stat(".")
	if lockedErr != nil || projectErr != nil || !os.SameFile(lockedInfo, projectInfo) {
		return errors.Join(errors.New("prune plan is not bound to the locked project generation"), lockedErr, projectErr)
	}
	return nil
}

func validatePruneRootSnapshot(snapshot pruneRootSnapshot) error {
	physical, err := safefs.ResolveRootForMutation(snapshot.logical)
	if err != nil {
		return err
	}
	if filepath.Clean(physical) != snapshot.physical {
		return fmt.Errorf("prune root %s now resolves to %s instead of %s", snapshot.logical, physical, snapshot.physical)
	}
	current, err := os.Lstat(snapshot.physical)
	if err != nil {
		return err
	}
	if snapshot.info == nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(snapshot.info, current) {
		return fmt.Errorf("prune root changed since it was selected: %s", snapshot.logical)
	}
	return nil
}

func candidateRootName(snapshot pruneRootSnapshot, candidate Candidate) (string, error) {
	target, err := safepath.Join(snapshot.physical, candidate.Path)
	if err != nil {
		return "", err
	}
	name, err := filepath.Rel(snapshot.physical, target)
	if err != nil {
		return "", err
	}
	name = filepath.Clean(name)
	if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid prune target path: %s", candidate.Path)
	}
	return name, nil
}

func openPruneTargetParent(project *os.Root, snapshot pruneRootSnapshot, candidate Candidate) (*pruneTargetParent, error) {
	if project == nil {
		return nil, errors.New("prune project root is required")
	}
	if err := validatePruneRootSnapshot(snapshot); err != nil {
		return nil, err
	}
	name, err := candidateRootName(snapshot, candidate)
	if err != nil {
		return nil, err
	}
	parentPath := filepath.Dir(name)
	if err := safefs.RejectRootSymlinkAncestors(project, parentPath); err != nil {
		return nil, err
	}
	root, err := project.OpenRoot(parentPath)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !opened.IsDir() || opened.Mode()&os.ModeSymlink != 0 {
		_ = root.Close()
		return nil, fmt.Errorf("prune target parent is not a real directory: %s", filepath.ToSlash(parentPath))
	}
	// Re-check every pathname component while the opened directory is pinned.
	// Opening first means an unlinked predecessor cannot be freed and recycled
	// into a different directory that os.SameFile would then accept.
	if err := safefs.RejectRootSymlinkAncestors(project, parentPath); err != nil {
		_ = root.Close()
		return nil, err
	}
	var current fs.FileInfo
	if parentPath == "." {
		current, err = project.Stat(".")
	} else {
		current, err = project.Lstat(parentPath)
	}
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		_ = root.Close()
		return nil, errors.Join(fmt.Errorf("prune target parent changed while it was opened: %s", filepath.ToSlash(parentPath)), err)
	}
	parent := &pruneTargetParent{
		project:  project,
		root:     root,
		path:     parentPath,
		base:     filepath.Base(name),
		info:     opened,
		snapshot: snapshot,
	}
	if err := parent.verify(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return parent, nil
}

func (parent *pruneTargetParent) verify() error {
	if parent == nil || parent.project == nil || parent.root == nil {
		return errors.New("prune target parent is not open")
	}
	if err := validatePruneRootSnapshot(parent.snapshot); err != nil {
		return err
	}
	projectInfo, err := parent.project.Stat(".")
	if err != nil || parent.snapshot.info == nil || !os.SameFile(parent.snapshot.info, projectInfo) {
		return errors.Join(errors.New("pinned prune project root changed"), err)
	}
	if err := safefs.RejectRootSymlinkAncestors(parent.project, parent.path); err != nil {
		return err
	}
	var current fs.FileInfo
	if parent.path == "." {
		current, err = parent.project.Stat(".")
	} else {
		current, err = parent.project.Lstat(parent.path)
	}
	if err != nil {
		return err
	}
	opened, err := parent.root.Stat(".")
	if err != nil {
		return err
	}
	if parent.info == nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(parent.info, current) || !os.SameFile(parent.info, opened) {
		return fmt.Errorf("prune target parent changed during mutation: %s", filepath.ToSlash(parent.path))
	}
	return nil
}

func readPruneRegular(root *os.Root, name string) ([]byte, fs.FileInfo, error) {
	body, info, pinned, err := readPinnedPruneRegular(root, name)
	if err != nil {
		return nil, nil, err
	}
	if err := pinned.Close(); err != nil {
		return nil, nil, err
	}
	return body, info, nil
}

// readPinnedPruneRegular returns a stable bounded read together with the open
// descriptor that established its identity. Callers that cross an observable
// race boundary keep the descriptor open so an unlink/recreate cannot recycle
// the reviewed inode and make a later os.SameFile comparison accept a different
// file.
func readPinnedPruneRegular(root *os.Root, name string) ([]byte, fs.FileInfo, *os.File, error) {
	if root == nil {
		return nil, nil, nil, errors.New("prune file root is required")
	}
	if err := safefs.RejectRootSymlinkAncestors(root, name); err != nil {
		return nil, nil, nil, err
	}
	expected, err := root.Lstat(name)
	if err != nil {
		return nil, nil, nil, err
	}
	if !expected.Mode().IsRegular() {
		return nil, nil, nil, fmt.Errorf("%s is not a regular file", filepath.ToSlash(name))
	}
	if expected.Size() > maxPruneArtifactSize {
		return nil, nil, nil, fmt.Errorf("%s is larger than %d bytes", filepath.ToSlash(name), maxPruneArtifactSize)
	}
	file, err := openPinnedPruneRegularFile(root, name)
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(cause error) ([]byte, fs.FileInfo, *os.File, error) {
		return nil, nil, nil, errors.Join(cause, file.Close())
	}
	opened, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return fail(fmt.Errorf("%s changed before it could be read", filepath.ToSlash(name)))
	}
	if opened.Size() > maxPruneArtifactSize {
		return fail(fmt.Errorf("%s is larger than %d bytes", filepath.ToSlash(name), maxPruneArtifactSize))
	}
	body, err := io.ReadAll(io.LimitReader(file, maxPruneArtifactSize+1))
	if err != nil {
		return fail(err)
	}
	if int64(len(body)) > maxPruneArtifactSize {
		return fail(fmt.Errorf("%s is larger than %d bytes", filepath.ToSlash(name), maxPruneArtifactSize))
	}
	after, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	current, err := root.Lstat(name)
	if err != nil {
		return fail(err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(opened, after) || !os.SameFile(opened, current) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || int64(len(body)) != after.Size() {
		return fail(fmt.Errorf("%s changed while it was read", filepath.ToSlash(name)))
	}
	return body, after, file, nil
}

// verifyPinnedPruneRegular re-reads the already-pinned source descriptor and
// binds its current pathname to that same descriptor. Once pruning establishes
// this pin, later validation never reopens the reviewed source by pathname.
func verifyPinnedPruneRegular(root *os.Root, name string, file *os.File, expected fs.FileInfo, expectedSHA256 string) ([]byte, fs.FileInfo, error) {
	if root == nil || file == nil || expected == nil {
		return nil, nil, errors.New("pinned prune source identity is required")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	before, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || !os.SameFile(expected, before) || before.Size() > maxPruneArtifactSize {
		return nil, nil, errors.New("pinned prune source changed identity")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxPruneArtifactSize+1))
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	current, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > maxPruneArtifactSize || !current.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(body)) != after.Size() || pruneSHA256(body) != expectedSHA256 {
		return nil, nil, errors.New("pinned prune source changed at the atomic detach boundary")
	}
	return body, after, nil
}

func pruneSHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func samePlannedPruneTarget(candidate Candidate, body []byte, info fs.FileInfo) bool {
	return candidate.currentInfo != nil && info != nil && os.SameFile(candidate.currentInfo, info) && pruneSHA256(body) == candidate.SHA256
}

func preflightPruneNoReplaceInRoot(plan *Plan, hooks applyHooks, projectRoot *os.Root) error {
	if plan == nil {
		return errors.New("prune plan is required")
	}
	if projectRoot == nil {
		return errors.New("pinned prune project root is required")
	}
	if err := validatePruneRootSnapshot(plan.root); err != nil {
		return err
	}
	opened, err := projectRoot.Stat(".")
	if err != nil || plan.root.info == nil || !os.SameFile(plan.root.info, opened) {
		return errors.Join(errors.New("pinned prune project root changed before atomic no-replace preflight"), err)
	}
	parents := map[string]Candidate{}
	for _, candidate := range plan.Candidates {
		if candidate.Status != StatusReady {
			continue
		}
		name, err := candidateRootName(plan.root, candidate)
		if err != nil {
			return err
		}
		parentPath := filepath.Dir(name)
		if _, exists := parents[parentPath]; !exists {
			parents[parentPath] = candidate
		}
	}
	ordered := make([]string, 0, len(parents))
	for parentPath := range parents {
		ordered = append(ordered, parentPath)
	}
	sort.Strings(ordered)
	probe := hooks.probeNoReplace
	if probe == nil {
		probe = safefs.ProbeRenameRootNoReplace
	}
	for _, parentPath := range ordered {
		parent, err := openPruneTargetParent(projectRoot, plan.root, parents[parentPath])
		if err != nil {
			return err
		}
		probeErr := probe(parent.root)
		closeErr := parent.root.Close()
		if probeErr != nil || closeErr != nil {
			return fmt.Errorf("atomic no-replace preflight for %s: %w", filepath.ToSlash(parentPath), errors.Join(probeErr, closeErr))
		}
	}
	if err := validatePruneRootSnapshot(plan.root); err != nil {
		return fmt.Errorf("project root changed during atomic no-replace preflight: %w", err)
	}
	return nil
}

// preflightPruneCandidates validates the complete reviewed candidate set
// before capability probes create private entries and before the first prune
// backup or detach. Per-candidate validation still repeats at its atomic
// boundary, but a stale later candidate cannot leave earlier prune effects.
func preflightPruneCandidates(ctx context.Context, plan *Plan, projectRoot *os.Root) error {
	if plan == nil || projectRoot == nil {
		return errors.New("pinned prune plan and project root are required")
	}
	if err := validatePruneRootSnapshot(plan.root); err != nil {
		return err
	}
	for _, candidate := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, err := candidateRootName(plan.root, candidate)
		if err != nil {
			return err
		}
		switch candidate.Status {
		case StatusReady:
			var body []byte
			var info fs.FileInfo
			var err error
			if candidate.currentPin != nil {
				body, info, err = verifyPinnedPruneRegular(projectRoot, name, candidate.currentPin, candidate.currentInfo, candidate.SHA256)
			} else {
				body, info, err = readPruneRegular(projectRoot, name)
			}
			if err != nil || !samePlannedPruneTarget(candidate, body, info) {
				return clierror.Refused(errors.Join(fmt.Errorf("cannot prune %s because it changed since the reviewed plan", candidate.Path), err))
			}
		case StatusMissing:
			if _, err := projectRoot.Lstat(name); err == nil {
				return clierror.Refused(fmt.Errorf("cannot prune %s because it reappeared since the reviewed plan", candidate.Path))
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return validatePruneRootSnapshot(plan.root)
}

func applyReadyCandidate(projectRoot *os.Root, snapshot pruneRootSnapshot, store *backup.Store, candidate Candidate, hooks applyHooks) (Resolution, error) {
	resolution := Resolution{Path: candidate.Path, Status: candidate.Status}
	parent, err := openPruneTargetParent(projectRoot, snapshot, candidate)
	if err != nil {
		return resolution, err
	}
	defer parent.root.Close()

	body, info, sourcePin, err := readPinnedPruneRegular(parent.root, parent.base)
	if err != nil {
		return resolution, err
	}
	defer func() {
		if resolution.recoveryPin != sourcePin {
			_ = sourcePin.Close()
		}
	}()
	if !samePlannedPruneTarget(candidate, body, info) {
		return resolution, clierror.Refused(fmt.Errorf("cannot prune %s because it changed since its reviewed commit; stage and commit it again before pruning", candidate.Path))
	}
	// Persist the verified source mode so a later restore reproduces the original
	// permissions instead of defaulting a private source to world-readable.
	entry, err := store.BackupBytesWithMode(candidate.Path, body, "pruned-native", info.Mode())
	if err != nil {
		return resolution, err
	}
	resolution.Backup = entry.BackupPath
	if hooks.afterBackup != nil {
		hooks.afterBackup(candidate, parent.root, parent.base)
	}
	afterBackupBody, afterBackupInfo, err := verifyPinnedPruneRegular(parent.root, parent.base, sourcePin, info, candidate.SHA256)
	if err != nil || !samePlannedPruneTarget(candidate, afterBackupBody, afterBackupInfo) {
		cause := fmt.Errorf("prune target %s changed after its prune backup was sealed", candidate.Path)
		if err != nil {
			cause = errors.Join(cause, err)
		}
		return resolution, discardTentativePruneBackup(store, candidate, &resolution, cause)
	}

	recovery, err := createPruneRecovery(parent, snapshot, candidate)
	if err != nil {
		return resolution, discardTentativePruneBackup(store, candidate, &resolution, err)
	}
	defer recovery.close()
	if err := store.SetRecoveryPath(candidate.Path, "pruned-native", recovery.path); err != nil {
		cleanupErr := cleanupEmptyPruneRecovery(parent, recovery)
		return resolution, discardTentativePruneBackup(store, candidate, &resolution, errors.Join(err, cleanupErr))
	}
	if hooks.afterRecoveryJournal != nil {
		if err := hooks.afterRecoveryJournal(candidate, recovery.path); err != nil {
			return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution, err)
		}
	}

	finalBody, finalInfo, err := verifyPinnedPruneRegular(parent.root, parent.base, sourcePin, info, candidate.SHA256)
	if err != nil || !samePlannedPruneTarget(candidate, finalBody, finalInfo) {
		cause := fmt.Errorf("prune target %s changed after its recovery path was journaled", candidate.Path)
		if err != nil {
			cause = errors.Join(cause, err)
		}
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution, cause)
	}
	if err := recovery.verify(parent); err != nil {
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution, err)
	}
	if _, err := recovery.root.Lstat(recovery.itemName); err == nil {
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution,
			fmt.Errorf("prune recovery destination appeared before detach: %s", recovery.path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution, err)
	}
	if hooks.beforeRecoveryRename != nil {
		hooks.beforeRecoveryRename(candidate, parent.root, parent.base, recovery.root, recovery.itemName)
	}
	if err := parent.verify(); err != nil {
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution, err)
	}
	if err := recovery.verify(parent); err != nil {
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution, err)
	}
	if _, err := recovery.root.Lstat(recovery.itemName); err == nil {
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution,
			fmt.Errorf("prune recovery destination appeared at the no-replace rename boundary: %s", recovery.path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution, err)
	}
	// Re-bind the source pathname to the retained descriptor after the last
	// testable race boundary. The no-replace rename protects the destination,
	// but it cannot condition the source move on an inode identity.
	boundaryBody, boundaryInfo, err := verifyPinnedPruneRegular(parent.root, parent.base, sourcePin, info, candidate.SHA256)
	if err != nil || finalInfo == nil || boundaryInfo == nil || !os.SameFile(finalInfo, boundaryInfo) || pruneSHA256(boundaryBody) != candidate.SHA256 {
		cause := fmt.Errorf("prune target %s changed at the atomic detach boundary", candidate.Path)
		if err != nil {
			cause = errors.Join(cause, err)
		}
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution, cause)
	}
	if hooks.afterSourceBoundary != nil {
		hooks.afterSourceBoundary(candidate, parent.root, parent.base, recovery.root, recovery.itemName)
	}
	if err := safefs.RenamePinnedRootNoReplace(parent.root, parent.base, recovery.root, recovery.itemName); err != nil {
		return resolution, abortPruneBeforeDetach(parent, recovery, store, candidate, &resolution,
			fmt.Errorf("atomically detaching prune target %s: %w", candidate.Path, err))
	}
	// Rename changes the moved inode's ctime on Linux, so capture the
	// post-rename generation only while the retained source descriptor proves
	// that the recovery name still identifies the inode observed at the final
	// pre-detach boundary. A raced replacement cannot reuse that inode while the
	// descriptor remains open.
	recovery.itemInfo = boundaryInfo
	if movedInfo, movedErr := recovery.root.Lstat(recovery.itemName); movedErr == nil &&
		movedInfo.Mode().IsRegular() && os.SameFile(boundaryInfo, movedInfo) {
		recovery.itemInfo = movedInfo
	}
	if hooks.afterDetach != nil {
		hooks.afterDetach(candidate, parent.root, parent.base, recovery.root, recovery.itemName)
	}

	detachedBody, detachedInfo, err := verifyPinnedPruneRegular(recovery.root, recovery.itemName, sourcePin, boundaryInfo, candidate.SHA256)
	if err == nil {
		if recovery.itemInfo != nil && safefs.SameFileGeneration(recovery.itemInfo, detachedInfo) && pruneSHA256(detachedBody) == candidate.SHA256 {
			if err := verifyPruneRecoveryPath(parent, recovery); err != nil {
				clearErr := store.ClearRecoveryPath(candidate.Path, "pruned-native")
				resolution.Messages = append(resolution.Messages, "detached target remains in the pinned private recovery container, but no stable absolute recovery path can be reported")
				return resolution, errors.Join(err, clearErr)
			}
			resolution.Recovery = recovery.path
			resolution.recoveryInfo = recovery.itemInfo
			resolution.recoveryPin = sourcePin
			resolution.Removed = true
			resolution.Messages = append(resolution.Messages, "canonical artifact was atomically detached; the original inode remains at the reported recovery path")
			return resolution, nil
		}
		err = errors.New("detached prune target no longer matches its retained source pin")
	}
	cause := fmt.Errorf("prune target %s changed during atomic detach", candidate.Path)
	if err != nil {
		cause = errors.Join(cause, err)
	}
	return resolution, rollbackChangedPruneTarget(parent, recovery, store, candidate, &resolution, sourcePin, hooks, cause)
}

func discardTentativePruneBackup(store *backup.Store, candidate Candidate, resolution *Resolution, cause error) error {
	if store == nil {
		return cause
	}
	if err := store.Discard("pruned-native", candidate.Path); err != nil {
		return errors.Join(cause, fmt.Errorf("discard tentative prune backup: %w", err))
	}
	if resolution != nil {
		resolution.Backup = ""
	}
	return cause
}

func abortPruneBeforeDetach(parent *pruneTargetParent, recovery *pruneRecovery, store *backup.Store, candidate Candidate, resolution *Resolution, cause error) error {
	discardErr := store.Discard("pruned-native", candidate.Path)
	if discardErr == nil && resolution != nil {
		resolution.Backup = ""
	}
	if discardErr != nil {
		if resolution != nil {
			resolution.Messages = append(resolution.Messages, "tentative backup and exact recovery intent may remain because they could not be discarded: "+recovery.path)
		}
		return errors.Join(cause, fmt.Errorf("discard tentative prune backup and recovery intent %s: %w", recovery.path, discardErr))
	}
	cleanupErr := cleanupEmptyPruneRecovery(parent, recovery)
	if cleanupErr != nil && resolution != nil {
		resolution.Messages = append(resolution.Messages, "private recovery container was retained because raced or unverifiable content prevented identity-clean removal")
	}
	return errors.Join(cause, cleanupErr)
}

func rollbackChangedPruneTarget(parent *pruneTargetParent, recovery *pruneRecovery, store *backup.Store, candidate Candidate, resolution *Resolution, sourcePin *os.File, hooks applyHooks, cause error) error {
	refreshPruneRecoveryGeneration(recovery, sourcePin)
	if hooks.beforeRollback != nil {
		hooks.beforeRollback(candidate, parent.root, parent.base, recovery.root, recovery.itemName)
	}
	rollbackErr := rollbackPruneNoReplace(parent, recovery)
	if rollbackErr == nil {
		clearErr := store.ClearRecoveryPath(candidate.Path, "pruned-native")
		cleanupErr := cleanupEmptyPruneRecovery(parent, recovery)
		if resolution != nil {
			resolution.Recovery = ""
			resolution.Messages = append(resolution.Messages, "detached target was rolled back without replacement after verification failed")
		}
		return errors.Join(fmt.Errorf("%w; detached target was rolled back without replacement", cause), clearErr, cleanupErr)
	}
	if verifyErr := verifyPruneRecoveryPath(parent, recovery); verifyErr == nil {
		if resolution != nil {
			resolution.Recovery = recovery.path
			resolution.recoveryInfo = recovery.itemInfo
			resolution.recoveryPin = sourcePin
			resolution.Messages = append(resolution.Messages, "rollback refused to overwrite a concurrent target; detached inode remains at the reported recovery path")
		}
		return fmt.Errorf("%w; rollback refused to overwrite a concurrent target: %w; detached inode remains at %s", cause, rollbackErr, recovery.path)
	} else {
		if racedInfo, racedPin, racedErr := pinCurrentPruneRecoveryPath(parent, recovery); racedErr == nil {
			if resolution != nil {
				resolution.Recovery = recovery.path
				resolution.recoveryInfo = racedInfo
				resolution.recoveryPin = racedPin
				resolution.Messages = append(resolution.Messages, "rollback refused; raced recovery content was never installed as canonical and remains at the reported path")
			} else {
				_ = racedPin.Close()
			}
			return errors.Join(fmt.Errorf("%w; rollback refused: %w; raced recovery content remains at %s with verified mode %s and size %d", cause, rollbackErr, recovery.path, racedInfo.Mode(), racedInfo.Size()), fmt.Errorf("detached entry did not match the reviewed inode: %w", verifyErr))
		}
		clearErr := store.ClearRecoveryPath(candidate.Path, "pruned-native")
		if resolution != nil {
			resolution.Recovery = ""
			resolution.Messages = append(resolution.Messages, "rollback refused and the pinned recovery entry has no stable verified absolute path")
		}
		return errors.Join(fmt.Errorf("%w; rollback refused: %w", cause, rollbackErr), verifyErr, clearErr)
	}
}

// refreshPruneRecoveryGeneration accepts an in-place post-detach mutation only
// while the retained source descriptor still proves that the recovery path
// names the original inode. Refreshing before the rollback race seam lets the
// generation-aware pathname check distinguish that mutation from a later
// delete/recreate replacement.
func refreshPruneRecoveryGeneration(recovery *pruneRecovery, sourcePin *os.File) {
	if recovery == nil || recovery.root == nil || recovery.itemInfo == nil || sourcePin == nil {
		return
	}
	pinned, pinnedErr := sourcePin.Stat()
	current, currentErr := recovery.root.Lstat(recovery.itemName)
	if pinnedErr == nil && currentErr == nil && pinned.Mode().IsRegular() && current.Mode().IsRegular() &&
		os.SameFile(recovery.itemInfo, pinned) && os.SameFile(pinned, current) {
		recovery.itemInfo = current
	}
}

func rollbackPruneNoReplace(parent *pruneTargetParent, recovery *pruneRecovery) error {
	if err := recovery.verify(parent); err != nil {
		return err
	}
	if _, err := parent.root.Lstat(parent.base); err == nil {
		return fmt.Errorf("prune target %s reappeared", filepath.ToSlash(filepath.Join(parent.path, parent.base)))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	itemInfo, err := recovery.root.Lstat(recovery.itemName)
	if err != nil {
		return err
	}
	if !itemInfo.Mode().IsRegular() {
		return errors.New("detached prune target is not a regular file")
	}
	if recovery.itemInfo == nil || !safefs.SameFileGeneration(recovery.itemInfo, itemInfo) {
		return errors.New("detached prune target changed identity before rollback")
	}
	if err := safefs.RenamePinnedRootNoReplace(recovery.root, recovery.itemName, parent.root, parent.base); err != nil {
		return err
	}
	current, err := parent.root.Lstat(parent.base)
	if err != nil || !os.SameFile(itemInfo, current) {
		return errors.Join(errors.New("rolled-back prune target changed identity"), err)
	}
	return nil
}

func createPruneRecovery(parent *pruneTargetParent, snapshot pruneRootSnapshot, candidate Candidate) (*pruneRecovery, error) {
	if err := parent.verify(); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 100; attempt++ {
		name := randomPruneName(".threadpoint-prune-recovery-")
		if err := parent.root.Mkdir(name, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		containerPath := filepath.Join(snapshot.physical, parent.path, name)
		containerInfo, err := parent.root.Lstat(name)
		if err != nil {
			return nil, fmt.Errorf("new prune recovery container identity could not be established; residue may remain at %s: %w", containerPath, err)
		}
		root, err := parent.root.OpenRoot(name)
		if err != nil {
			return nil, fmt.Errorf("new prune recovery container may remain at %s because no exact cleanup handle could be opened: %w", containerPath, err)
		}
		opened, err := root.Stat(".")
		if err != nil || !containerInfo.IsDir() || containerInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(containerInfo, opened) {
			_ = root.Close()
			return nil, fmt.Errorf("prune recovery container changed while it was opened; residue may remain at %s: %w", containerPath, errors.Join(errors.New("recovery container identity mismatch"), err))
		}
		itemName := "original"
		containerRel := filepath.Join(parent.path, name)
		itemRel := filepath.Join(name, itemName)
		recovery := &pruneRecovery{
			containerName: name,
			itemName:      itemName,
			itemRel:       itemRel,
			path:          filepath.Join(snapshot.physical, containerRel, itemName),
			root:          root,
			containerInfo: containerInfo,
		}
		if err := recovery.verify(parent); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("new prune recovery container may remain at %s: %w", containerPath, err)
		}
		return recovery, nil
	}
	return nil, fmt.Errorf("could not allocate a private recovery path for prune target %s", candidate.Path)
}

func randomPruneName(prefix string) string {
	var token [12]byte
	_, _ = rand.Read(token[:])
	return prefix + hex.EncodeToString(token[:])
}

func (recovery *pruneRecovery) verify(parent *pruneTargetParent) error {
	if recovery == nil || recovery.root == nil {
		return errors.New("prune recovery container is not open")
	}
	if err := parent.verify(); err != nil {
		return err
	}
	current, err := parent.root.Lstat(recovery.containerName)
	if err != nil {
		return err
	}
	opened, err := recovery.root.Stat(".")
	if err != nil {
		return err
	}
	if recovery.containerInfo == nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(recovery.containerInfo, current) || !os.SameFile(recovery.containerInfo, opened) {
		return errors.New("prune recovery container changed during the rooted transaction")
	}
	return nil
}

func (recovery *pruneRecovery) close() {
	if recovery != nil && recovery.root != nil {
		_ = recovery.root.Close()
		recovery.root = nil
	}
}

func verifyPruneRecoveryPath(parent *pruneTargetParent, recovery *pruneRecovery) error {
	if err := recovery.verify(parent); err != nil {
		return err
	}
	current, err := recovery.root.Lstat(recovery.itemName)
	if err != nil {
		return err
	}
	if recovery.itemInfo == nil || !current.Mode().IsRegular() || !safefs.SameFileGeneration(recovery.itemInfo, current) {
		return errors.New("prune recovery item changed identity")
	}
	projectName := filepath.Join(parent.path, recovery.itemRel)
	if err := safefs.RejectRootSymlinkAncestors(parent.project, projectName); err != nil {
		return err
	}
	absolute, err := parent.project.Lstat(projectName)
	if err != nil {
		return err
	}
	if !absolute.Mode().IsRegular() || !safefs.SameFileGeneration(recovery.itemInfo, absolute) {
		return errors.New("prune recovery pathname no longer names the detached inode")
	}
	return parent.verify()
}

func pinCurrentPruneRecoveryPath(parent *pruneTargetParent, recovery *pruneRecovery) (fs.FileInfo, *os.File, error) {
	if err := recovery.verify(parent); err != nil {
		return nil, nil, err
	}
	_, current, pin, err := readPinnedPruneRegular(recovery.root, recovery.itemName)
	if err != nil {
		return nil, nil, err
	}
	fail := func(cause error) (fs.FileInfo, *os.File, error) {
		return nil, nil, errors.Join(cause, pin.Close())
	}
	projectName := filepath.Join(parent.path, recovery.itemRel)
	if err := safefs.RejectRootSymlinkAncestors(parent.project, projectName); err != nil {
		return fail(err)
	}
	absolute, err := parent.project.Lstat(projectName)
	if err != nil {
		return fail(err)
	}
	if !absolute.Mode().IsRegular() || !os.SameFile(current, absolute) {
		return fail(errors.New("current prune recovery pathname changed during verification"))
	}
	if err := parent.verify(); err != nil {
		return fail(err)
	}
	return current, pin, nil
}

// finalizePruneRecoveryClaims is the last non-crash reporting boundary. An
// interrupted process may leave RecoveryPath marking the sealed run-local
// payload as durable transaction intent; a returned report, however, publishes
// the transient quarantine path only while it still names the inode verified
// by the rooted transaction.
func finalizePruneRecoveryClaims(root *os.Root, snapshot pruneRootSnapshot, store *backup.Store, report *Report) error {
	if report == nil || store == nil {
		return nil
	}
	hasRecovery := false
	for _, resolution := range report.Resolutions {
		if resolution.Recovery != "" {
			hasRecovery = true
			break
		}
	}
	if !hasRecovery {
		return nil
	}
	rootErr := validatePruneRootSnapshot(snapshot)
	if rootErr == nil {
		opened, err := root.Stat(".")
		if err != nil || snapshot.info == nil || !os.SameFile(snapshot.info, opened) {
			rootErr = errors.Join(errors.New("pinned prune root changed before recovery finalization"), err)
		}
	}
	var result error
	for index := range report.Resolutions {
		resolution := &report.Resolutions[index]
		if resolution.Recovery == "" {
			continue
		}
		claimErr := rootErr
		if claimErr == nil {
			name, err := filepath.Rel(snapshot.physical, filepath.Clean(resolution.Recovery))
			if err == nil && (name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator))) {
				err = fmt.Errorf("recovery path escapes the selected physical root: %s", resolution.Recovery)
			}
			if err == nil {
				err = safefs.RejectRootSymlinkAncestors(root, name)
			}
			var current fs.FileInfo
			if err == nil {
				current, err = root.Lstat(name)
			}
			var pinned fs.FileInfo
			if err == nil && resolution.recoveryPin != nil {
				pinned, err = resolution.recoveryPin.Stat()
			}
			if err == nil && (resolution.recoveryInfo == nil || pinned == nil || !current.Mode().IsRegular() || !pinned.Mode().IsRegular() || !safefs.SameFileGeneration(resolution.recoveryInfo, pinned) || !safefs.SameFileGeneration(resolution.recoveryInfo, current)) {
				err = errors.New("recovery path no longer names its verified inode")
			}
			claimErr = err
		}
		if claimErr == nil {
			continue
		}
		stale := resolution.Recovery
		resolution.Recovery = ""
		resolution.recoveryInfo = nil
		resolution.Messages = append(resolution.Messages, "recovery path was cleared because it no longer named the inode verified by the prune transaction")
		clearErr := store.ClearRecoveryPath(resolution.Path, "pruned-native")
		result = errors.Join(result, fmt.Errorf("recovery claim %s became stale: %w", stale, claimErr), clearErr)
	}
	return result
}

func closePruneRecoveryPins(report *Report) error {
	if report == nil {
		return nil
	}
	var result error
	for index := range report.Resolutions {
		pin := report.Resolutions[index].recoveryPin
		if pin == nil {
			continue
		}
		result = errors.Join(result, pin.Close())
		report.Resolutions[index].recoveryPin = nil
	}
	return result
}

func cleanupEmptyPruneRecovery(parent *pruneTargetParent, recovery *pruneRecovery) error {
	if recovery == nil || recovery.root == nil {
		return nil
	}
	directory, err := recovery.root.Open(".")
	if err != nil {
		return fmt.Errorf("private prune recovery container could not be inspected and was retained at %s: %w", filepath.Dir(recovery.path), err)
	}
	entries, readErr := directory.ReadDir(1)
	closeDirErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return fmt.Errorf("private prune recovery container could not be inspected and was retained at %s: %w", filepath.Dir(recovery.path), errors.Join(readErr, closeDirErr))
	}
	if closeDirErr != nil {
		return fmt.Errorf("private prune recovery container was retained at %s after its directory handle could not be closed: %w", filepath.Dir(recovery.path), closeDirErr)
	}
	if len(entries) != 0 {
		return fmt.Errorf("private prune recovery container received raced content and was retained at %s", filepath.Dir(recovery.path))
	}
	if err := recovery.verify(parent); err != nil {
		return fmt.Errorf("empty prune recovery container could not be identity-cleaned at %s: %w", filepath.Dir(recovery.path), err)
	}
	opened, err := recovery.root.Stat(".")
	if err != nil {
		return err
	}
	current, err := parent.root.Lstat(recovery.containerName)
	if err != nil || recovery.containerInfo == nil || !os.SameFile(recovery.containerInfo, opened) || !os.SameFile(recovery.containerInfo, current) {
		return errors.Join(fmt.Errorf("empty prune recovery container changed before cleanup: %s", filepath.Dir(recovery.path)), err)
	}
	if err := parent.root.Remove(recovery.containerName); err != nil {
		return fmt.Errorf("empty prune recovery container remains at %s: %w", filepath.Dir(recovery.path), err)
	}
	if err := recovery.root.Close(); err != nil {
		return fmt.Errorf("removed empty prune recovery container could not be closed: %w", err)
	}
	recovery.root = nil
	return nil
}

func buildCandidate(projectRoot *os.Root, snapshot pruneRootSnapshot, root string, runID string, entry backup.Entry) (Candidate, error) {
	sourceRoot := root
	candidate := Candidate{
		Path:       filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry.Path))),
		Root:       sourceRoot,
		BackupRun:  runID,
		BackupPath: entry.BackupPath,
		SHA256:     entry.SHA256,
	}
	if entry.SHA256 == "" {
		candidate.Status = StatusChanged
		candidate.Code = "prune_backup_hash_missing"
		candidate.Message = "Backup entry has no content hash."
		return candidate, nil
	}
	target, err := safepath.Join(snapshot.physical, entry.Path)
	if err != nil {
		return Candidate{}, err
	}
	targetName, err := filepath.Rel(snapshot.physical, target)
	if err != nil {
		return Candidate{}, err
	}
	body, currentInfo, currentPin, err := readPinnedPruneRegular(projectRoot, targetName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			candidate.Status = StatusMissing
			candidate.Code = "prune_artifact_missing"
			candidate.Message = "Native artifact is already absent."
			return candidate, nil
		}
		return Candidate{}, err
	}
	current := pruneSHA256(body)
	candidate.CurrentSHA256 = current
	candidate.currentInfo = currentInfo
	if current != entry.SHA256 {
		_ = currentPin.Close()
		candidate.Status = StatusChanged
		candidate.Code = "prune_artifact_changed"
		candidate.Message = "Native artifact changed after it was backed up."
		return candidate, nil
	}
	candidate.Status = StatusReady
	candidate.Code = "prune_artifact_ready"
	candidate.Message = "Native artifact still matches its backup."
	candidate.currentPin = currentPin
	return candidate, nil
}

func closePrunePlanPins(plan *Plan) {
	if plan == nil {
		return
	}
	for index := range plan.Candidates {
		if plan.Candidates[index].currentPin != nil {
			_ = plan.Candidates[index].currentPin.Close()
			plan.Candidates[index].currentPin = nil
		}
	}
}

func confirmed(opts Options) bool {
	return opts.Yes || opts.Confirm == pruneConfirmation
}
