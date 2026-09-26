// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/internal/abspath"
	"github.com/threadgrid/threadpoint/safefs"
)

const (
	maxUpdateStateBytes       = 1 << 20
	updateStateLockWait       = 5 * time.Second
	updateStateLockRetryDelay = 10 * time.Millisecond
	updateStateCASAttempts    = 8
)

var (
	errUpdateStateGenerationChanged = errors.New("threadpoint update state generation changed")
	updateStateBeforeCompare        func()
	updateStateAfterRootBorrow      func()
)

type updateStateGeneration struct {
	body   []byte
	info   os.FileInfo
	exists bool
}

type updateReminderMutation struct {
	Enabled              *bool
	DismissedVersion     *string
	LastAutomaticCheckAt *time.Time
}

func boolPointer(value bool) *bool           { return &value }
func stringPointer(value string) *string     { return &value }
func timePointer(value time.Time) *time.Time { return &value }

func withUpdateStateRoot(productHome, command string, action func(*os.Root) error) (returnErr error) {
	resolvedHome, err := resolveThreadpointHomeForPath(productHome)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(updateStateLockWait)
	var locks *safefs.LockSet
	for {
		locks, err = safefs.AcquireLocks(resolvedHome, []string{resolvedHome}, command)
		if err == nil {
			break
		}
		if !updateStateLockContention(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(updateStateLockRetryDelay)
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Release()) }()
	root, err := locks.BorrowProductRoot()
	if err != nil {
		return err
	}
	if updateStateAfterRootBorrow != nil {
		updateStateAfterRootBorrow()
	}
	if err := action(root); err != nil {
		return err
	}
	_, err = locks.BorrowProductRoot()
	return err
}

func updateStateLockContention(err error) bool {
	if errors.Is(err, safefs.ErrLockHeld) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "file lock changed while opened") || strings.Contains(message, "file lock changed while read")
}

func ensurePrivateUpdateStateDirectory(root *os.Root, rel string) error {
	if root == nil {
		return errors.New("update state root is required")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("update state directory escapes retained root: %s", rel)
	}
	current := ""
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if current == "" {
			current = component
		} else {
			current = filepath.Join(current, component)
		}
		if err := safefs.RejectRootSymlinkAncestors(root, current); err != nil {
			return err
		}
		if err := root.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		before, err := root.Lstat(current)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return errors.Join(fmt.Errorf("update state path is not a physical directory: %s", current), err)
		}
		child, err := root.OpenRoot(current)
		if err != nil {
			return err
		}
		opened, openErr := child.Stat(".")
		file, fileErr := child.Open(".")
		var chmodErr, syncErr, fileCloseErr error
		if fileErr == nil {
			chmodErr = file.Chmod(0o700)
			syncErr = file.Sync()
			fileCloseErr = file.Close()
		}
		after, afterErr := root.Lstat(current)
		closeErr := child.Close()
		if openErr != nil || fileErr != nil || chmodErr != nil || syncErr != nil || fileCloseErr != nil || afterErr != nil || !opened.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
			return errors.Join(fmt.Errorf("update state directory changed while it was pinned: %s", current), openErr, fileErr, chmodErr, syncErr, fileCloseErr, afterErr, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func readUpdateStateGeneration(root *os.Root, name string) (updateStateGeneration, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return updateStateGeneration{}, nil
	}
	if err != nil {
		return updateStateGeneration{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return updateStateGeneration{}, fmt.Errorf("update state path is not a regular file: %s", name)
	}
	body, opened, err := backup.ReadRootRegularFileBoundedInfo(root, name, maxUpdateStateBytes)
	if err != nil {
		return updateStateGeneration{}, err
	}
	if !os.SameFile(info, opened) {
		return updateStateGeneration{}, errUpdateStateGenerationChanged
	}
	return updateStateGeneration{body: body, info: opened, exists: true}, nil
}

func sameUpdateStateGeneration(root *os.Root, name string, expected updateStateGeneration) error {
	if updateStateBeforeCompare != nil {
		updateStateBeforeCompare()
	}
	current, err := readUpdateStateGeneration(root, name)
	if err != nil {
		return err
	}
	if current.exists != expected.exists || !bytes.Equal(current.body, expected.body) || (expected.exists && !os.SameFile(current.info, expected.info)) {
		return errUpdateStateGenerationChanged
	}
	return nil
}

func writeUpdateStateCAS(root *os.Root, name string, build func(updateStateGeneration) ([]byte, error)) error {
	for attempt := 0; attempt < updateStateCASAttempts; attempt++ {
		generation, err := readUpdateStateGeneration(root, name)
		if err != nil {
			return err
		}
		body, err := build(generation)
		if err != nil {
			return err
		}
		validate := func(parent *os.Root, base string) error {
			return sameUpdateStateGeneration(parent, base, generation)
		}
		if err := safefs.AtomicWriteRootFile(root, name, append(body, '\n'), 0o600, validate); err == nil {
			return nil
		} else if !errors.Is(err, errUpdateStateGenerationChanged) {
			return err
		}
	}
	return errors.New("threadpoint update state kept changing during compare-and-swap")
}

func updateStatePathLocation(path string) (string, string, error) {
	clean, err := abspath.Abs(strings.TrimSpace(path))
	if err != nil || strings.TrimSpace(path) == "" {
		return "", "", errors.Join(errors.New("update state path is required"), err)
	}
	cacheDir := filepath.Dir(clean)
	updatesDir := filepath.Dir(cacheDir)
	productHome := filepath.Dir(updatesDir)
	if filepath.Base(cacheDir) != "cache" || filepath.Base(updatesDir) != "updates" || filepath.Base(clean) == "." {
		return "", "", fmt.Errorf("update cache path is outside the canonical update state layout: %s", clean)
	}
	return productHome, filepath.Join("updates", "cache", filepath.Base(clean)), nil
}

func readUpdateCacheLocked(path string) (record updateCacheMetadata, found bool, returnErr error) {
	productHome, rel, err := updateStatePathLocation(path)
	if err != nil {
		return updateCacheMetadata{}, false, err
	}
	err = withUpdateStateRoot(productHome, "update state read", func(root *os.Root) error {
		generation, err := readUpdateStateGeneration(root, rel)
		if err != nil || !generation.exists {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(generation.body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return errors.New("update cache contains multiple JSON values")
			}
			return err
		}
		if err := validateUpdateCacheRecord(path, productHome, record); err != nil {
			return err
		}
		found = true
		return nil
	})
	return record, found, err
}

func validateUpdateCacheRecord(path, productHome string, record updateCacheMetadata) error {
	if record.SchemaVersion != expectedUpdateCacheSchema {
		return fmt.Errorf("update cache schema_version %q does not match %q", record.SchemaVersion, expectedUpdateCacheSchema)
	}
	if !validUpdateRepository(record.Repo) {
		return errors.New("update cache requires a canonical repository namespace")
	}
	channel, err := normalizeUpdateChannel(record.Channel)
	if err != nil || channel != record.Channel {
		return errors.Join(errors.New("update cache requires an exact channel"), err)
	}
	expectedPath, ok := updateCachePathWithHome(productHome, record.Repo, record.Channel)
	if !ok {
		return errors.New("update cache repository and channel do not resolve to a namespace")
	}
	actualPath, err := abspath.Abs(path)
	if err != nil || filepath.Clean(actualPath) != filepath.Clean(expectedPath) {
		return errors.Join(errors.New("update cache does not match its repository and channel namespace"), err)
	}
	version, err := parseCanonicalReleaseTag(record.Tag)
	if err != nil {
		return errors.Join(errors.New("update cache requires a canonical release tag"), err)
	}
	wantPrerelease := len(version.prerelease) > 0
	if record.Prerelease != wantPrerelease || (record.Channel == updateChannelStable && wantPrerelease) || (record.Channel == updateChannelPreview && !wantPrerelease) {
		return errors.New("update cache tag, prerelease flag, and channel do not agree")
	}
	wantReleaseURL := updateReleaseTagURL(record.Repo, record.Tag)
	if record.HTMLURL != wantReleaseURL {
		return errors.New("update cache release URL does not match its repository and tag")
	}
	if err := validateUpdateLearnMoreURL(record.Repo, record.LearnMoreURL); err != nil {
		return err
	}
	if record.Summary != sanitizeUpdateText(record.Summary) {
		return errors.New("update cache summary is not canonical sanitized text")
	}
	if record.NewerStableReleaseCount < 0 {
		return errors.New("update cache newer stable release count cannot be negative")
	}
	if record.CheckedAt.IsZero() || record.CheckedAt.Location() != time.UTC {
		return errors.New("update cache requires a nonzero UTC checked_at")
	}
	if !record.Published.IsZero() {
		if record.Published.Location() != time.UTC || record.Published.After(record.CheckedAt) {
			return errors.New("update cache published_at must be UTC and no later than checked_at")
		}
	}
	return nil
}

func parseCanonicalReleaseTag(raw string) (versionParts, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || !strings.HasPrefix(raw, "v") || strings.HasPrefix(raw, "vv") {
		return versionParts{}, errors.New("release tag must begin with one lowercase v and contain no surrounding whitespace")
	}
	value := strings.TrimPrefix(raw, "v")
	if strings.Count(value, "+") > 1 {
		return versionParts{}, errors.New("release tag contains multiple build metadata separators")
	}
	precedence, build, hasBuild := strings.Cut(value, "+")
	if hasBuild {
		if err := validateSemVerIdentifiers(build, false); err != nil {
			return versionParts{}, fmt.Errorf("invalid release tag build metadata: %w", err)
		}
	}
	core, prerelease, hasPrerelease := strings.Cut(precedence, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return versionParts{}, errors.New("release tag core must contain exactly major, minor, and patch")
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return versionParts{}, errors.New("release tag core has an empty or zero-prefixed number")
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return versionParts{}, errors.New("release tag core contains a non-decimal number")
			}
		}
	}
	if hasPrerelease {
		if err := validateSemVerIdentifiers(prerelease, true); err != nil {
			return versionParts{}, fmt.Errorf("invalid release tag prerelease: %w", err)
		}
	}
	return parseSemVer(raw)
}

func validateSemVerIdentifiers(raw string, rejectNumericLeadingZero bool) error {
	if raw == "" {
		return errors.New("identifier list is empty")
	}
	for _, identifier := range strings.Split(raw, ".") {
		if identifier == "" {
			return errors.New("identifier is empty")
		}
		numeric := true
		for _, char := range identifier {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
				return errors.New("identifier contains a character outside [0-9A-Za-z-]")
			}
			if char < '0' || char > '9' {
				numeric = false
			}
		}
		if rejectNumericLeadingZero && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return errors.New("numeric prerelease identifier has a leading zero")
		}
	}
	return nil
}

func validUpdateRepository(repo string) bool {
	if strings.TrimSpace(repo) != repo {
		return false
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") {
			return false
		}
		for _, char := range part {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' && char != '.' {
				return false
			}
		}
	}
	return true
}

func writeUpdateCacheLocked(path string, record updateCacheMetadata) error {
	productHome, rel, err := updateStatePathLocation(path)
	if err != nil {
		return err
	}
	if err := validateUpdateCacheRecord(path, productHome, record); err != nil {
		return err
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return withUpdateStateRoot(productHome, "update state cache", func(root *os.Root) error {
		if err := ensurePrivateUpdateStateDirectory(root, filepath.Join("updates", "cache")); err != nil {
			return err
		}
		return writeUpdateStateCAS(root, rel, func(updateStateGeneration) ([]byte, error) {
			return body, nil
		})
	})
}

func decodeUpdateReminderGeneration(generation updateStateGeneration) (updateReminderState, error) {
	if !generation.exists {
		return defaultUpdateReminderState(), nil
	}
	var state updateReminderState
	if err := json.Unmarshal(generation.body, &state); err != nil {
		return updateReminderState{}, err
	}
	return state, nil
}

func mutateUpdateReminderStateWithHome(productHome string, transform func(updateReminderState) (updateReminderState, bool, error)) (result updateReminderState, returnErr error) {
	returnErr = withUpdateStateRoot(productHome, "update state reminders", func(root *os.Root) error {
		if err := ensurePrivateUpdateStateDirectory(root, "updates"); err != nil {
			return err
		}
		const rel = "updates/reminders.json"
		for attempt := 0; attempt < updateStateCASAttempts; attempt++ {
			generation, err := readUpdateStateGeneration(root, rel)
			if err != nil {
				return err
			}
			current, err := decodeUpdateReminderGeneration(generation)
			if err != nil {
				return err
			}
			next, changed, err := transform(current)
			if err != nil {
				return err
			}
			result = next
			if !changed {
				return nil
			}
			body, err := json.MarshalIndent(next, "", "  ")
			if err != nil {
				return err
			}
			validate := func(parent *os.Root, base string) error {
				return sameUpdateStateGeneration(parent, base, generation)
			}
			if err := safefs.AtomicWriteRootFile(root, rel, append(body, '\n'), 0o600, validate); err == nil {
				return nil
			} else if !errors.Is(err, errUpdateStateGenerationChanged) {
				return err
			}
		}
		return errors.New("threadpoint reminder state kept changing during compare-and-swap")
	})
	return result, returnErr
}

func mergeUpdateReminderStateWithHome(productHome string, mutation updateReminderMutation) (updateReminderState, error) {
	return mutateUpdateReminderStateWithHome(productHome, func(state updateReminderState) (updateReminderState, bool, error) {
		if mutation.Enabled != nil {
			state.Enabled = *mutation.Enabled
		}
		if mutation.DismissedVersion != nil {
			state.DismissedVersion = *mutation.DismissedVersion
		}
		if mutation.LastAutomaticCheckAt != nil {
			state.LastAutomaticCheckAt = mutation.LastAutomaticCheckAt.UTC()
		}
		return state, true, nil
	})
}

func readUpdateReminderStateLocked(productHome string) (state updateReminderState, returnErr error) {
	returnErr = withUpdateStateRoot(productHome, "update state read", func(root *os.Root) error {
		generation, err := readUpdateStateGeneration(root, "updates/reminders.json")
		if err != nil {
			return err
		}
		state, err = decodeUpdateReminderGeneration(generation)
		return err
	})
	return state, returnErr
}

func writeUpdateReminderStateLocked(productHome string, replacement updateReminderState) error {
	_, err := mutateUpdateReminderStateWithHome(productHome, func(updateReminderState) (updateReminderState, bool, error) {
		return replacement, true, nil
	})
	return err
}

func claimAutomaticUpdateReminderCheck(productHome string, now time.Time) (updateReminderState, bool, error) {
	claimed := false
	state, err := mutateUpdateReminderStateWithHome(productHome, func(state updateReminderState) (updateReminderState, bool, error) {
		claimed = false
		if !state.Enabled || (!state.LastAutomaticCheckAt.IsZero() && now.Sub(state.LastAutomaticCheckAt) < defaultAutomaticReminderInterval) {
			return state, false, nil
		}
		state.LastAutomaticCheckAt = now.UTC()
		claimed = true
		return state, true, nil
	})
	return state, claimed, err
}
