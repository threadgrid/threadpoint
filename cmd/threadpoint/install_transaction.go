// SPDX-License-Identifier: Apache-2.0

package main

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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	tpbackup "github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

const (
	updateOperationUninstall updateOperation = "uninstall"
	updateOperationInstall   updateOperation = "install"

	installTransactionSchemaVersion          = "threadpoint.install_transaction.v1"
	installTransactionMetadataBackup         = "install-metadata.json"
	installTransactionChecksumManifest       = "checksums.txt"
	maxInstallTransactionBytes         int64 = 256 << 10
	maxInstallManagedFileBytes         int64 = 256 << 20
	installGenerationKindAbsent              = "absent"
	installGenerationKindRegular             = "regular"
	installGenerationKindSymlink             = "symlink"
	installJournalFieldProduced              = "produced"
	installJournalFieldRecovery              = "recovery"
	installJournalActionUpdateFrom           = "update-from"
	installJournalTargetLink                 = "link"
	installManagedBinaryEntry                = "bin/threadpoint"
)

var (
	installLockHeartbeatInterval          = 5 * time.Second
	installLockLease                      = 30 * time.Second
	syncInstallJournalRoot                = syncInstallRootDirectory
	installTransactionAfterBackupRootOpen func()
	installRemovalBeforeQuarantine        func(*os.Root, string)
	installLifecycleBeforeRelease         func()
)

var errInstallRecoveryOwnershipMismatch = errors.New("install recovery found a replacement outside the saved managed generation")

type installJournalPublicationSyncError struct {
	err error
}

func (err *installJournalPublicationSyncError) Error() string {
	return err.err.Error()
}

func (err *installJournalPublicationSyncError) Unwrap() error {
	return err.err
}

// installJournalRewritePublishedError reports a failure after the replacement
// journal has become the visible canonical generation. Its durability may be
// ambiguous, so callers must retain the matching in-memory generation while
// the displaced exact prior journal remains available for crash recovery.
type installJournalRewritePublishedError struct {
	err error
}

func (err *installJournalRewritePublishedError) Error() string { return err.err.Error() }
func (err *installJournalRewritePublishedError) Unwrap() error { return err.err }

// installEntryRemovalCommittedError reports a failure after the exact reviewed
// generation has already been removed from its detached quarantine. Callers
// finalizing a journal must treat that unlink as the commit point and retain
// recovery state for the crash-ambiguous directory durability outcome.
type installEntryRemovalCommittedError struct {
	err error
}

func (err *installEntryRemovalCommittedError) Error() string { return err.err.Error() }
func (err *installEntryRemovalCommittedError) Unwrap() error { return err.err }

type installLockOwner struct {
	PID       int       `json:"pid"`
	Created   time.Time `json:"created"`
	Heartbeat time.Time `json:"heartbeat"`
	Command   string    `json:"command"`
	Token     string    `json:"token"`
}

type installLifecycleLock struct {
	path                  string
	owner                 installLockOwner
	guard                 *safefs.KernelFileLock
	lock                  *safefs.FileLock
	file                  *os.File
	fileInfo              os.FileInfo
	anchorPath            string
	anchorLock            *safefs.FileLock
	anchorFile            *os.File
	anchorFileInfo        os.FileInfo
	lockParent            *os.Root
	lockParentPath        string
	lockParentInfo        os.FileInfo
	commandRoot           *os.Root
	commandRootPath       string
	commandRootInfo       os.FileInfo
	bundleRoot            *os.Root
	bundleRootPath        string
	bundleRootInfo        os.FileInfo
	transactionsRoot      *os.Root
	transactionsRootPath  string
	transactionsRootInfo  os.FileInfo
	transactionBackup     *pinnedInstallTransactionBackup
	activeTransaction     *preparedInstallTransaction
	recoveringTransaction bool
	stop                  chan struct{}
	done                  chan struct{}
	mu                    sync.Mutex
	heartbeatErr          error
	releaseOnce           sync.Once
}

type installTransactionJournal struct {
	SchemaVersion   string                          `json:"schema_version"`
	Operation       updateOperation                 `json:"operation"`
	CommandPath     string                          `json:"command_path"`
	MetadataPath    string                          `json:"metadata_path"`
	BundleRoot      string                          `json:"bundle_root"`
	BackupRoot      string                          `json:"backup_root"`
	PreviousInstall bool                            `json:"previous_install"`
	LinkExisted     bool                            `json:"link_existed"`
	CreatedAt       time.Time                       `json:"created_at"`
	Targets         map[string]installJournalTarget `json:"targets"`
}

type installJournalGeneration struct {
	Kind       string `json:"kind"`
	Identity   string `json:"identity,omitempty"`
	Digest     string `json:"digest,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
}

type installJournalTarget struct {
	Prior    installJournalGeneration `json:"prior"`
	Produced installJournalGeneration `json:"produced,omitzero"`
	Recovery installJournalGeneration `json:"recovery,omitzero"`
}

type preparedInstallTransaction struct {
	journalPath     string
	journal         installTransactionJournal
	journalIdentity installRemovalIdentity
}

type pinnedInstallTransactionBackup struct {
	path             string
	base             string
	root             *os.Root
	info             os.FileInfo
	transactionsRoot *os.Root
	transactionsInfo os.FileInfo
}

type pinnedSelfUpdateWorkspace struct {
	path   string
	base   string
	parent *os.Root
	root   *os.Root
	info   os.FileInfo
}

func installLifecycleJournalPath(productHome, commandPath string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(commandPath)))
	return filepath.Join(productHome, "installs", hex.EncodeToString(sum[:])+".transaction.json")
}

func installLifecycleLockPath(productHome, commandPath string) string {
	commandPath = filepath.Clean(commandPath)
	sum := sha256.Sum256([]byte(commandPath))
	return filepath.Join(productHome, "installs", "."+filepath.Base(commandPath)+".install-"+hex.EncodeToString(sum[:])+".lock")
}

func installLifecycleGuardPath(_ string, commandPath string) string {
	commandPath = filepath.Clean(commandPath)
	sum := sha256.Sum256([]byte(commandPath))
	return filepath.Join(filepath.Dir(commandPath), "."+filepath.Base(commandPath)+".install-"+hex.EncodeToString(sum[:])+".guard")
}

func installLifecycleAnchorPath(commandPath string) string {
	commandPath = filepath.Clean(commandPath)
	sum := sha256.Sum256([]byte(commandPath))
	return filepath.Join(filepath.Dir(commandPath), "."+filepath.Base(commandPath)+".install-anchor-"+hex.EncodeToString(sum[:])+".lock")
}

// Keep owner-handle acquisition injectable so tests can verify that every
// earlier lock is released when retaining either file fails.
var openInstallLockOwnerFile = func(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDWR, 0)
}

func acquireInstallLifecycleLock(ctx context.Context, productHome, commandPath string) (*installLifecycleLock, error) {
	var token [16]byte
	_, _ = rand.Read(token[:])
	now := time.Now().UTC()
	owner := installLockOwner{
		PID: os.Getpid(), Created: now, Heartbeat: now,
		Command: filepath.Clean(commandPath), Token: hex.EncodeToString(token[:]),
	}
	body, err := json.Marshal(owner)
	if err != nil {
		return nil, err
	}
	path := installLifecycleLockPath(productHome, commandPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lockParent, lockParentInfo, err := pinInstallDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	commandRoot, commandRootInfo, err := pinInstallDirectory(filepath.Dir(commandPath))
	if err != nil {
		_ = lockParent.Close()
		return nil, err
	}
	guardPath := installLifecycleGuardPath(productHome, commandPath)
	guard, err := safefs.AcquireKernelFileLockRoot(ctx, commandRoot, filepath.Base(guardPath), guardPath, 30*time.Second, 50*time.Millisecond)
	if err != nil {
		_ = commandRoot.Close()
		_ = lockParent.Close()
		return nil, err
	}
	lockOptions := safefs.FileLockOptions{
		Path: path, Body: body, DirMode: 0o700, FileMode: 0o600,
		WaitTimeout: 30 * time.Second, PollInterval: 50 * time.Millisecond,
		StaleAfter: installLockLease, StalePolicy: installLifecycleStalePolicy,
		ReleaseMatches: func(current []byte) bool { return installLockBodyHasToken(current, owner.Token) },
	}
	fileLock, err := safefs.AcquireFileLockRoot(ctx, lockParent, filepath.Base(path), lockOptions)
	if err != nil {
		_ = guard.Release()
		_ = commandRoot.Close()
		_ = lockParent.Close()
		return nil, err
	}
	anchorPath := installLifecycleAnchorPath(commandPath)
	anchorOptions := lockOptions
	anchorOptions.Path = anchorPath
	anchorOptions.WaitTimeout = 5 * time.Second
	anchorLock, err := safefs.AcquireFileLockRoot(ctx, commandRoot, filepath.Base(anchorPath), anchorOptions)
	if err != nil {
		_ = fileLock.Release()
		_ = guard.Release()
		_ = commandRoot.Close()
		_ = lockParent.Close()
		return nil, err
	}
	file, err := openInstallLockOwnerFile(lockParent, filepath.Base(path))
	if err != nil {
		_ = anchorLock.Release()
		_ = fileLock.Release()
		_ = guard.Release()
		_ = commandRoot.Close()
		_ = lockParent.Close()
		return nil, err
	}
	anchorFile, err := openInstallLockOwnerFile(commandRoot, filepath.Base(anchorPath))
	if err != nil {
		_ = file.Close()
		_ = anchorLock.Release()
		_ = fileLock.Release()
		_ = guard.Release()
		_ = commandRoot.Close()
		_ = lockParent.Close()
		return nil, err
	}
	info, err := file.Stat()
	current, currentErr := lockParent.Lstat(filepath.Base(path))
	if err != nil || currentErr != nil || !info.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		_ = anchorFile.Close()
		_ = file.Close()
		_ = anchorLock.Release()
		_ = fileLock.Release()
		_ = guard.Release()
		_ = commandRoot.Close()
		_ = lockParent.Close()
		return nil, errors.New("install lifecycle lock identity changed during acquisition")
	}
	anchorInfo, err := anchorFile.Stat()
	anchorCurrent, anchorCurrentErr := commandRoot.Lstat(filepath.Base(anchorPath))
	if err != nil || anchorCurrentErr != nil || !anchorInfo.Mode().IsRegular() || !anchorCurrent.Mode().IsRegular() || !os.SameFile(anchorInfo, anchorCurrent) {
		_ = anchorFile.Close()
		_ = file.Close()
		_ = anchorLock.Release()
		_ = fileLock.Release()
		_ = guard.Release()
		_ = commandRoot.Close()
		_ = lockParent.Close()
		return nil, errors.New("install lifecycle anchor identity changed during acquisition")
	}
	transactionsRoot, transactionsInfo, err := ensurePinnedInstallChildDirectory(lockParent, "transactions")
	if err != nil {
		_ = anchorFile.Close()
		_ = file.Close()
		_ = anchorLock.Release()
		_ = fileLock.Release()
		_ = guard.Release()
		_ = commandRoot.Close()
		_ = lockParent.Close()
		return nil, fmt.Errorf("could not retain install transactions root: %w", err)
	}
	lock := &installLifecycleLock{
		path: path, owner: owner, guard: guard, lock: fileLock, file: file, fileInfo: info,
		anchorPath: anchorPath, anchorLock: anchorLock, anchorFile: anchorFile, anchorFileInfo: anchorInfo,
		lockParent: lockParent, lockParentPath: filepath.Dir(path), lockParentInfo: lockParentInfo,
		commandRoot: commandRoot, commandRootPath: filepath.Dir(commandPath), commandRootInfo: commandRootInfo,
		transactionsRoot: transactionsRoot, transactionsRootPath: filepath.Join(filepath.Dir(path), "transactions"), transactionsRootInfo: transactionsInfo,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go lock.heartbeat()
	return lock, nil
}

func pinInstallDirectory(path string) (*os.Root, os.FileInfo, error) {
	path = filepath.Clean(path)
	current, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() {
		return nil, nil, fmt.Errorf("install lifecycle directory is not a physical directory: %s", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := root.Stat(".")
	after, afterErr := os.Lstat(path)
	if err != nil || afterErr != nil || !opened.IsDir() || !os.SameFile(current, opened) || !os.SameFile(opened, after) {
		_ = root.Close()
		return nil, nil, fmt.Errorf("install lifecycle directory identity changed while pinning: %s", path)
	}
	return root, opened, nil
}

func validatePinnedInstallDirectory(root *os.Root, path string, expected os.FileInfo) error {
	if root == nil || expected == nil {
		return errors.New("install lifecycle directory is not pinned")
	}
	opened, err := root.Stat(".")
	current, currentErr := os.Lstat(path)
	if err != nil || currentErr != nil || !opened.IsDir() || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, current) {
		return fmt.Errorf("install lifecycle directory identity changed while held: %s", path)
	}
	return nil
}

func ensurePinnedInstallChildDirectory(parent *os.Root, name string) (*os.Root, os.FileInfo, error) {
	if parent == nil || filepath.Clean(name) != filepath.Base(name) || name == "." || name == ".." {
		return nil, nil, errors.New("install lifecycle child directory is invalid")
	}
	before, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, nil, err
		}
		if err := syncInstallRootDirectory(parent); err != nil {
			return nil, nil, err
		}
		before, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, nil, errors.New("install lifecycle child is not a physical directory")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	fail := func(cause error) (*os.Root, os.FileInfo, error) {
		return nil, nil, errors.Join(cause, root.Close())
	}
	opened, openErr := root.Stat(".")
	after, afterErr := parent.Lstat(name)
	if openErr != nil || afterErr != nil || !opened.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return fail(errors.Join(errors.New("install lifecycle child identity changed while opening"), openErr, afterErr))
	}
	directory, err := root.Open(".")
	if err != nil {
		return fail(err)
	}
	chmodErr := directory.Chmod(0o700)
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(chmodErr, syncErr, closeErr); err != nil {
		return fail(err)
	}
	parentMount, parentMountErr := installRootMountIdentity(parent)
	childMount, childMountErr := installRootMountIdentity(root)
	if parentMountErr != nil || childMountErr != nil || parentMount != childMount {
		return fail(errors.Join(errors.New("install lifecycle child crosses a mount boundary"), parentMountErr, childMountErr))
	}
	return root, opened, nil
}

func validatePinnedInstallChildDirectory(parent *os.Root, name string, root *os.Root, expected os.FileInfo) error {
	if parent == nil || root == nil || expected == nil {
		return errors.New("install lifecycle child directory is not pinned")
	}
	opened, openErr := root.Stat(".")
	current, currentErr := parent.Lstat(name)
	if openErr != nil || currentErr != nil || !opened.IsDir() || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, current) {
		return errors.Join(errors.New("install lifecycle child directory identity changed while held"), openErr, currentErr)
	}
	return nil
}

func createPinnedInstallTransactionBackup(lock *installLifecycleLock) (*pinnedInstallTransactionBackup, error) {
	if lock == nil || lock.lockParent == nil {
		return nil, errors.New("install lifecycle lock is required for a transaction backup")
	}
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	if err := lock.lockParent.Mkdir("transactions", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := syncInstallRootDirectory(lock.lockParent); err != nil {
		return nil, err
	}
	base, err := uniqueInstallEntryName(lock.lockParent, "transactions/.bundle-", "")
	if err != nil {
		return nil, err
	}
	// uniqueInstallEntryName returns a path beneath the lock root here. Keep
	// only the final component when operating through the pinned transactions
	// root below.
	base = filepath.Base(base)
	transactionsBefore, err := lock.lockParent.Lstat("transactions")
	if err != nil || !transactionsBefore.IsDir() || transactionsBefore.Mode()&os.ModeSymlink != 0 || transactionsBefore.Mode().Perm()&0o077 != 0 {
		return nil, errors.Join(errors.New("install transactions root is not an owner-private physical directory"), err)
	}
	transactionsRoot, err := lock.lockParent.OpenRoot("transactions")
	if err != nil {
		return nil, err
	}
	transactionsOpened, openErr := transactionsRoot.Stat(".")
	transactionsAfter, afterErr := lock.lockParent.Lstat("transactions")
	if openErr != nil || afterErr != nil || !transactionsOpened.IsDir() || !os.SameFile(transactionsBefore, transactionsOpened) || !os.SameFile(transactionsOpened, transactionsAfter) {
		_ = transactionsRoot.Close()
		return nil, errors.Join(errors.New("install transactions root identity changed while opening"), openErr, afterErr)
	}
	lockMount, mountErr := installRootMountIdentity(lock.lockParent)
	transactionsMount, transactionsMountErr := installRootMountIdentity(transactionsRoot)
	if mountErr != nil || transactionsMountErr != nil || lockMount != transactionsMount {
		_ = transactionsRoot.Close()
		return nil, errors.Join(errors.New("install transactions root crosses a mount boundary"), mountErr, transactionsMountErr)
	}
	if err := transactionsRoot.Mkdir(base, 0o700); err != nil {
		_ = transactionsRoot.Close()
		return nil, err
	}
	backupBefore, err := transactionsRoot.Lstat(base)
	if err != nil || !backupBefore.IsDir() || backupBefore.Mode()&os.ModeSymlink != 0 || backupBefore.Mode().Perm()&0o077 != 0 {
		_ = transactionsRoot.Close()
		return nil, errors.Join(errors.New("install transaction backup is not an owner-private physical directory; unbound residue retained"), err)
	}
	backupRoot, err := transactionsRoot.OpenRoot(base)
	if err != nil {
		_ = removePinnedInstallBackupEntryExact(transactionsRoot, base, backupBefore)
		_ = transactionsRoot.Close()
		return nil, err
	}
	backupOpened, openErr := backupRoot.Stat(".")
	backupAfter, afterErr := transactionsRoot.Lstat(base)
	backupMount, backupMountErr := installRootMountIdentity(backupRoot)
	if openErr != nil || afterErr != nil || backupMountErr != nil || !backupOpened.IsDir() ||
		!os.SameFile(backupBefore, backupOpened) || !os.SameFile(backupOpened, backupAfter) || backupMount != transactionsMount {
		_ = backupRoot.Close()
		_ = removePinnedInstallBackupEntryExact(transactionsRoot, base, backupBefore)
		_ = transactionsRoot.Close()
		return nil, errors.Join(errors.New("install transaction backup identity changed while opening or crosses a mount boundary"), openErr, afterErr, backupMountErr)
	}
	if err := syncInstallRootDirectory(transactionsRoot); err != nil {
		_ = backupRoot.Close()
		_ = removePinnedInstallBackupEntryExact(transactionsRoot, base, backupBefore)
		_ = transactionsRoot.Close()
		return nil, err
	}
	backup := &pinnedInstallTransactionBackup{
		path: filepath.Join(lock.lockParentPath, "transactions", base), base: base,
		root: backupRoot, info: backupOpened,
		transactionsRoot: transactionsRoot, transactionsInfo: transactionsOpened,
	}
	if err := backup.Validate(lock); err != nil {
		_ = backup.Remove()
		return nil, err
	}
	return backup, nil
}

func openPinnedInstallTransactionBackup(lock *installLifecycleLock, path string) (*pinnedInstallTransactionBackup, error) {
	if lock == nil || lock.lockParent == nil {
		return nil, errors.New("install lifecycle lock is required for a transaction backup")
	}
	rel, err := filepath.Rel(filepath.Join(lock.lockParentPath, "transactions"), filepath.Clean(path))
	if err != nil || filepath.IsAbs(rel) || filepath.Dir(rel) != "." || !strings.HasPrefix(filepath.Base(rel), ".bundle-") {
		return nil, errors.Join(errors.New("install transaction backup is outside the pinned transactions namespace"), err)
	}
	lock.mu.Lock()
	retained := lock.transactionBackup
	lock.mu.Unlock()
	if retained != nil {
		if filepath.Clean(path) != retained.path {
			return nil, errors.New("install lifecycle lock retains a different transaction backup")
		}
		if err := retained.Validate(lock); err != nil {
			return nil, err
		}
		return retained, nil
	}
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	transactionsBefore, err := lock.lockParent.Lstat("transactions")
	if err != nil || !transactionsBefore.IsDir() || transactionsBefore.Mode()&os.ModeSymlink != 0 || transactionsBefore.Mode().Perm()&0o077 != 0 {
		return nil, errors.Join(errors.New("install transactions root is not an owner-private physical directory"), err)
	}
	transactionsRoot, err := lock.lockParent.OpenRoot("transactions")
	if err != nil {
		return nil, err
	}
	transactionsOpened, openErr := transactionsRoot.Stat(".")
	transactionsAfter, afterErr := lock.lockParent.Lstat("transactions")
	if openErr != nil || afterErr != nil || !os.SameFile(transactionsBefore, transactionsOpened) || !os.SameFile(transactionsOpened, transactionsAfter) {
		_ = transactionsRoot.Close()
		return nil, errors.Join(errors.New("install transactions root identity changed while opening"), openErr, afterErr)
	}
	base := filepath.Base(rel)
	backupBefore, err := transactionsRoot.Lstat(base)
	if err != nil || !backupBefore.IsDir() || backupBefore.Mode()&os.ModeSymlink != 0 || backupBefore.Mode().Perm()&0o077 != 0 {
		_ = transactionsRoot.Close()
		return nil, errors.Join(errors.New("install transaction backup is not an owner-private physical directory"), err)
	}
	backupRoot, err := transactionsRoot.OpenRoot(base)
	if err != nil {
		_ = transactionsRoot.Close()
		return nil, err
	}
	backupOpened, openErr := backupRoot.Stat(".")
	backupAfter, afterErr := transactionsRoot.Lstat(base)
	lockMount, lockMountErr := installRootMountIdentity(lock.lockParent)
	transactionsMount, transactionsMountErr := installRootMountIdentity(transactionsRoot)
	backupMount, backupMountErr := installRootMountIdentity(backupRoot)
	if openErr != nil || afterErr != nil || lockMountErr != nil || transactionsMountErr != nil || backupMountErr != nil ||
		!os.SameFile(backupBefore, backupOpened) || !os.SameFile(backupOpened, backupAfter) ||
		lockMount != transactionsMount || transactionsMount != backupMount {
		_ = backupRoot.Close()
		_ = transactionsRoot.Close()
		return nil, errors.Join(errors.New("install transaction backup identity changed while opening or crosses a mount boundary"), openErr, afterErr, lockMountErr, transactionsMountErr, backupMountErr)
	}
	backup := &pinnedInstallTransactionBackup{
		path: filepath.Clean(path), base: base, root: backupRoot, info: backupOpened,
		transactionsRoot: transactionsRoot, transactionsInfo: transactionsOpened,
	}
	if err := backup.Validate(lock); err != nil {
		_ = backup.Close()
		return nil, err
	}
	if err := lock.retainTransactionBackup(backup); err != nil {
		return nil, errors.Join(err, backup.Close())
	}
	return backup, nil
}

func (backup *pinnedInstallTransactionBackup) Validate(lock *installLifecycleLock) error {
	if backup == nil || backup.root == nil || backup.transactionsRoot == nil || backup.info == nil || backup.transactionsInfo == nil {
		return errors.New("install transaction backup is not pinned")
	}
	if err := lock.Validate(); err != nil {
		return err
	}
	return backup.validateNamespace(lock.lockParent)
}

func (backup *pinnedInstallTransactionBackup) validateNamespace(lockParent *os.Root) error {
	if backup == nil || backup.root == nil || backup.transactionsRoot == nil || backup.info == nil || backup.transactionsInfo == nil {
		return errors.New("install transaction backup is not pinned")
	}
	transactionsOpened, openErr := backup.transactionsRoot.Stat(".")
	transactionsCurrent, currentErr := lockParent.Lstat("transactions")
	backupOpened, backupOpenErr := backup.root.Stat(".")
	backupCurrent, backupCurrentErr := backup.transactionsRoot.Lstat(backup.base)
	if openErr != nil || currentErr != nil || backupOpenErr != nil || backupCurrentErr != nil ||
		!transactionsOpened.IsDir() || !backupOpened.IsDir() ||
		!os.SameFile(backup.transactionsInfo, transactionsOpened) || !os.SameFile(transactionsOpened, transactionsCurrent) ||
		!os.SameFile(backup.info, backupOpened) || !os.SameFile(backupOpened, backupCurrent) {
		return errors.Join(errors.New("install transaction backup namespace identity changed while held"), openErr, currentErr, backupOpenErr, backupCurrentErr)
	}
	return nil
}

func (lock *installLifecycleLock) retainTransactionBackup(backup *pinnedInstallTransactionBackup) error {
	if lock == nil || backup == nil {
		return errors.New("install transaction backup retention requires a lifecycle lock")
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.transactionBackup != nil && lock.transactionBackup != backup {
		return errors.New("install lifecycle lock already retains another transaction backup")
	}
	lock.transactionBackup = backup
	return nil
}

func (lock *installLifecycleLock) clearTransactionBackup(backup *pinnedInstallTransactionBackup, closeBackup bool) error {
	if lock == nil || backup == nil {
		return nil
	}
	lock.mu.Lock()
	if lock.transactionBackup != nil && lock.transactionBackup != backup {
		lock.mu.Unlock()
		return errors.New("refusing to clear a different retained install transaction backup")
	}
	lock.transactionBackup = nil
	lock.mu.Unlock()
	if closeBackup {
		return backup.Close()
	}
	return nil
}

func (backup *pinnedInstallTransactionBackup) Close() error {
	if backup == nil {
		return nil
	}
	var rootErr, transactionsErr error
	if backup.root != nil {
		rootErr = backup.root.Close()
		backup.root = nil
	}
	if backup.transactionsRoot != nil {
		transactionsErr = backup.transactionsRoot.Close()
		backup.transactionsRoot = nil
	}
	return errors.Join(rootErr, transactionsErr)
}

func (backup *pinnedInstallTransactionBackup) Remove() error {
	if backup == nil {
		return nil
	}
	if backup.root == nil || backup.transactionsRoot == nil {
		return errors.New("install transaction backup is not open for removal")
	}
	cleanupErr := removePinnedInstallTransactionContents(backup.root)
	closeErr := backup.root.Close()
	backup.root = nil
	if cleanupErr != nil || closeErr != nil {
		return errors.Join(cleanupErr, closeErr, backup.transactionsRoot.Close())
	}
	current, err := backup.transactionsRoot.Lstat(backup.base)
	if err != nil || !os.SameFile(backup.info, current) {
		return errors.Join(errors.New("install transaction backup changed after cleanup"), err, backup.transactionsRoot.Close())
	}
	removeErr := removePinnedInstallBackupEntryExact(backup.transactionsRoot, backup.base, backup.info)
	transactionsCloseErr := backup.transactionsRoot.Close()
	backup.transactionsRoot = nil
	return errors.Join(removeErr, transactionsCloseErr)
}

func installLockBodyHasToken(body []byte, token string) bool {
	var owner installLockOwner
	return json.Unmarshal(body, &owner) == nil && owner.Token == token
}

func installLifecycleStalePolicy(snapshot safefs.FileLockSnapshot, now time.Time, staleAfter time.Duration) (bool, error) {
	var owner installLockOwner
	if err := json.Unmarshal(snapshot.Body, &owner); err != nil || owner.PID <= 0 || owner.Token == "" {
		return safefs.DefaultFileLockStalePolicy(snapshot, now, staleAfter)
	}
	if !installOwnerProcessAlive(owner.PID) {
		return true, nil
	}
	lastSeen := owner.Heartbeat
	if lastSeen.IsZero() {
		lastSeen = owner.Created
	}
	if snapshot.ModTime.After(lastSeen) {
		lastSeen = snapshot.ModTime
	}
	return lastSeen.IsZero() || now.Sub(lastSeen) >= staleAfter, nil
}

func installOwnerProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (lock *installLifecycleLock) heartbeat() {
	ticker := time.NewTicker(installLockHeartbeatInterval)
	defer ticker.Stop()
	defer close(lock.done)
	for {
		select {
		case <-lock.stop:
			return
		case now := <-ticker.C:
			lock.mu.Lock()
			if lock.heartbeatErr == nil {
				lock.owner.Heartbeat = now.UTC()
				body, err := json.Marshal(lock.owner)
				if err == nil {
					err = rewriteInstallLockFile(lock.file, body)
				}
				if err == nil {
					err = rewriteInstallLockFile(lock.anchorFile, body)
				}
				lock.heartbeatErr = err
			}
			lock.mu.Unlock()
		}
	}
}

func rewriteInstallLockFile(file *os.File, body []byte) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	return file.Sync()
}

func (lock *installLifecycleLock) Validate() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.heartbeatErr != nil {
		return fmt.Errorf("install lifecycle lock heartbeat failed: %w", lock.heartbeatErr)
	}
	if err := lock.guard.Validate(); err != nil {
		return fmt.Errorf("install lifecycle kernel guard changed while held: %w", err)
	}
	if err := validatePinnedInstallDirectory(lock.lockParent, lock.lockParentPath, lock.lockParentInfo); err != nil {
		return err
	}
	if err := validatePinnedInstallDirectory(lock.commandRoot, lock.commandRootPath, lock.commandRootInfo); err != nil {
		return err
	}
	if err := validatePinnedInstallChildDirectory(lock.lockParent, "transactions", lock.transactionsRoot, lock.transactionsRootInfo); err != nil {
		return err
	}
	if lock.transactionBackup != nil {
		if err := lock.transactionBackup.validateNamespace(lock.lockParent); err != nil {
			return fmt.Errorf("install transaction backup changed while held: %w", err)
		}
	}
	if lock.bundleRoot != nil {
		if err := validatePinnedInstallDirectory(lock.bundleRoot, lock.bundleRootPath, lock.bundleRootInfo); err != nil {
			return err
		}
	}
	opened, err := lock.file.Stat()
	current, currentErr := lock.lockParent.Lstat(filepath.Base(lock.path))
	if err != nil || currentErr != nil || !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(lock.fileInfo, opened) || !os.SameFile(opened, current) {
		return errors.New("install lifecycle lock identity changed while held")
	}
	anchorOpened, err := lock.anchorFile.Stat()
	if err != nil {
		return err
	}
	anchorCurrent, err := lock.commandRoot.Lstat(filepath.Base(lock.anchorPath))
	if err != nil || !anchorOpened.Mode().IsRegular() || !anchorCurrent.Mode().IsRegular() ||
		!os.SameFile(lock.anchorFileInfo, anchorOpened) || !os.SameFile(anchorOpened, anchorCurrent) {
		return errors.New("install lifecycle anchor identity changed while held")
	}
	return nil
}

func (lock *installLifecycleLock) PinBundleRoot(path string) error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	path = filepath.Clean(path)
	if lock.bundleRoot != nil {
		if lock.bundleRootPath != path {
			return fmt.Errorf("install lifecycle already owns a different bundle root: %s", lock.bundleRootPath)
		}
		return validatePinnedInstallDirectory(lock.bundleRoot, lock.bundleRootPath, lock.bundleRootInfo)
	}
	root, info, err := pinInstallDirectory(path)
	if err != nil {
		return err
	}
	lock.bundleRoot = root
	lock.bundleRootPath = path
	lock.bundleRootInfo = info
	return nil
}

func (lock *installLifecycleLock) Release() (returnErr error) {
	if lock == nil {
		return nil
	}
	lock.releaseOnce.Do(func() {
		close(lock.stop)
		<-lock.done
		lock.mu.Lock()
		if installLifecycleBeforeRelease != nil {
			installLifecycleBeforeRelease()
		}
		pinnedReleaseErr := removeRetainedInstallLock(lock.lockParent, lock.file, lock.path, lock.fileInfo)
		anchorReleaseErr := removeRetainedInstallLock(lock.commandRoot, lock.anchorFile, lock.anchorPath, lock.anchorFileInfo)
		fileErr := errors.Join(lock.file.Close(), lock.anchorFile.Close())
		fileLockErr := errors.Join(lock.anchorLock.Release(), lock.lock.Release())
		guardErr := lock.guard.Release()
		var bundleErr error
		if lock.bundleRoot != nil {
			bundleErr = lock.bundleRoot.Close()
		}
		var backupErr error
		if lock.transactionBackup != nil {
			backupErr = lock.transactionBackup.Close()
			lock.transactionBackup = nil
		}
		transactionsErr := lock.transactionsRoot.Close()
		commandErr := lock.commandRoot.Close()
		parentErr := lock.lockParent.Close()
		lock.mu.Unlock()
		returnErr = errors.Join(pinnedReleaseErr, anchorReleaseErr, fileErr, fileLockErr, guardErr, bundleErr, backupErr, transactionsErr, commandErr, parentErr)
	})
	return returnErr
}

func removeRetainedInstallLock(root *os.Root, file *os.File, path string, expected os.FileInfo) error {
	base := filepath.Base(path)
	current, currentErr := snapshotRegularInstallRemoval(root, base)
	switch {
	case errors.Is(currentErr, os.ErrNotExist):
		return nil
	case currentErr != nil:
		return currentErr
	}
	if file == nil || expected == nil {
		return errors.New("retained install lifecycle lock identity is required")
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(expected, opened) || !os.SameFile(opened, current.info) {
		return errors.Join(statErr, errors.New("refusing to release a replaced install lifecycle lock"))
	}
	return quarantineAndRemoveInstallEntry(root, base, current, nil)
}

func prepareInstallTransaction(productHome, commandPath string, operation updateOperation, metadataPath string, metadata installMetadata, lock *installLifecycleLock) (*preparedInstallTransaction, error) {
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	pinnedMetadata, metadataBody, err := readInstallMetadataForTransaction(lock, metadataPath)
	if err != nil {
		return nil, err
	}
	providedBody, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	pinnedBody, err := json.Marshal(pinnedMetadata)
	if err != nil {
		return nil, err
	}
	if string(providedBody) != string(pinnedBody) {
		return nil, errors.New("installer metadata changed before transaction preparation")
	}
	metadata = pinnedMetadata
	if operation != updateOperationInstall && operation != updateOperationUpdate && operation != updateOperationRollback && operation != updateOperationUninstall {
		return nil, fmt.Errorf("unsupported install transaction operation %q", operation)
	}
	if err := verifyInstallOwnership(metadata, metadata.BinaryPath); err != nil {
		return nil, err
	}
	if filepath.Clean(metadata.LinkPath) != filepath.Clean(commandPath) || installerMetadataPath(productHome, metadata.BinaryPath) != filepath.Clean(metadataPath) {
		return nil, errors.New("refusing install transaction with mismatched managed identity")
	}
	if err := lock.PinBundleRoot(metadata.BundleRoot); err != nil {
		return nil, err
	}
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	journalPath := installLifecycleJournalPath(productHome, commandPath)
	if _, err := readInstallLockFileBounded(lock, journalPath, maxInstallTransactionBytes); err == nil {
		return nil, errors.New("an interrupted install transaction must be recovered first")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	backup, err := createPinnedInstallTransactionBackup(lock)
	if err != nil {
		return nil, err
	}
	if err := lock.retainTransactionBackup(backup); err != nil {
		return nil, errors.Join(err, backup.Remove())
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = lock.clearTransactionBackup(backup, false)
			if err := backup.Remove(); err != nil {
				_ = backup.Close()
			}
		}
	}()
	if installTransactionAfterBackupRootOpen != nil {
		installTransactionAfterBackupRootOpen()
	}
	if err := backup.Validate(lock); err != nil {
		return nil, err
	}
	backedUpEntries, err := backupBundleEntriesForOperationChecked(
		metadata.BundleRoot, backup.root, threadpointBundleEntries, operation, lock, backup,
	)
	if err != nil {
		return nil, err
	}
	if err := writeInstallBackupRootFile(backup.root, installTransactionMetadataBackup, bytes.NewReader(metadataBody)); err != nil {
		return nil, err
	}
	if err := writeInstallTransactionChecksumsForEntries(backup.root, backedUpEntries); err != nil {
		return nil, err
	}
	if err := syncPinnedInstallTransactionTree(backup.root); err != nil {
		return nil, err
	}
	if err := backup.Validate(lock); err != nil {
		return nil, err
	}
	_, linkExisted, err := verifiedInstallerLinkPath(metadata, metadata.BinaryPath)
	if err != nil {
		return nil, err
	}
	targets, err := snapshotInstallJournalTargets(metadata, metadataPath, operation, backedUpEntries, lock)
	if err != nil {
		return nil, err
	}
	journal := installTransactionJournal{
		SchemaVersion: installTransactionSchemaVersion, Operation: operation,
		CommandPath: filepath.Clean(commandPath), MetadataPath: filepath.Clean(metadataPath),
		BundleRoot: filepath.Clean(metadata.BundleRoot), BackupRoot: backup.path,
		PreviousInstall: true, LinkExisted: linkExisted, CreatedAt: time.Now().UTC(), Targets: targets,
	}
	if err := writeInstallTransactionJournalChecked(journalPath, journal, lock, func() error {
		return backup.Validate(lock)
	}); err != nil {
		var publicationSyncErr *installJournalPublicationSyncError
		if errors.As(err, &publicationSyncErr) {
			// The rename succeeded but its parent-directory sync did not. Retain
			// the exact backup because either the published or pre-publication
			// namespace can survive a crash.
			cleanup = false
		}
		return nil, err
	}
	// Once the journal is published, retain both it and the exact backup on any
	// namespace error. Removing either after a publication-boundary ambiguity
	// could make a resurrected journal unrecoverable.
	cleanup = false
	if err := backup.Validate(lock); err != nil {
		return nil, fmt.Errorf("install transaction backup namespace changed after journal publication: %w", err)
	}
	journalIdentity, err := snapshotRegularInstallRemoval(lock.lockParent, filepath.Base(journalPath))
	if err != nil {
		return nil, err
	}
	transaction := &preparedInstallTransaction{journalPath: journalPath, journal: journal, journalIdentity: journalIdentity}
	lock.activeTransaction = transaction
	return transaction, nil
}

func snapshotInstallJournalTargets(metadata installMetadata, metadataPath string, operation updateOperation, backedUpEntries []string, lock *installLifecycleLock) (map[string]installJournalTarget, error) {
	if lock == nil || lock.bundleRoot == nil || lock.transactionBackup == nil {
		return nil, errors.New("install transaction roots are not retained")
	}
	backedUp := make(map[string]bool, len(backedUpEntries))
	for _, entry := range backedUpEntries {
		backedUp[filepath.ToSlash(entry)] = true
	}
	targets := make(map[string]installJournalTarget, len(threadpointBundleEntries)+2)
	for _, entry := range threadpointBundleEntries {
		parent, err := openPinnedBundleParent(lock.bundleRoot, filepath.Dir(filepath.FromSlash(entry)))
		if errors.Is(err, os.ErrNotExist) && operation == updateOperationUninstall && entry == installManagedBinaryEntry && !backedUp[entry] {
			targets[installJournalBundleKey(entry)] = installJournalTarget{Prior: installJournalGeneration{Kind: installGenerationKindAbsent}}
			continue
		}
		if err != nil {
			return nil, err
		}
		identity, snapshotErr := snapshotRegularInstallRemoval(parent, filepath.Base(filepath.FromSlash(entry)))
		closeErr := parent.Close()
		if errors.Is(snapshotErr, os.ErrNotExist) && closeErr == nil && operation == updateOperationUninstall && entry == installManagedBinaryEntry && !backedUp[entry] {
			targets[installJournalBundleKey(entry)] = installJournalTarget{Prior: installJournalGeneration{Kind: installGenerationKindAbsent}}
			continue
		}
		if snapshotErr != nil || closeErr != nil {
			return nil, errors.Join(snapshotErr, closeErr)
		}
		if !backedUp[entry] {
			return nil, fmt.Errorf("managed bundle entry appeared after backup preparation: %s", entry)
		}
		if err := verifyInstallRemovalBackupDigest(lock, filepath.ToSlash(entry), identity.digest); err != nil {
			return nil, err
		}
		recorded, err := recordedInstallGeneration(identity)
		if err != nil {
			return nil, err
		}
		targets[installJournalBundleKey(entry)] = installJournalTarget{Prior: recorded}
	}
	metadataBase, err := installLockParentBase(lock, metadataPath)
	if err != nil {
		return nil, err
	}
	metadataIdentity, err := snapshotRegularInstallRemoval(lock.lockParent, metadataBase)
	if err != nil {
		return nil, err
	}
	if err := verifyInstallRemovalBackupDigest(lock, installTransactionMetadataBackup, metadataIdentity.digest); err != nil {
		return nil, err
	}
	recordedMetadata, err := recordedInstallGeneration(metadataIdentity)
	if err != nil {
		return nil, err
	}
	targets["metadata"] = installJournalTarget{Prior: recordedMetadata}
	if link, err := lock.commandRoot.Lstat(filepath.Base(metadata.LinkPath)); errors.Is(err, os.ErrNotExist) {
		targets[installJournalTargetLink] = installJournalTarget{Prior: installJournalGeneration{Kind: installGenerationKindAbsent}}
	} else if err != nil {
		return nil, err
	} else {
		_ = link
		identity, err := snapshotSymlinkInstallRemoval(lock.commandRoot, filepath.Base(metadata.LinkPath))
		if err != nil {
			return nil, err
		}
		recorded, err := recordedInstallGeneration(identity)
		if err != nil {
			return nil, err
		}
		targets[installJournalTargetLink] = installJournalTarget{Prior: recorded}
	}
	return targets, nil
}

func installJournalBundleKey(entry string) string {
	return "bundle:" + filepath.ToSlash(entry)
}

func recordProducedInstallGeneration(lock *installLifecycleLock, key string, identity installRemovalIdentity) error {
	recorded, err := recordedInstallGeneration(identity)
	if err != nil {
		return err
	}
	return recordProducedInstallJournalGeneration(lock, key, recorded)
}

func recordProducedAbsentInstallGeneration(lock *installLifecycleLock, key string) error {
	return recordProducedInstallJournalGeneration(lock, key, installJournalGeneration{Kind: installGenerationKindAbsent})
}

func recordRecoveryInstallGeneration(lock *installLifecycleLock, key string, identity installRemovalIdentity) error {
	if lock == nil || lock.activeTransaction == nil || !lock.recoveringTransaction {
		return nil
	}
	recorded, err := recordedInstallGeneration(identity)
	if err != nil {
		return err
	}
	transaction := lock.activeTransaction
	target, ok := transaction.journal.Targets[key]
	if !ok {
		return fmt.Errorf("install transaction journal does not own target %q", key)
	}
	previous := target.Recovery
	target.Recovery = recorded
	transaction.journal.Targets[key] = target
	if err := rewriteActiveInstallTransactionJournal(transaction, lock); err != nil {
		var publishedErr *installJournalRewritePublishedError
		if !errors.As(err, &publishedErr) {
			target.Recovery = previous
			transaction.journal.Targets[key] = target
		}
		return err
	}
	return nil
}

func recordProducedInstallJournalGeneration(lock *installLifecycleLock, key string, recorded installJournalGeneration) error {
	if lock == nil || lock.activeTransaction == nil || lock.recoveringTransaction {
		return nil
	}
	transaction := lock.activeTransaction
	target, ok := transaction.journal.Targets[key]
	if !ok {
		return fmt.Errorf("install transaction journal does not own target %q", key)
	}
	previous := target.Produced
	target.Produced = recorded
	transaction.journal.Targets[key] = target
	if err := rewriteActiveInstallTransactionJournal(transaction, lock); err != nil {
		var publishedErr *installJournalRewritePublishedError
		if !errors.As(err, &publishedErr) {
			target.Produced = previous
			transaction.journal.Targets[key] = target
		}
		return err
	}
	return nil
}

func recordRecoveredPriorInstallGeneration(lock *installLifecycleLock, key string, identity installRemovalIdentity) error {
	if lock == nil || lock.activeTransaction == nil || !lock.recoveringTransaction {
		return nil
	}
	recorded, err := recordedInstallGeneration(identity)
	if err != nil {
		return err
	}
	transaction := lock.activeTransaction
	target, ok := transaction.journal.Targets[key]
	if !ok {
		return fmt.Errorf("install transaction journal does not own target %q", key)
	}
	previous := target.Prior
	previousRecovery := target.Recovery
	target.Prior = recorded
	target.Recovery = installJournalGeneration{}
	transaction.journal.Targets[key] = target
	if err := rewriteActiveInstallTransactionJournal(transaction, lock); err != nil {
		var publishedErr *installJournalRewritePublishedError
		if !errors.As(err, &publishedErr) {
			target.Prior = previous
			target.Recovery = previousRecovery
			transaction.journal.Targets[key] = target
		}
		return err
	}
	return nil
}

func rewriteActiveInstallTransactionJournal(transaction *preparedInstallTransaction, lock *installLifecycleLock) (returnErr error) {
	if transaction == nil || transaction.journalIdentity.info == nil || lock == nil {
		return errors.New("active install transaction journal identity is required")
	}
	body, err := json.MarshalIndent(transaction.journal, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	base, err := installLockParentBase(lock, transaction.journalPath)
	if err != nil {
		return err
	}
	tempBase, err := uniqueInstallEntryName(lock.lockParent, ".transaction-update-", ".tmp")
	if err != nil {
		return err
	}
	temp, err := lock.lockParent.OpenFile(tempBase, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	created, err := temp.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.Join(errors.New("install journal temporary is not a regular file"), err, temp.Close())
	}
	cleanup := true
	defer func() {
		if cleanup {
			returnErr = errors.Join(returnErr, removeInstallTemporaryIfSame(lock.lockParent, tempBase, created))
		}
	}()
	if _, err := temp.Write(body); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	tempInfo, err := temp.Stat()
	if err != nil || !tempInfo.Mode().IsRegular() || !os.SameFile(created, tempInfo) {
		return errors.Join(errors.New("install journal temporary changed while writing"), err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	newIdentity, err := snapshotRegularInstallRemoval(lock.lockParent, tempBase)
	newDigest := sha256.Sum256(body)
	newDigestText := hex.EncodeToString(newDigest[:])
	if err != nil || !os.SameFile(tempInfo, newIdentity.info) || newIdentity.digest != newDigestText {
		return errors.Join(errors.New("install journal update changed before publication"), err)
	}
	expected := transaction.journalIdentity
	oldIdentity, err := snapshotRegularInstallRemoval(lock.lockParent, base)
	if err != nil || !os.SameFile(expected.info, oldIdentity.info) || oldIdentity.digest != expected.digest {
		return errors.Join(errors.New("install journal changed before atomic rewrite"), err)
	}
	if err := lock.Validate(); err != nil {
		return err
	}
	if err := safefs.ExchangeRoot(lock.lockParent, base, tempBase); err != nil {
		return fmt.Errorf("atomically exchange install journal rewrite: %w", err)
	}
	restore := func(cause error) error {
		restoreErr := safefs.ExchangeRoot(lock.lockParent, base, tempBase)
		if restoreErr != nil {
			cleanup = false
			return errors.Join(cause, restoreErr)
		}
		if syncErr := syncInstallJournalRoot(lock.lockParent); syncErr != nil {
			cleanup = false
			return errors.Join(cause, syncErr)
		}
		return cause
	}
	published, publishedErr := snapshotRegularInstallRemoval(lock.lockParent, base)
	displaced, displacedErr := snapshotRegularInstallRemoval(lock.lockParent, tempBase)
	if publishedErr != nil || displacedErr != nil || !os.SameFile(newIdentity.info, published.info) || published.digest != newDigestText ||
		!os.SameFile(expected.info, displaced.info) || displaced.digest != expected.digest {
		return restore(errors.Join(errors.New("install journal generations changed during atomic rewrite"), publishedErr, displacedErr))
	}
	if err := lock.Validate(); err != nil {
		return restore(err)
	}
	transaction.journalIdentity = published
	if err := syncInstallJournalRoot(lock.lockParent); err != nil {
		// The exchange succeeded but its durability is ambiguous. Keep the exact
		// prior journal at the random temporary name so either outcome recovers.
		cleanup = false
		return &installJournalRewritePublishedError{err: err}
	}
	cleanup = false
	if err := quarantineAndRemoveInstallEntry(lock.lockParent, tempBase, displaced, lock.Validate); err != nil {
		return &installJournalRewritePublishedError{err: err}
	}
	if err := syncInstallJournalRoot(lock.lockParent); err != nil {
		return &installJournalRewritePublishedError{err: err}
	}
	return nil
}

func recoverInstallTransaction(productHome, commandPath string, lock *installLifecycleLock) error {
	if err := lock.Validate(); err != nil {
		return err
	}
	journalPath := installLifecycleJournalPath(productHome, commandPath)
	body, err := readInstallLockFileBounded(lock, journalPath, maxInstallTransactionBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal installTransactionJournal
	if err := decodeInstallTransactionJournalStrict(body, &journal); err != nil {
		return err
	}
	if err := validateInstallTransactionJournal(productHome, filepath.Clean(commandPath), journalPath, journal); err != nil {
		return err
	}
	journalIdentity, err := snapshotRegularInstallRemoval(lock.lockParent, filepath.Base(journalPath))
	if err != nil {
		return err
	}
	journalDigest := sha256.Sum256(body)
	if journalIdentity.digest != hex.EncodeToString(journalDigest[:]) {
		return errors.New("install transaction journal changed while it was reviewed")
	}
	transaction := &preparedInstallTransaction{journalPath: journalPath, journal: journal, journalIdentity: journalIdentity}
	lock.activeTransaction = transaction
	lock.recoveringTransaction = true
	defer func() {
		lock.recoveringTransaction = false
		if lock.activeTransaction == transaction {
			lock.activeTransaction = nil
		}
	}()
	if err := lock.PinBundleRoot(journal.BundleRoot); err != nil {
		return err
	}
	if !journal.PreviousInstall {
		binaryPath := filepath.Join(journal.BundleRoot, "bin", threadpointProductName)
		if _, err := removeInstallerLinkChecked(journal.CommandPath, binaryPath, lock); err != nil {
			return fmt.Errorf("remove incomplete installer link: %w", err)
		}
		for _, entry := range threadpointBundleEntries {
			if _, err := removeBundleEntryChecked(entry, lock); err != nil {
				return fmt.Errorf("remove incomplete bundle entry %s: %w", entry, err)
			}
		}
		if _, err := removeManagedPathChecked(journal.MetadataPath, lock); err != nil {
			return fmt.Errorf("remove incomplete installer metadata: %w", err)
		}
		return completeInstallTransaction(transaction, lock)
	}
	backup, err := openPinnedInstallTransactionBackup(lock, journal.BackupRoot)
	if err != nil {
		return err
	}
	if err := validateInstallTransactionChecksumsForJournal(backup.root, journal); err != nil {
		return err
	}
	metadataBody, err := readInstallBackupFileBounded(backup.root, installTransactionMetadataBackup, maxInstallTransactionBytes)
	if err != nil {
		return err
	}
	metadata, err := decodeInstallMetadataStrict(metadataBody)
	if err != nil {
		return err
	}
	if err := verifyInstallOwnership(metadata, metadata.BinaryPath); err != nil {
		return err
	}
	if filepath.Clean(metadata.LinkPath) != filepath.Clean(journal.CommandPath) || filepath.Clean(metadata.BundleRoot) != filepath.Clean(journal.BundleRoot) {
		return errors.New("install transaction backup metadata has a different managed identity")
	}
	recoveryEntries := make([]string, 0, len(threadpointBundleEntries))
	for _, entry := range threadpointBundleEntries {
		target := journal.Targets[installJournalBundleKey(entry)]
		if target.Prior.Kind == installGenerationKindRegular {
			recoveryEntries = append(recoveryEntries, entry)
			continue
		}
		if target.Prior.Kind != installGenerationKindAbsent || journal.Operation != updateOperationUninstall || entry != installManagedBinaryEntry {
			return fmt.Errorf("install transaction has unsupported absent prior bundle entry: %s", entry)
		}
		parent, openErr := openPinnedBundleParent(lock.bundleRoot, filepath.Dir(filepath.FromSlash(entry)))
		if errors.Is(openErr, os.ErrNotExist) {
			continue
		}
		if openErr != nil {
			return openErr
		}
		_, snapshotErr := snapshotRegularInstallRemoval(parent, filepath.Base(filepath.FromSlash(entry)))
		closeErr := parent.Close()
		if !errors.Is(snapshotErr, os.ErrNotExist) || closeErr != nil {
			return errors.Join(fmt.Errorf("%w: absent prior bundle entry was replaced: %s", errInstallRecoveryOwnershipMismatch, entry), snapshotErr, closeErr)
		}
	}
	if err := installBundleEntriesFromPinnedBackupChecked(backup, journal.BundleRoot, recoveryEntries, lock); err != nil {
		return err
	}
	if err := backup.Validate(lock); err != nil {
		return err
	}
	if err := writeInstallMetadataChecked(journal.MetadataPath, metadata, lock); err != nil {
		return err
	}
	if journal.LinkExisted {
		if err := recoverInstallerLink(metadata, lock); err != nil {
			return err
		}
	} else if _, err := removeInstallerLinkChecked(metadata.LinkPath, metadata.BinaryPath, lock); err != nil {
		return err
	}
	if err := backup.Validate(lock); err != nil {
		return err
	}
	return completeInstallTransaction(transaction, lock)
}

func completeInstallTransaction(transaction *preparedInstallTransaction, lock *installLifecycleLock) error {
	if transaction == nil {
		return errors.New("install transaction is required")
	}
	if err := lock.Validate(); err != nil {
		return err
	}
	if err := validateActiveInstallTransactionTargets(transaction, lock); err != nil {
		return err
	}
	base, err := installLockParentBase(lock, transaction.journalPath)
	if err != nil {
		return err
	}
	if transaction.journalIdentity.info == nil {
		return errors.New("install transaction journal generation is not retained")
	}
	if err := quarantineAndRemoveInstallEntry(lock.lockParent, base, transaction.journalIdentity, lock.Validate); err != nil {
		var committedErr *installEntryRemovalCommittedError
		if errors.As(err, &committedErr) {
			return nil
		}
		return err
	}
	// A successful unlink is the commit point. A post-unlink directory-sync
	// error cannot safely be reported as a rollback because the journal may be
	// absent in this process yet reappear after a crash. Retain the exact backup
	// unless the unlink is durably synced, so either outcome remains recoverable.
	if err := syncInstallJournalRoot(lock.lockParent); err != nil {
		return nil //nolint:nilerr // unlink committed; reporting rollback would be unsafe
	}
	// Cleanup is best effort after the durable commit point and stays beneath
	// retained roots. It must never turn a committed install into a reported
	// failure or follow a replaced transactions pathname.
	lock.mu.Lock()
	backup := lock.transactionBackup
	lock.mu.Unlock()
	if backup != nil && backup.path == filepath.Clean(transaction.journal.BackupRoot) {
		_ = lock.clearTransactionBackup(backup, true)
	}
	_ = removeInstallTransactionBackupChecked(lock, transaction.journal.BackupRoot)
	if lock.activeTransaction == transaction {
		lock.activeTransaction = nil
	}
	return nil
}

func validateActiveInstallTransactionTargets(transaction *preparedInstallTransaction, lock *installLifecycleLock) error {
	if transaction == nil || lock == nil || lock.bundleRoot == nil {
		return errors.New("active install transaction roots are required")
	}
	for _, entry := range threadpointBundleEntries {
		key := installJournalBundleKey(entry)
		target, ok := transaction.journal.Targets[key]
		if !ok {
			return fmt.Errorf("install transaction journal does not own target %q", key)
		}
		expected, phase, err := expectedInstallCompletionGeneration(target, lock.recoveringTransaction)
		if err != nil {
			return fmt.Errorf("bundle target %s: %w", entry, err)
		}
		parent, err := openPinnedBundleParent(lock.bundleRoot, filepath.Dir(filepath.FromSlash(entry)))
		if errors.Is(err, os.ErrNotExist) {
			if expected.Kind != installGenerationKindAbsent {
				return fmt.Errorf("bundle target %s does not match the expected %s", entry, phase)
			}
			continue
		}
		if err != nil {
			return err
		}
		identity, snapshotErr := snapshotRegularInstallRemoval(parent, filepath.Base(filepath.FromSlash(entry)))
		closeErr := parent.Close()
		if errors.Is(snapshotErr, os.ErrNotExist) {
			if closeErr != nil {
				return closeErr
			}
			if expected.Kind == installGenerationKindAbsent {
				continue
			}
			return fmt.Errorf("bundle target %s does not match the expected %s", entry, phase)
		}
		if snapshotErr != nil || closeErr != nil {
			return errors.Join(snapshotErr, closeErr)
		}
		if !installGenerationMatches(identity, expected) {
			return fmt.Errorf("bundle target %s does not match the expected %s", entry, phase)
		}
	}
	metadataTarget, ok := transaction.journal.Targets["metadata"]
	if !ok {
		return errors.New("install transaction journal does not own installer metadata")
	}
	expectedMetadata, metadataPhase, err := expectedInstallCompletionGeneration(metadataTarget, lock.recoveringTransaction)
	if err != nil {
		return fmt.Errorf("installer metadata: %w", err)
	}
	metadataBase, err := installLockParentBase(lock, transaction.journal.MetadataPath)
	if err != nil {
		return err
	}
	if _, err := lock.lockParent.Lstat(metadataBase); errors.Is(err, os.ErrNotExist) {
		if expectedMetadata.Kind != installGenerationKindAbsent {
			return fmt.Errorf("installer metadata does not match the expected %s", metadataPhase)
		}
	} else if err != nil {
		return err
	} else {
		identity, err := snapshotRegularInstallRemoval(lock.lockParent, metadataBase)
		if err != nil || !installGenerationMatches(identity, expectedMetadata) {
			return errors.Join(fmt.Errorf("installer metadata does not match the expected %s", metadataPhase), err)
		}
	}
	linkTarget, ok := transaction.journal.Targets[installJournalTargetLink]
	if !ok {
		return errors.New("install transaction journal does not own the command link")
	}
	expectedLink, linkPhase, err := expectedInstallCompletionGeneration(linkTarget, lock.recoveringTransaction)
	if err != nil {
		return fmt.Errorf("installer command link: %w", err)
	}
	linkBase := filepath.Base(transaction.journal.CommandPath)
	if _, err := lock.commandRoot.Lstat(linkBase); errors.Is(err, os.ErrNotExist) {
		if expectedLink.Kind != installGenerationKindAbsent {
			return fmt.Errorf("installer command link does not match the expected %s", linkPhase)
		}
	} else if err != nil {
		return err
	} else {
		identity, err := snapshotSymlinkInstallRemoval(lock.commandRoot, linkBase)
		if err != nil || !installGenerationMatches(identity, expectedLink) {
			return errors.Join(fmt.Errorf("installer command link does not match the expected %s", linkPhase), err)
		}
	}
	return lock.Validate()
}

func expectedInstallCompletionGeneration(target installJournalTarget, recovering bool) (installJournalGeneration, string, error) {
	if recovering && target.Recovery.Kind != "" {
		return target.Recovery, "recovery generation", nil
	}
	if !recovering && target.Produced.Kind != "" {
		return target.Produced, "produced generation", nil
	}
	if target.Prior.Kind == "" {
		return installJournalGeneration{}, "", errors.New("install transaction target has no authoritative generation for its completion phase")
	}
	return target.Prior, "prior generation", nil
}

func removeInstallTransactionBackupChecked(lock *installLifecycleLock, backupPath string) (returnErr error) {
	if err := lock.Validate(); err != nil {
		return err
	}
	rel, err := filepath.Rel(lock.lockParentPath, filepath.Clean(backupPath))
	if err != nil || filepath.IsAbs(rel) || filepath.Dir(rel) != "transactions" || !strings.HasPrefix(filepath.Base(rel), ".bundle-") {
		return errors.Join(errors.New("install transaction backup is outside the retained transactions root"), err)
	}
	transactionsInfo, err := lock.lockParent.Lstat("transactions")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !transactionsInfo.IsDir() || transactionsInfo.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New("install transactions root changed before cleanup"), err)
	}
	transactionsRoot, err := lock.lockParent.OpenRoot("transactions")
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, transactionsRoot.Close()) }()
	openedTransactions, err := transactionsRoot.Stat(".")
	if err != nil || !os.SameFile(transactionsInfo, openedTransactions) {
		return errors.Join(errors.New("install transactions root changed while opened"), err)
	}
	base := filepath.Base(rel)
	backupInfo, err := transactionsRoot.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !backupInfo.IsDir() || backupInfo.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New("install transaction backup changed before cleanup"), err)
	}
	backupRoot, err := transactionsRoot.OpenRoot(base)
	if err != nil {
		return err
	}
	openedBackup, err := backupRoot.Stat(".")
	if err != nil || !os.SameFile(backupInfo, openedBackup) {
		return errors.Join(errors.New("install transaction backup changed while opened"), err, backupRoot.Close())
	}
	cleanupErr := removePinnedInstallTransactionContents(backupRoot)
	closeErr := backupRoot.Close()
	if cleanupErr != nil || closeErr != nil {
		return errors.Join(cleanupErr, closeErr)
	}
	current, err := transactionsRoot.Lstat(base)
	if err != nil || !os.SameFile(backupInfo, current) {
		return errors.Join(errors.New("install transaction backup changed after cleanup"), err)
	}
	if err := removePinnedInstallBackupEntryExact(transactionsRoot, base, backupInfo); err != nil {
		return err
	}
	return nil
}

const (
	maxInstallTransactionCleanupEntries = 64
	maxInstallTransactionCleanupDepth   = 8
)

func removePinnedInstallTransactionContents(root *os.Root) error {
	remaining := maxInstallTransactionCleanupEntries
	return removePinnedInstallTransactionContentsBounded(root, 0, &remaining)
}

func removePinnedInstallTransactionContentsBounded(root *os.Root, depth int, remaining *int) error {
	if root == nil || remaining == nil || depth > maxInstallTransactionCleanupDepth {
		return errors.New("install transaction backup cleanup exceeds its traversal budget")
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(*remaining + 1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(entries) > *remaining {
		return errors.New("install transaction backup cleanup exceeds its entry budget")
	}
	*remaining -= len(entries)
	for _, entry := range entries {
		name := entry.Name()
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			child, err := root.OpenRoot(name)
			if err != nil {
				return err
			}
			opened, statErr := child.Stat(".")
			if statErr != nil || !os.SameFile(info, opened) {
				return errors.Join(errors.New("install transaction backup child changed while opened"), statErr, child.Close())
			}
			cleanupErr := removePinnedInstallTransactionContentsBounded(child, depth+1, remaining)
			closeErr := child.Close()
			if cleanupErr != nil || closeErr != nil {
				return errors.Join(cleanupErr, closeErr)
			}
			current, err := root.Lstat(name)
			if err != nil || !os.SameFile(info, current) {
				return errors.Join(errors.New("install transaction backup child changed after cleanup"), err)
			}
		} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("install transaction backup contains unsupported entry %s", name)
		}
		if err := removePinnedInstallBackupEntryExact(root, name, info); err != nil {
			return err
		}
	}
	return syncInstallRootDirectory(root)
}

func removePinnedInstallBackupEntryExact(root *os.Root, name string, expected os.FileInfo) error {
	if root == nil || expected == nil || filepath.Clean(name) != filepath.Base(name) || !safefs.RenameRootNoReplaceSupported() {
		return errors.New("exact install transaction backup cleanup is unavailable")
	}
	current, err := root.Lstat(name)
	if err != nil || current.Mode().Type() != expected.Mode().Type() || !os.SameFile(expected, current) {
		return errors.Join(errors.New("install transaction backup entry changed before exact cleanup"), err)
	}
	detached, err := uniqueInstallEntryName(root, ".backup-cleanup-", "")
	if err != nil {
		return err
	}
	if err := safefs.RenameRootNoReplace(root, name, detached); err != nil {
		return err
	}
	moved, moveErr := root.Lstat(detached)
	if moveErr != nil || moved.Mode().Type() != expected.Mode().Type() || !os.SameFile(expected, moved) {
		restoreErr := safefs.RenameRootNoReplace(root, detached, name)
		return errors.Join(errors.New("install transaction backup entry changed during exact cleanup"), moveErr, restoreErr)
	}
	if moved.IsDir() {
		child, err := root.OpenRoot(detached)
		if err != nil {
			return err
		}
		directory, err := child.Open(".")
		if err != nil {
			return errors.Join(err, child.Close())
		}
		residue, readErr := directory.ReadDir(1)
		closeErr := errors.Join(directory.Close(), child.Close())
		if readErr != nil && !errors.Is(readErr, io.EOF) || len(residue) != 0 || closeErr != nil {
			return errors.Join(errors.New("detached install transaction backup directory is not empty"), readErr, closeErr)
		}
	}
	current, err = root.Lstat(detached)
	if err != nil || current.Mode().Type() != expected.Mode().Type() || !os.SameFile(expected, current) {
		return errors.Join(errors.New("detached install transaction backup entry changed before removal"), err)
	}
	if err := root.Remove(detached); err != nil {
		return fmt.Errorf("exact install transaction backup entry retained as %s: %w", detached, err)
	}
	return syncInstallRootDirectory(root)
}

func writeInstallTransactionJournalChecked(path string, journal installTransactionJournal, lock *installLifecycleLock, validateBeforePublish func() error) error {
	body, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return writeInstallJournalFileChecked(lock, path, append(body, '\n'), 0o600, ".transaction-", validateBeforePublish)
}

func writeInstallMetadataChecked(path string, metadata installMetadata, lock *installLifecycleLock) (returnErr error) {
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	base, err := installLockParentBase(lock, path)
	if err != nil {
		return err
	}
	var expected *installRemovalIdentity
	if _, err := lock.lockParent.Lstat(base); err == nil {
		current, err := snapshotRegularInstallRemoval(lock.lockParent, base)
		if err != nil {
			return err
		}
		expected = &current
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if lock.activeTransaction != nil && !lock.recoveringTransaction {
		target, ok := lock.activeTransaction.journal.Targets["metadata"]
		if expected == nil || !ok || !installGenerationMatches(*expected, target.Prior) {
			return errors.New("installer metadata no longer matches the generation recorded by the install journal")
		}
		if err := verifyInstallRemovalBackupDigest(lock, installTransactionMetadataBackup, expected.digest); err != nil {
			return err
		}
	} else if lock.activeTransaction != nil && lock.recoveringTransaction && expected != nil {
		target, ok := lock.activeTransaction.journal.Targets["metadata"]
		if !ok {
			return fmt.Errorf("%w: installer metadata is absent from the journal", errInstallRecoveryOwnershipMismatch)
		}
		if installGenerationMatches(*expected, target.Prior) || installGenerationMatches(*expected, target.Recovery) {
			digest := sha256.Sum256(body)
			if expected.digest != hex.EncodeToString(digest[:]) {
				return fmt.Errorf("%w: prior installer metadata digest differs", errInstallRecoveryOwnershipMismatch)
			}
			if installGenerationMatches(*expected, target.Recovery) {
				return recordRecoveredPriorInstallGeneration(lock, "metadata", *expected)
			}
			return lock.Validate()
		}
		if target.Produced.Kind == "" || !installGenerationMatches(*expected, target.Produced) {
			return fmt.Errorf("%w: installer metadata is an unrecorded replacement", errInstallRecoveryOwnershipMismatch)
		}
	}
	tempBase, err := uniqueInstallEntryName(lock.lockParent, ".metadata-", ".tmp")
	if err != nil {
		return err
	}
	temp, err := lock.lockParent.OpenFile(tempBase, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	created, err := temp.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.Join(errors.New("installer metadata temporary is not a regular file"), err, temp.Close())
	}
	cleanup := true
	defer func() {
		if cleanup {
			returnErr = errors.Join(returnErr, removeInstallTemporaryIfSame(lock.lockParent, tempBase, created))
		}
	}()
	if _, err := temp.Write(body); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	tempInfo, err := temp.Stat()
	if err != nil || !tempInfo.Mode().IsRegular() || !os.SameFile(created, tempInfo) {
		return errors.Join(errors.New("installer metadata temporary changed while writing"), err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	produced, err := snapshotRegularInstallRemoval(lock.lockParent, tempBase)
	if err != nil || !os.SameFile(tempInfo, produced.info) {
		return errors.Join(errors.New("installer metadata temporary changed before publication"), err)
	}
	if lock.recoveringTransaction {
		if err := recordRecoveryInstallGeneration(lock, "metadata", produced); err != nil {
			return err
		}
	} else if err := recordProducedInstallGeneration(lock, "metadata", produced); err != nil {
		return err
	}
	if err := quarantineAndReplaceInstallEntry(lock.lockParent, base, tempBase, tempInfo, expected, lock.Validate); err != nil {
		return err
	}
	cleanup = false
	if lock.recoveringTransaction {
		restored, err := snapshotRegularInstallRemoval(lock.lockParent, base)
		if err != nil {
			return err
		}
		if err := recordRecoveredPriorInstallGeneration(lock, "metadata", restored); err != nil {
			return err
		}
	}
	return nil
}

func writeInstallJournalFileChecked(lock *installLifecycleLock, path string, body []byte, mode os.FileMode, prefix string, validateBeforePublish func() error) (returnErr error) {
	base, err := installLockParentBase(lock, path)
	if err != nil {
		return err
	}
	tempBase, err := uniqueInstallEntryName(lock.lockParent, prefix, ".tmp")
	if err != nil {
		return err
	}
	temp, err := lock.lockParent.OpenFile(tempBase, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	tempInfo, err := temp.Stat()
	if err != nil || !tempInfo.Mode().IsRegular() {
		return errors.Join(errors.New("installer state temporary is not a regular file"), err, temp.Close())
	}
	cleanup := true
	defer func() {
		if cleanup {
			returnErr = errors.Join(returnErr, removeInstallTemporaryIfSame(lock.lockParent, tempBase, tempInfo))
		}
	}()
	if _, err := temp.Write(body); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	currentTemp, err := lock.lockParent.Lstat(tempBase)
	if err != nil || !currentTemp.Mode().IsRegular() || !os.SameFile(tempInfo, currentTemp) {
		return errors.Join(errors.New("installer state temporary changed before publication"), err)
	}
	if err := lock.Validate(); err != nil {
		return err
	}
	if validateBeforePublish != nil {
		if err := validateBeforePublish(); err != nil {
			return err
		}
	}
	if err := safefs.RenameRootNoReplace(lock.lockParent, tempBase, base); err != nil {
		return err
	}
	cleanup = false
	if err := syncInstallJournalRoot(lock.lockParent); err != nil {
		return &installJournalPublicationSyncError{err: err}
	}
	return nil
}

func installLockParentBase(lock *installLifecycleLock, path string) (string, error) {
	if lock == nil || lock.lockParent == nil || filepath.Clean(filepath.Dir(path)) != lock.lockParentPath {
		return "", errors.New("installer state path is outside the pinned lock namespace")
	}
	return filepath.Base(path), nil
}

func readInstallLockFileBounded(lock *installLifecycleLock, path string, maxBytes int64) (returnBody []byte, returnErr error) {
	base, err := installLockParentBase(lock, path)
	if err != nil {
		return nil, err
	}
	before, err := lock.lockParent.Lstat(base)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("installer state entry is not a regular file")
	}
	file, err := openInstallRegularFile(lock.lockParent, base)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("installer state entry changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("installer state entry exceeds its size limit")
	}
	return body, nil
}

func decodeInstallTransactionJournalStrict(body []byte, journal *installTransactionJournal) error {
	if journal == nil {
		return errors.New("install transaction journal destination is required")
	}
	if err := rejectDuplicateInstallJSONKeys(body); err != nil {
		return fmt.Errorf("malformed install transaction journal: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(journal); err != nil {
		return fmt.Errorf("malformed install transaction journal: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fmt.Errorf("malformed install transaction journal: %w", err)
	}
	return nil
}

func readInstallMetadataForTransaction(lock *installLifecycleLock, path string) (installMetadata, []byte, error) {
	body, err := readInstallLockFileBounded(lock, path, maxInstallTransactionBytes)
	if err != nil {
		return installMetadata{}, nil, err
	}
	metadata, err := decodeInstallMetadataStrict(body)
	if err != nil {
		return installMetadata{}, nil, fmt.Errorf("malformed installer metadata at %s", path)
	}
	return metadata, body, nil
}

func decodeInstallMetadataStrict(body []byte) (installMetadata, error) {
	if err := rejectDuplicateInstallJSONKeys(body); err != nil {
		return installMetadata{}, err
	}
	var metadata installMetadata
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return installMetadata{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("installer metadata contains a trailing JSON value")
		}
		return installMetadata{}, err
	}
	return metadata, nil
}

func validateInstallTransactionJournal(productHome, commandPath, journalPath string, journal installTransactionJournal) error {
	if journal.SchemaVersion != installTransactionSchemaVersion || filepath.Clean(journal.CommandPath) != commandPath || installLifecycleJournalPath(productHome, commandPath) != journalPath {
		return errors.New("install transaction journal identity is invalid")
	}
	if journal.Operation != updateOperationInstall && journal.Operation != updateOperationUpdate && journal.Operation != updateOperationRollback && journal.Operation != updateOperationUninstall {
		return fmt.Errorf("unsupported install transaction operation %q", journal.Operation)
	}
	if !filepath.IsAbs(filepath.Clean(journal.BundleRoot)) || !filepath.IsAbs(filepath.Clean(journal.MetadataPath)) {
		return errors.New("install transaction journal paths must be absolute")
	}
	binaryPath := filepath.Join(filepath.Clean(journal.BundleRoot), "bin", threadpointProductName)
	if installerMetadataPath(productHome, binaryPath) != filepath.Clean(journal.MetadataPath) {
		return errors.New("install transaction metadata is outside the managed identity")
	}
	transactionsRoot := filepath.Join(productHome, "installs", "transactions")
	rel, err := filepath.Rel(transactionsRoot, filepath.Clean(journal.BackupRoot))
	if err != nil || filepath.IsAbs(rel) || filepath.Dir(rel) != "." || !strings.HasPrefix(filepath.Base(rel), ".bundle-") {
		return errors.New("install transaction backup is outside its private directory")
	}
	expectedTargets := make(map[string]bool, len(threadpointBundleEntries)+2)
	for _, entry := range threadpointBundleEntries {
		expectedTargets[installJournalBundleKey(entry)] = true
	}
	expectedTargets["metadata"] = true
	expectedTargets[installJournalTargetLink] = true
	if len(journal.Targets) != len(expectedTargets) {
		return errors.New("install transaction journal target generations are incomplete")
	}
	for key, target := range journal.Targets {
		if !expectedTargets[key] || !validInstallJournalGeneration(target.Prior, true) ||
			!validInstallJournalGeneration(target.Produced, false) || !validInstallJournalGeneration(target.Recovery, false) {
			return fmt.Errorf("install transaction journal target generation is invalid: %s", key)
		}
	}
	if !journal.PreviousInstall {
		for _, entry := range threadpointBundleEntries {
			if journal.Targets[installJournalBundleKey(entry)].Prior.Kind != installGenerationKindAbsent {
				return fmt.Errorf("new install transaction prior bundle generation is invalid: %s", entry)
			}
		}
		if journal.Targets["metadata"].Prior.Kind != installGenerationKindAbsent || journal.Targets[installJournalTargetLink].Prior.Kind != installGenerationKindAbsent {
			return errors.New("new install transaction prior state must be absent")
		}
	} else {
		for _, entry := range threadpointBundleEntries {
			target := journal.Targets[installJournalBundleKey(entry)]
			if target.Prior.Kind == installGenerationKindRegular {
				continue
			}
			if target.Prior.Kind != installGenerationKindAbsent || journal.Operation != updateOperationUninstall || entry != installManagedBinaryEntry {
				return fmt.Errorf("install transaction prior bundle generation is invalid: %s", entry)
			}
		}
		if journal.Targets["metadata"].Prior.Kind != installGenerationKindRegular {
			return errors.New("install transaction prior metadata generation is invalid")
		}
		linkPrior := journal.Targets[installJournalTargetLink].Prior.Kind
		if linkPrior != installGenerationKindAbsent && linkPrior != installGenerationKindSymlink {
			return errors.New("install transaction prior link generation is invalid")
		}
	}
	return nil
}

func validInstallJournalGeneration(generation installJournalGeneration, required bool) bool {
	switch generation.Kind {
	case "":
		return !required && generation.Identity == "" && generation.Digest == "" && generation.LinkTarget == ""
	case installGenerationKindAbsent:
		return generation.Identity == "" && generation.Digest == "" && generation.LinkTarget == ""
	case installGenerationKindRegular:
		return generation.Identity != "" && len(generation.Digest) == sha256.Size*2 && generation.LinkTarget == ""
	case installGenerationKindSymlink:
		return generation.Identity != "" && generation.Digest == "" && generation.LinkTarget != ""
	default:
		return false
	}
}

func backupBundleEntriesForOperationChecked(sourceRoot string, backupRoot *os.Root, entries []string, operation updateOperation, lock *installLifecycleLock, backup *pinnedInstallTransactionBackup) ([]string, error) {
	if lock == nil || lock.bundleRoot == nil || filepath.Clean(sourceRoot) != lock.bundleRootPath {
		return nil, errors.New("bundle source is outside the pinned install namespace")
	}
	backedUp := make([]string, 0, len(entries))
	for _, entry := range entries {
		if err := backup.Validate(lock); err != nil {
			return nil, err
		}
		if err := copyBundleEntryFromPinnedRoot(backupRoot, entry, lock); err != nil {
			if !errors.Is(err, os.ErrNotExist) || operation != updateOperationUninstall || entry != installManagedBinaryEntry {
				return nil, err
			}
			// Keep the backup tree shape deterministic while representing the
			// already-missing binary as an absent prior generation in the journal.
			parent, ensureErr := ensurePinnedInstallBackupParent(backupRoot, filepath.Dir(filepath.FromSlash(entry)))
			if ensureErr != nil {
				return nil, ensureErr
			}
			if closeErr := parent.Close(); closeErr != nil {
				return nil, closeErr
			}
			continue
		}
		backedUp = append(backedUp, filepath.ToSlash(entry))
	}
	return backedUp, backup.Validate(lock)
}

func copyBundleEntryFromPinnedRoot(targetRoot *os.Root, entry string, lock *installLifecycleLock) (returnErr error) {
	if !safeBundleEntry(entry) {
		return fmt.Errorf("unsafe bundle entry %q", entry)
	}
	parent, err := openPinnedBundleParent(lock.bundleRoot, filepath.Dir(filepath.FromSlash(entry)))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	base := filepath.Base(filepath.FromSlash(entry))
	before, err := parent.Lstat(base)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return fmt.Errorf("bundle source entry is not a regular file: %s", entry)
	}
	source, err := openInstallRegularFile(parent, base)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, source.Close()) }()
	opened, err := source.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return fmt.Errorf("bundle source entry changed while opening: %s", entry)
	}
	if err := validateInstallMountBoundary(lock.bundleRoot, source); err != nil {
		return fmt.Errorf("bundle source entry crosses a mount boundary: %s: %w", entry, err)
	}
	if err := lock.Validate(); err != nil {
		return err
	}
	targetName := filepath.FromSlash(entry)
	parentName := filepath.Dir(targetName)
	targetParent, err := ensurePinnedInstallBackupParent(targetRoot, parentName)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, targetParent.Close()) }()
	targetBase := filepath.Base(targetName)
	tempBase, err := uniqueInstallEntryName(targetParent, "."+targetBase+".backup-", ".tmp")
	if err != nil {
		return err
	}
	temp, err := targetParent.OpenFile(tempBase, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	tempInfo, err := temp.Stat()
	if err != nil || !tempInfo.Mode().IsRegular() {
		return errors.Join(errors.New("install backup temporary is not a regular file"), err, temp.Close())
	}
	cleanup := true
	defer func() {
		if cleanup {
			returnErr = errors.Join(returnErr, removeInstallTemporaryIfSame(targetParent, tempBase, tempInfo))
		}
	}()
	if err := copyInstallFileBounded(temp, source); err != nil {
		return errors.Join(err, temp.Close())
	}
	after, err := parent.Lstat(base)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return errors.Join(fmt.Errorf("bundle source entry changed while copying: %s", entry), err, temp.Close())
	}
	if err := temp.Chmod(before.Mode().Perm()); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	currentTemp, err := targetParent.Lstat(tempBase)
	if err != nil || !currentTemp.Mode().IsRegular() || !os.SameFile(tempInfo, currentTemp) {
		return errors.Join(errors.New("install backup temporary changed before publication"), err)
	}
	if err := safefs.RenameRootNoReplace(targetParent, tempBase, targetBase); err != nil {
		return err
	}
	cleanup = false
	return syncInstallRootDirectory(targetParent)
}

func ensurePinnedInstallBackupParent(root *os.Root, rel string) (*os.Root, error) {
	if root == nil {
		return nil, errors.New("install transaction backup root is required")
	}
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	clean := filepath.Clean(rel)
	if clean == "." {
		return current, nil
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		_ = current.Close()
		return nil, errors.New("install transaction backup parent escapes its root")
	}
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		next, _, err := ensurePinnedInstallChildDirectory(current, component)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		_ = current.Close()
		current = next
	}
	return current, nil
}

func createPinnedSelfUpdateWorkspace(parentPath string) (*pinnedSelfUpdateWorkspace, error) {
	parent, _, err := pinInstallDirectory(parentPath)
	if err != nil {
		return nil, err
	}
	base, err := uniqueInstallEntryName(parent, "threadpoint-update-", "")
	if err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	root, info, err := ensurePinnedInstallChildDirectory(parent, base)
	if err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	return &pinnedSelfUpdateWorkspace{
		path:   filepath.Join(parentPath, base),
		base:   base,
		parent: parent,
		root:   root,
		info:   info,
	}, nil
}

func (workspace *pinnedSelfUpdateWorkspace) Remove() error {
	if workspace == nil {
		return nil
	}
	if workspace.parent == nil || workspace.root == nil || workspace.info == nil || workspace.base == "" {
		return errors.New("self-update workspace is not retained")
	}
	cleanupErr := removePinnedInstallTransactionContents(workspace.root)
	rootCloseErr := workspace.root.Close()
	workspace.root = nil
	var removeErr error
	if cleanupErr == nil && rootCloseErr == nil {
		removeErr = removePinnedInstallBackupEntryExact(workspace.parent, workspace.base, workspace.info)
	}
	parentCloseErr := workspace.parent.Close()
	workspace.parent = nil
	return errors.Join(cleanupErr, rootCloseErr, removeErr, parentCloseErr)
}

func writeInstallBackupRootFile(root *os.Root, name string, source io.Reader) (returnErr error) {
	if root == nil || filepath.Clean(name) != filepath.Base(name) {
		return errors.New("install backup file must be a direct child of a pinned root")
	}
	tempBase, err := uniqueInstallEntryName(root, "."+name+"-", ".tmp")
	if err != nil {
		return err
	}
	temp, err := root.OpenFile(tempBase, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	created, err := temp.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.Join(errors.New("install backup temporary is not a regular file"), err, temp.Close())
	}
	cleanup := true
	defer func() {
		if cleanup {
			returnErr = errors.Join(returnErr, removeInstallTemporaryIfSame(root, tempBase, created))
		}
	}()
	written, err := io.Copy(temp, io.LimitReader(source, maxInstallTransactionBytes+1))
	if err != nil {
		return errors.Join(err, temp.Close())
	}
	if written > maxInstallTransactionBytes {
		return errors.Join(errors.New("install backup state exceeds its size limit"), temp.Close())
	}
	if err := temp.Chmod(0o600); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	finalTemp, err := temp.Stat()
	if err != nil || !finalTemp.Mode().IsRegular() || !os.SameFile(created, finalTemp) {
		return errors.Join(errors.New("install backup temporary changed while writing"), err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	current, err := root.Lstat(tempBase)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(created, current) {
		return errors.Join(errors.New("install backup temporary file identity changed before publication"), err)
	}
	if err := safefs.RenameRootNoReplace(root, tempBase, name); err != nil {
		return err
	}
	cleanup = false
	return syncInstallRootDirectory(root)
}

func installBundleEntriesChecked(sourceRoot, targetRoot string, entries []string, lock *installLifecycleLock) error {
	if lock == nil || lock.bundleRoot == nil || filepath.Clean(targetRoot) != lock.bundleRootPath {
		return errors.New("bundle installation requires the retained managed install root")
	}
	for _, entry := range entries {
		if err := lock.Validate(); err != nil {
			return err
		}
		if err := copyBundleEntryToPinnedRoot(sourceRoot, entry, lock); err != nil {
			return err
		}
	}
	return lock.Validate()
}

func copyBundleEntryToPinnedRoot(sourceRoot, entry string, lock *installLifecycleLock) (returnErr error) {
	if !safeBundleEntry(entry) {
		return fmt.Errorf("unsafe bundle entry %q", entry)
	}
	sourcePath := filepath.Join(sourceRoot, filepath.FromSlash(entry))
	sourceInfo, err := os.Lstat(sourcePath)
	if err != nil {
		return err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("bundle entry is not a regular file: %s", sourcePath)
	}
	// #nosec G304 -- source is a fixed allow-listed verified release or private backup entry.
	source, err := os.OpenFile(sourcePath, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, source.Close()) }()
	openedSource, err := source.Stat()
	if err != nil || !openedSource.Mode().IsRegular() || !os.SameFile(sourceInfo, openedSource) {
		return fmt.Errorf("bundle source entry changed while opening: %s", sourcePath)
	}

	parent, err := openPinnedBundleParent(lock.bundleRoot, filepath.Dir(filepath.FromSlash(entry)))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	base := filepath.Base(filepath.FromSlash(entry))
	expected, err := snapshotRegularInstallRemoval(parent, base)
	if err != nil {
		return err
	}
	if err := verifyInstallRemovalBackupDigest(lock, filepath.ToSlash(entry), expected.digest); err != nil {
		return err
	}
	if transaction := lock.activeTransaction; transaction != nil {
		target, ok := transaction.journal.Targets[installJournalBundleKey(entry)]
		if !ok || !installGenerationMatches(expected, target.Prior) {
			return errors.New("bundle target no longer matches the generation recorded by the install journal")
		}
	}
	tempBase, err := uniqueInstallEntryName(parent, "."+base+".install-", ".tmp")
	if err != nil {
		return err
	}
	// #nosec G302 -- bundle entry modes are fixed by the release contract.
	temp, err := parent.OpenFile(tempBase, os.O_WRONLY|os.O_CREATE|os.O_EXCL, threadpointBundleEntryMode(entry))
	if err != nil {
		return err
	}
	created, err := temp.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.Join(errors.New("install temporary is not a regular file"), err, temp.Close())
	}
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			returnErr = errors.Join(returnErr, removeInstallTemporaryIfSame(parent, tempBase, created))
		}
	}()
	if err := copyInstallFileBounded(temp, source); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Chmod(threadpointBundleEntryMode(entry)); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	tempInfo, err := temp.Stat()
	if err != nil || !tempInfo.Mode().IsRegular() || !os.SameFile(created, tempInfo) {
		return errors.Join(errors.New("install temporary changed while writing"), err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := lock.Validate(); err != nil {
		return err
	}
	produced, err := snapshotRegularInstallRemoval(parent, tempBase)
	if err != nil || !os.SameFile(tempInfo, produced.info) {
		return errors.Join(errors.New("install temporary changed before publication"), err)
	}
	if err := recordProducedInstallGeneration(lock, installJournalBundleKey(entry), produced); err != nil {
		return err
	}
	if err := quarantineAndReplaceInstallEntry(parent, base, tempBase, tempInfo, &expected, lock.Validate); err != nil {
		return err
	}
	cleanupTemp = false
	return syncInstallRootDirectory(parent)
}

func openPinnedBundleParent(root *os.Root, rel string) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	bundleMount, err := installRootMountIdentity(current)
	if err != nil {
		_ = current.Close()
		return nil, err
	}
	clean := filepath.Clean(rel)
	if clean == "." {
		return current, nil
	}
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		before, err := current.Lstat(component)
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			_ = current.Close()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("bundle ancestor is not a physical directory: %s", rel)
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		opened, openErr := next.Stat(".")
		after, afterErr := current.Lstat(component)
		if openErr != nil || afterErr != nil || !opened.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
			_ = next.Close()
			_ = current.Close()
			return nil, fmt.Errorf("bundle ancestor identity changed while opening: %s", rel)
		}
		nextMount, mountErr := installRootMountIdentity(next)
		if mountErr != nil || nextMount != bundleMount {
			_ = next.Close()
			_ = current.Close()
			return nil, errors.Join(mountErr, fmt.Errorf("bundle ancestor crosses a mount boundary: %s", rel))
		}
		_ = current.Close()
		current = next
	}
	return current, nil
}

func installRootMountIdentity(root *os.Root) (identity uint64, returnErr error) {
	if root == nil {
		return 0, errors.New("install root is required for mount-boundary validation")
	}
	file, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	return installMountIdentity(file)
}

func validateInstallMountBoundary(root *os.Root, file *os.File) error {
	rootIdentity, err := installRootMountIdentity(root)
	if err != nil {
		return err
	}
	fileIdentity, err := installMountIdentity(file)
	if err != nil {
		return err
	}
	if rootIdentity != fileIdentity {
		return errors.New("install path crosses a mount boundary")
	}
	return nil
}

func uniqueInstallEntryName(parent *os.Root, prefix, suffix string) (string, error) {
	for range 100 {
		var token [12]byte
		_, _ = rand.Read(token[:])
		candidate := prefix + hex.EncodeToString(token[:]) + suffix
		if _, err := parent.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique install transaction entry")
}

func removeBundleEntryChecked(entry string, lock *installLifecycleLock) (returnRemoved bool, returnErr error) {
	if !safeBundleEntry(entry) {
		return false, fmt.Errorf("unsafe bundle entry %q", entry)
	}
	if err := lock.Validate(); err != nil {
		return false, err
	}
	parent, err := openPinnedBundleParent(lock.bundleRoot, filepath.Dir(filepath.FromSlash(entry)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	base := filepath.Base(filepath.FromSlash(entry))
	expected, err := snapshotRegularInstallRemoval(parent, base)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := verifyInstallRemovalBackupDigest(lock, filepath.ToSlash(entry), expected.digest); err != nil {
		return false, err
	}
	if transaction := lock.activeTransaction; transaction != nil && lock.recoveringTransaction {
		target, ok := transaction.journal.Targets[installJournalBundleKey(entry)]
		if !ok || (!installGenerationMatches(expected, target.Produced) && !installGenerationMatches(expected, target.Recovery)) {
			return false, fmt.Errorf("%w: bundle entry %s is an unrecorded replacement", errInstallRecoveryOwnershipMismatch, entry)
		}
	} else if transaction := lock.activeTransaction; transaction != nil {
		target, ok := transaction.journal.Targets[installJournalBundleKey(entry)]
		if !ok || !installGenerationMatches(expected, target.Prior) {
			return false, errors.New("managed bundle entry no longer matches the generation recorded by the install journal")
		}
		if err := recordProducedAbsentInstallGeneration(lock, installJournalBundleKey(entry)); err != nil {
			return false, err
		}
	}
	if err := lock.Validate(); err != nil {
		return false, err
	}
	if err := quarantineAndRemoveInstallEntry(parent, base, expected, lock.Validate); err != nil {
		return false, err
	}
	return true, syncInstallRootDirectory(parent)
}

func removeManagedPathChecked(path string, lock *installLifecycleLock) (returnRemoved bool, returnErr error) {
	if lock == nil || lock.bundleRoot == nil {
		return false, errors.New("installer metadata removal requires retained lifecycle and bundle roots")
	}
	rel, err := filepath.Rel(lock.bundleRootPath, filepath.Clean(path))
	if err != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, errors.New("refusing to remove a path outside the pinned managed bundle")
	}
	parent, err := openPinnedBundleParent(lock.bundleRoot, filepath.Dir(rel))
	if err != nil {
		return false, err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	base := filepath.Base(rel)
	expected, err := snapshotRegularInstallRemoval(parent, base)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := verifyInstallRemovalBackupDigest(lock, installTransactionMetadataBackup, expected.digest); err != nil {
		return false, err
	}
	if transaction := lock.activeTransaction; transaction != nil && lock.recoveringTransaction {
		target, ok := transaction.journal.Targets["metadata"]
		if !ok || (!installGenerationMatches(expected, target.Produced) && !installGenerationMatches(expected, target.Recovery)) {
			return false, fmt.Errorf("%w: installer metadata is an unrecorded replacement", errInstallRecoveryOwnershipMismatch)
		}
	} else if transaction := lock.activeTransaction; transaction != nil {
		target, ok := transaction.journal.Targets["metadata"]
		if !ok || !installGenerationMatches(expected, target.Prior) {
			return false, errors.New("installer metadata no longer matches the generation recorded by the install journal")
		}
		if err := recordProducedAbsentInstallGeneration(lock, "metadata"); err != nil {
			return false, err
		}
	}
	if err := lock.Validate(); err != nil {
		return false, err
	}
	if err := quarantineAndRemoveInstallEntry(parent, base, expected, lock.Validate); err != nil {
		return false, err
	}
	return true, syncInstallRootDirectory(parent)
}

func removeInstallerLinkChecked(linkPath, binaryPath string, lock *installLifecycleLock) (bool, error) {
	if lock == nil {
		return false, errors.New("installer link removal requires a retained lifecycle lock")
	}
	if filepath.Clean(filepath.Dir(linkPath)) != lock.commandRootPath || filepath.Clean(linkPath) != lock.owner.Command {
		return false, errors.New("refusing to remove a command link outside the pinned install identity")
	}
	_, exists, err := verifiedInstallerLinkPath(installMetadata{LinkPath: linkPath}, binaryPath)
	if err != nil || !exists {
		return false, err
	}
	base := filepath.Base(linkPath)
	expected, err := snapshotSymlinkInstallRemoval(lock.commandRoot, base)
	if err != nil {
		return false, err
	}
	expectedTarget := expected.linkTarget
	if !filepath.IsAbs(expectedTarget) {
		expectedTarget = filepath.Join(lock.commandRootPath, expectedTarget)
	}
	if filepath.Clean(expectedTarget) != filepath.Clean(binaryPath) {
		return false, errors.New("installer command link ownership changed before removal")
	}
	if transaction := lock.activeTransaction; transaction != nil && lock.recoveringTransaction {
		target, ok := transaction.journal.Targets[installJournalTargetLink]
		if !ok || (!installGenerationMatches(expected, target.Produced) && !installGenerationMatches(expected, target.Recovery)) {
			return false, fmt.Errorf("%w: command link is an unrecorded replacement", errInstallRecoveryOwnershipMismatch)
		}
	} else if transaction := lock.activeTransaction; transaction != nil {
		target, ok := transaction.journal.Targets[installJournalTargetLink]
		if !ok || !installGenerationMatches(expected, target.Prior) {
			return false, errors.New("installer command link no longer matches the generation recorded by the install journal")
		}
		if err := recordProducedAbsentInstallGeneration(lock, installJournalTargetLink); err != nil {
			return false, err
		}
	}
	if err := lock.Validate(); err != nil {
		return false, err
	}
	if err := quarantineAndRemoveInstallEntry(lock.commandRoot, base, expected, lock.Validate); err != nil {
		return false, err
	}
	return true, syncInstallRootDirectory(lock.commandRoot)
}

func verifyInstallRemovalBackupDigest(lock *installLifecycleLock, backupName, currentDigest string) error {
	if lock == nil || lock.transactionBackup == nil {
		return nil
	}
	if err := lock.transactionBackup.Validate(lock); err != nil {
		return err
	}
	expectedDigest, err := installRootFileSHA256(lock.transactionBackup.root, backupName)
	if err != nil {
		return err
	}
	if currentDigest != expectedDigest {
		return errors.New("managed install entry ownership changed after transaction preparation")
	}
	return nil
}

type installRemovalIdentity struct {
	info       os.FileInfo
	regular    bool
	symlink    bool
	digest     string
	linkTarget string
}

func installRemovalIdentityString(info os.FileInfo) (string, error) {
	if info == nil {
		return "", errors.New("install entry identity is required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return "", errors.New("install entry does not expose a stable filesystem identity")
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}

func recordedInstallGeneration(identity installRemovalIdentity) (installJournalGeneration, error) {
	id, err := installRemovalIdentityString(identity.info)
	if err != nil {
		return installJournalGeneration{}, err
	}
	switch {
	case identity.regular:
		return installJournalGeneration{Kind: installGenerationKindRegular, Identity: id, Digest: identity.digest}, nil
	case identity.symlink:
		return installJournalGeneration{Kind: installGenerationKindSymlink, Identity: id, LinkTarget: identity.linkTarget}, nil
	default:
		return installJournalGeneration{}, errors.New("unsupported install generation type")
	}
}

func installGenerationMatches(identity installRemovalIdentity, recorded installJournalGeneration) bool {
	id, err := installRemovalIdentityString(identity.info)
	if err != nil || id != recorded.Identity {
		return false
	}
	return (identity.regular && recorded.Kind == installGenerationKindRegular && identity.digest == recorded.Digest) ||
		(identity.symlink && recorded.Kind == installGenerationKindSymlink && identity.linkTarget == recorded.LinkTarget)
}

func snapshotRegularInstallRemoval(parent *os.Root, base string) (installRemovalIdentity, error) {
	info, err := parent.Lstat(base)
	if err != nil {
		return installRemovalIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return installRemovalIdentity{}, fmt.Errorf("refusing to remove non-regular managed install entry: %s", base)
	}
	digest, err := installRootFileSHA256(parent, base)
	if err != nil {
		return installRemovalIdentity{}, err
	}
	current, err := parent.Lstat(base)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return installRemovalIdentity{}, errors.Join(errors.New("managed install entry identity changed during validation"), err)
	}
	return installRemovalIdentity{info: info, regular: true, digest: digest}, nil
}

func snapshotSymlinkInstallRemoval(parent *os.Root, base string) (installRemovalIdentity, error) {
	info, err := parent.Lstat(base)
	if err != nil {
		return installRemovalIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return installRemovalIdentity{}, fmt.Errorf("refusing to remove non-symlink managed install entry: %s", base)
	}
	target, err := parent.Readlink(base)
	if err != nil {
		return installRemovalIdentity{}, err
	}
	current, err := parent.Lstat(base)
	if err != nil || current.Mode()&os.ModeSymlink == 0 || !os.SameFile(info, current) {
		return installRemovalIdentity{}, errors.Join(errors.New("managed install symlink identity changed during validation"), err)
	}
	return installRemovalIdentity{info: info, symlink: true, linkTarget: target}, nil
}

func quarantineAndRemoveInstallEntry(parent *os.Root, base string, expected installRemovalIdentity, validate func() error) (returnErr error) {
	if parent == nil || expected.info == nil || filepath.Clean(base) != filepath.Base(base) {
		return errors.New("managed install removal requires a retained parent and final-component identity")
	}
	if !safefs.RenameRootNoReplaceSupported() {
		return safefs.ErrRenameNoReplaceUnsupported
	}
	quarantineName, quarantineRoot, err := createInstallRemovalQuarantine(parent)
	if err != nil {
		return err
	}
	quarantineOpen := true
	defer func() {
		if quarantineOpen {
			returnErr = errors.Join(returnErr, quarantineRoot.Close())
		}
	}()
	if installRemovalBeforeQuarantine != nil {
		installRemovalBeforeQuarantine(parent, base)
	}
	if validate != nil {
		if err := validate(); err != nil {
			return errors.Join(err, cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen))
		}
	}
	const candidate = "candidate"
	if err := safefs.RenamePinnedRootNoReplace(parent, base, quarantineRoot, candidate); err != nil {
		return errors.Join(err, cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen))
	}
	moved, err := quarantineRoot.Lstat(candidate)
	if err != nil {
		restoreErr := restoreQuarantinedInstallEntry(quarantineRoot, expected.info, parent, base)
		cleanupErr := cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen)
		return errors.Join(errors.New("quarantined install entry could not be inspected"), err, restoreErr, cleanupErr)
	}
	if err := verifyQuarantinedInstallEntry(quarantineRoot, candidate, moved, expected); err != nil {
		restoreErr := restoreQuarantinedInstallEntry(quarantineRoot, moved, parent, base)
		cleanupErr := cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen)
		return errors.Join(errors.New("managed install entry ownership changed before quarantine"), err, restoreErr, cleanupErr)
	}
	current, err := quarantineRoot.Lstat(candidate)
	if err != nil || !os.SameFile(moved, current) {
		restoreInfo := moved
		if err == nil {
			restoreInfo = current
		}
		restoreErr := restoreQuarantinedInstallEntry(quarantineRoot, restoreInfo, parent, base)
		cleanupErr := cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen)
		return errors.Join(errors.New("managed install entry ownership changed inside quarantine"), err, restoreErr, cleanupErr)
	}
	if err := quarantineRoot.Remove(candidate); err != nil {
		restoreErr := restoreQuarantinedInstallEntry(quarantineRoot, moved, parent, base)
		cleanupErr := cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen)
		return errors.Join(err, restoreErr, cleanupErr)
	}
	if err := syncInstallJournalRoot(quarantineRoot); err != nil {
		return &installEntryRemovalCommittedError{err: errors.Join(err, cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen))}
	}
	if err := cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen); err != nil {
		return &installEntryRemovalCommittedError{err: err}
	}
	return nil
}

func quarantineAndReplaceInstallEntry(parent *os.Root, base, tempBase string, tempInfo os.FileInfo, expected *installRemovalIdentity, validate func() error) (returnErr error) {
	if parent == nil || tempInfo == nil || filepath.Clean(base) != filepath.Base(base) || filepath.Clean(tempBase) != filepath.Base(tempBase) {
		return errors.New("managed install replacement requires retained final-component identities")
	}
	if !safefs.RenameRootNoReplaceSupported() {
		return safefs.ErrRenameNoReplaceUnsupported
	}
	if expected == nil {
		if validate != nil {
			if err := validate(); err != nil {
				return err
			}
		}
		if err := safefs.RenameRootNoReplace(parent, tempBase, base); err != nil {
			return err
		}
		published, err := parent.Lstat(base)
		if err != nil || !os.SameFile(tempInfo, published) {
			return errors.Join(errors.New("published install entry changed identity"), err)
		}
		return syncInstallRootDirectory(parent)
	}
	quarantineName, quarantineRoot, err := createInstallRemovalQuarantine(parent)
	if err != nil {
		return err
	}
	quarantineOpen := true
	defer func() {
		if quarantineOpen {
			returnErr = errors.Join(returnErr, quarantineRoot.Close())
		}
	}()
	if installRemovalBeforeQuarantine != nil {
		installRemovalBeforeQuarantine(parent, base)
	}
	if validate != nil {
		if err := validate(); err != nil {
			return errors.Join(err, cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen))
		}
	}
	const candidate = "candidate"
	if err := safefs.RenamePinnedRootNoReplace(parent, base, quarantineRoot, candidate); err != nil {
		return errors.Join(err, cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen))
	}
	moved, err := quarantineRoot.Lstat(candidate)
	if err != nil || verifyQuarantinedInstallEntry(quarantineRoot, candidate, moved, *expected) != nil {
		restoreErr := restoreQuarantinedInstallEntry(quarantineRoot, moved, parent, base)
		return errors.Join(errors.New("managed install entry ownership changed before replacement"), err, restoreErr)
	}
	if err := safefs.RenameRootNoReplace(parent, tempBase, base); err != nil {
		restoreErr := restoreQuarantinedInstallEntry(quarantineRoot, moved, parent, base)
		return errors.Join(err, restoreErr)
	}
	published, err := parent.Lstat(base)
	if err != nil || !os.SameFile(tempInfo, published) {
		return errors.Join(errors.New("published install entry changed identity"), err)
	}
	if err := quarantineRoot.Remove(candidate); err != nil {
		return err
	}
	if err := syncInstallRootDirectory(quarantineRoot); err != nil {
		return err
	}
	return cleanupEmptyInstallRemovalQuarantine(parent, quarantineName, quarantineRoot, &quarantineOpen)
}

func createInstallRemovalQuarantine(parent *os.Root) (string, *os.Root, error) {
	for range 100 {
		name, err := uniqueInstallEntryName(parent, ".threadpoint-uninstall-", "")
		if err != nil {
			return "", nil, err
		}
		if err := parent.Mkdir(name, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", nil, err
		}
		before, err := parent.Lstat(name)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm() != 0o700 {
			_ = parent.Remove(name)
			return "", nil, errors.Join(errors.New("install removal quarantine is not a private directory"), err)
		}
		root, err := parent.OpenRoot(name)
		if err != nil {
			_ = parent.Remove(name)
			return "", nil, err
		}
		opened, openErr := root.Stat(".")
		after, afterErr := parent.Lstat(name)
		if openErr != nil || afterErr != nil || !opened.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
			return "", nil, errors.Join(errors.New("install removal quarantine changed while it was opened"), openErr, afterErr, root.Close())
		}
		return name, root, nil
	}
	return "", nil, errors.New("could not allocate a private install removal quarantine")
}

func verifyQuarantinedInstallEntry(root *os.Root, name string, moved os.FileInfo, expected installRemovalIdentity) error {
	if moved == nil || !os.SameFile(expected.info, moved) {
		return errors.New("quarantined inode does not match the reviewed install entry")
	}
	if expected.regular {
		if !moved.Mode().IsRegular() || moved.Mode()&os.ModeSymlink != 0 {
			return errors.New("quarantined install entry type changed")
		}
		digest, err := installRootFileSHA256(root, name)
		if err != nil || digest != expected.digest {
			return errors.Join(errors.New("quarantined install entry digest changed"), err)
		}
		return nil
	}
	if expected.symlink {
		if moved.Mode()&os.ModeSymlink == 0 {
			return errors.New("quarantined install symlink type changed")
		}
		target, err := root.Readlink(name)
		if err != nil || target != expected.linkTarget {
			return errors.Join(errors.New("quarantined install symlink target changed"), err)
		}
		return nil
	}
	return errors.New("reviewed install entry has an unsupported type")
}

func restoreQuarantinedInstallEntry(quarantine *os.Root, moved os.FileInfo, parent *os.Root, base string) error {
	const candidate = "candidate"
	current, err := quarantine.Lstat(candidate)
	if err != nil || moved == nil || !os.SameFile(moved, current) {
		return errors.Join(errors.New("quarantined install replacement changed before restore and was retained"), err)
	}
	if err := safefs.RenamePinnedRootNoReplace(quarantine, candidate, parent, base); err != nil {
		return fmt.Errorf("could not restore quarantined install replacement; residue retained: %w", err)
	}
	return syncInstallRootDirectory(parent)
}

func cleanupEmptyInstallRemovalQuarantine(parent *os.Root, name string, root *os.Root, open *bool) error {
	if root == nil || open == nil || !*open {
		return nil
	}
	closeErr := root.Close()
	*open = false
	removeErr := parent.Remove(name)
	syncErr := syncInstallRootDirectory(parent)
	return errors.Join(closeErr, removeErr, syncErr)
}

func recoverInstallerLink(metadata installMetadata, lock *installLifecycleLock) error {
	linkPath := filepath.Clean(metadata.LinkPath)
	if lock == nil || lock.commandRoot == nil {
		return errors.New("command link recovery requires a retained install lock")
	}
	if filepath.Clean(filepath.Dir(linkPath)) != lock.commandRootPath || linkPath != lock.owner.Command {
		return errors.New("refusing to recover a command link outside the pinned install identity")
	}
	base := filepath.Base(linkPath)
	if _, err := lock.commandRoot.Lstat(base); err == nil {
		current, err := snapshotSymlinkInstallRemoval(lock.commandRoot, base)
		if err != nil {
			return err
		}
		target, ok := lock.activeTransaction.journal.Targets[installJournalTargetLink]
		if !ok {
			return errors.New("install transaction journal does not own the command link")
		}
		if installGenerationMatches(current, target.Prior) {
			return lock.Validate()
		}
		if installGenerationMatches(current, target.Recovery) {
			return recordRecoveredPriorInstallGeneration(lock, installJournalTargetLink, current)
		}
		return fmt.Errorf("%w: command link is an unrecorded replacement", errInstallRecoveryOwnershipMismatch)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if lock.activeTransaction != nil {
		target, ok := lock.activeTransaction.journal.Targets[installJournalTargetLink]
		if !ok || (target.Produced.Kind != installGenerationKindAbsent && target.Recovery.Kind != installGenerationKindAbsent) {
			return fmt.Errorf("%w: command link disappeared unexpectedly", errInstallRecoveryOwnershipMismatch)
		}
	}
	if err := lock.Validate(); err != nil {
		return err
	}
	tempBase, err := uniqueInstallEntryName(lock.commandRoot, ".threadpoint-link-recover-", ".tmp")
	if err != nil {
		return err
	}
	if err := lock.commandRoot.Symlink(metadata.BinaryPath, tempBase); err != nil {
		return fmt.Errorf("could not recover installer command link: %w", err)
	}
	produced, err := snapshotSymlinkInstallRemoval(lock.commandRoot, tempBase)
	if err != nil {
		// A symlink creation does not return a retained descriptor. If its exact
		// identity cannot be snapshotted, retain the random temporary instead of
		// risking deletion of a last-moment replacement.
		return fmt.Errorf("could not retain recovery link temporary; residue retained: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = quarantineAndRemoveInstallEntry(lock.commandRoot, tempBase, produced, lock.Validate)
		}
	}()
	if err := recordRecoveryInstallGeneration(lock, installJournalTargetLink, produced); err != nil {
		return err
	}
	if err := quarantineAndReplaceInstallEntry(lock.commandRoot, base, tempBase, produced.info, nil, lock.Validate); err != nil {
		return fmt.Errorf("could not recover installer command link: %w", err)
	}
	cleanup = false
	restored, err := snapshotSymlinkInstallRemoval(lock.commandRoot, base)
	if err != nil {
		return err
	}
	if err := recordRecoveredPriorInstallGeneration(lock, installJournalTargetLink, restored); err != nil {
		return err
	}
	return syncInstallRootDirectory(lock.commandRoot)
}

func syncInstallRootDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func writeInstallTransactionChecksums(backupRoot *os.Root) error {
	return writeInstallTransactionChecksumsForEntries(backupRoot, threadpointBundleEntries)
}

func writeInstallTransactionChecksumsForEntries(backupRoot *os.Root, bundleEntries []string) error {
	entries := append([]string(nil), bundleEntries...)
	entries = append(entries, installTransactionMetadataBackup)
	var body strings.Builder
	for _, entry := range entries {
		digest, err := installRootFileSHA256(backupRoot, filepath.FromSlash(entry))
		if err != nil {
			return err
		}
		fmt.Fprintf(&body, "%s  %s\n", digest, entry)
	}
	return writeInstallBackupRootFile(backupRoot, installTransactionChecksumManifest, strings.NewReader(body.String()))
}

func syncPinnedInstallTransactionTree(backupRoot *os.Root) error {
	for _, name := range []string{"bin", "scripts", "."} {
		root, err := openPinnedBundleParent(backupRoot, name)
		if err != nil {
			return err
		}
		syncErr := syncInstallRootDirectory(root)
		closeErr := root.Close()
		if syncErr != nil || closeErr != nil {
			return errors.Join(syncErr, closeErr)
		}
	}
	return nil
}

func installerManagedBinarySHA256(metadata installMetadata, lock *installLifecycleLock) (returnDigest string, returnErr error) {
	if err := verifyBundleOwnership(metadata, metadata.BinaryPath); err != nil {
		return "", err
	}
	bundlePath := filepath.Clean(metadata.BundleRoot)
	var (
		bundleRoot *os.Root
		bundleInfo os.FileInfo
		validate   func() error
	)
	if lock != nil {
		if err := lock.PinBundleRoot(bundlePath); err != nil {
			return "", err
		}
		bundleRoot = lock.bundleRoot
		validate = lock.Validate
	} else {
		var err error
		bundleRoot, bundleInfo, err = pinInstallDirectory(bundlePath)
		if err != nil {
			return "", err
		}
		defer func() { returnErr = errors.Join(returnErr, bundleRoot.Close()) }()
		validate = func() error {
			return validatePinnedInstallDirectory(bundleRoot, bundlePath, bundleInfo)
		}
	}
	if err := validate(); err != nil {
		return "", err
	}
	binBefore, err := bundleRoot.Lstat("bin")
	if err != nil || binBefore.Mode()&os.ModeSymlink != 0 || !binBefore.IsDir() {
		return "", errors.Join(errors.New("installer-managed binary directory is not a physical directory"), err)
	}
	binRoot, err := openPinnedBundleParent(bundleRoot, "bin")
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, binRoot.Close()) }()
	binOpened, err := binRoot.Stat(".")
	if err != nil || !binOpened.IsDir() || !os.SameFile(binBefore, binOpened) {
		return "", errors.Join(errors.New("installer-managed binary directory changed while opening"), err)
	}
	digest, hashErr := installRootFileSHA256(binRoot, threadpointProductName)
	binAfter, afterErr := bundleRoot.Lstat("bin")
	if afterErr != nil || !binAfter.IsDir() || binAfter.Mode()&os.ModeSymlink != 0 || !os.SameFile(binOpened, binAfter) {
		return "", errors.Join(hashErr, errors.New("installer-managed binary directory changed while hashing"), afterErr)
	}
	if err := validate(); err != nil {
		return "", errors.Join(hashErr, err)
	}
	if hashErr != nil {
		return "", hashErr
	}
	return digest, nil
}

func installRootFileSHA256(root *os.Root, name string) (returnDigest string, returnErr error) {
	if root == nil {
		return "", errors.New("install transaction root is required")
	}
	before, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", fmt.Errorf("install transaction entry is not a regular file: %s", name)
	}
	file, err := openInstallRegularFile(root, name)
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", errors.Join(fmt.Errorf("install transaction entry changed while opening: %s", name), err)
	}
	if err := validateInstallMountBoundary(root, file); err != nil {
		return "", err
	}
	if opened.Size() < 0 || opened.Size() > maxInstallManagedFileBytes {
		return "", fmt.Errorf("install transaction entry exceeds its size limit: %s", name)
	}
	hash := sha256.New()
	if err := copyInstallFileBounded(hash, file); err != nil {
		return "", err
	}
	after, err := root.Lstat(name)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return "", errors.Join(fmt.Errorf("install transaction entry changed while hashing: %s", name), err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func openInstallRegularFile(root *os.Root, name string) (*os.File, error) {
	if root == nil {
		return nil, errors.New("install root is required")
	}
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

func copyInstallFileBounded(destination io.Writer, source *os.File) error {
	if destination == nil || source == nil {
		return errors.New("bounded install copy requires a destination and source")
	}
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxInstallManagedFileBytes {
		return errors.New("installer-managed file is not regular or exceeds its size limit")
	}
	written, err := io.Copy(destination, io.LimitReader(source, maxInstallManagedFileBytes+1))
	if err != nil {
		return err
	}
	if written > maxInstallManagedFileBytes {
		return errors.New("installer-managed file grew beyond its size limit")
	}
	return nil
}

func validateInstallTransactionChecksumsRoot(backupRoot *os.Root) error {
	return validateInstallTransactionChecksumsForEntries(backupRoot, threadpointBundleEntries)
}

func validateInstallTransactionChecksumsForJournal(backupRoot *os.Root, journal installTransactionJournal) error {
	entries := make([]string, 0, len(threadpointBundleEntries))
	for _, entry := range threadpointBundleEntries {
		if journal.Targets[installJournalBundleKey(entry)].Prior.Kind == installGenerationKindRegular {
			entries = append(entries, entry)
		}
	}
	return validateInstallTransactionChecksumsForEntries(backupRoot, entries)
}

func validateInstallTransactionChecksumsForEntries(backupRoot *os.Root, bundleEntries []string) error {
	body, err := tpbackup.ReadRootRegularFileBounded(backupRoot, installTransactionChecksumManifest, maxInstallTransactionBytes)
	if err != nil {
		return err
	}
	expected := make(map[string]bool, len(bundleEntries)+1)
	for _, entry := range append(append([]string(nil), bundleEntries...), installTransactionMetadataBackup) {
		expected[entry] = true
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !expected[fields[1]] || seen[fields[1]] {
			return errors.New("install transaction checksum manifest is malformed")
		}
		actual, err := installRootFileSHA256(backupRoot, filepath.FromSlash(fields[1]))
		if err != nil {
			return fmt.Errorf("read install transaction backup %s: %w", fields[1], err)
		}
		if actual != fields[0] {
			return fmt.Errorf("install transaction backup checksum mismatch for %s", fields[1])
		}
		seen[fields[1]] = true
	}
	if len(seen) != len(expected) {
		return errors.New("install transaction checksum manifest is incomplete")
	}
	return nil
}

func readInstallBackupFileBounded(root *os.Root, name string, maxBytes int64) ([]byte, error) {
	if filepath.Clean(name) != filepath.Base(name) {
		return nil, errors.New("install backup metadata must be a direct child of its pinned root")
	}
	return tpbackup.ReadRootRegularFileBounded(root, name, maxBytes)
}

func installBundleEntriesFromPinnedBackupChecked(source *pinnedInstallTransactionBackup, targetRoot string, entries []string, lock *installLifecycleLock) error {
	if source == nil || source.root == nil || lock == nil || lock.bundleRoot == nil || filepath.Clean(targetRoot) != lock.bundleRootPath {
		return errors.New("install recovery roots are not pinned")
	}
	for _, entry := range entries {
		if err := source.Validate(lock); err != nil {
			return err
		}
		if err := copyBundleEntryBetweenPinnedRoots(source.root, entry, lock); err != nil {
			return err
		}
	}
	return source.Validate(lock)
}

func copyBundleEntryBetweenPinnedRoots(sourceRoot *os.Root, entry string, lock *installLifecycleLock) (returnErr error) {
	return copyBundleEntryBetweenPinnedRootsWithPolicy(sourceRoot, entry, lock, false)
}

func copyBundleEntryBetweenPinnedRootsWithPolicy(sourceRoot *os.Root, entry string, lock *installLifecycleLock, preserveExisting bool) (returnErr error) {
	if !safeBundleEntry(entry) {
		return fmt.Errorf("unsafe bundle entry %q", entry)
	}
	sourceParent, err := openPinnedBundleParent(sourceRoot, filepath.Dir(filepath.FromSlash(entry)))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, sourceParent.Close()) }()
	base := filepath.Base(filepath.FromSlash(entry))
	before, err := sourceParent.Lstat(base)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return errors.Join(fmt.Errorf("install backup entry is not a regular file: %s", entry), err)
	}
	sourceFile, err := openInstallRegularFile(sourceParent, base)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, sourceFile.Close()) }()
	opened, err := sourceFile.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return errors.Join(fmt.Errorf("install backup entry changed while opening: %s", entry), err)
	}
	if err := validateInstallMountBoundary(sourceRoot, sourceFile); err != nil {
		return err
	}
	targetParent, err := openPinnedBundleParent(lock.bundleRoot, filepath.Dir(filepath.FromSlash(entry)))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, targetParent.Close()) }()
	expectedDigest, err := installRootFileSHA256(sourceParent, base)
	if err != nil {
		return err
	}
	var currentExpected *installRemovalIdentity
	if _, err := targetParent.Lstat(base); err == nil {
		currentIdentity, snapshotErr := snapshotRegularInstallRemoval(targetParent, base)
		if snapshotErr != nil {
			return fmt.Errorf("%w: bundle entry %s is not a stable regular file: %w", errInstallRecoveryOwnershipMismatch, entry, snapshotErr)
		}
		if lock.activeTransaction != nil {
			target, ok := lock.activeTransaction.journal.Targets[installJournalBundleKey(entry)]
			if !ok {
				return fmt.Errorf("%w: bundle entry %s is absent from the journal", errInstallRecoveryOwnershipMismatch, entry)
			}
			if installGenerationMatches(currentIdentity, target.Prior) || installGenerationMatches(currentIdentity, target.Recovery) {
				if currentIdentity.digest != expectedDigest {
					return fmt.Errorf("%w: prior bundle digest differs for %s", errInstallRecoveryOwnershipMismatch, entry)
				}
				if installGenerationMatches(currentIdentity, target.Recovery) {
					return recordRecoveredPriorInstallGeneration(lock, installJournalBundleKey(entry), currentIdentity)
				}
				return lock.Validate()
			}
			if target.Produced.Kind == "" || !installGenerationMatches(currentIdentity, target.Produced) {
				return fmt.Errorf("%w: bundle entry %s is an unrecorded replacement", errInstallRecoveryOwnershipMismatch, entry)
			}
		} else if preserveExisting && currentIdentity.digest != expectedDigest {
			return fmt.Errorf("%w: bundle entry %s", errInstallRecoveryOwnershipMismatch, entry)
		}
		currentExpected = &currentIdentity
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tempBase, err := uniqueInstallEntryName(targetParent, "."+base+".install-", ".tmp")
	if err != nil {
		return err
	}
	temp, err := targetParent.OpenFile(tempBase, os.O_WRONLY|os.O_CREATE|os.O_EXCL, threadpointBundleEntryMode(entry))
	if err != nil {
		return err
	}
	cleanup := true
	created, err := temp.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.Join(errors.New("install recovery temporary is not a regular file"), err, temp.Close())
	}
	defer func() {
		if cleanup {
			returnErr = errors.Join(returnErr, removeInstallTemporaryIfSame(targetParent, tempBase, created))
		}
	}()
	if err := copyInstallFileBounded(temp, sourceFile); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Chmod(threadpointBundleEntryMode(entry)); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	finalTemp, err := temp.Stat()
	if err != nil || !finalTemp.Mode().IsRegular() || !os.SameFile(created, finalTemp) {
		return errors.Join(errors.New("install recovery temporary is not a regular file"), err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := lock.Validate(); err != nil {
		return err
	}
	produced, err := snapshotRegularInstallRemoval(targetParent, tempBase)
	if err != nil || !os.SameFile(created, produced.info) {
		return errors.Join(errors.New("install recovery temporary changed before journal authorization"), err)
	}
	if err := recordRecoveryInstallGeneration(lock, installJournalBundleKey(entry), produced); err != nil {
		return err
	}
	if err := quarantineAndReplaceInstallEntry(targetParent, base, tempBase, created, currentExpected, lock.Validate); err != nil {
		return err
	}
	cleanup = false
	published, err := targetParent.Lstat(base)
	if err != nil || !published.Mode().IsRegular() || !os.SameFile(created, published) {
		return errors.Join(fmt.Errorf("%w: recovered bundle entry changed at publication: %s", errInstallRecoveryOwnershipMismatch, entry), err)
	}
	publishedDigest, err := installRootFileSHA256(targetParent, base)
	if err != nil || publishedDigest != expectedDigest {
		return errors.Join(fmt.Errorf("%w: recovered bundle entry digest changed: %s", errInstallRecoveryOwnershipMismatch, entry), err)
	}
	restored, err := snapshotRegularInstallRemoval(targetParent, base)
	if err != nil {
		return err
	}
	if err := recordRecoveredPriorInstallGeneration(lock, installJournalBundleKey(entry), restored); err != nil {
		return err
	}
	return syncInstallRootDirectory(targetParent)
}

func removeInstallTemporaryIfSame(root *os.Root, name string, expected os.FileInfo) error {
	if root == nil || expected == nil {
		return nil
	}
	identity, err := snapshotRegularInstallRemoval(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(expected, identity.info) {
		return errors.New("install recovery temporary changed and was retained")
	}
	if err := quarantineAndRemoveInstallEntry(root, name, identity, nil); err != nil {
		return fmt.Errorf("install temporary could not be safely quarantined and was retained: %w", err)
	}
	return nil
}
