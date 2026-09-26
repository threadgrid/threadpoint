// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tpbackup "github.com/threadgrid/threadpoint/backup"
)

func writeInstallMetadata(path string, metadata installMetadata) error {
	return writeInstallFixtureJSON(path, metadata, 0o644)
}

func writeInstallFixtureJSON(path string, value any, mode os.FileMode) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), mode)
}

type installHelperFixture struct {
	productHome string
	commandPath string
	bundleRoot  string
	metadata    string
	backupRoot  string
	journalPath string
	journal     installTransactionJournal
}

func newInstallHelperFixture(t *testing.T) installHelperFixture {
	t.Helper()
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	bundleRoot := productHome
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	binaryPath := filepath.Join(bundleRoot, "bin", threadpointProductName)
	metadataPath := installerMetadataPath(productHome, binaryPath)
	backupRoot := filepath.Join(productHome, "installs", "transactions", ".bundle-helper")
	for _, entry := range threadpointBundleEntries {
		writeInstallHelperFile(t, filepath.Join(bundleRoot, filepath.FromSlash(entry)), []byte("prior:"+entry+"\n"))
		writeInstallHelperFile(t, filepath.Join(backupRoot, filepath.FromSlash(entry)), []byte("prior:"+entry+"\n"))
	}
	writeInstallHelperFile(t, metadataPath, []byte("prior metadata\n"))
	writeInstallHelperFile(t, filepath.Join(backupRoot, installTransactionMetadataBackup), []byte("prior metadata\n"))
	if err := os.Symlink(binaryPath, commandPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	targets := make(map[string]installJournalTarget, len(threadpointBundleEntries)+2)
	for _, entry := range threadpointBundleEntries {
		targets[installJournalBundleKey(entry)] = installJournalTarget{Prior: installHelperGeneration(t, filepath.Join(bundleRoot, filepath.FromSlash(entry)))}
	}
	targets["metadata"] = installJournalTarget{Prior: installHelperGeneration(t, metadataPath)}
	targets["link"] = installJournalTarget{Prior: installHelperGeneration(t, commandPath)}
	journalPath := installLifecycleJournalPath(productHome, commandPath)
	journal := installTransactionJournal{
		SchemaVersion: installTransactionSchemaVersion, Operation: updateOperationUpdate,
		CommandPath: commandPath, MetadataPath: metadataPath, BundleRoot: bundleRoot,
		BackupRoot: backupRoot, PreviousInstall: true, LinkExisted: true,
		CreatedAt: time.Now().UTC(), Targets: targets,
	}
	writeInstallHelperJournal(t, journalPath, journal)
	return installHelperFixture{
		productHome: productHome, commandPath: commandPath, bundleRoot: bundleRoot,
		metadata: metadataPath, backupRoot: backupRoot, journalPath: journalPath, journal: journal,
	}
}

func writeInstallHelperFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// #nosec G703 -- every caller constructs this test-only path beneath a
	// fresh t.TempDir-owned fixture.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeInstallHelperJournal(t *testing.T, path string, journal installTransactionJournal) []byte {
	t.Helper()
	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	writeInstallHelperFile(t, path, body)
	return body
}

func installHelperGeneration(t *testing.T, path string) installJournalGeneration {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	base := filepath.Base(path)
	info, err := root.Lstat(base)
	if err != nil {
		t.Fatal(err)
	}
	var identity installRemovalIdentity
	if info.Mode()&os.ModeSymlink != 0 {
		identity, err = snapshotSymlinkInstallRemoval(root, base)
	} else {
		identity, err = snapshotRegularInstallRemoval(root, base)
	}
	if err != nil {
		t.Fatal(err)
	}
	generation, err := recordedInstallGeneration(identity)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func installHelperDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func parseInstallHelperUpdateOutput(t *testing.T, output string) (string, string) {
	t.Helper()
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) != 2 || !strings.Contains(fields[0], ":") || len(fields[1]) != 64 {
		t.Fatalf("invalid helper update output %q", output)
	}
	return fields[0], fields[1]
}

func installHelperPathIdentity(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := installRemovalIdentityString(info)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func mustInstallHelperIdentity(t *testing.T, path string) installRemovalIdentity {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	identity, err := snapshotRegularInstallRemoval(root, filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func inspectInstallHelperEntry(t *testing.T, path string) (string, string, string) {
	t.Helper()
	var output bytes.Buffer
	if err := runInstallEntryInspect(&output, []string{path}); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(strings.TrimSpace(output.String()))
	if len(fields) != 3 {
		t.Fatalf("invalid install entry inspection %q", output.String())
	}
	return fields[0], fields[1], fields[2]
}

type rejectedInstallHelperWrite struct{}

type installTransactionFixture struct {
	productHome  string
	commandDir   string
	commandPath  string
	binaryPath   string
	metadataPath string
	metadata     installMetadata
}

func newInstallTransactionFixture(t *testing.T, withLink bool) installTransactionFixture {
	t.Helper()
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandDir := t.TempDir()
	commandPath := filepath.Join(commandDir, threadpointProductName)
	binaryPath := filepath.Join(productHome, "bin", threadpointProductName)
	metadataPath := installerMetadataPath(productHome, binaryPath)
	writeThreadpointBundle(t, productHome, "previous binary")
	if withLink {
		if err := os.Symlink(binaryPath, commandPath); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}
	binarySHA, err := tpbackup.FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata := installMetadata{
		SchemaVersion: installerMetadataSchemaVersion,
		Channel:       installerChannelScript,
		Repo:          defaultUpdateRepo,
		InstallDir:    commandDir,
		BinaryPath:    binaryPath,
		LinkPath:      commandPath,
		BundleRoot:    productHome,
		BundleEntries: append([]string(nil), threadpointBundleEntries...),
		Version:       "v1.0.0",
		ReleaseBase:   "https://example.test/v1.0.0",
		Archive:       "threadpoint_1.0.0_linux_amd64.tar.gz",
		ArchiveSHA256: strings.Repeat("1", 64),
		BinarySHA256:  binarySHA,
	}
	if err := writeInstallMetadata(metadataPath, metadata); err != nil {
		t.Fatal(err)
	}
	return installTransactionFixture{
		productHome: productHome, commandDir: commandDir, commandPath: commandPath,
		binaryPath: binaryPath, metadataPath: metadataPath, metadata: metadata,
	}
}

func testInstallJournalGeneration(t *testing.T, path string, symlink bool) installJournalGeneration {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var identity installRemovalIdentity
	if symlink {
		identity, err = snapshotSymlinkInstallRemoval(root, filepath.Base(path))
	} else {
		identity, err = snapshotRegularInstallRemoval(root, filepath.Base(path))
	}
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := recordedInstallGeneration(identity)
	if err != nil {
		t.Fatal(err)
	}
	return recorded
}

func buildThreadpointInstallLockHelper(t *testing.T) string {
	t.Helper()
	helper := filepath.Join(t.TempDir(), threadpointProductName)
	command := exec.CommandContext(t.Context(), "go", "build", "-o", helper, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build threadpoint install lock helper: %v\n%s", err, output)
	}
	return helper
}

func writeThreadpointBundle(t *testing.T, bundleRoot string, binaryPayload string) {
	t.Helper()
	for _, entry := range threadpointBundleEntries {
		path := filepath.Join(bundleRoot, filepath.FromSlash(entry))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "fixture\n"
		mode := os.FileMode(0o644)
		switch entry {
		case "bin/threadpoint":
			body = binaryPayload
			mode = 0o755
		case "README.md":
			body = "old readme"
		case "LICENSE":
			body = "license\n"
		case "NOTICE":
			body = "notice\n"
		case "scripts/install.sh", "scripts/uninstall.sh":
			body = "#!/bin/sh\n"
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
}
