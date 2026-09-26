// SPDX-License-Identifier: Apache-2.0

// Package stage implements the explicit stage/commit lifecycle for project
// agent artifacts. Review copies live under THREADPOINT_HOME; canonical shared
// and local project artifacts are never used as a staging directory.
package stage

import (
	"bytes"
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
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/internal/abspath"
	"github.com/threadgrid/threadpoint/internal/textdiff"
	"github.com/threadgrid/threadpoint/layout"
	"github.com/threadgrid/threadpoint/safefs"
	"github.com/threadgrid/threadpoint/safepath"
)

const (
	// ManifestFile is the immutable scope and source identity for one stage.
	ManifestFile = "manifest.json"
	// SourceFile is the original bytes captured when the item was staged.
	SourceFile = "source.md"
	// ContentFile is the editable review copy consumed by Commit.
	ContentFile = "content.md"

	maxStageBytes = 8 << 20
	// maxCanonicalBytes is the single ceiling for every readable and
	// publishable canonical generation. A commit must never create bytes that
	// its retry and recovery paths cannot read.
	maxCanonicalBytes               = backup.MaxCanonicalGenerationBytes
	maxEnumeratedStageEntries       = 4096
	maxEnumeratedStageManifestBytes = 64 << 20
	maxEnumeratedStagePathBytes     = 4 << 20
	maxEnumeratedStageDepth         = 128
)

var (
	// ErrClassificationRequired means an untracked, non-ignored source needs an
	// explicit project-shared or project-local mapping before it can be staged.
	ErrClassificationRequired = errors.New("stage classification is required")
	// ErrAlreadyStaged preserves edited review copies instead of overwriting them.
	ErrAlreadyStaged = errors.New("source already has an active stage; commit or discard it first")
	// These three are the production implementations, indirected only so tests
	// can inject failures. atomicWriteRootStageFile writes through the pinned
	// stage root so a swapped stage directory cannot redirect a rebase write:
	// do not inline it or substitute an unrooted write.
	atomicWriteRootStageFile = safefs.AtomicWriteRootFile
	syncStageParent          = syncStageDirectory
	exchangeStageRoots       = safefs.ExchangeRoot
	// The hooks below are nil in production; tests set them to force race,
	// crash, and filesystem-replacement boundaries.
	commitBeforePostPublishValidation  func()
	commitBeforeStageCleanupValidation func()
	commitStageCleanupAfterDetach      func()
	stageGenerationAfterExchange       func(*os.Root, string, string) error
	stageGenerationAfterDetach         func(*os.Root, string, string) error
	discardStageAfterDetach            func()
	stageAfterOpen                     func()
	stageTempAfterCreate               func(*os.Root, string, fs.FileInfo) error
	openMutationRootAfterResolve       func(string)
	stageBeforeClassify                func()
	stageEnumerationAfterManifestStat  func(*os.Root) error
	// Production revalidates identity after this seam and refuses to unlink a
	// replacement inserted immediately before removal.
	removePinnedRootBeforeEntryRemove func(*os.Root, string)
	gitRootBeforeRun                  func()
	gitRootAfterRun                   func()
	gitRootOpened                     func(uintptr)
)

// Scope identifies the canonical project lane. It deliberately contains only
// the two public project lanes; user-private and project-private artifacts do
// not enter this package.
type Scope string

// ScopeProjectShared publishes to the shared project knowledgebase;
// ScopeProjectLocal publishes to the repository-local private lane.
const (
	ScopeProjectShared Scope = "project-shared"
	ScopeProjectLocal  Scope = "project-local"
)

// Kind classifies the source artifact for target mapping.
type Kind string

// KindInstruction through KindAgent identify the supported source artifact
// shapes used when mapping a stage target.
const (
	KindInstruction Kind = "instruction"
	KindKnowledge   Kind = "knowledge"
	KindRule        Kind = "rule"
	KindSkill       Kind = "skill"
	KindPrompt      Kind = "prompt"
	KindCommand     Kind = "command"
	KindAgent       Kind = "agent"
)

// Input describes a regular native project artifact to stage. Source is always
// relative to Root; callers cannot stage an arbitrary path outside the project.
type Input struct {
	Provider      string
	Source        string
	Kind          Kind
	RequiredScope Scope
}

// Options configures a stage creation operation. Classify maps an otherwise
// unclassified source path to exactly one project scope.
type Options struct {
	Root            string
	ThreadpointHome string
	Inputs          []Input
	Classify        map[string]Scope
	Warnings        []string
}

// Record is the durable, scope-pinned description of a review copy.
type Record struct {
	SchemaVersion string `json:"schemaVersion"`
	ID            string `json:"id"`
	ProjectID     string `json:"projectId"`
	Root          string `json:"root"`
	Scope         Scope  `json:"scope"`
	Provider      string `json:"provider"`
	Source        string `json:"source"`
	Kind          Kind   `json:"kind"`
	Target        string `json:"target"`
	SourceSHA256  string `json:"sourceSha256"`
	ContentSHA256 string `json:"contentSha256"`
	CreatedAt     string `json:"createdAt"`
	Dir           string `json:"dir"`
}

// Report is returned after review copies are created.
type Report struct {
	SchemaVersion string   `json:"schemaVersion"`
	Root          string   `json:"root"`
	Stages        []Record `json:"stages"`
	Warnings      []string `json:"warnings,omitempty"`
}

// CommitOptions selects exactly one existing staged item.
type CommitOptions struct {
	Root            string
	ThreadpointHome string
	ID              string
}

// CommitInput is the public reviewed-byte API. It is deliberately detached
// from stage storage so paid callers can commit only reviewed project output
// without accessing user-private or project-private material.
type CommitInput struct {
	Root                  string
	Scope                 Scope
	Provider              string
	Source                string
	Kind                  Kind
	ID                    string
	Content               []byte
	ValidateBeforePublish func() error
	ValidateAfterPublish  func() error
	// PrepareCanonicalTransaction seals both sides of the canonical
	// publication before mutation. Transactional callers should prefer it to
	// PrepareTransaction so restart recovery has the exact prior generation.
	PrepareCanonicalTransaction func(CanonicalTransactionSnapshot) error
	// PrepareTransaction is retained for callers that do not own durable
	// restart recovery. It receives only the proposed generation.
	PrepareTransaction  func(target string, proposed []byte) error
	FinalizeTransaction func(target string, proposed []byte) error
	AbortTransaction    func() error
}

// CanonicalTransactionSnapshot binds a proposed canonical generation to the
// exact bytes and permission mode observed immediately before publication.
// PriorExists distinguishes an absent canonical path from an existing empty
// regular file.
type CanonicalTransactionSnapshot struct {
	Target      string
	Proposed    []byte
	PriorExists bool
	PriorBody   []byte
	PriorMode   os.FileMode
}

// CommitReport records the canonical project file written from a stage.
type CommitReport struct {
	SchemaVersion string `json:"schemaVersion"`
	ID            string `json:"id,omitempty"`
	Scope         Scope  `json:"scope"`
	Target        string `json:"target"`
	BackupRun     string `json:"backupRun"`
	GitNextStep   string `json:"gitNextStep"`
}

type openedStage struct {
	record       Record
	dir          string
	info         fs.FileInfo
	homeRoot     *os.Root
	homeGuard    *selectedRootGuard
	ownsHomeRoot bool
	scopeRoot    *os.Root
	stageRoot    *os.Root
}

// selectedRootGuard retains the caller-visible logical parent/entry together
// with the physical root capability used for I/O. A physical descriptor alone
// remains usable after the selected entry is retargeted or detached; this guard
// makes that change observable at every publication boundary.
type selectedRootGuard struct {
	logical    string
	parentPath string
	entryName  string
	root       *os.Root
	parent     *os.Root
	rootInfo   fs.FileInfo
	parentInfo fs.FileInfo
	entryInfo  fs.FileInfo
}

func retainSelectedRoot(logical string, root *os.Root) (*selectedRootGuard, error) {
	if root == nil {
		return nil, errors.New("selected physical root is required")
	}
	abs, err := abspath.Abs(logical)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	opened, openedErr := root.Stat(".")
	current, currentErr := os.Stat(abs)
	if openedErr != nil || currentErr != nil || !opened.IsDir() || !current.IsDir() || !os.SameFile(opened, current) {
		return nil, errors.Join(errors.New("selected logical root changed while its physical generation was retained"), openedErr, currentErr)
	}
	guard := &selectedRootGuard{logical: abs, root: root, rootInfo: opened}
	parentPath := filepath.Dir(abs)
	entryName := filepath.Base(abs)
	if parentPath == abs || entryName == "." || entryName == ".." || entryName == string(filepath.Separator) {
		entry, entryErr := os.Lstat(abs)
		if entryErr != nil {
			return nil, entryErr
		}
		guard.entryInfo = entry
		return guard, nil
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, err
	}
	parentInfo, parentErr := parent.Stat(".")
	ambientParent, ambientParentErr := os.Stat(parentPath)
	entry, entryErr := parent.Lstat(entryName)
	ambientEntry, ambientEntryErr := os.Lstat(abs)
	if parentErr != nil || ambientParentErr != nil || entryErr != nil || ambientEntryErr != nil ||
		!parentInfo.IsDir() || !ambientParent.IsDir() || !os.SameFile(parentInfo, ambientParent) || !os.SameFile(entry, ambientEntry) {
		return nil, errors.Join(errors.New("selected logical root parent or entry changed while it was retained"), parentErr, ambientParentErr, entryErr, ambientEntryErr, parent.Close())
	}
	guard.parentPath = parentPath
	guard.entryName = entryName
	guard.parent = parent
	guard.parentInfo = parentInfo
	guard.entryInfo = entry
	return guard, nil
}

func retainedMutationProjectGuard(logical string, retainedRoot *os.Root) (*selectedRootGuard, error) {
	if retainedRoot == nil {
		// A nil retained root means the project mutation is not applicable; the
		// optional guard is intentionally absent without an error.
		return nil, nil //nolint:nilnil
	}
	guard, err := retainSelectedRoot(logical, retainedRoot)
	if err != nil {
		return nil, err
	}
	if err := guard.Verify(); err != nil {
		return nil, errors.Join(err, guard.Close())
	}
	return guard, nil
}

// Verify proves the caller-visible path still selects the retained root.
func (guard *selectedRootGuard) Verify() error {
	if guard == nil || guard.root == nil || guard.rootInfo == nil || guard.entryInfo == nil {
		return errors.New("selected logical root guard is incomplete")
	}
	opened, openedErr := guard.root.Stat(".")
	current, currentErr := os.Stat(guard.logical)
	ambientEntry, entryErr := os.Lstat(guard.logical)
	if openedErr != nil || currentErr != nil || entryErr != nil || !opened.IsDir() || !current.IsDir() ||
		!os.SameFile(guard.rootInfo, opened) || !os.SameFile(guard.rootInfo, current) || !os.SameFile(guard.entryInfo, ambientEntry) {
		return errors.Join(errors.New("selected logical root changed from its retained physical generation"), openedErr, currentErr, entryErr)
	}
	if guard.parent == nil {
		return nil
	}
	parent, parentErr := guard.parent.Stat(".")
	ambientParent, ambientParentErr := os.Stat(guard.parentPath)
	entry, parentEntryErr := guard.parent.Lstat(guard.entryName)
	if parentErr != nil || ambientParentErr != nil || parentEntryErr != nil || !parent.IsDir() || !ambientParent.IsDir() ||
		!os.SameFile(guard.parentInfo, parent) || !os.SameFile(guard.parentInfo, ambientParent) || !os.SameFile(guard.entryInfo, entry) {
		return errors.Join(errors.New("selected logical root changed from its retained parent and entry"), parentErr, ambientParentErr, parentEntryErr)
	}
	return nil
}

// Close releases the retained logical-parent handle.
func (guard *selectedRootGuard) Close() error {
	if guard == nil || guard.parent == nil {
		return nil
	}
	err := guard.parent.Close()
	guard.parent = nil
	return err
}

// Close releases the retained stage and ancestor directory handles.
func (stage *openedStage) Close() error {
	if stage == nil {
		return nil
	}
	err := errors.Join(closeOSRoot(stage.stageRoot), closeOSRoot(stage.scopeRoot), stage.homeGuard.Close())
	if stage.ownsHomeRoot {
		err = errors.Join(err, closeOSRoot(stage.homeRoot))
	}
	stage.stageRoot = nil
	stage.scopeRoot = nil
	stage.homeRoot = nil
	stage.homeGuard = nil
	stage.ownsHomeRoot = false
	return err
}

func closeOSRoot(root *os.Root) error {
	if root == nil {
		return nil
	}
	return root.Close()
}

func verifyLockedStageHome(stage *openedStage) error {
	if stage == nil || stage.homeRoot == nil {
		return errors.New("opened stage home root is required")
	}
	if stage.homeGuard != nil {
		return stage.homeGuard.Verify()
	}
	if stage.ownsHomeRoot {
		return nil
	}
	return verifyMutationRootPath(stage.homeRoot)
}

// TargetFor maps an artifact to the canonical location inside its immutable
// scope. It is shared by standalone threadpoint and callers using CommitReviewed.
func TargetFor(scope Scope, provider, source string, kind Kind) (string, error) {
	if err := validateScope(scope); err != nil {
		return "", err
	}
	provider = safepath.SanitizeSegment(strings.TrimSpace(provider))
	source = filepath.ToSlash(filepath.Clean(source))
	if err := validateRelative(source); err != nil {
		return "", err
	}
	base := safepath.SanitizeSegment(strings.TrimSuffix(filepath.Base(source), filepath.Ext(source)))
	agentsDir := layout.AgentsDir
	guide := layout.GuideFile
	if scope == ScopeProjectLocal {
		agentsDir = ".agents.local"
		guide = "AGENTS.local.md"
	}
	switch kind {
	case KindInstruction:
		return guide, nil
	case KindRule:
		return filepath.ToSlash(filepath.Join(agentsDir, "rules", provider+"-"+base+".md")), nil
	case KindSkill:
		identity := shortID(provider + "\x00" + source)
		return filepath.ToSlash(filepath.Join(agentsDir, "skills", provider+"-"+base+"-"+identity, "SKILL.md")), nil
	default:
		return filepath.ToSlash(filepath.Join(agentsDir, "knowledge", "imported", provider, base+".md")), nil
	}
}

// ClassifyProjectSource applies the public project-lane rule: tracked sources
// are shared, pre-existing ignored sources are local, and only neither may use
// an explicit classification. The ignored-source path preserves a user's
// existing privacy choice; threadpoint does not add or recommend native artifact
// ignore rules. It is exported so paid callers can use identical Git scope
// semantics without receiving access to private stage storage.
func ClassifyProjectSource(root, source string, explicit map[string]Scope) (Scope, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("project root is required")
	}
	absRoot, err := abspath.Abs(root)
	if err != nil {
		return "", err
	}
	rel, err := normalizedSource(filepath.Clean(absRoot), source)
	if err != nil {
		return "", err
	}
	return classify(filepath.Clean(absRoot), rel, "", explicit)
}

// ClassifyProjectSourceFromRoot runs Git against the exact physical worktree
// retained by the caller while proving that logicalRoot still selects that
// generation before and after the query. Paid callers use this public,
// provider-neutral boundary instead of reopening an ambient project pathname.
func ClassifyProjectSourceFromRoot(logicalRoot, source string, explicit map[string]Scope, retainedRoot *os.Root) (scope Scope, returnErr error) {
	if retainedRoot == nil {
		return "", errors.New("retained project root is required")
	}
	absRoot, err := abspath.Abs(logicalRoot)
	if err != nil {
		return "", err
	}
	absRoot = filepath.Clean(absRoot)
	rel, err := normalizedSource(absRoot, source)
	if err != nil {
		return "", err
	}
	guard, err := retainSelectedRoot(absRoot, retainedRoot)
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, guard.Close()) }()
	if err := guard.Verify(); err != nil {
		return "", err
	}
	scope, err = classifyFromRoot(retainedRoot, rel, "", explicit)
	if err != nil {
		return "", err
	}
	if err := guard.Verify(); err != nil {
		return "", err
	}
	return scope, nil
}

// NativeArtifactIgnoreWarning reports the classification advisory for an ignored
// provider-native source. Existing ignored sources remain eligible for the
// project-local canonical lane, but threadpoint only manages ignores for its
// own canonical local outputs.
func NativeArtifactIgnoreWarning(root, source string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("project root is required")
	}
	absRoot, err := abspath.Abs(root)
	if err != nil {
		return "", err
	}
	rel, err := normalizedSource(filepath.Clean(absRoot), source)
	if err != nil {
		return "", err
	}
	return nativeArtifactIgnoreWarning(filepath.Clean(absRoot), rel)
}

func nativeArtifactIgnoreWarning(gitRoot, rel string) (string, error) {
	ignored, err := gitQuery(gitRoot, nil, "check-ignore", "-q", "--", rel)
	if err != nil {
		return "", err
	}
	if !ignored {
		return "", nil
	}
	return fmt.Sprintf("provider-native source %s is ignored by Git; threadpoint does not add or recommend native artifact ignore rules. This reviewed source will be consolidated into project-local canonical memory.", rel), nil
}

func nativeArtifactIgnoreWarningFromRoot(gitRoot *os.Root, rel string) (string, error) {
	ignored, err := gitQuery("", gitRoot, "check-ignore", "-q", "--", rel)
	if err != nil {
		return "", err
	}
	if !ignored {
		return "", nil
	}
	return fmt.Sprintf("provider-native source %s is ignored by Git; threadpoint does not add or recommend native artifact ignore rules. This reviewed source will be consolidated into project-local canonical memory.", rel), nil
}

// NormalizeReviewedContent creates canonical, provider-neutral content from
// supplied reviewed bytes. Scope is intentionally not inferred from content.
func NormalizeReviewedContent(input CommitInput) (target string, content []byte, err error) {
	target, err = TargetFor(input.Scope, input.Provider, input.Source, input.Kind)
	if err != nil {
		return "", nil, err
	}
	if len(input.Content) == 0 {
		return "", nil, errors.New("reviewed content is empty")
	}
	if len(input.Content) > maxStageBytes {
		return "", nil, fmt.Errorf("reviewed content exceeds %d bytes", maxStageBytes)
	}
	body := strings.TrimSpace(string(input.Content))
	if body == "" {
		return "", nil, errors.New("reviewed content is empty")
	}
	marker := fmt.Sprintf(`<!-- threadpoint:committed id="%s" provider="%s" source="%s" kind="%s" -->`, safeID(input.ID), strings.TrimSpace(input.Provider), filepath.ToSlash(input.Source), input.Kind)
	if input.Kind == KindSkill {
		return target, []byte(body + "\n\n" + marker + "\n"), nil
	}
	if input.Kind == KindInstruction {
		return target, []byte("## Imported Agent Guidance\n\n" + marker + "\n\n" + body + "\n"), nil
	}
	title := strings.ReplaceAll(strings.TrimSuffix(filepath.Base(input.Source), filepath.Ext(input.Source)), "-", " ")
	if title == "" || title == "." {
		title = "Imported Knowledge"
	}
	return target, []byte("# " + title + "\n\n" + marker + "\n\n" + body + "\n"), nil
}

// Create creates private review copies for the supplied native project inputs.
// Existing stages are never replaced, including when their source has changed.
func Create(ctx context.Context, opts Options) (returnReport *Report, returnErr error) {
	return stage(ctx, opts, nil, nil)
}

// CreateFromRoot creates private review copies beneath a caller-retained
// threadpoint product-home root. The root is borrowed and remains owned by the
// caller. Lock-owning callers use this entrypoint so stage publication cannot
// reopen an ambient replacement product home.
func CreateFromRoot(ctx context.Context, opts Options, retainedHomeRoot *os.Root) (returnReport *Report, returnErr error) {
	if retainedHomeRoot == nil {
		return nil, errors.New("retained threadpoint home root is required")
	}
	return stage(ctx, opts, retainedHomeRoot, nil)
}

// CreateFromRoots creates review copies through both the caller-retained
// product-home and selected-project generations. Lock-owning callers use this
// entrypoint so source reads, Git classification, and stage publication remain
// bound to the same project generation as their cooperative project lock.
func CreateFromRoots(ctx context.Context, opts Options, retainedHomeRoot, retainedProjectRoot *os.Root) (returnReport *Report, returnErr error) {
	if retainedHomeRoot == nil || retainedProjectRoot == nil {
		return nil, errors.New("retained threadpoint home and project roots are required")
	}
	return stage(ctx, opts, retainedHomeRoot, retainedProjectRoot)
}

func stage(ctx context.Context, opts Options, retainedHomeRoot, retainedProjectRoot *os.Root) (returnReport *Report, returnErr error) {
	root, home, err := normalizeOptions(opts.Root, opts.ThreadpointHome)
	if err != nil {
		return nil, err
	}
	if retainedHomeRoot != nil {
		if err := verifyMutationRootPath(retainedHomeRoot); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projectRoot := retainedProjectRoot
	if projectRoot == nil {
		projectRoot, err = openMutationRoot(root)
		if err != nil {
			return nil, err
		}
		defer projectRoot.Close()
	} else if opened, openErr := projectRoot.Stat("."); openErr != nil || !opened.IsDir() {
		return nil, errors.Join(errors.New("retained project root is not a directory"), openErr)
	}
	projectGuard, err := retainSelectedRoot(root, projectRoot)
	if err != nil {
		return nil, err
	}
	defer projectGuard.Close()
	inputs := append([]Input(nil), opts.Inputs...)
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Source < inputs[j].Source })
	projectID := projectID(root)
	report := &Report{SchemaVersion: "threadpoint.stage.v1", Root: root, Warnings: append([]string(nil), opts.Warnings...)}
	type createdStage struct {
		dir    string
		record Record
		info   fs.FileInfo
	}
	var created []createdStage
	defer func() {
		if returnErr == nil {
			return
		}
		for index := len(created) - 1; index >= 0; index-- {
			if retainedHomeRoot == nil {
				returnErr = errors.Join(returnErr, removeStageGeneration(created[index].dir, created[index].info))
			} else {
				returnErr = errors.Join(returnErr, removeStageGenerationFromHomeRoot(retainedHomeRoot, created[index].record, created[index].info))
			}
		}
		returnReport = nil
	}()
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel, err := normalizedSource(root, input.Source)
		if err != nil {
			return nil, err
		}
		if stageBeforeClassify != nil {
			stageBeforeClassify()
		}
		if err := projectGuard.Verify(); err != nil {
			return nil, err
		}
		scope, err := classifyFromRoot(projectRoot, rel, input.RequiredScope, opts.Classify)
		if err != nil {
			return nil, err
		}
		if err := projectGuard.Verify(); err != nil {
			return nil, err
		}
		if warning, err := nativeArtifactIgnoreWarningFromRoot(projectRoot, rel); err != nil {
			return nil, err
		} else if warning != "" {
			report.Warnings = append(report.Warnings, warning)
		}
		if err := projectGuard.Verify(); err != nil {
			return nil, err
		}
		target, err := TargetFor(scope, input.Provider, rel, input.Kind)
		if err != nil {
			return nil, err
		}
		body, err := backup.ReadRootRegularFileBounded(projectRoot, filepath.FromSlash(rel), maxStageBytes)
		if err != nil {
			return nil, fmt.Errorf("read stage source %s: %w", rel, err)
		}
		if err := projectGuard.Verify(); err != nil {
			return nil, err
		}
		var existing *Record
		if retainedHomeRoot == nil {
			existing, err = findBySource(home, root, projectID, rel)
		} else {
			existing, err = findBySourceFromRoot(retainedHomeRoot, home, root, projectID, rel)
		}
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return nil, fmt.Errorf("%w: %s (%s)", ErrAlreadyStaged, rel, existing.ID)
		}
		id := newID()
		dir := stageDir(home, projectID, scope, id)
		record := Record{
			SchemaVersion: "threadpoint.stage-record.v1", ID: id, ProjectID: projectID, Root: root,
			Scope: scope, Provider: input.Provider, Source: rel, Kind: input.Kind, Target: target,
			SourceSHA256: digest(body), ContentSHA256: digest(body), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Dir: dir,
		}
		var info fs.FileInfo
		if err := projectGuard.Verify(); err != nil {
			return nil, err
		}
		if retainedHomeRoot == nil {
			info, err = writeStage(dir, record, body)
		} else {
			if err := verifyMutationRootPath(retainedHomeRoot); err != nil {
				return nil, err
			}
			info, err = writeStageFromHomeRoot(retainedHomeRoot, record, body)
		}
		if info != nil {
			created = append(created, createdStage{dir: dir, record: record, info: info})
		}
		if err != nil {
			return nil, err
		}
		if err := projectGuard.Verify(); err != nil {
			return nil, err
		}
		if info == nil || !info.IsDir() {
			return nil, errors.Join(errors.New("published stage is not a directory"), err)
		}
		report.Stages = append(report.Stages, record)
	}
	return report, nil
}

// Preview discovers exactly the stages that Create would create without writing
// review copies. Preview records intentionally have no ID or Dir because no
// durable stage exists until the user confirms --apply.
func Preview(ctx context.Context, opts Options) (*Report, error) {
	root, home, err := normalizeOptions(opts.Root, opts.ThreadpointHome)
	if err != nil {
		return nil, err
	}
	projectRoot, err := openMutationRoot(root)
	if err != nil {
		return nil, err
	}
	defer projectRoot.Close()
	projectGuard, err := retainSelectedRoot(root, projectRoot)
	if err != nil {
		return nil, err
	}
	defer projectGuard.Close()
	inputs := append([]Input(nil), opts.Inputs...)
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Source < inputs[j].Source })
	project := projectID(root)
	report := &Report{SchemaVersion: "threadpoint.stage-plan.v1", Root: root, Warnings: append([]string(nil), opts.Warnings...)}
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel, err := normalizedSource(root, input.Source)
		if err != nil {
			return nil, err
		}
		if err := projectGuard.Verify(); err != nil {
			return nil, err
		}
		scope, err := classifyFromRoot(projectRoot, rel, input.RequiredScope, opts.Classify)
		if err != nil {
			return nil, err
		}
		if err := projectGuard.Verify(); err != nil {
			return nil, err
		}
		if warning, err := nativeArtifactIgnoreWarningFromRoot(projectRoot, rel); err != nil {
			return nil, err
		} else if warning != "" {
			report.Warnings = append(report.Warnings, warning)
		}
		target, err := TargetFor(scope, input.Provider, rel, input.Kind)
		if err != nil {
			return nil, err
		}
		body, err := backup.ReadRootRegularFileBounded(projectRoot, filepath.FromSlash(rel), maxStageBytes)
		if err != nil {
			return nil, fmt.Errorf("read stage source %s: %w", rel, err)
		}
		if err := projectGuard.Verify(); err != nil {
			return nil, err
		}
		if existing, err := findBySource(home, root, project, rel); err != nil {
			return nil, err
		} else if existing != nil {
			return nil, fmt.Errorf("%w: %s (%s)", ErrAlreadyStaged, rel, existing.ID)
		}
		report.Stages = append(report.Stages, Record{
			SchemaVersion: "threadpoint.stage-record.v1", ProjectID: project, Root: root, Scope: scope,
			Provider: input.Provider, Source: rel, Kind: input.Kind, Target: target, SourceSHA256: digest(body), ContentSHA256: digest(body),
		})
	}
	return report, nil
}

// List returns all active stages for Root, ordered by scope and identifier.
func List(root, threadpointHome string) ([]Record, error) {
	root, home, err := normalizeOptions(root, threadpointHome)
	if err != nil {
		return nil, err
	}
	project := projectID(root)
	return listStages(home, root, project)
}

// Diff returns a redaction-free unified diff for the explicit local review
// workflow. Its caller is responsible for the warning required for raw output.
func Diff(root, threadpointHome, id string) (result string, returnErr error) {
	stage, err := openStage(root, threadpointHome, id)
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, stage.Close()) }()
	source, err := backup.ReadRootRegularFileBounded(stage.stageRoot, SourceFile, maxStageBytes)
	if err != nil {
		return "", err
	}
	content, err := backup.ReadRootRegularFileBounded(stage.stageRoot, ContentFile, maxStageBytes)
	if err != nil {
		return "", err
	}
	return textdiff.Unified(stage.record.Source, "source", "staged", string(source), string(content)), nil
}

// ReplaceContent replaces only an existing private review copy. It does not
// alter the source snapshot, scope, or manifest.
func ReplaceContent(root, threadpointHome, id string, body []byte) (returnErr error) {
	return replaceContent(root, threadpointHome, id, body, nil, nil)
}

// ReplaceContentFromRoot replaces a review copy beneath a caller-retained
// threadpoint product-home root. The root is borrowed and is not closed.
func ReplaceContentFromRoot(root, threadpointHome, id string, body []byte, retainedHomeRoot *os.Root) (returnErr error) {
	if retainedHomeRoot == nil {
		return errors.New("retained threadpoint home root is required")
	}
	return replaceContent(root, threadpointHome, id, body, retainedHomeRoot, nil)
}

// ReplaceContentFromRoots replaces a review copy only while the caller's
// retained project generation still matches the selected logical project.
func ReplaceContentFromRoots(root, threadpointHome, id string, body []byte, retainedHomeRoot, retainedProjectRoot *os.Root) (returnErr error) {
	if retainedHomeRoot == nil || retainedProjectRoot == nil {
		return errors.New("retained threadpoint home and project roots are required")
	}
	return replaceContent(root, threadpointHome, id, body, retainedHomeRoot, retainedProjectRoot)
}

func replaceContent(root, threadpointHome, id string, body []byte, retainedHomeRoot, retainedProjectRoot *os.Root) (returnErr error) {
	projectGuard, err := retainedMutationProjectGuard(root, retainedProjectRoot)
	if err != nil {
		return err
	}
	if projectGuard != nil {
		defer func() { returnErr = errors.Join(returnErr, projectGuard.Close()) }()
	}
	stage, err := openStageForMutationRoot(root, threadpointHome, id, retainedHomeRoot)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, stage.Close()) }()
	if len(body) == 0 || len(body) > maxStageBytes {
		return errors.New("edited staged content is empty or too large")
	}
	if err := verifyPinnedStageGeneration(stage.stageRoot, stage.info, stage.record); err != nil {
		return err
	}
	source, err := backup.ReadRootRegularFileBounded(stage.stageRoot, SourceFile, maxStageBytes)
	if err != nil || digest(source) != stage.record.SourceSHA256 {
		return errors.Join(errors.New("stage source generation changed before edit publication"), err)
	}
	stage.record.ContentSHA256 = digest(body)
	if err := verifyLockedStageHome(stage); err != nil {
		return err
	}
	if projectGuard != nil {
		if err := projectGuard.Verify(); err != nil {
			return err
		}
	}
	var validateProjectRoot func() error
	if projectGuard != nil {
		validateProjectRoot = projectGuard.Verify
	}
	return replaceStageGeneration(stage, stage.record, source, body, validateProjectRoot)
}

// RebaseMergedReview atomically updates the review, source snapshot, and
// provenance after a merge. Later source changes still make Commit refuse it.
func RebaseMergedReview(root, threadpointHome, id string, source, body []byte) (returnErr error) {
	return rebaseMergedReview(root, threadpointHome, id, source, body, nil, nil)
}

// RebaseMergedReviewFromRoot publishes merged review provenance beneath a
// caller-retained threadpoint product-home root. The root is borrowed.
func RebaseMergedReviewFromRoot(root, threadpointHome, id string, source, body []byte, retainedHomeRoot *os.Root) (returnErr error) {
	if retainedHomeRoot == nil {
		return errors.New("retained threadpoint home root is required")
	}
	return rebaseMergedReview(root, threadpointHome, id, source, body, retainedHomeRoot, nil)
}

// RebaseMergedReviewFromRoots publishes a merged review only while both
// caller-retained lock generations remain selected.
func RebaseMergedReviewFromRoots(root, threadpointHome, id string, source, body []byte, retainedHomeRoot, retainedProjectRoot *os.Root) (returnErr error) {
	if retainedHomeRoot == nil || retainedProjectRoot == nil {
		return errors.New("retained threadpoint home and project roots are required")
	}
	return rebaseMergedReview(root, threadpointHome, id, source, body, retainedHomeRoot, retainedProjectRoot)
}

func rebaseMergedReview(root, threadpointHome, id string, source, body []byte, retainedHomeRoot, retainedProjectRoot *os.Root) (returnErr error) {
	projectGuard, err := retainedMutationProjectGuard(root, retainedProjectRoot)
	if err != nil {
		return err
	}
	if projectGuard != nil {
		defer func() { returnErr = errors.Join(returnErr, projectGuard.Close()) }()
	}
	stage, err := openStageForMutationRoot(root, threadpointHome, id, retainedHomeRoot)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, stage.Close()) }()
	if len(source) == 0 || len(source) > maxStageBytes {
		return errors.New("merged stage source is empty or too large")
	}
	if len(body) == 0 || len(body) > maxStageBytes {
		return errors.New("merged staged content is empty or too large")
	}
	if err := verifyPinnedStageGeneration(stage.stageRoot, stage.info, stage.record); err != nil {
		return err
	}
	if _, err := backup.ReadRootRegularFileBounded(stage.stageRoot, SourceFile, maxStageBytes); err != nil {
		return err
	}
	if _, err := backup.ReadRootRegularFileBounded(stage.stageRoot, ContentFile, maxStageBytes); err != nil {
		return err
	}
	stage.record.SourceSHA256 = digest(source)
	stage.record.ContentSHA256 = digest(body)
	if err := verifyLockedStageHome(stage); err != nil {
		return err
	}
	if projectGuard != nil {
		if err := projectGuard.Verify(); err != nil {
			return err
		}
	}
	var validateProjectRoot func() error
	if projectGuard != nil {
		validateProjectRoot = projectGuard.Verify
	}
	return replaceStageGeneration(stage, stage.record, source, body, validateProjectRoot)
}

func replaceStageGeneration(stage *openedStage, record Record, source, content []byte, validateMutation func() error) (returnErr error) {
	if stage == nil || stage.scopeRoot == nil {
		return errors.New("opened stage generation is required")
	}
	base := filepath.Base(stage.dir)
	randomID := newID()
	suffix := strings.TrimPrefix(randomID, "stage-")
	nextName := "." + base + ".next-" + suffix
	nextInfo, err := writeStageTransaction(stage.scopeRoot, nextName, record, source, content)
	if err != nil {
		return err
	}
	exchanged := false
	defer func() {
		if !exchanged {
			returnErr = errors.Join(returnErr, removeStageGenerationFromParent(stage.scopeRoot, nextName, nextInfo))
		}
	}()
	current, err := stage.scopeRoot.Lstat(base)
	if err != nil || !current.IsDir() || !os.SameFile(stage.info, current) {
		return errors.Join(errors.New("stage generation changed before rebase publication"), err)
	}
	if validateMutation != nil {
		if err := validateMutation(); err != nil {
			return err
		}
	}
	if err := syncOpenedRoot(stage.scopeRoot); err != nil {
		return err
	}
	exchangeErr := safefs.ErrRenameExchangeUnsupported
	if safefs.RenameRootExchangeSupported() {
		exchangeErr = exchangeStageRoots(stage.scopeRoot, nextName, base)
	}
	if errors.Is(exchangeErr, safefs.ErrRenameExchangeUnsupported) {
		retained, err := publishStageGenerationWithJournal(stage.scopeRoot, nextName, base, suffix, nextInfo, stage.info, validateMutation)
		exchanged = retained
		return err
	}
	if exchangeErr != nil {
		return exchangeErr
	}
	exchanged = true
	installed, installErr := stage.scopeRoot.Lstat(base)
	detached, detachErr := stage.scopeRoot.Lstat(nextName)
	if installErr != nil || detachErr != nil || !installed.IsDir() || !detached.IsDir() || !os.SameFile(nextInfo, installed) || !os.SameFile(stage.info, detached) {
		rollbackErr := rollbackStageGenerationExchange(stage.scopeRoot, nextName, base, nextInfo, stage.info)
		if rollbackErr == nil {
			exchanged = false
		}
		return errors.Join(errors.New("stage generation changed during exchange publication"), installErr, detachErr, rollbackErr)
	}
	if err := syncOpenedRoot(stage.scopeRoot); err != nil {
		return err
	}
	if stageGenerationAfterExchange != nil {
		if err := stageGenerationAfterExchange(stage.scopeRoot, base, nextName); err != nil {
			return err
		}
	}
	if validateMutation != nil {
		if err := validateMutation(); err != nil {
			rollbackErr := rollbackStageGenerationExchange(stage.scopeRoot, nextName, base, nextInfo, stage.info)
			if rollbackErr == nil {
				exchanged = false
			}
			return errors.Join(fmt.Errorf("selected project changed during stage publication: %w", err), rollbackErr)
		}
	}
	if err := removeStageGenerationFromParent(stage.scopeRoot, nextName, stage.info); err != nil {
		return fmt.Errorf("rebased stage published but old generation cleanup failed: %w", err)
	}
	return nil
}

// publishStageGenerationWithJournal is the portability path for filesystems
// without atomic directory exchange. The old and next names form a durable
// transaction journal, with a parent-directory sync before and after each
// rename. openStage reconciles every crash state before exposing the stage.
func publishStageGenerationWithJournal(parent *os.Root, nextName, base, suffix string, nextInfo, oldInfo fs.FileInfo, validateMutation func() error) (bool, error) {
	oldName := "." + base + ".old-" + suffix
	if _, err := parent.Lstat(oldName); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return false, fmt.Errorf("stage transaction journal already exists: %s", oldName)
		}
		return false, err
	}
	// The complete next generation has already been file- and directory-synced.
	// Persist its parent entry before the stable name can be detached.
	if err := syncOpenedRoot(parent); err != nil {
		return false, err
	}
	if validateMutation != nil {
		if err := validateMutation(); err != nil {
			return false, err
		}
	}
	if err := safefs.RenameRootNoReplace(parent, base, oldName); err != nil {
		return false, err
	}
	detached, detachErr := parent.Lstat(oldName)
	if detachErr != nil || !detached.IsDir() || !os.SameFile(oldInfo, detached) {
		restored, restoreErr := restoreDetachedStageGeneration(parent, oldName, base, nil)
		return !restored, errors.Join(errors.New("stage generation changed before journaled publication"), detachErr, restoreErr)
	}
	if err := syncOpenedRoot(parent); err != nil {
		return true, err
	}
	if stageGenerationAfterDetach != nil {
		if err := stageGenerationAfterDetach(parent, base, oldName); err != nil {
			return true, err
		}
	}
	if err := safefs.RenameRootNoReplace(parent, nextName, base); err != nil {
		restored, restoreErr := restoreDetachedStageGeneration(parent, oldName, base, oldInfo)
		return !restored, errors.Join(err, restoreErr)
	}
	installed, installErr := parent.Lstat(base)
	if installErr != nil || !installed.IsDir() || !os.SameFile(nextInfo, installed) {
		rolledBack, rollbackErr := rollbackJournaledStageGeneration(parent, nextName, oldName, base, nextInfo, oldInfo)
		return !rolledBack, errors.Join(errors.New("journaled stage generation changed during publication"), installErr, rollbackErr)
	}
	if err := syncOpenedRoot(parent); err != nil {
		return true, err
	}
	if stageGenerationAfterExchange != nil {
		if err := stageGenerationAfterExchange(parent, base, oldName); err != nil {
			return true, err
		}
	}
	if validateMutation != nil {
		if err := validateMutation(); err != nil {
			rolledBack, rollbackErr := rollbackJournaledStageGeneration(parent, nextName, oldName, base, nextInfo, oldInfo)
			return !rolledBack, errors.Join(fmt.Errorf("selected project changed during journaled stage publication: %w", err), rollbackErr)
		}
	}
	if err := removeStageGenerationFromParent(parent, oldName, oldInfo); err != nil {
		return true, fmt.Errorf("rebased stage published but old journal generation cleanup failed: %w", err)
	}
	return true, nil
}

func rollbackJournaledStageGeneration(parent *os.Root, nextName, oldName, base string, nextInfo, oldInfo fs.FileInfo) (bool, error) {
	current, currentErr := parent.Lstat(base)
	old, oldErr := parent.Lstat(oldName)
	if currentErr != nil || oldErr != nil || !current.IsDir() || !old.IsDir() || !os.SameFile(nextInfo, current) || !os.SameFile(oldInfo, old) {
		return false, errors.Join(errors.New("refusing to roll back changed journaled stage generation"), currentErr, oldErr)
	}
	if err := safefs.RenameRootNoReplace(parent, base, nextName); err != nil {
		return false, err
	}
	if err := syncOpenedRoot(parent); err != nil {
		return false, err
	}
	return restoreDetachedStageGeneration(parent, oldName, base, oldInfo)
}

func restoreDetachedStageGeneration(parent *os.Root, oldName, base string, expected fs.FileInfo) (bool, error) {
	if err := safefs.RenameRootNoReplace(parent, oldName, base); err != nil {
		return false, err
	}
	restored, err := parent.Lstat(base)
	if err != nil || !restored.IsDir() || (expected != nil && !os.SameFile(expected, restored)) {
		return false, errors.Join(errors.New("journaled prior stage generation could not be restored"), err)
	}
	if err := syncOpenedRoot(parent); err != nil {
		return false, err
	}
	return true, nil
}

func rollbackStageGenerationExchange(parent *os.Root, nextName, base string, nextInfo, oldInfo fs.FileInfo) error {
	installed, installErr := parent.Lstat(base)
	detached, detachErr := parent.Lstat(nextName)
	if installErr != nil || detachErr != nil || !installed.IsDir() || !detached.IsDir() || !os.SameFile(nextInfo, installed) || !os.SameFile(oldInfo, detached) {
		return errors.Join(errors.New("refusing to roll back changed stage exchange"), installErr, detachErr)
	}
	if err := safefs.ExchangeRoot(parent, nextName, base); err != nil {
		return err
	}
	restored, restoredErr := parent.Lstat(base)
	temporary, temporaryErr := parent.Lstat(nextName)
	if restoredErr != nil || temporaryErr != nil || !restored.IsDir() || !temporary.IsDir() || !os.SameFile(oldInfo, restored) || !os.SameFile(nextInfo, temporary) {
		return errors.Join(errors.New("stage exchange rollback could not verify restored generations"), restoredErr, temporaryErr)
	}
	return syncOpenedRoot(parent)
}

func writeStageTransaction(parent *os.Root, name string, record Record, source, content []byte) (returnInfo fs.FileInfo, returnErr error) {
	if parent == nil {
		return nil, errors.New("stage transaction parent is required")
	}
	if err := parent.Mkdir(name, 0o700); err != nil {
		return nil, err
	}
	publishedInfo, err := parent.Lstat(name)
	if err != nil || !publishedInfo.IsDir() {
		return publishedInfo, errors.Join(errors.New("stage transaction identity is unavailable"), err)
	}
	completed := false
	defer func() {
		if !completed {
			returnErr = errors.Join(returnErr, removeStageGenerationFromParent(parent, name, publishedInfo))
		}
	}()
	transactionRoot, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, transactionRoot.Close()) }()
	if err := validateStageRecord(record); err != nil {
		return nil, err
	}
	manifest, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	for _, item := range []struct {
		name string
		body []byte
	}{{SourceFile, source}, {ContentFile, content}, {ManifestFile, manifest}} {
		if err := atomicWriteRootStageFile(transactionRoot, item.name, item.body, 0o600, nil); err != nil {
			return nil, err
		}
	}
	if err := syncOpenedRoot(transactionRoot); err != nil {
		return nil, err
	}
	current, err := parent.Lstat(name)
	if err != nil || !current.IsDir() || !os.SameFile(publishedInfo, current) {
		return nil, errors.Join(errors.New("stage transaction changed before exchange"), err)
	}
	completed = true
	return publishedInfo, nil
}

// ReadContent returns the current editable review bytes after validating that
// the stage content is a bounded regular file.
func ReadContent(root, threadpointHome, id string) (body []byte, returnErr error) {
	return readContent(root, threadpointHome, id, nil)
}

// ReadContentFromRoot reads a review copy beneath a caller-retained
// threadpoint product-home root. The root is borrowed and is not closed.
func ReadContentFromRoot(root, threadpointHome, id string, retainedHomeRoot *os.Root) (body []byte, returnErr error) {
	if retainedHomeRoot == nil {
		return nil, errors.New("retained threadpoint home root is required")
	}
	return readContent(root, threadpointHome, id, retainedHomeRoot)
}

func readContent(root, threadpointHome, id string, retainedHomeRoot *os.Root) (body []byte, returnErr error) {
	stage, err := openStageForRoot(root, threadpointHome, id, retainedHomeRoot)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, stage.Close()) }()
	return backup.ReadRootRegularFileBounded(stage.stageRoot, ContentFile, maxStageBytes)
}

// ReadReview returns the immutable stage record, frozen source snapshot, and
// current editable review copy for an explicit local review tool.
func ReadReview(root, threadpointHome, id string) (record Record, source []byte, content []byte, returnErr error) {
	return readReview(root, threadpointHome, id, nil)
}

// ReadReviewFromRoot reads a complete review beneath a caller-retained
// threadpoint product-home root. The root is borrowed and is not closed.
func ReadReviewFromRoot(root, threadpointHome, id string, retainedHomeRoot *os.Root) (record Record, source []byte, content []byte, returnErr error) {
	if retainedHomeRoot == nil {
		return Record{}, nil, nil, errors.New("retained threadpoint home root is required")
	}
	return readReview(root, threadpointHome, id, retainedHomeRoot)
}

func readReview(root, threadpointHome, id string, retainedHomeRoot *os.Root) (record Record, source []byte, content []byte, returnErr error) {
	stage, err := openStageForRoot(root, threadpointHome, id, retainedHomeRoot)
	if err != nil {
		return Record{}, nil, nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, stage.Close()) }()
	source, err = backup.ReadRootRegularFileBounded(stage.stageRoot, SourceFile, maxStageBytes)
	if err != nil {
		return Record{}, nil, nil, err
	}
	content, err = backup.ReadRootRegularFileBounded(stage.stageRoot, ContentFile, maxStageBytes)
	if err != nil {
		return Record{}, nil, nil, err
	}
	return stage.record, source, content, nil
}

// ReadCurrentSource returns the current provider source bytes for an existing
// stage. The source remains constrained to the selected project root.
func ReadCurrentSource(root, threadpointHome, id string) (body []byte, returnErr error) {
	return readCurrentSource(root, threadpointHome, id, nil, nil)
}

// ReadCurrentSourceFromRoots reads the provider source and stage identity
// through the same retained home and project generations held by a caller.
func ReadCurrentSourceFromRoots(root, threadpointHome, id string, retainedHomeRoot, retainedProjectRoot *os.Root) (body []byte, returnErr error) {
	if retainedHomeRoot == nil || retainedProjectRoot == nil {
		return nil, errors.New("retained threadpoint home and project roots are required")
	}
	return readCurrentSource(root, threadpointHome, id, retainedHomeRoot, retainedProjectRoot)
}

func readCurrentSource(root, threadpointHome, id string, retainedHomeRoot, retainedProjectRoot *os.Root) (body []byte, returnErr error) {
	root, _, err := normalizeOptions(root, threadpointHome)
	if err != nil {
		return nil, err
	}
	projectRoot := retainedProjectRoot
	if projectRoot == nil {
		projectRoot, err = openMutationRoot(root)
		if err != nil {
			return nil, err
		}
		defer func() { returnErr = errors.Join(returnErr, projectRoot.Close()) }()
	}
	projectGuard, err := retainSelectedRoot(root, projectRoot)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, projectGuard.Close()) }()
	stage, err := openStageForRoot(root, threadpointHome, id, retainedHomeRoot)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, stage.Close()) }()
	source, err := normalizedSource(root, stage.record.Source)
	if err != nil {
		return nil, err
	}
	body, err = backup.ReadRootRegularFileBounded(projectRoot, filepath.FromSlash(source), maxStageBytes)
	if err != nil {
		return nil, err
	}
	if err := projectGuard.Verify(); err != nil {
		return nil, err
	}
	return body, nil
}

// Discard removes exactly one private staged item. It never touches its source
// or any canonical project artifact.
func Discard(root, threadpointHome, id string) (returnErr error) {
	return discard(root, threadpointHome, id, nil, nil)
}

// DiscardFromRoot removes one review copy beneath a caller-retained
// threadpoint product-home root. The root is borrowed and is not closed.
func DiscardFromRoot(root, threadpointHome, id string, retainedHomeRoot *os.Root) (returnErr error) {
	if retainedHomeRoot == nil {
		return errors.New("retained threadpoint home root is required")
	}
	return discard(root, threadpointHome, id, retainedHomeRoot, nil)
}

// DiscardFromRoots removes a review only while the retained project generation
// associated with the caller's lock remains selected.
func DiscardFromRoots(root, threadpointHome, id string, retainedHomeRoot, retainedProjectRoot *os.Root) (returnErr error) {
	if retainedHomeRoot == nil || retainedProjectRoot == nil {
		return errors.New("retained threadpoint home and project roots are required")
	}
	return discard(root, threadpointHome, id, retainedHomeRoot, retainedProjectRoot)
}

func discard(root, threadpointHome, id string, retainedHomeRoot, retainedProjectRoot *os.Root) (returnErr error) {
	projectGuard, err := retainedMutationProjectGuard(root, retainedProjectRoot)
	if err != nil {
		return err
	}
	if projectGuard != nil {
		defer func() { returnErr = errors.Join(returnErr, projectGuard.Close()) }()
	}
	stage, err := openStageForMutationRoot(root, threadpointHome, id, retainedHomeRoot)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, stage.Close()) }()
	if err := verifyLockedStageHome(stage); err != nil {
		return err
	}
	if projectGuard != nil {
		if err := projectGuard.Verify(); err != nil {
			return err
		}
	}
	base := filepath.Base(stage.dir)
	quarantine, err := detachStageGenerationForCommit(stage.scopeRoot, base, stage.info)
	if err != nil {
		return err
	}
	if discardStageAfterDetach != nil {
		discardStageAfterDetach()
	}
	if projectGuard != nil {
		if err := projectGuard.Verify(); err != nil {
			restored, restoreErr := restoreDetachedStageGeneration(stage.scopeRoot, quarantine, base, stage.info)
			if !restored && restoreErr == nil {
				restoreErr = errors.New("detached reviewed stage was not restored")
			}
			return errors.Join(fmt.Errorf("selected project changed during stage discard: %w", err), restoreErr)
		}
	}
	return removeDetachedStageGenerationFromParent(stage.scopeRoot, quarantine, stage.info)
}

// Commit applies exactly one staged review copy then removes that exact stage.
func Commit(ctx context.Context, opts CommitOptions) (*CommitReport, error) {
	return commit(ctx, opts, nil, nil)
}

// CommitFromRoot commits a review whose private state is resolved only beneath
// a caller-retained threadpoint product-home root. The root is borrowed.
func CommitFromRoot(ctx context.Context, opts CommitOptions, retainedHomeRoot *os.Root) (*CommitReport, error) {
	if retainedHomeRoot == nil {
		return nil, errors.New("retained threadpoint home root is required")
	}
	return commit(ctx, opts, retainedHomeRoot, nil)
}

// CommitFromRoots commits a review through the exact product-home and project
// generations retained by the caller's lock set.
func CommitFromRoots(ctx context.Context, opts CommitOptions, retainedHomeRoot, retainedProjectRoot *os.Root) (*CommitReport, error) {
	if retainedHomeRoot == nil || retainedProjectRoot == nil {
		return nil, errors.New("retained threadpoint home and project roots are required")
	}
	return commit(ctx, opts, retainedHomeRoot, retainedProjectRoot)
}

func commit(ctx context.Context, opts CommitOptions, retainedHomeRoot, retainedProjectRoot *os.Root) (*CommitReport, error) {
	if strings.TrimSpace(opts.ID) == "" {
		return nil, errors.New("stage id is required")
	}
	root, home, err := normalizeOptions(opts.Root, opts.ThreadpointHome)
	if err != nil {
		return nil, err
	}
	projectRoot := retainedProjectRoot
	if projectRoot == nil {
		projectRoot, err = openMutationRoot(root)
		if err != nil {
			return nil, err
		}
		defer projectRoot.Close()
	} else if opened, openErr := projectRoot.Stat("."); openErr != nil || !opened.IsDir() {
		return nil, errors.Join(errors.New("retained project root is not a directory"), openErr)
	}
	projectGuard, err := retainSelectedRoot(root, projectRoot)
	if err != nil {
		return nil, err
	}
	defer projectGuard.Close()
	if err := projectGuard.Verify(); err != nil {
		return nil, err
	}
	stage, err := openStageForMutationRoot(root, home, opts.ID, retainedHomeRoot)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	record := stage.record
	dir := stage.dir
	if record.Root != root || record.ProjectID != projectID(root) || record.Dir != dir {
		return nil, errors.New("staged item does not belong to this project root")
	}
	target, err := TargetFor(record.Scope, record.Provider, record.Source, record.Kind)
	if err != nil || target != record.Target {
		return nil, errors.New("staged item scope or target was tampered with")
	}
	// Pin the exact stage generation: the review copy read here and the stage
	// removed below both resolve beneath one pinned directory identity, so a
	// stage directory replaced (with a symlink or another generation) after the
	// manifest load cannot substitute the committed bytes or misdirect the
	// removal.
	if err := verifyPinnedStageGeneration(stage.stageRoot, stage.info, record); err != nil {
		return nil, err
	}
	body, err := backup.ReadRootRegularFileBounded(stage.stageRoot, ContentFile, maxStageBytes)
	if err != nil {
		return nil, err
	}
	if digest(body) != record.ContentSHA256 {
		return nil, errors.New("stage content generation does not match its manifest")
	}
	if err := verifyLockedStageHome(stage); err != nil {
		return nil, err
	}
	if err := projectGuard.Verify(); err != nil {
		return nil, err
	}
	recoveredTransaction, err := recoverCommitBackupTransaction(record, root, home, stage.homeRoot, projectRoot, body)
	if err != nil {
		return nil, err
	}
	if recoveredTransaction != nil {
		report := &CommitReport{
			SchemaVersion: "threadpoint.commit.v1", ID: record.ID, Scope: record.Scope,
			Target: record.Target, BackupRun: "commit-" + record.ID,
			GitNextStep: "review the project diff, then run git add and git commit yourself",
		}
		if commitBeforeStageCleanupValidation != nil {
			commitBeforeStageCleanupValidation()
		}
		if err := errors.Join(projectGuard.Verify(), verifyLockedStageHome(stage)); err != nil {
			rollbackErr := recoveredTransaction.rollbackCommitted(projectRoot)
			return nil, errors.Join(fmt.Errorf("recovered committed %s but retained its stage after a selected logical root changed: %w", record.ID, err), rollbackErr)
		}
		if err := cleanupCommittedStage(stage, func() error {
			return errors.Join(projectGuard.Verify(), verifyLockedStageHome(stage))
		}, func() error {
			return recoveredTransaction.rollbackCommitted(projectRoot)
		}); err != nil {
			return nil, fmt.Errorf("recovered committed %s but could not complete reversible stage cleanup: %w", record.ID, err)
		}
		return report, nil
	}
	if err := projectGuard.Verify(); err != nil {
		return nil, err
	}

	// Read and pin the provider source identity from a single no-follow bounded
	// open, and re-verify identity and bytes immediately before publication and
	// before backup, so a source changed after review is refused rather than
	// committed stale or snapshotted with a replacement file's mode.
	relSource := filepath.FromSlash(record.Source)
	sourceBody, sourceInfo, err := backup.ReadRootRegularFileBoundedInfo(projectRoot, relSource, maxStageBytes)
	if err != nil {
		return nil, fmt.Errorf("read staged source %s: %w", record.Source, err)
	}
	if digest(sourceBody) != record.SourceSHA256 {
		return nil, fmt.Errorf("staged source %s changed; create a new stage before committing", record.Source)
	}
	sourceBody, sourceInfo, err = revalidateSource(projectRoot, relSource, record, sourceInfo)
	if err != nil {
		return nil, err
	}
	if _, _, err := revalidateSource(projectRoot, relSource, record, sourceInfo); err != nil {
		return nil, err
	}
	backupTransaction := newCommitBackupTransaction(record, root, home, stage.homeRoot, sourceBody, sourceInfo.Mode())
	if err := verifyLockedStageHome(stage); err != nil {
		return nil, err
	}
	report, err := commitReviewed(ctx, CommitInput{
		Root: root, Scope: record.Scope, Provider: record.Provider, Source: record.Source, Kind: record.Kind, ID: record.ID, Content: body,
		ValidateBeforePublish: func() error {
			if err := projectGuard.Verify(); err != nil {
				return err
			}
			_, _, err := revalidateSource(projectRoot, relSource, record, sourceInfo)
			return err
		},
		ValidateAfterPublish: func() error {
			if commitBeforePostPublishValidation != nil {
				commitBeforePostPublishValidation()
			}
			if err := projectGuard.Verify(); err != nil {
				return err
			}
			_, _, err := revalidateSource(projectRoot, relSource, record, sourceInfo)
			return errors.Join(err, projectGuard.Verify())
		},
		PrepareCanonicalTransaction: backupTransaction.prepare,
		FinalizeTransaction:         backupTransaction.finalize,
		AbortTransaction:            backupTransaction.abort,
	}, projectRoot)
	if err != nil {
		return nil, err
	}
	report.BackupRun = backupTransaction.runID()
	if commitBeforeStageCleanupValidation != nil {
		commitBeforeStageCleanupValidation()
	}
	if err := errors.Join(projectGuard.Verify(), verifyLockedStageHome(stage)); err != nil {
		rollbackErr := backupTransaction.rollbackCommitted(projectRoot)
		return nil, errors.Join(fmt.Errorf("committed %s but retained its stage after a selected logical root changed: %w", record.ID, err), rollbackErr)
	}
	if err := cleanupCommittedStage(stage, func() error {
		return errors.Join(projectGuard.Verify(), verifyLockedStageHome(stage))
	}, func() error {
		return backupTransaction.rollbackCommitted(projectRoot)
	}); err != nil {
		return nil, fmt.Errorf("committed %s but could not complete reversible stage cleanup: %w", record.ID, err)
	}
	return report, nil
}

func cleanupCommittedStage(stage *openedStage, verifySelectedRoots, rollbackCanonical func() error) error {
	if stage == nil || stage.scopeRoot == nil || stage.info == nil {
		return errors.New("committed stage cleanup requires a retained generation")
	}
	base := filepath.Base(stage.dir)
	quarantine, err := detachStageGenerationForCommit(stage.scopeRoot, base, stage.info)
	if err != nil {
		return fmt.Errorf("detach reviewed stage before final root validation: %w", err)
	}
	if commitStageCleanupAfterDetach != nil {
		commitStageCleanupAfterDetach()
	}
	if err := verifySelectedRoots(); err != nil {
		restored, restoreErr := restoreDetachedStageGeneration(stage.scopeRoot, quarantine, base, stage.info)
		if !restored && restoreErr == nil {
			restoreErr = errors.New("detached reviewed stage was not restored")
		}
		rollbackErr := rollbackCanonical()
		return errors.Join(fmt.Errorf("selected logical root changed after reviewed stage detachment: %w", err), restoreErr, rollbackErr)
	}
	if err := removeDetachedStageGenerationFromParent(stage.scopeRoot, quarantine, stage.info); err != nil {
		return fmt.Errorf("delete validated detached stage: %w", err)
	}
	return nil
}

// verifyPinnedStageGeneration confirms the pinned stage root is still the exact
// reviewed directory and that its manifest still matches the loaded record.
func verifyPinnedStageGeneration(stageRoot *os.Root, expected fs.FileInfo, record Record) error {
	opened, err := stageRoot.Stat(".")
	if err != nil || !opened.IsDir() || opened.Mode().Perm() != 0o700 || !os.SameFile(expected, opened) {
		return errors.Join(errors.New("stage directory changed after it was loaded"), err)
	}
	current, err := readStageRecordRoot(stageRoot)
	if err != nil {
		return err
	}
	if current != record {
		return errors.New("stage manifest changed after it was loaded")
	}
	return nil
}

// revalidateSource re-reads the provider source at the publication boundary and
// returns its current bytes and mode only when the identity and digest still
// match; otherwise it refuses the commit.
func revalidateSource(projectRoot *os.Root, relSource string, record Record, expected fs.FileInfo) ([]byte, fs.FileInfo, error) {
	body, info, err := backup.ReadRootRegularFileBoundedInfo(projectRoot, relSource, maxStageBytes)
	if err != nil || expected == nil || info == nil || !os.SameFile(expected, info) ||
		expected.Mode().Perm() != info.Mode().Perm() || digest(body) != record.SourceSHA256 {
		return nil, nil, fmt.Errorf("staged source %s changed; create a new stage before committing", record.Source)
	}
	return body, info, nil
}

// openMutationRoot resolves and pins the project root for mutation, following
// benign ancestor symlinks while rejecting in-root symlink escapes.
func openMutationRoot(root string) (*os.Root, error) {
	// #nosec G703 -- root is a caller-selected root whose identity is captured
	// here and compared with both the resolved handle and pathname before use.
	selected, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !selected.IsDir() {
		return nil, fmt.Errorf("mutation root is not a directory: %s", root)
	}
	physical, err := safefs.ResolveRootForMutation(root)
	if err != nil {
		return nil, err
	}
	if openMutationRootAfterResolve != nil {
		openMutationRootAfterResolve(root)
	}
	openedRoot, err := os.OpenRoot(physical)
	if err != nil {
		return nil, err
	}
	opened, openedErr := openedRoot.Stat(".")
	// #nosec G703 -- this second ambient lookup is only the other side of the
	// identity comparison; no descendant access is authorized by it.
	current, currentErr := os.Stat(root)
	if openedErr != nil || currentErr != nil || !opened.IsDir() || !current.IsDir() || !os.SameFile(selected, opened) || !os.SameFile(selected, current) {
		return nil, errors.Join(errors.New("mutation root changed identity while it was selected"), openedErr, currentErr, openedRoot.Close())
	}
	return openedRoot, nil
}

func verifyMutationRootPath(root *os.Root) error {
	if root == nil {
		return errors.New("mutation root is required")
	}
	current, currentErr := os.Lstat(root.Name())
	opened, openedErr := root.Stat(".")
	if currentErr != nil || openedErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !opened.IsDir() || !os.SameFile(current, opened) {
		return errors.Join(errors.New("mutation root path no longer identifies the retained project generation"), currentErr, openedErr)
	}
	return nil
}

// removeStageGeneration removes only the exact reviewed stage directory by
// detaching it into a uniquely owned quarantine under its parent, verifying the
// detached inode identity, then removing it; a swapped stage directory fails
// closed and is restored.
func removeStageGeneration(dir string, expected fs.FileInfo) (returnErr error) {
	parentPath := filepath.Dir(dir)
	base := filepath.Base(dir)
	if err := safefs.RejectExistingSymlinkAncestors(parentPath); err != nil {
		return err
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	return removeStageGenerationFromParent(parent, base, expected)
}

func removeStageGenerationFromParent(parent *os.Root, base string, expected fs.FileInfo) (returnErr error) {
	budget := &stageEnumerationBudget{}
	return removeStageGenerationFromParentWithBudget(parent, base, expected, budget)
}

func removeStageGenerationFromParentWithBudget(parent *os.Root, base string, expected fs.FileInfo, budget *stageEnumerationBudget) (returnErr error) {
	if parent == nil {
		return errors.New("stage parent root is required")
	}
	if budget == nil {
		return errors.New("stage cleanup enumeration budget is required")
	}
	var quarantine string
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		candidate := "." + base + ".remove-" + hex.EncodeToString(token[:])
		if _, err := parent.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			quarantine = candidate
			break
		} else if err != nil {
			return err
		}
	}
	if quarantine == "" {
		return errors.New("could not allocate a unique stage removal quarantine name")
	}
	if err := detachStageGenerationTo(parent, base, quarantine, expected); err != nil {
		return err
	}
	return removeDetachedStageGenerationFromParentWithBudget(parent, quarantine, expected, budget)
}

func detachStageGenerationForCommit(parent *os.Root, base string, expected fs.FileInfo) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		id := newID()
		quarantine := "." + base + ".old-" + strings.TrimPrefix(id, "stage-")
		if _, err := parent.Lstat(quarantine); errors.Is(err, os.ErrNotExist) {
			if err := detachStageGenerationTo(parent, base, quarantine, expected); err != nil {
				return "", err
			}
			if err := syncOpenedRoot(parent); err != nil {
				_, restoreErr := restoreDetachedStageGeneration(parent, quarantine, base, expected)
				return "", errors.Join(fmt.Errorf("sync committed-stage detachment: %w", err), restoreErr)
			}
			return quarantine, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique committed-stage quarantine name")
}

func detachStageGenerationTo(parent *os.Root, base, quarantine string, expected fs.FileInfo) error {
	if parent == nil {
		return errors.New("stage parent root is required")
	}
	current, err := parent.Lstat(base)
	if err != nil || !current.IsDir() || !os.SameFile(expected, current) {
		return errors.Join(errors.New("stage directory changed before detachment"), err)
	}
	if !safefs.RenameRootNoReplaceSupported() {
		return safefs.ErrRenameNoReplaceUnsupported
	}
	if _, err := parent.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("stage detachment quarantine already exists: %s", quarantine)
		}
		return err
	}
	if err := safefs.RenameRootNoReplace(parent, base, quarantine); err != nil {
		return err
	}
	detached, err := parent.Lstat(quarantine)
	if err != nil || !detached.IsDir() || !os.SameFile(expected, detached) {
		return errors.Join(errors.New("stage directory changed during detachment"), err)
	}
	return nil
}

func removeDetachedStageGenerationFromParent(parent *os.Root, quarantine string, expected fs.FileInfo) (returnErr error) {
	budget := &stageEnumerationBudget{}
	return removeDetachedStageGenerationFromParentWithBudget(parent, quarantine, expected, budget)
}

func removeDetachedStageGenerationFromParentWithBudget(parent *os.Root, quarantine string, expected fs.FileInfo, budget *stageEnumerationBudget) (returnErr error) {
	if budget == nil {
		return errors.New("stage cleanup enumeration budget is required")
	}
	detachedRoot, err := parent.OpenRoot(quarantine)
	if err != nil {
		return err
	}
	opened, err := detachedRoot.Stat(".")
	if err != nil || !os.SameFile(expected, opened) {
		return errors.Join(errors.New("detached stage generation changed before cleanup"), err, detachedRoot.Close())
	}
	// Recursive cleanup stays rooted in the retained detached generation. It
	// never reopens the quarantine pathname, so replacing that ambient name
	// cannot redirect deletion into another tree.
	cleanupErr := removePinnedRootContentsWithBudget(detachedRoot, quarantine, 0, budget)
	closeErr := detachedRoot.Close()
	if cleanupErr != nil || closeErr != nil {
		return errors.Join(cleanupErr, closeErr)
	}
	current, err := parent.Lstat(quarantine)
	if err != nil || !os.SameFile(expected, current) {
		return errors.Join(errors.New("detached stage generation changed after cleanup"), err)
	}
	if err := parent.Remove(quarantine); err != nil {
		return err
	}
	return syncOpenedRoot(parent)
}

func syncOpenedRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func removePinnedRootContents(root *os.Root) error {
	return removePinnedRootContentsWithBudget(root, ".", 0, &stageEnumerationBudget{})
}

func removePinnedRootContentsWithBudget(root *os.Root, walkPath string, depth int, budget *stageEnumerationBudget) error {
	entries, err := readStageDirectoryEntries(root, walkPath, depth, budget)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		var retainedChild *os.Root
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			child, err := root.OpenRoot(name)
			if err != nil {
				return err
			}
			opened, statErr := child.Stat(".")
			if statErr != nil || !safefs.SameFileGeneration(info, opened) {
				return errors.Join(errors.New("detached stage child changed before cleanup"), statErr, child.Close())
			}
			cleanupErr := removePinnedRootContentsWithBudget(child, filepath.Join(walkPath, name), depth+1, budget)
			if cleanupErr != nil {
				return errors.Join(cleanupErr, child.Close())
			}
			current, err := root.Lstat(name)
			if err != nil || !os.SameFile(info, current) {
				return errors.Join(errors.New("detached stage child changed after cleanup"), err, child.Close())
			}
			// Keep the child descriptor open through the unlink. Its retained
			// inode cannot be recycled into a same-device/same-inode replacement
			// between this final pathname check and Remove.
			retainedChild = child
		} else {
			if removePinnedRootBeforeEntryRemove != nil {
				removePinnedRootBeforeEntryRemove(root, name)
			}
			current, err := root.Lstat(name)
			if err != nil || !safefs.SameFileGeneration(info, current) {
				return errors.Join(errors.New("detached stage entry changed before cleanup"), err)
			}
		}
		if err := root.Remove(name); err != nil {
			if retainedChild != nil {
				return errors.Join(err, retainedChild.Close())
			}
			return err
		}
		if retainedChild != nil {
			if err := retainedChild.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// CommitReviewed writes scope-safe canonical project content from reviewed
// bytes. It never performs git add or git commit.
func CommitReviewed(ctx context.Context, input CommitInput) (*CommitReport, error) {
	return commitReviewed(ctx, input, nil)
}

// CommitReviewedFromRoot writes reviewed content through a caller-retained
// project root. retainedRoot is borrowed and is not closed by this function;
// all canonical reads and mutation remain beneath that exact filesystem
// generation.
func CommitReviewedFromRoot(ctx context.Context, input CommitInput, retainedRoot *os.Root) (*CommitReport, error) {
	if retainedRoot == nil {
		return nil, errors.New("retained project root is required")
	}
	return commitReviewed(ctx, input, retainedRoot)
}

func commitReviewed(ctx context.Context, input CommitInput, retainedRoot *os.Root) (returnReport *CommitReport, returnErr error) {
	root, _, err := normalizeOptions(input.Root, filepath.Join(os.TempDir(), "threadpoint-commit-home"))
	if err != nil {
		return nil, err
	}
	target, normalized, err := NormalizeReviewedContent(input)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Pin the canonical root so every read, temporary file, and rename resolves
	// beneath the same directory identity even if an ancestor pathname is
	// replaced after this transaction begins.
	pinnedRoot := retainedRoot
	if pinnedRoot == nil {
		pinnedRoot, err = openMutationRoot(root)
		if err != nil {
			return nil, err
		}
		defer pinnedRoot.Close()
	}
	rootGuard, err := retainSelectedRoot(root, pinnedRoot)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, rootGuard.Close()) }()
	if err := rootGuard.Verify(); err != nil {
		return nil, err
	}
	if err := validateTargetGitFromRoot(pinnedRoot, target, input.Scope); err != nil {
		return nil, err
	}
	if err := rootGuard.Verify(); err != nil {
		return nil, err
	}

	// Capture the exact current canonical content (and its mode) beneath the
	// pinned root. The atomic write below republishes only if this identity is
	// still intact at the mutation boundary, so a concurrent edit or a paid-lane
	// commit cannot be silently overwritten.
	relTarget := filepath.FromSlash(target)
	current, currentInfo, currentExists, err := readCanonicalGenerationUnderRoot(pinnedRoot, relTarget, target)
	if err != nil {
		return nil, err
	}
	report := &CommitReport{SchemaVersion: "threadpoint.commit.v1", ID: input.ID, Scope: input.Scope, Target: target, GitNextStep: "review the project diff, then run git add and git commit yourself"}
	transactionStarted := false
	defer func() {
		if returnErr != nil && transactionStarted && input.AbortTransaction != nil {
			returnErr = errors.Join(returnErr, input.AbortTransaction())
		}
	}()
	postPublish := func(proposed []byte) error {
		if err := rootGuard.Verify(); err != nil {
			return err
		}
		if input.ValidateAfterPublish != nil {
			if err := input.ValidateAfterPublish(); err != nil {
				return err
			}
		} else if input.ValidateBeforePublish != nil {
			if err := input.ValidateBeforePublish(); err != nil {
				return err
			}
		}
		if input.FinalizeTransaction != nil {
			if err := input.FinalizeTransaction(target, proposed); err != nil {
				return err
			}
		}
		return rootGuard.Verify()
	}
	prepare := func(proposed []byte) error {
		if input.PrepareCanonicalTransaction != nil {
			transactionStarted = true
			snapshot := CanonicalTransactionSnapshot{
				Target: target, Proposed: append([]byte(nil), proposed...),
				PriorExists: currentExists, PriorBody: append([]byte(nil), current...),
			}
			if currentInfo != nil {
				snapshot.PriorMode = currentInfo.Mode().Perm()
			}
			return input.PrepareCanonicalTransaction(snapshot)
		}
		if input.PrepareTransaction != nil {
			transactionStarted = true
			return input.PrepareTransaction(target, proposed)
		}
		return nil
	}
	marker := []byte(`threadpoint:committed id="` + safeID(input.ID) + `"`)
	if currentExists && bytes.Contains(current, marker) {
		return nil, fmt.Errorf("canonical target %s already contains an unbound marker for stage %s; reconcile the collision before committing", target, input.ID)
	}
	proposed := normalized
	if len(current) > 0 {
		proposed = append(append(append([]byte{}, current...), '\n'), normalized...)
	}
	if err := validateCanonicalGenerationSize(target, proposed); err != nil {
		return nil, err
	}
	if err := prepare(proposed); err != nil {
		return nil, err
	}
	validate := func(parent *os.Root, base string) error {
		if err := rootGuard.Verify(); err != nil {
			return err
		}
		if input.ValidateBeforePublish != nil {
			if err := input.ValidateBeforePublish(); err != nil {
				return err
			}
		}
		latest, _, latestExists, err := readCanonicalGenerationUnderRoot(parent, base, target)
		if err != nil {
			return err
		}
		if latestExists != currentExists || !bytes.Equal(latest, current) {
			return fmt.Errorf("canonical target %s changed during commit; re-run commit against the current content", target)
		}
		return rootGuard.Verify()
	}
	// Non-secret canonical content keeps umask semantics (and preserves an
	// existing target's mode); the validate callback makes the publish fail
	// closed on a concurrent change.
	validateAfter := func(*os.Root, string) error { return postPublish(proposed) }
	if err := safefs.AtomicWriteRootFileUsingUmaskTransaction(pinnedRoot, target, proposed, 0o666, 0o777, validate, validateAfter); err != nil {
		return nil, err
	}
	return report, nil
}

// readCanonicalUnderRoot returns the current canonical bytes for name beneath a
// pinned root and whether it exists. A missing target reports (nil, false, nil);
// a non-regular target is rejected. name is the root-relative OS path; label is
// the slash target used only for error messages.
func readCanonicalGenerationUnderRoot(root *os.Root, name, label string) ([]byte, fs.FileInfo, bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, false, fmt.Errorf("refusing to replace non-regular canonical target %s", label)
	}
	body, opened, err := backup.ReadRootRegularFileBoundedInfo(root, name, maxCanonicalBytes)
	if err != nil {
		return nil, nil, false, err
	}
	if !os.SameFile(info, opened) {
		return nil, nil, false, errors.New("canonical target changed while its transaction snapshot was captured")
	}
	return body, opened, true, nil
}

func validateCanonicalGenerationSize(target string, body []byte) error {
	if len(body) > maxCanonicalBytes {
		return fmt.Errorf("canonical generation %s exceeds the %d-byte limit", target, maxCanonicalBytes)
	}
	return nil
}

func normalizeOptions(root, home string) (string, string, error) {
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	absRoot, err := abspath.Abs(root)
	if err != nil {
		return "", "", err
	}
	absRoot = filepath.Clean(absRoot)
	if info, err := os.Stat(absRoot); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("project root is not a directory: %s", absRoot)
	}
	if strings.TrimSpace(home) == "" {
		return "", "", errors.New("threadpoint home is required")
	}
	absHome, err := abspath.Abs(home)
	if err != nil {
		return "", "", err
	}
	if filepath.Clean(absHome) == string(filepath.Separator) {
		return "", "", errors.New("threadpoint home cannot be filesystem root")
	}
	return absRoot, filepath.Clean(absHome), nil
}

func normalizedSource(root, source string) (string, error) {
	if strings.TrimSpace(source) == "" {
		return "", errors.New("stage source is required")
	}
	if filepath.IsAbs(source) {
		rel, err := filepath.Rel(root, source)
		if err != nil {
			return "", err
		}
		source = rel
	}
	source = filepath.ToSlash(filepath.Clean(source))
	if err := validateRelative(source); err != nil {
		return "", err
	}
	return source, nil
}

func validateRelative(path string) error {
	if path == "." || path == "" || strings.HasPrefix(path, "../") || path == ".." || filepath.IsAbs(path) {
		return fmt.Errorf("path must be relative to the project root: %s", path)
	}
	return nil
}

func classify(root, source string, required Scope, explicit map[string]Scope) (Scope, error) {
	return classifyWithGitRoot(root, nil, source, required, explicit)
}

func classifyFromRoot(root *os.Root, source string, required Scope, explicit map[string]Scope) (Scope, error) {
	if root == nil {
		return "", errors.New("retained project root is required for Git classification")
	}
	return classifyWithGitRoot("", root, source, required, explicit)
}

func classifyWithGitRoot(root string, retainedRoot *os.Root, source string, required Scope, explicit map[string]Scope) (Scope, error) {
	if required != "" {
		if err := validateScope(required); err != nil {
			return "", err
		}
		if requested, ok := explicit[source]; ok && requested != required {
			return "", fmt.Errorf("%w for %s: declared %s scope conflicts with --classify %s", ErrClassificationRequired, source, required, requested)
		}
		return required, nil
	}
	tracked, err := gitQuery(root, retainedRoot, "ls-files", "--error-unmatch", "--", source)
	if err != nil {
		return "", err
	}
	if tracked {
		return ScopeProjectShared, nil
	}
	ignored, err := gitQuery(root, retainedRoot, "check-ignore", "-q", "--", source)
	if err != nil {
		return "", err
	}
	if ignored {
		return ScopeProjectLocal, nil
	}
	if scope, ok := explicit[source]; ok {
		if err := validateScope(scope); err != nil {
			return "", err
		}
		return scope, nil
	}
	return "", fmt.Errorf("%w for %s; pass --classify %s=project-shared or %s=project-local", ErrClassificationRequired, source, source, source)
}

func validateScope(scope Scope) error {
	switch scope {
	case ScopeProjectShared, ScopeProjectLocal:
		return nil
	default:
		return fmt.Errorf("unknown project scope %q", scope)
	}
}

func validateTargetGitFromRoot(root *os.Root, target string, scope Scope) error {
	if root == nil {
		return errors.New("retained project root is required for Git target validation")
	}
	return validateTargetGitWithRoot("", root, target, scope)
}

func validateTargetGitWithRoot(root string, retainedRoot *os.Root, target string, scope Scope) error {
	tracked, err := gitQuery(root, retainedRoot, "ls-files", "--error-unmatch", "--", target)
	if err != nil {
		return err
	}
	ignored, err := gitQuery(root, retainedRoot, "check-ignore", "-q", "--", target)
	if err != nil {
		return err
	}
	switch scope {
	case ScopeProjectShared:
		if ignored {
			return fmt.Errorf("project-shared target %s is ignored by git", target)
		}
	case ScopeProjectLocal:
		if tracked || !ignored {
			return fmt.Errorf("project-local target %s must be ignored and untracked by git", target)
		}
	default:
		return validateScope(scope)
	}
	return nil
}

func gitQuery(root string, retainedRoot *os.Root, args ...string) (matched bool, returnErr error) {
	program := "git"
	commandArgs := args
	var directory *os.File
	if retainedRoot == nil {
		if strings.TrimSpace(root) == "" {
			return false, errors.New("git worktree root is required")
		}
	} else {
		var err error
		directory, err = retainedRoot.Open(".")
		if err != nil {
			return false, err
		}
		defer func() { returnErr = errors.Join(returnErr, directory.Close()) }()
		if gitRootOpened != nil {
			gitRootOpened(directory.Fd())
		}
	}
	if gitRootBeforeRun != nil {
		gitRootBeforeRun()
	}
	if gitRootAfterRun != nil {
		defer gitRootAfterRun()
	}
	if directory != nil {
		// ExtraFiles remaps the retained directory to descriptor 3 in the child,
		// which enters it before running Git so repository discovery keeps its
		// ordinary semantics. Build the wrapper as late as possible: on darwin it
		// carries the directory's current path, which the child re-verifies.
		script, scriptArgs, err := descriptorBoundGitScript(runtime.GOOS, func() (string, string, error) {
			return descriptorLocation(directory)
		})
		if err != nil {
			return false, err
		}
		program = "sh"
		commandArgs = append(append([]string{"-c", script, "threadpoint-git"}, scriptArgs...), args...)
	}
	// #nosec G204 -- the program and shell script are fixed; args are internal, caller-controlled subcommands.
	command := exec.CommandContext(context.Background(), program, commandArgs...)
	if directory == nil {
		command.Dir = root
	} else {
		command.ExtraFiles = []*os.File{directory}
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Env = gitQueryEnvironment(command.Environ())
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && directory != nil && exitErr.ExitCode() == gitWorktreeEntryFailed {
		return false, fmt.Errorf("descriptor-bound Git query could not enter the retained worktree: %s", strings.TrimSpace(stderr.String()))
	}
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 && gitMissingRepository(stderr.String()) {
		return false, nil
	}
	return false, fmt.Errorf("run descriptor-bound Git query: %w", err)
}

func gitQueryEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if key == "LC_ALL" {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "LC_ALL=C")
}

func gitMissingRepository(stderr string) bool {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	if firstLine == "fatal: not a git repository (or any of the parent directories): .git" {
		return true
	}
	return strings.HasPrefix(firstLine, "fatal: not a git repository (or any parent up to mount point ") && strings.HasSuffix(firstLine, ")")
}

// gitWorktreeEntryFailed is the wrapper's exit status when the Git child cannot
// enter the retained worktree. It must differ from Git's "no match" status 1,
// or an unenterable worktree would silently read as an unmatched path.
const gitWorktreeEntryFailed = 125

// descriptorBoundGitScript returns the shell wrapper, and its leading arguments,
// that makes the Git child enter the retained directory inherited as descriptor
// 3. Darwin's descriptor entries can be neither entered nor used to compare
// identity, so there the child enters the directory's current path and checks
// its working directory against the device:inode read from the retained
// descriptor. Once entered, the working directory is pinned to that inode.
func descriptorBoundGitScript(goos string, locate func() (path string, identity string, err error)) (string, []string, error) {
	descriptorPath, err := descriptorBoundGitDirectory(goos, 3)
	if err != nil {
		return "", nil, err
	}
	if goos != "darwin" {
		return fmt.Sprintf(`cd "$1" || exit %d; shift; exec git "$@"`, gitWorktreeEntryFailed), []string{descriptorPath}, nil
	}
	path, identity, err := locate()
	if err != nil {
		return "", nil, fmt.Errorf("locate retained Git worktree: %w", err)
	}
	return fmt.Sprintf(`cd "$1" && [ "$(stat -f '%%d:%%i' .)" = "$2" ] || exit %d; shift 2; exec git "$@"`, gitWorktreeEntryFailed), []string{path, identity}, nil
}

func descriptorBoundGitDirectory(goos string, descriptor uintptr) (string, error) {
	switch goos {
	case "linux", "android":
		return fmt.Sprintf("/proc/self/fd/%d", descriptor), nil
	case "darwin", "dragonfly", "freebsd", "netbsd", "openbsd":
		return fmt.Sprintf("/dev/fd/%d", descriptor), nil
	default:
		// In particular, Windows has no equivalent path that Git can use as
		// Cmd.Dir. Reopening the ambient worktree would violate the caller's
		// retained-generation guarantee, so unsupported systems fail closed.
		return "", fmt.Errorf("descriptor-bound Git worktree queries are unavailable on %s", goos)
	}
}

func projectID(root string) string { return shortID(root) }

func shortID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func newID() string {
	var raw [10]byte
	_, _ = rand.Read(raw[:])
	return "stage-" + hex.EncodeToString(raw[:])
}

func safeID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "reviewed"
	}
	return safepath.SanitizeSegment(id)
}

func stageDir(home, projectID string, scope Scope, id string) string {
	return filepath.Join(home, "imports", projectID, string(scope), id)
}

func writeStage(dir string, record Record, body []byte) (publishedInfo fs.FileInfo, returnErr error) {
	if err := safefs.RejectExistingSymlinkAncestors(dir); err != nil {
		return nil, err
	}
	parentPath := filepath.Dir(dir)
	if err := os.MkdirAll(parentPath, 0o700); err != nil {
		return nil, err
	}
	// #nosec G302 -- 0o700 keeps the private state directory owner-only yet traversable.
	if err := os.Chmod(parentPath, 0o700); err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	return writeStageToParent(parent, filepath.Base(dir), record, body, func() error { return syncStageParent(parentPath) })
}

func writeStageFromHomeRoot(homeRoot *os.Root, record Record, body []byte) (publishedInfo fs.FileInfo, returnErr error) {
	if homeRoot == nil {
		return nil, errors.New("retained threadpoint home root is required")
	}
	if err := verifyMutationRootPath(homeRoot); err != nil {
		return nil, err
	}
	parentRel := filepath.Join("imports", record.ProjectID, string(record.Scope))
	parent, err := openOrCreateRootDirectory(homeRoot, parentRel, 0o700)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	if err := verifyMutationRootPath(homeRoot); err != nil {
		return nil, err
	}
	return writeStageToParent(parent, record.ID, record, body, func() error { return syncOpenedRoot(parent) })
}

func writeStageToParent(parent *os.Root, base string, record Record, body []byte, syncParent func() error) (publishedInfo fs.FileInfo, returnErr error) {
	if parent == nil {
		return nil, errors.New("stage parent root is required")
	}
	if syncParent == nil {
		return nil, errors.New("stage parent sync is required")
	}
	if _, err := parent.Lstat(base); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrAlreadyStaged, record.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	tmpID := newID()
	tmpName := "." + base + ".tmp-" + strings.TrimPrefix(tmpID, "stage-")
	if err := parent.Mkdir(tmpName, 0o700); err != nil {
		return nil, err
	}
	tmpInfo, err := parent.Lstat(tmpName)
	if err != nil || !tmpInfo.IsDir() || tmpInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Join(errors.New("stage temporary generation identity is unavailable"), err)
	}
	published := false
	defer func() {
		if !published {
			returnErr = errors.Join(returnErr, removeStageGenerationFromParent(parent, tmpName, tmpInfo))
		}
	}()
	tmpRoot, err := parent.OpenRoot(tmpName)
	if err != nil {
		return nil, err
	}
	openedTmp, err := tmpRoot.Stat(".")
	if err != nil || !openedTmp.IsDir() || !os.SameFile(tmpInfo, openedTmp) {
		return nil, errors.Join(errors.New("stage temporary generation changed while it was opened"), err, tmpRoot.Close())
	}
	if stageTempAfterCreate != nil {
		if err := stageTempAfterCreate(parent, tmpName, tmpInfo); err != nil {
			return nil, errors.Join(err, tmpRoot.Close())
		}
	}
	if err := validateStageRecord(record); err != nil {
		return nil, errors.Join(err, tmpRoot.Close())
	}
	manifest, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, errors.Join(err, tmpRoot.Close())
	}
	for _, item := range []struct {
		name string
		body []byte
	}{{SourceFile, body}, {ContentFile, body}, {ManifestFile, manifest}} {
		if err := atomicWriteRootStageFile(tmpRoot, item.name, item.body, 0o600, nil); err != nil {
			return nil, errors.Join(err, tmpRoot.Close())
		}
	}
	if err := tmpRoot.Close(); err != nil {
		return nil, err
	}
	if err := syncParent(); err != nil {
		return nil, err
	}
	if err := safefs.RenameRootNoReplace(parent, tmpName, base); err != nil {
		return nil, err
	}
	published = true
	publishedInfo, err = parent.Lstat(base)
	if err != nil || !publishedInfo.IsDir() {
		return publishedInfo, errors.Join(errors.New("published stage identity is unavailable"), err)
	}
	if err := syncParent(); err != nil {
		return publishedInfo, err
	}
	return publishedInfo, nil
}

func openOrCreateRootDirectory(root *os.Root, relative string, mode os.FileMode) (returnRoot *os.Root, returnErr error) {
	if root == nil {
		return nil, errors.New("root is required")
	}
	clean := filepath.Clean(relative)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("invalid rooted directory path: %s", relative)
	}
	components := strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' })
	current := root
	currentOwned := false
	defer func() {
		if returnErr != nil && currentOwned {
			returnErr = errors.Join(returnErr, current.Close())
		}
	}()
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("invalid rooted directory component: %s", component)
		}
		info, err := current.Lstat(component)
		if errors.Is(err, os.ErrNotExist) {
			if err := current.Mkdir(component, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return nil, err
			}
			if err := syncOpenedRoot(current); err != nil {
				return nil, err
			}
			info, err = current.Lstat(component)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(fmt.Errorf("rooted stage directory is not a physical directory: %s", component), err)
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			return nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
			return nil, errors.Join(errors.New("rooted stage directory changed while it was opened"), err, next.Close())
		}
		if currentOwned {
			if err := current.Close(); err != nil {
				return nil, errors.Join(err, next.Close())
			}
		}
		current = next
		currentOwned = true
	}
	directory, err := current.Open(".")
	if err != nil {
		return nil, err
	}
	chmodErr := directory.Chmod(mode)
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(chmodErr, syncErr, closeErr); err != nil {
		return nil, err
	}
	return current, nil
}

func syncStageDirectory(path string) error {
	// #nosec G304 -- path is the validated private stage parent selected by the caller.
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func listStages(home, wantedRoot, wantedProjectID string) (records []Record, returnErr error) {
	info, err := os.Lstat(home)
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil || !info.IsDir() {
		return nil, errors.Join(errors.New("threadpoint home is not a directory"), err)
	}
	homeRoot, err := openMutationRoot(home)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, homeRoot.Close()) }()
	homeGuard, err := retainSelectedRoot(home, homeRoot)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, homeGuard.Close()) }()
	records, err = listStagesFromHomeRoot(homeRoot, home, wantedRoot, wantedProjectID)
	if err != nil {
		return nil, err
	}
	if err := homeGuard.Verify(); err != nil {
		return nil, err
	}
	return records, nil
}

type stageEnumerationBudget struct {
	entries       int
	pathBytes     int64
	manifestBytes int64
}

func (budget *stageEnumerationBudget) chargePath(path string) error {
	if budget == nil {
		return errors.New("stage enumeration budget is required")
	}
	pathBytes := int64(len(filepath.ToSlash(path)))
	if pathBytes > maxEnumeratedStagePathBytes-budget.pathBytes {
		return fmt.Errorf("stage enumeration exceeds the %d-byte path budget", maxEnumeratedStagePathBytes)
	}
	budget.pathBytes += pathBytes
	return nil
}

func readStageDirectoryEntries(root *os.Root, walkPath string, depth int, budget *stageEnumerationBudget) ([]fs.DirEntry, error) {
	if root == nil || budget == nil {
		return nil, errors.New("stage root and enumeration budget are required")
	}
	if depth > maxEnumeratedStageDepth {
		return nil, fmt.Errorf("stage enumeration exceeds maximum depth %d", maxEnumeratedStageDepth)
	}
	remaining := maxEnumeratedStageEntries - budget.entries
	if remaining < 0 {
		return nil, fmt.Errorf("stage enumeration exceeds %d entries", maxEnumeratedStageEntries)
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(remaining + 1)
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(entries) > remaining {
		return nil, fmt.Errorf("stage enumeration exceeds %d entries", maxEnumeratedStageEntries)
	}
	budget.entries += len(entries)
	for _, entry := range entries {
		if err := budget.chargePath(filepath.Join(walkPath, entry.Name())); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func listStagesFromHomeRoot(homeRoot *os.Root, home, wantedRoot, wantedProjectID string) (returnRecords []Record, returnErr error) {
	if homeRoot == nil {
		return nil, errors.New("retained threadpoint home root is required")
	}
	if err := verifyMutationRootPath(homeRoot); err != nil {
		return nil, err
	}
	budget := stageEnumerationBudget{}
	for _, scope := range []Scope{ScopeProjectShared, ScopeProjectLocal} {
		scopeRel := filepath.Join("imports", wantedProjectID, string(scope))
		if err := budget.chargePath(scopeRel); err != nil {
			return nil, err
		}
		if err := safefs.RejectRootSymlinkAncestors(homeRoot, scopeRel); err != nil {
			return nil, err
		}
		scopeInfo, err := homeRoot.Lstat(scopeRel)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !scopeInfo.IsDir() || scopeInfo.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(errors.New("stage scope root is not a physical directory"), err)
		}
		scopeRoot, err := homeRoot.OpenRoot(scopeRel)
		if err != nil {
			return nil, err
		}
		if err := safefs.VerifyPinnedRootPath(homeRoot, scopeRel, scopeRoot); err != nil {
			return nil, errors.Join(err, scopeRoot.Close())
		}
		records, err := listStagesInScopeRoot(scopeRoot, home, wantedRoot, wantedProjectID, scope, &budget)
		closeErr := scopeRoot.Close()
		if err != nil || closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		returnRecords = append(returnRecords, records...)
	}
	sort.Slice(returnRecords, func(i, j int) bool {
		if returnRecords[i].Scope != returnRecords[j].Scope {
			return returnRecords[i].Scope < returnRecords[j].Scope
		}
		return returnRecords[i].ID < returnRecords[j].ID
	})
	return returnRecords, nil
}

func listStagesInScopeRoot(scopeRoot *os.Root, home, wantedRoot, wantedProjectID string, scope Scope, budget *stageEnumerationBudget) (records []Record, returnErr error) {
	if scopeRoot == nil || budget == nil {
		return nil, errors.New("stage scope root and enumeration budget are required")
	}
	remaining := maxEnumeratedStageEntries - budget.entries
	if remaining < 0 {
		return nil, fmt.Errorf("stage enumeration exceeds %d entries", maxEnumeratedStageEntries)
	}
	directory, err := scopeRoot.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(remaining + 1)
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(entries) > remaining {
		return nil, fmt.Errorf("stage enumeration exceeds %d entries", maxEnumeratedStageEntries)
	}
	budget.entries += len(entries)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		id := entry.Name()
		if err := budget.chargePath(filepath.Join(id, ManifestFile)); err != nil {
			return nil, err
		}
		if !validStageID(id) {
			continue
		}
		info, err := scopeRoot.Lstat(id)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(errors.New("canonical stage generation is not a physical directory"), err)
		}
		stageRoot, err := scopeRoot.OpenRoot(id)
		if err != nil {
			return nil, err
		}
		opened, statErr := stageRoot.Stat(".")
		if statErr != nil || !opened.IsDir() || !os.SameFile(info, opened) {
			return nil, errors.Join(errors.New("stage generation changed while it was opened"), statErr, stageRoot.Close())
		}
		manifestInfo, manifestErr := stageRoot.Lstat(ManifestFile)
		if manifestErr != nil || !manifestInfo.Mode().IsRegular() {
			return nil, errors.Join(errors.New("canonical stage generation has no regular manifest"), manifestErr, stageRoot.Close())
		}
		if manifestInfo.Size() < 0 || manifestInfo.Size() > maxStageBytes || manifestInfo.Size() > maxEnumeratedStageManifestBytes-budget.manifestBytes {
			return nil, errors.Join(fmt.Errorf("stage manifest bytes exceed the %d-byte aggregate budget", maxEnumeratedStageManifestBytes), stageRoot.Close())
		}
		if stageEnumerationAfterManifestStat != nil {
			if err := stageEnumerationAfterManifestStat(stageRoot); err != nil {
				return nil, errors.Join(err, stageRoot.Close())
			}
		}
		record, manifestBytes, openedManifest, readErr := readStageRecordRootGeneration(stageRoot)
		closeErr := stageRoot.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}
		if !os.SameFile(manifestInfo, openedManifest) {
			return nil, errors.New("stage manifest changed between enumeration and its bounded read")
		}
		if manifestBytes > maxEnumeratedStageManifestBytes-budget.manifestBytes {
			return nil, fmt.Errorf("stage manifest bytes exceed the %d-byte aggregate budget", maxEnumeratedStageManifestBytes)
		}
		budget.manifestBytes += manifestBytes
		dir := stageDir(home, wantedProjectID, scope, id)
		if record.ID != id || record.Scope != scope || record.ProjectID != wantedProjectID || record.Root != wantedRoot || record.Dir != dir {
			return nil, errors.New("staged item manifest does not match its canonical immutable location")
		}
		records = append(records, record)
	}
	return records, nil
}

func recoverStageTransactions(parent *os.Root, base string) error {
	if parent == nil {
		return errors.New("stage scope root is required")
	}
	budget := &stageEnumerationBudget{}
	entries, err := readStageDirectoryEntries(parent, ".", 0, budget)
	if err != nil {
		return err
	}
	var oldNames []string
	var nextNames []string
	for _, entry := range entries {
		if stageTransactionName(entry.Name(), "."+base+".old-") {
			oldNames = append(oldNames, entry.Name())
		}
		if stageTransactionName(entry.Name(), "."+base+".next-") {
			nextNames = append(nextNames, entry.Name())
		}
	}
	if len(oldNames) == 0 && len(nextNames) == 0 {
		return nil
	}
	sort.Strings(oldNames)
	sort.Strings(nextNames)
	stable, stableErr := parent.Lstat(base)
	switch {
	case stableErr == nil:
		if !stable.IsDir() || stable.Mode()&os.ModeSymlink != 0 {
			return errors.New("stable stage generation is not a physical directory")
		}
	case errors.Is(stableErr, os.ErrNotExist):
		if len(oldNames) > 1 {
			return fmt.Errorf("cannot recover stage %s: multiple prior generations exist", base)
		}
		if len(oldNames) == 1 {
			oldInfo, err := parent.Lstat(oldNames[0])
			if err != nil || !oldInfo.IsDir() || oldInfo.Mode()&os.ModeSymlink != 0 {
				return errors.Join(errors.New("prior stage transaction generation is invalid"), err)
			}
			if err := safefs.RenameRootNoReplace(parent, oldNames[0], base); err != nil {
				return err
			}
			restored, err := parent.Lstat(base)
			if err != nil || !restored.IsDir() || !os.SameFile(oldInfo, restored) {
				return errors.Join(errors.New("restored stage generation changed during recovery"), err)
			}
			if err := syncOpenedRoot(parent); err != nil {
				return err
			}
		}
	case stableErr != nil:
		return stableErr
	}
	for _, name := range append(oldNames, nextNames...) {
		info, err := parent.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(fmt.Errorf("stage transaction %s is not a physical directory", name), err)
		}
		if err := removeStageGenerationFromParentWithBudget(parent, name, info, budget); err != nil {
			return err
		}
	}
	return nil
}

func stageTransactionName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if len(suffix) != 20 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func findBySource(home, wantedRoot, projectID, source string) (*Record, error) {
	records, err := listStages(home, wantedRoot, projectID)
	if err != nil {
		return nil, err
	}
	for index := range records {
		if records[index].Source == source {
			return &records[index], nil
		}
	}
	return nil, nil //nolint:nilnil // (nil, nil) is the deliberate "no matching stage" result; callers branch on a nil record.
}

func findBySourceFromRoot(homeRoot *os.Root, home, wantedRoot, wantedProjectID, source string) (returnRecord *Record, returnErr error) {
	records, err := listStagesFromHomeRoot(homeRoot, home, wantedRoot, wantedProjectID)
	if err != nil {
		return nil, err
	}
	for index := range records {
		if records[index].Source == source {
			return &records[index], nil
		}
	}
	return nil, nil //nolint:nilnil // absence is the expected result when no stage matches the source.
}

func removeStageGenerationFromHomeRoot(homeRoot *os.Root, record Record, expected fs.FileInfo) (returnErr error) {
	if err := verifyMutationRootPath(homeRoot); err != nil {
		return err
	}
	if !strings.HasPrefix(record.ID, "stage-") || strings.ContainsAny(record.ID, "/\\") {
		return errors.New("invalid stage id")
	}
	if err := validateScope(record.Scope); err != nil {
		return err
	}
	scopeRel := filepath.Join("imports", record.ProjectID, string(record.Scope))
	if err := safefs.RejectRootSymlinkAncestors(homeRoot, scopeRel); err != nil {
		return err
	}
	scopeInfo, err := homeRoot.Lstat(scopeRel)
	if err != nil || !scopeInfo.IsDir() || scopeInfo.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New("stage scope root is not a physical directory"), err)
	}
	scopeRoot, err := homeRoot.OpenRoot(scopeRel)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, scopeRoot.Close()) }()
	if err := safefs.VerifyPinnedRootPath(homeRoot, scopeRel, scopeRoot); err != nil {
		return err
	}
	return removeStageGenerationFromParent(scopeRoot, record.ID, expected)
}

func openStage(root, home, id string) (*openedStage, error) {
	return openStageForRootMode(root, home, id, nil, false)
}

func openStageForRoot(root, home, id string, retainedHomeRoot *os.Root) (*openedStage, error) {
	return openStageForRootMode(root, home, id, retainedHomeRoot, false)
}

// openStageForMutationRoot reconciles an interrupted generation only for a
// writer path. CLI callers hold the project lock for the complete mutation;
// observational reads deliberately use openStageForRoot and never recover.
func openStageForMutationRoot(root, home, id string, retainedHomeRoot *os.Root) (*openedStage, error) {
	return openStageForRootMode(root, home, id, retainedHomeRoot, true)
}

func openStageForRootMode(root, home, id string, retainedHomeRoot *os.Root, allowRecovery bool) (*openedStage, error) {
	if retainedHomeRoot == nil {
		root, home, err := normalizeOptions(root, home)
		if err != nil {
			return nil, err
		}
		homeRoot, err := openMutationRoot(home)
		if err != nil {
			return nil, err
		}
		homeGuard, err := retainSelectedRoot(home, homeRoot)
		if err != nil {
			return nil, errors.Join(err, homeRoot.Close())
		}
		stage, err := openStageUnderRootMode(root, home, id, homeRoot, true, allowRecovery)
		if err != nil {
			return nil, errors.Join(err, homeGuard.Close(), homeRoot.Close())
		}
		stage.homeGuard = homeGuard
		return stage, nil
	}
	root, home, err := normalizeOptions(root, home)
	if err != nil {
		return nil, err
	}
	if err := verifyMutationRootPath(retainedHomeRoot); err != nil {
		return nil, err
	}
	homeGuard, err := retainSelectedRoot(home, retainedHomeRoot)
	if err != nil {
		return nil, err
	}
	stage, err := openStageUnderRootMode(root, home, id, retainedHomeRoot, false, allowRecovery)
	if err != nil {
		return nil, errors.Join(err, homeGuard.Close())
	}
	stage.homeGuard = homeGuard
	return stage, nil
}

func openStageUnderRoot(root, home, id string, homeRoot *os.Root, ownsHomeRoot bool) (*openedStage, error) {
	return openStageUnderRootMode(root, home, id, homeRoot, ownsHomeRoot, false)
}

func openStageUnderRootMode(root, home, id string, homeRoot *os.Root, ownsHomeRoot, allowRecovery bool) (*openedStage, error) {
	if !strings.HasPrefix(id, "stage-") || strings.ContainsAny(id, "/\\") {
		return nil, errors.New("invalid stage id")
	}
	project := projectID(root)
	for _, scope := range []Scope{ScopeProjectShared, ScopeProjectLocal} {
		scopeRel := filepath.Join("imports", project, string(scope))
		if err := safefs.RejectRootSymlinkAncestors(homeRoot, scopeRel); err != nil {
			return nil, err
		}
		scopeInfo, err := homeRoot.Lstat(scopeRel)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !scopeInfo.IsDir() || scopeInfo.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(errors.New("stage scope root is not a physical directory"), err)
		}
		scopeRoot, err := homeRoot.OpenRoot(scopeRel)
		if err != nil {
			return nil, err
		}
		if err := safefs.VerifyPinnedRootPath(homeRoot, scopeRel, scopeRoot); err != nil {
			_ = scopeRoot.Close()
			return nil, err
		}
		if allowRecovery {
			if err := recoverStageTransactions(scopeRoot, id); err != nil {
				_ = scopeRoot.Close()
				return nil, err
			}
		}
		stageInfo, err := scopeRoot.Lstat(id)
		if errors.Is(err, os.ErrNotExist) {
			_ = scopeRoot.Close()
			continue
		}
		if err != nil || !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
			_ = scopeRoot.Close()
			return nil, errors.Join(errors.New("stage generation is not a physical directory"), err)
		}
		stageRoot, err := scopeRoot.OpenRoot(id)
		if err != nil {
			_ = scopeRoot.Close()
			return nil, err
		}
		opened, err := stageRoot.Stat(".")
		if err != nil || !opened.IsDir() || !os.SameFile(stageInfo, opened) {
			_ = stageRoot.Close()
			_ = scopeRoot.Close()
			return nil, errors.Join(errors.New("stage generation changed while it was opened"), err)
		}
		record, err := readStageRecordRoot(stageRoot)
		if err != nil {
			_ = stageRoot.Close()
			_ = scopeRoot.Close()
			return nil, err
		}
		dir := stageDir(home, project, scope, id)
		if record.ID != id || record.Scope != scope || record.ProjectID != project || record.Dir != dir {
			_ = stageRoot.Close()
			_ = scopeRoot.Close()
			return nil, errors.New("staged item manifest does not match its immutable location")
		}
		stage := &openedStage{record: record, dir: dir, info: stageInfo, homeRoot: homeRoot, ownsHomeRoot: ownsHomeRoot, scopeRoot: scopeRoot, stageRoot: stageRoot}
		if stageAfterOpen != nil {
			stageAfterOpen()
		}
		return stage, nil
	}
	return nil, fmt.Errorf("staged item %s was not found", id)
}

func readStageRecordRoot(root *os.Root) (Record, error) {
	record, _, _, err := readStageRecordRootGeneration(root)
	return record, err
}

func readStageRecordRootGeneration(root *os.Root) (Record, int64, fs.FileInfo, error) {
	body, info, err := backup.ReadRootRegularFileBoundedInfo(root, ManifestFile, maxStageBytes)
	if err != nil {
		return Record{}, 0, nil, err
	}
	var record Record
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Record{}, 0, nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Record{}, 0, nil, errors.New("stage manifest contains multiple JSON values")
		}
		return Record{}, 0, nil, err
	}
	if err := validateStageRecord(record); err != nil {
		return Record{}, 0, nil, err
	}
	return record, int64(len(body)), info, nil
}

func validateStageRecord(record Record) error {
	if record.SchemaVersion == "" {
		return errors.New("stage manifest requires schemaVersion")
	}
	if record.SchemaVersion != "threadpoint.stage-record.v1" {
		return errors.New("unsupported stage manifest")
	}
	if !validStageID(record.ID) {
		return errors.New("stage manifest requires a valid id")
	}
	if !validLowerHex(record.ProjectID, 16) {
		return errors.New("stage manifest requires a valid projectId")
	}
	if record.Root == "" || !filepath.IsAbs(record.Root) || filepath.Clean(record.Root) != record.Root || projectID(record.Root) != record.ProjectID {
		return errors.New("stage manifest requires a valid root bound to projectId")
	}
	if err := validateScope(record.Scope); err != nil {
		return fmt.Errorf("stage manifest scope: %w", err)
	}
	if strings.TrimSpace(record.Provider) == "" {
		return errors.New("stage manifest requires provider")
	}
	if err := validateRelative(record.Source); err != nil || filepath.ToSlash(filepath.Clean(record.Source)) != record.Source {
		return errors.Join(errors.New("stage manifest requires a canonical source"), err)
	}
	if err := validateKind(record.Kind); err != nil {
		return fmt.Errorf("stage manifest kind: %w", err)
	}
	wantTarget, err := TargetFor(record.Scope, record.Provider, record.Source, record.Kind)
	if err != nil || record.Target != wantTarget {
		return errors.Join(errors.New("stage manifest requires the derived target"), err)
	}
	if !validLowerHex(record.SourceSHA256, sha256.Size*2) {
		return errors.New("stage manifest requires a valid sourceSha256")
	}
	if !validLowerHex(record.ContentSHA256, sha256.Size*2) {
		return errors.New("stage manifest requires a valid contentSha256")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.CreatedAt); err != nil || !strings.HasSuffix(record.CreatedAt, "Z") {
		return errors.Join(errors.New("stage manifest requires a valid UTC createdAt"), err)
	}
	if record.Dir == "" || !filepath.IsAbs(record.Dir) || filepath.Clean(record.Dir) != record.Dir {
		return errors.New("stage manifest requires a valid dir")
	}
	return nil
}

func validateKind(kind Kind) error {
	switch kind {
	case KindInstruction, KindKnowledge, KindRule, KindSkill, KindPrompt, KindCommand, KindAgent:
		return nil
	default:
		return fmt.Errorf("unknown artifact kind %q", kind)
	}
}

func validStageID(id string) bool {
	return strings.HasPrefix(id, "stage-") && validLowerHex(strings.TrimPrefix(id, "stage-"), 20)
}

func validLowerHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// readRegular pins the parent and validates one no-follow, nonblocking open so
// a final-component race cannot redirect, hang, or bypass the size bound.
func readRegular(path string) ([]byte, error) {
	absolute, err := abspath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return backup.ReadRootRegularFileBounded(parent, filepath.Base(absolute), maxStageBytes)
}
