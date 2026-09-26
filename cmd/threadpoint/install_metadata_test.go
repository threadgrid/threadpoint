// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstallMetadataSchemaMatchesShellWriter(t *testing.T) {
	body, err := json.Marshal(installMetadata{SchemaVersion: 1, LinkPath: "/x/threadpoint-link", BundleRoot: "/x", BundleEntries: threadpointBundleEntries})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"schema_version", "channel", "repo", "install_dir", "binary_path",
		"link_path", "bundle_root", "bundle_entries", "version", "release_base", "archive", "archive_sha256", "binary_sha256",
	} {
		if _, ok := fields[key]; !ok {
			t.Errorf("installMetadata JSON missing %q; keep the shell writer and Go readers in lockstep", key)
		}
	}

	valid := installMetadata{
		SchemaVersion: installerMetadataSchemaVersion,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		BinaryPath:    "/x/bin/threadpoint",
		LinkPath:      "/x/link/threadpoint",
		BundleRoot:    "/x",
		BundleEntries: threadpointBundleEntries,
		BinarySHA256:  "abc",
	}
	if err := verifyInstallOwnership(valid, "/x/bin/threadpoint"); err != nil {
		t.Fatalf("expected valid bundle metadata to pass ownership check: %v", err)
	}
	bad := valid
	bad.SchemaVersion = 2
	if err := verifyInstallOwnership(bad, "/x/bin/threadpoint"); err == nil {
		t.Error("unsupported schema_version must be rejected")
	}
	missingBundle := valid
	missingBundle.BundleRoot = ""
	if err := verifyInstallOwnership(missingBundle, "/x/bin/threadpoint"); err == nil || !strings.Contains(err.Error(), "bundle_root") {
		t.Fatalf("missing bundle_root must be rejected, got %v", err)
	}
	missingLink := valid
	missingLink.LinkPath = ""
	if err := verifyInstallOwnership(missingLink, "/x/bin/threadpoint"); err == nil || !strings.Contains(err.Error(), "link path") {
		t.Fatalf("missing link_path must be rejected, got %v", err)
	}
	badChannel := valid
	badChannel.Channel = "other"
	if err := verifyInstallOwnership(badChannel, "/x/bin/threadpoint"); err == nil {
		t.Error("channel != installer-script must be rejected")
	}
}

func TestDecodeInstallMetadataStrictRejectsDuplicateKnownKeys(t *testing.T) {
	body := []byte(`{"schema_version":1,"binary_sha256":"reviewed","binary_sha256":"replacement"}`)
	if _, err := decodeInstallMetadataStrict(body); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate installer metadata key error = %v", err)
	}
}

func TestInstallMetadataReadIsStrictBoundedAndNonblocking(t *testing.T) {
	fixture := newInstallTransactionFixture(t, true)
	body, err := os.ReadFile(fixture.metadataPath)
	if err != nil {
		t.Fatal(err)
	}

	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	object["retired_field"] = true
	unknown, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.metadataPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstallMetadata(fixture.metadataPath); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("unknown-field metadata error = %v", err)
	}

	if err := os.WriteFile(fixture.metadataPath, append(body, []byte("\n{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstallMetadata(fixture.metadataPath); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("trailing-value metadata error = %v", err)
	}

	if err := os.Remove(fixture.metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fixture.metadataPath, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readInstallMetadata(fixture.metadataPath)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO installer metadata was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO installer metadata blocked instead of failing closed")
	}
}

func TestInstallerOwnershipHelpersRejectUnsafeOrMismatchedMetadata(t *testing.T) {
	for _, entry := range []struct {
		value string
		want  bool
	}{
		{value: "bin/threadpoint", want: true},
		{value: "../threadpoint", want: false},
		{value: "/threadpoint", want: false},
		{value: `bin\\threadpoint`, want: false},
	} {
		if got := safeBundleEntry(entry.value); got != entry.want {
			t.Fatalf("safeBundleEntry(%q) = %v, want %v", entry.value, got, entry.want)
		}
	}
	if !sameStringSet([]string{"a", "b", "a"}, []string{"b", "a", "a"}) || sameStringSet([]string{"a"}, []string{"b"}) {
		t.Fatal("string-set comparison is incorrect")
	}

	bundleRoot := filepath.Join(t.TempDir(), "bundle")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	valid := installMetadata{
		SchemaVersion: installerMetadataSchemaVersion,
		Channel:       installerChannelScript,
		Repo:          "threadgrid/threadpoint",
		BinaryPath:    binaryPath,
		LinkPath:      filepath.Join(t.TempDir(), "threadpoint"),
		BinarySHA256:  "checksum",
		BundleRoot:    bundleRoot,
		BundleEntries: append([]string(nil), threadpointBundleEntries...),
	}
	if err := verifyInstallOwnership(valid, binaryPath); err != nil {
		t.Fatalf("valid installer metadata = %v", err)
	}
	invalid := valid
	invalid.BundleEntries = []string{"../escape"}
	if err := verifyInstallOwnership(invalid, binaryPath); err == nil {
		t.Fatal("unsafe bundle entry should fail ownership verification")
	}
	if _, exists, err := verifiedInstallerLinkPath(installMetadata{LinkPath: "relative"}, binaryPath); err == nil || exists {
		t.Fatalf("relative installer link = %v, %v", exists, err)
	}
}
