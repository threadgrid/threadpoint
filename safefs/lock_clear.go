// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/threadgrid/threadpoint/internal/abspath"
)

const maxClearableFileLockMetadataBytes int64 = 64 * 1024

// errFileLockChangedBeforeCleanup and errFileLockChangedDuringCleanup report
// that the reviewed lock object stopped matching between review and removal,
// i.e. another writer reclaimed or replaced it. The reclaim-on-acquire path
// treats them as a signal to retry acquisition rather than a hard failure.
var (
	errFileLockChangedBeforeCleanup = errors.New("file lock changed before cleanup")
	errFileLockChangedDuringCleanup = errors.New("file lock changed during cleanup")
)

// FileLockClearOptions configures explicit cooperative lock cleanup.
type FileLockClearOptions struct {
	Path        string
	Force       bool
	StaleAfter  time.Duration
	Now         func() time.Time
	StalePolicy FileLockStalePolicy
}

// FileLockClearResult describes the outcome of explicit lock cleanup.
type FileLockClearResult struct {
	Cleared bool
	Stale   bool
	Forced  bool
}

// ClearFileLock removes a missing or stale cooperative lock. A fresh lock is
// retained unless Force is set. The reviewed lock entry is detached with an
// atomic no-replace rename before deletion, so a replacement is never removed.
func ClearFileLock(opts FileLockClearOptions) (result FileLockClearResult, returnErr error) {
	normalized, err := normalizeFileLockOptions(FileLockOptions{
		Path:        opts.Path,
		StaleAfter:  opts.StaleAfter,
		Now:         opts.Now,
		StalePolicy: opts.StalePolicy,
	})
	if err != nil {
		return result, err
	}
	if err := RejectExistingSymlink(filepath.Dir(normalized.Path)); err != nil {
		return result, FileLockError{Reason: FileLockErrorUnsafe, Path: normalized.Path, Message: err.Error(), Err: err}
	}

	parent, err := os.OpenRoot(filepath.Dir(normalized.Path))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()

	entry, err := reviewFileLockEntry(parent, filepath.Base(normalized.Path))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, entry.Close()) }()

	snapshot := FileLockSnapshot{
		Path:    normalized.Path,
		Body:    append([]byte(nil), entry.body...),
		ModTime: entry.info.ModTime(),
	}
	stale, err := normalized.StalePolicy(snapshot, normalized.Now().UTC(), normalized.StaleAfter)
	if err != nil {
		return result, err
	}
	if !stale && !opts.Force {
		return result, FileLockError{Reason: FileLockErrorHeld, Path: normalized.Path, Body: snapshot.Body}
	}
	if err := removeReviewedFileLock(parent, filepath.Base(normalized.Path), entry); err != nil {
		return result, err
	}
	return FileLockClearResult{Cleared: true, Stale: stale, Forced: !stale && opts.Force}, nil
}

// ClearProjectLock clears the one cooperative mutation lock for root beneath
// threadpointHome. It uses the same canonical root and home resolution as
// AcquireLocks.
func ClearProjectLock(threadpointHome string, root string, force bool) (FileLockClearResult, error) {
	roots, err := normalizedLockRoots([]string{root})
	if err != nil {
		return FileLockClearResult{}, err
	}
	if len(roots) != 1 {
		return FileLockClearResult{}, errors.New("exactly one project root is required")
	}
	if strings.TrimSpace(threadpointHome) == "" {
		threadpointHome, err = ResolveThreadpointHome("")
		if err != nil {
			return FileLockClearResult{}, err
		}
	}
	threadpointHome, err = abspath.Abs(threadpointHome)
	if err != nil {
		return FileLockClearResult{}, err
	}
	return ClearFileLock(FileLockClearOptions{
		Path:  filepath.Join(threadpointHome, "locks", lockName(roots[0])+".lock"),
		Force: force,
	})
}

type reviewedFileLockEntry struct {
	info   fs.FileInfo
	file   *os.File
	body   []byte
	digest [sha256.Size]byte
}

// Close releases the reviewed lock file without closing it more than once.
func (entry *reviewedFileLockEntry) Close() error {
	if entry == nil || entry.file == nil {
		return nil
	}
	err := entry.file.Close()
	entry.file = nil
	return err
}

func reviewFileLockEntry(parent *os.Root, name string) (*reviewedFileLockEntry, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		err := fmt.Errorf("file lock must not be a symlink: %s: %w", name, ErrUnsafeSymlink)
		return nil, FileLockError{Reason: FileLockErrorUnsafe, Path: name, Message: err.Error(), Err: err}
	}
	if !info.Mode().IsRegular() {
		err := fmt.Errorf("file lock must be a regular file: %s", name)
		return nil, FileLockError{Reason: FileLockErrorUnsafe, Path: name, Message: err.Error(), Err: err}
	}
	if info.Size() < 0 || info.Size() > maxClearableFileLockMetadataBytes {
		return nil, FileLockError{Reason: FileLockErrorUnsafe, Path: name, Message: "file lock metadata exceeds the bounded cleanup limit"}
	}
	file, err := openReviewedLockFile(parent, name)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("file lock changed while opened"), err, file.Close())
	}
	body, err := io.ReadAll(io.LimitReader(file, maxClearableFileLockMetadataBytes+1))
	after, afterErr := file.Stat()
	current, currentErr := parent.Lstat(name)
	if err != nil || afterErr != nil || currentErr != nil || int64(len(body)) != opened.Size() || !after.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(opened, after) || !os.SameFile(opened, current) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || after.Mode() != current.Mode() || after.Size() != current.Size() || !after.ModTime().Equal(current.ModTime()) {
		return nil, errors.Join(errors.New("file lock changed while read"), err, afterErr, currentErr, file.Close())
	}
	return &reviewedFileLockEntry{info: after, file: file, body: body, digest: sha256.Sum256(body)}, nil
}

func (entry *reviewedFileLockEntry) matches(parent *os.Root, name string) bool {
	if entry == nil || entry.info == nil || entry.file == nil || parent == nil {
		return false
	}
	current, err := parent.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(entry.info, current) || current.Mode() != entry.info.Mode() || current.Size() != entry.info.Size() || !current.ModTime().Equal(entry.info.ModTime()) {
		return false
	}
	opened, err := entry.file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(entry.info, opened) || opened.Mode() != entry.info.Mode() || opened.Size() != entry.info.Size() || !opened.ModTime().Equal(entry.info.ModTime()) {
		return false
	}
	if _, err := entry.file.Seek(0, io.SeekStart); err != nil {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(entry.file, maxClearableFileLockMetadataBytes+1))
	after, afterErr := entry.file.Stat()
	canonical, canonicalErr := parent.Lstat(name)
	return err == nil && int64(len(body)) == opened.Size() && sha256.Sum256(body) == entry.digest && afterErr == nil && canonicalErr == nil &&
		after.Mode().IsRegular() && canonical.Mode().IsRegular() && os.SameFile(opened, after) && os.SameFile(opened, canonical) &&
		opened.Mode() == after.Mode() && opened.Size() == after.Size() && opened.ModTime().Equal(after.ModTime())
}

func removeReviewedFileLock(parent *os.Root, name string, entry *reviewedFileLockEntry) error {
	if !RenameRootNoReplaceSupported() {
		return ErrRenameNoReplaceUnsupported
	}
	if !entry.matches(parent, name) {
		return errFileLockChangedBeforeCleanup
	}
	quarantine, err := fileLockQuarantineName(parent, name)
	if err != nil {
		return err
	}
	if err := RenameRootNoReplace(parent, name, quarantine); err != nil {
		return err
	}
	if !entry.matches(parent, quarantine) {
		restoreErr := RenameRootNoReplace(parent, quarantine, name)
		return errors.Join(errFileLockChangedDuringCleanup, restoreErr)
	}
	if err := parent.Remove(quarantine); err != nil {
		return err
	}
	return syncFileLockParent(parent)
}

// Keep the directory durability boundary injectable for transaction failure tests.
var syncFileLockParent = syncFileLockParentDirectory

func syncFileLockParentDirectory(parent *os.Root) error {
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func fileLockQuarantineName(parent *os.Root, base string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var token [12]byte
		_, _ = rand.Read(token[:])
		name := "." + base + ".clear-" + fmt.Sprintf("%x", token[:])
		if _, err := parent.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate file lock cleanup quarantine name")
}
