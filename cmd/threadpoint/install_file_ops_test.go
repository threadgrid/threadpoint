// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstallHelperCopiesOnlyExpectedBoundedRegularGeneration(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source", "managed")
	destination := filepath.Join(root, "backup", "managed")
	writeInstallHelperFile(t, source, []byte("reviewed managed generation\n"))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	generation := installHelperGeneration(t, source)
	var copiedOutput bytes.Buffer
	if err := runInstallCopyBoundedAs(&copiedOutput, []string{source, destination, generation.Identity, generation.Digest, "0644"}); err != nil {
		t.Fatal(err)
	}
	copiedIdentity, copiedDigest := parseInstallHelperUpdateOutput(t, copiedOutput.String())
	if copiedIdentity != installHelperPathIdentity(t, destination) || copiedDigest != generation.Digest {
		t.Fatalf("bounded copy generation = %q", copiedOutput.String())
	}
	body, err := os.ReadFile(destination)
	if err != nil || string(body) != "reviewed managed generation\n" {
		t.Fatalf("bounded copy body = %q, err=%v", body, err)
	}
	if mode := mustInstallHelperIdentity(t, destination).info.Mode().Perm(); mode != 0o644 {
		t.Fatalf("bounded copy mode = %04o", mode)
	}
	if err := runInstallCopyBoundedAs(io.Discard, []string{source, destination, generation.Identity, generation.Digest, "0600"}); err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("existing bounded-copy destination error = %v", err)
	}
	if err := runInstallCopyBoundedAs(io.Discard, []string{source, filepath.Join(root, "backup", "bad-mode"), generation.Identity, generation.Digest, "0777"}); err == nil || !strings.Contains(err.Error(), "copy mode") {
		t.Fatalf("invalid bounded-copy mode error = %v", err)
	}

	oversized := filepath.Join(root, "source", "oversized")
	file, err := os.OpenFile(oversized, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxInstallManagedFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(oversized)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := installRemovalIdentityString(info)
	if err != nil {
		t.Fatal(err)
	}
	oversizedDestination := filepath.Join(root, "backup", "oversized")
	if err := runInstallCopyBoundedAs(io.Discard, []string{oversized, oversizedDestination, identity, strings.Repeat("0", sha256.Size*2), "0600"}); err == nil || !strings.Contains(err.Error(), "expected regular generation") {
		t.Fatalf("oversized bounded-copy error = %v", err)
	}
	if _, err := os.Lstat(oversizedDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized bounded copy left destination: %v", err)
	}

	symlinkSource := filepath.Join(root, "source", "symlink")
	if err := os.Symlink(source, symlinkSource); err != nil {
		t.Fatal(err)
	}
	symlinkInfo, err := os.Lstat(symlinkSource)
	if err != nil {
		t.Fatal(err)
	}
	symlinkIdentity, err := installRemovalIdentityString(symlinkInfo)
	if err != nil {
		t.Fatal(err)
	}
	symlinkDestination := filepath.Join(root, "backup", "symlink")
	if err := runInstallCopyBoundedAs(io.Discard, []string{symlinkSource, symlinkDestination, symlinkIdentity, generation.Digest, "0600"}); err == nil || !strings.Contains(err.Error(), "expected regular generation") {
		t.Fatalf("symlink bounded-copy error = %v", err)
	}

	fifoSource := filepath.Join(root, "source", "fifo")
	if err := syscall.Mkfifo(fifoSource, 0o600); err != nil {
		t.Fatal(err)
	}
	fifoInfo, err := os.Lstat(fifoSource)
	if err != nil {
		t.Fatal(err)
	}
	fifoIdentity, err := installRemovalIdentityString(fifoInfo)
	if err != nil {
		t.Fatal(err)
	}
	fifoDestination := filepath.Join(root, "backup", "fifo")
	if err := runInstallCopyBoundedAs(io.Discard, []string{fifoSource, fifoDestination, fifoIdentity, generation.Digest, "0600"}); err == nil || !strings.Contains(err.Error(), "expected regular generation") {
		t.Fatalf("FIFO bounded-copy error = %v", err)
	}

	replacementSource := filepath.Join(root, "source", "replacement")
	replacementDestination := filepath.Join(root, "backup", "replacement")
	writeInstallHelperFile(t, replacementSource, []byte("reviewed recovery generation\n"))
	replacementGeneration := installHelperGeneration(t, replacementSource)
	displacedSource := replacementSource + ".displaced"
	inheritedInstallHelperAfterBoundedCopySnapshot = func(path string) {
		if path != replacementSource {
			return
		}
		if err := os.Rename(replacementSource, displacedSource); err != nil {
			t.Fatal(err)
		}
		writeInstallHelperFile(t, replacementSource, []byte("last-moment replacement\n"))
	}
	t.Cleanup(func() { inheritedInstallHelperAfterBoundedCopySnapshot = nil })
	if err := runInstallCopyBoundedAs(io.Discard, []string{replacementSource, replacementDestination, replacementGeneration.Identity, replacementGeneration.Digest, "0600"}); err == nil || !strings.Contains(err.Error(), "changed while it was opened") {
		t.Fatalf("replacement bounded-copy error = %v", err)
	}
	inheritedInstallHelperAfterBoundedCopySnapshot = nil
	if _, err := os.Lstat(replacementDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement bounded copy left destination: %v", err)
	}
}

func TestInstallHelperBoundedWriteRefusesSpecialAndReplacedDestinations(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "state.json")
	body := []byte("reviewed state\n")
	var output bytes.Buffer
	if err := runInstallWriteBounded(&output, bytes.NewReader(body), []string{destination, "0600"}); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(output.String())
	if len(fields) != 2 || fields[0] != installHelperPathIdentity(t, destination) {
		t.Fatalf("bounded-write output = %q", output.String())
	}
	if got, err := os.ReadFile(destination); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("bounded-write body = %q, err=%v", got, err)
	}
	if mode := mustInstallHelperIdentity(t, destination).info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("bounded-write mode = %04o", mode)
	}
	if err := runInstallWriteBounded(io.Discard, bytes.NewReader(body), []string{destination, "0600"}); err == nil {
		t.Fatal("bounded write replaced an existing destination")
	}

	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if err := runInstallWriteBounded(io.Discard, bytes.NewReader(body), []string{fifo, "0600"}); err == nil {
		t.Fatal("bounded write opened an existing FIFO")
	}
	symlink := filepath.Join(root, "symlink")
	outside := filepath.Join(root, "outside")
	writeInstallHelperFile(t, outside, []byte("outside\n"))
	if err := os.Symlink(outside, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := runInstallWriteBounded(io.Discard, bytes.NewReader(body), []string{symlink, "0600"}); err == nil {
		t.Fatal("bounded write opened an existing symlink")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside\n" {
		t.Fatalf("bounded write changed symlink target: %q, err=%v", got, err)
	}
	if err := runInstallWriteBounded(io.Discard, bytes.NewReader(make([]byte, maxInstallTransactionBytes+1)), []string{filepath.Join(root, "oversized"), "0600"}); err == nil {
		t.Fatal("bounded write accepted oversized input")
	}

	replaced := filepath.Join(root, "replaced")
	reviewed := replaced + ".reviewed"
	replacementBody := []byte("replacement\n")
	inheritedInstallHelperAfterBoundedWrite = func(path string) {
		inheritedInstallHelperAfterBoundedWrite = nil
		if err := os.Rename(path, reviewed); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(path, replacementBody, 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { inheritedInstallHelperAfterBoundedWrite = nil })
	if err := runInstallWriteBounded(io.Discard, bytes.NewReader(body), []string{replaced, "0600"}); err == nil {
		t.Fatal("bounded write accepted a final-component replacement")
	}
	if got, err := os.ReadFile(replaced); err != nil || !bytes.Equal(got, replacementBody) {
		t.Fatalf("replacement changed: %q, err=%v", got, err)
	}
	if got, err := os.ReadFile(reviewed); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("reviewed generation changed: %q, err=%v", got, err)
	}
}

func TestInstallNoReplaceHelperUsesRootedNativeRename(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	writeInstallHelperFile(t, source, []byte("source"))
	if err := runInstallNoReplace([]string{source, target}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "source" {
		t.Fatalf("renamed body = %q, %v", body, err)
	}
	writeInstallHelperFile(t, source, []byte("second"))
	if err := runInstallNoReplace([]string{source, target}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing destination error = %v", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "source" {
		t.Fatalf("existing destination replaced: %q, %v", body, err)
	}
}

func TestInstallNoReplaceAsRestoresOpaqueCraftedSymlinkByIdentity(t *testing.T) {
	source := filepath.Join(t.TempDir(), "candidate")
	target := filepath.Join(t.TempDir(), "restored")
	craftedTarget := "foreign\\target\"with-quote\nand-newline"
	if err := os.Symlink(craftedTarget, source); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runInstallEntryIdentity(&output, []string{source}); err != nil {
		t.Fatal(err)
	}
	identity := strings.TrimSpace(output.String())
	if identity == "" || strings.ContainsAny(identity, " \t\r\n") {
		t.Fatalf("unsafe opaque identity %q", identity)
	}
	if err := runInstallNoReplaceExpected([]string{source, target, "0:0"}, true); err == nil {
		t.Fatal("wrong source identity accepted")
	}
	displaced := source + ".displaced"
	inheritedInstallHelperBeforeNoReplaceRename = func(string) {
		if err := os.Rename(source, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("replacement", source); err != nil {
			t.Fatal(err)
		}
	}
	err := runInstallNoReplaceExpected([]string{source, target, identity}, true)
	inheritedInstallHelperBeforeNoReplaceRename = nil
	if err == nil {
		t.Fatal("last-moment no-replace source swap accepted")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source swap published a destination: %v", err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(displaced, source); err != nil {
		t.Fatal(err)
	}
	if err := runInstallNoReplaceExpected([]string{source, target, identity}, true); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(target); err != nil || got != craftedTarget {
		t.Fatalf("opaque symlink target = %q, %v", got, err)
	}
}

func TestInstallRegularSnapshotReportsExactBoundedGeneration(t *testing.T) {
	body := []byte("reviewed generation\n")
	path := filepath.Join(t.TempDir(), "regular")
	writeInstallHelperFile(t, path, body)
	var output bytes.Buffer
	if err := runInstallRegularSnapshot(&output, []string{path}); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(output.String())
	if len(fields) != 3 || fields[0] != installHelperPathIdentity(t, path) ||
		fields[1] != installHelperDigest(body) || fields[2] != strconv.Itoa(len(body)) {
		t.Fatalf("regular snapshot output = %q", output.String())
	}
}

func TestInstallHelperPathsRequireCanonicalAbsoluteNames(t *testing.T) {
	for _, path := range []string{"relative", "/tmp/../tmp", "/tmp/has\\slash", "/tmp/has\"quote", "/tmp/has\nnewline", string([]byte{'/', 0xff})} {
		if canonicalInstallHelperPath(path) {
			t.Fatalf("non-canonical helper path accepted: %q", path)
		}
	}
}

func TestInstallHelperParentPinningRejectsMissingSymlinkedAndReplacedParents(t *testing.T) {
	if root, _, _, err := openPinnedInstallHelperParent(string(filepath.Separator)); err == nil {
		_ = root.Close()
		t.Fatal("filesystem root accepted as a helper child")
	}
	missingParentPath := filepath.Join(t.TempDir(), "missing", "entry")
	if root, _, _, err := openPinnedInstallHelperParent(missingParentPath); err == nil {
		_ = root.Close()
		t.Fatal("missing helper parent accepted")
	}
	realParent := t.TempDir()
	symlinkParent := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(realParent, symlinkParent); err != nil {
		t.Fatal(err)
	}
	if root, _, _, err := openPinnedInstallHelperParent(filepath.Join(symlinkParent, "entry")); err == nil {
		_ = root.Close()
		t.Fatal("symlink helper parent accepted")
	}
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(parent, "entry")
	writeInstallHelperFile(t, entry, []byte("reviewed\n"))
	displaced := parent + "-displaced"
	inheritedInstallHelperAfterParentOpen = func(string) {
		if err := os.Rename(parent, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, err := openPinnedInstallHelperParent(entry)
	inheritedInstallHelperAfterParentOpen = nil
	if err == nil {
		t.Fatal("helper parent replacement accepted after open")
	}
}

func TestInstallGenerationCompletionUsesAuthoritativePhase(t *testing.T) {
	prior := installJournalGeneration{Kind: installGenerationKindAbsent}
	produced := installJournalGeneration{Kind: installGenerationKindRegular, Identity: "1:1", Digest: strings.Repeat("a", 64)}
	recovery := installJournalGeneration{Kind: installGenerationKindRegular, Identity: "2:2", Digest: strings.Repeat("b", 64)}
	target := installJournalTarget{Prior: prior, Produced: produced, Recovery: recovery}
	if got, phase, err := expectedInstallCompletionGeneration(target, true); err != nil || got != recovery || phase != "recovery generation" {
		t.Fatalf("recovery completion = %#v %q %v", got, phase, err)
	}
	if got, phase, err := expectedInstallCompletionGeneration(target, false); err != nil || got != produced || phase != "produced generation" {
		t.Fatalf("normal completion = %#v %q %v", got, phase, err)
	}
	if got, phase, err := expectedInstallCompletionGeneration(installJournalTarget{Prior: prior}, false); err != nil || got != prior || phase != "prior generation" {
		t.Fatalf("prior completion = %#v %q %v", got, phase, err)
	}
	if _, _, err := expectedInstallCompletionGeneration(installJournalTarget{}, false); err == nil {
		t.Fatal("completion without an authoritative generation accepted")
	}
	if validInstallJournalGeneration(installJournalGeneration{Kind: "device"}, true) {
		t.Fatal("unsupported journal generation accepted")
	}
}

func TestPinnedInstallHelpersRejectUntrustedFilesystemShapes(t *testing.T) {
	base := t.TempDir()
	directory := filepath.Join(base, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(base, "regular")
	if err := os.WriteFile(regular, []byte("regular"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(base, "symlink")
	if err := os.Symlink(directory, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, path := range []string{regular, symlink, filepath.Join(base, "missing")} {
		if root, _, err := pinInstallDirectory(path); err == nil {
			_ = root.Close()
			t.Fatalf("pinInstallDirectory accepted %s", path)
		}
	}
	root, info, err := pinInstallDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := validatePinnedInstallDirectory(root, directory, info); err != nil {
		t.Fatal(err)
	}
	if err := validatePinnedInstallDirectory(nil, directory, info); err == nil {
		t.Fatal("nil pinned install root was accepted")
	}

	if child, _, err := ensurePinnedInstallChildDirectory(root, "../outside"); err == nil {
		_ = child.Close()
		t.Fatal("unsafe pinned child name was accepted")
	}
	if err := root.WriteFile("not-a-directory", []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if child, _, err := ensurePinnedInstallChildDirectory(root, "not-a-directory"); err == nil {
		_ = child.Close()
		t.Fatal("regular file was accepted as pinned child directory")
	}
	if err := root.Symlink(".", "linked-directory"); err != nil {
		t.Skipf("root symlink unavailable: %v", err)
	}
	if child, _, err := ensurePinnedInstallChildDirectory(root, "linked-directory"); err == nil {
		_ = child.Close()
		t.Fatal("symlink was accepted as pinned child directory")
	}
	child, childInfo, err := ensurePinnedInstallChildDirectory(root, "child")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if err := validatePinnedInstallChildDirectory(root, "child", child, childInfo); err != nil {
		t.Fatal(err)
	}
	if err := validatePinnedInstallChildDirectory(root, "child", nil, childInfo); err == nil {
		t.Fatal("nil retained child root was accepted")
	}

	if err := writeInstallBackupRootFile(nil, "metadata.json", bytes.NewReader(nil)); err == nil {
		t.Fatal("nil backup root was accepted")
	}
	if err := writeInstallBackupRootFile(root, "nested/metadata.json", bytes.NewReader(nil)); err == nil {
		t.Fatal("nested backup metadata name was accepted")
	}
	if _, err := installRootFileSHA256(nil, "regular"); err == nil {
		t.Fatal("nil checksum root was accepted")
	}
	if _, err := installRootFileSHA256(root, "linked-directory"); err == nil {
		t.Fatal("symlink checksum input was accepted")
	}
	if _, err := installRootFileSHA256(root, "child"); err == nil {
		t.Fatal("directory checksum input was accepted")
	}
	if _, err := readInstallBackupFileBounded(root, "nested/metadata.json", 100); err == nil {
		t.Fatal("nested backup metadata read was accepted")
	}
	if _, err := openPinnedBundleParent(root, "linked-directory"); err == nil {
		t.Fatal("symlink bundle ancestor was accepted")
	}
	if _, err := openPinnedBundleParent(root, "missing"); err == nil {
		t.Fatal("missing bundle ancestor was accepted")
	}
	if err := validateInstallMountBoundary(nil, nil); err == nil {
		t.Fatal("nil mount-boundary roots were accepted")
	}
}

func TestInstallStateReadsAreBoundedAndNoFollow(t *testing.T) {
	fixture := newInstallTransactionFixture(t, false)
	lock := fixture.acquire(t)
	installsRoot := filepath.Join(fixture.productHome, "installs")

	symlinkPath := filepath.Join(installsRoot, "state-symlink")
	if err := os.Symlink(fixture.metadataPath, symlinkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := readInstallLockFileBounded(lock, symlinkPath, maxInstallTransactionBytes); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink state read error = %v", err)
	}
	directoryPath := filepath.Join(installsRoot, "state-directory")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstallLockFileBounded(lock, directoryPath, maxInstallTransactionBytes); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory state read error = %v", err)
	}
	largePath := filepath.Join(installsRoot, "state-large")
	if err := os.WriteFile(largePath, bytes.Repeat([]byte("x"), 65), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstallLockFileBounded(lock, largePath, 64); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized state read error = %v", err)
	}
	if _, err := readInstallLockFileBounded(lock, filepath.Join(fixture.productHome, "outside"), 64); err == nil || !strings.Contains(err.Error(), "outside the pinned lock namespace") {
		t.Fatalf("outside state read error = %v", err)
	}
}

func TestInstallBundleMutationHelpersRejectUnsafeEntries(t *testing.T) {
	fixture := newInstallTransactionFixture(t, false)
	lock := fixture.acquire(t)
	if err := lock.PinBundleRoot(fixture.productHome); err != nil {
		t.Fatal(err)
	}
	if err := lock.PinBundleRoot(t.TempDir()); err == nil || !strings.Contains(err.Error(), "different bundle root") {
		t.Fatalf("second bundle root error = %v", err)
	}

	if removed, err := removeBundleEntryChecked("../outside", lock); err == nil || removed {
		t.Fatalf("unsafe bundle removal = %v, err=%v", removed, err)
	}
	if removed, err := removeBundleEntryChecked("missing", lock); err != nil || removed {
		t.Fatalf("missing bundle removal = %v, err=%v", removed, err)
	}
	badDir := filepath.Join(fixture.productHome, "bad-directory")
	if err := os.Mkdir(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if removed, err := removeBundleEntryChecked("bad-directory", lock); err == nil || removed {
		t.Fatalf("directory bundle removal = %v, err=%v", removed, err)
	}
	if removed, err := removeManagedPathChecked(filepath.Join(t.TempDir(), "outside"), lock); err == nil || removed {
		t.Fatalf("outside managed removal = %v, err=%v", removed, err)
	}
	if removed, err := removeManagedPathChecked(filepath.Join(fixture.productHome, "missing"), lock); err != nil || removed {
		t.Fatalf("missing managed removal = %v, err=%v", removed, err)
	}
	if removed, err := removeManagedPathChecked(badDir, lock); err == nil || removed {
		t.Fatalf("directory managed removal = %v, err=%v", removed, err)
	}
	if removed, err := removeInstallerLinkChecked(filepath.Join(t.TempDir(), threadpointProductName), fixture.binaryPath, lock); err == nil || removed {
		t.Fatalf("foreign command-link removal = %v, err=%v", removed, err)
	}
	if err := recoverInstallerLink(installMetadata{LinkPath: filepath.Join(t.TempDir(), threadpointProductName), BinaryPath: fixture.binaryPath}, lock); err == nil {
		t.Fatal("foreign command link recovery succeeded")
	}

	for _, invoke := range []func() error{
		func() error { return copyBundleEntryFromPinnedRoot(lock.transactionsRoot, "../unsafe", lock) },
		func() error { return copyBundleEntryToPinnedRoot(fixture.productHome, "../unsafe", lock) },
		func() error { return copyBundleEntryBetweenPinnedRoots(lock.transactionsRoot, "../unsafe", lock) },
	} {
		if err := invoke(); err == nil {
			t.Fatal("unsafe bundle copy entry was accepted")
		}
	}

	source := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("ambient copy"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := installBundleEntriesChecked(source, target, []string{"README.md"}, nil); err == nil || !strings.Contains(err.Error(), "retained managed install root") {
		t.Fatalf("ambient bundle copy was not rejected: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "README.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambient bundle copy published outside a retained root: %v", err)
	}
}

func TestCopyBundleEntryFromPinnedRootRejectsOversizedSourceBeforePublication(t *testing.T) {
	fixture := newInstallTransactionFixture(t, true)
	oversized := filepath.Join(fixture.productHome, "README.md")
	if err := os.Truncate(oversized, maxInstallManagedFileBytes+1); err != nil {
		t.Fatal(err)
	}
	lock := fixture.acquire(t)
	if err := lock.PinBundleRoot(fixture.productHome); err != nil {
		t.Fatal(err)
	}
	targetPath := t.TempDir()
	target, err := os.OpenRoot(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	err = copyBundleEntryFromPinnedRoot(target, "README.md", lock)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized pinned backup copy error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(targetPath, "README.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized backup source was published: %v", err)
	}
}

func TestInstallerManagedBinarySHA256IsPinnedBoundedAndNonblocking(t *testing.T) {
	t.Run("oversized under lifecycle lock", func(t *testing.T) {
		fixture := newInstallTransactionFixture(t, true)
		if err := os.Truncate(fixture.binaryPath, maxInstallManagedFileBytes+1); err != nil {
			t.Fatal(err)
		}
		lock := fixture.acquire(t)
		if _, err := installerManagedBinarySHA256(fixture.metadata, lock); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("oversized managed binary hash error = %v", err)
		}
	})

	t.Run("fifo without lifecycle lock", func(t *testing.T) {
		fixture := newInstallTransactionFixture(t, true)
		if err := os.Remove(fixture.binaryPath); err != nil {
			t.Fatal(err)
		}
		if err := syscallMkfifo(fixture.binaryPath); err != nil {
			t.Skipf("FIFO unavailable: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := installerManagedBinarySHA256(fixture.metadata, nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("FIFO managed binary hash error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("managed binary hash blocked on a FIFO")
		}
	})

	t.Run("symlink without lifecycle lock", func(t *testing.T) {
		fixture := newInstallTransactionFixture(t, true)
		foreign := filepath.Join(t.TempDir(), "foreign-binary")
		if err := os.WriteFile(foreign, []byte("foreign"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fixture.binaryPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(foreign, fixture.binaryPath); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := installerManagedBinarySHA256(fixture.metadata, nil); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink managed binary hash error = %v", err)
		}
	})
}

func TestValidateActiveInstallTransactionTargetsUsesPhaseExactGeneration(t *testing.T) {
	t.Run("normal completion rejects revert to prior", func(t *testing.T) {
		fixture := newInstallTransactionFixture(t, true)
		lock := fixture.acquire(t)
		transaction := prepareFixtureTransaction(t, fixture, lock)
		foreign := filepath.Join(t.TempDir(), "foreign-readme")
		if err := os.WriteFile(foreign, []byte("foreign generation"), 0o600); err != nil {
			t.Fatal(err)
		}
		key := installJournalBundleKey("README.md")
		target := transaction.journal.Targets[key]
		target.Produced = testInstallJournalGeneration(t, foreign, false)
		transaction.journal.Targets[key] = target

		if err := validateActiveInstallTransactionTargets(transaction, lock); err == nil || !strings.Contains(err.Error(), "produced generation") {
			t.Fatalf("normal completion accepted a target reverted to Prior: %v", err)
		}
	})

	t.Run("normal completion accepts intentionally unchanged link", func(t *testing.T) {
		fixture := newInstallTransactionFixture(t, true)
		lock := fixture.acquire(t)
		transaction := prepareFixtureTransaction(t, fixture, lock)
		for key, target := range transaction.journal.Targets {
			if key == "link" {
				continue
			}
			target.Produced = target.Prior
			transaction.journal.Targets[key] = target
		}
		if transaction.journal.Targets["link"].Produced.Kind != "" {
			t.Fatal("fixture unexpectedly recorded a produced link generation")
		}

		if err := validateActiveInstallTransactionTargets(transaction, lock); err != nil {
			t.Fatalf("normal completion rejected intentionally unchanged link: %v", err)
		}
	})

	t.Run("recovery completion rejects revert from recovery generation", func(t *testing.T) {
		fixture := newInstallTransactionFixture(t, true)
		lock := fixture.acquire(t)
		transaction := prepareFixtureTransaction(t, fixture, lock)
		foreign := filepath.Join(t.TempDir(), "foreign-recovery-readme")
		if err := os.WriteFile(foreign, []byte("recovery generation"), 0o600); err != nil {
			t.Fatal(err)
		}
		key := installJournalBundleKey("README.md")
		target := transaction.journal.Targets[key]
		target.Recovery = testInstallJournalGeneration(t, foreign, false)
		transaction.journal.Targets[key] = target
		lock.recoveringTransaction = true
		defer func() { lock.recoveringTransaction = false }()

		if err := validateActiveInstallTransactionTargets(transaction, lock); err == nil || !strings.Contains(err.Error(), "recovery generation") {
			t.Fatalf("recovery completion accepted a target reverted to Prior: %v", err)
		}
	})
}

func TestInstallerManagedFileReadsRejectOversizedSparseFiles(t *testing.T) {
	fixture := newInstallTransactionFixture(t, true)
	oversized := filepath.Join(fixture.productHome, "README.md")
	if err := os.Truncate(oversized, maxInstallManagedFileBytes+1); err != nil {
		t.Fatal(err)
	}
	lock := fixture.acquire(t)
	if _, err := prepareInstallTransaction(
		fixture.productHome,
		fixture.commandPath,
		updateOperationUpdate,
		fixture.metadataPath,
		fixture.metadata,
		lock,
	); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized backup error = %v", err)
	}

	root, err := os.OpenRoot(fixture.productHome)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := installRootFileSHA256(root, "README.md"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized hash error = %v", err)
	}
}

func TestInstallPublicationPreservesLastMomentUnrecordedReplacement(t *testing.T) {
	fixture := newInstallTransactionFixture(t, true)
	lock := fixture.acquire(t)
	transaction := prepareFixtureTransaction(t, fixture, lock)
	source := t.TempDir()
	writeThreadpointBundle(t, source, "new binary")
	target := filepath.Join(fixture.productHome, "README.md")
	installRemovalBeforeQuarantine = func(_ *os.Root, base string) {
		if base != "README.md" {
			return
		}
		installRemovalBeforeQuarantine = nil
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("foreign replacement\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { installRemovalBeforeQuarantine = nil })
	if err := installBundleEntriesChecked(source, fixture.productHome, threadpointBundleEntries, lock); err == nil {
		t.Fatal("publication accepted a last-moment unrecorded replacement")
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "foreign replacement\n" {
		t.Fatalf("publication changed the foreign replacement: body=%q err=%v", body, err)
	}
	if _, err := os.Lstat(transaction.journalPath); err != nil {
		t.Fatalf("failed publication removed its recovery journal: %v", err)
	}
	if err := recoverInstallTransaction(fixture.productHome, fixture.commandPath, lock); err == nil {
		t.Fatal("recovery overwrote an unrecorded target replacement")
	}
	body, err = os.ReadFile(target)
	if err != nil || string(body) != "foreign replacement\n" {
		t.Fatalf("failed recovery changed the foreign replacement: body=%q err=%v", body, err)
	}
}

func syscallMkfifo(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

func TestInstallQuarantineRejectsLostAuthorityAndRestoresOwnedEntry(t *testing.T) {
	for _, operation := range []string{"remove", "replace"} {
		for _, damage := range []string{"nil-parent", "closed-parent", "missing-identity", "escaping-name", "validation-failed", "target-removed", "target-changed", "prepared-removed", "read-only-parent"} {
			if operation == "remove" && damage == "prepared-removed" {
				continue
			}
			t.Run(operation+"/"+damage, func(t *testing.T) {
				dir := t.TempDir()
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "prepared"), []byte("new"), 0o600); err != nil {
					t.Fatal(err)
				}
				identity := mustInstallHelperIdentity(t, target)
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				info, err := root.Lstat("prepared")
				if err != nil {
					t.Fatal(err)
				}
				base := "target"
				var validate func() error
				sentinel := errors.New("install lock lost")
				switch damage {
				case "nil-parent":
					root = nil
				case "closed-parent":
					if err := root.Close(); err != nil {
						t.Fatal(err)
					}
				case "missing-identity":
					identity.info = nil
					info = nil
				case "escaping-name":
					base = "../target"
				case "validation-failed":
					validate = func() error { return sentinel }
				case "target-removed":
					if err := os.Remove(target); err != nil {
						t.Fatal(err)
					}
				case "target-changed":
					if err := os.WriteFile(target, []byte("third party"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "prepared-removed":
					if err := os.Remove(filepath.Join(dir, "prepared")); err != nil {
						t.Fatal(err)
					}
				case "read-only-parent":
					if err := os.Chmod(dir, 0o500); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
					if err := os.WriteFile(filepath.Join(dir, "permission-probe"), nil, 0o600); err == nil {
						t.Skip("filesystem does not enforce directory write permissions")
					}
				}
				if operation == "remove" {
					err = quarantineAndRemoveInstallEntry(root, base, identity, validate)
				} else {
					err = quarantineAndReplaceInstallEntry(root, base, "prepared", info, &identity, validate)
				}
				if err == nil {
					t.Fatal("invalid install mutation accepted")
				}
				if damage == "validation-failed" && !errors.Is(err, sentinel) {
					t.Fatalf("validation error lost: %v", err)
				}
				body, readErr := os.ReadFile(target)
				if damage == "target-removed" {
					if !errors.Is(readErr, os.ErrNotExist) {
						t.Fatalf("missing target recreated: %v", readErr)
					}
				} else {
					want := "original"
					if damage == "target-changed" {
						want = "third party"
					}
					if readErr != nil || string(body) != want {
						t.Fatalf("target not preserved: %q %v", body, readErr)
					}
				}
			})
		}
	}
}

func TestInstallCompletionRejectsMissingAndChangedOwnedTargets(t *testing.T) {
	for _, targetName := range []string{"bundle", "metadata", "link"} {
		for _, damage := range []string{"missing-journal-entry", "missing-file", "wrong-type", "replacement", "invalid-path"} {
			t.Run(targetName+"/"+damage, func(t *testing.T) {
				fixture := newInstallTransactionFixture(t, true)
				lock := fixture.acquire(t)
				transaction := prepareFixtureTransaction(t, fixture, lock)
				for key, target := range transaction.journal.Targets {
					target.Produced = target.Prior
					transaction.journal.Targets[key] = target
				}
				if err := validateActiveInstallTransactionTargets(transaction, lock); err != nil {
					t.Fatalf("valid baseline: %v", err)
				}
				path, key := fixture.binaryPath, installJournalBundleKey("bin/threadpoint")
				switch targetName {
				case "metadata":
					path, key = fixture.metadataPath, "metadata"
				case "link":
					path, key = fixture.commandPath, "link"
				}
				switch damage {
				case "missing-journal-entry":
					delete(transaction.journal.Targets, key)
				case "missing-file":
					if err := os.Rename(path, path+".retained"); err != nil {
						t.Fatal(err)
					}
				case "wrong-type":
					if err := os.Rename(path, path+".retained"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				case "replacement":
					if err := os.Rename(path, path+".retained"); err != nil {
						t.Fatal(err)
					}
					if targetName == "link" {
						if err := os.Symlink(fixture.binaryPath+".foreign", path); err != nil {
							t.Fatal(err)
						}
					} else {
						writeInstallHelperFile(t, path, []byte("concurrent"))
					}
				case "invalid-path":
					if targetName == "metadata" {
						transaction.journal.MetadataPath = filepath.Join(t.TempDir(), "metadata")
					} else {
						target := transaction.journal.Targets[key]
						target.Produced.Identity = "invalid"
						transaction.journal.Targets[key] = target
					}
				}
				before, beforeErr := os.Lstat(path)
				if err := validateActiveInstallTransactionTargets(transaction, lock); err == nil {
					t.Fatal("invalid completion accepted")
				}
				after, afterErr := os.Lstat(path)
				if os.IsNotExist(beforeErr) {
					if !os.IsNotExist(afterErr) {
						t.Fatalf("missing target recreated: %v", afterErr)
					}
				} else if beforeErr != nil || afterErr != nil || !os.SameFile(before, after) {
					t.Fatalf("validation changed target: %v %v", beforeErr, afterErr)
				}
			})
		}
	}
}

func TestInstallNoReplaceRefusesInvalidRequestsAndRestoresDetachedParents(t *testing.T) {
	for _, failure := range []string{"missing-arguments", "extra-arguments", "missing-identity", "missing-source-parent", "missing-destination-parent", "missing-source", "occupied-destination", "source-parent-replaced", "destination-parent-replaced"} {
		t.Run(failure, func(t *testing.T) {
			sourceDir, destinationDir := t.TempDir(), t.TempDir()
			source := filepath.Join(sourceDir, "source")
			destination := filepath.Join(destinationDir, "destination")
			writeInstallHelperFile(t, source, []byte("original"))
			args := []string{source, destination}
			expected := false
			retainedSource := source
			original := inheritedInstallHelperBeforeNoReplaceRename
			defer func() { inheritedInstallHelperBeforeNoReplaceRename = original }()
			switch failure {
			case "missing-arguments":
				args = args[:1]
			case "extra-arguments":
				args = append(args, "extra")
			case "missing-identity":
				expected = true
			case "missing-source-parent":
				args[0] = filepath.Join(sourceDir, "missing", "source")
			case "missing-destination-parent":
				args[1] = filepath.Join(destinationDir, "missing", "destination")
			case "missing-source":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
			case "occupied-destination":
				writeInstallHelperFile(t, destination, []byte("concurrent"))
			case "source-parent-replaced", "destination-parent-replaced":
				inheritedInstallHelperBeforeNoReplaceRename = func(string) {
					path := destinationDir
					if failure == "source-parent-replaced" {
						path = sourceDir
						retainedSource = filepath.Join(path+".retained", "source")
					}
					if err := os.Rename(path, path+".retained"); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.RemoveAll(path + ".retained") })
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
					writeInstallHelperFile(t, filepath.Join(path, "sentinel"), []byte("concurrent"))
				}
			}
			if err := runInstallNoReplaceExpected(args, expected); err == nil {
				t.Fatal("invalid rename succeeded")
			}
			if failure != "missing-source" {
				if body, err := os.ReadFile(retainedSource); err != nil || string(body) != "original" {
					t.Fatalf("original was not preserved: %q %v", body, err)
				}
			}
			if failure == "occupied-destination" {
				if body, err := os.ReadFile(destination); err != nil || string(body) != "concurrent" {
					t.Fatalf("destination replaced: %q %v", body, err)
				}
			} else if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("failed rename left destination: %v", err)
			}
		})
	}
}

func TestInstallBackupCopyRefusesUnstableSourcesAndUnusableDestinations(t *testing.T) {
	for _, failure := range []string{"missing-source", "source-directory", "source-symlink", "source-unreadable", "closed-bundle", "nil-target", "closed-target", "occupied-target", "unwritable-target"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, true)
			lock := fixture.acquire(t)
			if err := lock.PinBundleRoot(fixture.metadata.BundleRoot); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(fixture.metadata.BundleRoot, "README.md")
			targetDir := t.TempDir()
			target, err := os.OpenRoot(targetDir)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			selected := target
			originalBundle := lock.bundleRoot
			defer func() { lock.bundleRoot = originalBundle }()
			switch failure {
			case "missing-source", "source-directory", "source-symlink":
				if err := os.Rename(source, source+".retained"); err != nil {
					t.Fatal(err)
				}
				if failure == "source-directory" {
					if err := os.Mkdir(source, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "source-symlink" {
					if err := os.Symlink(source+".retained", source); err != nil {
						t.Fatal(err)
					}
				}
			case "source-unreadable":
				if err := os.Chmod(source, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(source, 0o600) }()
			case "closed-bundle":
				closed, err := os.OpenRoot(fixture.metadata.BundleRoot)
				if err != nil {
					t.Fatal(err)
				}
				_ = closed.Close()
				lock.bundleRoot = closed
			case "nil-target":
				selected = nil
			case "closed-target":
				_ = target.Close()
			case "occupied-target":
				if err := target.WriteFile("README.md", []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unwritable-target":
				if err := os.Chmod(targetDir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(targetDir, 0o700) }()
			}
			if err := copyBundleEntryFromPinnedRoot(selected, "README.md", lock); err == nil {
				t.Fatal("unsafe install backup copy succeeded")
			}
			if failure == "occupied-target" {
				body, err := os.ReadFile(filepath.Join(targetDir, "README.md"))
				if err != nil || string(body) != "foreign" {
					t.Fatalf("foreign destination changed: %q %v", body, err)
				}
			} else {
				entries, err := os.ReadDir(targetDir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("failed copy left destination residue: %v %v", entries, err)
				}
			}
		})
	}
}

func TestBoundedInstallCopyRejectsMalformedRequestsAndUnavailableParents(t *testing.T) {
	for _, failure := range []string{"arity", "identity", "digest-length", "digest-encoding", "source-parent", "destination-parent", "unreadable-source"} {
		t.Run(failure, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source")
			destination := filepath.Join(t.TempDir(), "destination")
			writeInstallHelperFile(t, source, []byte("reviewed"))
			generation := installHelperGeneration(t, source)
			args := []string{source, destination, generation.Identity, generation.Digest, "0600"}
			switch failure {
			case "arity":
				args = args[:4]
			case "identity":
				args[2] = "invalid"
			case "digest-length":
				args[3] = "short"
			case "digest-encoding":
				args[3] = strings.Repeat("g", 64)
			case "source-parent":
				args[0] = filepath.Join(t.TempDir(), "missing", "source")
			case "destination-parent":
				args[1] = filepath.Join(t.TempDir(), "missing", "destination")
			case "unreadable-source":
				if err := os.Chmod(source, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(source, 0o600) }()
			}
			if err := runInstallCopyBoundedAs(io.Discard, args); err == nil {
				t.Fatal("invalid bounded copy accepted")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid request wrote destination: %v", err)
			}
		})
	}
}

func TestBoundedInstallWriteRefusesInvalidInputAndUnwritableDestination(t *testing.T) {
	for _, failure := range []string{"arity", "nil-input", "invalid-mode", "missing-parent", "occupied", "unwritable-parent", "read-failure"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "destination")
			args := []string{path, "0600"}
			var input io.Reader = strings.NewReader("reviewed")
			switch failure {
			case "arity":
				args = args[:1]
			case "nil-input":
				input = nil
			case "invalid-mode":
				args[1] = "0777"
			case "missing-parent":
				args[0] = filepath.Join(dir, "missing", "destination")
			case "occupied":
				writeInstallHelperFile(t, path, []byte("foreign"))
			case "unwritable-parent":
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(dir, 0o700) }()
			case "read-failure":
				input = installInputErrorReader{}
			}
			if err := runInstallWriteBounded(io.Discard, input, args); err == nil {
				t.Fatal("invalid bounded write accepted")
			}
			if failure == "occupied" {
				body, err := os.ReadFile(path)
				if err != nil || string(body) != "foreign" {
					t.Fatalf("foreign destination changed: %q %v", body, err)
				}
			} else if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed write left destination: %v", err)
			}
		})
	}
}

type installInputErrorReader struct{}

func (installInputErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestBackupStateWriteRefusesIncompleteInputAndUnavailableNamespace(t *testing.T) {
	for _, failure := range []string{"closed-root", "unwritable", "read-error", "oversized", "occupied", "replaced-temporary"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			var source io.Reader = strings.NewReader("metadata")
			switch failure {
			case "closed-root":
				err = root.Close()
			case "unwritable":
				err = os.Chmod(dir, 0o500)
				defer func() { _ = os.Chmod(dir, 0o700) }()
			case "read-error":
				source = installInputErrorReader{}
			case "oversized":
				source = strings.NewReader(strings.Repeat("x", int(maxInstallTransactionBytes)+1))
			case "occupied":
				err = root.WriteFile("metadata", []byte("foreign"), 0o600)
			case "replaced-temporary":
				source = installSourceReaderFunc(func([]byte) (int, error) {
					names, err := os.ReadDir(dir)
					if err != nil {
						t.Fatal(err)
					}
					if len(names) != 1 {
						t.Fatalf("temporary entries: %v", names)
					}
					name := names[0].Name()
					if err := root.Rename(name, "retained"); err != nil {
						t.Fatal(err)
					}
					if err := root.WriteFile(name, []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
					return 0, io.EOF
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := writeInstallBackupRootFile(root, "metadata", source); err == nil {
				t.Fatal("incomplete backup accepted")
			}
			if failure == "occupied" {
				body, err := os.ReadFile(filepath.Join(dir, "metadata"))
				if err != nil || string(body) != "foreign" {
					t.Fatalf("foreign backup changed: %q %v", body, err)
				}
			} else if _, err := os.Lstat(filepath.Join(dir, "metadata")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed backup became visible: %v", err)
			}
		})
	}
}

type installSourceReaderFunc func([]byte) (int, error)

func (read installSourceReaderFunc) Read(body []byte) (int, error) { return read(body) }

func TestEntryIdentityHelperRefusesUntrustedPathsAndBrokenOutput(t *testing.T) {
	for _, failure := range []string{"arity", "relative", "filesystem-root", "missing-parent", "parent-file", "parent-symlink", "missing-entry", "parent-unreadable", "broken-output"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			parent := filepath.Join(dir, "parent")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "entry")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{path}
			output := io.Discard
			switch failure {
			case "arity":
				args = nil
			case "relative":
				args[0] = "entry"
			case "filesystem-root":
				args[0] = string(filepath.Separator)
			case "missing-parent":
				args[0] = filepath.Join(dir, "missing", "entry")
			case "parent-file", "parent-symlink":
				if err := os.Rename(parent, parent+".retained"); err != nil {
					t.Fatal(err)
				}
				if failure == "parent-file" {
					if err := os.WriteFile(parent, []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(parent+".retained", parent); err != nil {
					t.Fatal(err)
				}
			case "missing-entry":
				args[0] = filepath.Join(parent, "missing")
			case "parent-unreadable":
				if err := os.Chmod(parent, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(parent, 0o700) }()
			case "broken-output":
				output = installIdentityErrorWriter{}
			}
			err := runInstallEntryIdentity(output, args)
			if err == nil {
				t.Fatal("untrusted identity request accepted")
			}
			if failure == "broken-output" && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("write cause lost: %v", err)
			}
		})
	}
}

type installIdentityErrorWriter struct{}

func (installIdentityErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
