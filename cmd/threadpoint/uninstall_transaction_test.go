// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRemoveAsRequiresExactSafeContentProofAndEmptyDirectory(t *testing.T) {
	regular := filepath.Join(t.TempDir(), "regular")
	writeInstallHelperFile(t, regular, []byte("reviewed\n"))
	regularID, regularKind, regularProof := inspectInstallHelperEntry(t, regular)
	if regularKind != "regular" {
		t.Fatalf("regular kind = %q", regularKind)
	}
	// Truncating and rewriting preserves the inode on ordinary filesystems;
	// the stale proof must nevertheless preserve the changed entry.
	writeInstallHelperFile(t, regular, []byte("same inode replacement\n"))
	if installHelperPathIdentity(t, regular) != regularID {
		t.Skip("filesystem replaced the inode during same-path rewrite")
	}
	if err := runInstallRemoveAs([]string{regular, regularID, regularKind, regularProof}); err == nil {
		t.Fatal("same-inode content mutation was removed with a stale proof")
	}
	if _, err := os.Lstat(regular); err != nil {
		t.Fatalf("changed regular entry was not preserved: %v", err)
	}
	regularID, regularKind, regularProof = inspectInstallHelperEntry(t, regular)
	if err := runInstallRemoveAs([]string{regular, regularID, regularKind, regularProof}); err != nil {
		t.Fatalf("remove exact regular entry: %v", err)
	}

	symlink := filepath.Join(t.TempDir(), "symlink")
	craftedTarget := "foreign\\target\"with-quote\nand-newline"
	if err := os.Symlink(craftedTarget, symlink); err != nil {
		t.Fatal(err)
	}
	symlinkID, symlinkKind, symlinkProof := inspectInstallHelperEntry(t, symlink)
	if symlinkKind != "symlink" || strings.ContainsAny(symlinkProof, "\\\" \t\r\n") {
		t.Fatalf("unsafe symlink inspection: %q %q", symlinkKind, symlinkProof)
	}
	if err := runInstallRemoveAs([]string{symlink, symlinkID, symlinkKind, symlinkProof}); err != nil {
		t.Fatalf("remove exact crafted symlink: %v", err)
	}

	directory := filepath.Join(t.TempDir(), "empty-only")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeInstallHelperFile(t, filepath.Join(directory, "child"), []byte("preserve\n"))
	directoryID, directoryKind, directoryProof := inspectInstallHelperEntry(t, directory)
	if directoryKind != "directory" || directoryProof != "-" {
		t.Fatalf("directory inspection = %q %q", directoryKind, directoryProof)
	}
	if err := runInstallRemoveAs([]string{directory, directoryID, directoryKind, directoryProof}); err == nil {
		t.Fatal("non-empty directory was removed")
	}
	if err := os.Remove(filepath.Join(directory, "child")); err != nil {
		t.Fatal(err)
	}
	if err := runInstallRemoveAs([]string{directory, directoryID, directoryKind, directoryProof}); err != nil {
		t.Fatalf("remove exact empty directory: %v", err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty directory remains: %v", err)
	}
}

func TestInstallRemovalIdentityAndBoundedCopyRejectWrongEntryTypes(t *testing.T) {
	rootPath := t.TempDir()
	regularPath := filepath.Join(rootPath, "regular")
	writeInstallHelperFile(t, regularPath, []byte("reviewed\n"))
	if err := os.Symlink("regular", filepath.Join(rootPath, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	regularInfo, err := root.Lstat("regular")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := installRemovalIdentityString(nil); err == nil {
		t.Fatal("nil install identity accepted")
	}
	if _, err := recordedInstallGeneration(installRemovalIdentity{info: regularInfo}); err == nil {
		t.Fatal("untyped install generation accepted")
	}
	if installGenerationMatches(installRemovalIdentity{}, installJournalGeneration{}) {
		t.Fatal("identity-free generation matched")
	}
	if _, err := snapshotRegularInstallRemoval(root, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing regular snapshot error = %v", err)
	}
	if _, err := snapshotRegularInstallRemoval(root, "link"); err == nil {
		t.Fatal("symlink accepted as a regular snapshot")
	}
	if _, err := snapshotSymlinkInstallRemoval(root, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing symlink snapshot error = %v", err)
	}
	if _, err := snapshotSymlinkInstallRemoval(root, "regular"); err == nil {
		t.Fatal("regular file accepted as a symlink snapshot")
	}
	if _, err := openInstallRegularFile(nil, "regular"); err == nil {
		t.Fatal("nil install root accepted")
	}
	if err := copyInstallFileBounded(nil, nil); err == nil {
		t.Fatal("nil bounded-copy endpoints accepted")
	}
	directory, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := copyInstallFileBounded(rejectedInstallHelperWrite{}, directory); err == nil {
		t.Fatal("directory accepted as an installer-managed file")
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	regular, err := openInstallRegularFile(root, "regular")
	if err != nil {
		t.Fatal(err)
	}
	if err := copyInstallFileBounded(rejectedInstallHelperWrite{}, regular); err == nil || !strings.Contains(err.Error(), "output failure") {
		t.Fatalf("bounded copy output error = %v", err)
	}
	if err := regular.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallQuarantineValidationAndRestorationBranches(t *testing.T) {
	parentPath := t.TempDir()
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	writeInstallHelperFile(t, filepath.Join(parentPath, "regular"), []byte("reviewed\n"))
	regular, err := snapshotRegularInstallRemoval(parent, "regular")
	if err != nil {
		t.Fatal(err)
	}
	if err := quarantineAndRemoveInstallEntry(parent, "regular", regular, func() error {
		return errors.New("injected authorization change")
	}); err == nil || !strings.Contains(err.Error(), "authorization change") {
		t.Fatalf("quarantine validation error = %v", err)
	}
	if _, err := parent.Lstat("regular"); err != nil {
		t.Fatalf("validation failure removed reviewed entry: %v", err)
	}

	if err := removeInstallTemporaryIfSame(nil, "anything", nil); err != nil {
		t.Fatalf("nil temporary cleanup = %v", err)
	}
	if err := removeInstallTemporaryIfSame(parent, "missing", regular.info); err != nil {
		t.Fatalf("missing temporary cleanup = %v", err)
	}
	writeInstallHelperFile(t, filepath.Join(parentPath, "other"), []byte("other\n"))
	otherInfo, err := parent.Lstat("other")
	if err != nil {
		t.Fatal(err)
	}
	if err := removeInstallTemporaryIfSame(parent, "regular", otherInfo); err == nil {
		t.Fatal("temporary cleanup removed a different generation")
	}

	writeInstallHelperFile(t, filepath.Join(parentPath, "temporary"), []byte("publish\n"))
	temporaryInfo, err := parent.Lstat("temporary")
	if err != nil {
		t.Fatal(err)
	}
	if err := quarantineAndReplaceInstallEntry(parent, "published", "temporary", temporaryInfo, nil, func() error {
		return errors.New("injected publish authorization change")
	}); err == nil {
		t.Fatal("replacement ignored validation failure")
	}
	if _, err := parent.Lstat("temporary"); err != nil {
		t.Fatalf("validation failure consumed temporary: %v", err)
	}
	if err := quarantineAndReplaceInstallEntry(parent, "published", "temporary", temporaryInfo, nil, nil); err != nil {
		t.Fatalf("publish into absent destination: %v", err)
	}

	quarantinePath := t.TempDir()
	quarantine, err := os.OpenRoot(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	defer quarantine.Close()
	writeInstallHelperFile(t, filepath.Join(quarantinePath, "candidate"), []byte("restore\n"))
	moved, err := quarantine.Lstat("candidate")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreQuarantinedInstallEntry(quarantine, otherInfo, parent, "restored"); err == nil {
		t.Fatal("restore accepted mismatched reviewed identity")
	}
	if err := restoreQuarantinedInstallEntry(quarantine, moved, parent, "restored"); err != nil {
		t.Fatalf("restore exact quarantined entry: %v", err)
	}
	if err := restoreQuarantinedInstallEntry(quarantine, moved, parent, "second"); err == nil {
		t.Fatal("restore accepted a missing quarantine candidate")
	}
}

func TestVerifyQuarantinedInstallEntryChecksTypeAndContent(t *testing.T) {
	rootPath := t.TempDir()
	writeInstallHelperFile(t, filepath.Join(rootPath, "regular"), []byte("reviewed\n"))
	if err := os.Symlink("opaque-target", filepath.Join(rootPath, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	regular, err := snapshotRegularInstallRemoval(root, "regular")
	if err != nil {
		t.Fatal(err)
	}
	symlink, err := snapshotSymlinkInstallRemoval(root, "link")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyQuarantinedInstallEntry(root, "regular", nil, regular); err == nil {
		t.Fatal("nil quarantined entry accepted")
	}
	if err := verifyQuarantinedInstallEntry(root, "regular", regular.info, installRemovalIdentity{info: regular.info, symlink: true}); err == nil {
		t.Fatal("regular entry accepted as reviewed symlink")
	}
	wrongDigest := regular
	wrongDigest.digest = strings.Repeat("0", 64)
	if err := verifyQuarantinedInstallEntry(root, "regular", regular.info, wrongDigest); err == nil {
		t.Fatal("wrong quarantined digest accepted")
	}
	if err := verifyQuarantinedInstallEntry(root, "regular", regular.info, regular); err != nil {
		t.Fatalf("exact quarantined regular entry: %v", err)
	}
	wrongTarget := symlink
	wrongTarget.linkTarget = "different"
	if err := verifyQuarantinedInstallEntry(root, "link", symlink.info, wrongTarget); err == nil {
		t.Fatal("wrong quarantined symlink target accepted")
	}
	if err := verifyQuarantinedInstallEntry(root, "link", symlink.info, symlink); err != nil {
		t.Fatalf("exact quarantined symlink: %v", err)
	}
	if err := verifyQuarantinedInstallEntry(root, "regular", regular.info, installRemovalIdentity{info: regular.info}); err == nil {
		t.Fatal("unsupported reviewed entry type accepted")
	}
}

func TestManagedUninstallRestoresLastMomentRegularReplacement(t *testing.T) {
	fixture := newInstallTransactionFixture(t, false)
	lock := fixture.acquire(t)
	if err := lock.PinBundleRoot(fixture.productHome); err != nil {
		t.Fatal(err)
	}

	originalHook := installRemovalBeforeQuarantine
	t.Cleanup(func() { installRemovalBeforeQuarantine = originalHook })
	installRemovalBeforeQuarantine = func(parent *os.Root, base string) {
		installRemovalBeforeQuarantine = nil
		if base != "README.md" {
			t.Fatalf("quarantine hook base = %q, want README.md", base)
		}
		if err := parent.Remove(base); err != nil {
			t.Fatal(err)
		}
		if err := parent.WriteFile(base, []byte("unrelated replacement\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := removeBundleEntryChecked("README.md", lock)
	if err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatalf("remove replacement = (%v, %v), want ownership refusal", removed, err)
	}
	if removed {
		t.Fatal("ownership mismatch was reported as removed")
	}
	body, readErr := os.ReadFile(filepath.Join(fixture.productHome, "README.md"))
	if readErr != nil || string(body) != "unrelated replacement\n" {
		t.Fatalf("last-moment replacement was not restored: body=%q err=%v", body, readErr)
	}
	assertNoInstallRemovalQuarantines(t, fixture.productHome)
}

func TestManagedUninstallRestoresLastMomentLinkReplacement(t *testing.T) {
	fixture := newInstallTransactionFixture(t, true)
	lock := fixture.acquire(t)
	replacementTarget := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(replacementTarget, []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalHook := installRemovalBeforeQuarantine
	t.Cleanup(func() { installRemovalBeforeQuarantine = originalHook })
	installRemovalBeforeQuarantine = func(parent *os.Root, base string) {
		installRemovalBeforeQuarantine = nil
		if err := parent.Remove(base); err != nil {
			t.Fatal(err)
		}
		if err := parent.Symlink(replacementTarget, base); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := removeInstallerLinkChecked(fixture.commandPath, fixture.binaryPath, lock)
	if err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatalf("remove replacement = (%v, %v), want ownership refusal", removed, err)
	}
	if removed {
		t.Fatal("ownership mismatch was reported as removed")
	}
	target, readErr := os.Readlink(fixture.commandPath)
	if readErr != nil || target != replacementTarget {
		t.Fatalf("last-moment link replacement was not restored: target=%q err=%v", target, readErr)
	}
	assertNoInstallRemovalQuarantines(t, fixture.commandDir)
}

func assertNoInstallRemovalQuarantines(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".threadpoint-uninstall-") {
			t.Fatalf("uninstall quarantine residue remains at %s", filepath.Join(root, entry.Name()))
		}
	}
}
