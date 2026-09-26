// SPDX-License-Identifier: Apache-2.0

// Package restore restores files from threadpoint backups. It supports
// listing runs, planning a rollback, applying clean restores, and an
// interactive conflict resolver. BuildPlan returns a pure plan value; Apply
// performs the effects.
package restore

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/clierror"
	"github.com/threadgrid/threadpoint/internal/editor"
	"github.com/threadgrid/threadpoint/internal/textdiff"
	"github.com/threadgrid/threadpoint/redact"
	"github.com/threadgrid/threadpoint/review"
	"github.com/threadgrid/threadpoint/safefs"
	"github.com/threadgrid/threadpoint/safepath"
)

const (
	restoreConfirmation          = "restore threadpoint backup"
	restoreOperation             = "restore"
	maxRestoreRegularPayloadSize = 64 * 1024 * 1024
	maxSymlinkMarkerSize         = 64 * 1024
	maxRestoreDiffSize           = 1024 * 1024
	maxRestoreDirectoryEntries   = 4096
	maxRestoreDirectoryBytes     = 64 * 1024 * 1024
)

// Status describes whether a backup candidate can be restored cleanly.
type Status string

// StatusReady can be restored without overwriting a divergent target;
// StatusAlreadyRestored and StatusConflict explain why no clean restore is needed.
const (
	StatusReady           Status = "ready"
	StatusAlreadyRestored Status = "already-restored"
	StatusConflict        Status = "conflict"
)

// Decision is the chosen restoration action for a candidate.
type Decision string

// DecisionRestore writes the backup, DecisionEdit opens conflict resolution,
// and DecisionSkip leaves the target unchanged.
const (
	DecisionRestore Decision = restoreOperation
	DecisionEdit    Decision = "edit"
	DecisionSkip    Decision = "skip"
)

// Options configures a restoration run: the project Root, the user HomeDir and
// backup directory, the backup selection (BackupID or Latest), the
// confirmation, and the clock (Now).
type Options struct {
	Root            string
	HomeDir         string
	ThreadpointHome string
	BackupDir       string
	BackupID        string
	Latest          bool
	PlanOnly        bool
	Apply           bool
	Yes             bool
	Confirm         string
	OutputRedaction redact.Options
	Now             func() time.Time
	// ProductRoot is a borrowed, caller-retained ThreadpointHome capability.
	// When set, manifests, payloads, and new backup runs stay under that root.
	ProductRoot *os.Root
	hooks       *restoreHooks
}

// restoreHooks provide deterministic transaction-boundary scheduling in
// package tests. Production callers cannot set them.
type restoreHooks struct {
	afterPlan                func(*Plan, *os.Root) error
	probeAtomicDetach        func(*os.Root) error
	renameNoReplace          func(*os.Root, string, string) error
	beforeCurrentRegularRead func(*os.Root, string)
	beforeDirectoryCopy      func()
	beforeOverwriteBackup    func(*os.Root, string)
	beforeDetach             func(*os.Root, string)
	beforeRecoveryRename     func(*os.Root, string)
	beforeDetachIdentityPin  func(*os.Root, string)
	afterRecoveryJournal     func(string) error
	afterDetachRename        func(*os.Root, string, string)
	beforeInstall            func(*os.Root, string)
	duringDirectoryInstall   func(*os.Root)
	beforeRollback           func(*os.Root, string)
	beforeDetachedDelete     func(*os.Root, string)
	afterRecoveryValidation  func()
	afterDetachedDelete      func(*os.Root, string)
	beforePreparedCleanup    func(*os.Root, string)
}

// Candidate is one file, directory, or symlink selected from a backup run.
type Candidate struct {
	Path              string `json:"path"`
	Root              string `json:"root"`
	Role              string `json:"role"`
	BackupRun         string `json:"backupRun"`
	BackupPath        string `json:"backupPath"`
	Target            string `json:"target"`
	SHA256            string `json:"sha256,omitempty"`
	CurrentSHA256     string `json:"currentSha256,omitempty"`
	PostSHA256        string `json:"postSha256,omitempty"`
	PendingPostSHA256 string `json:"pendingPostSha256,omitempty"`
	Directory         bool   `json:"directory,omitempty"`
	Symlink           bool   `json:"symlink,omitempty"`
	Absent            bool   `json:"absent,omitempty"`
	currentSnapshot   string
	backupSnapshot    string
	currentInfo       fs.FileInfo
	currentPin        *os.File
	backupInfo        fs.FileInfo
	backupPin         *os.File
	backupTree        *restorePayloadTreePin
	backupMode        os.FileMode
	backupModeKnown   bool
	backupTreeModes   map[string]uint32
	backupProductRoot *os.Root
	backupProductPath string
	backupProductRel  string
	Status            Status `json:"status"`
	Code              string `json:"code,omitempty"`
	Message           string `json:"message,omitempty"`
	Diff              string `json:"diff,omitempty"`
}

// MarshalJSON redacts sensitive fields at the type boundary so every JSON
// rendering is safe. Raw fields remain available to non-JSON operations.
func (candidate Candidate) MarshalJSON() ([]byte, error) {
	options := redact.Options{Prefixes: []redact.Prefix{{Value: candidate.Root, Label: "$ROOT"}}}
	path, pathRedacted := redact.StringWithOptions(candidate.Path, options)
	root, rootRedacted := redact.StringWithOptions(candidate.Root, options)
	backupPath, backupPathRedacted := redact.StringWithOptions(candidate.BackupPath, options)
	target, targetRedacted := redact.StringWithOptions(candidate.Target, options)
	message, messageRedacted := redact.StringWithOptions(candidate.Message, options)
	diff, diffRedacted := redact.StringWithOptions(candidate.Diff, options)
	type candidateJSON struct {
		Path              string `json:"path"`
		Root              string `json:"root"`
		Role              string `json:"role"`
		BackupRun         string `json:"backupRun"`
		BackupPath        string `json:"backupPath"`
		Target            string `json:"target"`
		SHA256            string `json:"sha256,omitempty"`
		CurrentSHA256     string `json:"currentSha256,omitempty"`
		PostSHA256        string `json:"postSha256,omitempty"`
		PendingPostSHA256 string `json:"pendingPostSha256,omitempty"`
		Directory         bool   `json:"directory,omitempty"`
		Symlink           bool   `json:"symlink,omitempty"`
		Absent            bool   `json:"absent,omitempty"`
		Status            Status `json:"status"`
		Code              string `json:"code,omitempty"`
		Message           string `json:"message,omitempty"`
		Diff              string `json:"diff,omitempty"`
		Redacted          bool   `json:"redacted,omitempty"`
	}
	return json.Marshal(candidateJSON{
		Path:              path,
		Root:              root,
		Role:              candidate.Role,
		BackupRun:         candidate.BackupRun,
		BackupPath:        backupPath,
		Target:            target,
		SHA256:            candidate.SHA256,
		CurrentSHA256:     candidate.CurrentSHA256,
		PostSHA256:        candidate.PostSHA256,
		PendingPostSHA256: candidate.PendingPostSHA256,
		Directory:         candidate.Directory,
		Symlink:           candidate.Symlink,
		Absent:            candidate.Absent,
		Status:            candidate.Status,
		Code:              candidate.Code,
		Message:           message,
		Diff:              diff,
		Redacted:          pathRedacted || rootRedacted || backupPathRedacted || targetRedacted || messageRedacted || diffRedacted,
	})
}

// Plan is the dry-run result of restoration: the candidate files to restore
// from the selected backup run and their status. It is a pure value with no
// effects.
type Plan struct {
	Root         string      `json:"root"`
	BackupRun    string      `json:"backupRun"`
	Operation    string      `json:"operation"`
	BackupPath   string      `json:"backupPath"`
	Candidates   []Candidate `json:"candidates"`
	Warnings     []string    `json:"warnings,omitempty"`
	rootSnapshot restoreRootSnapshot
}

// Resolution reports the restoration outcome for one candidate.
type Resolution struct {
	Path     string   `json:"path"`
	Decision Decision `json:"decision"`
	Status   Status   `json:"status"`
	Backup   string   `json:"backup,omitempty"`
	Recovery string   `json:"recovery,omitempty"`
	Restored bool     `json:"restored,omitempty"`
	Skipped  bool     `json:"skipped,omitempty"`
	Messages []string `json:"messages,omitempty"`
}

// Report is the result of applying a restoration Plan: the resolution for each
// candidate.
type Report struct {
	Root        string       `json:"root"`
	Applied     bool         `json:"applied"`
	Plan        *Plan        `json:"plan"`
	Resolutions []Resolution `json:"resolutions,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
}

type pendingResolution struct {
	candidate Candidate
	decision  Decision
	content   string
	edited    bool
}

type preparedBackupPayload struct {
	body          []byte
	symlinkTarget string
	directory     bool
	postHash      string
	mode          os.FileMode
	modeKnown     bool
}

type preparedRestoreDirectory struct {
	name string
	root *os.Root
	info fs.FileInfo
}

type restorePayloadTreePin struct {
	root    *os.Root
	info    fs.FileInfo
	entries map[string]restorePayloadEntryPin
}

type restorePayloadEntryPin struct {
	info fs.FileInfo
	pin  *os.File
}

type retainedDetachedRestoreError struct {
	cause                error
	detached             string
	recoveryRef          string
	rolledBack           bool
	recoveryIdentityLost bool
}

type detachedRestoreTarget struct {
	containerName string
	itemName      string
	itemPath      string
	recoveryPath  string
	container     *os.Root
	containerInfo fs.FileInfo
	itemInfo      fs.FileInfo
	itemPin       *os.File
}

// Error reports the failed transaction and its retained recovery location.
func (err *retainedDetachedRestoreError) Error() string {
	if err.rolledBack {
		return fmt.Sprintf("%v; original restore target was rolled back without replacement", err.cause)
	}
	if err.recoveryIdentityLost {
		return fmt.Sprintf("%v; the recovery pathname %s no longer identifies the detached original, so its replacement was retained and was not moved into the canonical target", err.cause, err.recoveryRef)
	}
	if err.detached != "" {
		return fmt.Sprintf("%v; original restore target was retained at %s", err.cause, err.detached)
	}
	return fmt.Sprintf("%v; original restore target was retained in %s, but the moved root or parent has no stable ambient pathname", err.cause, err.recoveryRef)
}

// Unwrap exposes the transaction failure that caused the original target to
// remain in its journaled recovery location.
func (err *retainedDetachedRestoreError) Unwrap() error {
	return err.cause
}

type restoreTargetParent struct {
	project      *os.Root
	root         *os.Root
	path         string
	base         string
	parentInfo   fs.FileInfo
	rootSnapshot restoreRootSnapshot
}

type restoreRootSnapshot struct {
	logicalRoot  string
	physicalRoot string
	info         fs.FileInfo
}

// ListRuns returns the backup runs available for opts' root, most recent first.
func ListRuns(opts Options) ([]backup.Run, error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if normalized.ProductRoot != nil {
		homePath, err := restoreProductHomePath(normalized)
		if err != nil {
			return nil, err
		}
		return backup.ListRunsFromRoot(normalized.Root, normalized.ProductRoot, homePath, normalized.BackupDir)
	}
	return backup.ListRunsWithOverride(normalized.Root, normalized.HomeDir, normalized.ThreadpointHome, normalized.BackupDir)
}

// BuildPlan returns a pure restoration plan for opts — the files to restore from
// the selected backup run and their status — without performing any effects.
// Apply executes it.
func BuildPlan(ctx context.Context, opts Options) (*Plan, error) {
	plan, root, err := buildRestorePlanWithPinnedRoot(ctx, opts)
	err = errors.Join(err, closeRestorePlanPins(plan))
	if root != nil {
		err = errors.Join(err, root.Close())
	}
	return plan, err
}

func buildRestorePlanWithPinnedRoot(ctx context.Context, opts Options) (resultPlan *Plan, resultRoot *os.Root, resultErr error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, nil, err
	}
	run, err := selectRun(normalized)
	if err != nil {
		return nil, nil, err
	}
	plan := &Plan{
		Root:       normalized.Root,
		BackupRun:  run.RunID,
		Operation:  run.Operation,
		BackupPath: run.Path,
	}
	if err := validateBackupManifest(run); err != nil {
		return nil, nil, err
	}
	rootSnapshot, root, err := captureAndOpenRestoreRoot(normalized.Root)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		resultErr = errors.Join(resultErr, closeRestorePlanPins(resultPlan), root.Close())
		resultRoot = nil
	}()
	plan.rootSnapshot = rootSnapshot
	if normalized.ProductRoot != nil {
		homePath, err := restoreProductHomePath(normalized)
		if err != nil {
			return plan, nil, err
		}
		plan.Warnings = append(plan.Warnings, backup.StaleWarningsFromRoot(normalized.Root, normalized.ProductRoot, homePath, normalized.BackupDir, normalized.Now())...)
	} else {
		plan.Warnings = append(plan.Warnings, backup.StaleWarningsWithOverride(normalized.Root, normalized.HomeDir, normalized.ThreadpointHome, normalized.BackupDir, normalized.Now())...)
	}
	seen := map[string]bool{}
	for _, entry := range run.Entries {
		if err := ctx.Err(); err != nil {
			return plan, nil, err
		}
		if seen[entry.Path] {
			continue
		}
		seen[entry.Path] = true
		candidate, err := buildCandidate(root, normalized, run.RunID, entry)
		if err != nil {
			return plan, nil, err
		}
		if err := captureRestoreCandidatePins(root, &candidate); err != nil {
			return plan, nil, errors.Join(err, closeRestoreCandidatePins(&candidate))
		}
		plan.Candidates = append(plan.Candidates, candidate)
	}
	sort.Slice(plan.Candidates, func(i, j int) bool {
		return plan.Candidates[i].Path < plan.Candidates[j].Path
	})
	return plan, root, nil
}

func validateBackupManifest(run *backup.Run) error {
	if run.Version != backup.ManifestVersion {
		return fmt.Errorf("backup run %s has unsupported manifest version %d; expected %d", run.RunID, run.Version, backup.ManifestVersion)
	}
	if err := backup.ValidateRun(run); err != nil {
		return fmt.Errorf("backup run does not satisfy the current manifest schema: %w", err)
	}
	return validateEffectiveRestoreTargets(run)
}

func validateEffectiveRestoreTargets(run *backup.Run) error {
	type targetOwner struct {
		key  string
		path string
	}
	seenTargets := make(map[string]string, len(run.Entries))
	seenFolded := make(map[string]targetOwner, len(run.Entries))
	for _, entry := range run.Entries {
		if strings.TrimSpace(entry.Path) == "" {
			return fmt.Errorf("backup run %s contains an entry with an empty path", run.RunID)
		}
		cleanPath := filepath.Clean(filepath.FromSlash(entry.Path))
		if filepath.IsAbs(cleanPath) || filepath.VolumeName(cleanPath) != "" || cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
			return fmt.Errorf("backup entry for %s has an unsafe restore path", entry.Path)
		}
		targetKey := filepath.ToSlash(cleanPath)
		if earlier, exists := seenTargets[targetKey]; exists {
			return fmt.Errorf("backup run %s contains duplicate effective restore target %q from entries %q and %q", run.RunID, targetKey, earlier, entry.Path)
		}
		folded := strings.ToLower(targetKey)
		if earlier, exists := seenFolded[folded]; exists && earlier.key != targetKey {
			return fmt.Errorf("backup run %s contains filesystem-equivalent restore target names %q and %q", run.RunID, earlier.path, entry.Path)
		}
		seenTargets[targetKey] = entry.Path
		seenFolded[folded] = targetOwner{key: targetKey, path: entry.Path}
	}
	return nil
}

// Apply builds the restoration plan for opts and restores the clean candidates,
// backing up any files it overwrites, and returns a Report.
func Apply(ctx context.Context, opts Options) (resultReport *Report, resultErr error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if !confirmed(normalized) {
		return nil, clierror.Refused(fmt.Errorf("restore --apply requires --confirm %q or --yes", restoreConfirmation))
	}
	plan, root, err := buildRestorePlanWithPinnedRoot(ctx, normalized)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeRestorePlanPins(plan), root.Close())
	}()
	if normalized.hooks != nil && normalized.hooks.afterPlan != nil {
		if err := normalized.hooks.afterPlan(plan, root); err != nil {
			return nil, err
		}
	}
	var pending []pendingResolution
	for _, candidate := range plan.Candidates {
		switch candidate.Status {
		case StatusAlreadyRestored:
			pending = append(pending, pendingResolution{candidate: candidate, decision: DecisionSkip})
		case StatusReady:
			pending = append(pending, pendingResolution{candidate: candidate, decision: DecisionRestore})
		case StatusConflict:
			return nil, clierror.Refused(fmt.Errorf("cannot restore %s noninteractively because the target has newer content", candidate.Path))
		}
	}
	return applyResolutionsWithPinnedRoot(ctx, normalized, plan, pending, root)
}

// RunInteractive resolves restore conflicts through stdin/stdout prompts.
func RunInteractive(ctx context.Context, opts Options, stdin io.Reader, stdout io.Writer, stderr io.Writer) (resultReport *Report, resultErr error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	plan, root, err := buildRestorePlanWithPinnedRoot(ctx, normalized)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeRestorePlanPins(plan), root.Close())
	}()
	if normalized.hooks != nil && normalized.hooks.afterPlan != nil {
		if err := normalized.hooks.afterPlan(plan, root); err != nil {
			return nil, err
		}
	}
	if len(plan.Candidates) == 0 {
		report := &Report{Root: normalized.Root, Plan: plan}
		fmt.Fprintln(stdout, "No backup entries found.")
		return report, nil
	}

	reader := bufio.NewReader(stdin)
	var pending []pendingResolution
	for _, candidate := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if candidate.Status == StatusAlreadyRestored {
			pending = append(pending, pendingResolution{candidate: candidate, decision: DecisionSkip})
			continue
		}
		path, pathRedacted := redact.StringWithOptions(candidate.Path, normalized.OutputRedaction)
		message, messageRedacted := redact.StringWithOptions(candidate.Message, normalized.OutputRedaction)
		fmt.Fprintf(stdout, "\n%s\n%s\ncode: %s\n", path, message, candidate.Code)
		if candidate.Diff != "" {
			diff, diffRedacted := redact.StringWithOptions(candidate.Diff, normalized.OutputRedaction)
			fmt.Fprintf(stdout, "\n%s\n", diff)
			if pathRedacted || messageRedacted || diffRedacted {
				fmt.Fprintln(stdout, "threadpoint: redacted obvious sensitive values in this preview.")
			}
		} else if pathRedacted || messageRedacted {
			fmt.Fprintln(stdout, "threadpoint: redacted obvious sensitive values in this preview.")
		}
		for {
			prompt := "Decision [r]estore, [s]kip, [q]uit: "
			if candidate.Status == StatusConflict && !candidate.Directory && !candidate.Symlink && !candidate.Absent {
				prompt = "Decision [r]estore, [e]dit, [s]kip, [q]uit: "
			}
			fmt.Fprint(stdout, prompt)
			answer, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer == "" {
				answer = "s"
			}
			switch answer {
			case "r", string(DecisionRestore):
				pending = append(pending, pendingResolution{candidate: candidate, decision: DecisionRestore})
				goto nextCandidate
			case "e", "edit":
				if candidate.Directory || candidate.Symlink || candidate.Absent {
					fmt.Fprintln(stderr, "edit is only available for file backups")
					continue
				}
				prepared, err := prepareBackupPayload(candidate, "", false)
				if err != nil {
					return nil, err
				}
				edited, err := editor.Open(ctx, review.Streams{Stdin: stdin, Stdout: stdout, Stderr: stderr}, string(prepared.body))
				if err != nil {
					fmt.Fprintf(stderr, "edit failed: %v\n", err)
					continue
				}
				pending = append(pending, pendingResolution{candidate: candidate, decision: DecisionEdit, content: edited, edited: true})
				goto nextCandidate
			case "s", "skip":
				pending = append(pending, pendingResolution{candidate: candidate, decision: DecisionSkip})
				goto nextCandidate
			case "q", "quit", "abort":
				return nil, clierror.Refused(errors.New("restore aborted"))
			default:
				fmt.Fprintln(stderr, "unknown decision")
			}
			if errors.Is(err, io.EOF) {
				break
			}
		}
	nextCandidate:
	}
	return applyResolutionsWithPinnedRoot(ctx, normalized, plan, pending, root)
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

func selectRun(opts Options) (*backup.Run, error) {
	if opts.ProductRoot != nil {
		homePath, err := restoreProductHomePath(opts)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(opts.BackupID) != "" {
			return backup.FindRunFromRoot(opts.Root, opts.ProductRoot, homePath, opts.BackupDir, opts.BackupID)
		}
		return backup.LatestRunFromRoot(opts.Root, opts.ProductRoot, homePath, opts.BackupDir)
	}
	if strings.TrimSpace(opts.BackupID) != "" {
		return backup.FindRunWithOverride(opts.Root, opts.HomeDir, opts.ThreadpointHome, opts.BackupDir, opts.BackupID)
	}
	return backup.LatestRunWithOverride(opts.Root, opts.HomeDir, opts.ThreadpointHome, opts.BackupDir)
}

func restoreProductHomePath(opts Options) (string, error) {
	return safefs.ResolveThreadpointHomeWithOverride(opts.HomeDir, opts.ThreadpointHome)
}

func buildCandidate(root *os.Root, opts Options, runID string, entry backup.Entry) (result Candidate, resultErr error) {
	targetRoot, err := targetRootForEntry(opts.Root, entry.Root)
	if err != nil {
		return Candidate{}, err
	}
	target, err := safepath.Join(targetRoot, entry.Path)
	if err != nil {
		return Candidate{}, err
	}
	candidate := Candidate{
		Path:              filepath.ToSlash(entry.Path),
		Root:              targetRoot,
		Role:              entry.Role,
		BackupRun:         runID,
		BackupPath:        entry.BackupPath,
		Target:            target,
		SHA256:            entry.SHA256,
		PostSHA256:        entry.PostSHA256,
		PendingPostSHA256: entry.PendingPostSHA256,
		Directory:         entry.Directory,
		Symlink:           entry.Symlink,
		Absent:            entry.Absent,
		backupMode:        os.FileMode(entry.Mode),
		backupModeKnown:   entry.ModeKnown,
		backupTreeModes:   entry.TreeModes,
	}
	if opts.ProductRoot != nil && !entry.Absent {
		homePath, err := restoreProductHomePath(opts)
		if err != nil {
			return Candidate{}, err
		}
		backupRel, err := backup.ProductRootRelativePath(homePath, entry.BackupPath)
		if err != nil {
			return Candidate{}, err
		}
		candidate.backupProductRoot = opts.ProductRoot
		candidate.backupProductPath = homePath
		candidate.backupProductRel = backupRel
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeRestoreCandidatePins(&candidate))
		}
	}()
	if err := captureRestoreBackupPins(&candidate); err != nil {
		return Candidate{}, err
	}
	targetName, err := candidateRootName(candidate)
	if err != nil {
		return Candidate{}, err
	}
	if err := safefs.RejectRootSymlinkAncestors(root, filepath.Dir(targetName)); err != nil {
		return Candidate{}, err
	}
	if entry.Absent {
		if entry.Directory || entry.Symlink || entry.BackupPath != "" || entry.SHA256 != "" {
			return Candidate{}, fmt.Errorf("backup entry for %s has an invalid absent-target encoding", entry.Path)
		}
		if info, statErr := root.Lstat(targetName); statErr == nil {
			if err := captureRestoreCurrentPin(root, targetName, &candidate, info); err != nil {
				return Candidate{}, err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return Candidate{}, statErr
		}
		return buildAbsentCandidate(root, candidate, targetName, opts.hooks)
	}
	if entry.Directory {
		if err := validateRestoreDirectoryModes(candidate); err != nil {
			return Candidate{}, err
		}
		if err := verifyRestorePayloadTree(candidate); err != nil {
			return Candidate{}, err
		}
		backupHash, err := restoreDirectoryDigestRoot(candidate.backupTree.root, ".")
		if err != nil {
			return Candidate{}, fmt.Errorf("backup payload for %s is not a directory or could not be verified: %w", entry.Path, err)
		}
		if entry.SHA256 != "" && backupHash != entry.SHA256 {
			return Candidate{}, fmt.Errorf("backup payload for %s does not match manifest hash", entry.Path)
		}
		candidate.SHA256 = backupHash
		candidate.backupSnapshot = backupHash
		targetInfo, err := root.Lstat(targetName)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				candidate.Status = StatusReady
				setRestoreOutcome(&candidate, "restore_target_directory_missing", "Target directory is missing.")
				return candidate, nil
			}
			return Candidate{}, err
		}
		if err := captureRestoreCurrentPin(root, targetName, &candidate, targetInfo); err != nil {
			return Candidate{}, err
		}
		snapshot, err := snapshotRestoreTarget(root, targetName, nil)
		if err != nil {
			return Candidate{}, err
		}
		candidate.currentSnapshot = snapshot
		candidate.CurrentSHA256 = snapshot
		if targetInfo.IsDir() && targetInfo.Mode()&os.ModeSymlink == 0 {
			currentHash, err := restoreDirectoryDigestRoot(root, targetName)
			if err != nil {
				return Candidate{}, err
			}
			candidate.CurrentSHA256 = currentHash
			switch {
			case currentHash == backupHash:
				candidate.Status = StatusAlreadyRestored
				setRestoreOutcome(&candidate, "restore_target_directory_already_restored", "Target directory already matches the backup.")
				return candidate, nil
			case candidate.matchesPostHash(currentHash):
				candidate.Status = StatusReady
				setRestoreOutcome(&candidate, "restore_target_directory_matches_post_change", "Target still matches the post-change directory captured with the backup.")
				return candidate, nil
			}
		}
		candidate.Status = StatusConflict
		setRestoreOutcome(&candidate, "restore_target_directory_conflict", "Target exists and requires interactive directory restore.")
		return candidate, nil
	}
	if entry.Symlink {
		backupBody, err := readRestoreBackupFile(candidate, maxSymlinkMarkerSize)
		if err != nil {
			return Candidate{}, fmt.Errorf("backup payload for %s is not a bounded regular symlink marker: %w", entry.Path, err)
		}
		backupHash := bytesSHA256(backupBody)
		if entry.SHA256 != "" && backupHash != entry.SHA256 {
			return Candidate{}, fmt.Errorf("backup payload for %s does not match manifest hash", entry.Path)
		}
		candidate.backupSnapshot = backupHash
		if info, statErr := root.Lstat(targetName); statErr == nil {
			if err := captureRestoreCurrentPin(root, targetName, &candidate, info); err != nil {
				return Candidate{}, err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return Candidate{}, statErr
		}
		return buildSymlinkCandidate(root, candidate, targetName, backupBody)
	}
	if entry.Size > maxRestoreRegularPayloadSize {
		return Candidate{}, fmt.Errorf("backup payload for %s is larger than %d bytes", entry.Path, maxRestoreRegularPayloadSize)
	}
	backupBody, err := readRestoreBackupFile(candidate, maxRestoreRegularPayloadSize)
	if err != nil {
		return Candidate{}, fmt.Errorf("backup payload for %s is not a bounded regular file: %w", entry.Path, err)
	}
	backupHash := bytesSHA256(backupBody)
	if entry.SHA256 != "" && backupHash != entry.SHA256 {
		return Candidate{}, fmt.Errorf("backup payload for %s does not match manifest hash", entry.Path)
	}
	if entry.Size > 0 && int64(len(backupBody)) != entry.Size {
		return Candidate{}, fmt.Errorf("backup payload for %s does not match manifest size", entry.Path)
	}
	if candidate.SHA256 == "" {
		candidate.SHA256 = backupHash
	}
	candidate.backupSnapshot = backupHash
	info, err := root.Lstat(targetName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			candidate.Status = StatusReady
			setRestoreOutcome(&candidate, "restore_target_file_missing", "Target file is missing.")
			return candidate, nil
		}
		return Candidate{}, err
	}
	if err := captureRestoreCurrentPin(root, targetName, &candidate, info); err != nil {
		return Candidate{}, err
	}
	if !info.Mode().IsRegular() {
		snapshot, err := snapshotRestoreTarget(root, targetName, nil)
		if err != nil {
			return Candidate{}, err
		}
		candidate.currentSnapshot = snapshot
		candidate.Status = StatusConflict
		setRestoreOutcome(&candidate, "restore_target_type_conflict", "Target exists with a different filesystem type.")
		return candidate, nil
	}
	if opts.hooks != nil && opts.hooks.beforeCurrentRegularRead != nil {
		opts.hooks.beforeCurrentRegularRead(root, targetName)
	}
	currentBody, err := backup.ReadRootRegularFileBounded(root, targetName, maxRestoreRegularPayloadSize)
	if err != nil {
		return Candidate{}, err
	}
	current := bytesSHA256(currentBody)
	candidate.currentSnapshot = "file:" + current
	candidate.CurrentSHA256 = current
	switch {
	case current == backupHash:
		candidate.Status = StatusAlreadyRestored
		setRestoreOutcome(&candidate, "restore_target_already_restored", "Target already matches the backup.")
	case candidate.matchesPostHash(current):
		candidate.Status = StatusReady
		setRestoreOutcome(&candidate, "restore_target_matches_post_change", "Target still matches the post-change state captured with the backup.")
	default:
		candidate.Status = StatusConflict
		setRestoreOutcome(&candidate, "restore_target_changed", "Target changed after the backup was captured.")
		if len(currentBody) <= maxRestoreDiffSize && len(backupBody) <= maxRestoreDiffSize {
			candidate.Diff = textdiff.Unified(candidate.Path, "current", "backup", string(currentBody), string(backupBody))
		}
	}
	return candidate, nil
}

func captureRestoreCandidatePins(projectRoot *os.Root, candidate *Candidate) error {
	if projectRoot == nil || candidate == nil {
		return errors.New("restore candidate pin capture requires a project root and candidate")
	}
	if candidate.backupInfo == nil {
		if err := captureRestoreBackupPins(candidate); err != nil {
			return err
		}
	}
	if !candidate.Absent {
		if _, err := prepareBackupPayload(*candidate, "", false); err != nil {
			return err
		}
	}
	if candidate.currentSnapshot == "" {
		return nil
	}
	name, err := candidateRootName(*candidate)
	if err != nil {
		return err
	}
	before, err := projectRoot.Lstat(name)
	if err != nil {
		return err
	}
	if candidate.currentInfo != nil {
		return verifyRestoreTargetIdentity(projectRoot, name, *candidate)
	}
	return captureRestoreCurrentPin(projectRoot, name, candidate, before)
}

func captureRestoreBackupPins(candidate *Candidate) error {
	if candidate == nil || candidate.Absent {
		return nil
	}
	if candidate.Directory {
		var tree *restorePayloadTreePin
		var err error
		if candidate.backupProductRoot != nil {
			tree, err = captureRestorePayloadTreeRoot(candidate.backupProductRoot, candidate.backupProductRel)
		} else {
			tree, err = captureRestorePayloadTree(candidate.BackupPath)
		}
		if err != nil {
			return fmt.Errorf("capturing backup payload tree identity for %s: %w", candidate.Path, err)
		}
		candidate.backupTree = tree
		candidate.backupInfo = tree.info
		return nil
	}
	var info fs.FileInfo
	var pin *os.File
	var err error
	if candidate.backupProductRoot != nil {
		info, pin, err = pinRootRestoreEntry(candidate.backupProductRoot, candidate.backupProductRel)
	} else {
		info, pin, err = pinAmbientRestoreEntry(candidate.BackupPath)
	}
	if err != nil {
		return fmt.Errorf("capturing backup payload identity for %s: %w", candidate.Path, err)
	}
	candidate.backupInfo = info
	candidate.backupPin = pin
	return nil
}

func pinRootRestoreEntry(root *os.Root, name string) (fs.FileInfo, *os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errors.New("rooted restore payload is not a regular file")
	}
	pin, err := openRestoreRegularPin(root, name)
	if err != nil {
		return nil, nil, err
	}
	opened, pinErr := pin.Stat()
	after, statErr := root.Lstat(name)
	if pinErr != nil || statErr != nil || !opened.Mode().IsRegular() || !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		_ = pin.Close()
		return nil, nil, errors.Join(errors.New("rooted restore payload changed while its identity was pinned"), pinErr, statErr)
	}
	return after, pin, nil
}

func captureRestoreCurrentPin(projectRoot *os.Root, name string, candidate *Candidate, before fs.FileInfo) error {
	if projectRoot == nil || candidate == nil || before == nil {
		return errors.New("restore target pin capture requires a root, candidate, and identity")
	}
	pin, err := openRestoreIdentityPin(projectRoot, name)
	if err != nil {
		return err
	}
	if pin != nil {
		opened, pinErr := pin.Stat()
		if pinErr != nil || opened.Mode().Type() != before.Mode().Type() || !os.SameFile(before, opened) {
			_ = pin.Close()
			return errors.Join(fmt.Errorf("restore target %s changed while its plan identity was pinned", candidate.Path), pinErr)
		}
	}
	after, statErr := projectRoot.Lstat(name)
	if statErr != nil || !sameRestoreIdentity(before, after) {
		if pin != nil {
			_ = pin.Close()
		}
		return errors.Join(fmt.Errorf("restore target %s changed while its exact plan identity was captured", candidate.Path), statErr)
	}
	candidate.currentInfo = before
	candidate.currentPin = pin
	return nil
}

func pinAmbientRestoreEntry(path string) (fs.FileInfo, *os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("restore payload is not a regular file: %s", path)
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	pin, pinErr := openRestoreRegularPin(parent, filepath.Base(path))
	closeErr := parent.Close()
	if pinErr != nil || closeErr != nil {
		if pin != nil {
			_ = pin.Close()
		}
		return nil, nil, errors.Join(pinErr, closeErr)
	}
	opened, err := pin.Stat()
	after, statErr := os.Lstat(path)
	if err != nil || statErr != nil || !opened.Mode().IsRegular() || !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		_ = pin.Close()
		return nil, nil, errors.Join(errors.New("restore payload changed while its identity was pinned"), err, statErr)
	}
	return after, pin, nil
}

func sameRestoreIdentity(expected fs.FileInfo, current fs.FileInfo) bool {
	return expected != nil && current != nil && expected.Mode().Type() == current.Mode().Type() && os.SameFile(expected, current)
}

func verifyRestoreBackupFileIdentity(candidate Candidate) (fs.FileInfo, error) {
	if candidate.backupPin == nil {
		return nil, errors.New("retained regular backup payload descriptor is required")
	}
	current, err := statRestoreBackupPayload(candidate)
	if err != nil {
		return nil, fmt.Errorf("backup payload identity for %s changed since restore plan: %w", candidate.Path, err)
	}
	if !current.Mode().IsRegular() || !sameRestoreIdentity(candidate.backupInfo, current) {
		return nil, fmt.Errorf("backup payload identity for %s changed since restore plan", candidate.Path)
	}
	pinned, pinErr := candidate.backupPin.Stat()
	if pinErr != nil || !pinned.Mode().IsRegular() || !sameRestoreIdentity(candidate.backupInfo, pinned) || !sameRestoreIdentity(current, pinned) {
		return nil, errors.Join(fmt.Errorf("backup payload identity for %s changed since restore plan", candidate.Path), pinErr)
	}
	return current, nil
}

func statRestoreBackupPayload(candidate Candidate) (fs.FileInfo, error) {
	if candidate.backupProductRoot != nil {
		return candidate.backupProductRoot.Lstat(candidate.backupProductRel)
	}
	return os.Lstat(candidate.BackupPath)
}

func readRestoreBackupFile(candidate Candidate, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, fmt.Errorf("invalid negative backup payload limit for %s", candidate.Path)
	}
	before, err := verifyRestoreBackupFileIdentity(candidate)
	if err != nil {
		return nil, err
	}
	if before.Size() > limit {
		return nil, fmt.Errorf("backup payload for %s is larger than %d bytes", candidate.Path, limit)
	}
	body, err := io.ReadAll(io.NewSectionReader(candidate.backupPin, 0, limit+1))
	if err != nil {
		return nil, fmt.Errorf("backup payload for %s changed since restore plan: %w", candidate.Path, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("backup payload for %s is larger than %d bytes", candidate.Path, limit)
	}
	after, err := verifyRestoreBackupFileIdentity(candidate)
	if err != nil {
		return nil, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(body)) != after.Size() {
		return nil, fmt.Errorf("backup payload identity for %s changed while it was read", candidate.Path)
	}
	return body, nil
}

func verifyRestorePayloadTree(candidate Candidate) error {
	tree := candidate.backupTree
	if tree == nil || tree.root == nil {
		return errors.New("retained backup payload tree is required")
	}
	ambient, err := statRestoreBackupPayload(candidate)
	opened, rootErr := tree.root.Stat(".")
	if err != nil || rootErr != nil || !ambient.IsDir() || ambient.Mode()&os.ModeSymlink != 0 || !sameRestoreIdentity(tree.info, ambient) || !sameRestoreIdentity(tree.info, opened) {
		return errors.Join(fmt.Errorf("backup payload tree identity for %s changed since restore plan", candidate.Path), err, rootErr)
	}
	seen := make(map[string]struct{}, len(tree.entries))
	err = backup.WalkRootDirectoryWithLimits(tree.root, ".", restoreRootWalkLimits(), func(_ *os.Root, _ string, walkPath string, info fs.FileInfo) error {
		if walkPath == "." {
			if !sameRestoreIdentity(tree.info, info) {
				return errors.New("payload root identity changed")
			}
			return nil
		}
		key := filepath.ToSlash(walkPath)
		expected, ok := tree.entries[key]
		if !ok {
			return fmt.Errorf("unexpected payload entry %s appeared", key)
		}
		if !sameRestoreIdentity(expected.info, info) {
			return fmt.Errorf("payload entry %s identity changed", key)
		}
		if expected.pin != nil {
			pinned, pinErr := expected.pin.Stat()
			if pinErr != nil || !sameRestoreIdentity(expected.info, pinned) || !sameRestoreIdentity(info, pinned) {
				return errors.Join(fmt.Errorf("payload entry %s identity changed", key), pinErr)
			}
		}
		seen[key] = struct{}{}
		return nil
	})
	if err != nil || len(seen) != len(tree.entries) {
		return errors.Join(fmt.Errorf("backup payload tree identity for %s changed since restore plan", candidate.Path), err)
	}
	ambientAfter, ambientErr := statRestoreBackupPayload(candidate)
	openedAfter, openedErr := tree.root.Stat(".")
	if ambientErr != nil || openedErr != nil || !sameRestoreIdentity(tree.info, ambientAfter) || !sameRestoreIdentity(tree.info, openedAfter) {
		return errors.Join(fmt.Errorf("backup payload tree identity for %s changed since restore plan", candidate.Path), ambientErr, openedErr)
	}
	return nil
}

func validateRestoreDirectoryModes(candidate Candidate) error {
	if !candidate.Directory || !candidate.backupModeKnown || candidate.backupTree == nil || candidate.backupTree.root == nil {
		return fmt.Errorf("backup entry for %s is missing complete directory mode metadata", candidate.Path)
	}
	expected := map[string]struct{}{".": {}}
	for rel, entry := range candidate.backupTree.entries {
		if entry.info.IsDir() || entry.info.Mode().IsRegular() {
			expected[filepath.ToSlash(rel)] = struct{}{}
		}
	}
	if len(candidate.backupTreeModes) != len(expected) {
		return fmt.Errorf("backup entry for %s is missing complete directory mode metadata", candidate.Path)
	}
	for rel := range expected {
		if _, ok := candidate.backupTreeModes[rel]; !ok {
			return fmt.Errorf("backup entry for %s is missing directory mode metadata for %s", candidate.Path, rel)
		}
	}
	return nil
}

func captureRestorePayloadTree(path string) (*restorePayloadTreePin, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("restore directory payload is not a directory or is a symlink: %s", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		_ = root.Close()
		return nil, errors.Join(errors.New("restore directory payload changed while its root identity was pinned"), err)
	}
	tree := &restorePayloadTreePin{root: root, info: opened, entries: map[string]restorePayloadEntryPin{}}
	fail := func(cause error) (*restorePayloadTreePin, error) {
		return nil, errors.Join(cause, closeRestorePayloadTree(tree))
	}
	err = backup.WalkRootDirectoryWithLimits(root, ".", restoreRootWalkLimits(), func(accessRoot *os.Root, accessName string, walkPath string, info fs.FileInfo) error {
		if walkPath == "." {
			return nil
		}
		pin, err := openRestoreIdentityPin(accessRoot, accessName)
		if err != nil {
			return err
		}
		if pin != nil {
			opened, pinErr := pin.Stat()
			if pinErr != nil || opened.Mode().Type() != info.Mode().Type() || !os.SameFile(info, opened) {
				_ = pin.Close()
				return errors.Join(fmt.Errorf("restore payload entry %s changed while its identity was pinned", walkPath), pinErr)
			}
		}
		tree.entries[filepath.ToSlash(walkPath)] = restorePayloadEntryPin{info: info, pin: pin}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return tree, nil
}

func captureRestorePayloadTreeRoot(parent *os.Root, name string) (*restorePayloadTreePin, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("rooted restore directory payload is not a real directory")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	after, statErr := parent.Lstat(name)
	if err != nil || statErr != nil || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		_ = root.Close()
		return nil, errors.Join(errors.New("rooted restore directory payload changed while its identity was pinned"), err, statErr)
	}
	tree := &restorePayloadTreePin{root: root, info: opened, entries: map[string]restorePayloadEntryPin{}}
	fail := func(cause error) (*restorePayloadTreePin, error) {
		return nil, errors.Join(cause, closeRestorePayloadTree(tree))
	}
	err = backup.WalkRootDirectoryWithLimits(root, ".", restoreRootWalkLimits(), func(accessRoot *os.Root, accessName string, walkPath string, info fs.FileInfo) error {
		if walkPath == "." {
			return nil
		}
		pin, err := openRestoreIdentityPin(accessRoot, accessName)
		if err != nil {
			return err
		}
		if pin != nil {
			opened, pinErr := pin.Stat()
			if pinErr != nil || opened.Mode().Type() != info.Mode().Type() || !os.SameFile(info, opened) {
				_ = pin.Close()
				return errors.Join(fmt.Errorf("rooted restore payload entry %s changed while its identity was pinned", walkPath), pinErr)
			}
		}
		tree.entries[filepath.ToSlash(walkPath)] = restorePayloadEntryPin{info: info, pin: pin}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	after, statErr = parent.Lstat(name)
	if statErr != nil || !os.SameFile(before, after) {
		return fail(errors.Join(errors.New("rooted restore directory payload changed after pinning"), statErr))
	}
	return tree, nil
}

func closeRestorePayloadTree(tree *restorePayloadTreePin) error {
	if tree == nil {
		return nil
	}
	var result error
	for path, entry := range tree.entries {
		if entry.pin != nil {
			result = errors.Join(result, entry.pin.Close())
			entry.pin = nil
			tree.entries[path] = entry
		}
	}
	if tree.root != nil {
		result = errors.Join(result, tree.root.Close())
		tree.root = nil
	}
	return result
}

func closeRestoreCandidatePins(candidate *Candidate) error {
	if candidate == nil {
		return nil
	}
	var result error
	if candidate.currentPin != nil {
		result = errors.Join(result, candidate.currentPin.Close())
		candidate.currentPin = nil
	}
	if candidate.backupPin != nil {
		result = errors.Join(result, candidate.backupPin.Close())
		candidate.backupPin = nil
	}
	result = errors.Join(result, closeRestorePayloadTree(candidate.backupTree))
	candidate.backupTree = nil
	return result
}

func closeRestorePlanPins(plan *Plan) error {
	if plan == nil {
		return nil
	}
	var result error
	for index := range plan.Candidates {
		result = errors.Join(result, closeRestoreCandidatePins(&plan.Candidates[index]))
	}
	return result
}

func buildAbsentCandidate(root *os.Root, candidate Candidate, targetName string, hooks *restoreHooks) (Candidate, error) {
	info, err := root.Lstat(targetName)
	if errors.Is(err, os.ErrNotExist) {
		candidate.Status = StatusAlreadyRestored
		setRestoreOutcome(&candidate, "restore_created_target_absent", "Target created by the operation is already absent.")
		return candidate, nil
	}
	if err != nil {
		return Candidate{}, err
	}
	snapshot, err := snapshotRestoreTarget(root, targetName, hooks)
	if err != nil {
		return Candidate{}, err
	}
	candidate.currentSnapshot = snapshot
	candidate.CurrentSHA256 = snapshot
	if info.Mode().IsRegular() && (candidate.PostSHA256 != "" || candidate.PendingPostSHA256 != "") {
		if hooks != nil && hooks.beforeCurrentRegularRead != nil {
			hooks.beforeCurrentRegularRead(root, targetName)
		}
		body, err := backup.ReadRootRegularFileBounded(root, targetName, maxRestoreRegularPayloadSize)
		if err != nil {
			return Candidate{}, err
		}
		current := bytesSHA256(body)
		candidate.CurrentSHA256 = current
		if candidate.matchesPostHash(current) {
			candidate.Status = StatusReady
			setRestoreOutcome(&candidate, "restore_created_target_matches_post_change", "Target still matches the file created by the operation.")
			return candidate, nil
		}
	}
	candidate.Status = StatusConflict
	setRestoreOutcome(&candidate, "restore_created_target_changed", "Target created by the operation changed after the backup was captured.")
	return candidate, nil
}

func targetRootForEntry(selectedRoot string, entryRoot string) (string, error) {
	selectedRoot, err := filepath.Abs(selectedRoot)
	if err != nil {
		return "", err
	}
	selectedRoot = filepath.Clean(selectedRoot)
	if strings.TrimSpace(entryRoot) == "" {
		return selectedRoot, nil
	}
	entryRoot, err = filepath.Abs(entryRoot)
	if err != nil {
		return "", err
	}
	entryRoot = filepath.Clean(entryRoot)
	if entryRoot != selectedRoot {
		return "", fmt.Errorf("backup entry root %q is outside selected root %q", entryRoot, selectedRoot)
	}
	return selectedRoot, nil
}

func buildSymlinkCandidate(root *os.Root, candidate Candidate, targetName string, backupBody []byte) (Candidate, error) {
	backupTarget, err := decodeSymlinkMarker(backupBody)
	if err != nil {
		return Candidate{}, fmt.Errorf("backup payload for %s has an invalid symlink marker: %w", candidate.Path, err)
	}
	info, err := root.Lstat(targetName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			candidate.Status = StatusReady
			setRestoreOutcome(&candidate, "restore_target_symlink_missing", "Target symlink is missing.")
			return candidate, nil
		}
		if isNotSymlink(err) {
			candidate.Status = StatusConflict
			setRestoreOutcome(&candidate, "restore_target_symlink_conflict", "Target exists but is not the backed-up symlink.")
			return candidate, nil
		}
		return Candidate{}, err
	}
	snapshot, err := snapshotRestoreTarget(root, targetName, nil)
	if err != nil {
		return Candidate{}, err
	}
	candidate.currentSnapshot = snapshot
	if info.Mode()&os.ModeSymlink == 0 {
		candidate.Status = StatusConflict
		setRestoreOutcome(&candidate, "restore_target_symlink_conflict", "Target exists but is not the backed-up symlink.")
		return candidate, nil
	}
	currentTarget, err := root.Readlink(targetName)
	if err != nil {
		return Candidate{}, err
	}
	if currentTarget == backupTarget {
		candidate.CurrentSHA256 = restoreSnapshotString(currentTarget)
		candidate.Status = StatusAlreadyRestored
		setRestoreOutcome(&candidate, "restore_target_symlink_already_restored", "Target symlink already matches the backup.")
		return candidate, nil
	}
	if candidate.matchesPostHash(restoreSnapshotString(currentTarget)) {
		candidate.CurrentSHA256 = restoreSnapshotString(currentTarget)
		candidate.Status = StatusReady
		setRestoreOutcome(&candidate, "restore_target_symlink_matches_post_change", "Target still matches the post-change symlink captured with the backup.")
		return candidate, nil
	}
	candidate.CurrentSHA256 = restoreSnapshotString(currentTarget)
	candidate.Status = StatusConflict
	setRestoreOutcome(&candidate, "restore_target_symlink_changed", "Target symlink changed after the backup was captured.")
	return candidate, nil
}

func decodeSymlinkMarker(body []byte) (string, error) {
	value := string(body)
	if !strings.HasSuffix(value, "\n") {
		return "", errors.New("symlink marker is missing its trailing delimiter")
	}
	return strings.TrimSuffix(value, "\n"), nil
}

func bytesSHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func setRestoreOutcome(candidate *Candidate, code string, message string) {
	if candidate == nil {
		return
	}
	candidate.Code = code
	candidate.Message = message
}

func (candidate Candidate) matchesPostHash(hash string) bool {
	return hash != "" && (hash == candidate.PostSHA256 || hash == candidate.PendingPostSHA256)
}

func restoreDirectoryDigestRoot(root *os.Root, directory string) (string, error) {
	return backup.RootDirSHA256WithLimits(root, directory, restoreDirectoryLimits())
}

func restoreDirectoryLimits() backup.DirectoryLimits {
	return backup.DirectoryLimits{
		MaxEntries:   maxRestoreDirectoryEntries + 1,
		MaxBytes:     maxRestoreDirectoryBytes,
		MaxFileBytes: maxRestoreRegularPayloadSize,
	}
}

func restoreRootWalkLimits() backup.RootWalkLimits {
	return backup.RootWalkLimits{MaxEntries: maxRestoreDirectoryEntries + 1}
}

func applyResolutionsWithPinnedRoot(ctx context.Context, opts Options, plan *Plan, pending []pendingResolution, root *os.Root) (resultReport *Report, resultErr error) {
	if root == nil {
		return nil, errors.New("pinned restore root is required")
	}
	opened, err := root.Stat(".")
	if err != nil || plan == nil || plan.rootSnapshot.info == nil || !os.SameFile(plan.rootSnapshot.info, opened) {
		return nil, errors.Join(errors.New("pinned restore root no longer matches the reviewed plan"), err)
	}
	if err := validateRestoreRootSnapshot(plan.rootSnapshot); err != nil {
		return nil, err
	}
	locks, err := acquireApplyLocks(opts, plan)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, locks.Release()) }()
	lockedProjectRoot, err := locks.BorrowProjectRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	if err := verifyRestoreLockRoot(lockedProjectRoot, root, plan); err != nil {
		return nil, err
	}
	root = lockedProjectRoot
	productRoot := opts.ProductRoot
	if productRoot == nil {
		productRoot, err = locks.BorrowProductRoot()
		if err != nil {
			return nil, err
		}
	}
	if err := preflightRestoreCandidates(ctx, plan, pending, root); err != nil {
		return nil, err
	}
	if err := preflightAtomicDetaches(plan, pending, opts.hooks, root); err != nil {
		return nil, err
	}
	storeOptions := backup.StoreOptions{
		ProjectRoot:     opts.Root,
		HomeDir:         opts.HomeDir,
		ThreadpointHome: opts.ThreadpointHome,
		BackupDir:       opts.BackupDir,
		Operation:       restoreOperation,
		Now:             opts.Now,
	}
	homePath, err := restoreProductHomePath(opts)
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

	report := &Report{Root: opts.Root, Applied: true, Plan: plan}
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		currentLockedRoot, err := locks.BorrowProjectRoot(opts.Root)
		if err != nil {
			return report, err
		}
		if err := verifyRestoreLockRoot(currentLockedRoot, root, plan); err != nil {
			return report, err
		}
		resolution := Resolution{Path: item.candidate.Path, Decision: item.decision, Status: item.candidate.Status}
		switch item.decision {
		case DecisionSkip:
			resolution.Skipped = true
		case DecisionRestore:
			backup, recovery, err := restoreBackup(root, plan.rootSnapshot, store, item.candidate, "", false, opts.hooks)
			resolution.Backup = backup
			resolution.Recovery = recovery
			if recovery != "" {
				resolution.Messages = append(resolution.Messages, "retained the detached original at "+recovery+" so later writes through pre-existing handles remain recoverable")
			}
			if err != nil {
				report.Resolutions = append(report.Resolutions, resolution)
				return report, err
			}
			resolution.Restored = true
		case DecisionEdit:
			backup, recovery, err := restoreBackup(root, plan.rootSnapshot, store, item.candidate, item.content, item.edited, opts.hooks)
			resolution.Backup = backup
			resolution.Recovery = recovery
			if recovery != "" {
				resolution.Messages = append(resolution.Messages, "retained the detached original at "+recovery+" so later writes through pre-existing handles remain recoverable")
			}
			if err != nil {
				report.Resolutions = append(report.Resolutions, resolution)
				return report, err
			}
			resolution.Restored = true
			resolution.Messages = append(resolution.Messages, "restored edited backup content")
		default:
			return nil, fmt.Errorf("unknown restore decision %q", item.decision)
		}
		report.Resolutions = append(report.Resolutions, resolution)
	}
	return report, nil
}

func verifyRestoreLockRoot(lockedRoot, reviewedRoot *os.Root, plan *Plan) error {
	if lockedRoot == nil || reviewedRoot == nil || plan == nil || plan.rootSnapshot.info == nil {
		return errors.New("restore lock, reviewed root, and plan identity are required")
	}
	lockedInfo, lockedErr := lockedRoot.Stat(".")
	reviewedInfo, reviewedErr := reviewedRoot.Stat(".")
	if lockedErr != nil || reviewedErr != nil || !os.SameFile(lockedInfo, reviewedInfo) || !os.SameFile(lockedInfo, plan.rootSnapshot.info) {
		return errors.Join(errors.New("restore plan is not bound to the locked project generation"), lockedErr, reviewedErr)
	}
	return validateRestoreRootSnapshot(plan.rootSnapshot)
}

// preflightRestoreCandidates validates every selected backup payload, edited
// output, and current target before capability probes create private entries
// and before restore creates parents, backup state, prepared payloads, or
// canonical mutations. Each candidate is revalidated at its own transaction
// boundary as well.
func preflightRestoreCandidates(ctx context.Context, plan *Plan, pending []pendingResolution, root *os.Root) error {
	if plan == nil || root == nil {
		return errors.New("pinned restore plan and root are required")
	}
	if err := validateRestoreRootSnapshot(plan.rootSnapshot); err != nil {
		return err
	}
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.decision != DecisionRestore && item.decision != DecisionEdit {
			continue
		}
		if filepath.Clean(item.candidate.Root) != filepath.Clean(plan.Root) {
			return fmt.Errorf("restore candidate root %s does not match planned root %s", item.candidate.Root, plan.Root)
		}
		if _, err := prepareBackupPayload(item.candidate, item.content, item.edited); err != nil {
			return err
		}
		name, err := candidateRootName(item.candidate)
		if err != nil {
			return err
		}
		if err := safefs.RejectRootSymlinkAncestors(root, filepath.Dir(name)); err != nil {
			return err
		}
		if item.candidate.currentSnapshot == "" {
			if _, err := root.Lstat(name); err == nil {
				return fmt.Errorf("restore target %s appeared since restore plan", item.candidate.Path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := verifyRestoreTargetIdentity(root, name, item.candidate); err != nil {
			return err
		}
	}
	return validateRestoreRootSnapshot(plan.rootSnapshot)
}

func candidateUsesNoReplace(candidate Candidate) bool {
	if candidate.currentSnapshot != "" {
		return true
	}
	return !candidate.Absent && !candidate.Symlink
}

func pendingRequiresNoReplace(pending []pendingResolution) bool {
	for _, item := range pending {
		if (item.decision == DecisionRestore || item.decision == DecisionEdit) && candidateUsesNoReplace(item.candidate) {
			return true
		}
	}
	return false
}

func preflightAtomicDetaches(plan *Plan, pending []pendingResolution, hooks *restoreHooks, root *os.Root) error {
	if !pendingRequiresNoReplace(pending) {
		return nil
	}
	if !safefs.RenameRootNoReplaceSupported() {
		return fmt.Errorf("%w: this restore requires an atomic destination-no-replace filesystem transaction", safefs.ErrRenameNoReplaceUnsupported)
	}
	probedParents := make(map[string]struct{})
	for _, item := range pending {
		if (item.decision != DecisionRestore && item.decision != DecisionEdit) || !candidateUsesNoReplace(item.candidate) {
			continue
		}
		parent, err := openRestoreNoReplaceProbeParent(root, plan.rootSnapshot, item.candidate)
		if err != nil {
			return err
		}
		if _, alreadyProbed := probedParents[parent.path]; alreadyProbed {
			if closeErr := parent.root.Close(); closeErr != nil {
				return closeErr
			}
			continue
		}
		probedParents[parent.path] = struct{}{}
		probe := safefs.ProbeRenameRootNoReplace
		if hooks != nil && hooks.probeAtomicDetach != nil {
			probe = hooks.probeAtomicDetach
		}
		probeErr := probe(parent.root)
		closeErr := parent.root.Close()
		if probeErr != nil || closeErr != nil {
			return fmt.Errorf("preflight atomic no-replace transaction for %s beneath %s: %w", item.candidate.Path, filepath.ToSlash(parent.path), errors.Join(probeErr, closeErr))
		}
	}
	return nil
}

type restoreNoReplaceProbeParent struct {
	root *os.Root
	path string
}

// openRestoreNoReplaceProbeParent pins the nearest existing real ancestor of
// the desired target parent. It performs no MkdirAll: all capability probes
// finish before restore state or project directories are created.
func openRestoreNoReplaceProbeParent(project *os.Root, rootSnapshot restoreRootSnapshot, candidate Candidate) (*restoreNoReplaceProbeParent, error) {
	if err := validateRestoreRootSnapshot(rootSnapshot); err != nil {
		return nil, err
	}
	name, err := candidateRootName(candidate)
	if err != nil {
		return nil, err
	}
	current := filepath.Clean(filepath.Dir(name))
	if err := safefs.RejectRootSymlinkAncestors(project, current); err != nil {
		return nil, err
	}
	for {
		var info fs.FileInfo
		if current == "." {
			info, err = project.Stat(".")
		} else {
			info, err = project.Lstat(current)
		}
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("restore no-replace probe parent is not a real directory: %s", filepath.ToSlash(current))
			}
			var pinned *os.Root
			if current == "." {
				pinned, err = os.OpenRoot(rootSnapshot.physicalRoot)
			} else {
				pinned, err = project.OpenRoot(current)
			}
			if err != nil {
				return nil, err
			}
			opened, statErr := pinned.Stat(".")
			if statErr != nil || !os.SameFile(info, opened) {
				_ = pinned.Close()
				return nil, errors.Join(errors.New("restore no-replace probe parent changed while it was opened"), statErr)
			}
			return &restoreNoReplaceProbeParent{root: pinned, path: current}, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if current == "." {
			return nil, err
		}
		current = filepath.Dir(current)
	}
}

func restoreBackup(projectRoot *os.Root, rootSnapshot restoreRootSnapshot, store *backup.Store, candidate Candidate, editedContent string, edited bool, hooks *restoreHooks) (backupResult string, recoveryResult string, returnErr error) {
	prepared, err := prepareBackupPayload(candidate, editedContent, edited)
	if err != nil {
		return "", "", err
	}
	parent, err := openRestoreTargetParent(projectRoot, rootSnapshot, candidate)
	if err != nil {
		return "", "", err
	}
	defer parent.root.Close()
	if err := verifyRestoreTargetUnchanged(parent, candidate); err != nil {
		return "", "", err
	}
	var preparedDirectory *preparedRestoreDirectory
	if prepared.directory {
		preparedDirectory, err = prepareRestoreDirectory(parent, candidate, hooks)
		if err != nil {
			return "", "", err
		}
		defer func() {
			if preparedDirectory.root != nil && hooks != nil && hooks.beforePreparedCleanup != nil {
				hooks.beforePreparedCleanup(parent.root, preparedDirectory.name)
			}
			if cleanupErr := discardPreparedDirectory(parent, preparedDirectory); cleanupErr != nil {
				returnErr = errors.Join(returnErr, cleanupErr)
			}
		}()
	}

	var overwriteEntry *backup.Entry
	existed := candidate.currentSnapshot != ""
	if existed {
		if hooks != nil && hooks.beforeOverwriteBackup != nil {
			hooks.beforeOverwriteBackup(parent.root, parent.base)
		}
		overwriteEntry, err = backupRootedCurrent(store, parent, candidate)
		if err != nil {
			return "", "", err
		}
		afterBackup, err := snapshotRestoreTarget(parent.root, parent.base, nil)
		if err != nil || afterBackup != candidate.currentSnapshot {
			discardErr := store.Discard("restore-overwrite", candidate.Path)
			cause := fmt.Errorf("restore target %s changed while its overwrite backup was captured", candidate.Path)
			if discardErr != nil {
				cause = errors.Join(cause, fmt.Errorf("discard tentative overwrite backup: %w", discardErr))
				return overwriteEntry.BackupPath, "", cause
			}
			return "", "", cause
		}
	} else if !candidate.Absent {
		overwriteEntry, err = store.RecordAbsent(candidate.Path, "restore-overwrite")
		if err != nil {
			return "", "", err
		}
	}

	overwriteBackup := ""
	if overwriteEntry != nil {
		overwriteBackup = overwriteEntry.BackupPath
		if prepared.postHash != "" {
			if err := store.SetPendingPostHashValue(candidate.Path, "restore-overwrite", prepared.postHash); err != nil {
				if discardErr := store.Discard("restore-overwrite", candidate.Path); discardErr != nil {
					return overwriteBackup, "", errors.Join(err, fmt.Errorf("discard tentative overwrite backup: %w", discardErr))
				}
				return "", "", err
			}
		}
	}
	if hooks != nil && hooks.beforeDetach != nil {
		hooks.beforeDetach(parent.root, parent.base)
	}
	var detached *detachedRestoreTarget
	if existed {
		detached, err = detachRestoreTarget(parent, candidate, hooks, store)
	} else {
		if verifyErr := parent.verify(); verifyErr != nil {
			err = verifyErr
		} else if exists, existsErr := rootPathExists(parent.root, parent.base); existsErr != nil {
			err = existsErr
		} else if exists {
			err = fmt.Errorf("restore target %s appeared immediately before installation", candidate.Path)
		}
	}
	if err != nil {
		var retained *retainedDetachedRestoreError
		if overwriteEntry != nil && !errors.As(err, &retained) {
			if discardErr := store.Discard("restore-overwrite", candidate.Path); discardErr != nil {
				return overwriteBackup, "", errors.Join(err, fmt.Errorf("discard tentative overwrite backup: %w", discardErr))
			}
			overwriteBackup = ""
		}
		recoveryPath := ""
		if retained != nil {
			recoveryPath = retained.detached
		}
		return overwriteBackup, recoveryPath, err
	}
	recoveryPath := ""
	if detached != nil {
		recoveryPath = detached.recoveryPath
		defer detached.container.Close()
		defer func() { _ = detached.closeItemPin() }()
	}
	detachedActive := detached != nil
	restoreDetached := func(cause error) error {
		if !detachedActive {
			return cause
		}
		if refreshErr := refreshDetachedRecoveryPath(parent, detached, candidate, store); refreshErr != nil {
			cause = errors.Join(cause, refreshErr)
		}
		recoveryPath = detached.recoveryPath
		if hooks != nil && hooks.beforeRollback != nil {
			hooks.beforeRollback(parent.root, parent.base)
		}
		if rollbackErr := rollbackDetachedNoReplace(parent, candidate, detached, hooks); rollbackErr == nil {
			detachedActive = false
			cause = errors.Join(cause, finalizeDetachedRollback(parent, detached, candidate, store))
			recoveryPath = ""
			return fmt.Errorf("%w; original restore target was rolled back without replacement", cause)
		} else {
			return fmt.Errorf("%w; rollback did not overwrite a reappeared target: %w; original restore target %s", cause, rollbackErr, detached.retentionMessage())
		}
	}
	if candidate.Absent {
		if exists, err := rootPathExists(parent.root, parent.base); err != nil {
			return overwriteBackup, recoveryPath, restoreDetached(err)
		} else if exists {
			return overwriteBackup, recoveryPath, restoreDetached(fmt.Errorf("restore target %s reappeared during removal", candidate.Path))
		}
		if detachedActive {
			if hooks != nil && hooks.beforeDetachedDelete != nil {
				hooks.beforeDetachedDelete(detached.container, detached.itemName)
			}
			if err := verifyRetainedOriginal(parent, candidate, detached); err != nil {
				return overwriteBackup, recoveryPath, restoreDetached(err)
			}
			if hooks != nil && hooks.afterRecoveryValidation != nil {
				hooks.afterRecoveryValidation()
			}
			if err := parent.verify(); err != nil {
				return overwriteBackup, recoveryPath, restoreDetached(err)
			}
			if err := refreshDetachedRecoveryPath(parent, detached, candidate, store); err != nil {
				return overwriteBackup, recoveryPath, restoreDetached(err)
			}
			recoveryPath = detached.recoveryPath
			detachedActive = false
		}
		if hooks != nil && hooks.afterDetachedDelete != nil {
			hooks.afterDetachedDelete(parent.root, parent.base)
		}
		if exists, err := rootPathExists(parent.root, parent.base); err != nil {
			return overwriteBackup, recoveryPath, err
		} else if exists {
			return overwriteBackup, recoveryPath, fmt.Errorf("restore target %s reappeared before removal completed", candidate.Path)
		}
		return overwriteBackup, recoveryPath, nil
	}

	if hooks != nil && hooks.beforeInstall != nil {
		hooks.beforeInstall(parent.root, parent.base)
	}
	if err := installPreparedBackup(parent, candidate, prepared, preparedDirectory, hooks); err != nil {
		if overwriteEntry != nil && !existed {
			if exists, existsErr := rootPathExists(parent.root, parent.base); existsErr == nil && !exists {
				if discardErr := store.Discard("restore-overwrite", candidate.Path); discardErr != nil {
					err = errors.Join(err, fmt.Errorf("discard tentative overwrite backup: %w", discardErr))
				} else {
					overwriteBackup = ""
				}
			}
		}
		return overwriteBackup, recoveryPath, restoreDetached(err)
	}
	if err := verifyInstalledBackup(parent, candidate, prepared); err != nil {
		return overwriteBackup, recoveryPath, restoreDetached(err)
	}
	if overwriteEntry != nil {
		var err error
		if prepared.postHash != "" {
			err = store.CommitPendingPostHashValue(candidate.Path, "restore-overwrite", prepared.postHash)
		} else {
			err = store.SetPostHashValue(candidate.Path, "restore-overwrite", "")
		}
		if err != nil {
			return overwriteBackup, recoveryPath, restoreDetached(err)
		}
	}
	if detachedActive {
		if hooks != nil && hooks.beforeDetachedDelete != nil {
			hooks.beforeDetachedDelete(detached.container, detached.itemName)
		}
		if err := verifyInstalledBackup(parent, candidate, prepared); err != nil {
			return overwriteBackup, recoveryPath, restoreDetached(err)
		}
		if err := verifyRetainedOriginal(parent, candidate, detached); err != nil {
			return overwriteBackup, recoveryPath, restoreDetached(err)
		}
		if hooks != nil && hooks.afterRecoveryValidation != nil {
			hooks.afterRecoveryValidation()
		}
		if err := parent.verify(); err != nil {
			return overwriteBackup, recoveryPath, restoreDetached(err)
		}
		if err := refreshDetachedRecoveryPath(parent, detached, candidate, store); err != nil {
			return overwriteBackup, recoveryPath, restoreDetached(err)
		}
		recoveryPath = detached.recoveryPath
		detachedActive = false
	}
	return overwriteBackup, recoveryPath, nil
}

func prepareBackupPayload(candidate Candidate, editedContent string, edited bool) (preparedBackupPayload, error) {
	if candidate.Absent {
		return preparedBackupPayload{}, nil
	}
	if candidate.Directory {
		if err := verifyRestorePayloadTree(candidate); err != nil {
			return preparedBackupPayload{}, err
		}
		digest, err := restoreDirectoryDigestRoot(candidate.backupTree.root, ".")
		if err != nil {
			return preparedBackupPayload{}, fmt.Errorf("backup payload for %s changed since restore plan: %w", candidate.Path, err)
		}
		if err := verifyRestorePayloadTree(candidate); err != nil {
			return preparedBackupPayload{}, err
		}
		if digest != candidate.backupSnapshot {
			return preparedBackupPayload{}, fmt.Errorf("backup payload for %s changed since restore plan", candidate.Path)
		}
		return preparedBackupPayload{directory: true, postHash: digest, mode: candidate.backupMode, modeKnown: candidate.backupModeKnown}, nil
	}
	limit := int64(maxRestoreRegularPayloadSize)
	if candidate.Symlink {
		limit = maxSymlinkMarkerSize
	}
	body, err := readRestoreBackupFile(candidate, limit)
	if err != nil {
		return preparedBackupPayload{}, err
	}
	if bytesSHA256(body) != candidate.backupSnapshot {
		return preparedBackupPayload{}, fmt.Errorf("backup payload for %s changed since restore plan", candidate.Path)
	}
	if candidate.Symlink {
		target, err := decodeSymlinkMarker(body)
		if err != nil {
			return preparedBackupPayload{}, fmt.Errorf("backup payload for %s changed since restore plan: %w", candidate.Path, err)
		}
		return preparedBackupPayload{symlinkTarget: target, postHash: restoreSnapshotString(target)}, nil
	}
	if edited {
		if len(editedContent) > maxRestoreRegularPayloadSize {
			return preparedBackupPayload{}, fmt.Errorf("edited restore payload for %s is larger than %d bytes", candidate.Path, maxRestoreRegularPayloadSize)
		}
		body = []byte(editedContent)
	}
	return preparedBackupPayload{body: body, postHash: bytesSHA256(body), mode: candidate.backupMode, modeKnown: candidate.backupModeKnown}, nil
}

func prepareRestoreDirectory(parent *restoreTargetParent, candidate Candidate, hooks *restoreHooks) (result *preparedRestoreDirectory, returnErr error) {
	if err := verifyRestorePayloadTree(candidate); err != nil {
		return nil, err
	}
	source := candidate.backupTree.root
	if hooks != nil && hooks.beforeDirectoryCopy != nil {
		hooks.beforeDirectoryCopy()
	}
	if err := verifyRestorePayloadTree(candidate); err != nil {
		return nil, err
	}
	name, err := makeRestoreTempDirectory(parent.root, ".threadpoint-restore-directory-")
	if err != nil {
		return nil, err
	}
	temporary, err := parent.root.OpenRoot(name)
	if err != nil {
		_ = parent.root.Remove(name)
		return nil, err
	}
	info, err := parent.root.Lstat(name)
	if err != nil {
		_ = temporary.Close()
		return nil, err
	}
	prepared := &preparedRestoreDirectory{name: name, root: temporary, info: info}
	ok := false
	defer func() {
		if !ok {
			if hooks != nil && hooks.beforePreparedCleanup != nil {
				hooks.beforePreparedCleanup(parent.root, prepared.name)
			}
			if cleanupErr := discardPreparedDirectory(parent, prepared); cleanupErr != nil {
				returnErr = errors.Join(returnErr, cleanupErr)
			}
		}
	}()
	if err := restoreDirectoryBackup(source, temporary, candidate.backupTreeModes); err != nil {
		return nil, err
	}
	if !candidate.backupModeKnown {
		return nil, fmt.Errorf("backup entry for %s is missing required mode metadata", candidate.Path)
	}
	if err := temporary.Chmod(".", candidate.backupMode.Perm()); err != nil {
		return nil, err
	}
	sourceDigest, sourceErr := restoreDirectoryDigestRoot(source, ".")
	preparedDigest, preparedErr := restoreDirectoryDigestRoot(temporary, ".")
	identityErr := verifyRestorePayloadTree(candidate)
	if sourceErr != nil || preparedErr != nil || identityErr != nil || sourceDigest != candidate.backupSnapshot || preparedDigest != candidate.backupSnapshot {
		return nil, errors.Join(fmt.Errorf("backup payload for %s changed while the restore directory was prepared", candidate.Path), sourceErr, preparedErr, identityErr)
	}
	ok = true
	return prepared, nil
}

func detachRestoreTarget(parent *restoreTargetParent, candidate Candidate, hooks *restoreHooks, store *backup.Store) (*detachedRestoreTarget, error) {
	if err := parent.verify(); err != nil {
		return nil, err
	}
	exists, err := rootPathExists(parent.root, parent.base)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("restore target %s disappeared immediately before atomic detach", candidate.Path)
	}
	containerName, err := makeRestoreRecoveryContainer(parent.root)
	if err != nil {
		return nil, err
	}
	containerInfo, err := parent.root.Lstat(containerName)
	if err != nil {
		_ = parent.root.Remove(containerName)
		return nil, err
	}
	container, err := parent.root.OpenRoot(containerName)
	if err != nil {
		if current, statErr := parent.root.Lstat(containerName); statErr == nil && os.SameFile(containerInfo, current) {
			_ = parent.root.Remove(containerName)
		}
		return nil, err
	}
	opened, err := container.Stat(".")
	if err != nil || !os.SameFile(containerInfo, opened) {
		_ = container.Close()
		if current, statErr := parent.root.Lstat(containerName); statErr == nil && os.SameFile(containerInfo, current) {
			_ = parent.root.Remove(containerName)
		}
		return nil, errors.New("restore recovery container changed while it was opened")
	}
	detached := &detachedRestoreTarget{
		containerName: containerName,
		itemName:      "original",
		itemPath:      filepath.Join(containerName, "original"),
		container:     container,
		containerInfo: containerInfo,
	}
	journaled := false
	failBeforeDetach := func(cause error) error {
		var journalErr error
		if journaled {
			journalErr = store.ClearRecoveryPath(candidate.Path, "restore-overwrite")
			if journalErr == nil {
				detached.recoveryPath = ""
				journaled = false
			}
		}
		cleanupErr := cleanupEmptyRecoveryContainer(parent, detached)
		return errors.Join(cause, journalErr, cleanupErr)
	}
	if err := detached.verify(parent); err != nil {
		return nil, failBeforeDetach(err)
	}
	if _, err := container.Lstat(detached.itemName); err == nil {
		return nil, failBeforeDetach(fmt.Errorf("restore recovery destination appeared before detach: %s", detached.itemPath))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, failBeforeDetach(err)
	}
	recoveryPath, err := locateProspectiveDetachedRecoveryPath(parent, detached)
	if err != nil {
		return nil, failBeforeDetach(err)
	}
	if err := store.SetRecoveryPath(candidate.Path, "restore-overwrite", recoveryPath); err != nil {
		return nil, failBeforeDetach(err)
	}
	detached.recoveryPath = recoveryPath
	journaled = true
	if hooks != nil && hooks.afterRecoveryJournal != nil {
		if err := hooks.afterRecoveryJournal(recoveryPath); err != nil {
			return nil, failBeforeDetach(err)
		}
	}
	if hooks != nil && hooks.beforeRecoveryRename != nil {
		hooks.beforeRecoveryRename(container, detached.itemName)
	}
	if err := parent.verify(); err != nil {
		return nil, failBeforeDetach(err)
	}
	if err := detached.verify(parent); err != nil {
		return nil, failBeforeDetach(err)
	}
	if _, err := container.Lstat(detached.itemName); err == nil {
		return nil, failBeforeDetach(fmt.Errorf("restore recovery destination appeared at the no-replace rename boundary: %s", detached.itemPath))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, failBeforeDetach(err)
	}
	sourceInfo, sourcePin, err := snapshotRestoreDetachSource(parent, candidate, hooks)
	if err != nil {
		return nil, failBeforeDetach(err)
	}
	keepSourcePin := false
	defer func() {
		if !keepSourcePin && sourcePin != nil {
			_ = sourcePin.Close()
		}
	}()
	if !sourceInfo.IsDir() && sourceInfo.Mode()&os.ModeSymlink == 0 && !sourceInfo.Mode().IsRegular() {
		return nil, failBeforeDetach(fmt.Errorf("cannot atomically detach non-regular restore target %s", candidate.Path))
	}
	detached.itemInfo = sourceInfo
	detached.itemPin = sourcePin
	if err := renameRestoreNoReplace(parent.root, parent.base, detached.itemPath, hooks); err != nil {
		return nil, failBeforeDetach(err)
	}
	if hooks != nil && hooks.afterDetachRename != nil {
		hooks.afterDetachRename(parent.root, parent.base, detached.itemPath)
	}
	movedInfo, err := container.Lstat(detached.itemName)
	if err != nil || movedInfo.Mode().Type() != detached.itemInfo.Mode().Type() || !os.SameFile(detached.itemInfo, movedInfo) {
		clearErr := store.ClearRecoveryPath(candidate.Path, "restore-overwrite")
		if clearErr == nil {
			detached.recoveryPath = ""
		}
		closeErr := container.Close()
		detached.container = nil
		return nil, &retainedDetachedRestoreError{
			cause:                errors.Join(fmt.Errorf("restore target %s could not be identity-verified after atomic detach", candidate.Path), err, clearErr, closeErr),
			recoveryRef:          detached.recoveryReference(),
			recoveryIdentityLost: true,
		}
	}
	current, err := snapshotRestoreTarget(container, detached.itemName, nil)
	if err != nil || current != candidate.currentSnapshot {
		cause := fmt.Errorf("restore target %s changed during atomic detach", candidate.Path)
		changedCandidate := candidate
		changedCandidate.currentSnapshot = current
		journalErr := refreshDetachedRecoveryPath(parent, detached, candidate, store)
		if current == "" {
			_ = container.Close()
			return nil, detached.retainedError(errors.Join(cause, err, journalErr))
		}
		if rollbackErr := rollbackDetachedNoReplace(parent, changedCandidate, detached, hooks); rollbackErr != nil {
			_ = container.Close()
			return nil, detached.retainedError(errors.Join(cause, rollbackErr, journalErr))
		}
		finalizeErr := finalizeDetachedRollback(parent, detached, changedCandidate, store)
		return nil, &retainedDetachedRestoreError{cause: errors.Join(cause, journalErr, finalizeErr), rolledBack: true}
	}
	bindingErr := parent.verify()
	journalErr := refreshDetachedRecoveryPath(parent, detached, candidate, store)
	if bindingErr != nil || journalErr != nil {
		cause := errors.Join(bindingErr, journalErr)
		if rollbackErr := rollbackDetachedNoReplace(parent, candidate, detached, hooks); rollbackErr != nil {
			cause = errors.Join(cause, fmt.Errorf("rollback did not overwrite a reappeared target: %w", rollbackErr))
			_ = container.Close()
			return nil, detached.retainedError(cause)
		}
		finalizeErr := finalizeDetachedRollback(parent, detached, candidate, store)
		return nil, &retainedDetachedRestoreError{cause: errors.Join(cause, finalizeErr), rolledBack: true}
	}
	keepSourcePin = true
	return detached, nil
}

func snapshotRestoreDetachSource(parent *restoreTargetParent, candidate Candidate, hooks *restoreHooks) (fs.FileInfo, *os.File, error) {
	if hooks != nil && hooks.beforeDetachIdentityPin != nil {
		hooks.beforeDetachIdentityPin(parent.root, parent.base)
	}
	currentInfo, currentErr := parent.root.Lstat(parent.base)
	if currentErr != nil {
		return nil, nil, fmt.Errorf("restore target %s changed immediately before atomic detach: %w", candidate.Path, currentErr)
	}
	if !currentInfo.IsDir() && currentInfo.Mode()&os.ModeSymlink == 0 && !currentInfo.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("cannot atomically detach non-regular restore target %s", candidate.Path)
	}
	if err := verifyRestoreTargetIdentity(parent.root, parent.base, candidate); err != nil {
		return nil, nil, fmt.Errorf("restore target %s changed immediately before atomic detach: %w", candidate.Path, err)
	}
	pin, err := openRestoreIdentityPin(parent.root, parent.base)
	if err != nil {
		return nil, nil, err
	}
	opened := currentInfo
	var statErr error
	if pin != nil {
		opened, statErr = pin.Stat()
	}
	before, beforeErr := parent.root.Lstat(parent.base)
	if statErr != nil || beforeErr != nil || !sameRestoreIdentity(before, opened) || !sameRestoreIdentity(candidate.currentInfo, opened) {
		return nil, nil, errors.Join(fmt.Errorf("restore target %s changed while its identity handle was opened", candidate.Path), statErr, beforeErr, closeRestorePin(pin))
	}
	if candidate.currentPin != nil {
		planned, plannedErr := candidate.currentPin.Stat()
		if plannedErr != nil || !sameRestoreIdentity(planned, opened) {
			return nil, nil, errors.Join(fmt.Errorf("restore target %s changed from its planned identity immediately before atomic detach", candidate.Path), plannedErr, closeRestorePin(pin))
		}
	}
	if !opened.IsDir() && opened.Mode()&os.ModeSymlink == 0 && !opened.Mode().IsRegular() {
		return nil, nil, errors.Join(fmt.Errorf("cannot atomically detach non-regular restore target %s", candidate.Path), closeRestorePin(pin))
	}
	snapshot, snapshotErr := snapshotRestoreTarget(parent.root, parent.base, nil)
	after, statErr := parent.root.Lstat(parent.base)
	if snapshotErr != nil || statErr != nil || before.Mode().Type() != after.Mode().Type() || !os.SameFile(before, after) || snapshot != candidate.currentSnapshot {
		var closeErr error
		if pin != nil {
			closeErr = pin.Close()
		}
		return nil, nil, errors.Join(fmt.Errorf("restore target %s changed immediately before atomic detach", candidate.Path), snapshotErr, statErr, closeErr)
	}
	return after, pin, nil
}

func closeRestorePin(pin *os.File) error {
	if pin == nil {
		return nil
	}
	return pin.Close()
}

func backupRootedCurrent(store *backup.Store, parent *restoreTargetParent, candidate Candidate) (*backup.Entry, error) {
	if err := parent.verify(); err != nil {
		return nil, err
	}
	info, err := parent.root.Lstat(parent.base)
	if err != nil {
		return nil, err
	}
	switch {
	case info.IsDir():
		directory, err := parent.root.OpenRoot(parent.base)
		if err != nil {
			return nil, err
		}
		defer directory.Close()
		opened, err := directory.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			return nil, fmt.Errorf("restore target %s changed before overwrite backup", candidate.Path)
		}
		return store.BackupRootDirWithLimits(candidate.Path, directory, ".", "restore-overwrite", restoreDirectoryLimits())
	case info.Mode()&os.ModeSymlink != 0:
		target, err := parent.root.Readlink(parent.base)
		if err != nil {
			return nil, err
		}
		return store.BackupSymlinkTarget(candidate.Path, target, "restore-overwrite")
	case info.Mode().IsRegular():
		body, err := backup.ReadRootRegularFileBounded(parent.root, parent.base, maxRestoreRegularPayloadSize)
		if err != nil {
			return nil, err
		}
		return store.BackupBytesWithMode(candidate.Path, body, "restore-overwrite", info.Mode())
	default:
		return nil, fmt.Errorf("cannot back up non-regular restore target %s", candidate.Path)
	}
}

func installPreparedBackup(parent *restoreTargetParent, candidate Candidate, prepared preparedBackupPayload, directory *preparedRestoreDirectory, hooks *restoreHooks) error {
	if err := parent.verify(); err != nil {
		return err
	}
	switch {
	case prepared.directory:
		if directory == nil {
			return errors.New("prepared restore directory is missing")
		}
		return installRootDirectoryNoReplace(parent, directory, candidate, prepared.postHash, hooks)
	case candidate.Symlink:
		if err := parent.root.Symlink(prepared.symlinkTarget, parent.base); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("restore target %s reappeared during installation", candidate.Path)
			}
			return err
		}
		return nil
	default:
		return installRegularFileIfMissing(parent, candidate, prepared.body, prepared.mode, prepared.modeKnown, hooks)
	}
}

func installRegularFileIfMissing(parent *restoreTargetParent, candidate Candidate, body []byte, mode os.FileMode, modeKnown bool, hooks *restoreHooks) (returnErr error) {
	if !modeKnown {
		return fmt.Errorf("backup entry for %s is missing required mode metadata", candidate.Path)
	}
	temporaryName, temporary, err := createRestoreTempFile(parent.root, ".threadpoint-restore-file-", 0o600)
	if err != nil {
		return err
	}
	temporaryInfo, err := temporary.Stat()
	if err != nil {
		return errors.Join(err, temporary.Close())
	}
	// Keep the prepared inode open through the no-replace rename hook and
	// identity-bound cleanup. Otherwise unlink/recreate can recycle its inode
	// number and make cleanup delete an untrusted pathname replacement.
	defer func() {
		returnErr = errors.Join(returnErr, cleanupRestoreTempFile(parent.root, temporaryName, temporaryInfo), temporary.Close())
	}()
	if _, err := temporary.Write(body); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Chmod(mode.Perm()); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := parent.verify(); err != nil {
		return err
	}
	current, err := parent.root.Lstat(temporaryName)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(temporaryInfo, current) {
		return errors.Join(fmt.Errorf("temporary restore file changed before atomic installation: %s", filepath.Join(parent.root.Name(), temporaryName)), err)
	}
	if err := renameRestoreNoReplace(parent.root, temporaryName, parent.base, hooks); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("restore target %s reappeared during installation", candidate.Path)
		}
		return fmt.Errorf("atomically installing restore target %s without replacement: %w", candidate.Path, err)
	}
	return nil
}

func cleanupRestoreTempFile(root *os.Root, name string, expected fs.FileInfo) error {
	current, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	path := filepath.Join(root.Name(), name)
	if err != nil {
		return fmt.Errorf("temporary restore residue was retained at %s: %w", path, err)
	}
	if expected == nil || !current.Mode().IsRegular() || !os.SameFile(expected, current) {
		return fmt.Errorf("temporary restore residue was retained after its identity changed: %s", path)
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("temporary restore residue was retained at %s: %w", path, err)
	}
	return nil
}

func renameRestoreNoReplace(root *os.Root, oldName string, newName string, hooks *restoreHooks) error {
	if hooks != nil && hooks.renameNoReplace != nil {
		return hooks.renameNoReplace(root, oldName, newName)
	}
	return safefs.RenameRootNoReplace(root, oldName, newName)
}

func installRootDirectoryNoReplace(parent *restoreTargetParent, prepared *preparedRestoreDirectory, candidate Candidate, expectedDigest string, hooks *restoreHooks) (returnErr error) {
	if prepared == nil || prepared.root == nil || prepared.info == nil {
		return errors.New("prepared restore directory is missing")
	}
	if err := parent.verify(); err != nil {
		return err
	}
	current, err := parent.root.Lstat(prepared.name)
	opened, openedErr := prepared.root.Stat(".")
	if err != nil || openedErr != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(prepared.info, current) || !os.SameFile(prepared.info, opened) {
		return errors.Join(fmt.Errorf("prepared restore directory for %s changed before atomic publication", candidate.Path), err, openedErr)
	}
	if hooks != nil && hooks.duringDirectoryInstall != nil {
		hooks.duringDirectoryInstall(prepared.root)
	}
	digest, digestErr := restoreDirectoryDigestRoot(prepared.root, ".")
	current, statErr := parent.root.Lstat(prepared.name)
	if digestErr != nil || statErr != nil || digest != expectedDigest || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(prepared.info, current) {
		return errors.Join(fmt.Errorf("prepared restore directory for %s changed before atomic publication", candidate.Path), digestErr, statErr)
	}
	if err := parent.verify(); err != nil {
		return err
	}
	if err := renameRestoreNoReplace(parent.root, prepared.name, parent.base, hooks); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("restore target %s reappeared during installation", candidate.Path)
		}
		return fmt.Errorf("atomically installing restore directory %s without replacement: %w", candidate.Path, err)
	}
	// Publication transferred ownership of the prepared inode to the canonical
	// name. Even if post-rename validation fails, deferred preparation cleanup
	// must never delete through the installed inode.
	defer func() {
		closeErr := prepared.root.Close()
		prepared.root = nil
		returnErr = errors.Join(returnErr, closeErr)
	}()
	installed, statErr := parent.root.Lstat(parent.base)
	digest, digestErr = restoreDirectoryDigestRoot(prepared.root, ".")
	if statErr != nil || digestErr != nil || !installed.IsDir() || installed.Mode()&os.ModeSymlink != 0 || !os.SameFile(prepared.info, installed) || digest != expectedDigest {
		return errors.Join(fmt.Errorf("restore target %s changed during atomic directory installation", candidate.Path), statErr, digestErr)
	}
	if _, err := parent.root.Lstat(prepared.name); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(fmt.Errorf("prepared restore directory name remained after atomic publication for %s", candidate.Path), err)
	}
	return nil
}

func verifyInstalledBackup(parent *restoreTargetParent, candidate Candidate, prepared preparedBackupPayload) error {
	if err := parent.verify(); err != nil {
		return err
	}
	var current string
	var err error
	switch {
	case prepared.directory:
		current, err = restoreDirectoryDigestRoot(parent.root, parent.base)
	case candidate.Symlink:
		var target string
		target, err = parent.root.Readlink(parent.base)
		if err == nil {
			current = restoreSnapshotString(target)
		}
	default:
		var body []byte
		body, err = backup.ReadRootRegularFileBounded(parent.root, parent.base, int64(len(prepared.body)))
		if err == nil {
			current = bytesSHA256(body)
		}
	}
	if err != nil || current != prepared.postHash {
		return fmt.Errorf("restore target %s changed during installation", candidate.Path)
	}
	if err := verifyRestoredPermissions(parent, candidate, prepared); err != nil {
		return err
	}
	return nil
}

func verifyRestoredPermissions(parent *restoreTargetParent, candidate Candidate, prepared preparedBackupPayload) error {
	if candidate.Symlink {
		return nil
	}
	if !prepared.modeKnown {
		return fmt.Errorf("backup entry for %s is missing required mode metadata", candidate.Path)
	}
	info, err := parent.root.Lstat(parent.base)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != prepared.mode.Perm() {
		return fmt.Errorf("restore target %s mode changed during installation", candidate.Path)
	}
	if !prepared.directory || len(candidate.backupTreeModes) == 0 {
		return nil
	}
	return backup.WalkRootDirectoryWithLimits(parent.root, parent.base, restoreRootWalkLimits(), func(_ *os.Root, _ string, sourcePath string, current fs.FileInfo) error {
		if !current.IsDir() && !current.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(parent.base, sourcePath)
		if err != nil {
			return err
		}
		want, ok := candidate.backupTreeModes[filepath.ToSlash(rel)]
		if !ok || current.Mode().Perm() != os.FileMode(want).Perm() {
			return fmt.Errorf("restore target %s mode changed during installation at %s", candidate.Path, rel)
		}
		return nil
	})
}

func verifyRestoreTargetUnchanged(parent *restoreTargetParent, candidate Candidate) error {
	if err := parent.verify(); err != nil {
		return err
	}
	exists, err := rootPathExists(parent.root, parent.base)
	if err != nil {
		return err
	}
	if !exists {
		if candidate.currentSnapshot != "" {
			return fmt.Errorf("restore target %s changed since restore plan: target disappeared", candidate.Path)
		}
		return nil
	}
	if candidate.currentSnapshot == "" {
		return fmt.Errorf("restore target %s changed since restore plan", candidate.Path)
	}
	return verifyRestoreTargetIdentity(parent.root, parent.base, candidate)
}

func verifyRestoreTargetIdentity(root *os.Root, name string, candidate Candidate) error {
	if candidate.currentSnapshot == "" {
		if _, err := root.Lstat(name); err == nil {
			return fmt.Errorf("restore target %s appeared since restore plan", candidate.Path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	before, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("restore target %s changed since restore plan: %w", candidate.Path, err)
	}
	if !sameRestoreIdentity(candidate.currentInfo, before) {
		return fmt.Errorf("restore target %s changed identity since restore plan", candidate.Path)
	}
	if candidate.currentPin != nil {
		pinned, pinErr := candidate.currentPin.Stat()
		if pinErr != nil || !sameRestoreIdentity(candidate.currentInfo, pinned) || !sameRestoreIdentity(before, pinned) {
			return errors.Join(fmt.Errorf("restore target %s changed identity since restore plan", candidate.Path), pinErr)
		}
	}
	current, snapshotErr := snapshotRestoreTarget(root, name, nil)
	after, statErr := root.Lstat(name)
	if snapshotErr != nil || statErr != nil || !sameRestoreIdentity(candidate.currentInfo, after) || !sameRestoreIdentity(before, after) || current != candidate.currentSnapshot {
		return errors.Join(fmt.Errorf("restore target %s changed since restore plan", candidate.Path), snapshotErr, statErr)
	}
	if candidate.currentPin != nil {
		pinned, pinErr := candidate.currentPin.Stat()
		if pinErr != nil || !sameRestoreIdentity(candidate.currentInfo, pinned) || !sameRestoreIdentity(after, pinned) {
			return errors.Join(fmt.Errorf("restore target %s changed identity since restore plan", candidate.Path), pinErr)
		}
	}
	return nil
}

func captureAndOpenRestoreRoot(logicalRoot string) (restoreRootSnapshot, *os.Root, error) {
	physicalRoot, err := safefs.ResolveRootForMutation(logicalRoot)
	if err != nil {
		return restoreRootSnapshot{}, nil, err
	}
	root, err := os.OpenRoot(physicalRoot)
	if err != nil {
		return restoreRootSnapshot{}, nil, err
	}
	info, err := root.Stat(".")
	if err != nil {
		return restoreRootSnapshot{}, nil, errors.Join(err, root.Close())
	}
	if !info.IsDir() {
		return restoreRootSnapshot{}, nil, errors.Join(fmt.Errorf("restore root is not a directory: %s", logicalRoot), root.Close())
	}
	snapshot := restoreRootSnapshot{logicalRoot: filepath.Clean(logicalRoot), physicalRoot: filepath.Clean(physicalRoot), info: info}
	if err := validateRestoreRootSnapshot(snapshot); err != nil {
		return restoreRootSnapshot{}, nil, errors.Join(err, root.Close())
	}
	return snapshot, root, nil
}

func validateRestoreRootSnapshot(snapshot restoreRootSnapshot) error {
	physicalRoot, err := safefs.ResolveRootForMutation(snapshot.logicalRoot)
	if err != nil {
		return fmt.Errorf("restore root changed since the restore plan was built: %w", err)
	}
	if filepath.Clean(physicalRoot) != snapshot.physicalRoot {
		return fmt.Errorf("restore root changed since the restore plan was built: %s now resolves to %s instead of %s", snapshot.logicalRoot, physicalRoot, snapshot.physicalRoot)
	}
	current, err := os.Stat(snapshot.physicalRoot)
	if err != nil {
		return fmt.Errorf("restore root changed since the restore plan was built: %w", err)
	}
	if snapshot.info == nil || !current.IsDir() || !os.SameFile(snapshot.info, current) {
		return errors.New("restore root changed since the restore plan was built")
	}
	return nil
}

func candidateRootName(candidate Candidate) (string, error) {
	rootAbs, err := filepath.Abs(candidate.Root)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(candidate.Target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(targetAbs))
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("restore target %q is outside root %q: %w", candidate.Target, candidate.Root, safepath.ErrUnsafe)
	}
	return filepath.Clean(rel), nil
}

func openRestoreTargetParent(project *os.Root, rootSnapshot restoreRootSnapshot, candidate Candidate) (*restoreTargetParent, error) {
	if err := validateRestoreRootSnapshot(rootSnapshot); err != nil {
		return nil, err
	}
	name, err := candidateRootName(candidate)
	if err != nil {
		return nil, err
	}
	parentPath := filepath.Dir(name)
	if err := safefs.RejectRootSymlinkAncestors(project, parentPath); err != nil {
		return nil, err
	}
	// #nosec G301 -- restore creates missing project parents with the process umask.
	if err := project.MkdirAll(parentPath, 0o777); err != nil {
		return nil, err
	}
	if err := safefs.RejectRootSymlinkAncestors(project, parentPath); err != nil {
		return nil, err
	}
	info, err := project.Lstat(parentPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("restore target parent is not a real directory: %s", filepath.ToSlash(parentPath))
	}
	root, err := project.OpenRoot(parentPath)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, errors.New("restore target parent changed while it was opened")
	}
	parent := &restoreTargetParent{project: project, root: root, path: parentPath, base: filepath.Base(name), parentInfo: opened, rootSnapshot: rootSnapshot}
	if err := parent.verify(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return parent, nil
}

func (parent *restoreTargetParent) verify() error {
	if err := validateRestoreRootSnapshot(parent.rootSnapshot); err != nil {
		return err
	}
	if err := safefs.RejectRootSymlinkAncestors(parent.project, parent.path); err != nil {
		return err
	}
	current, err := parent.project.Lstat(parent.path)
	if err != nil {
		return err
	}
	opened, err := parent.root.Stat(".")
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(parent.parentInfo, current) || !os.SameFile(parent.parentInfo, opened) {
		return errors.New("restore target parent changed during the rooted transaction")
	}
	return nil
}

func (parent *restoreTargetParent) verifyPinned() error {
	projectInfo, err := parent.project.Stat(".")
	if err != nil {
		return err
	}
	opened, err := parent.root.Stat(".")
	if err != nil {
		return err
	}
	if parent.rootSnapshot.info == nil || !projectInfo.IsDir() || !os.SameFile(parent.rootSnapshot.info, projectInfo) {
		return errors.New("pinned restore root changed during the rooted transaction")
	}
	if parent.parentInfo == nil || !opened.IsDir() || !os.SameFile(parent.parentInfo, opened) {
		return errors.New("pinned restore target parent changed during the rooted transaction")
	}
	return nil
}

func (detached *detachedRestoreTarget) verify(parent *restoreTargetParent) error {
	if detached == nil || detached.container == nil {
		return errors.New("restore recovery container is not open")
	}
	if err := parent.verify(); err != nil {
		return err
	}
	current, err := parent.root.Lstat(detached.containerName)
	if err != nil {
		return err
	}
	opened, err := detached.container.Stat(".")
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(detached.containerInfo, current) || !os.SameFile(detached.containerInfo, opened) {
		return errors.New("restore recovery container changed during the rooted transaction")
	}
	return detached.verifyItemIdentity()
}

func (detached *detachedRestoreTarget) verifyPinned(parent *restoreTargetParent) error {
	if detached == nil || detached.container == nil {
		return errors.New("restore recovery container is not open")
	}
	if err := parent.verifyPinned(); err != nil {
		return err
	}
	current, err := parent.root.Lstat(detached.containerName)
	if err != nil {
		return err
	}
	opened, err := detached.container.Stat(".")
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(detached.containerInfo, current) || !os.SameFile(detached.containerInfo, opened) {
		return errors.New("pinned restore recovery container changed during the rooted transaction")
	}
	return detached.verifyItemIdentity()
}

func (detached *detachedRestoreTarget) verifyItemIdentity() error {
	if detached.itemInfo == nil {
		return nil
	}
	if detached.itemPin != nil {
		pinned, err := detached.itemPin.Stat()
		if err != nil || pinned.Mode().Type() != detached.itemInfo.Mode().Type() || !os.SameFile(detached.itemInfo, pinned) {
			return errors.Join(errors.New("detached restore item identity handle changed"), err)
		}
	}
	current, err := detached.container.Lstat(detached.itemName)
	if err != nil {
		return err
	}
	if current.Mode().Type() != detached.itemInfo.Mode().Type() || !os.SameFile(detached.itemInfo, current) {
		return errors.New("detached restore item changed identity inside its recovery container")
	}
	return nil
}

func (detached *detachedRestoreTarget) closeItemPin() error {
	if detached == nil || detached.itemPin == nil {
		return nil
	}
	err := detached.itemPin.Close()
	detached.itemPin = nil
	return err
}

var errRestoreParentLocated = errors.New("restore parent located")

func (detached *detachedRestoreTarget) recoveryReference() string {
	if detached == nil {
		return "an unknown pinned recovery entry"
	}
	return filepath.ToSlash(detached.itemPath)
}

func (detached *detachedRestoreTarget) retentionMessage() string {
	if detached != nil && detached.recoveryPath != "" {
		return "was retained at " + detached.recoveryPath
	}
	return "remains retained in " + detached.recoveryReference() + " inside the pinned or moved restore parent"
}

func (detached *detachedRestoreTarget) retainedError(cause error) error {
	return &retainedDetachedRestoreError{
		cause:       cause,
		detached:    detached.recoveryPath,
		recoveryRef: detached.recoveryReference(),
	}
}

func refreshDetachedRecoveryPath(parent *restoreTargetParent, detached *detachedRestoreTarget, candidate Candidate, store *backup.Store) error {
	recoveryPath, locateErr := locateDetachedRecoveryPath(parent, detached)
	if recoveryPath == "" {
		clearErr := error(nil)
		if detached.recoveryPath != "" {
			clearErr = store.ClearRecoveryPath(candidate.Path, "restore-overwrite")
		}
		detached.recoveryPath = ""
		return errors.Join(locateErr, clearErr)
	}
	if err := store.SetRecoveryPath(candidate.Path, "restore-overwrite", recoveryPath); err != nil {
		return errors.Join(locateErr, err)
	}
	detached.recoveryPath = recoveryPath
	return locateErr
}

func finalizeDetachedRollback(parent *restoreTargetParent, detached *detachedRestoreTarget, candidate Candidate, store *backup.Store) error {
	clearErr := store.ClearRecoveryPath(candidate.Path, "restore-overwrite")
	if clearErr == nil {
		detached.recoveryPath = ""
	}
	cleanupErr := cleanupEmptyRecoveryContainer(parent, detached)
	return errors.Join(clearErr, cleanupErr)
}

func locateDetachedRecoveryPath(parent *restoreTargetParent, detached *detachedRestoreTarget) (string, error) {
	if err := detached.verifyPinned(parent); err != nil {
		return "", err
	}
	if detached.itemInfo == nil {
		return "", errors.New("detached restore target identity is unavailable")
	}
	rootPath, err := locateRestoreRootPath(parent.rootSnapshot)
	if err != nil || rootPath == "" {
		return "", err
	}
	parentPath, err := locateRestoreParentPath(parent)
	if err != nil || parentPath == "" {
		return "", err
	}
	recoveryPath := filepath.Join(rootPath, parentPath, detached.itemPath)
	current, err := os.Lstat(recoveryPath)
	if err != nil {
		return "", err
	}
	if current.Mode().Type() != detached.itemInfo.Mode().Type() || !os.SameFile(detached.itemInfo, current) {
		return "", errors.New("restore recovery pathname changed after detach")
	}
	return recoveryPath, nil
}

func locateProspectiveDetachedRecoveryPath(parent *restoreTargetParent, detached *detachedRestoreTarget) (string, error) {
	if err := detached.verify(parent); err != nil {
		return "", err
	}
	if _, err := detached.container.Lstat(detached.itemName); err == nil {
		return "", fmt.Errorf("restore recovery destination already exists: %s", detached.itemPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	rootPath, err := locateRestoreRootPath(parent.rootSnapshot)
	if err != nil || rootPath == "" {
		return "", err
	}
	parentPath, err := locateRestoreParentPath(parent)
	if err != nil || parentPath == "" {
		return "", err
	}
	recoveryPath := filepath.Join(rootPath, parentPath, detached.itemPath)
	containerPath := filepath.Dir(recoveryPath)
	current, err := os.Lstat(containerPath)
	if err != nil {
		return "", err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(detached.containerInfo, current) {
		return "", errors.New("restore recovery container pathname changed before journaling")
	}
	if _, err := os.Lstat(recoveryPath); err == nil {
		return "", errors.New("restore recovery item appeared before journaling")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return recoveryPath, nil
}

func locateRestoreRootPath(snapshot restoreRootSnapshot) (string, error) {
	current, err := os.Lstat(snapshot.physicalRoot)
	if err == nil && current.IsDir() && current.Mode()&os.ModeSymlink == 0 && snapshot.info != nil && os.SameFile(snapshot.info, current) {
		return snapshot.physicalRoot, nil
	}
	parentPath := filepath.Dir(snapshot.physicalRoot)
	// #nosec G304 -- the parent is derived from the reviewed physical restore root and is used only for a bounded identity probe.
	directory, openErr := os.Open(parentPath)
	if openErr != nil {
		return "", errors.Join(err, openErr)
	}
	defer directory.Close()
	for seen := 0; ; seen++ {
		if seen >= maxRestoreDirectoryEntries+1 {
			return "", fmt.Errorf("restore root relocation probe exceeds %d filesystem entries", maxRestoreDirectoryEntries+1)
		}
		entries, readErr := directory.ReadDir(1)
		if len(entries) == 1 {
			candidatePath := filepath.Join(parentPath, entries[0].Name())
			info, statErr := os.Lstat(candidatePath)
			if statErr != nil {
				return "", statErr
			}
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 && snapshot.info != nil && os.SameFile(snapshot.info, info) {
				return candidatePath, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			return "", nil
		}
		if readErr != nil {
			return "", readErr
		}
	}
}

func locateRestoreParentPath(parent *restoreTargetParent) (string, error) {
	current, err := parent.project.Lstat(parent.path)
	if err == nil && current.IsDir() && current.Mode()&os.ModeSymlink == 0 && os.SameFile(parent.parentInfo, current) {
		return parent.path, nil
	}
	found := ""
	walkErr := backup.WalkRootDirectoryWithLimits(parent.project, ".", restoreRootWalkLimits(), func(_ *os.Root, _ string, walkPath string, info fs.FileInfo) error {
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 && os.SameFile(parent.parentInfo, info) {
			found = filepath.FromSlash(walkPath)
			return errRestoreParentLocated
		}
		return nil
	})
	if errors.Is(walkErr, errRestoreParentLocated) {
		return found, nil
	}
	if walkErr != nil {
		return "", walkErr
	}
	return "", nil
}

func cleanupEmptyRecoveryContainer(parent *restoreTargetParent, detached *detachedRestoreTarget) error {
	if detached == nil || detached.container == nil {
		return nil
	}
	hasEntry, readErr := rootDirectoryHasAnyEntry(detached.container)
	if readErr != nil || hasEntry {
		closeErr := detached.container.Close()
		detached.container = nil
		if hasEntry {
			retained := filepath.Join(parent.root.Name(), detached.containerName)
			if detached.recoveryPath != "" {
				retained = filepath.Dir(detached.recoveryPath)
			}
			return errors.Join(readErr, closeErr, fmt.Errorf("restore recovery container received content and was retained at %s", retained))
		}
		return errors.Join(readErr, closeErr)
	}
	closeErr := detached.container.Close()
	detached.container = nil
	current, statErr := parent.root.Lstat(detached.containerName)
	if statErr != nil {
		return errors.Join(closeErr, statErr)
	}
	if !os.SameFile(detached.containerInfo, current) {
		return errors.Join(closeErr, errors.New("empty restore recovery container changed before cleanup"))
	}
	return errors.Join(closeErr, parent.root.Remove(detached.containerName))
}

func rollbackDetachedNoReplace(parent *restoreTargetParent, candidate Candidate, detached *detachedRestoreTarget, hooks *restoreHooks) error {
	if err := detached.verifyPinned(parent); err != nil {
		return err
	}
	if exists, err := rootPathExists(parent.root, parent.base); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("restore target %s reappeared", candidate.Path)
	}
	info, err := detached.container.Lstat(detached.itemName)
	if err != nil {
		return err
	}
	if detached.itemInfo == nil || info.Mode().Type() != detached.itemInfo.Mode().Type() || !os.SameFile(detached.itemInfo, info) {
		return fmt.Errorf("detached original for %s changed identity before rollback", candidate.Path)
	}
	if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 && !info.Mode().IsRegular() {
		return fmt.Errorf("cannot roll back non-regular restore target %s", candidate.Path)
	}
	if err := renameRestoreNoReplace(parent.root, detached.itemPath, parent.base, hooks); err != nil {
		return err
	}
	restoredInfo, err := parent.root.Lstat(parent.base)
	if err != nil || restoredInfo.Mode().Type() != detached.itemInfo.Mode().Type() || !os.SameFile(detached.itemInfo, restoredInfo) {
		return errors.Join(fmt.Errorf("restore target %s changed identity during rollback", candidate.Path), err)
	}
	current, err := snapshotRestoreTarget(parent.root, parent.base, nil)
	if err != nil || current != candidate.currentSnapshot {
		return fmt.Errorf("restore target %s could not be verified after rollback", candidate.Path)
	}
	return nil
}

func verifyRetainedOriginal(parent *restoreTargetParent, candidate Candidate, detached *detachedRestoreTarget) error {
	if err := detached.verify(parent); err != nil {
		return err
	}
	current, err := snapshotRestoreTarget(detached.container, detached.itemName, nil)
	if err != nil {
		return fmt.Errorf("retained original for %s could not be verified at %s: %w", candidate.Path, detached.recoveryPath, err)
	}
	if current != candidate.currentSnapshot {
		// The path remains retained. Refuse to claim a snapshot-bound restore,
		// while preserving late writes through already-open handles for recovery.
		return fmt.Errorf("retained original for %s changed during restore and remains at %s", candidate.Path, detached.recoveryPath)
	}
	return nil
}

func discardPreparedDirectory(parent *restoreTargetParent, prepared *preparedRestoreDirectory) error {
	if prepared == nil || prepared.root == nil {
		return nil
	}
	cleanupErr := removeRestoreRootContents(prepared.root)
	closeErr := prepared.root.Close()
	prepared.root = nil
	if cleanupErr != nil || closeErr != nil {
		return preparedRestoreResidueError(parent, prepared, fmt.Errorf("bounded cleanup failed: %w", errors.Join(cleanupErr, closeErr)))
	}
	current, err := parent.root.Lstat(prepared.name)
	if errors.Is(err, os.ErrNotExist) {
		return preparedRestoreResidueError(parent, prepared, errors.New("prepared restore directory moved before cleanup"))
	}
	if err != nil {
		return preparedRestoreResidueError(parent, prepared, err)
	}
	if !os.SameFile(prepared.info, current) {
		return preparedRestoreResidueError(parent, prepared, errors.New("prepared restore directory changed before cleanup"))
	}
	if err := parent.root.Remove(prepared.name); err != nil {
		return preparedRestoreResidueError(parent, prepared, err)
	}
	return nil
}

func removeRestoreRootContents(root *os.Root) error {
	type cleanupEntry struct {
		name string
		info fs.FileInfo
	}
	entries := make([]cleanupEntry, 0, maxRestoreDirectoryEntries)
	if err := backup.WalkRootDirectoryWithLimits(root, ".", restoreRootWalkLimits(), func(_ *os.Root, _ string, walkPath string, info fs.FileInfo) error {
		if walkPath != "." {
			entries = append(entries, cleanupEntry{name: filepath.FromSlash(walkPath), info: info})
		}
		return nil
	}); err != nil {
		return err
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		current, err := root.Lstat(entry.name)
		if err != nil {
			return fmt.Errorf("prepared restore entry changed before cleanup: %s: %w", entry.name, err)
		}
		if current.Mode().Type() != entry.info.Mode().Type() || !os.SameFile(entry.info, current) {
			return fmt.Errorf("prepared restore entry changed before cleanup: %s", entry.name)
		}
		if err := root.Remove(entry.name); err != nil {
			return err
		}
	}
	hasEntry, err := rootDirectoryHasAnyEntry(root)
	if err != nil {
		return err
	}
	if hasEntry {
		return errors.New("prepared restore directory changed during cleanup")
	}
	return nil
}

func rootDirectoryHasAnyEntry(root *os.Root) (bool, error) {
	directory, err := root.Open(".")
	if err != nil {
		return false, err
	}
	entries, readErr := directory.ReadDir(1)
	closeErr := directory.Close()
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	return len(entries) != 0, errors.Join(readErr, closeErr)
}

func preparedRestoreResidueError(parent *restoreTargetParent, prepared *preparedRestoreDirectory, cause error) error {
	name, locateErr := findRootChildByIdentity(parent.root, prepared.info, maxRestoreDirectoryEntries+1)
	rootPath, rootErr := locateRestoreRootPath(parent.rootSnapshot)
	parentPath, parentErr := locateRestoreParentPath(parent)
	if locateErr == nil && rootErr == nil && parentErr == nil && name != "" && rootPath != "" && parentPath != "" {
		path := filepath.Join(rootPath, parentPath, name)
		return fmt.Errorf("prepared restore directory was retained at %s: %w", path, cause)
	}
	cause = errors.Join(cause, locateErr, rootErr, parentErr)
	return fmt.Errorf("prepared restore directory was retained under a moved or unbound parent: %w", cause)
}

func findRootChildByIdentity(root *os.Root, expected fs.FileInfo, maxEntries int) (string, error) {
	directory, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer directory.Close()
	for seen := 0; ; seen++ {
		if maxEntries > 0 && seen >= maxEntries {
			return "", fmt.Errorf("rooted identity probe exceeds %d filesystem entries", maxEntries)
		}
		entries, err := directory.ReadDir(1)
		if len(entries) == 1 {
			name := entries[0].Name()
			info, statErr := root.Lstat(name)
			if statErr != nil {
				return "", statErr
			}
			if os.SameFile(expected, info) {
				return name, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
}

func makeRestoreTempDirectory(root *os.Root, prefix string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name := randomRestoreName(prefix)
		if err := root.Mkdir(name, 0o755); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errors.New("could not allocate a temporary restore directory")
}

func makeRestoreRecoveryContainer(root *os.Root) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name := randomRestoreName(".threadpoint-restore-recovery-")
		if err := root.Mkdir(name, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errors.New("could not allocate a no-replace restore recovery container")
}

func createRestoreTempFile(root *os.Root, prefix string, perm os.FileMode) (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name := randomRestoreName(prefix)
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return name, file, err
	}
	return "", nil, errors.New("could not allocate a temporary restore file")
}

func randomRestoreName(prefix string) string {
	var token [12]byte
	_, _ = rand.Read(token[:])
	return prefix + hex.EncodeToString(token[:])
}

func rootPathExists(root *os.Root, name string) (bool, error) {
	_, err := root.Lstat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func snapshotRestoreTarget(root *os.Root, name string, hooks *restoreHooks) (string, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(name)
		if err != nil {
			return "", err
		}
		return "symlink:" + restoreSnapshotString(target), nil
	}
	if info.Mode().IsRegular() {
		if hooks != nil && hooks.beforeCurrentRegularRead != nil {
			hooks.beforeCurrentRegularRead(root, name)
		}
		body, err := backup.ReadRootRegularFileBounded(root, name, maxRestoreRegularPayloadSize)
		if err != nil {
			return "", err
		}
		return "file:" + bytesSHA256(body), nil
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cannot snapshot non-regular restore target %s", name)
	}
	digest, err := backup.RootDirSHA256WithLimits(root, name, restoreDirectoryLimits())
	if err != nil {
		return "", err
	}
	return "directory:" + digest, nil
}

func restoreSnapshotString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func restoreDirectoryBackup(source *os.Root, target *os.Root, modes map[string]uint32) error {
	var totalBytes int64
	return backup.WalkRootDirectoryWithLimits(source, ".", restoreRootWalkLimits(), func(accessRoot *os.Root, accessName string, sourcePath string, info fs.FileInfo) error {
		if sourcePath == "." {
			return nil
		}
		name := filepath.FromSlash(sourcePath)
		switch {
		case info.IsDir():
			mode, err := restoredTreeMode(modes, sourcePath)
			if err != nil {
				return err
			}
			if err := target.Mkdir(name, mode); err != nil {
				if errors.Is(err, os.ErrExist) {
					return fmt.Errorf("restore directory destination %s appeared during copy", sourcePath)
				}
				return err
			}
			// #nosec G302 -- the destination is a newly created directory beneath a private pinned root.
			if err := target.Chmod(name, mode); err != nil {
				return err
			}
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, err := accessRoot.Readlink(accessName)
			if err != nil {
				return err
			}
			if err := target.Symlink(linkTarget, name); err != nil {
				if errors.Is(err, os.ErrExist) {
					return fmt.Errorf("restore directory destination %s appeared during copy", sourcePath)
				}
				return err
			}
			return nil
		case info.Mode().IsRegular():
			mode, err := restoredTreeMode(modes, sourcePath)
			if err != nil {
				return err
			}
			if info.Size() > maxRestoreRegularPayloadSize || info.Size() > maxRestoreDirectoryBytes-totalBytes {
				return fmt.Errorf("restore directory exceeds the %d-byte copy limit", maxRestoreDirectoryBytes)
			}
			body, err := backup.ReadRootRegularFileBounded(accessRoot, accessName, maxRestoreRegularPayloadSize)
			if err != nil {
				return err
			}
			totalBytes += int64(len(body))
			if totalBytes > maxRestoreDirectoryBytes {
				return fmt.Errorf("restore directory exceeds the %d-byte copy limit", maxRestoreDirectoryBytes)
			}
			out, err := target.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				if errors.Is(err, os.ErrExist) {
					return fmt.Errorf("restore directory destination %s appeared during copy", sourcePath)
				}
				return err
			}
			if _, err := out.Write(body); err != nil {
				return errors.Join(err, out.Close())
			}
			if err := out.Sync(); err != nil {
				return errors.Join(err, out.Close())
			}
			// #nosec G122 G302 -- chmod operates on the newly created, already-open file handle.
			if err := out.Chmod(mode); err != nil {
				return errors.Join(err, out.Close())
			}
			if err := out.Sync(); err != nil {
				return errors.Join(err, out.Close())
			}
			return out.Close()
		default:
			return fmt.Errorf("unsupported non-regular file in restore directory: %s", sourcePath)
		}
	})
}

func restoredTreeMode(modes map[string]uint32, sourcePath string) (os.FileMode, error) {
	mode, ok := modes[filepath.ToSlash(sourcePath)]
	if !ok {
		return 0, fmt.Errorf("backup directory entry %s is missing required mode metadata", sourcePath)
	}
	return os.FileMode(mode).Perm(), nil
}

func acquireApplyLocks(opts Options, plan *Plan) (*safefs.LockSet, error) {
	threadpointHome, err := safefs.ResolveThreadpointHomeWithOverride(opts.HomeDir, opts.ThreadpointHome)
	if err != nil {
		return nil, err
	}
	backupRoot, err := backup.ResolveBackupRootWithOverride(opts.HomeDir, opts.ThreadpointHome, opts.BackupDir)
	if err != nil {
		return nil, err
	}
	roots := []string{opts.Root}
	if opts.ProductRoot == nil {
		// A caller-retained product root is the authority for backup I/O and
		// must not cause the ambient backup pathname to be reopened.
		roots = append(roots, backupRoot)
	}
	for _, candidate := range plan.Candidates {
		roots = append(roots, candidate.Root)
	}
	return safefs.AcquireLocks(threadpointHome, roots, restoreOperation)
}

func isNotSymlink(err error) bool {
	return errors.Is(err, syscall.EINVAL)
}

func confirmed(opts Options) bool {
	return opts.Yes || opts.Confirm == restoreConfirmation
}
