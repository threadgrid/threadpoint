// SPDX-License-Identifier: Apache-2.0

// Package safefs provides filesystem-safety helpers shared across the mutating
// subsystems: path containment, atomic writes, advisory locks, and resolution
// of the threadpoint home directory.
package safefs

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
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/threadgrid/threadpoint/internal/abspath"
	"github.com/threadgrid/threadpoint/internal/home"
)

var (
	// ErrLockHeld marks an existing threadpoint mutation lock.
	ErrLockHeld = errors.New("threadpoint lock is held")
	// ErrUnsafeSymlink marks an attempted write through an existing symlink.
	ErrUnsafeSymlink = errors.New("unsafe symlink path")
	// ErrRenameNoReplaceUnsupported marks a platform, kernel, or filesystem
	// without an atomic rooted rename operation that refuses replacement.
	ErrRenameNoReplaceUnsupported = errors.New("atomic rooted no-replace rename is unsupported")
	// ErrRenameExchangeUnsupported marks a platform, kernel, or filesystem
	// without an atomic rooted exchange operation.
	ErrRenameExchangeUnsupported      = errors.New("atomic rooted exchange rename is unsupported")
	linkFile                          = os.Link
	probeRenameRootNoReplaceOperation = RenameRootNoReplace
	atomicCopyBeforeSourceOpen        func(string)
	atomicTempBeforePublish           func(string)
	atomicRootTempBeforePublish       func(*os.Root, string)
	atomicRootBeforePublishRename     func(*os.Root, string)
	atomicRootAfterPublishExchange    func(*os.Root, string, string) error
	atomicRootAfterPublishDetach      func(*os.Root, string, string) error
	exchangeRootOperation             = ExchangeRoot
	writeFileIfMissingAfterCreate     func(string, *os.File) error
)

// lockStaleAfter bounds how long a lock with a still-live owner PID is trusted.
// Beyond it the PID is assumed to have been reused and the lock is reclaimable.
const (
	DefaultFileLockStaleAfter    = 24 * time.Hour
	DefaultFileLockMetadataGrace = 30 * time.Second
	lockStaleAfter               = DefaultFileLockStaleAfter
	lockMetadataGrace            = DefaultFileLockMetadataGrace
	defaultFileLockPoll          = 100 * time.Millisecond
	rootPublishScanBatch         = 128
	rootPublishScanMaxEntries    = 4096
	rootPublishScanMaxNameBytes  = 1 << 20
)

// LockSet owns one or more threadpoint mutation locks.
type LockSet struct {
	locks         []heldLock
	productRoot   *os.Root
	productParent *os.Root
	productEntry  fs.FileInfo
	productPath   string
	productName   string
	locksRoot     *os.Root
	projectRoots  map[string]*retainedProjectRoot
	projectPins   []*retainedProjectRoot
}

type retainedProjectRoot struct {
	root          *os.Root
	logicalParent *os.Root
	logicalEntry  fs.FileInfo
	logicalPath   string
	logicalName   string
	physicalPath  string
}

// BorrowProductRoot returns the exact product-home root retained by this lock
// set. The returned root is borrowed: callers must not close it or use it after
// Release. Resolving private state beneath this capability keeps the data
// transaction in the same filesystem generation as its cooperative locks;
// borrowing fails once the ambient product-home name no longer identifies that
// generation.
func (set *LockSet) BorrowProductRoot() (*os.Root, error) {
	if set == nil || set.productRoot == nil {
		return nil, errors.New("lock set does not retain a product-home root")
	}
	if err := set.verifyProductRootPath(); err != nil {
		return nil, err
	}
	return set.productRoot, nil
}

// BorrowProjectRoot returns the exact selected project generation associated
// with this lock set. The returned root is borrowed and remains owned by the
// LockSet. Both the caller's logical final component and its resolved physical
// target are revalidated on every borrow, so an explicitly selected symlink
// cannot be retargeted after lock acquisition without invalidating mutation.
func (set *LockSet) BorrowProjectRoot(selectedPath string) (*os.Root, error) {
	if set == nil {
		return nil, errors.New("project root is not locked")
	}
	logicalPath, err := abspath.Abs(strings.TrimSpace(selectedPath))
	if err != nil || strings.TrimSpace(selectedPath) == "" {
		return nil, errors.Join(errors.New("project root is not locked"), err)
	}
	retained := set.projectRoots[filepath.Clean(logicalPath)]
	if retained == nil {
		return nil, fmt.Errorf("project root is not locked: %s", filepath.Clean(logicalPath))
	}
	if err := retained.verify(); err != nil {
		return nil, err
	}
	return retained.root, nil
}

func (retained *retainedProjectRoot) verify() error {
	if retained == nil || retained.root == nil || retained.logicalParent == nil || retained.logicalEntry == nil || retained.logicalPath == "" || retained.logicalName == "" || retained.physicalPath == "" {
		return errors.New("project lock does not retain a complete locked generation")
	}
	physical, resolveErr := ResolveRootForMutation(retained.logicalPath)
	logicalEntry, entryErr := retained.logicalParent.Lstat(retained.logicalName)
	logicalTarget, targetErr := os.Stat(retained.logicalPath)
	opened, openedErr := retained.root.Stat(".")
	if resolveErr != nil || filepath.Clean(physical) != retained.physicalPath || entryErr != nil || targetErr != nil || openedErr != nil || !os.SameFile(retained.logicalEntry, logicalEntry) || !logicalTarget.IsDir() || !opened.IsDir() || !os.SameFile(logicalTarget, opened) {
		return errors.Join(errors.New("selected project root no longer identifies the locked generation"), resolveErr, entryErr, targetErr, openedErr)
	}
	return nil
}

// verifyProductRootPath binds the retained descriptor to the exact final
// component beneath its retained parent. Checking only productRoot.Stat(".")
// would keep succeeding after the complete product-home directory had been
// renamed away and replaced, allowing a caller to mutate detached state while
// a second process acquired locks in the replacement namespace.
func (set *LockSet) verifyProductRootPath() error {
	if set == nil || set.productRoot == nil || set.productParent == nil || set.productEntry == nil || strings.TrimSpace(set.productPath) == "" || strings.TrimSpace(set.productName) == "" {
		return errors.New("lock set does not retain a complete product-home identity")
	}
	currentEntry, entryErr := set.productParent.Lstat(set.productName)
	currentTarget, targetErr := os.Stat(set.productPath)
	opened, openedErr := set.productRoot.Stat(".")
	if entryErr != nil || targetErr != nil || openedErr != nil || !os.SameFile(set.productEntry, currentEntry) || !currentTarget.IsDir() || !opened.IsDir() || !os.SameFile(currentTarget, opened) {
		return errors.Join(errors.New("threadpoint product home no longer identifies the locked generation"), entryErr, targetErr, openedErr)
	}
	return nil
}

type heldLock struct {
	lock *FileLock
}

// FileLockStalePolicy decides whether an existing lock record can be reclaimed.
type FileLockStalePolicy func(snapshot FileLockSnapshot, now time.Time, staleAfter time.Duration) (bool, error)

// FileLockOptions configures a reusable cooperative file lock.
type FileLockOptions struct {
	Path           string
	Body           []byte
	DirMode        os.FileMode
	FileMode       os.FileMode
	EnforceDirMode bool
	WaitTimeout    time.Duration
	PollInterval   time.Duration
	StaleAfter     time.Duration
	Now            func() time.Time
	StalePolicy    FileLockStalePolicy
	ReleaseMatches func(body []byte) bool
}

// FileLockSnapshot describes an existing lock record for stale-policy checks.
type FileLockSnapshot struct {
	Path    string
	Body    []byte
	ModTime time.Time
}

// FileLock owns a reusable cooperative file lock.
type FileLock struct {
	path           string
	root           *os.Root
	name           string
	releaseMatches func([]byte) bool
	released       bool
}

const (
	productLocksRootIdentityName = ".threadpoint-locks-root.identity"
	locksRootIdentityName        = ".threadpoint-root.identity"
)

type locksRootIdentity struct {
	Version int    `json:"version"`
	Token   string `json:"token"`
}

// FileLockErrorReason classifies reusable file-lock failures.
type FileLockErrorReason string

// FileLockErrorHeld reports an active lock; FileLockErrorUnsafe reports a lock
// path or operation that cannot be trusted safely.
const (
	FileLockErrorHeld   FileLockErrorReason = "held"
	FileLockErrorUnsafe FileLockErrorReason = "unsafe"
)

// FileLockError reports reusable file-lock failures with machine-readable
// classification and the current lock body when available.
type FileLockError struct {
	Reason  FileLockErrorReason
	Path    string
	Body    []byte
	Message string
	Err     error
}

type lockOwner struct {
	Command string    `json:"command"`
	PID     int       `json:"pid"`
	Root    string    `json:"root"`
	Created time.Time `json:"created"`
	Token   string    `json:"token,omitempty"`
}

type commonLockOwner struct {
	PID       int       `json:"pid"`
	Created   time.Time `json:"created"`
	CreatedAt string    `json:"created_at"`
}

// Error is part of the safefs public locking API.
func (err FileLockError) Error() string {
	if strings.TrimSpace(err.Message) != "" {
		return err.Message
	}
	switch err.Reason {
	case FileLockErrorUnsafe:
		return "file lock path is unsafe"
	default:
		return "file lock is already held"
	}
}

// Unwrap exposes sentinel lock errors for errors.Is callers.
func (err FileLockError) Unwrap() error {
	if err.Err != nil {
		return err.Err
	}
	switch err.Reason {
	case FileLockErrorUnsafe:
		return ErrUnsafeSymlink
	default:
		return ErrLockHeld
	}
}

// AcquireFileLock creates a cooperative lock file atomically, reclaiming stale
// locks according to the configured policy before reporting contention.
func AcquireFileLock(ctx context.Context, opts FileLockOptions) (*FileLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := normalizeFileLockOptions(opts)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(normalized.WaitTimeout)
	for {
		lock, err := tryAcquireFileLock(normalized)
		if err == nil {
			return lock, nil
		}
		var lockErr FileLockError
		if !errors.As(err, &lockErr) || lockErr.Reason != FileLockErrorHeld {
			return nil, err
		}
		if normalized.WaitTimeout <= 0 || !time.Now().Before(deadline) {
			return nil, lockErr
		}
		timer := time.NewTimer(normalized.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Release removes only the reviewed lock generation while its owner predicate
// still matches, so it cannot delete a concurrent replacement.
func (lock *FileLock) Release() (returnErr error) {
	if lock == nil || lock.released {
		return nil
	}
	if lock.root != nil {
		return releaseFileLockFromRoot(lock, lock.root, lock.name)
	}
	if err := RejectExistingSymlink(filepath.Dir(lock.path)); err != nil {
		return FileLockError{Reason: FileLockErrorUnsafe, Path: lock.path, Message: err.Error(), Err: err}
	}
	parent, err := os.OpenRoot(filepath.Dir(lock.path))
	if errors.Is(err, os.ErrNotExist) {
		lock.released = true
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	return releaseFileLockFromRoot(lock, parent, filepath.Base(lock.path))
}

func releaseFileLockFromRoot(lock *FileLock, parent *os.Root, base string) (returnErr error) {
	if lock == nil || lock.released {
		return nil
	}
	if parent == nil {
		return errors.New("file lock root is required")
	}
	entry, err := reviewFileLockEntry(parent, base)
	if errors.Is(err, os.ErrNotExist) {
		lock.released = true
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, entry.Close()) }()

	if lock.releaseMatches != nil && !lock.releaseMatches(entry.body) {
		return FileLockError{Reason: FileLockErrorHeld, Path: lock.path, Body: append([]byte(nil), entry.body...), Message: "refusing to release file lock owned by another process"}
	}
	if err := removeReviewedFileLock(parent, base, entry); err != nil {
		if errors.Is(err, errFileLockChangedBeforeCleanup) || errors.Is(err, errFileLockChangedDuringCleanup) {
			// Another process reclaimed or replaced our lock; the object we held
			// is already gone, so treat the release as complete rather than
			// deleting the replacement.
			lock.released = true
			return nil
		}
		return err
	}
	lock.released = true
	return nil
}

func normalizeFileLockOptions(opts FileLockOptions) (FileLockOptions, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return FileLockOptions{}, errors.New("file lock path is required")
	}
	opts.Path = filepath.Clean(opts.Path)
	if opts.DirMode == 0 {
		opts.DirMode = 0o755
	}
	if opts.FileMode == 0 {
		opts.FileMode = 0o644
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultFileLockPoll
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = DefaultFileLockStaleAfter
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.StalePolicy == nil {
		opts.StalePolicy = DefaultFileLockStalePolicy
	}
	return opts, nil
}

func tryAcquireFileLock(opts FileLockOptions) (*FileLock, error) {
	dir := filepath.Dir(opts.Path)
	if err := RejectExistingSymlink(dir); err != nil {
		return nil, FileLockError{Reason: FileLockErrorUnsafe, Path: opts.Path, Message: err.Error(), Err: err}
	}
	if err := RejectExistingSymlink(opts.Path); err != nil {
		return nil, FileLockError{Reason: FileLockErrorUnsafe, Path: opts.Path, Message: err.Error(), Err: err}
	}
	// #nosec G301 -- callers choose whether lock directories are public
	// coordination metadata or private state through DirMode.
	if err := os.MkdirAll(dir, opts.DirMode); err != nil {
		return nil, err
	}
	if opts.EnforceDirMode {
		// #nosec G302 -- mode is caller-supplied and used for private state locks.
		if err := os.Chmod(dir, opts.DirMode); err != nil {
			return nil, err
		}
	}
	lock, err := createFileLock(opts)
	if err == nil {
		return lock, nil
	}
	if !os.IsExist(err) {
		return nil, err
	}
	if err := reclaimStaleFileLock(opts); err != nil {
		return nil, err
	}
	return tryAcquireFileLock(opts)
}

// AcquireFileLockRoot acquires one final-component lock entry beneath an
// already-pinned directory. Unlike AcquireFileLock, no pathname parent is
// reopened while waiting, reclaiming, or releasing the lock. The caller keeps
// root open until the returned lock has been released and must not close the
// borrowed root through the lock.
func AcquireFileLockRoot(ctx context.Context, root *os.Root, name string, opts FileLockOptions) (*FileLock, error) {
	return acquireFileLockRoot(ctx, root, name, opts)
}

func acquireFileLockRoot(ctx context.Context, root *os.Root, name string, opts FileLockOptions) (*FileLock, error) {
	if root == nil {
		return nil, errors.New("file lock root is required")
	}
	base, err := cleanPinnedRenameBase(name)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := normalizeFileLockOptions(opts)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(normalized.WaitTimeout)
	for {
		lock, err := tryAcquireFileLockRoot(root, base, normalized)
		if err == nil {
			return lock, nil
		}
		var lockErr FileLockError
		if !errors.As(err, &lockErr) || lockErr.Reason != FileLockErrorHeld {
			return nil, err
		}
		if normalized.WaitTimeout <= 0 || !time.Now().Before(deadline) {
			return nil, lockErr
		}
		timer := time.NewTimer(normalized.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func tryAcquireFileLockRoot(root *os.Root, name string, opts FileLockOptions) (*FileLock, error) {
	if info, err := root.Lstat(name); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			unsafeErr := fmt.Errorf("file lock must not be a symlink: %s: %w", opts.Path, ErrUnsafeSymlink)
			return nil, FileLockError{Reason: FileLockErrorUnsafe, Path: opts.Path, Message: unsafeErr.Error(), Err: unsafeErr}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := createRootExclusiveFile(root, name, opts.Body, opts.FileMode); err == nil {
		return &FileLock{path: opts.Path, root: root, name: name, releaseMatches: opts.ReleaseMatches}, nil
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := reclaimStaleFileLockRoot(root, name, opts); err != nil {
		return nil, err
	}
	return tryAcquireFileLockRoot(root, name, opts)
}

func reclaimStaleFileLockRoot(parent *os.Root, base string, opts FileLockOptions) (returnErr error) {
	entry, err := reviewFileLockEntry(parent, base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, entry.Close()) }()

	snapshot := FileLockSnapshot{
		Path:    opts.Path,
		Body:    append([]byte(nil), entry.body...),
		ModTime: entry.info.ModTime(),
	}
	stale, err := opts.StalePolicy(snapshot, opts.Now().UTC(), opts.StaleAfter)
	if err != nil {
		return err
	}
	if !stale {
		return FileLockError{Reason: FileLockErrorHeld, Path: opts.Path, Body: snapshot.Body}
	}
	if err := removeReviewedFileLock(parent, base, entry); err != nil {
		if errors.Is(err, os.ErrNotExist) ||
			errors.Is(err, errFileLockChangedBeforeCleanup) ||
			errors.Is(err, errFileLockChangedDuringCleanup) {
			return nil
		}
		return err
	}
	return nil
}

// reclaimStaleFileLock pins and conditionally removes only the reviewed lock
// generation. Binding the removal to the reviewed object closes the race where
// another writer reclaims the stale lock and installs a fresh live lock between
// the staleness decision and the removal; a blind rename+delete would destroy
// that new live lock. It returns nil when acquisition should retry, a held
// FileLockError for a live lock, or the underlying failure.
func reclaimStaleFileLock(opts FileLockOptions) (returnErr error) {
	parent, err := os.OpenRoot(filepath.Dir(opts.Path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()

	base := filepath.Base(opts.Path)
	entry, err := reviewFileLockEntry(parent, base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, entry.Close()) }()

	snapshot := FileLockSnapshot{
		Path:    opts.Path,
		Body:    append([]byte(nil), entry.body...),
		ModTime: entry.info.ModTime(),
	}
	stale, err := opts.StalePolicy(snapshot, opts.Now().UTC(), opts.StaleAfter)
	if err != nil {
		return err
	}
	if !stale {
		return FileLockError{Reason: FileLockErrorHeld, Path: opts.Path, Body: snapshot.Body}
	}
	if err := removeReviewedFileLock(parent, base, entry); err != nil {
		if errors.Is(err, os.ErrNotExist) ||
			errors.Is(err, errFileLockChangedBeforeCleanup) ||
			errors.Is(err, errFileLockChangedDuringCleanup) {
			// Another writer reclaimed or replaced the lock; retry acquisition so
			// the current lock is re-reviewed instead of blindly removed.
			return nil
		}
		return err
	}
	return nil
}

func createFileLock(opts FileLockOptions) (*FileLock, error) {
	// #nosec G302 G304 -- lock path and mode are caller-selected lock API inputs.
	file, err := os.OpenFile(opts.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, opts.FileMode)
	if err != nil {
		if os.IsExist(err) {
			return nil, os.ErrExist
		}
		if _, statErr := os.Lstat(opts.Path); statErr == nil {
			return nil, os.ErrExist
		}
		return nil, err
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			_ = os.Remove(opts.Path)
		}
	}()
	if err := file.Chmod(opts.FileMode); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if _, err := file.Write(opts.Body); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	removeOnError = false
	return &FileLock{path: opts.Path, releaseMatches: opts.ReleaseMatches}, nil
}

// createRootExclusiveFile durably creates one private regular file beneath a
// retained root. Failure cleanup is identity-bound so it cannot remove a
// replacement installed after the create.
func createRootExclusiveFile(root *os.Root, name string, body []byte, mode os.FileMode) error {
	if root == nil {
		return errors.New("root is required")
	}
	if !RenameRootNoReplaceSupported() {
		return ErrRenameNoReplaceUnsupported
	}
	base, err := cleanPinnedRenameBase(name)
	if err != nil {
		return err
	}
	tempName, err := rootExclusiveTempName(root, base)
	if err != nil {
		return err
	}
	// #nosec G302 -- the caller selects the lock/identity mode and both current
	// callers pass 0600. The private temporary is published with an atomic
	// destination-no-replace rename only after its body is durable.
	file, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		if _, statErr := root.Lstat(tempName); statErr == nil {
			return os.ErrExist
		}
		return err
	}
	created, err := file.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return failCreatedRootExclusiveFile(root, tempName, file, created, errors.Join(errors.New("created rooted file is not regular"), err))
	}
	if err := file.Chmod(mode); err != nil {
		return failCreatedRootExclusiveFile(root, tempName, file, created, err)
	}
	if _, err := file.Write(body); err != nil {
		return failCreatedRootExclusiveFile(root, tempName, file, created, err)
	}
	if err := file.Sync(); err != nil {
		return failCreatedRootExclusiveFile(root, tempName, file, created, err)
	}
	opened, openedErr := file.Stat()
	current, currentErr := root.Lstat(tempName)
	if openedErr != nil || currentErr != nil || !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(created, opened) || !os.SameFile(opened, current) || opened.Size() != int64(len(body)) || current.Size() != int64(len(body)) {
		return failCreatedRootExclusiveFile(root, tempName, file, created, errors.Join(errors.New("created rooted file changed before completion"), openedErr, currentErr))
	}
	if err := syncFileLockParent(root); err != nil {
		return failCreatedRootExclusiveFile(root, tempName, file, created, err)
	}
	if err := RenameRootNoReplace(root, tempName, base); err != nil {
		cleanupErr := cleanupCreatedRootExclusiveFile(root, tempName, created)
		closeErr := file.Close()
		if errors.Is(err, os.ErrExist) {
			return errors.Join(os.ErrExist, cleanupErr, closeErr)
		}
		return errors.Join(err, cleanupErr, closeErr)
	}
	published, publishedErr := root.Lstat(base)
	if publishedErr != nil || !published.Mode().IsRegular() || !os.SameFile(created, published) || published.Size() != int64(len(body)) {
		return failCreatedRootExclusiveFile(root, base, file, created, errors.Join(errors.New("created rooted file changed during publication"), publishedErr))
	}
	if err := syncFileLockParent(root); err != nil {
		return failCreatedRootExclusiveFile(root, base, file, created, err)
	}
	if err := file.Close(); err != nil {
		return errors.Join(err, cleanupCreatedRootExclusiveFile(root, base, created))
	}
	return nil
}

func failCreatedRootExclusiveFile(root *os.Root, name string, file *os.File, created fs.FileInfo, cause error) error {
	cleanupErr := cleanupCreatedRootExclusiveFile(root, name, created)
	return errors.Join(cause, cleanupErr, file.Close())
}

func cleanupCreatedRootExclusiveFile(root *os.Root, name string, created fs.FileInfo) (returnErr error) {
	if created == nil {
		return nil
	}
	entry, err := reviewFileLockEntry(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, entry.Close()) }()
	if !os.SameFile(created, entry.info) {
		return errFileLockChangedBeforeCleanup
	}
	return removeReviewedFileLock(root, name, entry)
}

func rootExclusiveTempName(root *os.Root, base string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		name := "." + base + ".create-" + hex.EncodeToString(token[:])
		if _, err := root.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate rooted exclusive-file temporary name")
}

// DefaultFileLockStalePolicy implements threadpoint's cooperative stale-lock
// policy: dead owners are stale immediately, live owners become stale after the
// configured age, and malformed or unreadable metadata gets a short grace
// period before reclaim.
func DefaultFileLockStalePolicy(snapshot FileLockSnapshot, now time.Time, staleAfter time.Duration) (bool, error) {
	if snapshot.Body == nil && snapshot.ModTime.IsZero() {
		return true, nil
	}
	if snapshot.Body == nil {
		return metadataIsPastGrace(snapshot.ModTime, now), nil
	}
	owner, ok := decodeCommonLockOwner(snapshot.Body)
	if !ok {
		return metadataIsPastGrace(snapshot.ModTime, now), nil
	}
	created := owner.Created
	if created.IsZero() && strings.TrimSpace(owner.CreatedAt) != "" {
		parsed, ok := parseLockCreatedAt(owner.CreatedAt)
		if !ok {
			return metadataIsPastGrace(snapshot.ModTime, now), nil
		}
		created = parsed
	}
	return ownerPIDIsStale(owner.PID, created, now, staleAfter), nil
}

func decodeCommonLockOwner(body []byte) (commonLockOwner, bool) {
	var owner commonLockOwner
	if err := json.Unmarshal(body, &owner); err != nil {
		return commonLockOwner{}, false
	}
	return owner, true
}

func parseLockCreatedAt(raw string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func ownerPIDIsStale(pid int, created time.Time, now time.Time, staleAfter time.Duration) bool {
	if pid <= 0 || !processAlive(pid) {
		return true
	}
	// A live PID on an old lock most likely means the PID was reused.
	return !created.IsZero() && now.Sub(created) >= staleAfter
}

// ResolveThreadpointHome returns threadpoint's home directory for locks.
func ResolveThreadpointHome(homeDir string) (string, error) {
	return home.Resolve(homeDir)
}

// ResolveThreadpointHomeWithOverride returns threadpoint's home directory,
// preferring an explicit threadpointHome before environment and defaults.
func ResolveThreadpointHomeWithOverride(homeDir string, threadpointHome string) (string, error) {
	return home.ResolveWithOverride(homeDir, threadpointHome)
}

// ResolveUserHome returns the absolute home directory to use (the provided
// homeDir, or the current user's home when empty). The mutating subsystems
// normalize Options.HomeDir through this so empty-home handling stays
// consistent across the stage, commit, prune, and restore subsystems.
func ResolveUserHome(homeDir string) (string, error) {
	return home.ResolveUserHome(homeDir)
}

// AcquireLocks obtains mutation locks for roots or returns ErrLockHeld.
func AcquireLocks(threadpointHome string, roots []string, command string) (*LockSet, error) {
	selections, err := normalizedLockRootSelections(roots)
	if err != nil {
		return nil, err
	}
	lockRoots := physicalLockRoots(selections)
	if len(lockRoots) == 0 {
		return &LockSet{}, nil
	}
	if strings.TrimSpace(threadpointHome) == "" {
		resolved, err := ResolveThreadpointHome("")
		if err != nil {
			return nil, err
		}
		threadpointHome = resolved
	}
	threadpointHome, err = abspath.Abs(threadpointHome)
	if err != nil {
		return nil, err
	}
	productRoot, locksRoot, err := openPinnedLocksRoot(threadpointHome)
	if err != nil {
		return nil, err
	}
	productParent, productEntry, productName, err := retainRootParentIdentity(threadpointHome, productRoot)
	if err != nil {
		return nil, errors.Join(err, closeRoot(locksRoot), closeRoot(productRoot))
	}
	set := &LockSet{
		productRoot: productRoot, productParent: productParent, productEntry: productEntry,
		productPath: threadpointHome, productName: productName, locksRoot: locksRoot,
	}
	for _, root := range lockRoots {
		token := newLockToken()
		base := lockName(root) + ".lock"
		path := filepath.Join(threadpointHome, "locks", base)
		owner := lockOwner{
			Command: strings.TrimSpace(command),
			PID:     os.Getpid(),
			Root:    root,
			Created: time.Now().UTC(),
			Token:   token,
		}
		body, err := marshalLockOwner(owner)
		if err != nil {
			_ = set.Release()
			return nil, err
		}
		lock, err := AcquireFileLockRoot(context.Background(), locksRoot, base, FileLockOptions{
			Path:           path,
			Body:           body,
			DirMode:        0o700,
			FileMode:       0o600,
			EnforceDirMode: true,
			ReleaseMatches: func(body []byte) bool {
				var current lockOwner
				if err := json.Unmarshal(body, &current); err != nil {
					return false
				}
				return current.Token == token
			},
		})
		if err != nil {
			_ = set.Release()
			if errors.Is(err, ErrLockHeld) {
				var lockErr FileLockError
				_ = errors.As(err, &lockErr)
				return nil, fmt.Errorf("refusing to mutate %s because %w at %s%s; wait or remove it if no threadpoint process is running", root, ErrLockHeld, path, ownerSuffix(lockErr.Body))
			}
			return nil, err
		}
		set.locks = append(set.locks, heldLock{lock: lock})
	}
	for _, selection := range selections {
		if err := set.retainProjectRoot(selection); err != nil {
			_ = set.Release()
			return nil, err
		}
	}
	return set, nil
}

type lockRootSelection struct {
	logicalPath  string
	physicalPath string
}

func (set *LockSet) retainProjectRoot(selection lockRootSelection) error {
	currentPhysical, err := ResolveRootForMutation(selection.logicalPath)
	if err != nil || filepath.Clean(currentPhysical) != selection.physicalPath {
		return errors.Join(errors.New("selected project root changed while its lock was acquired"), err)
	}
	root, err := os.OpenRoot(selection.physicalPath)
	if err != nil {
		return err
	}
	parent, entry, name, err := retainRootParentIdentity(selection.logicalPath, root)
	if err != nil {
		return errors.Join(err, root.Close())
	}
	retained := &retainedProjectRoot{
		root: root, logicalParent: parent, logicalEntry: entry,
		logicalPath: selection.logicalPath, logicalName: name, physicalPath: selection.physicalPath,
	}
	if err := retained.verify(); err != nil {
		return errors.Join(err, parent.Close(), root.Close())
	}
	if set.projectRoots == nil {
		set.projectRoots = make(map[string]*retainedProjectRoot)
	}
	set.projectRoots[selection.logicalPath] = retained
	set.projectPins = append(set.projectPins, retained)
	return nil
}

func retainRootParentIdentity(selectedPath string, root *os.Root) (*os.Root, fs.FileInfo, string, error) {
	if root == nil {
		return nil, nil, "", errors.New("retained root is required")
	}
	path := filepath.Clean(selectedPath)
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	if path == string(filepath.Separator) || parentPath == path || name == "." || name == ".." || name == string(filepath.Separator) {
		return nil, nil, "", errors.New("retained product home requires a final component")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, nil, "", err
	}
	entry, entryErr := parent.Lstat(name)
	current, currentErr := os.Stat(path)
	opened, openedErr := root.Stat(".")
	if entryErr != nil || currentErr != nil || openedErr != nil || !current.IsDir() || !opened.IsDir() || !os.SameFile(current, opened) {
		return nil, nil, "", errors.Join(errors.New("threadpoint product home changed while its parent identity was retained"), entryErr, currentErr, openedErr, parent.Close())
	}
	return parent, entry, name, nil
}

// openPinnedLocksRoot opens the product home and its locks directory once and
// retains both identities for the complete LockSet lifetime. A private identity
// pair anchors the locks directory in its pinned product home: replacing the
// directory leaves the product-home half behind, so a later acquirer fails
// closed instead of creating a duplicate lock namespace.
func openPinnedLocksRoot(threadpointHome string) (productRoot *os.Root, locksRoot *os.Root, returnErr error) {
	var openedProductRoot *os.Root
	var openedLocksRoot *os.Root
	resolvedHome, err := ResolveRootForMutation(threadpointHome)
	if err != nil {
		return nil, nil, err
	}
	if err := RejectExistingSymlinkAncestors(resolvedHome); err != nil {
		return nil, nil, err
	}
	// #nosec G301 -- product state is private; an existing product home keeps its
	// current mode while a newly-created one starts owner-only.
	if err := os.MkdirAll(resolvedHome, 0o700); err != nil {
		return nil, nil, err
	}
	if err := RejectExistingSymlink(resolvedHome); err != nil {
		return nil, nil, err
	}
	productRoot, err = os.OpenRoot(resolvedHome)
	if err != nil {
		return nil, nil, err
	}
	openedProductRoot = productRoot
	defer func() {
		if returnErr == nil {
			return
		}
		returnErr = errors.Join(returnErr, closeRoot(openedLocksRoot), closeRoot(openedProductRoot))
		locksRoot = nil
		productRoot = nil
	}()
	homePathInfo, pathErr := os.Lstat(resolvedHome)
	homePinnedInfo, pinnedErr := productRoot.Stat(".")
	if pathErr != nil || pinnedErr != nil || homePathInfo.Mode()&os.ModeSymlink != 0 || !homePathInfo.IsDir() || !homePinnedInfo.IsDir() || !os.SameFile(homePathInfo, homePinnedInfo) {
		return nil, nil, errors.Join(errors.New("threadpoint product home changed while it was pinned"), pathErr, pinnedErr)
	}

	locksInfo, err := productRoot.Lstat("locks")
	if errors.Is(err, os.ErrNotExist) {
		if err := productRoot.Mkdir("locks", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, nil, err
		}
		locksInfo, err = productRoot.Lstat("locks")
	}
	if err != nil {
		return nil, nil, err
	}
	// This final-component symlink check intentionally precedes both OpenRoot and
	// descriptor-based chmod. A raced replacement is caught by the identity
	// comparison below before chmod is attempted.
	if locksInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("refusing to use symlinked locks directory: %w", ErrUnsafeSymlink)
	}
	if !locksInfo.IsDir() {
		return nil, nil, errors.New("threadpoint locks path must be a directory")
	}
	locksRoot, err = productRoot.OpenRoot("locks")
	if err != nil {
		return nil, nil, err
	}
	openedLocksRoot = locksRoot
	if err := verifyPinnedRootPath(productRoot, "locks", locksRoot); err != nil {
		return nil, nil, err
	}
	directory, err := openPinnedRootDirectory(locksRoot)
	if err != nil {
		return nil, nil, err
	}
	// #nosec G302 -- lock owner tokens and project paths are owner-private.
	chmodErr := directory.Chmod(0o700)
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(chmodErr, syncErr, closeErr); err != nil {
		return nil, nil, err
	}
	if err := verifyPinnedRootPath(productRoot, "locks", locksRoot); err != nil {
		return nil, nil, err
	}
	if err := ensureLocksRootIdentity(productRoot, locksRoot); err != nil {
		return nil, nil, err
	}
	if err := verifyPinnedRootPath(productRoot, "locks", locksRoot); err != nil {
		return nil, nil, err
	}
	return productRoot, locksRoot, nil
}

func ensureLocksRootIdentity(productRoot *os.Root, locksRoot *os.Root) error {
	for attempt := 0; attempt < 100; attempt++ {
		productIdentity, productBody, productExists, err := readLocksRootIdentity(productRoot, productLocksRootIdentityName)
		if err != nil {
			return err
		}
		locksIdentity, locksBody, locksExists, err := readLocksRootIdentity(locksRoot, locksRootIdentityName)
		if err != nil {
			return err
		}
		switch {
		case productExists:
			if !locksExists {
				return errors.New("threadpoint locks directory identity is missing; the locks directory may have been replaced")
			}
			if productIdentity != locksIdentity || !bytes.Equal(productBody, locksBody) {
				return errors.New("threadpoint locks directory identity does not match its product home; the locks directory may have been replaced")
			}
			return nil
		case locksExists:
			// The locks-root half is published first. Any concurrent initializer can
			// safely finish the product-home half using the same reviewed body.
			if err := createRootExclusiveFile(productRoot, productLocksRootIdentityName, locksBody, 0o600); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			continue
		default:
			token := newLockToken()
			body, err := json.Marshal(locksRootIdentity{Version: 1, Token: token})
			if err != nil {
				return err
			}
			body = append(body, '\n')
			if err := createRootExclusiveFile(locksRoot, locksRootIdentityName, body, 0o600); err != nil {
				if errors.Is(err, os.ErrExist) {
					continue
				}
				return err
			}
			if err := createRootExclusiveFile(productRoot, productLocksRootIdentityName, body, 0o600); err != nil && !errors.Is(err, os.ErrExist) {
				return errors.Join(err, removeRootFileMatchingBody(locksRoot, locksRootIdentityName, body))
			}
		}
	}
	return errors.New("threadpoint locks directory identity did not stabilize")
}

func readLocksRootIdentity(root *os.Root, name string) (locksRootIdentity, []byte, bool, error) {
	entry, err := reviewFileLockEntry(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return locksRootIdentity{}, nil, false, nil
	}
	if err != nil {
		return locksRootIdentity{}, nil, false, err
	}
	defer entry.Close()
	if entry.info.Mode().Perm() != 0o600 {
		return locksRootIdentity{}, nil, false, fmt.Errorf("threadpoint locks identity %s must have mode 0600", name)
	}
	var identity locksRootIdentity
	if err := json.Unmarshal(entry.body, &identity); err != nil || identity.Version != 1 || strings.TrimSpace(identity.Token) == "" {
		return locksRootIdentity{}, nil, false, errors.Join(fmt.Errorf("threadpoint locks identity %s is invalid", name), err)
	}
	return identity, append([]byte(nil), entry.body...), true, nil
}

func removeRootFileMatchingBody(root *os.Root, name string, expected []byte) (returnErr error) {
	entry, err := reviewFileLockEntry(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, entry.Close()) }()
	if !bytes.Equal(entry.body, expected) {
		return errFileLockChangedBeforeCleanup
	}
	return removeReviewedFileLock(root, name, entry)
}

func closeRoot(root *os.Root) error {
	if root == nil {
		return nil
	}
	return root.Close()
}

// Release releases all locks in the set.
// Release releases every held lock, newest first, and returns the combined
// error so callers can surface a failed release instead of silently continuing.
func (set *LockSet) Release() error {
	if set == nil {
		return nil
	}
	var err error
	for i := len(set.locks) - 1; i >= 0; i-- {
		err = errors.Join(err, releaseLock(set.locks[i]))
	}
	set.locks = nil
	for i := len(set.projectPins) - 1; i >= 0; i-- {
		retained := set.projectPins[i]
		if retained == nil {
			continue
		}
		if retained.root != nil {
			err = errors.Join(err, retained.root.Close())
			retained.root = nil
		}
		if retained.logicalParent != nil {
			err = errors.Join(err, retained.logicalParent.Close())
			retained.logicalParent = nil
		}
	}
	set.projectPins = nil
	set.projectRoots = nil
	if set.locksRoot != nil {
		err = errors.Join(err, set.locksRoot.Close())
		set.locksRoot = nil
	}
	if set.productRoot != nil {
		err = errors.Join(err, set.productRoot.Close())
		set.productRoot = nil
	}
	if set.productParent != nil {
		err = errors.Join(err, set.productParent.Close())
		set.productParent = nil
	}
	set.productEntry = nil
	set.productPath = ""
	set.productName = ""
	return err
}

type pinnedAtomicTemp struct {
	file      *os.File
	path      string
	info      fs.FileInfo
	published bool
	finalized bool
}

// RootFileGeneration is a durable content generation used to reconcile an
// interrupted cross-file transaction. Exists=false represents an exact absent
// generation; Mode is applied only when Exists is true.
type RootFileGeneration struct {
	Exists bool
	Body   []byte
	Mode   os.FileMode
}

type rootPublishTarget struct {
	file   *os.File
	info   fs.FileInfo
	digest [sha256.Size]byte
}

func createPinnedAtomicTemp(dir string, pattern string) (*pinnedAtomicTemp, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.Join(errors.New("atomic temporary file identity could not be established"), err, file.Close())
	}
	return &pinnedAtomicTemp{file: file, path: file.Name(), info: info}, nil
}

// createPinnedAtomicTempWithMode creates a pinned atomic temporary file using
// mode as the creation mode. Unlike os.CreateTemp, this intentionally lets the
// process umask determine the resulting mode.
func createPinnedAtomicTempWithMode(dir, prefix string, mode os.FileMode) (*pinnedAtomicTemp, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		path := filepath.Join(dir, prefix+hex.EncodeToString(token[:]))
		// #nosec G304 G703 -- dir is symlink-checked by the atomic write preparation path and the random O_EXCL name is identity-pinned.
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.Join(errors.New("atomic temporary file identity could not be established"), err, file.Close())
		}
		return &pinnedAtomicTemp{file: file, path: path, info: info}, nil
	}
	return nil, errors.New("could not allocate a unique atomic temporary file")
}

func verifyPinnedAtomicPath(temp *pinnedAtomicTemp, path string) error {
	if temp == nil || temp.file == nil || temp.info == nil {
		return errors.New("atomic temporary file has no pinned identity")
	}
	pinned, err := temp.file.Stat()
	if err != nil {
		return err
	}
	// #nosec G703 -- path names the already-open random temporary file and is compared with its pinned identity below.
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !pinned.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(temp.info, pinned) || !os.SameFile(temp.info, current) {
		return errors.New("atomic temporary file changed identity")
	}
	return nil
}

func cleanupPinnedAtomicTemp(temp *pinnedAtomicTemp) error {
	if temp == nil || temp.file == nil {
		return nil
	}
	defer func() { temp.file = nil }()
	pinned, pinErr := temp.file.Stat()
	if pinErr != nil || temp.info == nil || !pinned.Mode().IsRegular() || !os.SameFile(temp.info, pinned) {
		return errors.Join(fmt.Errorf("atomic temporary file identity became unverifiable; residue may remain at %s", temp.path), pinErr, temp.file.Close())
	}
	current, err := os.Lstat(temp.path)
	if errors.Is(err, os.ErrNotExist) {
		return temp.file.Close()
	}
	if err != nil {
		return errors.Join(err, temp.file.Close())
	}
	if !current.Mode().IsRegular() || !os.SameFile(temp.info, current) {
		return errors.Join(fmt.Errorf("atomic temporary path changed identity and was retained at %s", temp.path), temp.file.Close())
	}
	if err := os.Remove(temp.path); err != nil {
		return errors.Join(err, temp.file.Close())
	}
	return temp.file.Close()
}

func verifyPinnedRootAtomicPath(root *os.Root, temp *pinnedAtomicTemp) error {
	if root == nil || temp == nil || temp.file == nil || temp.info == nil {
		return errors.New("rooted atomic temporary file has no pinned identity")
	}
	pinned, err := temp.file.Stat()
	if err != nil {
		return err
	}
	current, err := root.Lstat(temp.path)
	if err != nil {
		return err
	}
	if !pinned.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(temp.info, pinned) || !os.SameFile(temp.info, current) {
		return errors.New("rooted atomic temporary file changed identity")
	}
	return nil
}

func cleanupPinnedRootAtomicTemp(root *os.Root, temp *pinnedAtomicTemp) error {
	if temp == nil || temp.file == nil {
		return nil
	}
	defer func() { temp.file = nil }()
	if temp.published {
		closeErr := temp.file.Close()
		if temp.finalized {
			// A successful transaction finalizer is the externally durable commit
			// point. Descriptor cleanup cannot turn that committed transaction into
			// a reported failure that would make the caller abort its linkage.
			return nil
		}
		return closeErr
	}
	pinned, pinErr := temp.file.Stat()
	if pinErr != nil || temp.info == nil || !pinned.Mode().IsRegular() || !os.SameFile(temp.info, pinned) {
		return errors.Join(fmt.Errorf("rooted atomic temporary identity became unverifiable; residue may remain at %s", filepath.Join(root.Name(), temp.path)), pinErr, temp.file.Close())
	}
	current, err := root.Lstat(temp.path)
	if errors.Is(err, os.ErrNotExist) {
		return temp.file.Close()
	}
	if err != nil {
		return errors.Join(err, temp.file.Close())
	}
	if !current.Mode().IsRegular() || !os.SameFile(temp.info, current) {
		return errors.Join(fmt.Errorf("rooted atomic temporary path changed identity and was retained at %s", filepath.Join(root.Name(), temp.path)), temp.file.Close())
	}
	if err := root.Remove(temp.path); err != nil {
		return errors.Join(err, temp.file.Close())
	}
	return temp.file.Close()
}

// AtomicWriteFile safely writes body to path by replacing the target atomically.
func AtomicWriteFile(path string, body []byte, perm os.FileMode) (resultErr error) {
	dir, err := prepareAtomicTarget(path)
	if err != nil {
		return err
	}
	tmp, err := createPinnedAtomicTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanupPinnedAtomicTemp(tmp)) }()
	if err := tmp.file.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.file.Write(body); err != nil {
		return err
	}
	if err := tmp.file.Sync(); err != nil {
		return err
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return err
	}
	if atomicTempBeforePublish != nil {
		atomicTempBeforePublish(tmp.path)
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return fmt.Errorf("atomic write temporary changed before publish: %w", err)
	}
	if err := validateAtomicTarget(path); err != nil {
		return err
	}
	// #nosec G703 -- both paths passed symlink checks and tmp remains bound to its pinned identity immediately before publication.
	if err := os.Rename(tmp.path, path); err != nil {
		return err
	}
	// #nosec G703 -- path was validated immediately before publish and is compared with the pinned temporary identity below.
	current, err := os.Lstat(path)
	pinned, pinErr := tmp.file.Stat()
	if err != nil || pinErr != nil || !current.Mode().IsRegular() || !os.SameFile(tmp.info, pinned) || !os.SameFile(tmp.info, current) {
		return errors.Join(errors.New("atomic write target changed during publish"), err, pinErr)
	}
	return nil
}

// AtomicWriteFileUsingUmask applies the process umask to new files and parents,
// while preserving an existing regular target's permissions. Callers pass
// unmasked modes, normally 0o666 and 0o777; private state uses AtomicWriteFile.
func AtomicWriteFileUsingUmask(path string, body []byte, fileMode, dirMode os.FileMode) (resultErr error) {
	dir, err := prepareAtomicTargetWithMode(path, dirMode)
	if err != nil {
		return err
	}
	tmp, err := createPinnedAtomicTempWithMode(dir, "."+filepath.Base(path)+".tmp-", fileMode)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanupPinnedAtomicTemp(tmp)) }()
	if _, err := tmp.file.Write(body); err != nil {
		return err
	}
	if err := tmp.file.Sync(); err != nil {
		return err
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return err
	}
	if atomicTempBeforePublish != nil {
		atomicTempBeforePublish(tmp.path)
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return fmt.Errorf("atomic write temporary changed before publish: %w", err)
	}
	if err := validateAtomicTarget(path); err != nil {
		return err
	}
	if existingMode, exists, err := existingRegularFileMode(path); err != nil {
		return err
	} else if exists {
		if err := tmp.file.Chmod(existingMode); err != nil {
			return err
		}
	}
	// #nosec G703 -- both paths passed symlink checks and tmp remains bound to its pinned identity immediately before publication.
	if err := os.Rename(tmp.path, path); err != nil {
		return err
	}
	// #nosec G703 -- path was validated immediately before publish and is compared with the pinned temporary identity below.
	current, err := os.Lstat(path)
	pinned, pinErr := tmp.file.Stat()
	if err != nil || pinErr != nil || !current.Mode().IsRegular() || !os.SameFile(tmp.info, pinned) || !os.SameFile(tmp.info, current) {
		return errors.Join(errors.New("atomic write target changed during publish"), err, pinErr)
	}
	return nil
}

func existingRegularFileMode(path string) (os.FileMode, bool, error) {
	// #nosec G703 -- callers invoke this only after prepareAtomicTarget has rejected symlink ancestors and non-regular targets.
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, false, fmt.Errorf("refusing to replace non-regular file %s", path)
	}
	return info.Mode().Perm(), true, nil
}

// AtomicWriteRootFile safely replaces name beneath a pinned os.Root. The
// optional validate callback runs against the pinned target parent at the final
// mutation boundary, allowing callers to compare the exact current state with
// a reviewed snapshot without reopening an ambient pathname.
func AtomicWriteRootFile(root *os.Root, name string, body []byte, perm os.FileMode, validate func(*os.Root, string) error) (resultErr error) {
	if root == nil {
		return errors.New("root is required")
	}
	clean, err := cleanRootName(name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(clean)
	base := filepath.Base(clean)
	if err := RejectRootSymlinkAncestors(root, dir); err != nil {
		return err
	}
	// #nosec G301 -- callers select user-visible project directories and file permissions.
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := RejectRootSymlinkAncestors(root, clean); err != nil {
		return err
	}
	parent, err := root.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := verifyPinnedRootPath(root, dir, parent); err != nil {
		return err
	}
	if err := recoverRootPublishArtifacts(parent, base); err != nil {
		return err
	}

	var tmp *os.File
	var tmpName string
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		tmpName = "." + base + ".tmp-" + hex.EncodeToString(token[:])
		tmp, err = parent.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if tmp == nil {
		return fmt.Errorf("could not allocate temporary file for %s", clean)
	}
	tmpInfo, err := tmp.Stat()
	if err != nil || !tmpInfo.Mode().IsRegular() {
		return errors.Join(errors.New("rooted atomic temporary identity could not be established"), err, tmp.Close())
	}
	prepared := &pinnedAtomicTemp{file: tmp, path: tmpName, info: tmpInfo}
	defer func() { resultErr = errors.Join(resultErr, cleanupPinnedRootAtomicTemp(parent, prepared)) }()
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := verifyPinnedRootAtomicPath(parent, prepared); err != nil {
		return err
	}
	if err := verifyPinnedRootPath(root, dir, parent); err != nil {
		return err
	}
	if err := RejectRootSymlinkAncestors(parent, base); err != nil {
		return err
	}
	expected, err := snapshotRootPublishTarget(parent, base)
	if err != nil {
		return err
	}
	if expected != nil {
		defer expected.file.Close()
	}
	if validate != nil {
		if err := validate(parent, base); err != nil {
			return err
		}
	}
	if atomicRootTempBeforePublish != nil {
		atomicRootTempBeforePublish(parent, tmpName)
	}
	return publishRootTemp(parent, tmpName, base, prepared, expected, nil)
}

// AtomicWriteRootFileUsingUmask is the rooted counterpart of
// AtomicWriteFileUsingUmask. It validates against the pinned parent at the final
// mutation boundary and expects unmasked creation modes.
func AtomicWriteRootFileUsingUmask(root *os.Root, name string, body []byte, fileMode, dirMode os.FileMode, validate func(*os.Root, string) error) (resultErr error) {
	return AtomicWriteRootFileUsingUmaskTransaction(root, name, body, fileMode, dirMode, validate, nil)
}

// AtomicWriteRootFileUsingUmaskTransaction extends
// AtomicWriteRootFileUsingUmask with a post-publication validator. The prior
// exact canonical generation remains at the private exchange name until this
// callback succeeds. A callback failure conditionally exchanges that prior
// generation back, or conditionally removes a newly-created target, without
// overwriting any later replacement.
func AtomicWriteRootFileUsingUmaskTransaction(root *os.Root, name string, body []byte, fileMode, dirMode os.FileMode, validateBefore, validateAfter func(*os.Root, string) error) (resultErr error) {
	if root == nil {
		return errors.New("root is required")
	}
	clean, err := cleanRootName(name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(clean)
	base := filepath.Base(clean)
	if err := RejectRootSymlinkAncestors(root, dir); err != nil {
		return err
	}
	// #nosec G301 -- canonical project directories use the user's umask; they are not secret state.
	if err := root.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	if err := RejectRootSymlinkAncestors(root, clean); err != nil {
		return err
	}
	parent, err := root.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := verifyPinnedRootPath(root, dir, parent); err != nil {
		return err
	}
	if err := recoverRootPublishArtifacts(parent, base); err != nil {
		return err
	}

	var tmp *os.File
	var tmpName string
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		tmpName = "." + base + ".tmp-" + hex.EncodeToString(token[:])
		// #nosec G302 -- fileMode is the caller's unmasked mode; the kernel applies the process umask on creation.
		tmp, err = parent.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if tmp == nil {
		return fmt.Errorf("could not allocate temporary file for %s", clean)
	}
	tmpInfo, err := tmp.Stat()
	if err != nil || !tmpInfo.Mode().IsRegular() {
		return errors.Join(errors.New("rooted atomic temporary identity could not be established"), err, tmp.Close())
	}
	prepared := &pinnedAtomicTemp{file: tmp, path: tmpName, info: tmpInfo}
	defer func() { resultErr = errors.Join(resultErr, cleanupPinnedRootAtomicTemp(parent, prepared)) }()
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := verifyPinnedRootAtomicPath(parent, prepared); err != nil {
		return err
	}
	if err := verifyPinnedRootPath(root, dir, parent); err != nil {
		return err
	}
	if err := RejectRootSymlinkAncestors(parent, base); err != nil {
		return err
	}
	// Preserve the permission bits of an existing regular target, matching
	// AtomicWriteFileUsingUmask; a new target keeps the umask-derived mode.
	expected, err := snapshotRootPublishTarget(parent, base)
	if err != nil {
		return err
	}
	if expected != nil {
		defer expected.file.Close()
		if err := tmp.Chmod(expected.info.Mode().Perm()); err != nil {
			return err
		}
	}
	if validateBefore != nil {
		if err := validateBefore(parent, base); err != nil {
			return err
		}
	}
	if atomicRootTempBeforePublish != nil {
		atomicRootTempBeforePublish(parent, tmpName)
	}
	return publishRootTemp(parent, tmpName, base, prepared, expected, validateAfter)
}

// publishRootTemp uses no-replace creation for a missing target and atomic
// exchange for an existing one. It verifies the displaced generation and rolls
// back a mismatch without leaving the canonical name absent.
func publishRootTemp(parent *os.Root, tmpName, base string, prepared *pinnedAtomicTemp, expected *rootPublishTarget, validateAfter func(*os.Root, string) error) error {
	if !RenameRootNoReplaceSupported() {
		return ErrRenameNoReplaceUnsupported
	}
	if err := verifyPinnedRootAtomicPath(parent, prepared); err != nil {
		return fmt.Errorf("rooted atomic temporary changed before publish: %w", err)
	}
	// Make the complete prepared generation durable before any publication
	// primitive can make it canonical or detach the prior canonical entry.
	if err := syncFileLockParent(parent); err != nil {
		return err
	}
	if atomicRootBeforePublishRename != nil {
		atomicRootBeforePublishRename(parent, base)
	}
	if expected == nil {
		if err := RenameRootNoReplace(parent, tmpName, base); err != nil {
			return err
		}
		prepared.published = true
		installed, installErr := parent.Lstat(base)
		pinned, pinErr := prepared.file.Stat()
		if installErr != nil || pinErr != nil || !installed.Mode().IsRegular() || !pinned.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, installed) {
			return errors.Join(errors.New("newly-created rooted target changed during publication"), installErr, pinErr)
		}
		if err := syncFileLockParent(parent); err != nil {
			rollbackErr := rollbackMissingRootPublish(parent, tmpName, base, prepared)
			if rollbackErr == nil {
				prepared.published = false
			}
			return errors.Join(err, rollbackErr)
		}
		if validateAfter != nil {
			if err := verifyPublishedRootTemp(parent, base, prepared, "", nil); err != nil {
				rollbackErr := rollbackMissingRootPublish(parent, tmpName, base, prepared)
				if rollbackErr == nil {
					prepared.published = false
				}
				return errors.Join(err, rollbackErr)
			}
			if err := validateAfter(parent, base); err != nil {
				rollbackErr := rollbackMissingRootPublish(parent, tmpName, base, prepared)
				if rollbackErr == nil {
					prepared.published = false
				}
				return errors.Join(err, rollbackErr)
			}
			prepared.finalized = true
			return nil
		}
	} else {
		exchangeErr := ErrRenameExchangeUnsupported
		if RenameRootExchangeSupported() {
			exchangeErr = exchangeRootOperation(parent, tmpName, base)
		}
		if errors.Is(exchangeErr, ErrRenameExchangeUnsupported) {
			return publishRootTempWithJournal(parent, tmpName, base, prepared, expected, validateAfter)
		}
		if exchangeErr != nil {
			return exchangeErr
		}
		detached, detachErr := parent.Lstat(tmpName)
		if detachErr != nil || !detached.Mode().IsRegular() || !rootPublishTargetMatches(expected, detached) {
			restoreErr := rollbackRootPublishExchangeCurrent(parent, tmpName, base, prepared)
			return errors.Join(errors.New("rooted atomic target changed before publish"), detachErr, restoreErr)
		}
		installed, installErr := parent.Lstat(base)
		pinned, pinErr := prepared.file.Stat()
		if installErr != nil || pinErr != nil || !installed.Mode().IsRegular() || !pinned.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, installed) {
			restoreErr := rollbackRootPublishExchange(parent, tmpName, base, prepared, expected)
			return errors.Join(errors.New("rooted atomic target changed during exchange publication"), installErr, pinErr, restoreErr)
		}
		prepared.published = true
		if err := syncFileLockParent(parent); err != nil {
			rollbackErr := rollbackRootPublishExchange(parent, tmpName, base, prepared, expected)
			if rollbackErr == nil {
				prepared.published = false
			}
			return errors.Join(err, rollbackErr)
		}
		if atomicRootAfterPublishExchange != nil {
			if err := atomicRootAfterPublishExchange(parent, base, tmpName); err != nil {
				return err
			}
		}
		if validateAfter != nil {
			if err := verifyPublishedRootTemp(parent, base, prepared, tmpName, expected); err != nil {
				rollbackErr := rollbackRootPublishExchange(parent, tmpName, base, prepared, expected)
				if rollbackErr == nil {
					prepared.published = false
				}
				return errors.Join(err, rollbackErr)
			}
			if err := validateAfter(parent, base); err != nil {
				rollbackErr := rollbackRootPublishExchange(parent, tmpName, base, prepared, expected)
				if rollbackErr == nil {
					prepared.published = false
				}
				return errors.Join(err, rollbackErr)
			}
			prepared.finalized = true
			// Canonical content and its external transaction linkage are now one
			// committed generation. The displaced exact prior inode is private
			// recovery residue; cleanup and its directory sync are best-effort and
			// recoverRootPublishArtifacts will reconcile any remainder next time.
			if err := removePinnedRootPublishTarget(parent, tmpName, expected); err == nil {
				_ = syncFileLockParent(parent)
			}
			return nil
		}
		installed, installErr = parent.Lstat(base)
		pinned, pinErr = prepared.file.Stat()
		if installErr != nil || pinErr != nil || !installed.Mode().IsRegular() || !pinned.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, installed) {
			return errors.Join(errors.New("rooted atomic target changed before prior-generation cleanup"), installErr, pinErr)
		}
		if err := removePinnedRootPublishTarget(parent, tmpName, expected); err != nil {
			return fmt.Errorf("rooted atomic target published but prior generation cleanup failed: %w", err)
		}
		if err := syncFileLockParent(parent); err != nil {
			return err
		}
	}
	current, err := parent.Lstat(base)
	pinned, pinErr := prepared.file.Stat()
	if err != nil || pinErr != nil || !current.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, current) {
		return errors.Join(errors.New("rooted atomic target changed during publish"), err, pinErr)
	}
	return nil
}

func verifyPublishedRootTemp(parent *os.Root, base string, prepared *pinnedAtomicTemp, oldName string, expected *rootPublishTarget) error {
	installed, installErr := parent.Lstat(base)
	pinned, pinErr := prepared.file.Stat()
	if installErr != nil || pinErr != nil || !installed.Mode().IsRegular() || !pinned.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, installed) {
		return errors.Join(errors.New("rooted atomic target changed before transaction finalization"), installErr, pinErr)
	}
	if expected != nil {
		displaced, err := parent.Lstat(oldName)
		if err != nil || !rootPublishTargetMatches(expected, displaced) {
			return errors.Join(errors.New("prior canonical generation changed before transaction finalization"), err)
		}
	}
	return nil
}

func rollbackMissingRootPublish(parent *os.Root, tmpName, base string, prepared *pinnedAtomicTemp) error {
	current, currentErr := parent.Lstat(base)
	pinned, pinnedErr := prepared.file.Stat()
	if currentErr != nil || pinnedErr != nil || !current.Mode().IsRegular() || !pinned.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, current) {
		return errors.Join(errors.New("refusing to roll back newly-created rooted target after it changed"), currentErr, pinnedErr)
	}
	if err := RenameRootNoReplace(parent, base, tmpName); err != nil {
		return err
	}
	temporary, err := parent.Lstat(tmpName)
	if err != nil || !temporary.Mode().IsRegular() || !os.SameFile(prepared.info, temporary) {
		return errors.Join(errors.New("newly-created rooted target rollback could not verify its generation"), err)
	}
	return syncFileLockParent(parent)
}

func rollbackRootPublishExchange(parent *os.Root, tmpName, base string, prepared *pinnedAtomicTemp, expected *rootPublishTarget) error {
	current, currentErr := parent.Lstat(base)
	pinned, pinnedErr := prepared.file.Stat()
	displaced, displacedErr := parent.Lstat(tmpName)
	if currentErr != nil || pinnedErr != nil || displacedErr != nil || !current.Mode().IsRegular() || !pinned.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, current) || !rootPublishTargetMatches(expected, displaced) {
		return errors.Join(errors.New("refusing to roll back rooted exchange after either transaction generation changed"), currentErr, pinnedErr, displacedErr)
	}
	if err := ExchangeRoot(parent, tmpName, base); err != nil {
		return err
	}
	restored, restoredErr := parent.Lstat(base)
	temporary, temporaryErr := parent.Lstat(tmpName)
	if restoredErr != nil || temporaryErr != nil || !restored.Mode().IsRegular() || !temporary.Mode().IsRegular() ||
		!rootPublishTargetMatches(expected, restored) || !os.SameFile(prepared.info, temporary) {
		return errors.Join(errors.New("rooted exchange rollback could not verify restored generations"), restoredErr, temporaryErr)
	}
	return syncFileLockParent(parent)
}

func rollbackRootPublishExchangeCurrent(parent *os.Root, tmpName, base string, prepared *pinnedAtomicTemp) error {
	current, currentErr := parent.Lstat(base)
	pinned, pinnedErr := prepared.file.Stat()
	displaced, displacedErr := parent.Lstat(tmpName)
	if currentErr != nil || pinnedErr != nil || displacedErr != nil || !current.Mode().IsRegular() || !pinned.Mode().IsRegular() || !displaced.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, current) {
		return errors.Join(errors.New("refusing to restore the current rooted target after either exchanged generation changed"), currentErr, pinnedErr, displacedErr)
	}
	if err := ExchangeRoot(parent, tmpName, base); err != nil {
		return err
	}
	restored, restoredErr := parent.Lstat(base)
	temporary, temporaryErr := parent.Lstat(tmpName)
	if restoredErr != nil || temporaryErr != nil || !restored.Mode().IsRegular() || !temporary.Mode().IsRegular() || !os.SameFile(displaced, restored) || !os.SameFile(prepared.info, temporary) {
		return errors.Join(errors.New("current rooted target rollback could not verify restored generations"), restoredErr, temporaryErr)
	}
	return syncFileLockParent(parent)
}

// publishRootTempWithJournal is the portability path for filesystems without
// an atomic exchange primitive. The .publish entry is the durable transaction
// journal: every rename is bracketed by a parent-directory sync, and recovery
// deterministically restores it whenever the stable canonical name is absent.
func publishRootTempWithJournal(parent *os.Root, tmpName, base string, prepared *pinnedAtomicTemp, expected *rootPublishTarget, validateAfter func(*os.Root, string) error) error {
	oldName, err := rootPublishName(parent, base)
	if err != nil {
		return err
	}
	// Persist the prepared generation before the first rename can detach the
	// canonical entry. A crash can then recover either the old or new complete
	// generation solely from the stable transaction names.
	if err := syncFileLockParent(parent); err != nil {
		return err
	}
	if err := RenameRootNoReplace(parent, base, oldName); err != nil {
		return err
	}
	detached, detachErr := parent.Lstat(oldName)
	if detachErr != nil || !detached.Mode().IsRegular() || !rootPublishTargetMatches(expected, detached) {
		restoreErr := restoreDetachedRootPublishTarget(parent, oldName, base, nil)
		return errors.Join(errors.New("rooted atomic target changed before journaled publication"), detachErr, restoreErr)
	}
	if err := syncFileLockParent(parent); err != nil {
		restoreErr := restoreDetachedRootPublishTarget(parent, oldName, base, expected)
		return errors.Join(err, restoreErr)
	}
	if atomicRootAfterPublishDetach != nil {
		if err := atomicRootAfterPublishDetach(parent, base, oldName); err != nil {
			return err
		}
	}
	if err := RenameRootNoReplace(parent, tmpName, base); err != nil {
		restoreErr := restoreDetachedRootPublishTarget(parent, oldName, base, expected)
		return errors.Join(err, restoreErr)
	}
	prepared.published = true
	installed, installErr := parent.Lstat(base)
	pinned, pinErr := prepared.file.Stat()
	if installErr != nil || pinErr != nil || !installed.Mode().IsRegular() || !pinned.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, installed) {
		return errors.Join(errors.New("journaled rooted target changed during publication"), installErr, pinErr)
	}
	if err := syncFileLockParent(parent); err != nil {
		rollbackErr := rollbackJournaledRootPublish(parent, tmpName, oldName, base, prepared, expected)
		if rollbackErr == nil {
			prepared.published = false
		}
		return errors.Join(err, rollbackErr)
	}
	if validateAfter != nil {
		if err := verifyPublishedRootTemp(parent, base, prepared, oldName, expected); err != nil {
			rollbackErr := rollbackJournaledRootPublish(parent, tmpName, oldName, base, prepared, expected)
			if rollbackErr == nil {
				prepared.published = false
			}
			return errors.Join(err, rollbackErr)
		}
		if err := validateAfter(parent, base); err != nil {
			rollbackErr := rollbackJournaledRootPublish(parent, tmpName, oldName, base, prepared, expected)
			if rollbackErr == nil {
				prepared.published = false
			}
			return errors.Join(err, rollbackErr)
		}
		prepared.finalized = true
		if err := removePinnedRootPublishTarget(parent, oldName, expected); err == nil {
			_ = syncFileLockParent(parent)
		}
		return nil
	}
	installed, installErr = parent.Lstat(base)
	pinned, pinErr = prepared.file.Stat()
	if installErr != nil || pinErr != nil || !installed.Mode().IsRegular() || !pinned.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, installed) {
		return errors.Join(errors.New("journaled rooted target changed before prior-generation cleanup"), installErr, pinErr)
	}
	if err := removePinnedRootPublishTarget(parent, oldName, expected); err != nil {
		return fmt.Errorf("journaled rooted target published but prior generation cleanup failed: %w", err)
	}
	return syncFileLockParent(parent)
}

func rollbackJournaledRootPublish(parent *os.Root, tmpName, oldName, base string, prepared *pinnedAtomicTemp, expected *rootPublishTarget) error {
	current, currentErr := parent.Lstat(base)
	pinned, pinnedErr := prepared.file.Stat()
	displaced, displacedErr := parent.Lstat(oldName)
	if currentErr != nil || pinnedErr != nil || displacedErr != nil || !current.Mode().IsRegular() || !pinned.Mode().IsRegular() || !os.SameFile(prepared.info, pinned) || !os.SameFile(prepared.info, current) || !rootPublishTargetMatches(expected, displaced) {
		return errors.Join(errors.New("refusing to roll back journaled rooted target after either transaction generation changed"), currentErr, pinnedErr, displacedErr)
	}
	if err := RenameRootNoReplace(parent, base, tmpName); err != nil {
		return err
	}
	if err := syncFileLockParent(parent); err != nil {
		return err
	}
	return restoreDetachedRootPublishTarget(parent, oldName, base, expected)
}

func restoreDetachedRootPublishTarget(parent *os.Root, oldName, base string, expected *rootPublishTarget) error {
	if err := RenameRootNoReplace(parent, oldName, base); err != nil {
		return err
	}
	restored, err := parent.Lstat(base)
	if err != nil || !restored.Mode().IsRegular() || (expected != nil && !rootPublishTargetMatches(expected, restored)) {
		return errors.Join(errors.New("journaled prior canonical generation could not be restored"), err)
	}
	return syncFileLockParent(parent)
}

func removePinnedRootPublishTarget(parent *os.Root, name string, expected *rootPublishTarget) error {
	current, err := parent.Lstat(name)
	if err != nil || !rootPublishTargetMatches(expected, current) {
		return errors.Join(errors.New("refusing to remove changed prior canonical generation"), err)
	}
	quarantine, err := rootCleanupName(parent, name)
	if err != nil {
		return err
	}
	if err := RenameRootNoReplace(parent, name, quarantine); err != nil {
		return err
	}
	detached, detachErr := parent.Lstat(quarantine)
	if detachErr != nil || !rootPublishTargetMatches(expected, detached) {
		restoreErr := RenameRootNoReplace(parent, quarantine, name)
		return errors.Join(errors.New("prior canonical generation changed during cleanup"), detachErr, restoreErr)
	}
	return parent.Remove(quarantine)
}

func rootPublishName(parent *os.Root, base string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		name := "." + base + ".publish-" + hex.EncodeToString(token[:])
		if _, err := parent.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate rooted publication journal name")
}

// recoverRootPublishArtifacts reconciles interrupted rooted publications before
// another write exposes the target. The exchange protocol always leaves base
// present; a leftover .tmp entry is therefore only the displaced prior
// generation. The older two-rename protocol may leave base absent with one
// .publish entry; in that case the exact prior generation is restored before
// private temporaries are removed.
func recoverRootPublishArtifacts(parent *os.Root, base string) error {
	oldNames, tmpNames, err := listRootPublishArtifacts(parent, base)
	if err != nil {
		return err
	}
	if len(oldNames) == 0 && len(tmpNames) == 0 {
		return nil
	}
	sort.Strings(oldNames)
	sort.Strings(tmpNames)
	canonical, canonicalErr := parent.Lstat(base)
	switch {
	case canonicalErr == nil:
		if !canonical.Mode().IsRegular() {
			return fmt.Errorf("refusing to recover publication artifacts beside non-regular target %s", base)
		}
	case errors.Is(canonicalErr, os.ErrNotExist):
		switch len(oldNames) {
		case 0:
			if len(tmpNames) == 0 {
				return nil
			}
			// A missing canonical plus only prepared temporaries is the ordinary
			// interrupted-create state. None was ever canonical, so discard them.
		case 1:
			if err := restoreRootPublishArtifact(parent, oldNames[0], base); err != nil {
				return fmt.Errorf("recover prior canonical generation %s: %w", base, err)
			}
		default:
			return fmt.Errorf("cannot recover %s: multiple prior canonical generations exist", base)
		}
	case canonicalErr != nil:
		return canonicalErr
	}
	for _, name := range append(oldNames, tmpNames...) {
		if _, err := parent.Lstat(name); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := removeRootPublishArtifact(parent, name); err != nil {
			return fmt.Errorf("remove recovered publication artifact %s: %w", name, err)
		}
	}
	return syncFileLockParent(parent)
}

func rootPublishArtifactName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if len(suffix) != 24 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func listRootPublishArtifacts(parent *os.Root, base string) ([]string, []string, error) {
	return listRootPublishArtifactsBounded(parent, base, rootPublishScanMaxEntries, rootPublishScanMaxNameBytes)
}

func listRootPublishArtifactsBounded(parent *os.Root, base string, maxEntries, maxNameBytes int) ([]string, []string, error) {
	if maxEntries <= 0 || maxNameBytes <= 0 {
		return nil, nil, errors.New("rooted publication artifact scan requires positive bounds")
	}
	directory, err := parent.Open(".")
	if err != nil {
		return nil, nil, err
	}
	var oldNames []string
	var tmpNames []string
	oldPrefix := "." + base + ".publish-"
	tmpPrefix := "." + base + ".tmp-"
	entryCount := 0
	nameBytes := 0
	var readErr error
	for {
		entries, batchErr := directory.ReadDir(rootPublishScanBatch)
		for _, entry := range entries {
			entryCount++
			nameBytes += len(entry.Name())
			if entryCount > maxEntries || nameBytes > maxNameBytes {
				readErr = fmt.Errorf("rooted publication artifact scan exceeded its directory budget (%d entries, %d name bytes)", maxEntries, maxNameBytes)
				break
			}
			switch {
			case rootPublishArtifactName(entry.Name(), oldPrefix):
				oldNames = append(oldNames, entry.Name())
			case rootPublishArtifactName(entry.Name(), tmpPrefix):
				tmpNames = append(tmpNames, entry.Name())
			}
		}
		if readErr != nil {
			break
		}
		if errors.Is(batchErr, io.EOF) {
			break
		}
		if batchErr != nil {
			readErr = batchErr
			break
		}
	}
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return nil, nil, errors.Join(readErr, closeErr)
	}
	sort.Strings(oldNames)
	sort.Strings(tmpNames)
	return oldNames, tmpNames, nil
}

func discardRootPublishArtifacts(parent *os.Root, base string) error {
	oldNames, tmpNames, err := listRootPublishArtifacts(parent, base)
	if err != nil {
		return err
	}
	for _, name := range append(oldNames, tmpNames...) {
		if _, err := parent.Lstat(name); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := removeRootPublishArtifact(parent, name); err != nil {
			return fmt.Errorf("discard rooted transaction residue %s: %w", name, err)
		}
	}
	if len(oldNames) == 0 && len(tmpNames) == 0 {
		return nil
	}
	return syncFileLockParent(parent)
}

func restoreRootPublishArtifact(parent *os.Root, name, base string) (returnErr error) {
	expected, err := snapshotRootPublishTarget(parent, name)
	if err != nil {
		return err
	}
	if expected == nil {
		return os.ErrNotExist
	}
	defer func() { returnErr = errors.Join(returnErr, expected.file.Close()) }()
	if err := RenameRootNoReplace(parent, name, base); err != nil {
		return err
	}
	restored, err := parent.Lstat(base)
	if err != nil || !rootPublishTargetMatches(expected, restored) {
		return errors.Join(errors.New("restored canonical generation changed during recovery"), err)
	}
	return syncFileLockParent(parent)
}

func removeRootPublishArtifact(parent *os.Root, name string) (returnErr error) {
	expected, err := snapshotRootPublishTarget(parent, name)
	if err != nil {
		return err
	}
	if expected == nil {
		return nil
	}
	defer func() { returnErr = errors.Join(returnErr, expected.file.Close()) }()
	quarantine, err := rootCleanupName(parent, name)
	if err != nil {
		return err
	}
	if err := RenameRootNoReplace(parent, name, quarantine); err != nil {
		return err
	}
	detached, err := parent.Lstat(quarantine)
	if err != nil || !rootPublishTargetMatches(expected, detached) {
		restoreErr := RenameRootNoReplace(parent, quarantine, name)
		return errors.Join(errors.New("publication artifact changed during cleanup"), err, restoreErr)
	}
	if err := parent.Remove(quarantine); err != nil {
		return err
	}
	return nil
}

func rootCleanupName(parent *os.Root, base string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		name := "." + filepath.Base(base) + ".cleanup-" + hex.EncodeToString(token[:])
		if _, err := parent.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate rooted publication cleanup name")
}

// snapshotRootPublishTarget captures the exact target generation before a
// caller's validation callback runs. Publication carries this snapshot through
// the detach-and-verify protocol, so changing the target after validation can
// never redefine which object the callback approved.
func snapshotRootPublishTarget(parent *os.Root, base string) (*rootPublishTarget, error) {
	info, err := parent.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // nil target is the deliberate missing-generation snapshot.
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to replace non-regular target %s", base)
	}
	file, err := parent.OpenFile(base, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("rooted atomic target changed while it was pinned"), err, file.Close())
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return &rootPublishTarget{file: file, info: opened, digest: digest}, nil
}

func rootPublishTargetMatches(expected *rootPublishTarget, current fs.FileInfo) bool {
	if expected == nil || expected.file == nil || expected.info == nil || current == nil || !current.Mode().IsRegular() || !os.SameFile(expected.info, current) || expected.info.Mode() != current.Mode() {
		return false
	}
	opened, err := expected.file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(expected.info, opened) || !os.SameFile(opened, current) || opened.Size() != current.Size() || !opened.ModTime().Equal(current.ModTime()) {
		return false
	}
	if _, err := expected.file.Seek(0, io.SeekStart); err != nil {
		return false
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, expected.file); err != nil {
		return false
	}
	return bytes.Equal(hash.Sum(nil), expected.digest[:])
}

// RestoreRootFileTransaction conditionally restores prior only when the
// current canonical generation is either the exact published bytes or already
// the exact prior generation. It deliberately ignores private publication
// residue when deciding what to restore: the caller's durable transaction
// record is authoritative. This operation deliberately leaves reserved
// publication residue in place: callers must durably tombstone the pending
// transaction before invoking CleanupRootFileTransaction for the restored
// generation.
func RestoreRootFileTransaction(root *os.Root, name string, published, prior RootFileGeneration) (resultErr error) {
	if root == nil {
		return errors.New("root is required")
	}
	if !published.Exists || len(published.Body) == 0 {
		return errors.New("published rooted transaction generation is required")
	}
	parent, base, err := openRootFileTransactionParent(root, name)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	current, err := snapshotRootPublishTarget(parent, base)
	if err != nil {
		return err
	}
	if current != nil {
		defer func() { resultErr = errors.Join(resultErr, current.file.Close()) }()
	}

	currentIsPrior := generationMatchesRootTarget(current, prior, true)
	switch {
	case currentIsPrior:
		// A prior recovery attempt may have restored the canonical name before
		// crashing. Reconciliation remains idempotent.
	case current == nil && !prior.Exists:
		// Both durable generations agree that the canonical entry is absent.
	case current == nil && prior.Exists:
		// The journal fallback may crash after detaching the prior generation
		// but before publishing the prepared generation. Restore the sealed
		// prior bytes without interpreting any private residue as canonical.
		if err := writeRootTransactionGeneration(parent, base, prior.Body, prior.Mode, nil); err != nil {
			return err
		}
	case !generationMatchesRootTarget(current, published, false):
		return errors.New("canonical target is neither the bound published generation nor the exact prior generation")
	case prior.Exists:
		if err := writeRootTransactionGeneration(parent, base, prior.Body, prior.Mode, current); err != nil {
			return err
		}
	case current != nil:
		if err := removePinnedRootPublishTarget(parent, base, current); err != nil {
			return err
		}
		if err := syncFileLockParent(parent); err != nil {
			return err
		}
	default:
		return errors.New("missing canonical target cannot match a bound published generation")
	}
	return nil
}

// CleanupRootFileTransaction verifies that the current canonical content is
// the exact expected generation (including an expected absence) before
// discarding reserved publication residue. It never changes the canonical
// entry. Recovery callers invoke it only after the pending transaction has
// been durably tombstoned.
func CleanupRootFileTransaction(root *os.Root, name string, expected RootFileGeneration) (resultErr error) {
	if root == nil {
		return errors.New("root is required")
	}
	parent, base, err := openRootFileTransactionParent(root, name)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	current, err := snapshotRootPublishTarget(parent, base)
	if err != nil {
		return err
	}
	if current != nil {
		defer func() { resultErr = errors.Join(resultErr, current.file.Close()) }()
	}
	if !generationMatchesRootTarget(current, expected, false) {
		return errors.New("canonical target does not match the bound expected generation")
	}
	return discardRootPublishArtifacts(parent, base)
}

func openRootFileTransactionParent(root *os.Root, name string) (*os.Root, string, error) {
	clean, err := cleanRootName(name)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Dir(clean)
	base := filepath.Base(clean)
	if err := RejectRootSymlinkAncestors(root, dir); err != nil {
		return nil, "", err
	}
	parent, err := root.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	if err := verifyPinnedRootPath(root, dir, parent); err != nil {
		return nil, "", errors.Join(err, parent.Close())
	}
	if err := RejectRootSymlinkAncestors(parent, base); err != nil {
		return nil, "", errors.Join(err, parent.Close())
	}
	return parent, base, nil
}

func generationMatchesRootTarget(target *rootPublishTarget, generation RootFileGeneration, requireMode bool) bool {
	if !generation.Exists {
		return target == nil
	}
	if target == nil || target.info == nil || target.info.Size() != int64(len(generation.Body)) {
		return false
	}
	digest := sha256.Sum256(generation.Body)
	if !bytes.Equal(target.digest[:], digest[:]) {
		return false
	}
	return !requireMode || target.info.Mode().Perm() == generation.Mode.Perm()
}

func writeRootTransactionGeneration(parent *os.Root, base string, body []byte, mode os.FileMode, expected *rootPublishTarget) (resultErr error) {
	var file *os.File
	var name string
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		name = "." + base + ".tmp-" + hex.EncodeToString(token[:])
		file, err = parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if file == nil {
		return fmt.Errorf("could not allocate recovery temporary for %s", base)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(errors.New("recovery temporary identity could not be established"), err, file.Close())
	}
	prepared := &pinnedAtomicTemp{file: file, path: name, info: info}
	defer func() { resultErr = errors.Join(resultErr, cleanupPinnedRootAtomicTemp(parent, prepared)) }()
	if err := file.Chmod(mode.Perm()); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return publishRootTemp(parent, name, base, prepared, expected, nil)
}

func cleanRootName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("root-relative path is required")
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes rooted filesystem: %s", name)
	}
	return clean, nil
}

// RenameRootNoReplaceSupported reports whether this build target has a native
// rooted rename primitive with atomic destination-no-replace semantics. The
// actual operation can still fail closed with ErrRenameNoReplaceUnsupported
// when the running kernel or filesystem does not implement the native flag.
func RenameRootNoReplaceSupported() bool {
	return renameRootNoReplaceSupported
}

// RenameRootExchangeSupported reports whether this build target has a native
// atomic rooted exchange primitive. Runtime kernels and filesystems may still
// reject an individual exchange, which fails closed.
func RenameRootExchangeSupported() bool {
	return renameRootExchangeSupported
}

// ExchangeRoot atomically swaps two existing entries beneath root. The
// canonical name therefore remains present throughout replacement, and the
// displaced generation remains available at the former temporary name for
// exact validation or recovery.
func ExchangeRoot(root *os.Root, firstName, secondName string) error {
	if root == nil {
		return errors.New("root is required")
	}
	firstParent, firstBase, err := openPinnedRenameParent(root, firstName)
	if err != nil {
		return err
	}
	defer firstParent.Close()
	secondParent, secondBase, err := openPinnedRenameParent(root, secondName)
	if err != nil {
		return err
	}
	defer secondParent.Close()
	if err := verifyPinnedRenameParent(root, filepath.Dir(filepath.Clean(filepath.FromSlash(firstName))), firstParent); err != nil {
		return err
	}
	if err := verifyPinnedRenameParent(root, filepath.Dir(filepath.Clean(filepath.FromSlash(secondName))), secondParent); err != nil {
		return err
	}
	if err := renameRootExchangeAt(firstParent, firstBase, secondParent, secondBase); err != nil {
		return &os.LinkError{Op: "rename-exchange", Old: firstName, New: secondName, Err: err}
	}
	return nil
}

// RenameRootNoReplace atomically renames beneath pinned, symlink-free parents
// only when the destination is absent. The source may itself be a symlink.
// Unsupported platforms and filesystems fail closed rather than emulating it.
func RenameRootNoReplace(root *os.Root, oldName string, newName string) error {
	if root == nil {
		return errors.New("root is required")
	}
	oldParent, oldBase, err := openPinnedRenameParent(root, oldName)
	if err != nil {
		return err
	}
	defer oldParent.Close()
	newParent, newBase, err := openPinnedRenameParent(root, newName)
	if err != nil {
		return err
	}
	defer newParent.Close()
	if err := verifyPinnedRenameParent(root, filepath.Dir(filepath.Clean(filepath.FromSlash(oldName))), oldParent); err != nil {
		return err
	}
	if err := verifyPinnedRenameParent(root, filepath.Dir(filepath.Clean(filepath.FromSlash(newName))), newParent); err != nil {
		return err
	}
	if err := renameRootNoReplaceAt(oldParent, oldBase, newParent, newBase); err != nil {
		return &os.LinkError{Op: "rename-noreplace", Old: oldName, New: newName, Err: err}
	}
	return nil
}

// RenamePinnedRootNoReplace atomically moves one final-component entry between
// two already-pinned parent directories only when the destination is absent.
// Unlike RenameRootNoReplace, this function never resolves a nested parent by
// pathname: callers that already hold the transaction's exact source and
// destination parent handles can carry those identities through the rename.
func RenamePinnedRootNoReplace(oldParent *os.Root, oldName string, newParent *os.Root, newName string) error {
	if oldParent == nil || newParent == nil {
		return errors.New("pinned source and destination roots are required")
	}
	oldBase, err := cleanPinnedRenameBase(oldName)
	if err != nil {
		return err
	}
	newBase, err := cleanPinnedRenameBase(newName)
	if err != nil {
		return err
	}
	oldDirectory, err := openPinnedRootDirectory(oldParent)
	if err != nil {
		return err
	}
	defer oldDirectory.Close()
	newDirectory, err := openPinnedRootDirectory(newParent)
	if err != nil {
		return err
	}
	defer newDirectory.Close()
	if err := renameRootNoReplaceAt(oldDirectory, oldBase, newDirectory, newBase); err != nil {
		return &os.LinkError{Op: "rename-noreplace", Old: filepath.Join(oldParent.Name(), oldBase), New: filepath.Join(newParent.Name(), newBase), Err: err}
	}
	return nil
}

func cleanPinnedRenameBase(name string) (string, error) {
	clean, err := cleanRootName(name)
	if err != nil {
		return "", err
	}
	if clean == "." || filepath.Dir(clean) != "." {
		return "", fmt.Errorf("pinned rooted rename requires a final-component name: %s", name)
	}
	base := filepath.Base(clean)
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("invalid pinned rooted rename basename: %s", name)
	}
	return base, nil
}

func openPinnedRootDirectory(root *os.Root) (*os.File, error) {
	expected, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	opened, err := directory.Stat()
	if err != nil || !expected.IsDir() || !opened.IsDir() || !os.SameFile(expected, opened) {
		_ = directory.Close()
		return nil, errors.Join(errors.New("pinned rooted rename parent changed while its directory descriptor was opened"), err)
	}
	return directory, nil
}

// ProbeRenameRootNoReplace exercises the actual filesystem beneath root with a
// private source and an absent destination. Unlike a same-entry rename, this
// reaches the mounted filesystem's real destination-no-replace implementation.
// Probe entries are removed only while their identity still matches the entry
// created here; raced or otherwise unverified residue is retained and named in
// the returned error.
func ProbeRenameRootNoReplace(root *os.Root) (returnErr error) {
	if root == nil {
		return errors.New("root is required")
	}
	if !RenameRootNoReplaceSupported() {
		return ErrRenameNoReplaceUnsupported
	}
	var token [12]byte
	_, _ = rand.Read(token[:])
	suffix := hex.EncodeToString(token[:])
	sourceName := ".threadpoint-noreplace-probe-source-" + suffix
	destinationName := ".threadpoint-noreplace-probe-destination-" + suffix
	source, err := root.OpenFile(sourceName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create no-replace probe source beneath %s: %w", root.Name(), err)
	}
	// Keep the created inode open until identity-bound cleanup is complete. If
	// a concurrent actor unlinks and recreates the probe pathname, the open
	// handle prevents the original inode number from being recycled and
	// falsely satisfying os.SameFile below.
	defer func() {
		returnErr = errors.Join(returnErr, source.Close())
	}()
	sourceInfo, statErr := source.Stat()
	if statErr != nil {
		return errors.Join(statErr, cleanupRootProbeEntry(root, sourceName, nil))
	}
	if _, err := io.WriteString(source, "threadpoint-no-replace-probe\n"); err != nil {
		return errors.Join(err, cleanupRootProbeEntry(root, sourceName, sourceInfo))
	}
	if err := source.Sync(); err != nil {
		return errors.Join(err, cleanupRootProbeEntry(root, sourceName, sourceInfo))
	}
	sourceInfo, statErr = source.Stat()
	if statErr != nil {
		return errors.Join(statErr, cleanupRootProbeEntry(root, sourceName, sourceInfo))
	}
	if _, err := root.Lstat(destinationName); err == nil {
		return errors.Join(fmt.Errorf("no-replace probe destination unexpectedly exists beneath %s", root.Name()), cleanupRootProbeEntry(root, sourceName, sourceInfo))
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, cleanupRootProbeEntry(root, sourceName, sourceInfo))
	}

	renameErr := probeRenameRootNoReplaceOperation(root, sourceName, destinationName)
	destinationInfo, destinationStatErr := root.Lstat(destinationName)
	sourceAfter, sourceStatErr := root.Lstat(sourceName)
	if errors.Is(sourceStatErr, os.ErrNotExist) {
		sourceStatErr = nil
		sourceAfter = nil
	}
	verified := renameErr == nil && destinationStatErr == nil && sourceStatErr == nil && sourceAfter == nil && sameProbeEntry(sourceInfo, destinationInfo)
	if !verified && renameErr == nil {
		renameErr = fmt.Errorf("no-replace probe result could not be identity-verified beneath %s", root.Name())
	}
	if destinationStatErr != nil && !errors.Is(destinationStatErr, os.ErrNotExist) {
		renameErr = errors.Join(renameErr, destinationStatErr)
	}
	if sourceStatErr != nil {
		renameErr = errors.Join(renameErr, sourceStatErr)
	}
	cleanupErr := errors.Join(
		cleanupRootProbeEntry(root, sourceName, sourceInfo),
		cleanupRootProbeEntry(root, destinationName, sourceInfo),
	)
	return errors.Join(renameErr, cleanupErr)
}

func sameProbeEntry(expected fs.FileInfo, current fs.FileInfo) bool {
	return expected != nil && current != nil && expected.Mode().Type() == current.Mode().Type() && os.SameFile(expected, current)
}

func cleanupRootProbeEntry(root *os.Root, name string, expected fs.FileInfo) error {
	current, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect retained no-replace probe residue %s: %w", filepath.Join(root.Name(), name), err)
	}
	if expected == nil || !sameProbeEntry(expected, current) {
		return fmt.Errorf("no-replace probe residue was retained after its identity changed: %s", filepath.Join(root.Name(), name))
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("no-replace probe residue was retained at %s: %w", filepath.Join(root.Name(), name), err)
	}
	return nil
}

func openPinnedRenameParent(root *os.Root, name string) (*os.File, string, error) {
	clean, err := cleanRootName(name)
	if err != nil {
		return nil, "", err
	}
	if clean == "." {
		return nil, "", errors.New("cannot rename the rooted directory itself")
	}
	parentName := filepath.Dir(clean)
	base := filepath.Base(clean)
	if base == "" || base == "." || base == ".." {
		return nil, "", fmt.Errorf("invalid rooted rename basename: %s", name)
	}
	if err := RejectRootSymlinkAncestors(root, parentName); err != nil {
		return nil, "", err
	}
	expected, err := root.Lstat(parentName)
	if err != nil {
		return nil, "", err
	}
	if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("rooted rename parent is not a real directory: %s", parentName)
	}
	parent, err := root.Open(parentName)
	if err != nil {
		return nil, "", err
	}
	opened, err := parent.Stat()
	if err != nil {
		_ = parent.Close()
		return nil, "", err
	}
	if !opened.IsDir() || !os.SameFile(expected, opened) {
		_ = parent.Close()
		return nil, "", fmt.Errorf("rooted rename parent changed while it was opened: %s", parentName)
	}
	if err := verifyPinnedRenameParent(root, parentName, parent); err != nil {
		_ = parent.Close()
		return nil, "", err
	}
	return parent, base, nil
}

func verifyPinnedRenameParent(root *os.Root, name string, pinned *os.File) error {
	if err := RejectRootSymlinkAncestors(root, name); err != nil {
		return err
	}
	current, err := root.Lstat(name)
	if err != nil {
		return err
	}
	opened, err := pinned.Stat()
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !opened.IsDir() || !os.SameFile(current, opened) {
		return fmt.Errorf("rooted rename parent changed during mutation: %s", name)
	}
	return nil
}

// RejectRootSymlinkAncestors rejects an existing symlink in name while using
// os.Root for containment and stable directory identity.
func RejectRootSymlinkAncestors(root *os.Root, name string) error {
	if root == nil {
		return errors.New("root is required")
	}
	clean, err := cleanRootName(name)
	if err != nil {
		return err
	}
	if clean == "." {
		return nil
	}
	current := "."
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlink %s: %w", name, ErrUnsafeSymlink)
		}
	}
	return nil
}

func verifyPinnedRootPath(root *os.Root, name string, pinned *os.Root) error {
	if err := RejectRootSymlinkAncestors(root, name); err != nil {
		return err
	}
	current, err := root.Stat(name)
	if err != nil {
		return err
	}
	opened, err := pinned.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(current, opened) {
		return fmt.Errorf("rooted directory %s changed during mutation", name)
	}
	return nil
}

// VerifyPinnedRootPath confirms that name beneath root still identifies the
// exact directory retained by pinned. Callers can carry a selected directory
// identity across multi-file reads without reopening an ambient ancestor.
func VerifyPinnedRootPath(root *os.Root, name string, pinned *os.Root) error {
	return verifyPinnedRootPath(root, name, pinned)
}

// AtomicWriteFileIfMissing writes body only when path does not already exist.
func AtomicWriteFileIfMissing(path string, body []byte, perm os.FileMode) (resultErr error) {
	dir, err := prepareAtomicTarget(path)
	if err != nil {
		return err
	}
	// #nosec G703 -- prepareAtomicTarget validated the caller-selected path and rejected symlink ancestors immediately above.
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := createPinnedAtomicTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanupPinnedAtomicTemp(tmp)) }()
	if err := tmp.file.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.file.Write(body); err != nil {
		return err
	}
	if err := tmp.file.Sync(); err != nil {
		return err
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return fmt.Errorf("missing-file atomic temporary changed before publish: %w", err)
	}
	if atomicTempBeforePublish != nil {
		atomicTempBeforePublish(tmp.path)
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return fmt.Errorf("missing-file atomic temporary changed before publish: %w", err)
	}
	if err := validateAtomicTarget(path); err != nil {
		return err
	}
	if err := linkFile(tmp.path, path); err != nil {
		if os.IsExist(err) {
			if err := RejectExistingSymlink(path); err != nil {
				return err
			}
			return nil
		}
		if linkRequiresDirectCreate(err) {
			return writeFileIfMissingDirect(path, body, perm)
		}
		return err
	}
	// #nosec G703 -- path was hard-linked from the pinned temporary file and is compared with that identity below.
	current, err := os.Lstat(path)
	pinned, pinErr := tmp.file.Stat()
	if err != nil || pinErr != nil || !current.Mode().IsRegular() || !os.SameFile(tmp.info, pinned) || !os.SameFile(tmp.info, current) {
		return errors.Join(errors.New("missing-file atomic target changed during publish"), err, pinErr)
	}
	return nil
}

// AtomicWriteFileIfMissingUsingUmask writes body only when path does not
// already exist, applying the process umask to a newly created file and any
// newly created parent directories. Callers should pass unmasked creation
// modes, normally 0o666 for files and 0o777 for directories.
func AtomicWriteFileIfMissingUsingUmask(path string, body []byte, fileMode, dirMode os.FileMode) (resultErr error) {
	dir, err := prepareAtomicTargetWithMode(path, dirMode)
	if err != nil {
		return err
	}
	// #nosec G703 -- prepareAtomicTargetWithMode validated the caller-selected path and rejected symlink ancestors immediately above.
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := createPinnedAtomicTempWithMode(dir, "."+filepath.Base(path)+".tmp-", fileMode)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanupPinnedAtomicTemp(tmp)) }()
	if _, err := tmp.file.Write(body); err != nil {
		return err
	}
	if err := tmp.file.Sync(); err != nil {
		return err
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return fmt.Errorf("missing-file atomic temporary changed before publish: %w", err)
	}
	if atomicTempBeforePublish != nil {
		atomicTempBeforePublish(tmp.path)
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return fmt.Errorf("missing-file atomic temporary changed before publish: %w", err)
	}
	if err := validateAtomicTarget(path); err != nil {
		return err
	}
	if err := linkFile(tmp.path, path); err != nil {
		if os.IsExist(err) {
			if err := RejectExistingSymlink(path); err != nil {
				return err
			}
			return nil
		}
		if linkRequiresDirectCreate(err) {
			return writeFileIfMissingDirectUsingUmask(path, body, fileMode)
		}
		return err
	}
	// #nosec G703 -- path was hard-linked from the pinned temporary file and is compared with that identity below.
	current, err := os.Lstat(path)
	pinned, pinErr := tmp.file.Stat()
	if err != nil || pinErr != nil || !current.Mode().IsRegular() || !os.SameFile(tmp.info, pinned) || !os.SameFile(tmp.info, current) {
		return errors.Join(errors.New("missing-file atomic target changed during publish"), err, pinErr)
	}
	return nil
}

func linkRequiresDirectCreate(err error) bool {
	if os.IsPermission(err) || errors.Is(err, syscall.EXDEV) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "operation not supported") || strings.Contains(lower, "not supported")
}

func writeFileIfMissingDirect(path string, body []byte, perm os.FileMode) error {
	return writeFileIfMissingDirectWithMode(path, body, perm, true)
}

func writeFileIfMissingDirectUsingUmask(path string, body []byte, mode os.FileMode) error {
	return writeFileIfMissingDirectWithMode(path, body, mode, false)
}

func writeFileIfMissingDirectWithMode(path string, body []byte, mode os.FileMode, setFinalMode bool) error {
	if err := validateAtomicTarget(path); err != nil {
		return err
	}
	// #nosec G304 G703 -- direct missing-file write targets caller-selected paths after symlink validation and uses O_EXCL.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if os.IsExist(err) {
			if err := RejectExistingSymlink(path); err != nil {
				return err
			}
			return nil
		}
		return err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(errors.New("direct missing-file identity could not be established"), err, file.Close())
	}
	created := &pinnedAtomicTemp{file: file, path: path, info: info}
	fail := func(cause error) error {
		return errors.Join(cause, cleanupPinnedAtomicTemp(created))
	}
	if writeFileIfMissingAfterCreate != nil {
		if err := writeFileIfMissingAfterCreate(path, file); err != nil {
			return fail(err)
		}
	}
	if setFinalMode {
		if err := file.Chmod(mode); err != nil {
			return fail(err)
		}
	}
	if _, err := file.Write(body); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := verifyPinnedAtomicPath(created, path); err != nil {
		return fail(fmt.Errorf("direct missing-file target changed before completion: %w", err))
	}
	if err := file.Close(); err != nil {
		return fail(err)
	}
	created.file = nil
	return nil
}

// AtomicCopyFile safely copies source to target by replacing the target atomically.
func AtomicCopyFile(source string, target string, perm os.FileMode) (resultErr error) {
	before, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to copy symlink %s: %w", source, ErrUnsafeSymlink)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("refusing to copy non-regular source %s", source)
	}
	if atomicCopyBeforeSourceOpen != nil {
		atomicCopyBeforeSourceOpen(source)
	}
	// The platform helper uses nonblocking, no-follow flags where available so
	// a raced FIFO or final-component symlink cannot block or redirect the copy.
	in, err := openAtomicCopySource(source)
	if err != nil {
		return err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return fmt.Errorf("atomic copy source changed before it was opened: %s", source)
	}
	dir, err := prepareAtomicTarget(target)
	if err != nil {
		return err
	}
	tmp, err := createPinnedAtomicTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanupPinnedAtomicTemp(tmp)) }()
	if err := tmp.file.Chmod(perm); err != nil {
		return err
	}
	readLimit := opened.Size()
	if readLimit < int64(^uint64(0)>>1) {
		readLimit++
	}
	written, err := io.Copy(tmp.file, io.LimitReader(in, readLimit))
	if err != nil {
		return err
	}
	if written != opened.Size() {
		return fmt.Errorf("atomic copy source changed while it was read: %s", source)
	}
	after, err := in.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(source)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, after) || !os.SameFile(opened, current) ||
		opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return errors.Join(fmt.Errorf("atomic copy source changed while it was read: %s", source), err)
	}
	if err := tmp.file.Sync(); err != nil {
		return err
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return fmt.Errorf("atomic copy temporary changed before publish: %w", err)
	}
	if atomicTempBeforePublish != nil {
		atomicTempBeforePublish(tmp.path)
	}
	if err := verifyPinnedAtomicPath(tmp, tmp.path); err != nil {
		return fmt.Errorf("atomic copy temporary changed before publish: %w", err)
	}
	if err := validateAtomicTarget(target); err != nil {
		return err
	}
	// #nosec G703 -- target passed symlink checks and tmp remains bound to its pinned identity immediately before publication.
	if err := os.Rename(tmp.path, target); err != nil {
		return err
	}
	// #nosec G703 -- target was validated immediately before publish and is compared with the pinned temporary identity below.
	currentTarget, err := os.Lstat(target)
	pinnedTarget, pinErr := tmp.file.Stat()
	if err != nil || pinErr != nil || !currentTarget.Mode().IsRegular() || !os.SameFile(tmp.info, pinnedTarget) || !os.SameFile(tmp.info, currentTarget) {
		return errors.Join(errors.New("atomic copy target changed during publish"), err, pinErr)
	}
	return nil
}

func prepareAtomicTarget(path string) (string, error) {
	return prepareAtomicTargetWithMode(path, 0o755)
}

func prepareAtomicTargetWithMode(path string, dirMode os.FileMode) (string, error) {
	dir := filepath.Dir(path)
	if err := RejectExistingSymlinkAncestors(dir); err != nil {
		return "", err
	}
	// #nosec G301 G703 -- safefs creates user-visible parent directories only after rejecting existing symlink ancestors, then rechecks the created directory below.
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", err
	}
	if err := RejectExistingSymlink(dir); err != nil {
		return "", err
	}
	if err := RejectExistingSymlink(path); err != nil {
		return "", err
	}
	return dir, nil
}

func validateAtomicTarget(path string) error {
	if err := RejectExistingSymlinkAncestors(filepath.Dir(path)); err != nil {
		return err
	}
	return RejectExistingSymlink(path)
}

// RejectExistingSymlinkAncestors rejects path when it or any existing parent is
// a symlink. It must be called before creating parent directories, because
// MkdirAll follows symlinks it encounters.
func RejectExistingSymlinkAncestors(path string) error {
	current := filepath.Clean(path)
	for {
		if err := RejectExistingSymlink(current); err != nil {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// RejectExistingSymlink rejects path when it already exists as a symlink.
func RejectExistingSymlink(path string) error {
	// #nosec G703 -- this validation helper intentionally inspects the caller-selected path without following its final symlink.
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write through symlink %s: %w", path, ErrUnsafeSymlink)
	}
	return nil
}

// ResolveRootForMutation resolves symlinks that are part of the caller-selected
// project root while preserving any not-yet-created suffix. Mutating code can
// then apply the ordinary no-symlink-ancestor checks below that trusted root
// without treating an explicitly selected logical root as repository content.
func ResolveRootForMutation(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("root is required")
	}
	absolute, err := abspath.Abs(root)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	var suffix []string
	for {
		_, err := os.Lstat(current)
		switch {
		case err == nil:
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(absolute), nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func normalizedLockRoots(roots []string) ([]string, error) {
	selections, err := normalizedLockRootSelections(roots)
	if err != nil {
		return nil, err
	}
	return physicalLockRoots(selections), nil
}

func normalizedLockRootSelections(roots []string) ([]lockRootSelection, error) {
	seen := map[string]bool{}
	var selections []lockRootSelection
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		abs, err := abspath.Abs(root)
		if err != nil {
			return nil, err
		}
		resolved, err := ResolveRootForMutation(abs)
		if err != nil {
			return nil, err
		}
		logical := filepath.Clean(abs)
		if !seen[logical] {
			seen[logical] = true
			selections = append(selections, lockRootSelection{logicalPath: logical, physicalPath: filepath.Clean(resolved)})
		}
	}
	sort.Slice(selections, func(i, j int) bool { return selections[i].logicalPath < selections[j].logicalPath })
	return selections, nil
}

func physicalLockRoots(selections []lockRootSelection) []string {
	seen := map[string]bool{}
	normalized := make([]string, 0, len(selections))
	for _, selection := range selections {
		if !seen[selection.physicalPath] {
			seen[selection.physicalPath] = true
			normalized = append(normalized, selection.physicalPath)
		}
	}
	sort.Strings(normalized)
	return normalized
}

func lockName(root string) string {
	sum := sha256.Sum256([]byte(filepath.ToSlash(root)))
	return hex.EncodeToString(sum[:])
}

func marshalLockOwner(owner lockOwner) ([]byte, error) {
	body, err := json.MarshalIndent(owner, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func newLockToken() string {
	var token [16]byte
	_, _ = rand.Read(token[:])
	return hex.EncodeToString(token[:])
}

func metadataIsPastGrace(modTime time.Time, now time.Time) bool {
	return !modTime.IsZero() && now.Sub(modTime) >= lockMetadataGrace
}

func releaseLock(lock heldLock) error {
	if lock.lock == nil {
		return nil
	}
	return lock.lock.Release()
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests for existence without delivering a signal: nil means the
	// process exists, EPERM means it exists but is owned by another user, and
	// ESRCH means there is no such process.
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func ownerSuffix(body []byte) string {
	var owner lockOwner
	if err := json.Unmarshal(body, &owner); err != nil {
		return ""
	}
	parts := []string{}
	if owner.Command != "" {
		parts = append(parts, "command "+owner.Command)
	}
	if owner.PID != 0 {
		parts = append(parts, fmt.Sprintf("pid %d", owner.PID))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
