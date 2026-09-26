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
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/threadgrid/threadpoint/safefs"
)

const (
	inheritedInstallLockCommand            = "__threadpoint-install-lock-guard"
	inheritedInstallNoReplaceCommand       = "__threadpoint-install-no-replace"
	inheritedInstallNoReplaceAs            = "__threadpoint-install-no-replace-as"
	inheritedInstallJournalCommand         = "__threadpoint-install-journal"
	inheritedInstallInspectCommand         = "__threadpoint-install-journal-inspect"
	inheritedInstallEntryIdentity          = "__threadpoint-install-entry-identity"
	inheritedInstallEntryInspect           = "__threadpoint-install-entry-inspect"
	inheritedInstallRegularSnapshot        = "__threadpoint-install-regular-snapshot"
	inheritedInstallLockOwnerInspect       = "__threadpoint-install-lock-owner"
	inheritedInstallLockHeartbeat          = "__threadpoint-install-lock-heartbeat"
	inheritedInstallRemoveAs               = "__threadpoint-install-remove-as"
	inheritedInstallRecoveryCleanup        = "__threadpoint-install-recovery-cleanup"
	inheritedInstallCopyBoundedAs          = "__threadpoint-install-copy-bounded-as"
	inheritedInstallWriteBounded           = "__threadpoint-install-write-bounded"
	installJournalActionUpdateExpect       = "update-from-expected"
	maxInstallLockOwnerBytes         int64 = 4096
)

// Tests use this hook to replace an ambient parent after the helper has opened
// it. Production leaves it nil.
var inheritedInstallHelperAfterParentOpen func(string)
var inheritedInstallHelperAfterBoundedCopySnapshot func(string)
var inheritedInstallHelperAfterLockAcquire func(string)
var inheritedInstallHelperAfterLockOwnerSnapshot func(string)
var inheritedInstallHelperAfterBoundedWrite func(string)
var inheritedInstallHelperAfterGenerationSnapshot func(string)
var inheritedInstallHelperAfterJournalExchange func(*os.Root, string, string)
var inheritedInstallHelperBeforeNoReplaceRename func(string)
var inheritedInstallHelperAfterRecoveryCleanupRootOpen func(string)
var inheritedInstallHelperAfterRecoveryCleanupChildOpen func(string)
var exchangeInstallHelperRoot = safefs.ExchangeRoot
var syncInstallHelperJournalRoot = syncInstallRootDirectory

// runInheritedInstallLockHelper is intentionally absent from user-facing
// command discovery. Verified installer scripts use these commands only for
// operations that POSIX sh cannot perform safely on its own.
func runInheritedInstallLockHelper(stdout, stderr io.Writer, stdin io.Reader, args []string) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	var err error
	switch args[0] {
	case inheritedInstallLockCommand:
		err = runInstallLockGuard(stdout, stdin, args[1:])
	case inheritedInstallNoReplaceCommand:
		err = runInstallNoReplaceExpected(args[1:], false)
	case inheritedInstallNoReplaceAs:
		err = runInstallNoReplaceExpected(args[1:], true)
	case inheritedInstallJournalCommand:
		err = runInstallJournalHelper(stdout, args[1:])
	case inheritedInstallInspectCommand:
		err = runInstallJournalInspect(stdout, args[1:])
	case inheritedInstallEntryIdentity:
		err = runInstallEntryIdentity(stdout, args[1:])
	case inheritedInstallEntryInspect:
		err = runInstallEntryInspect(stdout, args[1:])
	case inheritedInstallRegularSnapshot:
		err = runInstallRegularSnapshot(stdout, args[1:])
	case inheritedInstallLockOwnerInspect:
		err = runInstallLockOwnerInspect(stdout, args[1:])
	case inheritedInstallLockHeartbeat:
		err = runInstallLockHeartbeat(args[1:])
	case inheritedInstallRemoveAs:
		err = runInstallRemoveAs(args[1:])
	case inheritedInstallRecoveryCleanup:
		err = runInstallRecoveryCleanup(args[1:])
	case inheritedInstallCopyBoundedAs:
		err = runInstallCopyBoundedAs(stdout, args[1:])
	case inheritedInstallWriteBounded:
		err = runInstallWriteBounded(stdout, stdin, args[1:])
	default:
		return false, 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "installer helper failed: %v\n", err)
		return true, ExitInternal
	}
	return true, ExitOK
}

func runInstallCopyBoundedAs(stdout io.Writer, args []string) (returnErr error) {
	if len(args) != 5 {
		return errors.New("invalid inherited bounded install copy invocation")
	}
	sourcePath, destinationPath := args[0], args[1]
	expectedIdentity, expectedDigest := args[2], args[3]
	destinationMode, err := installBoundedCopyMode(args[4])
	if err != nil {
		return err
	}
	if !validInstallHelperIdentity(expectedIdentity) || len(expectedDigest) != sha256.Size*2 {
		return errors.New("invalid inherited bounded install copy invocation")
	}
	for _, char := range expectedDigest {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return errors.New("invalid inherited bounded install copy digest")
		}
	}
	sourceRoot, sourceParent, sourceBase, err := openPinnedInstallHelperParent(sourcePath)
	if err != nil {
		return fmt.Errorf("retain bounded-copy source parent: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, sourceRoot.Close()) }()
	destinationRoot, destinationParent, destinationBase, err := openPinnedInstallHelperParent(destinationPath)
	if err != nil {
		return fmt.Errorf("retain bounded-copy destination parent: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, destinationRoot.Close()) }()

	before, err := sourceRoot.Lstat(sourceBase)
	var identity string
	var identityErr error
	if err == nil {
		identity, identityErr = installRemovalIdentityString(before)
	}
	if err != nil || identityErr != nil || identity != expectedIdentity || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > maxInstallManagedFileBytes {
		return errors.Join(errors.New("bounded-copy source does not match its expected regular generation"), err, identityErr)
	}
	if _, err := destinationRoot.Lstat(destinationBase); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("bounded-copy destination already exists"), err)
	}
	if inheritedInstallHelperAfterBoundedCopySnapshot != nil {
		inheritedInstallHelperAfterBoundedCopySnapshot(sourcePath)
	}
	source, err := openInstallRegularFile(sourceRoot, sourceBase)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, source.Close()) }()
	opened, err := source.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return errors.Join(errors.New("bounded-copy source changed while it was opened"), err)
	}
	destination, err := destinationRoot.OpenFile(destinationBase, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	created, err := destination.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.Join(errors.New("bounded-copy destination is not a regular file"), err, destination.Close())
	}
	cleanup := true
	defer func() {
		if cleanup {
			returnErr = errors.Join(returnErr, removeInstallTemporaryIfSame(destinationRoot, destinationBase, created))
		}
	}()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(source, maxInstallManagedFileBytes+1))
	if copyErr != nil || written > maxInstallManagedFileBytes {
		return errors.Join(errors.New("bounded-copy source exceeded its copy limit"), copyErr, destination.Close())
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return errors.Join(errors.New("bounded-copy source digest changed"), destination.Close())
	}
	if err := destination.Chmod(destinationMode); err != nil {
		return errors.Join(err, destination.Close())
	}
	if err := destination.Sync(); err != nil {
		return errors.Join(err, destination.Close())
	}
	finishedDestination, destinationStatErr := destination.Stat()
	closeErr := destination.Close()
	finishedSource, sourceStatErr := source.Stat()
	after, afterErr := sourceRoot.Lstat(sourceBase)
	if destinationStatErr != nil || closeErr != nil || sourceStatErr != nil || afterErr != nil ||
		!os.SameFile(created, finishedDestination) || !os.SameFile(opened, finishedSource) || !os.SameFile(finishedSource, after) ||
		opened.Size() != finishedSource.Size() || !opened.ModTime().Equal(finishedSource.ModTime()) {
		return errors.Join(errors.New("bounded-copy generation changed during snapshot"), destinationStatErr, closeErr, sourceStatErr, afterErr)
	}
	copied, err := snapshotRegularInstallRemoval(destinationRoot, destinationBase)
	if err != nil || !os.SameFile(created, copied.info) || copied.digest != expectedDigest {
		return errors.Join(errors.New("bounded-copy destination changed after snapshot"), err)
	}
	if err := errors.Join(revalidateInstallHelperParent(sourceRoot, sourceParent), revalidateInstallHelperParent(destinationRoot, destinationParent), syncInstallRootDirectory(destinationRoot)); err != nil {
		return err
	}
	copiedIdentity, err := installRemovalIdentityString(copied.info)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%s %s\n", copiedIdentity, expectedDigest); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func runInstallWriteBounded(stdout io.Writer, stdin io.Reader, args []string) (returnErr error) {
	if len(args) != 2 || stdin == nil {
		return errors.New("invalid inherited bounded install write invocation")
	}
	destinationMode, err := installBoundedCopyMode(args[1])
	if err != nil {
		return err
	}
	root, parent, base, err := openPinnedInstallHelperParent(args[0])
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	if _, err := root.Lstat(base); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("bounded-write destination already exists"), err)
	}
	file, err := root.OpenFile(base, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return err
	}
	created, err := file.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.Join(errors.New("bounded-write destination is not a regular file"), err, file.Close())
	}
	cleanup := true
	fileOpen := true
	defer func() {
		if fileOpen {
			returnErr = errors.Join(returnErr, file.Close())
		}
		if cleanup {
			returnErr = errors.Join(returnErr, removeInstallTemporaryIfSame(root, base, created))
		}
	}()
	if err := validateInstallMountBoundary(root, file); err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(stdin, maxInstallTransactionBytes+1))
	if copyErr != nil || written > maxInstallTransactionBytes {
		return errors.Join(errors.New("bounded-write content exceeds its size limit"), copyErr)
	}
	if err := file.Chmod(destinationMode); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	finished, err := file.Stat()
	if err != nil || !finished.Mode().IsRegular() || !os.SameFile(created, finished) {
		return errors.Join(errors.New("bounded-write destination changed while writing"), err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	fileOpen = false
	if inheritedInstallHelperAfterBoundedWrite != nil {
		inheritedInstallHelperAfterBoundedWrite(args[0])
	}
	current, currentErr := root.Lstat(base)
	if currentErr != nil || !current.Mode().IsRegular() || !os.SameFile(finished, current) {
		return errors.Join(errors.New("bounded-write destination changed before publication"), currentErr)
	}
	if err := revalidateInstallHelperParent(root, parent); err != nil {
		return err
	}
	if err := syncInstallRootDirectory(root); err != nil {
		return err
	}
	identity, err := installRemovalIdentityString(finished)
	if err != nil {
		return err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	cleanup = false
	_, err = fmt.Fprintf(stdout, "%s %s\n", identity, digest)
	return err
}

func installBoundedCopyMode(value string) (os.FileMode, error) {
	switch value {
	case "0700":
		return 0o700, nil
	case "0600":
		return 0o600, nil
	case "0644":
		return 0o644, nil
	case "0755":
		return 0o755, nil
	default:
		return 0, errors.New("invalid inherited bounded install copy mode")
	}
}

func validInstallHelperIdentity(identity string) bool {
	parts := strings.Split(identity, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, part := range parts {
		if _, err := strconv.ParseUint(part, 10, 64); err != nil {
			return false
		}
	}
	return true
}

func runInstallLockGuard(stdout io.Writer, stdin io.Reader, args []string) error {
	if len(args) != 2 {
		return errors.New("invalid inherited install lock helper invocation")
	}
	waitSeconds, err := strconv.Atoi(args[1])
	if err != nil || waitSeconds < 1 || waitSeconds > 300 {
		return errors.New("invalid install lock guard timeout")
	}
	parent, parentPath, _, err := openPinnedInstallHelperParent(args[0])
	if err != nil {
		return fmt.Errorf("retain install lock guard parent: %w", err)
	}
	defer parent.Close()
	lock, err := safefs.AcquireKernelFileLockRoot(context.Background(), parent, filepath.Base(args[0]), args[0], time.Duration(waitSeconds)*time.Second, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("acquire install lock guard: %w", err)
	}
	if inheritedInstallHelperAfterLockAcquire != nil {
		inheritedInstallHelperAfterLockAcquire(parentPath)
	}
	if err := errors.Join(lock.Validate(), revalidateInstallHelperParent(parent, parentPath)); err != nil {
		return errors.Join(errors.New("revalidate install lock guard after acquisition"), err, lock.Release())
	}
	if _, err := fmt.Fprintln(stdout, "ready"); err != nil {
		return errors.Join(err, lock.Release())
	}
	// EOF is the release capability. Descendants inherit the writer, so a
	// crashed parent cannot release the guard while one of its children may
	// still be inside a mutation.
	_, copyErr := io.Copy(io.Discard, stdin)
	return errors.Join(copyErr, lock.Release())
}

func runInstallNoReplace(args []string) error {
	return runInstallNoReplaceExpected(args, false)
}

func runInstallNoReplaceExpected(args []string, requireExpected bool) error {
	if len(args) < 2 {
		return errors.New("invalid inherited install no-replace invocation")
	}
	sourcePath, destinationPath := args[0], args[1]
	expectedIdentity := ""
	if requireExpected {
		if len(args) != 3 {
			return errors.New("invalid inherited install no-replace invocation")
		}
		expectedIdentity = args[2]
	} else if len(args) != 2 {
		return errors.New("invalid inherited install no-replace invocation")
	}
	oldRoot, oldParent, oldBase, err := openPinnedInstallHelperParent(sourcePath)
	if err != nil {
		return fmt.Errorf("retain no-replace source parent: %w", err)
	}
	defer oldRoot.Close()
	newRoot, newParent, newBase, err := openPinnedInstallHelperParent(destinationPath)
	if err != nil {
		return fmt.Errorf("retain no-replace destination parent: %w", err)
	}
	defer newRoot.Close()
	source, err := oldRoot.Lstat(oldBase)
	if err != nil {
		return fmt.Errorf("inspect no-replace source: %w", err)
	}
	if requireExpected {
		identity, err := installRemovalIdentityString(source)
		if err != nil || identity != expectedIdentity {
			return errors.Join(errors.New("no-replace source does not match its expected identity"), err)
		}
	}
	if err := revalidateInstallHelperParent(oldRoot, oldParent); err != nil {
		return err
	}
	if err := revalidateInstallHelperParent(newRoot, newParent); err != nil {
		return err
	}
	if inheritedInstallHelperBeforeNoReplaceRename != nil {
		inheritedInstallHelperBeforeNoReplaceRename(sourcePath)
	}
	current, err := oldRoot.Lstat(oldBase)
	if err != nil || !os.SameFile(source, current) {
		return errors.Join(errors.New("no-replace source changed before rename"), err)
	}
	if err := safefs.RenamePinnedRootNoReplace(oldRoot, oldBase, newRoot, newBase); err != nil {
		return fmt.Errorf("atomic no-replace rename: %w", err)
	}
	restore := func(cause error) error {
		restoreErr := safefs.RenamePinnedRootNoReplace(newRoot, newBase, oldRoot, oldBase)
		return errors.Join(cause, restoreErr)
	}
	moved, movedErr := newRoot.Lstat(newBase)
	if movedErr != nil || !os.SameFile(source, moved) {
		return restore(errors.Join(errors.New("no-replace source changed during rename"), movedErr))
	}
	if err := errors.Join(revalidateInstallHelperParent(oldRoot, oldParent), revalidateInstallHelperParent(newRoot, newParent)); err != nil {
		return restore(errors.Join(errors.New("no-replace parent changed during rename"), err))
	}
	return errors.Join(syncInstallRootDirectory(newRoot), syncInstallRootDirectory(oldRoot))
}

func runInstallEntryIdentity(stdout io.Writer, args []string) (returnErr error) {
	if len(args) != 1 {
		return errors.New("invalid inherited install entry identity invocation")
	}
	root, parent, base, err := openPinnedInstallHelperParent(args[0])
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	before, err := root.Lstat(base)
	if err != nil || (!before.Mode().IsRegular() && before.Mode()&os.ModeSymlink == 0 && !before.IsDir()) {
		return errors.Join(errors.New("install entry identity requires a regular file, symlink, or physical directory"), err)
	}
	current, err := root.Lstat(base)
	if err != nil || !os.SameFile(before, current) {
		return errors.Join(errors.New("install entry changed during identity inspection"), err)
	}
	if err := revalidateInstallHelperParent(root, parent); err != nil {
		return err
	}
	identity, err := installRemovalIdentityString(before)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, identity)
	return err
}

func runInstallRemoveAs(args []string) (returnErr error) {
	if len(args) != 4 {
		return errors.New("invalid inherited install remove-as invocation")
	}
	root, parent, base, err := openPinnedInstallHelperParent(args[0])
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	before, err := root.Lstat(base)
	if err != nil {
		return err
	}
	identity, err := installRemovalIdentityString(before)
	if err != nil || identity != args[1] {
		return errors.Join(errors.New("install removal entry does not match its expected identity"), err)
	}
	if before.IsDir() && before.Mode()&os.ModeSymlink == 0 {
		if args[2] != "directory" || args[3] != "-" {
			return errors.New("install directory removal proof is invalid")
		}
		return removeEmptyInstallHelperDirectory(root, parent, base, before)
	}
	var expected installRemovalIdentity
	switch {
	case before.Mode().IsRegular():
		if args[2] != installGenerationKindRegular {
			return errors.New("install removal kind does not match regular entry")
		}
		expected, err = snapshotRegularInstallRemoval(root, base)
	case before.Mode()&os.ModeSymlink != 0:
		if args[2] != installGenerationKindSymlink {
			return errors.New("install removal kind does not match symlink entry")
		}
		expected, err = snapshotSymlinkInstallRemoval(root, base)
	default:
		return errors.New("install remove-as refuses special filesystem entries")
	}
	if err != nil || !os.SameFile(before, expected.info) {
		return errors.Join(errors.New("install removal entry changed during validation"), err)
	}
	proof := expected.digest
	if expected.symlink {
		digest := sha256.Sum256([]byte(expected.linkTarget))
		proof = hex.EncodeToString(digest[:])
	}
	// #nosec G602 -- both guards above establish that args has exactly four entries.
	providedProof := args[3]
	if len(providedProof) != sha256.Size*2 || proof != providedProof {
		return errors.New("install removal entry does not match its expected content proof")
	}
	return quarantineAndRemoveInstallEntry(root, base, expected, func() error {
		return revalidateInstallHelperParent(root, parent)
	})
}

type installRecoveryCleanupDirectory struct {
	name    string
	entries []string
}

func runInstallRecoveryCleanup(args []string) (returnErr error) {
	if len(args) != 2 || !validInstallHelperIdentity(args[1]) {
		return errors.New("invalid inherited install recovery cleanup invocation")
	}
	cleanupPath := args[0]
	transactionsRoot, transactionsPath, cleanupBase, err := openPinnedInstallHelperParent(cleanupPath)
	if err != nil {
		return fmt.Errorf("retain install recovery transactions root: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, transactionsRoot.Close()) }()
	if !strings.HasPrefix(cleanupBase, ".bundle-cleanup-") || cleanupBase == ".bundle-cleanup-" {
		return errors.New("install recovery cleanup path is outside the cleanup namespace")
	}
	cleanupInfo, err := transactionsRoot.Lstat(cleanupBase)
	if err != nil || !cleanupInfo.IsDir() || cleanupInfo.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New("install recovery cleanup root is not a physical directory"), err)
	}
	cleanupIdentity, err := installRemovalIdentityString(cleanupInfo)
	if err != nil || cleanupIdentity != args[1] {
		return errors.Join(errors.New("install recovery cleanup root does not match its expected identity"), err)
	}
	cleanupRoot, err := transactionsRoot.OpenRoot(cleanupBase)
	if err != nil {
		return err
	}
	cleanupRootOpen := true
	defer func() {
		if cleanupRootOpen {
			returnErr = errors.Join(returnErr, cleanupRoot.Close())
		}
	}()
	if inheritedInstallHelperAfterRecoveryCleanupRootOpen != nil {
		inheritedInstallHelperAfterRecoveryCleanupRootOpen(cleanupPath)
	}
	transactionsMount, transactionsMountErr := installRootMountIdentity(transactionsRoot)
	cleanupMount, cleanupMountErr := installRootMountIdentity(cleanupRoot)
	if transactionsMountErr != nil || cleanupMountErr != nil || transactionsMount != cleanupMount {
		return errors.Join(
			errors.New("install recovery cleanup root crosses a mount boundary"),
			transactionsMountErr,
			cleanupMountErr,
		)
	}
	validateRoot := func() error {
		return validatePinnedInstallRecoveryCleanupRoot(
			transactionsRoot, transactionsPath, cleanupBase, cleanupRoot, cleanupInfo, cleanupMount,
		)
	}
	if err := validateRoot(); err != nil {
		return err
	}
	topLevelFiles, directories, err := installRecoveryCleanupAllowlist()
	if err != nil {
		return err
	}
	allowedTopLevel := make(map[string]struct{}, len(topLevelFiles)+len(directories))
	for _, name := range topLevelFiles {
		allowedTopLevel[name] = struct{}{}
	}
	for _, directory := range directories {
		allowedTopLevel[directory.name] = struct{}{}
	}
	if err := validateInstallRecoveryCleanupNames(cleanupRoot, allowedTopLevel); err != nil {
		return err
	}
	for _, name := range topLevelFiles {
		if err := removePinnedInstallRecoveryRegular(cleanupRoot, name, validateRoot); err != nil {
			return fmt.Errorf("clean install recovery file %s: %w", name, err)
		}
	}
	for _, directory := range directories {
		if err := removePinnedInstallRecoveryDirectory(cleanupRoot, cleanupPath, directory, cleanupMount, validateRoot); err != nil {
			return fmt.Errorf("clean install recovery directory %s: %w", directory.name, err)
		}
	}
	if err := errors.Join(validateRoot(), validateInstallRecoveryCleanupNames(cleanupRoot, allowedTopLevel)); err != nil {
		return err
	}
	if err := cleanupRoot.Close(); err != nil {
		return err
	}
	cleanupRootOpen = false
	current, err := transactionsRoot.Lstat(cleanupBase)
	if err != nil || !current.IsDir() || !os.SameFile(cleanupInfo, current) {
		return errors.Join(errors.New("install recovery cleanup root changed before final removal"), err)
	}
	if err := revalidateInstallHelperParent(transactionsRoot, transactionsPath); err != nil {
		return err
	}
	if err := validateEmptyInstallHelperDirectory(transactionsRoot, cleanupBase, cleanupInfo); err != nil {
		return err
	}
	if err := removePinnedInstallBackupEntryExact(transactionsRoot, cleanupBase, cleanupInfo); err != nil {
		return err
	}
	return revalidateInstallHelperParent(transactionsRoot, transactionsPath)
}

func installRecoveryCleanupAllowlist() ([]string, []installRecoveryCleanupDirectory, error) {
	topLevel := []string{installTransactionMetadataBackup, installTransactionChecksumManifest}
	directoryIndexes := make(map[string]int)
	directories := make([]installRecoveryCleanupDirectory, 0, 2)
	for _, slashPath := range threadpointBundleEntries {
		path := filepath.FromSlash(slashPath)
		if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, nil, fmt.Errorf("invalid managed bundle cleanup entry %q", slashPath)
		}
		directory, name := filepath.Dir(path), filepath.Base(path)
		if directory == "." {
			topLevel = append(topLevel, name)
			continue
		}
		if filepath.Clean(directory) != filepath.Base(directory) || name == "." {
			return nil, nil, fmt.Errorf("managed bundle cleanup entry is too deeply nested: %q", slashPath)
		}
		index, ok := directoryIndexes[directory]
		if !ok {
			index = len(directories)
			directoryIndexes[directory] = index
			directories = append(directories, installRecoveryCleanupDirectory{name: directory})
		}
		directories[index].entries = append(directories[index].entries, name)
	}
	return topLevel, directories, nil
}

func validateInstallRecoveryCleanupNames(root *os.Root, allowed map[string]struct{}) error {
	if root == nil {
		return errors.New("install recovery cleanup root is not retained")
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(len(allowed) + 1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(entries) > len(allowed) {
		return errors.New("install recovery cleanup root contains too many entries")
	}
	for _, entry := range entries {
		if _, ok := allowed[entry.Name()]; !ok {
			return fmt.Errorf("install recovery cleanup refuses unexpected entry %q", entry.Name())
		}
	}
	return nil
}

func validatePinnedInstallRecoveryCleanupRoot(
	transactionsRoot *os.Root,
	transactionsPath, cleanupBase string,
	cleanupRoot *os.Root,
	expected os.FileInfo,
	expectedMount uint64,
) error {
	if transactionsRoot == nil || cleanupRoot == nil || expected == nil {
		return errors.New("install recovery cleanup roots are not retained")
	}
	if err := revalidateInstallHelperParent(transactionsRoot, transactionsPath); err != nil {
		return err
	}
	opened, openErr := cleanupRoot.Stat(".")
	current, currentErr := transactionsRoot.Lstat(cleanupBase)
	currentMount, mountErr := installRootMountIdentity(cleanupRoot)
	if openErr != nil || currentErr != nil || !opened.IsDir() || !current.IsDir() ||
		opened.Mode()&os.ModeSymlink != 0 || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, current) ||
		mountErr != nil || currentMount != expectedMount {
		return errors.Join(errors.New("install recovery cleanup root changed while retained or crossed a mount boundary"), openErr, currentErr, mountErr)
	}
	return nil
}

func removePinnedInstallRecoveryRegular(root *os.Root, name string, validate func() error) error {
	if err := validate(); err != nil {
		return err
	}
	expected, err := snapshotRegularInstallRemoval(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := quarantineAndRemoveInstallEntry(root, name, expected, validate); err != nil {
		return err
	}
	return validate()
}

func removePinnedInstallRecoveryDirectory(
	root *os.Root,
	cleanupPath string,
	directory installRecoveryCleanupDirectory,
	expectedMount uint64,
	validateRoot func() error,
) (returnErr error) {
	if err := validateRoot(); err != nil {
		return err
	}
	expected, err := root.Lstat(directory.name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New("install recovery cleanup child is not a physical directory"), err)
	}
	child, err := root.OpenRoot(directory.name)
	if err != nil {
		return err
	}
	childOpen := true
	defer func() {
		if childOpen {
			returnErr = errors.Join(returnErr, child.Close())
		}
	}()
	if inheritedInstallHelperAfterRecoveryCleanupChildOpen != nil {
		inheritedInstallHelperAfterRecoveryCleanupChildOpen(filepath.Join(cleanupPath, directory.name))
	}
	validateChild := func() error {
		if err := validateRoot(); err != nil {
			return err
		}
		opened, openErr := child.Stat(".")
		current, currentErr := root.Lstat(directory.name)
		childMount, mountErr := installRootMountIdentity(child)
		if openErr != nil || currentErr != nil || !opened.IsDir() || !current.IsDir() ||
			opened.Mode()&os.ModeSymlink != 0 || current.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(expected, opened) || !os.SameFile(opened, current) ||
			mountErr != nil || childMount != expectedMount {
			return errors.Join(errors.New("install recovery cleanup child changed while retained or crossed a mount boundary"), openErr, currentErr, mountErr)
		}
		return nil
	}
	if err := validateChild(); err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(directory.entries))
	for _, name := range directory.entries {
		allowed[name] = struct{}{}
	}
	if err := validateInstallRecoveryCleanupNames(child, allowed); err != nil {
		return err
	}
	for _, name := range directory.entries {
		if err := removePinnedInstallRecoveryRegular(child, name, validateChild); err != nil {
			return err
		}
	}
	if err := errors.Join(validateChild(), validateInstallRecoveryCleanupNames(child, allowed)); err != nil {
		return err
	}
	if err := child.Close(); err != nil {
		return err
	}
	childOpen = false
	if err := validateRoot(); err != nil {
		return err
	}
	current, err := root.Lstat(directory.name)
	if err != nil || !current.IsDir() || !os.SameFile(expected, current) {
		return errors.Join(errors.New("install recovery cleanup child changed before final removal"), err)
	}
	if err := validateEmptyInstallHelperDirectory(root, directory.name, expected); err != nil {
		return err
	}
	if err := removePinnedInstallBackupEntryExact(root, directory.name, expected); err != nil {
		return err
	}
	return validateRoot()
}

func runInstallEntryInspect(stdout io.Writer, args []string) (returnErr error) {
	if len(args) != 1 {
		return errors.New("invalid inherited install entry inspect invocation")
	}
	root, parent, base, err := openPinnedInstallHelperParent(args[0])
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	before, err := root.Lstat(base)
	if err != nil {
		return err
	}
	identity, err := installRemovalIdentityString(before)
	if err != nil {
		return err
	}
	var kind, proof string
	switch {
	case before.Mode().IsRegular():
		current, err := snapshotRegularInstallRemoval(root, base)
		if err != nil || !os.SameFile(before, current.info) {
			return errors.Join(errors.New("install regular entry changed during inspection"), err)
		}
		kind, proof = installGenerationKindRegular, current.digest
	case before.Mode()&os.ModeSymlink != 0:
		current, err := snapshotSymlinkInstallRemoval(root, base)
		if err != nil || !os.SameFile(before, current.info) {
			return errors.Join(errors.New("install symlink changed during inspection"), err)
		}
		digest := sha256.Sum256([]byte(current.linkTarget))
		kind, proof = installGenerationKindSymlink, hex.EncodeToString(digest[:])
	case before.IsDir() && before.Mode()&os.ModeSymlink == 0:
		current, err := root.Lstat(base)
		if err != nil || !current.IsDir() || !os.SameFile(before, current) {
			return errors.Join(errors.New("install directory changed during inspection"), err)
		}
		kind, proof = "directory", "-"
	default:
		return errors.New("install entry inspection refuses special filesystem entries")
	}
	if err := revalidateInstallHelperParent(root, parent); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s %s %s\n", identity, kind, proof)
	return err
}

func runInstallRegularSnapshot(stdout io.Writer, args []string) (returnErr error) {
	if len(args) != 1 {
		return errors.New("invalid inherited install regular snapshot invocation")
	}
	root, parent, base, err := openPinnedInstallHelperParent(args[0])
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	before, err := root.Lstat(base)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New("install regular snapshot requires a regular file"), err)
	}
	current, err := snapshotRegularInstallRemoval(root, base)
	if err != nil || !os.SameFile(before, current.info) {
		return errors.Join(errors.New("install regular entry changed during snapshot"), err)
	}
	if err := revalidateInstallHelperParent(root, parent); err != nil {
		return err
	}
	identity, err := installRemovalIdentityString(current.info)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s %s %d\n", identity, current.digest, current.info.Size())
	return err
}

type pinnedInstallLockOwner struct {
	root     *os.Root
	file     *os.File
	parent   string
	base     string
	identity string
	info     os.FileInfo
	owner    installLockOwner
}

func openPinnedInstallLockOwner(path string, writable bool) (_ *pinnedInstallLockOwner, returnErr error) {
	root, parent, base, err := openPinnedInstallHelperParent(path)
	if err != nil {
		return nil, err
	}
	cleanupRoot := true
	defer func() {
		if cleanupRoot {
			returnErr = errors.Join(returnErr, root.Close())
		}
	}()
	before, err := root.Lstat(base)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > maxInstallLockOwnerBytes {
		return nil, errors.Join(errors.New("install lock owner is not a bounded regular file"), err)
	}
	flags := os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
	if writable {
		flags = os.O_RDWR | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
	}
	file, err := root.OpenFile(base, flags, 0)
	if err != nil {
		return nil, err
	}
	cleanupFile := true
	defer func() {
		if cleanupFile {
			returnErr = errors.Join(returnErr, file.Close())
		}
	}()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.Join(errors.New("install lock owner changed while opening"), err)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxInstallLockOwnerBytes+1))
	if err != nil || int64(len(body)) > maxInstallLockOwnerBytes {
		return nil, errors.Join(errors.New("install lock owner exceeds its size limit"), err)
	}
	if err := rejectDuplicateInstallJSONKeys(body); err != nil {
		return nil, fmt.Errorf("malformed install lock owner: %w", err)
	}
	var owner installLockOwner
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&owner); err != nil {
		return nil, fmt.Errorf("malformed install lock owner: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("malformed install lock owner: %w", err)
	}
	if err := validateInstallLockOwner(owner); err != nil {
		return nil, err
	}
	identity, err := installRemovalIdentityString(opened)
	if err != nil {
		return nil, err
	}
	pinned := &pinnedInstallLockOwner{
		root: root, file: file, parent: parent, base: base,
		identity: identity, info: opened, owner: owner,
	}
	if err := pinned.Validate(); err != nil {
		return nil, err
	}
	cleanupRoot = false
	cleanupFile = false
	return pinned, nil
}

func validateInstallLockOwner(owner installLockOwner) error {
	if owner.PID <= 0 || owner.Created.IsZero() || owner.Heartbeat.IsZero() ||
		!filepath.IsAbs(owner.Command) || filepath.Clean(owner.Command) != owner.Command || !validInstallLockToken(owner.Token) {
		return errors.New("install lock owner fields are invalid")
	}
	_, createdOffset := owner.Created.Zone()
	_, heartbeatOffset := owner.Heartbeat.Zone()
	if createdOffset != 0 || heartbeatOffset != 0 {
		return errors.New("install lock owner timestamps must be UTC")
	}
	return nil
}

func validInstallLockToken(token string) bool {
	if len(token) != 32 && len(token) != 64 {
		return false
	}
	for _, char := range token {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func (pinned *pinnedInstallLockOwner) Validate() error {
	if pinned == nil || pinned.root == nil || pinned.file == nil || pinned.info == nil {
		return errors.New("install lock owner generation is not pinned")
	}
	opened, openErr := pinned.file.Stat()
	current, currentErr := pinned.root.Lstat(pinned.base)
	if openErr != nil || currentErr != nil || !opened.Mode().IsRegular() ||
		!os.SameFile(pinned.info, opened) || !os.SameFile(opened, current) {
		return errors.Join(errors.New("install lock owner generation changed"), openErr, currentErr)
	}
	return revalidateInstallHelperParent(pinned.root, pinned.parent)
}

func (pinned *pinnedInstallLockOwner) Close() error {
	if pinned == nil {
		return nil
	}
	fileErr := pinned.file.Close()
	rootErr := pinned.root.Close()
	pinned.file = nil
	pinned.root = nil
	return errors.Join(fileErr, rootErr)
}

func runInstallLockOwnerInspect(stdout io.Writer, args []string) (returnErr error) {
	if len(args) != 1 {
		return errors.New("invalid inherited install lock owner invocation")
	}
	pinned, err := openPinnedInstallLockOwner(args[0], false)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, pinned.Close()) }()
	if inheritedInstallHelperAfterLockOwnerSnapshot != nil {
		inheritedInstallHelperAfterLockOwnerSnapshot(args[0])
	}
	if err := pinned.Validate(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s %d %s\n", pinned.identity, pinned.owner.PID, pinned.owner.Token)
	return err
}

func runInstallLockHeartbeat(args []string) (returnErr error) {
	if len(args) != 4 || !validInstallHelperIdentity(args[1]) || !validInstallLockToken(args[2]) {
		return errors.New("invalid inherited install lock heartbeat invocation")
	}
	heartbeat, err := time.Parse(time.RFC3339, args[3])
	if err != nil {
		return fmt.Errorf("invalid install lock heartbeat: %w", err)
	}
	_, offset := heartbeat.Zone()
	if offset != 0 {
		return errors.New("install lock heartbeat must be UTC")
	}
	pinned, err := openPinnedInstallLockOwner(args[0], true)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, pinned.Close()) }()
	if pinned.identity != args[1] || pinned.owner.Token != args[2] {
		return errors.New("install lock heartbeat does not own the expected generation")
	}
	if inheritedInstallHelperAfterLockOwnerSnapshot != nil {
		inheritedInstallHelperAfterLockOwnerSnapshot(args[0])
	}
	if err := pinned.Validate(); err != nil {
		return err
	}
	pinned.owner.Heartbeat = heartbeat.UTC()
	body, err := json.Marshal(pinned.owner)
	if err != nil {
		return err
	}
	if err := rewriteInstallLockFile(pinned.file, append(body, '\n')); err != nil {
		return err
	}
	return pinned.Validate()
}

func removeEmptyInstallHelperDirectory(root *os.Root, parent, base string, expected os.FileInfo) error {
	if err := validateEmptyInstallHelperDirectory(root, base, expected); err != nil {
		return err
	}
	if err := revalidateInstallHelperParent(root, parent); err != nil {
		return err
	}
	var quarantine string
	for range 16 {
		var suffix [12]byte
		_, _ = rand.Read(suffix[:])
		quarantine = ".remove-as-" + hex.EncodeToString(suffix[:])
		if err := safefs.RenameRootNoReplace(root, base, quarantine); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return err
		}
		break
	}
	if quarantine == "" {
		return errors.New("could not allocate install directory removal quarantine")
	}
	restore := func(cause error) error {
		return errors.Join(cause, safefs.RenameRootNoReplace(root, quarantine, base), syncInstallRootDirectory(root))
	}
	moved, err := root.Lstat(quarantine)
	if err != nil || !moved.IsDir() || moved.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, moved) {
		return restore(errors.Join(errors.New("install directory changed during quarantine"), err))
	}
	if err := validateEmptyInstallHelperDirectory(root, quarantine, expected); err != nil {
		return restore(err)
	}
	if err := revalidateInstallHelperParent(root, parent); err != nil {
		return restore(err)
	}
	if err := syncInstallRootDirectory(root); err != nil {
		return err
	}
	if err := root.Remove(quarantine); err != nil {
		return restore(err)
	}
	return syncInstallRootDirectory(root)
}

func validateEmptyInstallHelperDirectory(root *os.Root, base string, expected os.FileInfo) error {
	directory, err := root.OpenFile(base, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	opened, statErr := directory.Stat()
	entries, readErr := directory.ReadDir(1)
	closeErr := directory.Close()
	current, currentErr := root.Lstat(base)
	if statErr != nil || currentErr != nil || !opened.IsDir() || !current.IsDir() ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, current) {
		return errors.Join(errors.New("install directory changed while checking emptiness"), statErr, currentErr, closeErr)
	}
	if len(entries) != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return errors.Join(errors.New("install remove-as refuses a non-empty directory"), readErr, closeErr)
	}
	return closeErr
}

func openPinnedInstallHelperParent(path string) (*os.Root, string, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\r\n\x00") {
		return nil, "", "", errors.New("installer helper path must be canonical and absolute")
	}
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return nil, "", "", errors.New("installer helper path must name a child")
	}
	parentPath := filepath.Dir(path)
	if err := safefs.RejectExistingSymlinkAncestors(parentPath); err != nil {
		return nil, "", "", err
	}
	// #nosec G703 -- parentPath is canonical, absolute, and has had every
	// existing symlink ancestor rejected immediately above.
	before, err := os.Lstat(parentPath)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, "", "", errors.Join(errors.New("installer helper parent is not a physical directory"), err)
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, "", "", err
	}
	if inheritedInstallHelperAfterParentOpen != nil {
		inheritedInstallHelperAfterParentOpen(parentPath)
	}
	opened, openErr := root.Stat(".")
	// #nosec G703 -- re-read the same validated path to prove that the opened
	// directory still names the pre-open filesystem object.
	after, afterErr := os.Lstat(parentPath)
	if openErr != nil || afterErr != nil || !opened.IsDir() || !after.IsDir() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return nil, "", "", errors.Join(errors.New("installer helper parent changed while it was opened"), openErr, afterErr, root.Close())
	}
	return root, parentPath, base, nil
}

func revalidateInstallHelperParent(root *os.Root, path string) error {
	opened, openErr := root.Stat(".")
	// #nosec G703 -- path is the canonical, absolute parent retained by
	// openPinnedInstallHelperParent; this re-read verifies its pinned identity.
	current, currentErr := os.Lstat(path)
	if openErr != nil || currentErr != nil || !opened.IsDir() || !current.IsDir() ||
		current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		return errors.Join(errors.New("installer helper parent changed after it was opened"), openErr, currentErr)
	}
	return nil
}

type installHelperJournal struct {
	journal installTransactionJournal
	body    []byte
	digest  string
	info    os.FileInfo
	root    *os.Root
	parent  string
	base    string
}

func runInstallJournalInspect(stdout io.Writer, args []string) (returnErr error) {
	if len(args) != 2 {
		return errors.New("invalid inherited install journal inspect invocation")
	}
	loaded, err := loadInstallHelperJournal(args[0], "", args[1])
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, loaded.root.Close()) }()
	identity, err := installRemovalIdentityString(loaded.info)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s %s\n", identity, loaded.digest)
	return err
}

func runInstallJournalHelper(stdout io.Writer, args []string) (returnErr error) {
	if len(args) < 3 {
		return errors.New("invalid inherited install journal invocation")
	}
	if len(args[1]) != sha256.Size*2 {
		return errors.New("install journal digest is required")
	}
	action := args[2]
	expectedPath := args[0]
	if action == "validate-as" || action == "backup-prior-as" {
		if len(args) != 4 {
			return fmt.Errorf("invalid journal %s invocation", action)
		}
		expectedPath = args[3]
	}
	loaded, err := loadInstallHelperJournal(args[0], args[1], expectedPath)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, loaded.root.Close()) }()
	switch action {
	case "validate":
		if len(args) != 3 {
			return errors.New("invalid journal validate invocation")
		}
		return nil
	case "validate-as":
		return nil
	case "value":
		if len(args) != 4 {
			return errors.New("invalid journal value invocation")
		}
		return writeInstallJournalValue(stdout, loaded.journal, args[3])
	case "generation":
		if len(args) != 5 {
			return errors.New("invalid journal generation invocation")
		}
		return writeInstallJournalGeneration(stdout, loaded.journal, args[3], args[4])
	case "backup-prior":
		if len(args) != 3 {
			return errors.New("invalid journal backup invocation")
		}
		return validateInstallJournalBackupPrior(loaded.journal)
	case "backup-prior-as":
		return validateInstallJournalBackupPrior(loaded.journal)
	case "matches":
		if len(args) != 6 {
			return errors.New("invalid journal generation match invocation")
		}
		generation, err := installHelperJournalGeneration(loaded.journal, args[3], args[4])
		if err != nil {
			return err
		}
		if err := validateInstallHelperCandidatePath(loaded.journal, args[3], args[5]); err != nil {
			return err
		}
		return installHelperGenerationMatchesPath(args[5], generation)
	case "remove-matched":
		if len(args) != 6 {
			return errors.New("invalid matched journal generation removal invocation")
		}
		return removeMatchedInstallHelperGeneration(loaded, args[3], args[4], args[5])
	case installJournalActionUpdateFrom, installJournalActionUpdateExpect:
		expectedUpdate := action == installJournalActionUpdateExpect
		if (!expectedUpdate && len(args) != 8) || (expectedUpdate && len(args) != 10) {
			return errors.New("invalid journal generation update invocation")
		}
		identity, err := installRemovalIdentityString(loaded.info)
		if err != nil || identity != args[3] {
			return errors.Join(errors.New("install journal identity changed before generation update"), err)
		}
		generation, err := snapshotInstallHelperGeneration(loaded.journal, args[4], args[6], args[7])
		if err != nil {
			return err
		}
		if expectedUpdate && (generation.Identity != args[8] || generation.Digest != args[9]) {
			return errors.New("install generation source does not match its expected copied identity and digest")
		}
		if err := updateInstallHelperTarget(&loaded.journal, args[4], args[5], generation); err != nil {
			return err
		}
		newIdentity, newDigest, err := replaceInstallHelperJournal(loaded)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s %s\n", newIdentity, newDigest)
		return err
	case "commit":
		if len(args) != 5 || (args[3] != "normal" && args[3] != installJournalFieldRecovery) {
			return errors.New("invalid journal commit invocation")
		}
		identity, err := installRemovalIdentityString(loaded.info)
		if err != nil || identity != args[4] {
			return errors.Join(errors.New("install journal identity changed before commit"), err)
		}
		return commitInstallHelperJournal(loaded, args[3])
	default:
		return fmt.Errorf("unsupported install journal helper action %q", action)
	}
}

func loadInstallHelperJournal(path, expectedDigest, expectedPath string) (installHelperJournal, error) {
	if expectedDigest != "" && len(expectedDigest) != sha256.Size*2 {
		return installHelperJournal{}, errors.New("install journal digest is invalid")
	}
	if !canonicalInstallHelperPath(expectedPath) {
		return installHelperJournal{}, errors.New("expected install journal path must be canonical and absolute")
	}
	root, parent, base, err := openPinnedInstallHelperParent(path)
	if err != nil {
		return installHelperJournal{}, err
	}
	fail := func(cause error) (installHelperJournal, error) {
		return installHelperJournal{}, errors.Join(cause, root.Close())
	}
	before, err := root.Lstat(base)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > maxInstallTransactionBytes {
		return fail(errors.Join(errors.New("install journal is not a bounded regular file"), err))
	}
	file, err := root.OpenFile(base, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fail(err)
	}
	opened, statErr := file.Stat()
	body, readErr := io.ReadAll(io.LimitReader(file, maxInstallTransactionBytes+1))
	finished, finishedErr := file.Stat()
	closeErr := file.Close()
	after, afterErr := root.Lstat(base)
	if statErr != nil || readErr != nil || finishedErr != nil || closeErr != nil || afterErr != nil || int64(len(body)) > maxInstallTransactionBytes ||
		!opened.Mode().IsRegular() || !finished.Mode().IsRegular() || !after.Mode().IsRegular() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, finished) || !os.SameFile(finished, after) ||
		opened.Size() != finished.Size() || !opened.ModTime().Equal(finished.ModTime()) {
		return fail(errors.Join(errors.New("install journal changed while it was read"), statErr, readErr, finishedErr, closeErr, afterErr))
	}
	digest := sha256.Sum256(body)
	digestText := hex.EncodeToString(digest[:])
	if expectedDigest != "" && digestText != expectedDigest {
		return fail(errors.New("install journal digest changed before helper validation"))
	}
	journal, err := decodeInstallHelperJournalStrict(body)
	if err != nil {
		return fail(err)
	}
	productHome := filepath.Dir(filepath.Dir(filepath.Dir(journal.BackupRoot)))
	if err := validateInstallTransactionJournal(productHome, journal.CommandPath, expectedPath, journal); err != nil {
		return fail(err)
	}
	if err := validateInstallHelperJournalContract(journal, body); err != nil {
		return fail(err)
	}
	current, currentErr := root.Lstat(base)
	if currentErr != nil || !os.SameFile(before, current) {
		return fail(errors.Join(errors.New("install journal changed during helper validation"), currentErr))
	}
	if err := revalidateInstallHelperParent(root, parent); err != nil {
		return fail(err)
	}
	return installHelperJournal{journal: journal, body: body, digest: digestText, info: before, root: root, parent: parent, base: base}, nil
}

func decodeInstallHelperJournalStrict(body []byte) (installTransactionJournal, error) {
	if err := rejectDuplicateInstallJSONKeys(body); err != nil {
		return installTransactionJournal{}, fmt.Errorf("malformed install transaction journal: %w", err)
	}
	var journal installTransactionJournal
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return installTransactionJournal{}, fmt.Errorf("malformed install transaction journal: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return installTransactionJournal{}, fmt.Errorf("malformed install transaction journal: %w", err)
	}
	return journal, nil
}

func rejectDuplicateInstallJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return fmt.Errorf("duplicate or invalid JSON object key %q", key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.Join(errors.New("unterminated JSON object"), err)
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.Join(errors.New("unterminated JSON array"), err)
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateInstallHelperJournalContract(journal installTransactionJournal, body []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return err
	}
	required := []string{"schema_version", "operation", "command_path", "metadata_path", "bundle_root", "backup_root", "previous_install", "link_existed", "created_at", "targets"}
	if len(top) != len(required) {
		return errors.New("install transaction journal top-level fields are not exact")
	}
	for _, key := range required {
		if _, ok := top[key]; !ok {
			return fmt.Errorf("install transaction journal is missing %s", key)
		}
	}
	if journal.CreatedAt.IsZero() {
		return errors.New("install transaction journal creation time is required")
	}
	for _, path := range []string{journal.CommandPath, journal.MetadataPath, journal.BundleRoot, journal.BackupRoot} {
		if !canonicalInstallHelperPath(path) {
			return fmt.Errorf("install transaction journal path is not canonical: %q", path)
		}
	}
	var rawTargets map[string]json.RawMessage
	if err := json.Unmarshal(top["targets"], &rawTargets); err != nil {
		return err
	}
	for key, rawTarget := range rawTargets {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawTarget, &fields); err != nil {
			return err
		}
		if _, ok := fields["prior"]; !ok || len(fields) > 3 {
			return fmt.Errorf("install transaction target fields are not exact: %s", key)
		}
		for field := range fields {
			if field != "prior" && field != installJournalFieldProduced && field != installJournalFieldRecovery {
				return fmt.Errorf("install transaction target contains unknown field %s", field)
			}
			if err := validateInstallHelperGenerationJSON(fields[field]); err != nil {
				return fmt.Errorf("install transaction target %s %s: %w", key, field, err)
			}
		}
		target := journal.Targets[key]
		if journal.PreviousInstall && key != installJournalTargetLink && target.Prior.Kind != installGenerationKindRegular {
			return fmt.Errorf("install transaction prior generation is incomplete: %s", key)
		}
		if !journal.PreviousInstall && target.Prior.Kind != installGenerationKindAbsent {
			return fmt.Errorf("initial install unexpectedly owns a prior generation: %s", key)
		}
	}
	link := journal.Targets[installJournalTargetLink].Prior
	if journal.LinkExisted != (link.Kind == installGenerationKindSymlink) ||
		(link.Kind != installGenerationKindSymlink && link.Kind != installGenerationKindAbsent) {
		return errors.New("install transaction link state contradicts its prior generation")
	}
	return nil
}

func validateInstallHelperGenerationJSON(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	var kind string
	if err := json.Unmarshal(fields["kind"], &kind); err != nil {
		return errors.Join(errors.New("generation kind is required"), err)
	}
	want := map[string]bool{"kind": true}
	switch kind {
	case "":
		return errors.New("generation kind is required")
	case installGenerationKindAbsent:
	case installGenerationKindRegular:
		want["identity"] = true
		want["digest"] = true
	case installGenerationKindSymlink:
		want["identity"] = true
		want["link_target"] = true
	default:
		return fmt.Errorf("unsupported generation kind %q", kind)
	}
	if len(fields) != len(want) {
		return errors.New("generation fields are not exact")
	}
	for key := range want {
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("generation is missing %s", key)
		}
	}
	return nil
}

func canonicalInstallHelperPath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !utf8.ValidString(path) {
		return false
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f || r == '\\' || r == '"' {
			return false
		}
	}
	return true
}

func writeInstallJournalValue(stdout io.Writer, journal installTransactionJournal, field string) error {
	var value string
	switch field {
	case "schema_version":
		value = journal.SchemaVersion
	case "operation":
		value = string(journal.Operation)
	case "command_path":
		value = journal.CommandPath
	case "metadata_path":
		value = journal.MetadataPath
	case "bundle_root":
		value = journal.BundleRoot
	case "backup_root":
		value = journal.BackupRoot
	case "previous_install":
		value = strconv.FormatBool(journal.PreviousInstall)
	case "link_existed":
		value = strconv.FormatBool(journal.LinkExisted)
	default:
		return fmt.Errorf("unsupported install journal value %q", field)
	}
	_, err := fmt.Fprintln(stdout, value)
	return err
}

func writeInstallJournalGeneration(stdout io.Writer, journal installTransactionJournal, key, field string) error {
	generation, err := installHelperJournalGeneration(journal, key, field)
	if err != nil {
		return err
	}
	if generation.Kind == "" {
		return errors.New("requested install journal generation is absent")
	}
	body, err := json.Marshal(generation)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(body))
	return err
}

func installHelperJournalGeneration(journal installTransactionJournal, key, field string) (installJournalGeneration, error) {
	target, ok := journal.Targets[key]
	if !ok {
		return installJournalGeneration{}, fmt.Errorf("unknown install journal target %q", key)
	}
	switch field {
	case "prior":
		return target.Prior, nil
	case installJournalFieldProduced:
		return target.Produced, nil
	case installJournalFieldRecovery:
		return target.Recovery, nil
	default:
		return installJournalGeneration{}, fmt.Errorf("unknown install journal generation %q", field)
	}
}

func installHelperTargetPath(journal installTransactionJournal, key string) (string, error) {
	switch {
	case key == installJournalTargetLink:
		return journal.CommandPath, nil
	case key == "metadata":
		return journal.MetadataPath, nil
	case strings.HasPrefix(key, "bundle:"):
		return filepath.Join(journal.BundleRoot, filepath.FromSlash(strings.TrimPrefix(key, "bundle:"))), nil
	default:
		return "", fmt.Errorf("unknown install journal target %q", key)
	}
}

func validateInstallHelperCandidatePath(journal installTransactionJournal, key, candidate string) error {
	target, err := installHelperTargetPath(journal, key)
	if err != nil {
		return err
	}
	if !canonicalInstallHelperPath(candidate) {
		return errors.New("install generation candidate is outside its target parent")
	}
	targetParent := filepath.Dir(target)
	candidateParent := filepath.Dir(candidate)
	if candidateParent == targetParent {
		return nil
	}
	if filepath.Dir(candidateParent) != targetParent || filepath.Base(candidate) != "candidate" ||
		!validInstallHelperPrivateDirectoryName(filepath.Base(candidateParent)) {
		return errors.New("install generation candidate is outside its target parent")
	}
	// #nosec G703 -- candidateParent is constrained above to an exact private
	// direct child of the journal-owned target parent.
	before, err := os.Lstat(candidateParent)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o077 != 0 || before.Mode().Perm()&0o700 != 0o700 {
		return errors.Join(errors.New("install generation candidate parent is not a private physical directory"), err)
	}
	root, err := os.OpenRoot(candidateParent)
	if err != nil {
		return err
	}
	opened, openErr := root.Stat(".")
	// #nosec G703 -- re-read the same constrained path to verify the opened
	// private directory retained its pre-open filesystem identity.
	after, afterErr := os.Lstat(candidateParent)
	closeErr := root.Close()
	if openErr != nil || afterErr != nil || !opened.IsDir() || !after.IsDir() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) || opened.Mode().Perm()&0o077 != 0 || opened.Mode().Perm()&0o700 != 0o700 {
		return errors.Join(errors.New("install generation candidate parent changed while it was opened"), openErr, afterErr, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func validInstallHelperPrivateDirectoryName(name string) bool {
	prefixes := []string{".install-detach-", ".install-state-detach-", ".install-remove-", ".link-recover-"}
	for _, prefix := range prefixes {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if len(suffix) != 6 {
			return false
		}
		for _, r := range suffix {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
				return false
			}
		}
		return true
	}
	return false
}

func snapshotInstallHelperGeneration(journal installTransactionJournal, key, kind, source string) (installJournalGeneration, error) {
	if kind == installGenerationKindAbsent {
		if source != "-" {
			return installJournalGeneration{}, errors.New("absent generation must use the '-' source marker")
		}
		return installJournalGeneration{Kind: installGenerationKindAbsent}, nil
	}
	if kind != installGenerationKindRegular && kind != installGenerationKindSymlink {
		return installJournalGeneration{}, fmt.Errorf("unsupported install generation source kind %q", kind)
	}
	if err := validateInstallHelperCandidatePath(journal, key, source); err != nil {
		return installJournalGeneration{}, err
	}
	root, parent, base, err := openPinnedInstallHelperParent(source)
	if err != nil {
		return installJournalGeneration{}, err
	}
	defer root.Close()
	var identity installRemovalIdentity
	if kind == installGenerationKindRegular {
		identity, err = snapshotRegularInstallRemoval(root, base)
	} else {
		identity, err = snapshotSymlinkInstallRemoval(root, base)
	}
	if err != nil {
		return installJournalGeneration{}, err
	}
	if err := revalidateInstallHelperParent(root, parent); err != nil {
		return installJournalGeneration{}, err
	}
	recorded, err := recordedInstallGeneration(identity)
	if err != nil {
		return installJournalGeneration{}, err
	}
	if inheritedInstallHelperAfterGenerationSnapshot != nil {
		inheritedInstallHelperAfterGenerationSnapshot(source)
	}
	var current installRemovalIdentity
	if kind == installGenerationKindRegular {
		current, err = snapshotRegularInstallRemoval(root, base)
	} else {
		current, err = snapshotSymlinkInstallRemoval(root, base)
	}
	if err != nil || !installGenerationMatches(current, recorded) {
		return installJournalGeneration{}, errors.Join(errors.New("install generation source changed during snapshot"), err)
	}
	if err := revalidateInstallHelperParent(root, parent); err != nil {
		return installJournalGeneration{}, err
	}
	return recorded, nil
}

func updateInstallHelperTarget(journal *installTransactionJournal, key, field string, generation installJournalGeneration) error {
	if journal == nil {
		return errors.New("install journal is required")
	}
	target, ok := journal.Targets[key]
	if !ok {
		return fmt.Errorf("unknown install journal target %q", key)
	}
	switch field {
	case "prior":
		target.Prior = generation
		target.Recovery = installJournalGeneration{}
	case installJournalFieldProduced:
		target.Produced = generation
	case installJournalFieldRecovery:
		target.Recovery = generation
	default:
		return fmt.Errorf("unknown install journal generation %q", field)
	}
	journal.Targets[key] = target
	return nil
}

func replaceInstallHelperJournal(loaded installHelperJournal) (returnIdentity, returnDigest string, returnErr error) {
	body, err := json.Marshal(loaded.journal)
	if err != nil || int64(len(body)) > maxInstallTransactionBytes {
		return "", "", errors.Join(errors.New("updated install journal exceeds its size limit"), err)
	}
	decoded, err := decodeInstallHelperJournalStrict(body)
	if err != nil {
		return "", "", err
	}
	productHome := filepath.Dir(filepath.Dir(filepath.Dir(decoded.BackupRoot)))
	expectedPath := filepath.Join(loaded.parent, loaded.base)
	if err := validateInstallTransactionJournal(productHome, decoded.CommandPath, expectedPath, decoded); err != nil {
		return "", "", err
	}
	if err := validateInstallHelperJournalContract(decoded, body); err != nil {
		return "", "", err
	}

	var tempName string
	var temp *os.File
	for range 16 {
		var suffix [12]byte
		_, _ = rand.Read(suffix[:])
		tempName = ".transaction-update-" + hex.EncodeToString(suffix[:])
		temp, err = loaded.root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		break
	}
	if temp == nil {
		return "", "", errors.New("could not allocate install journal update")
	}
	created, err := temp.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return "", "", errors.Join(errors.New("install journal update is not a regular file"), err, temp.Close())
	}
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		current, err := loaded.root.Lstat(tempName)
		if err == nil && os.SameFile(created, current) {
			returnErr = errors.Join(returnErr, loaded.root.Remove(tempName))
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	if written, err := io.Copy(temp, bytes.NewReader(body)); err != nil || written != int64(len(body)) {
		return "", "", errors.Join(errors.New("short install journal update write"), err, temp.Close())
	}
	if err := temp.Chmod(0o600); err != nil {
		return "", "", errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return "", "", errors.Join(err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return "", "", err
	}
	newIdentity, err := snapshotRegularInstallRemoval(loaded.root, tempName)
	if err != nil || !os.SameFile(created, newIdentity.info) {
		return "", "", errors.Join(errors.New("install journal update changed before publication"), err)
	}
	newDigest := sha256.Sum256(body)
	newDigestText := hex.EncodeToString(newDigest[:])
	if newIdentity.digest != newDigestText {
		return "", "", errors.New("install journal update digest changed before publication")
	}
	oldIdentity, err := snapshotRegularInstallRemoval(loaded.root, loaded.base)
	if err != nil || !os.SameFile(loaded.info, oldIdentity.info) || oldIdentity.digest != loaded.digest {
		return "", "", errors.Join(errors.New("install journal changed before atomic update"), err)
	}
	if err := revalidateInstallHelperParent(loaded.root, loaded.parent); err != nil {
		return "", "", err
	}
	if err := exchangeInstallHelperRoot(loaded.root, loaded.base, tempName); err != nil {
		return "", "", fmt.Errorf("atomically exchange install journal update: %w", err)
	}
	if inheritedInstallHelperAfterJournalExchange != nil {
		inheritedInstallHelperAfterJournalExchange(loaded.root, loaded.base, tempName)
	}
	restore := func(cause error) error {
		restoreErr := exchangeInstallHelperRoot(loaded.root, loaded.base, tempName)
		return errors.Join(cause, restoreErr, syncInstallHelperJournalRoot(loaded.root))
	}
	published, publishedErr := snapshotRegularInstallRemoval(loaded.root, loaded.base)
	displaced, displacedErr := snapshotRegularInstallRemoval(loaded.root, tempName)
	if publishedErr != nil || displacedErr != nil || !os.SameFile(created, published.info) || published.digest != newDigestText ||
		!os.SameFile(loaded.info, displaced.info) || displaced.digest != loaded.digest {
		return "", "", restore(errors.Join(errors.New("install journal generations changed during atomic update"), publishedErr, displacedErr))
	}
	if err := revalidateInstallHelperParent(loaded.root, loaded.parent); err != nil {
		return "", "", restore(err)
	}
	if err := syncInstallHelperJournalRoot(loaded.root); err != nil {
		// The exchange succeeded but its durability is ambiguous. Preserve the
		// displaced exact journal at the random temporary name for diagnosis.
		cleanup = false
		return "", "", err
	}
	if err := quarantineAndRemoveInstallEntry(loaded.root, tempName, displaced, func() error {
		return revalidateInstallHelperParent(loaded.root, loaded.parent)
	}); err != nil {
		cleanup = false
		return "", "", err
	}
	cleanup = false
	if err := syncInstallHelperJournalRoot(loaded.root); err != nil {
		return "", "", err
	}
	identity, err := installRemovalIdentityString(published.info)
	if err != nil {
		return "", "", err
	}
	return identity, newDigestText, nil
}

func validateInstallJournalBackupPrior(journal installTransactionJournal) error {
	root, parent, _, err := openPinnedInstallHelperParent(filepath.Join(journal.BackupRoot, ".identity-pin"))
	if err != nil {
		return fmt.Errorf("retain install recovery backup: %w", err)
	}
	defer root.Close()
	if filepath.Clean(parent) != journal.BackupRoot {
		return errors.New("install recovery backup path changed")
	}
	if !journal.PreviousInstall {
		directory, err := root.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(1)
		closeErr := directory.Close()
		if len(entries) != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil {
			return errors.Join(errors.New("initial install recovery backup is not empty"), err)
		}
		return revalidateInstallHelperParent(root, parent)
	}
	for key, target := range journal.Targets {
		if key == installJournalTargetLink {
			continue
		}
		name := installTransactionMetadataBackup
		if strings.HasPrefix(key, "bundle:") {
			name = strings.TrimPrefix(key, "bundle:")
		}
		identity, err := snapshotRegularInstallRemoval(root, filepath.FromSlash(name))
		if err != nil || identity.digest != target.Prior.Digest {
			return errors.Join(fmt.Errorf("recovery backup does not match recorded prior generation: %s", key), err)
		}
	}
	return revalidateInstallHelperParent(root, parent)
}

func validateInstallJournalPhase(journal installTransactionJournal, phase string) error {
	for key, target := range journal.Targets {
		expected := target.Prior
		if phase == "normal" && target.Produced.Kind != "" {
			expected = target.Produced
		}
		if phase == installJournalFieldRecovery && target.Recovery.Kind != "" {
			expected = target.Recovery
		}
		path := journal.MetadataPath
		switch {
		case key == installJournalTargetLink:
			path = journal.CommandPath
		case strings.HasPrefix(key, "bundle:"):
			path = filepath.Join(journal.BundleRoot, filepath.FromSlash(strings.TrimPrefix(key, "bundle:")))
		case key != "metadata":
			return fmt.Errorf("unknown install journal target %q", key)
		}
		if err := installHelperGenerationMatchesPath(path, expected); err != nil {
			return fmt.Errorf("install target does not match %s phase generation %s: %w", phase, key, err)
		}
	}
	return nil
}

func installHelperGenerationMatchesPath(path string, expected installJournalGeneration) error {
	root, _, base, err := openPinnedInstallHelperParent(path)
	if err != nil {
		return err
	}
	defer root.Close()
	if expected.Kind == installGenerationKindAbsent {
		if _, err := root.Lstat(base); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		return errors.New("expected path to be absent")
	}
	var identity installRemovalIdentity
	switch expected.Kind {
	case installGenerationKindRegular:
		identity, err = snapshotRegularInstallRemoval(root, base)
	case installGenerationKindSymlink:
		identity, err = snapshotSymlinkInstallRemoval(root, base)
	default:
		return fmt.Errorf("unsupported expected generation %q", expected.Kind)
	}
	if err != nil || !installGenerationMatches(identity, expected) {
		return errors.Join(errors.New("path generation changed"), err)
	}
	return nil
}

func validateLoadedInstallHelperJournal(loaded installHelperJournal) error {
	current, err := snapshotRegularInstallRemoval(loaded.root, loaded.base)
	if err != nil || !os.SameFile(loaded.info, current.info) || current.digest != loaded.digest {
		return errors.Join(errors.New("install journal changed during authorized helper operation"), err)
	}
	return revalidateInstallHelperParent(loaded.root, loaded.parent)
}

func removeMatchedInstallHelperGeneration(loaded installHelperJournal, key, field, path string) (returnErr error) {
	generation, err := installHelperJournalGeneration(loaded.journal, key, field)
	if err != nil || generation.Kind == "" {
		return errors.Join(errors.New("install journal generation is unavailable for removal"), err)
	}
	if err := validateInstallHelperCandidatePath(loaded.journal, key, path); err != nil {
		return err
	}
	if generation.Kind == installGenerationKindAbsent {
		return installHelperGenerationMatchesPath(path, generation)
	}
	root, parent, base, err := openPinnedInstallHelperParent(path)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	var identity installRemovalIdentity
	switch generation.Kind {
	case installGenerationKindRegular:
		identity, err = snapshotRegularInstallRemoval(root, base)
	case installGenerationKindSymlink:
		identity, err = snapshotSymlinkInstallRemoval(root, base)
	default:
		return fmt.Errorf("unsupported install journal generation %q", generation.Kind)
	}
	if err != nil || !installGenerationMatches(identity, generation) {
		return errors.Join(errors.New("install candidate does not match its journal generation"), err)
	}
	return quarantineAndRemoveInstallEntry(root, base, identity, func() error {
		return errors.Join(revalidateInstallHelperParent(root, parent), validateLoadedInstallHelperJournal(loaded))
	})
}

func commitInstallHelperJournal(loaded installHelperJournal, phase string) error {
	if err := validateInstallJournalPhase(loaded.journal, phase); err != nil {
		return err
	}
	// Revalidate the whole set immediately before the journal commit. The
	// lifecycle guard excludes compliant writers throughout both passes.
	if err := validateInstallJournalPhase(loaded.journal, phase); err != nil {
		return err
	}
	current, err := loaded.root.Lstat(loaded.base)
	if err != nil || !os.SameFile(loaded.info, current) {
		return errors.Join(errors.New("install journal changed before commit"), err)
	}
	if err := revalidateInstallHelperParent(loaded.root, loaded.parent); err != nil {
		return err
	}
	var quarantine string
	renamed := false
	for range 16 {
		var suffix [12]byte
		_, _ = rand.Read(suffix[:])
		quarantine = ".transaction-finalize-" + hex.EncodeToString(suffix[:])
		if err := safefs.RenameRootNoReplace(loaded.root, loaded.base, quarantine); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return err
		}
		renamed = true
		break
	}
	if !renamed {
		return errors.New("could not allocate install journal quarantine")
	}
	moved, err := loaded.root.Lstat(quarantine)
	if err != nil || !os.SameFile(loaded.info, moved) {
		restoreErr := safefs.RenameRootNoReplace(loaded.root, quarantine, loaded.base)
		return errors.Join(errors.New("install journal changed during commit"), err, restoreErr)
	}
	if err := syncInstallHelperJournalRoot(loaded.root); err != nil {
		// The canonical unlink is not yet known durable. Preserve the exact
		// journal at its quarantine name; either name can survive a crash.
		return err
	}
	if err := loaded.root.Remove(quarantine); err != nil {
		restoreErr := safefs.RenameRootNoReplace(loaded.root, quarantine, loaded.base)
		return errors.Join(err, restoreErr)
	}
	return syncInstallHelperJournalRoot(loaded.root)
}
