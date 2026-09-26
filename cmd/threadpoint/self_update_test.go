// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestSelfUpdateNoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	currentPayload := []byte("current-threadpoint")
	archiveName := detectSelfUpdateArchiveName("v0.1.0")
	writeThreadpointBundle(t, bundleRoot, string(currentPayload))
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	currentSHA, err := backup.FileSHA256(binaryPath)
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
		Version:       "v0.1.0",
		ReleaseBase:   "https://github.com/threadgrid/threadpoint/releases/download/v0.1.0",
		Archive:       archiveName,
		ArchiveSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		BinarySHA256:  currentSHA,
	}); err != nil {
		t.Fatal(err)
	}

	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		return releaseMetadata{
			TagName: "v0.1.0",
			HTMLURL: "https://example.invalid/releases/tag/v0.1.0",
		}, nil
	}
	release.currentVersion = func() string {
		return "v0.1.0"
	}
	release.download = func(context.Context, string, string) error {
		t.Fatal("managed update should not download when already up-to-date")
		return nil
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "--dir", commandDir, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var report selfUpdateReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("managed update JSON invalid: %v\n%s", err, stdout)
	}
	if report.Updated {
		t.Fatalf("expected no update: %#v", report)
	}
	if report.Code != "update_current" || report.Message == "" {
		t.Fatalf("expected coded current outcome, got %#v", report)
	}
	currentData, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(currentData) != string(currentPayload) {
		t.Fatalf("expected binary content unchanged")
	}
}

func TestSelfUpdateUpdatesBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	currentPayload := []byte("current-threadpoint")
	nextPayload := []byte("updated-threadpoint")
	writeThreadpointBundle(t, bundleRoot, string(currentPayload))
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	currentSHA, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	releaseDir := t.TempDir()
	archivePath, checksumPath, err := writeThreadpointReleaseAssets(t, releaseDir, string(nextPayload))
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
		Archive:       detectSelfUpdateArchiveName("v0.1.0"),
		ArchiveSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		BinarySHA256:  currentSHA,
	}); err != nil {
		t.Fatal(err)
	}

	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		return releaseMetadata{
			TagName: "v0.1.0",
			HTMLURL: "https://example.invalid/releases/tag/v0.1.0",
		}, nil
	}
	release.currentVersion = func() string {
		return "v0.0.0"
	}
	release.download = func(_ context.Context, source string, destination string) error {
		switch path.Base(source) {
		case path.Base(archivePath):
			return copyFileContents(archivePath, destination)
		case "checksums.txt":
			return copyFileContents(checksumPath, destination)
		case "checksums.txt.sig":
			return copyFileContents(checksumPath+".sig", destination)
		default:
			return fmt.Errorf("unexpected source %q", source)
		}
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "--dir", commandDir, "--format", "json", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	var report selfUpdateReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("managed update JSON invalid: %v\n%s", err, stdout)
	}
	if !report.Updated {
		t.Fatalf("expected update to happen: %#v", report)
	}
	if report.Code != "update_applied" || report.Message == "" {
		t.Fatalf("expected coded applied outcome, got %#v", report)
	}
	metadata, err := readInstallMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	newBinaryHash, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.BinarySHA256 != newBinaryHash {
		t.Fatalf("expected metadata binary checksum to match updated binary")
	}
	if metadata.Version != "v0.1.0" {
		t.Fatalf("expected metadata version to update, got %s", metadata.Version)
	}
	updatedData, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(updatedData) != string(nextPayload) {
		t.Fatalf("expected updated binary payload, got %q", updatedData)
	}
}

func TestSelfUpdateUpdatesManagedBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	writeThreadpointBundle(t, bundleRoot, "current-threadpoint")
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	currentSHA, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	releaseDir := t.TempDir()
	archivePath, checksumPath, err := writeThreadpointReleaseAssets(t, releaseDir, "updated-threadpoint")
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
		Archive:       detectSelfUpdateArchiveName("v0.1.0"),
		ArchiveSHA256: strings.Repeat("0", 64),
		BinarySHA256:  currentSHA,
	}); err != nil {
		t.Fatal(err)
	}

	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		return releaseMetadata{TagName: "v0.1.0", HTMLURL: "https://example.invalid/releases/tag/v0.1.0"}, nil
	}
	release.currentVersion = func() string { return "v0.0.0" }
	release.download = func(_ context.Context, source string, destination string) error {
		switch path.Base(source) {
		case path.Base(archivePath):
			return copyFileContents(archivePath, destination)
		case "checksums.txt":
			return copyFileContents(checksumPath, destination)
		case "checksums.txt.sig":
			return copyFileContents(checksumPath+".sig", destination)
		default:
			return fmt.Errorf("unexpected source %q", source)
		}
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "--dir", commandDir, "--format", "json", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	var report selfUpdateReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("managed update JSON invalid: %v\n%s", err, stdout)
	}
	if !report.Updated || report.BinaryPath != binaryPath || report.LinkPath != linkPath {
		t.Fatalf("expected bundle update report, got %#v", report)
	}
	if got, err := os.ReadFile(binaryPath); err != nil || string(got) != "updated-threadpoint" {
		t.Fatalf("updated binary = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(bundleRoot, "README.md")); err != nil || string(got) != "# threadpoint\n" {
		t.Fatalf("updated README = %q err=%v", got, err)
	}
	metadata, err := readInstallMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SchemaVersion != 1 || metadata.Version != "v0.1.0" || metadata.BundleRoot != bundleRoot || !sameStringSet(metadata.BundleEntries, threadpointBundleEntries) {
		t.Fatalf("unexpected updated metadata: %#v", metadata)
	}
}

func TestSelfUpdateUpdatesBinaryThroughManagedSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	currentPayload := []byte("current-threadpoint")
	nextPayload := []byte("updated-threadpoint-symlink")
	writeThreadpointBundle(t, bundleRoot, string(currentPayload))
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	currentSHA, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	releaseDir := t.TempDir()
	archivePath, checksumPath, err := writeThreadpointReleaseAssets(t, releaseDir, string(nextPayload))
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
		Archive:       detectSelfUpdateArchiveName("v0.1.0"),
		ArchiveSHA256: strings.Repeat("0", 64),
		BinarySHA256:  currentSHA,
	}); err != nil {
		t.Fatal(err)
	}

	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		return releaseMetadata{TagName: "v0.1.0", HTMLURL: "https://example.invalid/releases/tag/v0.1.0"}, nil
	}
	release.currentVersion = func() string { return "v0.0.0" }
	release.download = func(_ context.Context, source string, destination string) error {
		switch path.Base(source) {
		case path.Base(archivePath):
			return copyFileContents(archivePath, destination)
		case "checksums.txt":
			return copyFileContents(checksumPath, destination)
		case "checksums.txt.sig":
			return copyFileContents(checksumPath+".sig", destination)
		default:
			return fmt.Errorf("unexpected source %q", source)
		}
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "--dir", commandDir, "--format", "json", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	var report selfUpdateReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("managed update JSON invalid: %v\n%s", err, stdout)
	}
	if !report.Updated || report.BinaryPath != binaryPath || report.LinkPath != linkPath {
		t.Fatalf("expected symlink-managed update report, got %#v", report)
	}
	updatedData, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(updatedData) != string(nextPayload) {
		t.Fatalf("expected updated binary payload, got %q", updatedData)
	}
	linkTarget, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if linkTarget != binaryPath {
		t.Fatalf("link target = %q, want %q", linkTarget, binaryPath)
	}
	metadata, err := readInstallMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.LinkPath != linkPath || metadata.BinaryPath != binaryPath {
		t.Fatalf("metadata lost managed paths: %#v", metadata)
	}
}

func TestSelfUpdateMetadataFailureIsNoopReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	currentPayload := []byte("current-threadpoint")
	mustWrite(t, binaryPath, string(currentPayload))
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	currentSHA, err := backup.FileSHA256(binaryPath)
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
		Archive:       detectSelfUpdateArchiveName("v0.1.0"),
		ArchiveSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		BinarySHA256:  currentSHA,
	}); err != nil {
		t.Fatal(err)
	}

	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		return releaseMetadata{}, errors.New("offline")
	}
	release.currentVersion = func() string {
		return "v0.0.0"
	}
	release.download = func(context.Context, string, string) error {
		t.Fatal("managed update should not download without fresh release metadata")
		return nil
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "--dir", commandDir, "--format", "json", "--no-cache")
	if err != nil {
		t.Fatal(err)
	}
	var report selfUpdateReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("managed update JSON invalid: %v\n%s", err, stdout)
	}
	if report.Updated || report.UpdateAvailable {
		t.Fatalf("expected metadata failure to be a no-op report: %#v", report)
	}
	if report.Code != "update_metadata_unavailable" || report.Message == "" {
		t.Fatalf("expected coded metadata outcome, got %#v", report)
	}
	currentData, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(currentData) != string(currentPayload) {
		t.Fatalf("expected binary unchanged after metadata failure, got %q", currentData)
	}
	metadata, err := readInstallMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.BinarySHA256 != currentSHA || metadata.Version != "v0.0.0" {
		t.Fatalf("expected metadata unchanged, got %#v", metadata)
	}
}

func TestSelfUpdateInteractiveConfirmationsRenderReleaseContextBeforeDeclining(t *testing.T) {
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if !interactiveInput(stdin) {
		t.Skip("platform null device is not reported as an interactive character device")
	}

	report := selfUpdateReport{
		LatestVersion: "v1.2.3",
		Summary:       "Important reliability fixes",
		LearnMoreURL:  "https://example.test/releases/v1.2.3",
	}
	for _, test := range []struct {
		name string
		run  func(*cli) error
		want []string
	}{
		{
			name: "stable",
			run:  func(app *cli) error { return confirmManagedUpdate(app, false, report) },
			want: []string{"Threadpoint release v1.2.3", "What's new: Important reliability fixes", "Learn more: https://example.test/releases/v1.2.3", "[y/N]"},
		},
		{
			name: "preview",
			run:  func(app *cli) error { return confirmPreviewUpdate(app, false, report) },
			want: []string{"Preview release v1.2.3 may be unstable", "What's new: Important reliability fixes", "Learn more: https://example.test/releases/v1.2.3", `Type "preview"`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			app := newCLI(&bytes.Buffer{}, &stderr, strings.NewReader(""))
			app.stdin = stdin
			if err := test.run(app); err == nil || !strings.Contains(err.Error(), "confirmation declined") {
				t.Fatalf("confirmation error = %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("confirmation prompt missing %q: %q", want, stderr.String())
				}
			}
		})
	}
}

func TestSelfUpdateEnvironmentFlagRecognizesEnabledValue(t *testing.T) {
	if !envFlagEnabledForTest(t, "THREADPOINT_TEST_SELF_UPDATE_FLAG") {
		t.Fatal("enabled flag not recognized")
	}
}

func TestSelfUpdatePersistsInstallMetadata(t *testing.T) {
	metadataPath := filepath.Join(t.TempDir(), "state", "install.json")
	if err := writeInstallMetadata(metadataPath, installMetadata{Version: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(metadataPath); err != nil || !strings.Contains(string(body), "v1.2.3") {
		t.Fatalf("install metadata = %q, err=%v", body, err)
	}
}

func TestSelfUpdateUpdatesBinaryAcrossDevices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	currentPayload := "current-threadpoint"
	nextPayload := "updated-threadpoint-xdev"
	writeThreadpointBundle(t, bundleRoot, currentPayload)
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	currentSHA, err := backup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	releaseDir := t.TempDir()
	archivePath, checksumPath, err := writeThreadpointReleaseAssets(t, releaseDir, nextPayload)
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
		Archive:       detectSelfUpdateArchiveName("v0.1.0"),
		ArchiveSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		BinarySHA256:  currentSHA,
	}); err != nil {
		t.Fatal(err)
	}

	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		return releaseMetadata{TagName: "v0.1.0", HTMLURL: "https://example.invalid/releases/tag/v0.1.0"}, nil
	}
	release.currentVersion = func() string { return "v0.0.0" }
	release.download = func(_ context.Context, source string, destination string) error {
		switch path.Base(source) {
		case path.Base(archivePath):
			return copyFileContents(archivePath, destination)
		case "checksums.txt":
			return copyFileContents(checksumPath, destination)
		case "checksums.txt.sig":
			return copyFileContents(checksumPath+".sig", destination)
		default:
			return fmt.Errorf("unexpected source %q", source)
		}
	}
	release.rename = func(string, string) error {
		return &os.LinkError{Op: "rename", Err: syscall.EXDEV}
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "--dir", commandDir, "--format", "json", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	var report selfUpdateReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("managed update JSON invalid: %v\n%s", err, stdout)
	}
	if !report.Updated {
		t.Fatalf("expected update across devices: %#v", report)
	}
	got, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != nextPayload {
		t.Fatalf("binary not updated: got %q want %q", got, nextPayload)
	}
}

func TestSelfUpdateWithoutInstallerMetadataHintsGoInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installDir := t.TempDir()
	_, _, err := runTestCLI(t, "update", "--dir", installDir, "--format", "json")
	if err == nil {
		t.Fatal("expected managed update to refuse without installer metadata")
	}
	if !strings.Contains(err.Error(), "go install github.com/threadgrid/threadpoint/cmd/threadpoint@latest") {
		t.Fatalf("expected go-install hint, got: %v", err)
	}
}

func TestManagedUpdateConfirmationRequiresYesWhenNoninteractive(t *testing.T) {
	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	app.nonInteractive = true
	report := selfUpdateReport{LatestVersion: "v1.2.3"}
	if err := confirmManagedUpdate(app, true, report); err != nil {
		t.Fatal(err)
	}
	if err := confirmPreviewUpdate(app, true, report); err != nil {
		t.Fatal(err)
	}
	if err := confirmManagedUpdate(app, false, report); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("managed confirmation error = %v", err)
	}
	if err := confirmPreviewUpdate(app, false, report); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("preview confirmation error = %v", err)
	}
}
