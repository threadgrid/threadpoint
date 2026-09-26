// SPDX-License-Identifier: Apache-2.0

// Package backup records and restores file backups for threadpoint's mutating
// operations. Each run captures originals into a manifest under the threadpoint
// home so stage/commit, prune, and restore operations can be rolled back.
package backup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/threadgrid/threadpoint/internal/abspath"
	"github.com/threadgrid/threadpoint/internal/home"
	"github.com/threadgrid/threadpoint/safefs"
	"github.com/threadgrid/threadpoint/safepath"
)

// ManifestName and ManifestVersion identify the backup manifest on disk and the
// schema used to decode it.
const (
	ManifestName              = "manifest.json"
	ManifestVersion           = 1
	staleThreshold            = 30 * 24 * time.Hour
	maxManifestBytes          = 16 * 1024 * 1024
	maxBoundCommitSourceBytes = 64 * 1024 * 1024
	// MaxCanonicalGenerationBytes is the single publication and recovery
	// ceiling for canonical memory generations.
	MaxCanonicalGenerationBytes = 8 * 1024 * 1024
	maxEnumeratedBackupRuns     = 4096
	maxEnumeratedBackupEntries  = 65536
	maxEnumeratedManifestBytes  = 64 * 1024 * 1024

	// PendingCommitOperation identifies a durable but prune-invisible commit
	// transaction whose canonical publication has not been finalized.
	PendingCommitOperation = "commit-pending"
	// CommittedCommitOperation identifies a finalized canonical commit backup.
	CommittedCommitOperation = "commit"
	// AbortedCommitOperation identifies a durable tombstone for a commit that
	// failed or was rolled back after its pending backup was prepared.
	AbortedCommitOperation = "commit-aborted"
	// RecoveryCommitOperation identifies a finalized canonical publication whose
	// backup remains available only for exact transaction recovery. It is not an
	// ordinary restore run or prune authorization.
	RecoveryCommitOperation = "commit-recovery"

	// CommitTransactionPending is the pre-publication transaction state.
	CommitTransactionPending = "pending"
	// CommitTransactionCommitted is the finalized publication state.
	CommitTransactionCommitted = "committed"
	// CommitTransactionAborted is the rolled-back/tombstoned state.
	CommitTransactionAborted = "aborted"
	// CommitTransactionRecoveryCommitted is a finalized publication whose
	// manifest is intentionally invisible to ordinary restore and prune flows.
	CommitTransactionRecoveryCommitted = "recovery-committed"

	// DefaultDirectoryMaxDepth, DefaultDirectoryMaxPathBytes, and
	// DefaultDirectoryMaxMetadataBytes are finite structural budgets inherited
	// by security-sensitive walks unless the caller supplies stricter values.
	DefaultDirectoryMaxDepth         = 128
	DefaultDirectoryMaxPathBytes     = 16 * 1024 * 1024
	DefaultDirectoryMaxMetadataBytes = 4 * 1024 * 1024
)

// pendingCommitGenerationBeforePublish is a test seam at the last fallible
// boundary before a complete private generation receives its canonical name.
var pendingCommitGenerationBeforePublish func(parent *os.Root, temporaryName string) error

// StoreOptions configures a backup store run for one project operation.
type StoreOptions struct {
	ProjectRoot     string
	HomeDir         string
	ThreadpointHome string
	BackupDir       string
	Operation       string
	RunID           string
	Now             func() time.Time
}

// Store records backup entries for a single operation run.
type Store struct {
	BackupRoot string
	ProjectID  string
	RunID      string
	RunDir     string
	Manifest   Manifest
	// index maps an entry's (role, path) key to its position in Manifest.Entries
	// so addOrReplace stays O(1) instead of rescanning every entry.
	index      map[string]int
	beforeSave func() error
	// manifestGenerationKnown distinguishes an unchecked store from a store
	// that reviewed an absent manifest. persistedManifest is the exact JSON
	// generation that the next conditional save may replace.
	manifestGenerationKnown bool
	persistedManifest       []byte
	borrowedHomeRoot        *os.Root
	borrowedHomePath        string
	loadedRunInfo           fs.FileInfo
}

// DirectoryLimits bounds work performed while copying or hashing a directory
// tree. A zero field is unlimited, except that a positive
// MaxEntries enables safe defaults for zero structural walk limits.
type DirectoryLimits struct {
	MaxEntries       int
	MaxBytes         int64
	MaxFileBytes     int64
	MaxDepth         int
	MaxPathBytes     int64
	MaxMetadataBytes int64
}

// RootWalkLimits bounds enumeration, nesting, and the aggregate metadata and
// complete relative paths emitted by WalkRootDirectoryWithLimits.
type RootWalkLimits struct {
	MaxEntries       int
	MaxDepth         int
	MaxPathBytes     int64
	MaxMetadataBytes int64
}

// runEnumerationLimits bounds the complete work of discovering backup runs
// for one project. The public list, latest, find, stale-warning, restore,
// prune, and doctor paths all use the same finite defaults.
type runEnumerationLimits struct {
	MaxRuns    int
	MaxEntries int
	MaxBytes   int64
}

type directoryBudget struct {
	limits DirectoryLimits
	bytes  int64
	work   string
}

// Manifest is the persisted metadata for a backup run.
type Manifest struct {
	Version           int                `json:"version"`
	Operation         string             `json:"operation"`
	ProjectRoot       string             `json:"projectRoot"`
	ProjectID         string             `json:"projectId"`
	RunID             string             `json:"runId"`
	CreatedAt         time.Time          `json:"createdAt"`
	CommitTransaction *CommitTransaction `json:"commitTransaction,omitempty"`
	Entries           []Entry            `json:"entries,omitempty"`
}

// CommitTransaction binds a pending or finalized backup run to one staged
// source digest and the exact canonical bytes intended by publication.
type CommitTransaction struct {
	State        string `json:"state"`
	StageID      string `json:"stageId"`
	SourcePath   string `json:"sourcePath"`
	SourceSHA256 string `json:"sourceSha256"`
	TargetPath   string `json:"targetPath"`
	TargetSHA256 string `json:"targetSha256"`
	TargetSize   int64  `json:"targetSize"`
}

// CommitBinding is the complete durable identity of one staged-source and
// canonical publication transaction. State is intentionally omitted so the
// same binding can inspect pending, committed, or aborted recovery evidence.
type CommitBinding struct {
	StageID      string
	SourcePath   string
	SourceSHA256 string
	TargetPath   string
	TargetSHA256 string
	TargetSize   int64
}

// PendingCommitGeneration is the complete pre-publication recovery evidence
// for one commit. SourceBody is the exact provider-native generation reviewed
// by the stage. PriorBody and PriorMode describe the exact canonical generation
// that publication will replace when PriorExists is true; an absent prior is
// represented explicitly when it is false.
type PendingCommitGeneration struct {
	Binding     CommitBinding
	SourceBody  []byte
	SourceMode  os.FileMode
	PriorExists bool
	PriorBody   []byte
	PriorMode   os.FileMode
}

// BoundCommitTransaction is a verified durable commit transaction plus the
// exact canonical generation that preceded it. Store retains the loaded
// manifest generation for conditional finalize or abort.
type BoundCommitTransaction struct {
	Store        *Store
	State        string
	TargetSHA256 string
	TargetSize   int64
	// PriorKnown is true for every accepted current-schema transaction. It is
	// retained as an explicit guard at rollback call sites and distinguishes a
	// sealed pre-publication absence marker from missing prior-state evidence.
	PriorKnown  bool
	PriorExists bool
	PriorBody   []byte
	PriorMode   os.FileMode
}

// Entry describes one backed-up file, directory, or symlink.
type Entry struct {
	Role       string `json:"role"`
	Root       string `json:"root,omitempty"`
	Path       string `json:"path"`
	BackupPath string `json:"backupPath"`
	SHA256     string `json:"sha256,omitempty"`
	PostSHA256 string `json:"postSha256,omitempty"`
	// PendingPostSHA256 records an in-flight predicted mutation while retaining
	// the last committed PostSHA256. Restore accepts either state after an
	// interrupted atomic write and normal completion clears this field.
	PendingPostSHA256 string `json:"pendingPostSha256,omitempty"`
	Size              int64  `json:"size,omitempty"`
	Directory         bool   `json:"directory,omitempty"`
	Symlink           bool   `json:"symlink,omitempty"`
	Absent            bool   `json:"absent,omitempty"`
	// Mode records the original permission bits for a regular-file or directory
	// entry. ModeKnown distinguishes an explicit 0000 mode and is required for
	// every current-schema regular-file and directory entry.
	Mode      uint32            `json:"mode,omitempty"`
	ModeKnown bool              `json:"modeKnown,omitempty"`
	TreeModes map[string]uint32 `json:"treeModes,omitempty"`
	// RecoveryPath is the run-local durable recovery payload while an exact
	// same-filesystem quarantine is active. Current writers set it to BackupPath;
	// the external quarantine pathname remains transient report metadata only.
	RecoveryPath string `json:"recoveryPath,omitempty"`
}

// Run summarizes a persisted backup run.
type Run struct {
	Version           int                `json:"version"`
	ProjectID         string             `json:"projectId"`
	RunID             string             `json:"runId"`
	Operation         string             `json:"operation"`
	ProjectRoot       string             `json:"projectRoot"`
	CreatedAt         time.Time          `json:"createdAt"`
	Path              string             `json:"path"`
	CommitTransaction *CommitTransaction `json:"commitTransaction,omitempty"`
	Entries           []Entry            `json:"entries,omitempty"`
}

// NewStore initializes a backup store for opts without writing files yet.
// Call LoadManifestGeneration before ordinary entry mutations or Save. Pending
// commits must use PublishPendingCommitGenerationFromRoot instead of entry APIs.
func NewStore(opts StoreOptions) (*Store, error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	backupRoot, err := ResolveBackupRootWithOverride(normalized.HomeDir, normalized.ThreadpointHome, normalized.BackupDir)
	if err != nil {
		return nil, err
	}
	return newStoreWithBackupRoot(normalized, backupRoot)
}

// NewStoreFromRoot constructs a backup run beneath a caller-retained
// product-home root without resolving or reopening the ambient home pathname.
// homePath is used only as the lexical absolute namespace recorded in paths;
// the borrowed root is the authority for all subsequent rooted operations.
func NewStoreFromRoot(homeRoot *os.Root, homePath string, opts StoreOptions) (*Store, error) {
	if homeRoot == nil {
		return nil, errors.New("pinned product-home root is required")
	}
	opened, err := homeRoot.Stat(".")
	if err != nil || !opened.IsDir() {
		return nil, errors.Join(errors.New("pinned product-home root is not a directory"), err)
	}
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	homePath, err = abspath.Abs(homePath)
	if err != nil {
		return nil, err
	}
	homePath = filepath.Clean(homePath)
	if homePath == string(filepath.Separator) {
		return nil, errors.New("threadpoint home cannot be filesystem root")
	}
	if strings.TrimSpace(normalized.ThreadpointHome) != "" {
		configured, err := abspath.Abs(normalized.ThreadpointHome)
		if err != nil || filepath.Clean(configured) != homePath {
			return nil, errors.Join(errors.New("configured threadpoint home does not match the pinned product-home namespace"), err)
		}
	}
	backupRoot, err := resolveBackupRootUnderHome(homePath, normalized.BackupDir)
	if err != nil {
		return nil, err
	}
	store, err := newStoreWithBackupRoot(normalized, backupRoot)
	if err != nil {
		return nil, err
	}
	store.borrowedHomeRoot = homeRoot
	store.borrowedHomePath = homePath
	return store, nil
}

// PublishPendingCommitGenerationFromRoot builds a complete pending commit run
// beneath a private, non-canonical directory and gives it its deterministic
// commit-<stage> name with one no-replace rename. Until that rename succeeds,
// backup enumeration and exact transaction recovery cannot observe a manifest
// or either payload. A colliding canonical generation is left untouched and is
// reported with os.ErrExist so callers can inspect it through
// LoadBoundCommitTransactionFromRoot instead of inferring or backfilling state.
func PublishPendingCommitGenerationFromRoot(homeRoot *os.Root, homePath string, opts StoreOptions, generation PendingCommitGeneration) (_ *Store, returnErr error) {
	if err := validateRunID(generation.Binding.StageID); err != nil {
		return nil, fmt.Errorf("invalid staged transaction id: %w", err)
	}
	runID := "commit-" + generation.Binding.StageID
	if opts.Operation != "" && opts.Operation != PendingCommitOperation {
		return nil, errors.New("pending commit generation requires the pending commit operation")
	}
	if opts.RunID != "" && opts.RunID != runID {
		return nil, errors.New("pending commit generation run id is not bound to the staged transaction")
	}
	if len(generation.SourceBody) > maxBoundCommitSourceBytes {
		return nil, fmt.Errorf("pending commit staged-source payload exceeds %d bytes", maxBoundCommitSourceBytes)
	}
	if generation.Binding.TargetSize > MaxCanonicalGenerationBytes {
		return nil, fmt.Errorf("pending commit target generation exceeds the %d-byte size limit", MaxCanonicalGenerationBytes)
	}
	if len(generation.PriorBody) > MaxCanonicalGenerationBytes {
		return nil, fmt.Errorf("sealed prior canonical generation exceeds the %d-byte size limit", MaxCanonicalGenerationBytes)
	}
	if !generation.PriorExists && (len(generation.PriorBody) != 0 || generation.PriorMode != 0) {
		return nil, errors.New("absent prior canonical generation cannot include body or mode metadata")
	}
	if generation.SourceMode&os.ModeType != 0 || generation.PriorMode&os.ModeType != 0 {
		return nil, errors.New("pending commit payload modes must contain permission bits only")
	}

	opts.Operation = PendingCommitOperation
	opts.RunID = runID
	store, err := NewStoreFromRoot(homeRoot, homePath, opts)
	if err != nil {
		return nil, err
	}
	binding := generation.Binding
	if err := store.ConfigurePendingCommitGeneration(binding.StageID, binding.SourcePath, binding.SourceSHA256, binding.TargetPath, binding.TargetSHA256, binding.TargetSize); err != nil {
		return nil, err
	}
	transaction := store.Manifest.CommitTransaction
	sourceSum := sha256.Sum256(generation.SourceBody)
	if hex.EncodeToString(sourceSum[:]) != transaction.SourceSHA256 {
		return nil, errors.New("pending commit staged-source body does not match its bound digest")
	}

	sourceEntry := pendingCommitPayloadEntry(store.RunDir, "stage-source", transaction.SourcePath, generation.SourceBody, generation.SourceMode)
	priorEntry := Entry{Role: "canonical-prior", Path: transaction.TargetPath, Absent: true}
	if generation.PriorExists {
		priorEntry = pendingCommitPayloadEntry(store.RunDir, "canonical-prior", transaction.TargetPath, generation.PriorBody, generation.PriorMode)
	}
	store.Manifest.Entries = []Entry{sourceEntry, priorEntry}
	if err := validateCurrentManifest(&store.Manifest); err != nil {
		return nil, fmt.Errorf("validate pending commit generation: %w", err)
	}
	if err := validateManifestNamespace(&store.Manifest, store.Manifest.ProjectRoot, store.ProjectID, store.RunID, store.RunDir); err != nil {
		return nil, fmt.Errorf("validate pending commit generation namespace: %w", err)
	}
	manifestBody, err := marshalManifest(store.Manifest)
	if err != nil {
		return nil, err
	}
	if len(manifestBody) > maxManifestBytes {
		return nil, fmt.Errorf("pending commit manifest exceeds %d bytes", maxManifestBytes)
	}

	backupRootRel, err := rootRelativePath(homePath, store.BackupRoot)
	if err != nil {
		return nil, err
	}
	projectRuns := filepath.Dir(store.RunDir)
	projectRunsRel, err := rootRelativePath(homePath, projectRuns)
	if err != nil {
		return nil, err
	}
	for _, rel := range []string{backupRootRel, projectRunsRel} {
		if err := ensurePrivateStateDirRoot(homeRoot, rel); err != nil {
			return nil, err
		}
	}
	projectRunsInfo, err := homeRoot.Lstat(projectRunsRel)
	if err != nil {
		return nil, err
	}
	projectRunsRoot, err := homeRoot.OpenRoot(projectRunsRel)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, projectRunsRoot.Close()) }()
	if err := verifyPendingCommitParentBinding(homeRoot, projectRunsRel, projectRunsInfo, projectRunsRoot); err != nil {
		return nil, err
	}
	if _, err := projectRunsRoot.Lstat(runID); err == nil {
		return nil, fmt.Errorf("pending commit generation %s already exists: %w", runID, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	temporaryName, err := makeBackupTempDirectory(projectRunsRoot, ".commit-pending-")
	if err != nil {
		return nil, err
	}
	temporaryInfo, err := projectRunsRoot.Lstat(temporaryName)
	if err != nil {
		return nil, err
	}
	temporaryRoot, err := projectRunsRoot.OpenRoot(temporaryName)
	if err != nil {
		return nil, err
	}
	openedTemporary, err := temporaryRoot.Stat(".")
	if err != nil || !temporaryInfo.IsDir() || temporaryInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(temporaryInfo, openedTemporary) {
		_ = temporaryRoot.Close()
		return nil, errors.Join(errors.New("private pending commit generation changed while it was opened"), err)
	}
	cleanupName := temporaryName
	cleanupActive := true
	cleanupLimits := DirectoryLimits{MaxEntries: 8}
	defer func() {
		if cleanupActive {
			returnErr = errors.Join(returnErr, cleanupBackupTempDirectory(projectRunsRoot, cleanupName, temporaryInfo, temporaryRoot, cleanupLimits))
		}
	}()

	for _, item := range []struct {
		entry Entry
		body  []byte
	}{
		{entry: sourceEntry, body: generation.SourceBody},
		{entry: priorEntry, body: generation.PriorBody},
	} {
		if item.entry.Absent {
			continue
		}
		payloadRel, err := filepath.Rel(store.RunDir, item.entry.BackupPath)
		if err != nil || payloadRel == "." || filepath.IsAbs(payloadRel) || strings.HasPrefix(payloadRel, ".."+string(filepath.Separator)) {
			return nil, errors.Join(errors.New("pending commit payload escapes its generation"), err)
		}
		if err := safefs.AtomicWriteRootFile(temporaryRoot, payloadRel, item.body, 0o600, nil); err != nil {
			return nil, err
		}
	}
	if err := safefs.AtomicWriteRootFile(temporaryRoot, ManifestName, manifestBody, 0o600, nil); err != nil {
		return nil, err
	}
	if err := verifyPendingCommitGeneration(temporaryRoot, store, manifestBody, generation); err != nil {
		return nil, err
	}
	if err := syncBackupRoot(temporaryRoot); err != nil {
		return nil, err
	}
	if pendingCommitGenerationBeforePublish != nil {
		if err := pendingCommitGenerationBeforePublish(projectRunsRoot, temporaryName); err != nil {
			return nil, err
		}
	}
	if err := verifyPendingCommitGeneration(temporaryRoot, store, manifestBody, generation); err != nil {
		return nil, fmt.Errorf("private pending commit generation changed before publication: %w", err)
	}
	currentTemporary, err := projectRunsRoot.Lstat(temporaryName)
	if err != nil || !currentTemporary.IsDir() || currentTemporary.Mode()&os.ModeSymlink != 0 || !os.SameFile(temporaryInfo, currentTemporary) {
		return nil, errors.Join(errors.New("private pending commit generation changed before publication"), err)
	}
	if err := verifyPendingCommitParentBinding(homeRoot, projectRunsRel, projectRunsInfo, projectRunsRoot); err != nil {
		return nil, err
	}
	if err := syncBackupRoot(projectRunsRoot); err != nil {
		return nil, err
	}
	if err := safefs.RenameRootNoReplace(projectRunsRoot, temporaryName, runID); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("pending commit generation %s already exists: %w", runID, os.ErrExist)
		}
		return nil, fmt.Errorf("publish complete pending commit generation without replacement: %w", err)
	}
	cleanupName = runID
	if err := verifyPendingCommitParentBinding(homeRoot, projectRunsRel, projectRunsInfo, projectRunsRoot); err != nil {
		return nil, err
	}
	publishedInfo, err := projectRunsRoot.Lstat(runID)
	if err != nil || !publishedInfo.IsDir() || publishedInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(temporaryInfo, publishedInfo) {
		return nil, errors.Join(errors.New("published pending commit generation changed identity"), err)
	}
	if err := verifyPendingCommitGeneration(temporaryRoot, store, manifestBody, generation); err != nil {
		return nil, fmt.Errorf("verify published pending commit generation: %w", err)
	}
	if err := syncBackupRoot(projectRunsRoot); err != nil {
		return nil, err
	}
	cleanupActive = false
	if err := temporaryRoot.Close(); err != nil {
		return store, err
	}
	temporaryRoot = nil
	store.rememberManifestGeneration(manifestBody)
	store.loadedRunInfo = publishedInfo
	return store, nil
}

func pendingCommitPayloadEntry(runDir, role, entryPath string, body []byte, mode os.FileMode) Entry {
	digest := sha256.Sum256(body)
	digestString := hex.EncodeToString(digest[:])
	pathIdentity := sha256.Sum256([]byte(role + "\x00" + entryPath))
	payloadName := safepath.SanitizeName(role) + "-" + hex.EncodeToString(pathIdentity[:])[:12] + "-" + digestString
	return Entry{
		Role:       role,
		Path:       entryPath,
		BackupPath: filepath.Join(runDir, "files", payloadName),
		SHA256:     digestString,
		Size:       int64(len(body)),
		Mode:       uint32(mode.Perm()),
		ModeKnown:  true,
	}
}

func verifyPendingCommitGeneration(root *os.Root, store *Store, expectedManifest []byte, generation PendingCommitGeneration) error {
	manifestBody, manifestInfo, err := ReadRootRegularFileBoundedInfo(root, ManifestName, maxManifestBytes)
	if err != nil {
		return err
	}
	if manifestInfo.Mode().Perm() != 0o600 || !bytes.Equal(manifestBody, expectedManifest) {
		return errors.New("pending commit manifest does not match its sealed private generation")
	}
	manifest, err := decodeManifest(manifestBody)
	if err != nil {
		return err
	}
	if err := validateManifestNamespace(manifest, store.Manifest.ProjectRoot, store.ProjectID, store.RunID, store.RunDir); err != nil {
		return err
	}
	wantBodies := map[string][]byte{
		entryKey("stage-source", manifest.CommitTransaction.SourcePath):    generation.SourceBody,
		entryKey("canonical-prior", manifest.CommitTransaction.TargetPath): generation.PriorBody,
	}
	for _, entry := range manifest.Entries {
		if entry.Absent {
			continue
		}
		payloadRel, err := filepath.Rel(store.RunDir, entry.BackupPath)
		if err != nil || payloadRel == "." || filepath.IsAbs(payloadRel) || strings.HasPrefix(payloadRel, ".."+string(filepath.Separator)) {
			return errors.Join(errors.New("pending commit payload escapes its generation"), err)
		}
		body, info, err := ReadRootRegularFileBoundedInfo(root, payloadRel, maxBoundCommitSourceBytes)
		if err != nil {
			return err
		}
		want, ok := wantBodies[entryKey(entry.Role, entry.Path)]
		if !ok || info.Mode().Perm() != 0o600 || !bytes.Equal(body, want) {
			return errors.New("pending commit payload does not match its sealed private generation")
		}
		digest := sha256.Sum256(body)
		if int64(len(body)) != entry.Size || hex.EncodeToString(digest[:]) != entry.SHA256 {
			return errors.New("pending commit payload does not match its manifest identity")
		}
	}
	return nil
}

func verifyPendingCommitParentBinding(homeRoot *os.Root, rel string, expected fs.FileInfo, parent *os.Root) error {
	current, currentErr := homeRoot.Lstat(rel)
	opened, openedErr := parent.Stat(".")
	if currentErr != nil || openedErr != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) || !os.SameFile(expected, opened) {
		return errors.Join(errors.New("backup project generation parent changed during publication"), currentErr, openedErr)
	}
	return nil
}

func syncBackupRoot(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func newStoreWithBackupRoot(normalized StoreOptions, backupRoot string) (*Store, error) {
	projectID := ProjectID(normalized.ProjectRoot)
	runDir := filepath.Join(backupRoot, projectID, normalized.RunID)
	if !pathWithin(runDir, filepath.Join(backupRoot, projectID)) {
		return nil, fmt.Errorf("backup run directory must stay under project backup root")
	}
	store := &Store{
		BackupRoot: backupRoot,
		ProjectID:  projectID,
		RunID:      normalized.RunID,
		RunDir:     runDir,
		Manifest: Manifest{
			Version:     ManifestVersion,
			Operation:   normalized.Operation,
			ProjectRoot: normalized.ProjectRoot,
			ProjectID:   projectID,
			RunID:       normalized.RunID,
			CreatedAt:   normalized.Now().UTC(),
		},
	}
	return store, nil
}

// LoadManifestGeneration reviews this store's current manifest and makes the
// next Save a conditional replacement of exactly that JSON generation. A
// missing manifest is remembered as an expected-absent generation, which makes
// first publication no-replace instead of overwriting a concurrent creator.
func (store *Store) LoadManifestGeneration() (*Manifest, error) {
	if store != nil && store.borrowedHomeRoot != nil {
		return store.LoadManifestGenerationFromRoot(store.borrowedHomeRoot, store.borrowedHomePath)
	}
	if err := store.validateRunDir(false); err != nil {
		return nil, err
	}
	body, err := ReadRegularFileBounded(filepath.Join(store.RunDir, ManifestName), maxManifestBytes)
	return store.adoptManifestGeneration(body, err)
}

// LoadManifestGenerationFromRoot is LoadManifestGeneration through a
// caller-retained product-home root.
func (store *Store) LoadManifestGenerationFromRoot(homeRoot *os.Root, homePath string) (*Manifest, error) {
	if err := store.validateRunDirRoot(homeRoot, homePath, false); err != nil {
		return nil, err
	}
	rel, err := rootRelativePath(homePath, filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		return nil, err
	}
	body, err := ReadRootRegularFileBounded(homeRoot, rel, maxManifestBytes)
	return store.adoptManifestGeneration(body, err)
}

func (store *Store) adoptManifestGeneration(body []byte, readErr error) (*Manifest, error) {
	if errors.Is(readErr, os.ErrNotExist) {
		store.manifestGenerationKnown = true
		store.persistedManifest = nil
		return nil, os.ErrNotExist
	}
	if readErr != nil {
		return nil, readErr
	}
	manifest, err := decodeManifest(body)
	if err != nil {
		return nil, err
	}
	if err := validateManifestNamespace(manifest, store.Manifest.ProjectRoot, store.ProjectID, store.RunID, store.RunDir); err != nil {
		return nil, err
	}
	store.Manifest = *manifest
	store.index = nil
	store.rememberManifestGeneration(body)
	return &store.Manifest, nil
}

// ConfigurePendingCommitGeneration binds a pending commit to the exact
// canonical generation length. The length lets prune prove that a finalized
// historical generation remains an unchanged prefix after later additive
// commits, rather than requiring every older run to equal the latest target.
func (store *Store) ConfigurePendingCommitGeneration(stageID, sourcePath, sourceSHA256, targetPath, targetSHA256 string, targetSize int64) error {
	if store == nil {
		return errors.New("backup store is required")
	}
	if targetSize <= 0 {
		return errors.New("commit target generation length must be positive")
	}
	if err := validateRunID(stageID); err != nil {
		return fmt.Errorf("invalid staged transaction id: %w", err)
	}
	sourcePath, err := normalizeEntryPath(sourcePath)
	if err != nil {
		return fmt.Errorf("invalid commit source path: %w", err)
	}
	targetPath, err = normalizeEntryPath(targetPath)
	if err != nil {
		return fmt.Errorf("invalid commit target path: %w", err)
	}
	sourceSHA256, err = normalizeSHA256Digest(sourceSHA256)
	if err != nil {
		return fmt.Errorf("invalid commit source digest: %w", err)
	}
	targetSHA256, err = normalizeSHA256Digest(targetSHA256)
	if err != nil {
		return fmt.Errorf("invalid commit target digest: %w", err)
	}
	store.Manifest.Operation = PendingCommitOperation
	store.Manifest.CommitTransaction = &CommitTransaction{
		State:        CommitTransactionPending,
		StageID:      stageID,
		SourcePath:   sourcePath,
		SourceSHA256: sourceSHA256,
		TargetPath:   targetPath,
		TargetSHA256: targetSHA256,
		TargetSize:   targetSize,
	}
	return nil
}

// FinalizePendingCommit atomically makes a prepared commit manifest visible to
// prune and restore after canonical publication has succeeded.
func (store *Store) FinalizePendingCommit() error {
	return store.transitionCommitTransaction(CommitTransactionCommitted, CommittedCommitOperation, store.saveExpected)
}

// FinalizePendingCommitFromRoot is FinalizePendingCommit through a
// caller-retained product-home root.
func (store *Store) FinalizePendingCommitFromRoot(homeRoot *os.Root, homePath string) error {
	return store.transitionCommitTransaction(CommitTransactionCommitted, CommittedCommitOperation, func(expected []byte) error {
		return store.saveExpectedFromRoot(homeRoot, homePath, expected)
	})
}

// FinalizePendingCommitRecovery atomically records a successful canonical
// publication while keeping its manifest invisible to ordinary restore and
// prune selection. The run remains available only to exact transaction
// recovery by its deterministic identifier.
func (store *Store) FinalizePendingCommitRecovery() error {
	return store.transitionCommitTransaction(CommitTransactionRecoveryCommitted, RecoveryCommitOperation, store.saveExpected)
}

// FinalizePendingCommitRecoveryFromRoot is
// FinalizePendingCommitRecovery through a caller-retained product-home root.
func (store *Store) FinalizePendingCommitRecoveryFromRoot(homeRoot *os.Root, homePath string) error {
	return store.transitionCommitTransaction(CommitTransactionRecoveryCommitted, RecoveryCommitOperation, func(expected []byte) error {
		return store.saveExpectedFromRoot(homeRoot, homePath, expected)
	})
}

// AbortPendingCommit writes a prune-invisible tombstone for a prepared or
// finalized manifest whose canonical transaction failed or was rolled back.
func (store *Store) AbortPendingCommit() error {
	return store.transitionCommitTransaction(CommitTransactionAborted, AbortedCommitOperation, store.saveExpected)
}

// AbortPendingCommitFromRoot is AbortPendingCommit through a caller-retained
// product-home root.
func (store *Store) AbortPendingCommitFromRoot(homeRoot *os.Root, homePath string) error {
	return store.transitionCommitTransaction(CommitTransactionAborted, AbortedCommitOperation, func(expected []byte) error {
		return store.saveExpectedFromRoot(homeRoot, homePath, expected)
	})
}

func (store *Store) transitionCommitTransaction(state, operation string, save func(expected []byte) error) error {
	if store == nil || store.Manifest.CommitTransaction == nil || save == nil {
		return errors.New("configured commit transaction is required")
	}
	if store.Manifest.CommitTransaction.State == state && store.Manifest.Operation == operation {
		return nil
	}
	if (state == CommitTransactionCommitted || state == CommitTransactionRecoveryCommitted) && store.Manifest.CommitTransaction.State != CommitTransactionPending {
		return fmt.Errorf("cannot finalize commit transaction from state %q", store.Manifest.CommitTransaction.State)
	}
	originalOperation := store.Manifest.Operation
	originalTransaction := *store.Manifest.CommitTransaction
	expected, err := store.expectedManifestGeneration()
	if err != nil {
		return err
	}
	store.Manifest.Operation = operation
	store.Manifest.CommitTransaction.State = state
	if err := validateCurrentManifest(&store.Manifest); err != nil {
		store.Manifest.Operation = originalOperation
		*store.Manifest.CommitTransaction = originalTransaction
		return fmt.Errorf("refuse incomplete commit transaction transition: %w", err)
	}
	if err := save(expected); err != nil {
		store.Manifest.Operation = originalOperation
		*store.Manifest.CommitTransaction = originalTransaction
		return err
	}
	return nil
}

func normalizeSHA256Digest(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return "", errors.Join(errors.New("digest must be 64 hexadecimal characters"), err)
	}
	return raw, nil
}

// CommitTransactionMatches reports whether manifest is bound to the supplied
// state, staged source generation, and canonical publication bytes.
func CommitTransactionMatches(manifest *Manifest, state, stageID, sourcePath, sourceSHA256, targetPath, targetSHA256 string) bool {
	if manifest == nil || manifest.CommitTransaction == nil {
		return false
	}
	sourcePath, sourceErr := normalizeEntryPath(sourcePath)
	targetPath, targetErr := normalizeEntryPath(targetPath)
	sourceSHA256, sourceHashErr := normalizeSHA256Digest(sourceSHA256)
	targetSHA256, targetHashErr := normalizeSHA256Digest(targetSHA256)
	if sourceErr != nil || targetErr != nil || sourceHashErr != nil || targetHashErr != nil {
		return false
	}
	transaction := manifest.CommitTransaction
	return transaction.State == state && transaction.StageID == stageID &&
		transaction.SourcePath == sourcePath && transaction.SourceSHA256 == sourceSHA256 &&
		transaction.TargetPath == targetPath && transaction.TargetSHA256 == targetSHA256
}

// LoadBoundCommitTransactionFromRoot loads and CAS-pins the exact manifest
// generation for binding beneath a retained product-home root. It accepts only
// a deterministic commit-<stage> run, a coherent pending/committed/aborted
// operation-state pair, the exact target hash and length, and a matching sealed
// stage-source entry. Every current-schema state seals exactly one prior marker;
// incomplete state is rejected without repair or mutation.
// Callers may safely inspect the returned Store before deciding whether exact
// publish-artifact recovery should resume or roll back.
func LoadBoundCommitTransactionFromRoot(homeRoot *os.Root, homePath string, opts StoreOptions, binding CommitBinding) (*BoundCommitTransaction, error) {
	if err := validateRunID(binding.StageID); err != nil {
		return nil, fmt.Errorf("invalid staged transaction id: %w", err)
	}
	if opts.RunID != "commit-"+binding.StageID {
		return nil, errors.New("commit recovery run id is not bound to the staged transaction")
	}
	if (strings.TrimSpace(binding.TargetSHA256) == "") != (binding.TargetSize == 0) || binding.TargetSize < 0 {
		return nil, errors.New("commit recovery target hash and size must either both be supplied or both be omitted")
	}
	store, err := NewStoreFromRoot(homeRoot, homePath, opts)
	if err != nil {
		return nil, err
	}
	runRel, err := rootRelativePath(homePath, store.RunDir)
	if err != nil {
		return nil, err
	}
	runInfo, err := homeRoot.Lstat(runRel)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	if !runInfo.IsDir() || runInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("commit recovery run is not a physical directory")
	}
	runRoot, err := homeRoot.OpenRoot(runRel)
	if err != nil {
		return nil, err
	}
	defer runRoot.Close()
	openedRun, err := runRoot.Stat(".")
	if err != nil || !os.SameFile(runInfo, openedRun) {
		return nil, errors.Join(errors.New("commit recovery run changed while it was opened"), err)
	}
	manifestBody, err := ReadRootRegularFileBounded(runRoot, ManifestName, maxManifestBytes)
	manifest, err := store.adoptManifestGeneration(manifestBody, err)
	if err != nil {
		return nil, err
	}
	transaction := manifest.CommitTransaction
	if transaction == nil || transaction.TargetSize <= 0 {
		return nil, errors.New("commit recovery manifest does not bind a canonical generation length")
	}
	if transaction.TargetSize > MaxCanonicalGenerationBytes {
		return nil, fmt.Errorf("bound canonical generation exceeds the %d-byte size limit", MaxCanonicalGenerationBytes)
	}
	targetSHA256 := binding.TargetSHA256
	targetSize := binding.TargetSize
	if targetSize == 0 {
		targetSHA256 = transaction.TargetSHA256
		targetSize = transaction.TargetSize
	}
	if transaction.TargetSize != targetSize {
		return nil, errors.New("commit recovery manifest does not match the canonical generation length")
	}
	wantOperation := map[string]string{
		CommitTransactionPending:           PendingCommitOperation,
		CommitTransactionCommitted:         CommittedCommitOperation,
		CommitTransactionAborted:           AbortedCommitOperation,
		CommitTransactionRecoveryCommitted: RecoveryCommitOperation,
	}[transaction.State]
	if wantOperation == "" || manifest.Operation != wantOperation || !CommitTransactionMatches(manifest, transaction.State, binding.StageID, binding.SourcePath, binding.SourceSHA256, binding.TargetPath, targetSHA256) {
		return nil, errors.New("commit recovery manifest does not match the staged source and canonical generation")
	}
	cleanSource, err := normalizeEntryPath(binding.SourcePath)
	if err != nil {
		return nil, err
	}
	if len(manifest.Entries) != 2 {
		return nil, errors.New("commit recovery manifest must contain exactly one staged source and exactly one prior canonical generation")
	}
	stageSourceCount := 0
	for _, entry := range manifest.Entries {
		switch entry.Role {
		case "stage-source":
			stageSourceCount++
			if entry.Path != cleanSource || entry.SHA256 != strings.ToLower(strings.TrimSpace(binding.SourceSHA256)) || entry.BackupPath == "" || entry.Absent || entry.Directory || entry.Symlink {
				return nil, errors.New("commit recovery manifest is missing its exact staged-source payload entry")
			}
			if entry.Size < 0 || entry.Size > maxBoundCommitSourceBytes {
				return nil, errors.New("commit recovery staged-source payload size is invalid")
			}
			payloadRel, err := filepath.Rel(store.RunDir, entry.BackupPath)
			if err != nil || payloadRel == "." || filepath.IsAbs(payloadRel) || strings.HasPrefix(payloadRel, ".."+string(filepath.Separator)) {
				return nil, errors.Join(errors.New("commit recovery staged-source payload escapes its run"), err)
			}
			body, info, err := ReadRootRegularFileBoundedInfo(runRoot, payloadRel, maxBoundCommitSourceBytes)
			if err != nil {
				return nil, fmt.Errorf("verify commit recovery staged-source payload: %w", err)
			}
			sum := sha256.Sum256(body)
			if int64(len(body)) != entry.Size || hex.EncodeToString(sum[:]) != entry.SHA256 || info.Mode().Perm() != 0o600 {
				return nil, errors.New("commit recovery staged-source payload does not match its sealed identity")
			}
		case "canonical-prior":
			if entry.Path != transaction.TargetPath {
				return nil, errors.New("commit recovery manifest has a prior entry for a different canonical target")
			}
		default:
			return nil, errors.New("commit recovery manifest contains an unsupported entry")
		}
	}
	if stageSourceCount != 1 {
		return nil, errors.New("commit recovery manifest is missing its exact staged-source payload entry")
	}
	result := &BoundCommitTransaction{Store: store, State: transaction.State, TargetSHA256: transaction.TargetSHA256, TargetSize: transaction.TargetSize}
	priorCount := 0
	for _, entry := range manifest.Entries {
		if entry.Role != "canonical-prior" || entry.Path != transaction.TargetPath {
			continue
		}
		priorCount++
		if entry.Absent {
			if entry.BackupPath != "" || entry.SHA256 != "" || entry.Size != 0 || entry.Directory || entry.Symlink {
				return nil, errors.New("commit recovery prior-absent marker is malformed")
			}
			result.PriorKnown = true
			continue
		}
		if entry.Directory || entry.Symlink || !entry.ModeKnown || entry.Size < 0 || entry.BackupPath == "" {
			return nil, errors.New("commit recovery prior canonical entry is malformed")
		}
		if entry.Size > MaxCanonicalGenerationBytes {
			return nil, fmt.Errorf("sealed prior canonical generation exceeds the %d-byte size limit", MaxCanonicalGenerationBytes)
		}
		payloadRel, err := filepath.Rel(store.RunDir, entry.BackupPath)
		if err != nil || payloadRel == "." || filepath.IsAbs(payloadRel) || strings.HasPrefix(payloadRel, ".."+string(filepath.Separator)) {
			return nil, errors.Join(errors.New("commit recovery prior canonical payload escapes its run"), err)
		}
		body, info, err := ReadRootRegularFileBoundedInfo(runRoot, payloadRel, MaxCanonicalGenerationBytes)
		if err != nil {
			return nil, fmt.Errorf("verify commit recovery prior canonical payload: %w", err)
		}
		sum := sha256.Sum256(body)
		if int64(len(body)) != entry.Size || hex.EncodeToString(sum[:]) != entry.SHA256 || info.Mode().Perm() != 0o600 {
			return nil, errors.New("commit recovery prior canonical payload does not match its sealed identity")
		}
		result.PriorExists = true
		result.PriorKnown = true
		result.PriorBody = body
		result.PriorMode = os.FileMode(entry.Mode).Perm()
	}
	if priorCount != 1 {
		return nil, errors.New("commit recovery manifest must contain exactly one prior canonical generation")
	}
	currentRun, err := homeRoot.Lstat(runRel)
	if err != nil || !currentRun.IsDir() || currentRun.Mode()&os.ModeSymlink != 0 || !os.SameFile(runInfo, currentRun) {
		return nil, errors.Join(errors.New("commit recovery run changed during validation"), err)
	}
	store.loadedRunInfo = runInfo
	return result, nil
}

// ResolveBackupRoot returns the backup root under the resolved threadpoint home.
func ResolveBackupRoot(homeDir string, backupDir string) (string, error) {
	return ResolveBackupRootWithOverride(homeDir, "", backupDir)
}

// ResolveBackupRootWithOverride returns the backup root under the resolved
// threadpoint home, preferring explicitThreadpointHome before environment and
// defaults.
func ResolveBackupRootWithOverride(homeDir string, explicitThreadpointHome string, backupDir string) (string, error) {
	threadpointHome, err := ResolveThreadpointHomeWithOverride(homeDir, explicitThreadpointHome)
	if err != nil {
		return "", err
	}
	return resolveBackupRootUnderHome(threadpointHome, backupDir)
}

func resolveBackupRootUnderHome(threadpointHome, backupDir string) (string, error) {
	defaultRoot := filepath.Join(threadpointHome, "backups")
	if strings.TrimSpace(backupDir) == "" {
		return defaultRoot, nil
	}
	if filepath.IsAbs(backupDir) {
		return "", fmt.Errorf("backup directory must be relative to %s", defaultRoot)
	}
	clean := filepath.Clean(backupDir)
	if clean == "." {
		return defaultRoot, nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("backup directory must stay under %s", defaultRoot)
	}
	return filepath.Join(defaultRoot, clean), nil
}

// ResolveThreadpointHome returns threadpoint's home directory for backups and locks.
func ResolveThreadpointHome(homeDir string) (string, error) {
	return home.Resolve(homeDir)
}

// ResolveThreadpointHomeWithOverride returns threadpoint's home directory for
// backups and locks, preferring explicitThreadpointHome before environment and
// defaults.
func ResolveThreadpointHomeWithOverride(homeDir string, explicitThreadpointHome string) (string, error) {
	return home.ResolveWithOverride(homeDir, explicitThreadpointHome)
}

// ProjectID returns a stable path-derived identifier for projectRoot.
func ProjectID(projectRoot string) string {
	clean := filepath.Clean(projectRoot)
	sum := sha256.Sum256([]byte(filepath.ToSlash(clean)))
	base := safepath.SanitizeName(filepath.Base(clean))
	if base == "" {
		base = "project"
	}
	return base + "-" + hex.EncodeToString(sum[:])[:12]
}

// DefaultRunID returns the timestamped default backup run identifier.
func DefaultRunID(operation string, now time.Time) string {
	operation = safepath.SanitizeName(operation)
	if operation == "" {
		operation = "backup"
	}
	return operation + "-" + now.UTC().Format("20060102T150405.000000000Z")
}

// FileSpec describes a single file to back up. Role labels why the file
// was captured; Root is optional and, when set, records the project root the
// file belongs to. Named fields keep the adjacent string arguments from being
// transposed at call sites.
type FileSpec struct {
	Rel    string
	Source string
	Role   string
	Root   string
}

// BackupFile copies source into the store and records it with role.
func (store *Store) BackupFile(rel string, source string, role string) (*Entry, error) {
	return store.BackupFileFromRoot(FileSpec{Rel: rel, Source: source, Role: role})
}

// BackupFileFromRoot copies a file into the store and records its optional root.
func (store *Store) BackupFileFromRoot(spec FileSpec) (*Entry, error) {
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	if err := store.validateRunDirForMutation(); err != nil {
		return nil, err
	}
	rel, err := normalizeEntryPath(spec.Rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(spec.Source)
	if err != nil {
		return nil, err
	}
	entry := Entry{
		Role:      spec.Role,
		Path:      rel,
		Size:      info.Size(),
		Directory: info.IsDir(),
		Symlink:   info.Mode()&os.ModeSymlink != 0,
	}
	if !entry.Symlink {
		entry.Mode = uint32(info.Mode().Perm())
		entry.ModeKnown = true
	}
	if strings.TrimSpace(spec.Root) != "" {
		entry.Root = filepath.Clean(spec.Root)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("BackupFile cannot back up directory %s", spec.Source)
	}
	if entry.Symlink {
		linkTarget, err := os.Readlink(spec.Source)
		if err != nil {
			return nil, err
		}
		after, err := os.Lstat(spec.Source)
		if err != nil || after.Mode()&os.ModeSymlink == 0 || !os.SameFile(info, after) {
			return nil, fmt.Errorf("backup symlink changed while it was read: %s", spec.Source)
		}
		confirmedTarget, err := os.Readlink(spec.Source)
		if err != nil || confirmedTarget != linkTarget {
			return nil, fmt.Errorf("backup symlink changed while it was read: %s", spec.Source)
		}
		marker := []byte(linkTarget + "\n")
		sum := sha256.Sum256(marker)
		entry.SHA256 = hex.EncodeToString(sum[:])
		entry.BackupPath = filepath.Join(store.RunDir, "files", backupPayloadRelWithIdentity(spec.Role, rel, entry.SHA256)) + ".symlink"
		entry.Size = int64(len(marker))
		var publishErr error
		if store.borrowedHomeRoot != nil {
			publishErr = publishBackupRegularPayloadRoot(store.borrowedHomeRoot, store.borrowedHomePath, entry.BackupPath, marker)
		} else {
			publishErr = publishBackupRegularPayload(entry.BackupPath, marker)
		}
		if publishErr != nil {
			return nil, publishErr
		}
	} else {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("BackupFile requires a regular file or symlink: %s", spec.Source)
		}
		sourceAbsolute, err := abspath.Abs(spec.Source)
		if err != nil {
			return nil, err
		}
		sourceParent, err := os.OpenRoot(filepath.Dir(sourceAbsolute))
		if err != nil {
			return nil, err
		}
		body, readErr := ReadRootRegularFileBounded(sourceParent, filepath.Base(sourceAbsolute), info.Size())
		closeErr := sourceParent.Close()
		if readErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		sum := sha256.Sum256(body)
		entry.SHA256 = hex.EncodeToString(sum[:])
		entry.BackupPath = filepath.Join(store.RunDir, "files", backupPayloadRelWithIdentity(spec.Role, rel, entry.SHA256))
		entry.Size = int64(len(body))
		var publishErr error
		if store.borrowedHomeRoot != nil {
			publishErr = publishBackupRegularPayloadRoot(store.borrowedHomeRoot, store.borrowedHomePath, entry.BackupPath, body)
		} else {
			publishErr = publishBackupRegularPayload(entry.BackupPath, body)
		}
		if publishErr != nil {
			return nil, publishErr
		}
	}
	return store.commitEntry(entry)
}

// BackupBytes records an already-read regular file body with an explicit
// owner-only mode. Callers that must preserve a source mode use
// BackupBytesWithMode instead.
func (store *Store) BackupBytes(rel string, body []byte, role string) (*Entry, error) {
	return store.backupBytesWithMode(rel, body, role, 0o600, true)
}

// BackupBytesWithMode records an already-read regular file body together with
// its captured original permission bits. It is for transactional callers that
// already hold a validated target snapshot and must not reopen its pathname.
func (store *Store) BackupBytesWithMode(rel string, body []byte, role string, mode os.FileMode) (*Entry, error) {
	return store.backupBytesWithMode(rel, body, role, uint32(mode.Perm()), true)
}

// BackupBytesWithModeFromRoot records bytes beneath a caller-retained product
// home root. The root is treated as the filesystem capability; homePath is used
// only to derive relative manifest and payload names and is never reopened.
func (store *Store) BackupBytesWithModeFromRoot(homeRoot *os.Root, homePath, rel string, body []byte, role string, mode os.FileMode) (*Entry, error) {
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	if err := store.validateRunDirForMutationRoot(homeRoot, homePath); err != nil {
		return nil, err
	}
	cleanRel, err := normalizeEntryPath(rel)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	entry := Entry{
		Role:       role,
		Path:       cleanRel,
		BackupPath: filepath.Join(store.RunDir, "files", backupPayloadRelWithIdentity(role, cleanRel, hex.EncodeToString(sum[:]))),
		SHA256:     hex.EncodeToString(sum[:]),
		Size:       int64(len(body)),
		Mode:       uint32(mode.Perm()),
		ModeKnown:  true,
	}
	if err := publishBackupRegularPayloadRoot(homeRoot, homePath, entry.BackupPath, body); err != nil {
		return nil, err
	}
	return store.commitEntryFromRoot(homeRoot, homePath, entry)
}

func (store *Store) backupBytesWithMode(rel string, body []byte, role string, mode uint32, modeKnown bool) (*Entry, error) {
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	if err := store.validateRunDirForMutation(); err != nil {
		return nil, err
	}
	cleanRel, err := normalizeEntryPath(rel)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	entry := Entry{
		Role:       role,
		Path:       cleanRel,
		BackupPath: filepath.Join(store.RunDir, "files", backupPayloadRelWithIdentity(role, cleanRel, hex.EncodeToString(sum[:]))),
		SHA256:     hex.EncodeToString(sum[:]),
		Size:       int64(len(body)),
		Mode:       mode,
		ModeKnown:  modeKnown,
	}
	var publishErr error
	if store.borrowedHomeRoot != nil {
		publishErr = publishBackupRegularPayloadRoot(store.borrowedHomeRoot, store.borrowedHomePath, entry.BackupPath, body)
	} else {
		publishErr = publishBackupRegularPayload(entry.BackupPath, body)
	}
	if publishErr != nil {
		return nil, publishErr
	}
	return store.commitEntry(entry)
}

// BackupSymlinkTarget records an already-read symlink target without reopening
// a pathname that may have changed after validation.
func (store *Store) BackupSymlinkTarget(rel string, linkTarget string, role string) (*Entry, error) {
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	if err := store.validateRunDirForMutation(); err != nil {
		return nil, err
	}
	cleanRel, err := normalizeEntryPath(rel)
	if err != nil {
		return nil, err
	}
	marker := []byte(linkTarget + "\n")
	sum := sha256.Sum256(marker)
	entry := Entry{
		Role:       role,
		Path:       cleanRel,
		BackupPath: filepath.Join(store.RunDir, "files", backupPayloadRelWithIdentity(role, cleanRel, hex.EncodeToString(sum[:]))) + ".symlink",
		SHA256:     hex.EncodeToString(sum[:]),
		Size:       int64(len(marker)),
		Symlink:    true,
	}
	var publishErr error
	if store.borrowedHomeRoot != nil {
		publishErr = publishBackupRegularPayloadRoot(store.borrowedHomeRoot, store.borrowedHomePath, entry.BackupPath, marker)
	} else {
		publishErr = publishBackupRegularPayload(entry.BackupPath, marker)
	}
	if publishErr != nil {
		return nil, publishErr
	}
	return store.commitEntry(entry)
}

// BackupDir copies a directory tree into the store and records it with role.
func (store *Store) BackupDir(rel string, source string, role string) (*Entry, error) {
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	if err := store.validateRunDirForMutation(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("BackupDir requires a directory: %s", source)
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return nil, err
	}
	defer sourceRoot.Close()
	opened, err := sourceRoot.Stat(".")
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("backup directory changed before it could be opened: %s", source)
	}
	return store.BackupRootDir(rel, sourceRoot, ".", role)
}

// BackupRootDir copies a directory from a pinned filesystem root into a sealed
// backup payload. The payload is built privately, hashed from its published
// paths, types, file bytes, and symlink targets, and only then renamed into the
// run's stable directory layout.
func (store *Store) BackupRootDir(rel string, source *os.Root, sourceRel string, role string) (*Entry, error) {
	return store.BackupRootDirWithLimits(rel, source, sourceRel, role, DirectoryLimits{})
}

// BackupRootDirWithLimits is BackupRootDir with explicit entry, aggregate
// regular-byte, and per-file budgets. The limits are enforced independently
// while copying the source and while hashing the private payload, so a source
// that grows after an earlier validation cannot turn backup publication into
// unbounded work.
func (store *Store) BackupRootDirWithLimits(rel string, source *os.Root, sourceRel string, role string, limits DirectoryLimits) (*Entry, error) {
	return store.backupRootDirWithLimitsAndHooks(rel, source, sourceRel, role, limits, backupRootDirHooks{})
}

type backupRootDirHooks struct {
	afterCopyEntry    func(sourcePath string) error
	afterSourceDigest func(temporary *os.Root) error
	afterParentOpen   func(parentPath string) error
}

func (store *Store) backupRootDirWithLimitsAndHooks(rel string, source *os.Root, sourceRel string, role string, limits DirectoryLimits, hooks backupRootDirHooks) (savedEntry *Entry, returnErr error) {
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	limits, err := normalizeDirectoryLimits(limits)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("backup source root is required")
	}
	if err := store.validateRunDirForMutation(); err != nil {
		return nil, err
	}
	cleanRel, err := normalizeEntryPath(rel)
	if err != nil {
		return nil, err
	}
	cleanSource, err := cleanRootRelativePath(sourceRel)
	if err != nil {
		return nil, err
	}
	info, err := source.Lstat(filepath.FromSlash(cleanSource))
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("BackupRootDir requires a real directory: %s", sourceRel)
	}
	sourceModes, err := captureRootDirectoryModes(source, cleanSource, limits)
	if err != nil {
		return nil, err
	}

	stableRel := backupPayloadRel(role, cleanRel)
	parent := filepath.Join(store.RunDir, "dirs", filepath.Dir(stableRel))
	var parentInfo fs.FileInfo
	var parentRoot *os.Root
	if store.borrowedHomeRoot != nil {
		parentRel, err := rootRelativePath(store.borrowedHomePath, parent)
		if err != nil {
			return nil, err
		}
		if err := safefs.RejectRootSymlinkAncestors(store.borrowedHomeRoot, parentRel); err != nil {
			return nil, err
		}
		// #nosec G301 -- this payload is nested beneath the owner-only backup root.
		if err := store.borrowedHomeRoot.MkdirAll(parentRel, 0o755); err != nil {
			return nil, err
		}
		parentInfo, err = store.borrowedHomeRoot.Lstat(parentRel)
		if err != nil {
			return nil, err
		}
		parentRoot, err = store.borrowedHomeRoot.OpenRoot(parentRel)
		if err != nil {
			return nil, err
		}
	} else {
		if err := safefs.RejectExistingSymlinkAncestors(parent); err != nil {
			return nil, err
		}
		// #nosec G301 -- backup payload directories contain user-visible project snapshots.
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, err
		}
		if err := safefs.RejectExistingSymlinkAncestors(parent); err != nil {
			return nil, err
		}
		parentInfo, err = os.Lstat(parent)
		if err != nil {
			return nil, err
		}
		parentRoot, err = os.OpenRoot(parent)
		if err != nil {
			return nil, err
		}
	}
	defer parentRoot.Close()
	openedParent, err := parentRoot.Stat(".")
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(parentInfo, openedParent) {
		return nil, errors.Join(errors.New("backup directory payload parent changed while it was opened"), err)
	}
	if hooks.afterParentOpen != nil {
		if err := hooks.afterParentOpen(parent); err != nil {
			return nil, err
		}
	}
	temporaryName, err := makeBackupTempDirectory(parentRoot, "."+filepath.Base(stableRel)+".tmp-")
	if err != nil {
		return nil, err
	}
	temporaryInfo, err := parentRoot.Lstat(temporaryName)
	if err != nil {
		return nil, err
	}
	temporaryRoot, err := parentRoot.OpenRoot(temporaryName)
	if err != nil {
		return nil, err
	}
	openedTemporary, err := temporaryRoot.Stat(".")
	if err != nil || !temporaryInfo.IsDir() || temporaryInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(temporaryInfo, openedTemporary) {
		_ = temporaryRoot.Close()
		return nil, errors.Join(errors.New("private backup directory changed while it was opened"), err)
	}
	temporaryActive := true
	defer func() {
		if temporaryActive {
			returnErr = errors.Join(returnErr, cleanupBackupTempDirectory(parentRoot, temporaryName, temporaryInfo, temporaryRoot, limits))
		}
	}()
	if err := copyRootDirectory(source, cleanSource, temporaryRoot, limits, hooks); err != nil {
		return nil, err
	}
	confirmedSourceModes, err := captureRootDirectoryModes(source, cleanSource, limits)
	if err != nil {
		return nil, err
	}
	if !sameDirectoryModes(sourceModes, confirmedSourceModes) {
		return nil, fmt.Errorf("backup source permissions changed while its sealed directory payload was created: %s", sourceRel)
	}
	// #nosec G302 -- published backup directories preserve the existing user-visible 0755 layout.
	if err := parentRoot.Chmod(temporaryName, 0o755); err != nil {
		return nil, err
	}
	digest, err := RootDirSHA256WithLimits(temporaryRoot, ".", limits)
	if err != nil {
		return nil, err
	}
	sourceDigest, err := RootDirSHA256WithLimits(source, cleanSource, limits)
	if err != nil {
		return nil, err
	}
	if sourceDigest != digest {
		return nil, fmt.Errorf("backup source changed while its sealed directory payload was created: %s", sourceRel)
	}
	if hooks.afterSourceDigest != nil {
		if err := hooks.afterSourceDigest(temporaryRoot); err != nil {
			return nil, err
		}
	}
	finalDigest, err := RootDirSHA256WithLimits(temporaryRoot, ".", limits)
	if err != nil {
		return nil, err
	}
	if finalDigest != digest {
		return nil, fmt.Errorf("private backup payload changed before publication: %s", sourceRel)
	}
	currentTemporary, err := parentRoot.Lstat(temporaryName)
	if err != nil || !currentTemporary.IsDir() || currentTemporary.Mode()&os.ModeSymlink != 0 || !os.SameFile(temporaryInfo, currentTemporary) {
		return nil, errors.Join(errors.New("private backup directory changed before publication"), err)
	}
	finalName := filepath.Base(backupPayloadRelWithIdentity(role, cleanRel, digest))
	finalPath := filepath.Join(parent, finalName)
	if err := safefs.RenameRootNoReplace(parentRoot, temporaryName, finalName); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("publish content-addressed backup directory without replacement: %w", err)
		}
		if err := verifyBackupDirectoryPayload(parentRoot, finalName, finalPath, digest, limits, nil); err != nil {
			return nil, fmt.Errorf("content-addressed backup directory collision: %w", err)
		}
	} else {
		temporaryActive = false
		if err := temporaryRoot.Close(); err != nil {
			return nil, err
		}
		temporaryRoot = nil
		if err := verifyBackupDirectoryPayload(parentRoot, finalName, finalPath, digest, limits, temporaryInfo); err != nil {
			return nil, err
		}
	}
	if store.borrowedHomeRoot != nil {
		parentRel, relErr := rootRelativePath(store.borrowedHomePath, parent)
		if relErr != nil {
			return nil, relErr
		}
		current, currentErr := store.borrowedHomeRoot.Lstat(parentRel)
		opened, openedErr := parentRoot.Stat(".")
		if currentErr != nil || openedErr != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(parentInfo, current) || !os.SameFile(parentInfo, opened) {
			return nil, errors.Join(errors.New("backup directory payload parent changed during rooted publication"), currentErr, openedErr)
		}
	} else if err := verifyBackupDirectoryParentBinding(parent, parentInfo, parentRoot); err != nil {
		return nil, err
	}

	entry := Entry{
		Role:       role,
		Path:       cleanRel,
		BackupPath: finalPath,
		SHA256:     digest,
		Directory:  true,
		Mode:       sourceModes["."],
		ModeKnown:  true,
		TreeModes:  sourceModes,
	}
	return store.commitEntry(entry)
}

func captureRootDirectoryModes(root *os.Root, sourceRel string, limits DirectoryLimits) (map[string]uint32, error) {
	clean, err := cleanRootRelativePath(sourceRel)
	if err != nil {
		return nil, err
	}
	modes := map[string]uint32{}
	err = walkRootDirectoryWithHooks(root, clean, rootWalkLimitsForDirectory(limits), "directory mode capture", rootWalkHooks{}, func(_ *os.Root, _ string, sourcePath string, info fs.FileInfo) error {
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(filepath.FromSlash(clean), filepath.FromSlash(sourcePath))
		if err != nil {
			return err
		}
		modes[filepath.ToSlash(rel)] = uint32(info.Mode().Perm())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return modes, nil
}

func sameDirectoryModes(left, right map[string]uint32) bool {
	if len(left) != len(right) {
		return false
	}
	for path, mode := range left {
		if right[path] != mode {
			return false
		}
	}
	return true
}

func makeBackupTempDirectory(root *os.Root, prefix string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		name := prefix + hex.EncodeToString(token[:])
		if err := root.Mkdir(name, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errors.New("could not allocate a private backup directory")
}

func verifyBackupDirectoryParentBinding(path string, expected fs.FileInfo, root *os.Root) error {
	if err := safefs.RejectExistingSymlinkAncestors(path); err != nil {
		return fmt.Errorf("backup directory payload parent changed during publication: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	opened, err := root.Stat(".")
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) || !os.SameFile(expected, opened) {
		return errors.Join(errors.New("backup directory payload parent changed during publication"), err)
	}
	return nil
}

func verifyBackupDirectoryPayload(parent *os.Root, name string, ambientPath string, digest string, limits DirectoryLimits, expected fs.FileInfo) error {
	info, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (expected != nil && !os.SameFile(expected, info)) {
		return fmt.Errorf("published backup payload is not the expected real directory: %s", ambientPath)
	}
	actual, err := RootDirSHA256WithLimits(parent, name, limits)
	if err != nil {
		return err
	}
	after, statErr := parent.Lstat(name)
	if statErr != nil || !os.SameFile(info, after) || actual != digest {
		return errors.Join(fmt.Errorf("published backup directory does not match its content identity: %s", ambientPath), statErr)
	}
	return nil
}

func cleanupBackupTempDirectory(parent *os.Root, name string, expected fs.FileInfo, temporary *os.Root, limits DirectoryLimits) error {
	if temporary == nil {
		return nil
	}
	type cleanupEntry struct {
		name string
		info fs.FileInfo
	}
	entries := make([]cleanupEntry, 0)
	walkErr := WalkRootDirectoryWithLimits(temporary, ".", rootWalkLimitsForDirectory(limits), func(_ *os.Root, _ string, walkPath string, info fs.FileInfo) error {
		if walkPath != "." {
			entries = append(entries, cleanupEntry{name: filepath.FromSlash(walkPath), info: info})
		}
		return nil
	})
	if walkErr == nil {
		for index := len(entries) - 1; index >= 0; index-- {
			entry := entries[index]
			current, err := temporary.Lstat(entry.name)
			if err != nil || current.Mode().Type() != entry.info.Mode().Type() || !os.SameFile(entry.info, current) {
				walkErr = errors.Join(errors.New("private backup directory entry changed before cleanup: "+entry.name), err)
				break
			}
			if err := temporary.Remove(entry.name); err != nil {
				walkErr = err
				break
			}
		}
	}
	closeErr := temporary.Close()
	if walkErr != nil || closeErr != nil {
		return fmt.Errorf("private backup directory residue was retained at %s: %w", filepath.Join(parent.Name(), name), errors.Join(walkErr, closeErr))
	}
	current, err := parent.Lstat(name)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
		return errors.Join(fmt.Errorf("private backup directory residue was retained after its identity changed: %s", filepath.Join(parent.Name(), name)), err)
	}
	if err := parent.Remove(name); err != nil {
		return fmt.Errorf("private backup directory residue was retained at %s: %w", filepath.Join(parent.Name(), name), err)
	}
	return nil
}

func cleanRootRelativePath(name string) (string, error) {
	clean := path.Clean(filepath.ToSlash(name))
	if clean == "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes rooted backup source: %s", name)
	}
	return clean, nil
}

// RootWalkFunc receives each entry in lexicographic depth-first order. The
// access root and name are borrowed handles for the entry: directories use an
// accessName of ".", while other entries use a pinned parent root and basename.
// Callbacks must not close or retain accessRoot.
type RootWalkFunc func(accessRoot *os.Root, accessName string, walkPath string, info fs.FileInfo) error

// WalkRootDirectoryBounded walks a real directory beneath root while bounding
// directory enumeration itself. The root entry counts toward maxEntries. A
// positive entry limit also enables finite depth, path, and metadata defaults.
func WalkRootDirectoryBounded(root *os.Root, start string, maxEntries int, visit RootWalkFunc) error {
	return WalkRootDirectoryWithLimits(root, start, RootWalkLimits{MaxEntries: maxEntries}, visit)
}

// WalkRootDirectoryWithLimits walks a real directory beneath root using pinned
// directory handles and a transaction-like child snapshot for every directory.
// Every child set is enumerated and validated again after its children have
// been processed, so additions, removals, and replacements fail the walk.
func WalkRootDirectoryWithLimits(root *os.Root, start string, limits RootWalkLimits, visit RootWalkFunc) error {
	return walkRootDirectoryWithHooks(root, start, limits, "rooted directory walk", rootWalkHooks{}, visit)
}

type rootWalkHooks struct {
	afterChildren func(root *os.Root, walkPath string) error
}

type rootChildSnapshot struct {
	name string
	info fs.FileInfo
}

type rootWalkState struct {
	limits        RootWalkLimits
	work          string
	start         string
	components    []string
	entries       int
	pathBytes     int64
	metadataBytes int64
	hooks         rootWalkHooks
	visit         RootWalkFunc
}

// walkRootDirectoryWithHooks traverses a tree through pinned directory roots.
// It retains only a component stack; full paths are constructed only when they
// are emitted and their complete lengths are charged to the aggregate budget.
func walkRootDirectoryWithHooks(root *os.Root, start string, limits RootWalkLimits, work string, hooks rootWalkHooks, visit RootWalkFunc) error {
	if root == nil {
		return errors.New("rooted directory walk requires a root")
	}
	if visit == nil {
		return errors.New("rooted directory walk requires a callback")
	}
	limits, err := normalizeRootWalkLimits(limits)
	if err != nil {
		return err
	}
	start, err = cleanRootRelativePath(start)
	if err != nil {
		return err
	}
	expected, err := root.Lstat(filepath.FromSlash(start))
	if err != nil {
		return err
	}
	if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("rooted directory walk requires a real directory: %s", start)
	}
	opened, err := root.OpenRoot(filepath.FromSlash(start))
	if err != nil {
		return err
	}
	openedInfo, err := opened.Stat(".")
	if err != nil {
		return errors.Join(err, opened.Close())
	}
	if !sameRootEntry(expected, openedInfo) {
		return errors.Join(fmt.Errorf("directory changed before %s: %s", work, start), opened.Close())
	}
	state := rootWalkState{
		limits:     limits,
		work:       work,
		start:      start,
		components: make([]string, 0, minPositive(limits.MaxDepth, limits.MaxEntries)),
		entries:    1,
		hooks:      hooks,
		visit:      visit,
	}
	if err := state.chargeMetadata(expected.Name()); err != nil {
		return errors.Join(err, opened.Close())
	}
	walkErr := state.walkOpenedRootDirectory(opened, expected)
	return errors.Join(walkErr, opened.Close())
}

func (state *rootWalkState) walkOpenedRootDirectory(root *os.Root, expected fs.FileInfo) error {
	if err := state.emit(root, ".", expected); err != nil {
		return err
	}
	children, err := state.captureChildren(root, expected)
	if err != nil {
		return err
	}
	for _, child := range children {
		state.components = append(state.components, child.name)
		if state.limits.MaxDepth > 0 && len(state.components) > state.limits.MaxDepth {
			childPath := state.currentPath()
			state.components = state.components[:len(state.components)-1]
			return fmt.Errorf("%s exceeds maximum depth %d at %s", state.work, state.limits.MaxDepth, childPath)
		}
		var childErr error
		if !child.info.IsDir() || child.info.Mode()&os.ModeSymlink != 0 {
			childErr = state.emit(root, child.name, child.info)
		} else {
			childRoot, openErr := root.OpenRoot(child.name)
			if openErr != nil {
				childErr = openErr
			} else {
				openedChild, statErr := childRoot.Stat(".")
				switch {
				case statErr != nil:
					childErr = statErr
				case !sameRootEntry(child.info, openedChild):
					childErr = fmt.Errorf("directory changed during %s: %s", state.work, state.currentPath())
				default:
					childErr = state.walkOpenedRootDirectory(childRoot, child.info)
				}
				childErr = errors.Join(childErr, childRoot.Close())
			}
		}
		state.components = state.components[:len(state.components)-1]
		if childErr != nil {
			return childErr
		}
	}
	if state.hooks.afterChildren != nil {
		if err := state.hooks.afterChildren(root, state.currentPath()); err != nil {
			return err
		}
	}
	return state.verifyChildren(root, expected, children)
}

func (state *rootWalkState) captureChildren(root *os.Root, expected fs.FileInfo) ([]rootChildSnapshot, error) {
	entries, err := readPinnedRootDirectory(root, expected, state.limits.MaxEntries, state.entries, state.work)
	if err != nil {
		return nil, err
	}
	state.entries += len(entries)
	children := make([]rootChildSnapshot, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if err := state.chargeMetadata(name); err != nil {
			return nil, err
		}
		children = append(children, rootChildSnapshot{name: name, info: info})
	}
	return children, nil
}

func (state *rootWalkState) verifyChildren(root *os.Root, expected fs.FileInfo, children []rootChildSnapshot) error {
	entries, err := readPinnedRootDirectory(root, expected, len(children)+1, 1, state.work+" verification")
	if err != nil {
		return fmt.Errorf("directory entries changed during %s at %s: %w", state.work, state.currentPath(), err)
	}
	if len(entries) != len(children) {
		return fmt.Errorf("directory entries changed during %s at %s", state.work, state.currentPath())
	}
	for index, entry := range entries {
		name := entry.Name()
		expectedChild := children[index]
		if name != expectedChild.name {
			return fmt.Errorf("directory entries changed during %s at %s", state.work, state.currentPath())
		}
		if err := state.chargeMetadata(name); err != nil {
			return err
		}
		current, err := root.Lstat(name)
		if err != nil {
			return fmt.Errorf("directory entry changed during %s at %s: %w", state.work, state.currentPath(), err)
		}
		if !sameRootEntry(expectedChild.info, current) {
			return fmt.Errorf("directory entry changed during %s at %s", state.work, state.currentPath())
		}
	}
	return nil
}

func readPinnedRootDirectory(root *os.Root, expected fs.FileInfo, maxEntries int, seen int, work string) ([]fs.DirEntry, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	openedInfo, err := directory.Stat()
	if err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	if !sameRootEntry(expected, openedInfo) {
		return nil, errors.Join(fmt.Errorf("directory changed during %s", work), directory.Close())
	}
	entries, readErr := readRootDirectoryEntries(directory, maxEntries, seen, work)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	current, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if !sameRootEntry(expected, current) {
		return nil, fmt.Errorf("directory changed during %s", work)
	}
	return entries, nil
}

func sameRootEntry(expected fs.FileInfo, current fs.FileInfo) bool {
	return expected != nil && current != nil &&
		os.SameFile(expected, current) &&
		expected.Mode() == current.Mode() &&
		expected.Size() == current.Size() &&
		expected.ModTime().Equal(current.ModTime())
}

func (state *rootWalkState) emit(accessRoot *os.Root, accessName string, info fs.FileInfo) error {
	walkPath := state.currentPath()
	pathBytes := int64(len(walkPath))
	if state.limits.MaxPathBytes > 0 && pathBytes > state.limits.MaxPathBytes-state.pathBytes {
		return fmt.Errorf("%s exceeds %d bytes of emitted paths", state.work, state.limits.MaxPathBytes)
	}
	state.pathBytes += pathBytes
	return state.visit(accessRoot, accessName, walkPath, info)
}

func (state *rootWalkState) chargeMetadata(name string) error {
	const fixedEntryMetadataBytes = int64(64)
	cost := fixedEntryMetadataBytes + int64(len(name))
	if state.limits.MaxMetadataBytes > 0 && cost > state.limits.MaxMetadataBytes-state.metadataBytes {
		return fmt.Errorf("%s exceeds %d bytes of entry metadata", state.work, state.limits.MaxMetadataBytes)
	}
	state.metadataBytes += cost
	return nil
}

func (state *rootWalkState) currentPath() string {
	if len(state.components) == 0 {
		return state.start
	}
	length := len(state.start)
	if state.start == "." {
		length = 0
	}
	for _, component := range state.components {
		if length != 0 {
			length++
		}
		length += len(component)
	}
	var builder strings.Builder
	builder.Grow(length)
	if state.start != "." {
		builder.WriteString(state.start)
	}
	for _, component := range state.components {
		if builder.Len() != 0 {
			builder.WriteByte('/')
		}
		builder.WriteString(component)
	}
	return builder.String()
}

func readRootDirectoryEntries(directory *os.File, maxEntries int, seen int, work string) ([]fs.DirEntry, error) {
	const batchSize = 128
	remaining := -1
	if maxEntries > 0 {
		remaining = maxEntries - seen
	}
	capacityLimit := remaining
	if capacityLimit >= 0 {
		capacityLimit++
	}
	entries := make([]fs.DirEntry, 0, minPositive(batchSize, capacityLimit))
	for {
		readSize := batchSize
		if remaining >= 0 && readSize > remaining+1-len(entries) {
			readSize = remaining + 1 - len(entries)
		}
		if readSize <= 0 {
			return nil, fmt.Errorf("%s exceeds %d filesystem entries", work, maxEntries)
		}
		batch, err := directory.ReadDir(readSize)
		entries = append(entries, batch...)
		if remaining >= 0 && len(entries) > remaining {
			return nil, fmt.Errorf("%s exceeds %d filesystem entries", work, maxEntries)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return nil, fmt.Errorf("directory enumeration made no progress during %s", work)
		}
	}
	sort.Slice(entries, func(left int, right int) bool {
		return entries[left].Name() < entries[right].Name()
	})
	return entries, nil
}

func minPositive(value int, limit int) int {
	if limit >= 0 && limit < value {
		return limit
	}
	return value
}

func copyRootDirectory(source *os.Root, sourceRel string, target *os.Root, limits DirectoryLimits, hooks backupRootDirHooks) error {
	sourceRel, err := cleanRootRelativePath(sourceRel)
	if err != nil {
		return err
	}
	budget := directoryBudget{limits: limits, work: "directory backup"}
	return walkRootDirectoryWithHooks(source, sourceRel, rootWalkLimitsForDirectory(limits), budget.work, rootWalkHooks{}, func(accessRoot *os.Root, accessName string, sourcePath string, info fs.FileInfo) error {
		rel, err := filepath.Rel(filepath.FromSlash(sourceRel), filepath.FromSlash(sourcePath))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		destination := filepath.FromSlash(rel)
		var copyErr error
		switch {
		case info.IsDir():
			// #nosec G301 -- sealed backup copies normalize directory permissions.
			copyErr = target.MkdirAll(destination, 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, err := accessRoot.Readlink(accessName)
			if err != nil {
				return err
			}
			// #nosec G301 -- sealed backup copies normalize directory permissions.
			if err := target.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return err
			}
			// #nosec G122 -- destination is derived beneath the private temporary backup directory.
			copyErr = target.Symlink(linkTarget, destination)
		case info.Mode().IsRegular():
			if err := budget.checkFileSize(sourcePath, info.Size()); err != nil {
				return err
			}
			written, err := copyRootRegularFile(accessRoot, accessName, info, target, destination, budget.readAllowance())
			if err != nil {
				return err
			}
			copyErr = budget.addFileBytes(sourcePath, written)
		default:
			return fmt.Errorf("unsupported non-regular file in backup source: %s", sourcePath)
		}
		if copyErr != nil {
			return copyErr
		}
		if hooks.afterCopyEntry != nil {
			return hooks.afterCopyEntry(sourcePath)
		}
		return nil
	})
}

func copyRootRegularFile(source *os.Root, sourceName string, expected fs.FileInfo, target *os.Root, targetName string, allowance int64) (int64, error) {
	if allowance >= 0 && expected.Size() > allowance {
		return 0, fmt.Errorf("directory backup regular-file budget exceeded by %s", sourceName)
	}
	in, err := openBoundedRegularRootFile(source, sourceName)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return 0, fmt.Errorf("backup source changed before it could be copied: %s", sourceName)
	}
	// #nosec G301 -- sealed backup copies normalize directory permissions.
	if err := target.MkdirAll(filepath.Dir(targetName), 0o755); err != nil {
		return 0, err
	}
	// #nosec G304 -- target is derived beneath the private temporary backup directory.
	out, err := target.OpenFile(targetName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	created, err := out.Stat()
	if err != nil {
		return 0, errors.Join(err, out.Close())
	}
	ok := false
	defer func() {
		if !ok {
			if current, err := target.Lstat(targetName); err == nil && current.Mode().Type() == created.Mode().Type() && os.SameFile(created, current) {
				_ = target.Remove(targetName)
			}
		}
	}()
	written, copyErr := io.Copy(out, io.LimitReader(in, boundedReadLimit(expected.Size(), allowance)))
	if copyErr != nil {
		return 0, errors.Join(copyErr, out.Close())
	}
	if allowance >= 0 && written > allowance {
		return 0, errors.Join(fmt.Errorf("directory backup regular-file budget exceeded by %s", sourceName), out.Close())
	}
	if written != expected.Size() {
		return 0, errors.Join(fmt.Errorf("backup source changed while it was copied: %s", sourceName), out.Close())
	}
	if err := out.Chmod(0o644); err != nil {
		return 0, errors.Join(err, out.Close())
	}
	if err := out.Sync(); err != nil {
		return 0, errors.Join(err, out.Close())
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	after, err := in.Stat()
	if err != nil {
		return 0, err
	}
	current, err := source.Lstat(sourceName)
	if err != nil {
		return 0, err
	}
	if !os.SameFile(opened, after) || !os.SameFile(opened, current) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return 0, fmt.Errorf("backup source changed while it was copied: %s", sourceName)
	}
	ok = true
	return written, nil
}

// DirectorySHA256 returns a deterministic digest of a real directory's
// relative paths, filesystem types, regular-file bytes, and symlink targets.
func DirectorySHA256(directory string) (string, error) {
	return DirectorySHA256WithLimits(directory, DirectoryLimits{})
}

// DirectorySHA256WithLimits returns DirectorySHA256 while bounding each tree
// walk and each stable, non-following regular-file read.
func DirectorySHA256WithLimits(directory string, limits DirectoryLimits) (string, error) {
	limits, err := normalizeDirectoryLimits(limits)
	if err != nil {
		return "", err
	}
	if err := safefs.RejectExistingSymlinkAncestors(directory); err != nil {
		return "", err
	}
	expected, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("backup payload is not a directory: %s", directory)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil {
		return "", err
	}
	if !os.SameFile(expected, opened) {
		return "", fmt.Errorf("backup directory changed before it could be hashed: %s", directory)
	}
	return RootDirSHA256WithLimits(root, ".", limits)
}

// RootDirSHA256 returns the deterministic tree digest for a directory beneath
// a pinned filesystem root.
func RootDirSHA256(root *os.Root, directory string) (string, error) {
	return RootDirSHA256WithLimits(root, directory, DirectoryLimits{})
}

// RootDirSHA256WithLimits returns RootDirSHA256 with explicit work budgets. It
// requires two consecutive bounded whole-tree passes to produce the same
// digest before reporting a stable result.
func RootDirSHA256WithLimits(root *os.Root, directory string, limits DirectoryLimits) (string, error) {
	return rootDirSHA256WithHooks(root, directory, limits, directoryDigestHooks{})
}

type directoryDigestHooks struct {
	afterFirstPass func() error
}

func rootDirSHA256WithHooks(root *os.Root, directory string, limits DirectoryLimits, hooks directoryDigestHooks) (string, error) {
	limits, err := normalizeDirectoryLimits(limits)
	if err != nil {
		return "", err
	}
	if root == nil {
		return "", errors.New("directory digest root is required")
	}
	clean, err := cleanRootRelativePath(directory)
	if err != nil {
		return "", err
	}
	first, err := rootDirSHA256Pass(root, clean, directory, limits)
	if err != nil {
		return "", err
	}
	if hooks.afterFirstPass != nil {
		if err := hooks.afterFirstPass(); err != nil {
			return "", err
		}
	}
	second, err := rootDirSHA256Pass(root, clean, directory, limits)
	if err != nil {
		return "", err
	}
	if first != second {
		return "", fmt.Errorf("backup directory changed between bounded digest passes: %s", directory)
	}
	return first, nil
}

// Directory digests serialize each entry's kind, so these spellings are part of
// the digest format: changing one changes every digest that contains it.
const (
	digestEntryKindFile      = "file"
	digestEntryKindDirectory = "directory"
	digestEntryKindSymlink   = "symlink"
)

func rootDirSHA256Pass(root *os.Root, clean string, directory string, limits DirectoryLimits) (string, error) {
	rootInfo, err := root.Lstat(filepath.FromSlash(clean))
	if err != nil {
		return "", err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("backup payload is not a directory: %s", directory)
	}
	digest := sha256.New()
	_, _ = io.WriteString(digest, "threadpoint-directory-v1\n")
	budget := directoryBudget{limits: limits, work: "directory digest"}
	type digestEntry struct {
		Path     string `json:"path"`
		Kind     string `json:"kind"`
		Identity string `json:"identity,omitempty"`
	}
	err = walkRootDirectoryWithHooks(root, clean, rootWalkLimitsForDirectory(limits), budget.work, rootWalkHooks{}, func(accessRoot *os.Root, accessName string, current string, info fs.FileInfo) error {
		rel, err := filepath.Rel(filepath.FromSlash(clean), filepath.FromSlash(current))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		record := digestEntry{Path: rel}
		switch {
		case info.IsDir():
			record.Kind = digestEntryKindDirectory
		case info.Mode()&os.ModeSymlink != 0:
			target, err := accessRoot.Readlink(accessName)
			if err != nil {
				return err
			}
			record.Kind = digestEntryKindSymlink
			sum := sha256.Sum256([]byte(target))
			record.Identity = hex.EncodeToString(sum[:])
		case info.Mode().IsRegular():
			if err := budget.checkFileSize(current, info.Size()); err != nil {
				return err
			}
			identity, read, err := hashRootRegularFile(accessRoot, accessName, info, budget.readAllowance())
			if err != nil {
				return err
			}
			if err := budget.addFileBytes(current, read); err != nil {
				return err
			}
			record.Kind = digestEntryKindFile
			record.Identity = identity
		default:
			return fmt.Errorf("unsupported non-regular file in backup payload: %s", current)
		}
		body, err := json.Marshal(record)
		if err != nil {
			return err
		}
		_, _ = digest.Write(body)
		_, _ = digest.Write([]byte{'\n'})
		return nil
	})
	if err != nil {
		return "", err
	}
	after, err := root.Lstat(filepath.FromSlash(clean))
	if err != nil {
		return "", err
	}
	if !sameRootEntry(rootInfo, after) {
		return "", fmt.Errorf("backup directory changed while it was hashed: %s", directory)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func hashRootRegularFile(root *os.Root, name string, expected fs.FileInfo, allowance int64) (string, int64, error) {
	if allowance >= 0 && expected.Size() > allowance {
		return "", 0, fmt.Errorf("directory digest regular-file budget exceeded by %s", name)
	}
	file, err := openBoundedRegularRootFile(root, name)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return "", 0, fmt.Errorf("backup file changed before it could be hashed: %s", name)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, boundedReadLimit(expected.Size(), allowance)))
	if err != nil {
		return "", 0, err
	}
	if allowance >= 0 && written > allowance {
		return "", 0, fmt.Errorf("directory digest regular-file budget exceeded by %s", name)
	}
	if written != expected.Size() {
		return "", 0, fmt.Errorf("backup file changed while it was hashed: %s", name)
	}
	after, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	current, err := root.Lstat(name)
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(opened, after) || !os.SameFile(opened, current) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return "", 0, fmt.Errorf("backup file changed while it was hashed: %s", name)
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

func normalizeDirectoryLimits(limits DirectoryLimits) (DirectoryLimits, error) {
	if limits.MaxEntries < 0 || limits.MaxBytes < 0 || limits.MaxFileBytes < 0 || limits.MaxDepth < 0 || limits.MaxPathBytes < 0 || limits.MaxMetadataBytes < 0 {
		return DirectoryLimits{}, errors.New("directory limits cannot be negative")
	}
	if limits.MaxEntries > 0 {
		if limits.MaxDepth == 0 {
			limits.MaxDepth = DefaultDirectoryMaxDepth
		}
		if limits.MaxPathBytes == 0 {
			limits.MaxPathBytes = DefaultDirectoryMaxPathBytes
		}
		if limits.MaxMetadataBytes == 0 {
			limits.MaxMetadataBytes = DefaultDirectoryMaxMetadataBytes
		}
	}
	return limits, nil
}

func rootWalkLimitsForDirectory(limits DirectoryLimits) RootWalkLimits {
	return RootWalkLimits{
		MaxEntries:       limits.MaxEntries,
		MaxDepth:         limits.MaxDepth,
		MaxPathBytes:     limits.MaxPathBytes,
		MaxMetadataBytes: limits.MaxMetadataBytes,
	}
}

func normalizeRootWalkLimits(limits RootWalkLimits) (RootWalkLimits, error) {
	if limits.MaxEntries < 0 || limits.MaxDepth < 0 || limits.MaxPathBytes < 0 || limits.MaxMetadataBytes < 0 {
		return RootWalkLimits{}, errors.New("rooted directory walk limits cannot be negative")
	}
	if limits.MaxEntries > 0 {
		if limits.MaxDepth == 0 {
			limits.MaxDepth = DefaultDirectoryMaxDepth
		}
		if limits.MaxPathBytes == 0 {
			limits.MaxPathBytes = DefaultDirectoryMaxPathBytes
		}
		if limits.MaxMetadataBytes == 0 {
			limits.MaxMetadataBytes = DefaultDirectoryMaxMetadataBytes
		}
	}
	return limits, nil
}

func (budget *directoryBudget) checkFileSize(name string, size int64) error {
	if size < 0 {
		return fmt.Errorf("%s file reported a negative size: %s", budget.work, name)
	}
	if budget.limits.MaxFileBytes > 0 && size > budget.limits.MaxFileBytes {
		return fmt.Errorf("%s file exceeds %d bytes: %s", budget.work, budget.limits.MaxFileBytes, name)
	}
	if budget.limits.MaxBytes > 0 && size > budget.limits.MaxBytes-budget.bytes {
		return fmt.Errorf("%s exceeds %d bytes of regular-file content", budget.work, budget.limits.MaxBytes)
	}
	return nil
}

// readAllowance returns the strictest remaining byte limit, or -1 when both
// byte dimensions are unlimited. The caller still measures the bytes actually
// read and commits that count with addFileBytes.
func (budget *directoryBudget) readAllowance() int64 {
	allowance := int64(-1)
	if budget.limits.MaxFileBytes > 0 {
		allowance = budget.limits.MaxFileBytes
	}
	if budget.limits.MaxBytes > 0 {
		remaining := budget.limits.MaxBytes - budget.bytes
		if remaining < 0 {
			remaining = 0
		}
		if allowance < 0 || remaining < allowance {
			allowance = remaining
		}
	}
	return allowance
}

func (budget *directoryBudget) addFileBytes(name string, read int64) error {
	if budget.limits.MaxFileBytes > 0 && read > budget.limits.MaxFileBytes {
		return fmt.Errorf("%s file exceeds %d bytes: %s", budget.work, budget.limits.MaxFileBytes, name)
	}
	if budget.limits.MaxBytes > 0 && read > budget.limits.MaxBytes-budget.bytes {
		return fmt.Errorf("%s exceeds %d bytes of regular-file content", budget.work, budget.limits.MaxBytes)
	}
	if budget.limits.MaxBytes > 0 {
		budget.bytes += read
	}
	return nil
}

func boundedReadLimit(expectedSize int64, allowance int64) int64 {
	const maxInt64 = int64(1<<63 - 1)
	limit := expectedSize
	if limit < maxInt64 {
		limit++
	}
	if allowance >= 0 {
		budgetLimit := allowance
		if budgetLimit < maxInt64 {
			budgetLimit++
		}
		if budgetLimit < limit {
			limit = budgetLimit
		}
	}
	return limit
}

// RecordAbsent records that rel did not exist before an operation. Restore
// uses this entry to remove a file created by the operation instead of
// treating an intermediate version of that file as the original backup.
func (store *Store) RecordAbsent(rel string, role string) (*Entry, error) {
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	if err := store.validateRunDirForMutation(); err != nil {
		return nil, err
	}
	entry, err := absentEntry(rel, role)
	if err != nil {
		return nil, err
	}
	return store.commitEntry(entry)
}

// RecordAbsentFromRoot records an absent pre-operation path through a
// caller-retained product-home root. The lexical homePath is used only to
// derive the manifest location; it is never reopened as filesystem authority.
func (store *Store) RecordAbsentFromRoot(homeRoot *os.Root, homePath, rel string, role string) (*Entry, error) {
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	if err := store.validateRunDirForMutationRoot(homeRoot, homePath); err != nil {
		return nil, err
	}
	entry, err := absentEntry(rel, role)
	if err != nil {
		return nil, err
	}
	return store.commitEntryFromRoot(homeRoot, homePath, entry)
}

func absentEntry(rel string, role string) (Entry, error) {
	cleanRel, err := normalizeEntryPath(rel)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Role:   role,
		Path:   cleanRel,
		Absent: true,
	}, nil
}

// SetPostHash records the current hash for a previously backed-up path.
func (store *Store) SetPostHash(rel string, path string) error {
	hash, err := FileSHA256(path)
	if err != nil {
		return err
	}
	return store.SetPostHashValue(rel, "", hash)
}

// SetPostHashValue records an already-computed post-operation hash for one
// role and path. Supplying the exact bytes written avoids accepting a
// concurrent writer's content as the operation's post-change state.
func (store *Store) SetPostHashValue(rel string, role string, hash string) error {
	return store.updatePostHash(rel, role, func(entry *Entry) error {
		entry.PostSHA256 = hash
		entry.PendingPostSHA256 = ""
		return nil
	})
}

// SetPendingPostHashValue records an in-flight predicted post-change hash
// without replacing the last committed hash. This makes either side of an
// interrupted atomic rename recognizable during restoration.
func (store *Store) SetPendingPostHashValue(rel string, role string, hash string) error {
	return store.updatePostHash(rel, role, func(entry *Entry) error {
		entry.PendingPostSHA256 = hash
		return nil
	})
}

// CommitPendingPostHashValue advances the committed post-change hash and
// clears its matching in-flight prediction after the target rename succeeds.
func (store *Store) CommitPendingPostHashValue(rel string, role string, hash string) error {
	return store.updatePostHash(rel, role, func(entry *Entry) error {
		if entry.PendingPostSHA256 != hash {
			return fmt.Errorf("pending post-change hash for %s with role %q does not match commit", rel, role)
		}
		entry.PostSHA256 = hash
		entry.PendingPostSHA256 = ""
		return nil
	})
}

// SetRecoveryPath marks an entry as having an active same-filesystem quarantine.
// quarantinePath is validated for immediate reporting but is never persisted;
// the manifest records only the sealed run-local BackupPath as recovery data.
func (store *Store) SetRecoveryPath(rel string, role string, quarantinePath string) error {
	cleanRel, err := normalizeEntryPath(rel)
	if err != nil {
		return err
	}
	quarantinePath = filepath.Clean(quarantinePath)
	if !filepath.IsAbs(quarantinePath) {
		return fmt.Errorf("backup quarantine path must be absolute: %s", quarantinePath)
	}
	original := append([]Entry(nil), store.Manifest.Entries...)
	found := false
	for i := range store.Manifest.Entries {
		if store.Manifest.Entries[i].Path == cleanRel && store.Manifest.Entries[i].Role == role {
			if store.Manifest.Entries[i].BackupPath == "" || !pathWithin(store.Manifest.Entries[i].BackupPath, store.RunDir) {
				store.Manifest.Entries = original
				return fmt.Errorf("backup entry %s with role %q has no run-local recovery payload", cleanRel, role)
			}
			store.Manifest.Entries[i].RecoveryPath = store.Manifest.Entries[i].BackupPath
			found = true
		}
	}
	if !found {
		return fmt.Errorf("backup entry %s with role %q was not recorded", cleanRel, role)
	}
	if err := store.Save(); err != nil {
		store.Manifest.Entries = original
		return err
	}
	return nil
}

// ClearRecoveryPath removes a previously journaled recovery-item intent after
// the quarantine is resolved or its transient pathname can no longer be
// reported. The entry and its sealed run-local payload remain available.
func (store *Store) ClearRecoveryPath(rel string, role string) error {
	cleanRel, err := normalizeEntryPath(rel)
	if err != nil {
		return err
	}
	original := append([]Entry(nil), store.Manifest.Entries...)
	found := false
	for i := range store.Manifest.Entries {
		if store.Manifest.Entries[i].Path == cleanRel && store.Manifest.Entries[i].Role == role {
			store.Manifest.Entries[i].RecoveryPath = ""
			found = true
		}
	}
	if !found {
		return fmt.Errorf("backup entry %s with role %q was not recorded", cleanRel, role)
	}
	if err := store.Save(); err != nil {
		store.Manifest.Entries = original
		return err
	}
	return nil
}

func (store *Store) updatePostHash(rel string, role string, update func(*Entry) error) error {
	cleanRel, err := normalizeEntryPath(rel)
	if err != nil {
		return err
	}
	original := append([]Entry(nil), store.Manifest.Entries...)
	found := false
	for i := range store.Manifest.Entries {
		if store.Manifest.Entries[i].Path == cleanRel && (role == "" || store.Manifest.Entries[i].Role == role) {
			if err := update(&store.Manifest.Entries[i]); err != nil {
				store.Manifest.Entries = original
				return err
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("backup entry %s with role %q was not recorded", cleanRel, role)
	}
	if err := store.Save(); err != nil {
		store.Manifest.Entries = original
		return err
	}
	return nil
}

// Discard removes one persisted entry and its payload from this run. It is
// used when an operation restores source data after a tentative backup, so a
// backup for a mutation that never committed cannot become a restore choice.
func (store *Store) Discard(role string, rel string) error {
	if store == nil || len(store.Manifest.Entries) == 0 {
		return nil
	}
	if err := store.validateRunDirForMutation(); err != nil {
		return err
	}
	cleanRel, err := normalizeEntryPath(rel)
	if err != nil {
		return err
	}
	key := entryKey(role, cleanRel)
	index := -1
	for i := range store.Manifest.Entries {
		if entryKey(store.Manifest.Entries[i].Role, store.Manifest.Entries[i].Path) == key {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	entry := store.Manifest.Entries[index]
	if len(store.Manifest.Entries) == 1 {
		expected, err := store.expectedManifestGeneration()
		if err != nil {
			return err
		}
		detached, discardErr := store.discardFinalRun(expected)
		if detached {
			store.Manifest.Entries = nil
			store.index = nil
			store.manifestGenerationKnown = false
			store.persistedManifest = nil
		}
		return discardErr
	}
	var payloadInfo fs.FileInfo
	if entry.BackupPath != "" {
		inside, err := filepath.Rel(store.RunDir, filepath.Clean(entry.BackupPath))
		if err != nil || inside == "." || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
			return fmt.Errorf("backup payload for %s is outside run directory", entry.Path)
		}
		if store.borrowedHomeRoot != nil {
			payloadRel, relErr := rootRelativePath(store.borrowedHomePath, entry.BackupPath)
			if relErr != nil {
				return relErr
			}
			payloadInfo, err = store.borrowedHomeRoot.Lstat(payloadRel)
		} else {
			if err := safefs.RejectExistingSymlinkAncestors(filepath.Dir(entry.BackupPath)); err != nil {
				return err
			}
			payloadInfo, err = os.Lstat(entry.BackupPath)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	original := append([]Entry(nil), store.Manifest.Entries...)
	store.Manifest.Entries = append(store.Manifest.Entries[:index], store.Manifest.Entries[index+1:]...)
	store.index = nil
	if err := store.Save(); err != nil {
		store.Manifest.Entries = original
		store.index = nil
		return err
	}
	if entry.BackupPath != "" && !payloadReferencedBy(store.Manifest.Entries, entry.BackupPath) {
		var removeErr error
		if store.borrowedHomeRoot != nil {
			removeErr = removeDiscardedBackupPayloadRoot(store.borrowedHomeRoot, store.borrowedHomePath, entry.BackupPath, payloadInfo)
		} else {
			removeErr = removeDiscardedBackupPayload(entry.BackupPath, payloadInfo)
		}
		if removeErr != nil {
			return removeErr
		}
	}
	return nil
}

// discardFinalRun removes the entire canonical run instead of publishing an
// empty manifest. The stable run name is detached only after its exact manifest
// generation is revalidated; cleanup then stays rooted in that detached inode.
func (store *Store) discardFinalRun(expectedManifest []byte) (detached bool, returnErr error) {
	if store.beforeSave != nil {
		if err := store.beforeSave(); err != nil {
			return false, err
		}
	}
	projectRuns := filepath.Dir(store.RunDir)
	var parent *os.Root
	var err error
	if store.borrowedHomeRoot != nil {
		rel, relErr := rootRelativePath(store.borrowedHomePath, projectRuns)
		if relErr != nil {
			return false, relErr
		}
		if err := safefs.RejectRootSymlinkAncestors(store.borrowedHomeRoot, rel); err != nil {
			return false, err
		}
		expectedParent, statErr := store.borrowedHomeRoot.Lstat(rel)
		if statErr != nil || !expectedParent.IsDir() || expectedParent.Mode()&os.ModeSymlink != 0 {
			return false, errors.Join(errors.New("backup project run parent is not a physical directory"), statErr)
		}
		parent, err = store.borrowedHomeRoot.OpenRoot(rel)
		if err == nil {
			opened, openedErr := parent.Stat(".")
			if openedErr != nil || !os.SameFile(expectedParent, opened) {
				err = errors.Join(errors.New("backup project run parent changed while it was opened"), openedErr)
			}
		}
	} else {
		if err := safefs.RejectExistingSymlinkAncestors(projectRuns); err != nil {
			return false, err
		}
		expectedParent, statErr := os.Lstat(projectRuns)
		if statErr != nil || !expectedParent.IsDir() || expectedParent.Mode()&os.ModeSymlink != 0 {
			return false, errors.Join(errors.New("backup project run parent is not a physical directory"), statErr)
		}
		parent, err = os.OpenRoot(projectRuns)
		if err == nil {
			opened, openedErr := parent.Stat(".")
			if openedErr != nil || !os.SameFile(expectedParent, opened) {
				err = errors.Join(errors.New("backup project run parent changed while it was opened"), openedErr)
			}
		}
	}
	if err != nil {
		if parent != nil {
			_ = parent.Close()
		}
		return false, err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	base := filepath.Base(store.RunDir)
	runInfo, err := parent.Lstat(base)
	if err != nil || !runInfo.IsDir() || runInfo.Mode()&os.ModeSymlink != 0 || (store.loadedRunInfo != nil && !os.SameFile(store.loadedRunInfo, runInfo)) {
		return false, errors.Join(errors.New("backup run changed before final-entry discard"), err)
	}
	runRoot, err := parent.OpenRoot(base)
	if err != nil {
		return false, err
	}
	opened, err := runRoot.Stat(".")
	if err != nil || !os.SameFile(runInfo, opened) {
		return false, errors.Join(errors.New("backup run changed while opened for final-entry discard"), err, runRoot.Close())
	}
	if err := validateExpectedManifest(expectedManifest)(runRoot, ManifestName); err != nil {
		return false, errors.Join(err, runRoot.Close())
	}
	var token [12]byte
	_, _ = rand.Read(token[:])
	quarantine := ".discard-" + hex.EncodeToString(token[:])
	if err := safefs.RenameRootNoReplace(parent, base, quarantine); err != nil {
		return false, errors.Join(err, runRoot.Close())
	}
	detached = true
	current, err := parent.Lstat(quarantine)
	if err != nil || !current.IsDir() || !os.SameFile(runInfo, current) {
		restoreErr := safefs.RenameRootNoReplace(parent, quarantine, base)
		if restoreErr == nil {
			detached = false
		}
		return detached, errors.Join(errors.New("backup run changed during final-entry discard"), err, restoreErr, runRoot.Close())
	}
	if err := syncBackupRoot(parent); err != nil {
		return true, errors.Join(err, runRoot.Close())
	}
	cleanupErr := cleanupBackupTempDirectory(parent, quarantine, runInfo, runRoot, DirectoryLimits{MaxEntries: 100000})
	return true, errors.Join(cleanupErr, syncBackupRoot(parent))
}

func removeDiscardedBackupPayloadRoot(homeRoot *os.Root, homePath, payload string, expected fs.FileInfo) error {
	if expected == nil {
		return nil
	}
	rel, err := rootRelativePath(homePath, payload)
	if err != nil {
		return err
	}
	parentRel := filepath.Dir(rel)
	if err := safefs.RejectRootSymlinkAncestors(homeRoot, parentRel); err != nil {
		return err
	}
	parentInfo, err := homeRoot.Lstat(parentRel)
	if err != nil {
		return err
	}
	parent, err := homeRoot.OpenRoot(parentRel)
	if err != nil {
		return err
	}
	opened, err := parent.Stat(".")
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(parentInfo, opened) {
		_ = parent.Close()
		return errors.Join(errors.New("discarded rooted backup payload parent changed while it was opened"), err)
	}
	name := filepath.Base(rel)
	current, err := parent.Lstat(name)
	if err != nil || current.Mode().Type() != expected.Mode().Type() || !safefs.SameFileGeneration(expected, current) {
		_ = parent.Close()
		return errors.Join(errors.New("discarded rooted backup payload changed identity"), err)
	}
	if expected.IsDir() && expected.Mode()&os.ModeSymlink == 0 {
		directory, err := parent.OpenRoot(name)
		if err != nil {
			_ = parent.Close()
			return err
		}
		return errors.Join(cleanupBackupTempDirectory(parent, name, expected, directory, DirectoryLimits{MaxEntries: 100000}), parent.Close())
	}
	if !expected.Mode().IsRegular() {
		_ = parent.Close()
		return errors.New("discarded rooted backup payload is not a regular file or real directory")
	}
	return errors.Join(parent.Remove(name), parent.Close())
}

func removeDiscardedBackupPayload(payload string, expected fs.FileInfo) error {
	if expected == nil {
		return nil
	}
	parentPath := filepath.Dir(payload)
	if err := safefs.RejectExistingSymlinkAncestors(parentPath); err != nil {
		return err
	}
	parentInfo, err := os.Lstat(parentPath)
	if err != nil {
		return err
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return err
	}
	opened, err := parent.Stat(".")
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(parentInfo, opened) {
		_ = parent.Close()
		return errors.Join(errors.New("discarded backup payload parent changed while it was opened"), err)
	}
	name := filepath.Base(payload)
	current, err := parent.Lstat(name)
	if err != nil || current.Mode().Type() != expected.Mode().Type() || !safefs.SameFileGeneration(expected, current) {
		_ = parent.Close()
		return errors.Join(fmt.Errorf("discarded backup payload was retained after its identity changed: %s", payload), err)
	}
	if expected.IsDir() && expected.Mode()&os.ModeSymlink == 0 {
		directory, err := parent.OpenRoot(name)
		if err != nil {
			_ = parent.Close()
			return err
		}
		cleanupErr := cleanupBackupTempDirectory(parent, name, expected, directory, DirectoryLimits{MaxEntries: 100000})
		return errors.Join(cleanupErr, parent.Close())
	}
	if !expected.Mode().IsRegular() {
		_ = parent.Close()
		return fmt.Errorf("discarded backup payload is not a regular file or real directory: %s", payload)
	}
	removeErr := parent.Remove(name)
	return errors.Join(removeErr, parent.Close())
}

func payloadReferencedBy(entries []Entry, payload string) bool {
	payload = filepath.Clean(payload)
	for _, entry := range entries {
		if entry.BackupPath != "" && filepath.Clean(entry.BackupPath) == payload {
			return true
		}
	}
	return false
}

// Save persists the store manifest to disk.
func (store *Store) Save() error {
	if store != nil && store.borrowedHomeRoot != nil {
		return store.SaveFromRoot(store.borrowedHomeRoot, store.borrowedHomePath)
	}
	if err := validateStoreManifestForPublication(store); err != nil {
		return err
	}
	if err := store.validateRunDirForMutation(); err != nil {
		return err
	}
	body, err := marshalManifest(store.Manifest)
	if err != nil {
		return err
	}
	if store.beforeSave != nil {
		if err := store.beforeSave(); err != nil {
			return err
		}
	}
	expected, err := store.expectedManifestGeneration()
	if err != nil {
		return err
	}
	return store.publishManifestExpected(body, expected)
}

// SaveFromRoot persists the manifest beneath a caller-retained product-home
// root without reopening homePath.
func (store *Store) SaveFromRoot(homeRoot *os.Root, homePath string) error {
	if err := validateStoreManifestForPublication(store); err != nil {
		return err
	}
	if err := store.validateRunDirForMutationRoot(homeRoot, homePath); err != nil {
		return err
	}
	body, err := marshalManifest(store.Manifest)
	if err != nil {
		return err
	}
	if store.beforeSave != nil {
		if err := store.beforeSave(); err != nil {
			return err
		}
	}
	expected, err := store.expectedManifestGeneration()
	if err != nil {
		return err
	}
	return store.publishManifestExpectedFromRoot(homeRoot, homePath, body, expected)
}

func (store *Store) saveExpected(expected []byte) error {
	if store != nil && store.borrowedHomeRoot != nil {
		return store.saveExpectedFromRoot(store.borrowedHomeRoot, store.borrowedHomePath, expected)
	}
	if err := validateStoreManifestForPublication(store); err != nil {
		return err
	}
	if err := store.validateRunDirForMutation(); err != nil {
		return err
	}
	body, err := marshalManifest(store.Manifest)
	if err != nil {
		return err
	}
	if store.beforeSave != nil {
		if err := store.beforeSave(); err != nil {
			return err
		}
	}
	return store.publishManifestExpected(body, expected)
}

func (store *Store) saveExpectedFromRoot(homeRoot *os.Root, homePath string, expected []byte) error {
	if err := validateStoreManifestForPublication(store); err != nil {
		return err
	}
	if err := store.validateRunDirForMutationRoot(homeRoot, homePath); err != nil {
		return err
	}
	body, err := marshalManifest(store.Manifest)
	if err != nil {
		return err
	}
	if store.beforeSave != nil {
		if err := store.beforeSave(); err != nil {
			return err
		}
	}
	return store.publishManifestExpectedFromRoot(homeRoot, homePath, body, expected)
}

func validateStoreManifestForPublication(store *Store) error {
	if store == nil {
		return errors.New("backup store is required")
	}
	if _, err := store.expectedManifestGeneration(); err != nil {
		return err
	}
	if len(store.Manifest.Entries) == 0 {
		return errors.New("refusing to publish a zero-entry backup manifest")
	}
	if err := validateCurrentManifest(&store.Manifest); err != nil {
		return fmt.Errorf("refusing to publish an invalid backup manifest: %w", err)
	}
	if err := validateManifestNamespace(&store.Manifest, store.Manifest.ProjectRoot, store.ProjectID, store.RunID, store.RunDir); err != nil {
		return fmt.Errorf("refusing to publish an invalid backup manifest namespace: %w", err)
	}
	return nil
}

func marshalManifest(manifest Manifest) ([]byte, error) {
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func (store *Store) expectedManifestGeneration() ([]byte, error) {
	if store.manifestGenerationKnown {
		return append([]byte(nil), store.persistedManifest...), nil
	}
	return nil, errors.New("backup manifest generation must be reviewed before mutation")
}

func (store *Store) rememberManifestGeneration(body []byte) {
	store.manifestGenerationKnown = true
	store.persistedManifest = append(store.persistedManifest[:0], body...)
}

func (store *Store) publishManifestExpected(body, expected []byte) (returnErr error) {
	parent, err := os.OpenRoot(store.RunDir)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	if err := safefs.AtomicWriteRootFile(parent, ManifestName, body, 0o600, validateExpectedManifest(expected)); err != nil {
		return err
	}
	store.rememberManifestGeneration(body)
	return nil
}

func (store *Store) publishManifestExpectedFromRoot(homeRoot *os.Root, homePath string, body, expected []byte) error {
	rel, err := rootRelativePath(homePath, filepath.Join(store.RunDir, ManifestName))
	if err != nil {
		return err
	}
	validate := validateExpectedManifest(expected)
	if store.loadedRunInfo != nil {
		validateManifest := validate
		validate = func(parent *os.Root, base string) error {
			opened, err := parent.Stat(".")
			if err != nil || !opened.IsDir() || opened.Mode()&os.ModeSymlink != 0 || !os.SameFile(store.loadedRunInfo, opened) {
				return errors.Join(errors.New("backup run generation changed at the manifest mutation boundary"), err)
			}
			return validateManifest(parent, base)
		}
	}
	if err := safefs.AtomicWriteRootFile(homeRoot, rel, body, 0o600, validate); err != nil {
		return err
	}
	if store.loadedRunInfo != nil {
		runRel, err := rootRelativePath(homePath, store.RunDir)
		if err != nil {
			return err
		}
		current, err := homeRoot.Lstat(runRel)
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(store.loadedRunInfo, current) {
			return errors.Join(errors.New("backup run generation changed during manifest publication"), err)
		}
	}
	store.rememberManifestGeneration(body)
	return nil
}

func validateExpectedManifest(expected []byte) func(*os.Root, string) error {
	return func(parent *os.Root, base string) error {
		if expected == nil {
			if _, err := parent.Lstat(base); errors.Is(err, os.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			return errors.New("backup manifest appeared after its generation was reviewed")
		}
		body, _, err := ReadRootRegularFileBoundedInfo(parent, base, maxManifestBytes)
		if err != nil {
			return err
		}
		if !bytes.Equal(body, expected) {
			return errors.New("backup manifest changed after its generation was reviewed")
		}
		return nil
	}
}

// ListRuns returns backup runs for projectRoot, newest first.
func ListRuns(projectRoot string, homeDir string, backupDir string) ([]Run, error) {
	return ListRunsWithOverride(projectRoot, homeDir, "", backupDir)
}

// ListRunsWithOverride returns backup runs for projectRoot, newest first,
// resolving backups under explicitThreadpointHome when it is set.
func ListRunsWithOverride(projectRoot string, homeDir string, explicitThreadpointHome string, backupDir string) ([]Run, error) {
	return listRunsWithOverrideLimits(projectRoot, homeDir, explicitThreadpointHome, backupDir, defaultRunEnumerationLimits())
}

func listRunsWithOverrideLimits(projectRoot string, homeDir string, explicitThreadpointHome string, backupDir string, limits runEnumerationLimits) ([]Run, error) {
	limits, err := normalizeRunEnumerationLimits(limits)
	if err != nil {
		return nil, err
	}
	projectRoot, err = abspath.Abs(projectRoot)
	if err != nil {
		return nil, err
	}
	projectRoot = filepath.Clean(projectRoot)
	backupRoot, err := ResolveBackupRootWithOverride(homeDir, explicitThreadpointHome, backupDir)
	if err != nil {
		return nil, err
	}
	projectID := ProjectID(projectRoot)
	projectDir := filepath.Join(backupRoot, projectID)
	info, err := os.Lstat(projectDir)
	if errors.Is(err, os.ErrNotExist) {
		return []Run{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("backup project run path is not a real directory")
	}
	if err := safefs.RejectExistingSymlinkAncestors(projectDir); err != nil {
		return nil, err
	}
	projectDirRoot, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, err
	}
	defer projectDirRoot.Close()
	opened, err := projectDirRoot.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("backup project run path changed while it was opened"), err)
	}
	return listRunsFromPinnedProjectDir(projectRoot, projectID, projectDir, projectDirRoot, limits)
}

// ListRunsFromRoot lists runs through a caller-retained product-home root.
// homePath supplies only the lexical namespace used in report paths; no
// operation reopens it.
func ListRunsFromRoot(projectRoot string, homeRoot *os.Root, homePath, backupDir string) ([]Run, error) {
	return listRunsFromRootLimits(projectRoot, homeRoot, homePath, backupDir, defaultRunEnumerationLimits())
}

func listRunsFromRootLimits(projectRoot string, homeRoot *os.Root, homePath, backupDir string, limits runEnumerationLimits) ([]Run, error) {
	if homeRoot == nil {
		return nil, errors.New("pinned product-home root is required")
	}
	limits, err := normalizeRunEnumerationLimits(limits)
	if err != nil {
		return nil, err
	}
	projectRoot, err = abspath.Abs(projectRoot)
	if err != nil {
		return nil, err
	}
	projectRoot = filepath.Clean(projectRoot)
	backupRoot, err := resolveBackupRootUnderHome(homePath, backupDir)
	if err != nil {
		return nil, err
	}
	projectID := ProjectID(projectRoot)
	projectDir := filepath.Join(backupRoot, projectID)
	projectRel, err := rootRelativePath(homePath, projectDir)
	if err != nil {
		return nil, err
	}
	info, err := homeRoot.Lstat(projectRel)
	if errors.Is(err, os.ErrNotExist) {
		return []Run{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("backup project run path is not a real directory")
	}
	projectDirRoot, err := homeRoot.OpenRoot(projectRel)
	if err != nil {
		return nil, err
	}
	defer projectDirRoot.Close()
	opened, err := projectDirRoot.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("backup project run path changed while it was opened"), err)
	}
	return listRunsFromPinnedProjectDir(projectRoot, projectID, projectDir, projectDirRoot, limits)
}

func defaultRunEnumerationLimits() runEnumerationLimits {
	return runEnumerationLimits{
		MaxRuns:    maxEnumeratedBackupRuns,
		MaxEntries: maxEnumeratedBackupEntries,
		MaxBytes:   maxEnumeratedManifestBytes,
	}
}

func normalizeRunEnumerationLimits(limits runEnumerationLimits) (runEnumerationLimits, error) {
	if limits.MaxRuns <= 0 || limits.MaxEntries <= 0 || limits.MaxBytes <= 0 {
		return runEnumerationLimits{}, errors.New("backup run enumeration limits must all be positive")
	}
	return limits, nil
}

func listRunsFromPinnedProjectDir(projectRoot, projectID, projectDir string, projectDirRoot *os.Root, limits runEnumerationLimits) ([]Run, error) {
	if projectDirRoot == nil {
		return nil, errors.New("pinned backup project directory is required")
	}
	directory, err := projectDirRoot.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(limits.MaxRuns + 1)
	closeErr := directory.Close()
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(entries) > limits.MaxRuns {
		return nil, fmt.Errorf("backup run enumeration limit exceeded: more than %d project entries", limits.MaxRuns)
	}
	runs := []Run{}
	var manifestBytes int64
	var manifestEntries int
	for _, entry := range entries {
		if !canonicalRunID(entry.Name()) {
			continue
		}
		runInfo, err := projectDirRoot.Lstat(entry.Name())
		if err != nil || !runInfo.IsDir() || runInfo.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(fmt.Errorf("canonical backup run %s is not a physical directory", entry.Name()), err)
		}
		runRoot, err := projectDirRoot.OpenRoot(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("open canonical backup run %s: %w", entry.Name(), err)
		}
		openedRun, statErr := runRoot.Stat(".")
		if statErr != nil || !os.SameFile(runInfo, openedRun) {
			return nil, errors.Join(fmt.Errorf("canonical backup run %s changed while it was opened", entry.Name()), statErr, runRoot.Close())
		}
		manifestInfo, statErr := runRoot.Lstat(ManifestName)
		if statErr != nil || !manifestInfo.Mode().IsRegular() {
			return nil, errors.Join(fmt.Errorf("canonical backup run %s has no regular manifest", entry.Name()), statErr, runRoot.Close())
		}
		if manifestInfo.Size() > maxManifestBytes {
			_ = runRoot.Close()
			return nil, fmt.Errorf("backup manifest %s exceeds the per-run byte limit", filepath.Join(projectDir, entry.Name(), ManifestName))
		}
		if manifestInfo.Size() > limits.MaxBytes-manifestBytes {
			_ = runRoot.Close()
			return nil, fmt.Errorf("backup manifest byte limit exceeded: more than %d bytes", limits.MaxBytes)
		}
		body, readErr := ReadRootRegularFileBounded(runRoot, ManifestName, maxManifestBytes)
		closeRunErr := runRoot.Close()
		if readErr != nil || closeRunErr != nil {
			return nil, errors.Join(fmt.Errorf("read canonical backup run %s manifest", entry.Name()), readErr, closeRunErr)
		}
		manifestBytes += int64(len(body))
		if manifestBytes > limits.MaxBytes {
			return nil, fmt.Errorf("backup manifest byte limit exceeded: more than %d bytes", limits.MaxBytes)
		}
		manifest, err := decodeManifest(body)
		if err != nil {
			return nil, fmt.Errorf("decode canonical backup run %s manifest: %w", entry.Name(), err)
		}
		if len(manifest.Entries) > limits.MaxEntries-manifestEntries {
			return nil, fmt.Errorf("backup entry enumeration limit exceeded: more than %d entries", limits.MaxEntries)
		}
		manifestEntries += len(manifest.Entries)
		runDir := filepath.Join(projectDir, entry.Name())
		if err := validateManifestNamespace(manifest, projectRoot, projectID, entry.Name(), runDir); err != nil {
			return nil, fmt.Errorf("validate canonical backup run %s manifest: %w", entry.Name(), err)
		}
		if manifest.Operation == PendingCommitOperation || manifest.Operation == AbortedCommitOperation || manifest.Operation == RecoveryCommitOperation {
			continue
		}
		runs = append(runs, Run{
			Version: manifest.Version, ProjectID: manifest.ProjectID, RunID: manifest.RunID,
			Operation: manifest.Operation, ProjectRoot: manifest.ProjectRoot, CreatedAt: manifest.CreatedAt,
			Path: runDir, CommitTransaction: manifest.CommitTransaction, Entries: manifest.Entries,
		})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	return runs, nil
}

func validateManifestNamespace(manifest *Manifest, projectRoot, projectID, runID, runDir string) error {
	if manifest == nil {
		return errors.New("backup manifest is required")
	}
	if manifest.ProjectID != projectID || filepath.Clean(manifest.ProjectRoot) != filepath.Clean(projectRoot) || manifest.RunID != runID {
		return errors.New("backup manifest does not belong to the selected project run")
	}
	if filepath.Base(filepath.Clean(runDir)) != runID || filepath.Base(filepath.Dir(filepath.Clean(runDir))) != projectID {
		return errors.New("backup run path does not match the selected project and run")
	}
	for _, entry := range manifest.Entries {
		if entry.BackupPath != "" && (!filepath.IsAbs(entry.BackupPath) || !pathWithin(entry.BackupPath, runDir)) {
			return fmt.Errorf("backup payload for %s is outside the selected project run", entry.Path)
		}
		if entry.Absent {
			if entry.RecoveryPath != "" {
				return fmt.Errorf("absent backup entry for %s has a recovery path", entry.Path)
			}
			continue
		}
		if entry.BackupPath == "" {
			return fmt.Errorf("backup payload for %s is outside the selected project run", entry.Path)
		}
		if entry.RecoveryPath != "" && (entry.RecoveryPath != entry.BackupPath || !filepath.IsAbs(entry.RecoveryPath) || !pathWithin(entry.RecoveryPath, runDir)) {
			return fmt.Errorf("backup recovery payload for %s is outside the selected project run", entry.Path)
		}
	}
	return nil
}

// LatestRun returns the most recent backup run for projectRoot.
func LatestRun(projectRoot string, homeDir string, backupDir string) (*Run, error) {
	return LatestRunWithOverride(projectRoot, homeDir, "", backupDir)
}

// LatestRunWithOverride returns the most recent backup run for projectRoot,
// resolving backups under explicitThreadpointHome when it is set.
func LatestRunWithOverride(projectRoot string, homeDir string, explicitThreadpointHome string, backupDir string) (*Run, error) {
	runs, err := ListRunsWithOverride(projectRoot, homeDir, explicitThreadpointHome, backupDir)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, os.ErrNotExist
	}
	return &runs[0], nil
}

// LatestRunFromRoot returns the latest run through a retained product root.
func LatestRunFromRoot(projectRoot string, homeRoot *os.Root, homePath, backupDir string) (*Run, error) {
	runs, err := ListRunsFromRoot(projectRoot, homeRoot, homePath, backupDir)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, os.ErrNotExist
	}
	return &runs[0], nil
}

// FindRun returns the backup run with runID for projectRoot.
func FindRun(projectRoot string, homeDir string, backupDir string, runID string) (*Run, error) {
	return FindRunWithOverride(projectRoot, homeDir, "", backupDir, runID)
}

// FindRunWithOverride returns the backup run with runID for projectRoot,
// resolving backups under explicitThreadpointHome when it is set.
func FindRunWithOverride(projectRoot string, homeDir string, explicitThreadpointHome string, backupDir string, runID string) (*Run, error) {
	runs, err := ListRunsWithOverride(projectRoot, homeDir, explicitThreadpointHome, backupDir)
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		if run.RunID == runID {
			return &run, nil
		}
	}
	return nil, os.ErrNotExist
}

// FindRunFromRoot returns runID through a retained product root.
func FindRunFromRoot(projectRoot string, homeRoot *os.Root, homePath, backupDir, runID string) (*Run, error) {
	runs, err := ListRunsFromRoot(projectRoot, homeRoot, homePath, backupDir)
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		if run.RunID == runID {
			return &run, nil
		}
	}
	return nil, os.ErrNotExist
}

// ReadManifest reads a backup manifest from path.
func ReadManifest(path string) (*Manifest, error) {
	body, err := ReadRegularFileBounded(path, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	return decodeManifest(body)
}

// ReadManifestFromRoot reads a bounded manifest beneath a caller-retained
// product-home root without reopening the ambient home pathname.
func ReadManifestFromRoot(homeRoot *os.Root, homePath, manifestPath string) (*Manifest, error) {
	if homeRoot == nil {
		return nil, errors.New("pinned product-home root is required")
	}
	rel, err := rootRelativePath(homePath, manifestPath)
	if err != nil {
		return nil, err
	}
	body, err := ReadRootRegularFileBounded(homeRoot, rel, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	return decodeManifest(body)
}

func decodeManifest(body []byte) (*Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("backup manifest contains multiple JSON values")
		}
		return nil, err
	}
	if err := validateCurrentManifest(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func validateCurrentManifest(manifest *Manifest) error {
	if manifest == nil {
		return errors.New("backup manifest is required")
	}
	if manifest.Version != ManifestVersion {
		return fmt.Errorf("backup manifest version %d does not match current version %d", manifest.Version, ManifestVersion)
	}
	if !canonicalOperation(manifest.Operation) {
		return errors.New("backup manifest requires a canonical operation")
	}
	if manifest.ProjectRoot == "" || !filepath.IsAbs(manifest.ProjectRoot) || filepath.Clean(manifest.ProjectRoot) != manifest.ProjectRoot {
		return errors.New("backup manifest requires a canonical absolute projectRoot")
	}
	if manifest.ProjectID == "" || ProjectID(manifest.ProjectRoot) != manifest.ProjectID {
		return errors.New("backup manifest projectId does not match projectRoot")
	}
	if !canonicalRunID(manifest.RunID) {
		return errors.New("backup manifest requires a canonical runId")
	}
	if manifest.CreatedAt.IsZero() || manifest.CreatedAt.Location() != time.UTC {
		return errors.New("backup manifest requires a nonzero UTC createdAt")
	}
	if len(manifest.Entries) == 0 {
		return errors.New("backup manifest requires at least one entry")
	}
	if err := validateManifestTransaction(manifest); err != nil {
		return err
	}
	for index := range manifest.Entries {
		if err := validateCurrentManifestEntry(manifest.Entries[index]); err != nil {
			return fmt.Errorf("backup manifest entry %d: %w", index, err)
		}
	}
	return nil
}

// ValidateRun applies the complete current-schema and project-run namespace
// contract to an enumerated backup run. Consumers such as restoration use this
// at their own trust boundary instead of maintaining a weaker copy of manifest
// validation.
func ValidateRun(run *Run) error {
	if run == nil {
		return errors.New("backup run is required")
	}
	manifest := Manifest{
		Version:           run.Version,
		Operation:         run.Operation,
		ProjectRoot:       run.ProjectRoot,
		ProjectID:         run.ProjectID,
		RunID:             run.RunID,
		CreatedAt:         run.CreatedAt,
		CommitTransaction: run.CommitTransaction,
		Entries:           run.Entries,
	}
	if err := validateCurrentManifest(&manifest); err != nil {
		return err
	}
	if run.Path == "" || !filepath.IsAbs(run.Path) || filepath.Clean(run.Path) != run.Path {
		return errors.New("backup run requires a canonical absolute path")
	}
	return validateManifestNamespace(&manifest, run.ProjectRoot, run.ProjectID, run.RunID, run.Path)
}

func validateManifestTransaction(manifest *Manifest) error {
	transaction := manifest.CommitTransaction
	if transaction == nil {
		switch manifest.Operation {
		case PendingCommitOperation, CommittedCommitOperation, AbortedCommitOperation, RecoveryCommitOperation:
			return errors.New("backup commit operation requires commitTransaction")
		default:
			return nil
		}
	}
	wantOperation := map[string]string{
		CommitTransactionPending:           PendingCommitOperation,
		CommitTransactionCommitted:         CommittedCommitOperation,
		CommitTransactionAborted:           AbortedCommitOperation,
		CommitTransactionRecoveryCommitted: RecoveryCommitOperation,
	}[transaction.State]
	if wantOperation == "" || manifest.Operation != wantOperation {
		return errors.New("backup manifest commit transaction state does not match operation")
	}
	if !canonicalRunID(transaction.StageID) {
		return errors.New("backup manifest commit transaction requires a canonical stageId")
	}
	if manifest.RunID != "commit-"+transaction.StageID {
		return errors.New("backup manifest commit runId is not bound to its stageId")
	}
	for label, value := range map[string]string{"sourcePath": transaction.SourcePath, "targetPath": transaction.TargetPath} {
		clean, err := normalizeEntryPath(value)
		if err != nil || clean != value {
			return errors.Join(fmt.Errorf("backup manifest commit transaction requires a canonical %s", label), err)
		}
	}
	if !validManifestSHA256(transaction.SourceSHA256) || !validManifestSHA256(transaction.TargetSHA256) || transaction.TargetSize <= 0 {
		return errors.New("backup manifest commit transaction has invalid generation metadata")
	}
	if err := validateCommitManifestEntries(manifest, transaction); err != nil {
		return err
	}
	return nil
}

func validateCommitManifestEntries(manifest *Manifest, transaction *CommitTransaction) error {
	if len(manifest.Entries) != 2 {
		return errors.New("backup commit manifest must contain exactly one staged source and exactly one prior canonical generation")
	}
	stageSourceCount := 0
	priorCount := 0
	for _, entry := range manifest.Entries {
		switch entry.Role {
		case "stage-source":
			stageSourceCount++
			if entry.Path != transaction.SourcePath || entry.SHA256 != transaction.SourceSHA256 || entry.Absent || entry.Directory || entry.Symlink || !entry.ModeKnown || entry.Size < 0 || entry.Size > maxBoundCommitSourceBytes {
				return errors.New("backup commit manifest staged-source entry does not match its transaction")
			}
		case "canonical-prior":
			priorCount++
			if entry.Path != transaction.TargetPath || entry.Directory || entry.Symlink || entry.Size < 0 || entry.Size > maxBoundCommitSourceBytes {
				return errors.New("backup commit manifest prior canonical entry does not match its transaction")
			}
			if !entry.Absent && !entry.ModeKnown {
				return errors.New("backup commit manifest prior canonical entry requires mode metadata")
			}
		default:
			return errors.New("backup commit manifest contains an unsupported entry")
		}
	}
	if stageSourceCount != 1 || priorCount != 1 {
		return errors.New("backup commit manifest must contain exactly one staged source and exactly one prior canonical generation")
	}
	return nil
}

func validateCurrentManifestEntry(entry Entry) error {
	if strings.TrimSpace(entry.Role) == "" || entry.Role != strings.TrimSpace(entry.Role) {
		return errors.New("role is required")
	}
	if clean, err := normalizeEntryPath(entry.Path); err != nil || clean != entry.Path {
		return errors.Join(errors.New("path must be canonical"), err)
	}
	if entry.Root != "" && (!filepath.IsAbs(entry.Root) || filepath.Clean(entry.Root) != entry.Root) {
		return errors.New("root must be a canonical absolute path when present")
	}
	if entry.Directory && entry.Symlink {
		return errors.New("entry cannot be both a directory and a symlink")
	}
	if entry.Size < 0 {
		return errors.New("entry has a negative size")
	}
	if entry.PostSHA256 != "" && !validManifestSHA256(entry.PostSHA256) {
		return errors.New("entry has an invalid post-change SHA-256")
	}
	if entry.PendingPostSHA256 != "" && !validManifestSHA256(entry.PendingPostSHA256) {
		return errors.New("entry has an invalid pending post-change SHA-256")
	}
	if entry.Absent {
		if entry.Directory || entry.Symlink || entry.BackupPath != "" || entry.SHA256 != "" || entry.Size != 0 || entry.Mode != 0 || entry.ModeKnown || len(entry.TreeModes) != 0 || entry.RecoveryPath != "" {
			return errors.New("entry has an invalid absent-target encoding")
		}
		return nil
	}
	if !validManifestSHA256(entry.SHA256) {
		return errors.New("entry has an invalid payload SHA-256")
	}
	if entry.BackupPath == "" || !filepath.IsAbs(entry.BackupPath) {
		return errors.New("entry requires an absolute payload path")
	}
	if entry.RecoveryPath != "" && entry.RecoveryPath != entry.BackupPath {
		return errors.New("entry recovery path must equal its durable payload path")
	}
	if !entry.Symlink && !entry.ModeKnown {
		return errors.New("regular and directory entries require mode metadata")
	}
	if entry.Mode > 0o777 {
		return errors.New("entry mode metadata is invalid")
	}
	if entry.Directory {
		if entry.Size != 0 {
			return errors.New("entry has an invalid directory size")
		}
		rootMode, hasRootMode := entry.TreeModes["."]
		if !hasRootMode || rootMode != entry.Mode {
			return errors.New("directory entry requires complete root mode metadata")
		}
		for rel, mode := range entry.TreeModes {
			clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
			if (rel != "." && (clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != rel)) || mode > 0o777 {
				return errors.New("directory entry has invalid tree mode metadata")
			}
		}
	} else if len(entry.TreeModes) != 0 {
		return errors.New("non-directory entry has tree mode metadata")
	}
	if entry.Symlink && entry.Size == 0 {
		return errors.New("entry has an invalid empty symlink marker")
	}
	return nil
}

func validManifestSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

// ReadRegularFileBounded reads a stable, non-symlink regular file without
// following any existing symlink ancestor and rejects growth beyond maxBytes.
func ReadRegularFileBounded(filePath string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("invalid negative file limit for %s", filePath)
	}
	absolute, err := abspath.Abs(filePath)
	if err != nil {
		return nil, err
	}
	if err := safefs.RejectExistingSymlinkAncestors(absolute); err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return ReadRootRegularFileBounded(parent, filepath.Base(absolute), maxBytes)
}

// ReadRootRegularFileBounded reads a stable regular file beneath a pinned
// filesystem root. O_NONBLOCK prevents a raced FIFO replacement from hanging;
// Lstat and SameFile checks reject symlink and type replacements.
func ReadRootRegularFileBounded(root *os.Root, name string, maxBytes int64) ([]byte, error) {
	body, _, err := ReadRootRegularFileBoundedInfo(root, name, maxBytes)
	return body, err
}

// ReadRootRegularFileBoundedInfo is ReadRootRegularFileBounded that also returns
// the opened descriptor's FileInfo, so a caller receives the bytes, mode, and
// identity of the same read and cannot combine a file's content with a
// replacement's mode.
func ReadRootRegularFileBoundedInfo(root *os.Root, name string, maxBytes int64) ([]byte, fs.FileInfo, error) {
	if root == nil {
		return nil, nil, errors.New("file root is required")
	}
	if maxBytes < 0 {
		return nil, nil, fmt.Errorf("invalid negative file limit for %s", name)
	}
	clean, err := cleanRootRelativePath(name)
	if err != nil {
		return nil, nil, err
	}
	rootName := filepath.FromSlash(clean)
	if err := safefs.RejectRootSymlinkAncestors(root, rootName); err != nil {
		return nil, nil, err
	}
	expected, err := root.Lstat(rootName)
	if err != nil {
		return nil, nil, err
	}
	if !expected.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", name)
	}
	if expected.Size() > maxBytes {
		return nil, nil, fmt.Errorf("%s is larger than %d bytes", name, maxBytes)
	}
	file, err := openBoundedRegularRootFile(root, rootName)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return nil, nil, fmt.Errorf("%s changed before it could be read", name)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, nil, fmt.Errorf("%s is larger than %d bytes", name, maxBytes)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	current, err := root.Lstat(rootName)
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(opened, after) || !os.SameFile(opened, current) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || int64(len(body)) != after.Size() {
		return nil, nil, fmt.Errorf("%s changed while it was being read", name)
	}
	return body, opened, nil
}

// StaleWarnings reports old backup runs for projectRoot.
func StaleWarnings(projectRoot string, homeDir string, backupDir string, now time.Time) []string {
	return StaleWarningsWithOverride(projectRoot, homeDir, "", backupDir, now)
}

// StaleWarningsWithOverride reports old backup runs for projectRoot, resolving
// backups under explicitThreadpointHome when it is set.
func StaleWarningsWithOverride(projectRoot string, homeDir string, explicitThreadpointHome string, backupDir string, now time.Time) []string {
	if now.IsZero() {
		now = time.Now()
	}
	runs, err := ListRunsWithOverride(projectRoot, homeDir, explicitThreadpointHome, backupDir)
	if err != nil {
		return []string{fmt.Sprintf("failed checking threadpoint backups: %v", err)}
	}
	var warnings []string
	for _, run := range runs {
		if now.Sub(run.CreatedAt) > staleThreshold {
			warnings = append(warnings, fmt.Sprintf("Threadpoint backup %s for %s is older than 30 days; review or remove %s", run.RunID, run.ProjectRoot, run.Path))
		}
	}
	return warnings
}

// StaleWarningsFromRoot reports old runs through a retained product root.
func StaleWarningsFromRoot(projectRoot string, homeRoot *os.Root, homePath, backupDir string, now time.Time) []string {
	if now.IsZero() {
		now = time.Now()
	}
	runs, err := ListRunsFromRoot(projectRoot, homeRoot, homePath, backupDir)
	if err != nil {
		return []string{fmt.Sprintf("failed checking threadpoint backups: %v", err)}
	}
	var warnings []string
	for _, run := range runs {
		if now.Sub(run.CreatedAt) > staleThreshold {
			warnings = append(warnings, fmt.Sprintf("Threadpoint backup %s for %s is older than 30 days; review or remove %s", run.RunID, run.ProjectRoot, run.Path))
		}
	}
	return warnings
}

// FileSHA256 returns the hex SHA-256 digest of a file's contents.
func FileSHA256(path string) (string, error) {
	// #nosec G304 -- hashing helper intentionally reads the caller-selected file.
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// CopyFile atomically copies source to target with threadpoint's safe file rules.
func CopyFile(source string, target string) error {
	return safefs.AtomicCopyFile(source, target, 0o644)
}

// CopyDir copies a directory tree using safe writes for regular files.
func CopyDir(source string, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, rel)
		if entry.IsDir() {
			// #nosec G301 -- backup directory copies preserve user-visible project directories.
			return os.MkdirAll(dest, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			// #nosec G122 -- WalkDir derives rel from source, so dest remains under the backup target.
			return os.Symlink(linkTarget, dest)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular file in backup source: %s", path)
		}
		return CopyFile(path, dest)
	})
}

func normalizeOptions(opts StoreOptions) (StoreOptions, error) {
	if opts.ProjectRoot == "" {
		opts.ProjectRoot = "."
	}
	root, err := abspath.Abs(opts.ProjectRoot)
	if err != nil {
		return opts, err
	}
	opts.ProjectRoot = filepath.Clean(root)
	if opts.Operation == "" {
		opts.Operation = "unknown"
	}
	if !canonicalOperation(opts.Operation) {
		return opts, fmt.Errorf("backup operation %q must be one canonical path component", opts.Operation)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RunID == "" {
		opts.RunID = DefaultRunID(opts.Operation, opts.Now())
	}
	if err := validateRunID(opts.RunID); err != nil {
		return opts, err
	}
	return opts, nil
}

func validateRunID(runID string) error {
	if !canonicalRunID(runID) {
		return fmt.Errorf("backup run id %q must be one canonical path component", runID)
	}
	return nil
}

func canonicalRunID(runID string) bool {
	if len(runID) == 0 || len(runID) > 255 || strings.TrimSpace(runID) != runID {
		return false
	}
	first := runID[0]
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') && (first < '0' || first > '9') {
		return false
	}
	for _, char := range runID {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func canonicalOperation(operation string) bool {
	return len(operation) <= 64 && safepath.SanitizeName(operation) == operation && canonicalRunID(operation)
}

func normalizeEntryPath(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe backup entry path %q", rel)
	}
	return filepath.ToSlash(clean), nil
}

func pathWithin(target string, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (store *Store) validateRunDirForMutation() error { return store.validateRunDir(true) }

func (store *Store) validateRunDir(create bool) error {
	if store != nil && store.borrowedHomeRoot != nil {
		return store.validateRunDirRoot(store.borrowedHomeRoot, store.borrowedHomePath, create)
	}
	projectRoot := filepath.Join(store.BackupRoot, store.ProjectID)
	if !pathWithin(store.RunDir, projectRoot) || filepath.Base(store.RunDir) != store.RunID {
		return fmt.Errorf("backup run directory %s escapes project backup root %s", store.RunDir, projectRoot)
	}
	// Create the backup hierarchy owner-only so payloads and manifests holding
	// private agent memory cannot be read or traversed by other local users, and
	// enforce the private-state permission boundary on existing directories.
	for _, dir := range []string{store.BackupRoot, projectRoot, store.RunDir} {
		if !create {
			if err := safefs.RejectExistingSymlinkAncestors(dir); err != nil {
				return err
			}
			continue
		}
		if err := ensurePrivateStateDir(dir); err != nil {
			return err
		}
	}
	return safefs.RejectExistingSymlinkAncestors(store.RunDir)
}

func (store *Store) validateRunDirForMutationRoot(homeRoot *os.Root, homePath string) error {
	return store.validateRunDirRoot(homeRoot, homePath, true)
}

func (store *Store) validateRunDirRoot(homeRoot *os.Root, homePath string, create bool) error {
	if store == nil {
		return errors.New("backup store is required")
	}
	if homeRoot == nil {
		return errors.New("pinned product-home root is required")
	}
	providedPath, err := abspath.Abs(homePath)
	if err != nil {
		return err
	}
	providedPath = filepath.Clean(providedPath)
	if store.borrowedHomeRoot != nil {
		borrowedInfo, borrowedErr := store.borrowedHomeRoot.Stat(".")
		providedInfo, providedErr := homeRoot.Stat(".")
		if borrowedErr != nil || providedErr != nil || !borrowedInfo.IsDir() || !providedInfo.IsDir() ||
			providedPath != filepath.Clean(store.borrowedHomePath) || !os.SameFile(borrowedInfo, providedInfo) {
			return errors.Join(errors.New("product-home root does not match the store's retained authority"), borrowedErr, providedErr)
		}
	}
	if store.loadedRunInfo != nil {
		runRel, relErr := rootRelativePath(providedPath, store.RunDir)
		if relErr != nil {
			return relErr
		}
		current, currentErr := homeRoot.Lstat(runRel)
		if currentErr != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(store.loadedRunInfo, current) {
			return errors.Join(errors.New("backup run generation changed after it was reviewed"), currentErr)
		}
	}
	projectRoot := filepath.Join(store.BackupRoot, store.ProjectID)
	if !pathWithin(store.RunDir, projectRoot) || filepath.Base(store.RunDir) != store.RunID {
		return fmt.Errorf("backup run directory %s escapes project backup root %s", store.RunDir, projectRoot)
	}
	for _, dir := range []string{store.BackupRoot, projectRoot, store.RunDir} {
		rel, err := rootRelativePath(providedPath, dir)
		if err != nil {
			return err
		}
		if !create {
			if err := safefs.RejectRootSymlinkAncestors(homeRoot, rel); err != nil {
				return err
			}
			continue
		}
		if err := ensurePrivateStateDirRoot(homeRoot, rel); err != nil {
			return err
		}
	}
	return nil
}

func rootRelativePath(rootPath, target string) (string, error) {
	rootPath, err := abspath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	target, err = abspath.Abs(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(filepath.Clean(rootPath), filepath.Clean(target))
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.Join(errors.New("backup path escapes the pinned product-home root"), err)
	}
	return rel, nil
}

// ProductRootRelativePath returns target relative to the lexical product-home
// namespace, rejecting the home itself and every escape. Callers use the
// result only with a retained product-home root.
func ProductRootRelativePath(homePath, target string) (string, error) {
	return rootRelativePath(homePath, target)
}

func ensurePrivateStateDirRoot(root *os.Root, rel string) error {
	if err := safefs.RejectRootSymlinkAncestors(root, rel); err != nil {
		return err
	}
	if err := root.MkdirAll(rel, 0o700); err != nil {
		return err
	}
	info, err := root.Lstat(rel)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New("backup state path is not a real directory"), err)
	}
	directory, err := root.OpenRoot(rel)
	if err != nil {
		return err
	}
	file, err := directory.Open(".")
	if err != nil {
		return errors.Join(err, directory.Close())
	}
	chmodErr := file.Chmod(0o700)
	return errors.Join(chmodErr, file.Close(), directory.Close())
}

func ensurePrivateStateDir(path string) error {
	if err := safefs.RejectExistingSymlinkAncestors(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	// #nosec G302 -- 0o700 keeps private backup state owner-only yet traversable by the owner.
	return os.Chmod(path, 0o700)
}

func (store *Store) addOrReplace(entry Entry) int {
	if store.index == nil {
		store.index = make(map[string]int, len(store.Manifest.Entries))
		for i := range store.Manifest.Entries {
			store.index[entryKey(store.Manifest.Entries[i].Role, store.Manifest.Entries[i].Path)] = i
		}
	}
	key := entryKey(entry.Role, entry.Path)
	if i, ok := store.index[key]; ok {
		store.Manifest.Entries[i] = entry
		return i
	}
	store.Manifest.Entries = append(store.Manifest.Entries, entry)
	i := len(store.Manifest.Entries) - 1
	store.index[key] = i
	return i
}

func (store *Store) commitEntry(entry Entry) (*Entry, error) {
	if store.borrowedHomeRoot != nil {
		return store.commitEntryFromRoot(store.borrowedHomeRoot, store.borrowedHomePath, entry)
	}
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	original := append([]Entry(nil), store.Manifest.Entries...)
	index := store.addOrReplace(entry)
	saved := store.Manifest.Entries[index]

	if err := store.Save(); err != nil {
		store.Manifest.Entries = original
		store.index = nil
		return nil, err
	}
	return &saved, nil
}

func (store *Store) commitEntryFromRoot(homeRoot *os.Root, homePath string, entry Entry) (*Entry, error) {
	if err := store.requireEntryMutation(); err != nil {
		return nil, err
	}
	original := append([]Entry(nil), store.Manifest.Entries...)
	index := store.addOrReplace(entry)
	saved := store.Manifest.Entries[index]
	if err := store.SaveFromRoot(homeRoot, homePath); err != nil {
		store.Manifest.Entries = original
		store.index = nil
		return nil, err
	}
	return &saved, nil
}

func entryKey(role string, path string) string {
	return role + "\x00" + path
}

func backupPayloadRel(role string, rel string) string {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	sum := sha256.Sum256([]byte(role + "\x00" + clean))
	safe := filepath.FromSlash(safepath.SanitizeRelative(clean))
	dir := filepath.Dir(safe)
	base := filepath.Base(safe)
	name := base + "-" + hex.EncodeToString(sum[:])[:12]
	if dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func backupPayloadRelWithIdentity(role string, rel string, identity string) string {
	return backupPayloadRel(role, rel) + "-" + identity
}

func publishBackupRegularPayload(target string, body []byte) error {
	// Backup payloads hold private agent memory; keep them owner-only.
	if err := safefs.AtomicWriteFileIfMissing(target, body, 0o600); err != nil {
		return err
	}
	published, err := ReadRegularFileBounded(target, int64(len(body)))
	if err != nil {
		return fmt.Errorf("verify content-addressed backup payload %s: %w", target, err)
	}
	if !bytes.Equal(published, body) {
		return fmt.Errorf("content-addressed backup payload does not match its identity: %s", target)
	}
	return nil
}

func publishBackupRegularPayloadRoot(homeRoot *os.Root, homePath, target string, body []byte) error {
	rel, err := rootRelativePath(homePath, target)
	if err != nil {
		return err
	}
	validateMissing := func(parent *os.Root, base string) error {
		if _, err := parent.Lstat(base); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		return errors.New("content-addressed backup payload already exists")
	}
	if _, err := homeRoot.Lstat(rel); errors.Is(err, os.ErrNotExist) {
		if err := safefs.AtomicWriteRootFile(homeRoot, rel, body, 0o600, validateMissing); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	parent, err := homeRoot.OpenRoot(filepath.Dir(rel))
	if err != nil {
		return err
	}
	published, publishedInfo, readErr := ReadRootRegularFileBoundedInfo(parent, filepath.Base(rel), int64(len(body)))
	closeErr := parent.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("verify content-addressed backup payload: %w", errors.Join(readErr, closeErr))
	}
	if !bytes.Equal(published, body) {
		return errors.New("content-addressed backup payload does not match its identity")
	}
	if publishedInfo.Mode().Perm() != 0o600 {
		return errors.New("content-addressed backup payload does not have private permissions")
	}
	return nil
}

// requireEntryMutation forbids unreviewed writes and incremental commit generations.
func (store *Store) requireEntryMutation() error {
	if store == nil {
		return errors.New("backup store is required")
	}
	if _, err := store.expectedManifestGeneration(); err != nil {
		return err
	}
	if store.Manifest.CommitTransaction != nil || store.Manifest.Operation == PendingCommitOperation {
		return errors.New("commit backup entries are immutable; publish a complete pending generation")
	}
	return nil
}
