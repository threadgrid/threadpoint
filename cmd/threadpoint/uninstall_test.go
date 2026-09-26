// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestUninstallRejectsAmbiguousInvocationBeforeMutation(t *testing.T) {
	tests := [][]string{
		{"uninstall", "positional"},
		{"uninstall", "--plan=false"},
		{"uninstall", "--plan", "--apply", "--yes"},
		{"uninstall", "--apply"},
		{"uninstall", "--format", "yaml"},
	}
	for _, args := range tests {
		name := strings.Join(args[1:], "_")
		t.Run(name, func(t *testing.T) {
			if _, _, err := runTestCLI(t, args...); err == nil {
				t.Fatalf("ambiguous uninstall invocation succeeded: %v", args)
			}
		})
	}
}

func TestStandaloneUninstallerDoesNotRequireSiblingScriptOrManagedBinary(t *testing.T) {
	caseDir := t.TempDir()
	rawDir := filepath.Join(caseDir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceScript := filepath.Join("..", "..", "scripts", "uninstall.sh")
	body, err := os.ReadFile(sourceScript)
	if err != nil {
		t.Fatal(err)
	}
	rawScript := filepath.Join(rawDir, "uninstall.sh")
	if err := os.WriteFile(rawScript, body, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(caseDir, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	goLog := filepath.Join(caseDir, "go.log")
	fakeGo := filepath.Join(fakeBin, "go")
	if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\n{ printf 'dir=%s\\n' \"$THREADPOINT_INSTALL_DIR\"; printf '%s\\n' \"$@\"; } >\"$THREADPOINT_TEST_GO_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(caseDir, "home")
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A non-executable partial binary must not prevent the independent Go
	// module fallback from running.
	if err := os.WriteFile(filepath.Join(home, "bin", "threadpoint"), []byte("partial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", rawScript, "--plan") //nolint:noctx // short-lived local shell regression.
	// A caller-controlled compatibility-looking variable must not expose an
	// alternate shell parser; the raw script always reaches the Go owner.
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+":/usr/bin:/bin",
		"HOME="+caseDir,
		"THREADPOINT_HOME="+home,
		"THREADPOINT_UNINSTALL_LEGACY_TEST_MODE=1",
		"THREADPOINT_TEST_GO_LOG="+goLog,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("raw standalone uninstall failed: %v\n%s", err, output)
	}
	logged, err := os.ReadFile(goLog)
	if err != nil {
		t.Fatal(err)
	}
	want := "dir=" + filepath.Join(caseDir, ".local", "bin") + "\nrun\ngithub.com/threadgrid/threadpoint/cmd/threadpoint@latest\nuninstall\n--plan\n"
	if string(logged) != want {
		t.Fatalf("standalone Go delegation = %q, want %q", logged, want)
	}
}

func TestUninstallRejectsSymlinkMetadataWithoutBundleOwnership(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installDir := t.TempDir()
	binaryPath := filepath.Join(installDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	mustWrite(t, binaryPath, "payload")
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	sha, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}

	metadata := installMetadata{
		SchemaVersion: 1,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		InstallDir:    installDir,
		BinaryPath:    binaryPath,
		Version:       "v0.0.0",
		ReleaseBase:   "https://github.com/threadgrid/threadpoint/releases/download/v0.0.0",
		Archive:       "threadpoint_0.1.0_linux_amd64.tar.gz",
		ArchiveSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		BinarySHA256:  sha,
	}
	if err := writeInstallMetadata(metadataPath, metadata); err != nil {
		t.Fatal(err)
	}

	_, _, err = runTestCLI(t, "uninstall", "--dir", installDir)
	if err == nil || !strings.Contains(err.Error(), "link path") {
		t.Fatalf("expected missing bundle-aware metadata refusal, got %v", err)
	}
}

func TestUninstallRejectsMetadataWithoutBundleOwnership(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	managedDir := t.TempDir()
	binaryPath := filepath.Join(managedDir, "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	mustWrite(t, binaryPath, "payload")
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sha, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata := installMetadata{
		SchemaVersion: 1,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		InstallDir:    commandDir,
		BinaryPath:    binaryPath,
		LinkPath:      linkPath,
		Version:       "v0.0.0",
		ReleaseBase:   "https://github.com/threadgrid/threadpoint/releases/download/v0.0.0",
		Archive:       "threadpoint_0.1.0_linux_amd64.tar.gz",
		ArchiveSHA256: strings.Repeat("0", 64),
		BinarySHA256:  sha,
	}
	if err := writeInstallMetadata(metadataPath, metadata); err != nil {
		t.Fatal(err)
	}

	_, _, err = runTestCLI(t, "uninstall", "--dir", commandDir)
	if err == nil || !strings.Contains(err.Error(), "bundle_root") {
		t.Fatalf("expected metadata without bundle ownership to be refused, got %v", err)
	}
}

func TestUninstallPlanAndApplyRemovesManagedBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	writeThreadpointBundle(t, bundleRoot, "payload")
	if err := os.WriteFile(filepath.Join(bundleRoot, "customer-note.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sha, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInstallMetadata(metadataPath, installMetadata{
		SchemaVersion: 1,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		InstallDir:    commandDir,
		BinaryPath:    binaryPath,
		LinkPath:      linkPath,
		BundleRoot:    bundleRoot,
		BundleEntries: threadpointBundleEntries,
		Version:       "v0.0.0",
		ReleaseBase:   "https://github.com/threadgrid/threadpoint/releases/download/v0.0.0",
		Archive:       "threadpoint_0.1.0_linux_amd64.tar.gz",
		ArchiveSHA256: strings.Repeat("0", 64),
		BinarySHA256:  sha,
	}); err != nil {
		t.Fatal(err)
	}

	textStdout, _, err := runTestCLI(t, "uninstall", "--dir", commandDir)
	if err != nil {
		t.Fatalf("expected bundle text plan mode to succeed, got %v", err)
	}
	if !strings.Contains(textStdout, "command: threadpoint uninstall") || strings.Contains(strings.TrimSpace(textStdout), "{") {
		t.Fatalf("unexpected uninstall text output:\n%s", textStdout)
	}

	stdout, _, err := runTestCLI(t, "uninstall", "--dir", commandDir, "--format", "json")
	if err != nil {
		t.Fatalf("expected bundle plan mode to succeed, got %v", err)
	}
	var report uninstallReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("uninstall stdout not valid JSON: %v\n%s", err, stdout)
	}
	if report.BundleRoot != bundleRoot || len(report.WouldRemove) != len(threadpointBundleEntries)+2 {
		t.Fatalf("unexpected bundle uninstall report: %#v", report)
	}
	if len(report.Notes) == 0 || !strings.Contains(strings.Join(report.Notes, "\n"), "preserved") {
		t.Fatalf("expected preserved-state uninstall notes, got %#v", report.Notes)
	}

	_, _, err = runTestCLI(t, "uninstall", "--dir", commandDir, "--apply", "--yes", "--format", "json")
	if err != nil {
		t.Fatalf("expected bundle uninstall apply to succeed, got %v", err)
	}
	for _, rel := range threadpointBundleEntries {
		path := filepath.Join(bundleRoot, filepath.FromSlash(rel))
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("expected bundle entry %s removed, got %v", path, err)
		}
	}
	for _, path := range []string{linkPath, metadataPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, got %v", path, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(bundleRoot, "customer-note.txt")); err != nil || string(got) != "keep\n" {
		t.Fatalf("expected unrelated product-home file preserved, got %q err=%v", got, err)
	}
}

func TestUninstallRetryRecoversBeforeCompletingAfterBlockedRemoval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	writeThreadpointBundle(t, bundleRoot, "payload")
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sha, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInstallMetadata(metadataPath, installMetadata{
		SchemaVersion: 1,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		InstallDir:    commandDir,
		BinaryPath:    binaryPath,
		LinkPath:      linkPath,
		BundleRoot:    bundleRoot,
		BundleEntries: threadpointBundleEntries,
		Version:       "v0.0.0",
		ReleaseBase:   "https://github.com/threadgrid/threadpoint/releases/download/v0.0.0",
		Archive:       "threadpoint_0.1.0_linux_amd64.tar.gz",
		ArchiveSHA256: strings.Repeat("0", 64),
		BinarySHA256:  sha,
	}); err != nil {
		t.Fatal(err)
	}

	blockedEntry := filepath.Join(bundleRoot, "README.md")
	if err := os.Remove(blockedEntry); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blockedEntry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blockedEntry, "keep"), []byte("blocked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err = runTestCLI(t, "uninstall", "--dir", commandDir, "--apply", "--yes")
	if err == nil {
		t.Fatal("expected partial uninstall failure")
	}
	if _, err := os.Lstat(linkPath); err != nil {
		t.Fatalf("command symlink should remain available for recovery: %v", err)
	}
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("installer metadata should remain available for recovery: %v", err)
	}
	if got, err := os.ReadFile(binaryPath); err != nil || string(got) != "payload" {
		t.Fatalf("durable uninstall recovery should restore the binary before returning, got %q err=%v", got, err)
	}

	if err := os.RemoveAll(blockedEntry); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockedEntry, []byte("old readme"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runTestCLI(t, "uninstall", "--dir", commandDir, "--apply", "--yes"); err != nil {
		t.Fatalf("retry should finish after the corrupted managed entry is repaired: %v", err)
	}
	for _, path := range []string{linkPath, metadataPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed after retry, got %v", path, err)
		}
	}
}

func TestUninstallApplyAcceptsAlreadyMissingManagedBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	writeThreadpointBundle(t, bundleRoot, "payload")
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sha, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInstallMetadata(metadataPath, installMetadata{
		SchemaVersion: 1,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		InstallDir:    commandDir,
		BinaryPath:    binaryPath,
		LinkPath:      linkPath,
		BundleRoot:    bundleRoot,
		BundleEntries: threadpointBundleEntries,
		Version:       "v0.0.0",
		ReleaseBase:   "https://github.com/threadgrid/threadpoint/releases/download/v0.0.0",
		Archive:       "threadpoint_0.1.0_linux_amd64.tar.gz",
		ArchiveSHA256: strings.Repeat("0", 64),
		BinarySHA256:  sha,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(binaryPath); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runTestCLI(t, "uninstall", "--dir", commandDir, "--apply", "--yes"); err != nil {
		t.Fatalf("missing managed binary should not block standalone uninstall: %v", err)
	}
	for _, rel := range threadpointBundleEntries {
		path := filepath.Join(bundleRoot, filepath.FromSlash(rel))
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("expected bundle entry %s removed, got %v", path, err)
		}
	}
	for _, path := range []string{linkPath, metadataPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, got %v", path, err)
		}
	}
}

func TestUninstallRejectsMetadataLinkHijack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := t.TempDir()
	installDir := filepath.Join(bundleRoot, "bin")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	wrongTarget := filepath.Join(bundleRoot, "bin", "other-threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	mustWrite(t, binaryPath, "payload")
	mustWrite(t, wrongTarget, "other")
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wrongTarget, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sha, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInstallMetadata(metadataPath, installMetadata{
		SchemaVersion: 1,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		InstallDir:    installDir,
		BinaryPath:    binaryPath,
		LinkPath:      linkPath,
		BundleRoot:    bundleRoot,
		BundleEntries: threadpointBundleEntries,
		Version:       "v0.0.0",
		ReleaseBase:   "https://github.com/threadgrid/threadpoint/releases/download/v0.0.0",
		Archive:       "threadpoint_0.1.0_linux_amd64.tar.gz",
		ArchiveSHA256: strings.Repeat("0", 64),
		BinarySHA256:  sha,
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err = runTestCLI(t, "uninstall", "--dir", installDir)
	if err == nil || !strings.Contains(err.Error(), "points at") {
		t.Fatalf("expected metadata link hijack refusal, got %v", err)
	}
}

func TestUninstallRejectsChecksumMismatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	mustWrite(t, binaryPath, "payload")
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	metadata := installMetadata{
		SchemaVersion: 1,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		InstallDir:    commandDir,
		BinaryPath:    binaryPath,
		LinkPath:      linkPath,
		BundleRoot:    bundleRoot,
		BundleEntries: threadpointBundleEntries,
		Version:       "v0.0.0",
		ReleaseBase:   "https://github.com/threadgrid/threadpoint/releases/download/v0.0.0",
		Archive:       "threadpoint_0.1.0_linux_amd64.tar.gz",
		ArchiveSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		BinarySHA256:  "0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := writeInstallMetadata(metadataPath, metadata); err != nil {
		t.Fatal(err)
	}

	_, _, err := runTestCLI(t, "uninstall", "--dir", commandDir)
	if err == nil || !strings.Contains(err.Error(), "current binary checksum does not match installer metadata") {
		t.Fatalf("expected checksum mismatch refusal, got %v", err)
	}
}

func TestDefaultUninstallDirUsesInstallDirEnv(t *testing.T) {
	home := t.TempDir()
	installDir := t.TempDir()
	t.Setenv("THREADPOINT_INSTALL_DIR", installDir)

	if got := defaultUninstallDir(filepath.Join(home, ".threadpoint")); got != installDir {
		t.Fatalf("default uninstall dir = %q, want %q", got, installDir)
	}
}

func TestDefaultUninstallDirUsesManagedSymlinkMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binaryPath := currentExecutablePath()
	commandDir := t.TempDir()
	linkPath := filepath.Join(commandDir, "threadpoint")
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := writeInstallMetadata(installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath), installMetadata{
		SchemaVersion: 1,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		InstallDir:    commandDir,
		BinaryPath:    binaryPath,
		LinkPath:      linkPath,
		BinarySHA256:  strings.Repeat("0", 64),
	}); err != nil {
		t.Fatal(err)
	}

	if got := defaultUninstallDir(filepath.Join(home, ".threadpoint")); got != commandDir {
		t.Fatalf("default uninstall dir = %q, want symlink dir %q", got, commandDir)
	}
}

func TestManagedUninstallRejectsReplacementAfterTransactionPreparation(t *testing.T) {
	fixture := newInstallTransactionFixture(t, false)
	lock := fixture.acquire(t)
	transaction, err := prepareInstallTransaction(
		fixture.productHome, fixture.commandPath, updateOperationUninstall,
		fixture.metadataPath, fixture.metadata, lock,
	)
	if err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(fixture.productHome, "README.md")
	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readme, []byte("unrelated after prepare\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := removeBundleEntryChecked("README.md", lock)
	if err == nil || !strings.Contains(err.Error(), "ownership changed after transaction preparation") {
		t.Fatalf("remove post-prepare replacement = (%v, %v), want ownership refusal", removed, err)
	}
	body, readErr := os.ReadFile(readme)
	if readErr != nil || string(body) != "unrelated after prepare\n" {
		t.Fatalf("post-prepare replacement was changed: body=%q err=%v", body, readErr)
	}
	if err := recoverInstallTransaction(fixture.productHome, fixture.commandPath, lock); !errors.Is(err, errInstallRecoveryOwnershipMismatch) {
		t.Fatalf("recover replacement error = %v, want ownership mismatch", err)
	}
	body, readErr = os.ReadFile(readme)
	if readErr != nil || string(body) != "unrelated after prepare\n" {
		t.Fatalf("uninstall recovery overwrote replacement: body=%q err=%v", body, readErr)
	}
	if _, err := os.Lstat(transaction.journalPath); err != nil {
		t.Fatalf("failed recovery removed durable journal: %v", err)
	}
}
