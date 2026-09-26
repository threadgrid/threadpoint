// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestSelfUpdateRejectsChecksumMismatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	commandDir := t.TempDir()
	bundleRoot := filepath.Join(home, ".threadpoint")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(commandDir, "threadpoint")
	metadataPath := installerMetadataPath(filepath.Join(home, ".threadpoint"), binaryPath)
	currentPayload := []byte("current-threadpoint")
	nextPayload := []byte("updated-threadpoint")
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
	releaseDir := t.TempDir()
	archivePath, checksumPath, err := writeThreadpointReleaseAssets(t, releaseDir, string(nextPayload))
	if err != nil {
		t.Fatal(err)
	}
	if err := overwriteChecksum(checksumPath, detectSelfUpdateArchiveName("v0.1.0"), "1111111111111111111111111111111111111111111111111111111111111111"); err != nil {
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

	_, err = runTestCLIWithRelease(t, release, "update", "--dir", commandDir, "--format", "json", "--yes")
	if err == nil {
		t.Fatalf("expected checksum failure")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	currentData, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(currentData) != string(currentPayload) {
		t.Fatalf("expected binary unchanged after failure, got %q", currentData)
	}
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("expected metadata to remain: %v", err)
	}
}

func TestSelfUpdateRequiresChecksumSignature(t *testing.T) {
	checksumsPath := filepath.Join(t.TempDir(), "checksums.txt")
	if err := os.WriteFile(checksumsPath, []byte("abc  threadpoint_0.1.0_linux_amd64.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := verifyReleaseChecksumsSignature(checksumsPath, filepath.Join(t.TempDir(), "missing.sig"))
	if err == nil {
		t.Fatal("expected missing signature to fail verification")
	}
}

func TestSelfUpdateRejectsChecksumSignatureMismatch(t *testing.T) {
	dir := t.TempDir()
	checksumsPath := filepath.Join(dir, "checksums.txt")
	signaturePath := filepath.Join(dir, "checksums.txt.sig")
	if err := os.WriteFile(checksumsPath, []byte("abc  threadpoint_0.1.0_linux_amd64.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envelope := `{"schema_version":"threadpoint.release_signature.v1","algorithm":"ed25519","key_id":"release-2026-07-09","signature":"` + base64.StdEncoding.EncodeToString(make([]byte, 64)) + `"}`
	if err := os.WriteFile(signaturePath, []byte(envelope), 0o600); err != nil {
		t.Fatal(err)
	}
	err := verifyReleaseChecksumsSignature(checksumsPath, signaturePath)
	if err == nil || !strings.Contains(err.Error(), "signature mismatch") {
		t.Fatalf("expected signature mismatch, got %v", err)
	}
}

func TestSelfUpdateVerifiesArchiveAndChecksumAttestations(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "threadpoint_0.1.0_linux_amd64.tar.gz")
	checksumsPath := filepath.Join(dir, "checksums.txt")
	var verified []string
	client := releaseClient{
		verifyAttestation: func(_ context.Context, repo string, tag string, path string) error {
			if repo != defaultUpdateRepo || tag != "v0.1.0" {
				t.Fatalf("unexpected attestation target repo=%q tag=%q", repo, tag)
			}
			verified = append(verified, filepath.Base(path))
			return nil
		},
	}

	if err := verifyReleaseAttestations(context.Background(), client, defaultUpdateRepo, "v0.1.0", archivePath, checksumsPath, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Join(verified, ",") != "threadpoint_0.1.0_linux_amd64.tar.gz,checksums.txt" {
		t.Fatalf("verified attestations = %#v", verified)
	}
}

func TestSelfUpdateAttestationSkipBypassesVerifier(t *testing.T) {
	client := releaseClient{
		verifyAttestation: func(context.Context, string, string, string) error {
			t.Fatal("attestation verifier should not run when skipped")
			return nil
		},
	}
	if err := verifyReleaseAttestations(context.Background(), client, defaultUpdateRepo, "v0.1.0", "archive", "checksums", true, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubArtifactAttestationIncludesCommandOutputOnFailure(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	argsLog := filepath.Join(dir, "args.log")
	t.Setenv("THREADPOINT_TEST_GH_ARGS", argsLog)
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nprintf '%s\\n' '---' \"$@\" >>\"$THREADPOINT_TEST_GH_ARGS\"\nprintf 'verified\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	archive := "/tmp/threadpoint_1.2.3_linux_amd64.tar.gz"
	if err := verifyGitHubArtifactAttestation(context.Background(), "owner/repo", "v1.2.3", archive); err != nil {
		t.Fatalf("successful archive attestation = %v", err)
	}
	if err := verifyGitHubArtifactAttestation(context.Background(), "owner/repo", "v1.2.3", "/tmp/checksums.txt"); err != nil {
		t.Fatalf("successful checksum attestation = %v", err)
	}
	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	invocations := strings.Split(strings.TrimPrefix(string(args), "---\n"), "---\n")
	if len(invocations) != 2 {
		t.Fatalf("attestation invocations = %q", args)
	}
	for _, expected := range []string{archive, "owner/repo/.github/workflows/release.yml", "refs/tags/v1.2.3"} {
		if !strings.Contains(invocations[0], expected+"\n") {
			t.Fatalf("archive attestation arguments %q do not contain %q", invocations[0], expected)
		}
	}
	for _, expected := range []string{"/tmp/checksums.txt", "owner/repo/.github/workflows/release-publish.yml", "refs/heads/main"} {
		if !strings.Contains(invocations[1], expected+"\n") {
			t.Fatalf("checksum attestation arguments %q do not contain %q", invocations[1], expected)
		}
	}
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nprintf 'attestation details' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyGitHubArtifactAttestation(context.Background(), "owner/repo", "v1.2.3", archive); err == nil || !strings.Contains(err.Error(), "attestation details") {
		t.Fatalf("failed attestation error = %v", err)
	}
}

func overwriteChecksum(path string, archive string, newSum string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := []string{}
	updated := false
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if parts[1] == archive {
			parts[0] = newSum
			updated = true
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	if !updated {
		lines = append(lines, fmt.Sprintf("%s %s", newSum, archive))
	}
	payload := strings.Join(lines, "\n")
	if payload != "" {
		payload += "\n"
	}
	return os.WriteFile(path, []byte(payload), 0o644)
}
