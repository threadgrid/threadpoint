// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestInstallRecoveryCleanupRemovesOnlyThePinnedAllowlistedBundle(t *testing.T) {
	transactions := filepath.Join(t.TempDir(), "transactions")
	cleanup := filepath.Join(transactions, ".bundle-cleanup-reviewed")
	for _, entry := range append(append([]string(nil), threadpointBundleEntries...), installTransactionMetadataBackup, installTransactionChecksumManifest) {
		writeInstallHelperFile(t, filepath.Join(cleanup, filepath.FromSlash(entry)), []byte("reviewed:"+entry+"\n"))
	}
	identity := installHelperPathIdentity(t, cleanup)
	if err := runInstallRecoveryCleanup([]string{cleanup, identity}); err != nil {
		t.Fatalf("rooted recovery cleanup: %v", err)
	}
	if _, err := os.Lstat(cleanup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery cleanup root remains: %v", err)
	}

	unexpected := filepath.Join(transactions, ".bundle-cleanup-unexpected")
	writeInstallHelperFile(t, filepath.Join(unexpected, "foreign.txt"), []byte("preserve\n"))
	identity = installHelperPathIdentity(t, unexpected)
	if err := runInstallRecoveryCleanup([]string{unexpected, identity}); err == nil {
		t.Fatal("recovery cleanup accepted a child outside its fixed allowlist")
	}
	if body, err := os.ReadFile(filepath.Join(unexpected, "foreign.txt")); err != nil || string(body) != "preserve\n" {
		t.Fatalf("unexpected recovery child = %q, %v", body, err)
	}
}

func TestInstallRecoveryCleanupRejectsCleanupRootReplacementAfterPin(t *testing.T) {
	transactions := filepath.Join(t.TempDir(), "transactions")
	cleanup := filepath.Join(transactions, ".bundle-cleanup-reviewed")
	reviewed := cleanup + ".reviewed"
	outside := filepath.Join(transactions, "outside-sentinel")
	writeInstallHelperFile(t, filepath.Join(cleanup, "README.md"), []byte("reviewed backup child\n"))
	writeInstallHelperFile(t, outside, []byte("outside\n"))
	identity := installHelperPathIdentity(t, cleanup)
	inheritedInstallHelperAfterRecoveryCleanupRootOpen = func(path string) {
		if path != cleanup {
			t.Fatalf("cleanup hook path = %q", path)
		}
		if err := os.Rename(cleanup, reviewed); err != nil {
			t.Fatal(err)
		}
		writeInstallHelperFile(t, filepath.Join(cleanup, "README.md"), []byte("foreign replacement child\n"))
	}
	t.Cleanup(func() { inheritedInstallHelperAfterRecoveryCleanupRootOpen = nil })
	if err := runInstallRecoveryCleanup([]string{cleanup, identity}); err == nil {
		t.Fatal("replacement cleanup root was accepted after pinning")
	}
	for path, want := range map[string]string{
		filepath.Join(cleanup, "README.md"):  "foreign replacement child\n",
		filepath.Join(reviewed, "README.md"): "reviewed backup child\n",
		outside:                              "outside\n",
	} {
		if body, err := os.ReadFile(path); err != nil || string(body) != want {
			t.Fatalf("preserved cleanup sentinel %s = %q, %v", path, body, err)
		}
	}
}

func TestInstallRecoveryCleanupRejectsNestedDirectoryReplacementAfterPin(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		entry string
	}{
		{name: "bin", entry: "threadpoint"},
		{name: "scripts", entry: "install.sh"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			transactions := filepath.Join(t.TempDir(), "transactions")
			cleanup := filepath.Join(transactions, ".bundle-cleanup-reviewed")
			nested := filepath.Join(cleanup, fixture.name)
			reviewed := nested + ".reviewed"
			outside := filepath.Join(transactions, "outside-sentinel")
			writeInstallHelperFile(t, filepath.Join(nested, fixture.entry), []byte("reviewed nested child\n"))
			writeInstallHelperFile(t, outside, []byte("outside\n"))
			identity := installHelperPathIdentity(t, cleanup)
			inheritedInstallHelperAfterRecoveryCleanupChildOpen = func(path string) {
				if path != nested {
					return
				}
				if err := os.Rename(nested, reviewed); err != nil {
					t.Fatal(err)
				}
				writeInstallHelperFile(t, filepath.Join(nested, fixture.entry), []byte("foreign nested child\n"))
			}
			t.Cleanup(func() { inheritedInstallHelperAfterRecoveryCleanupChildOpen = nil })
			if err := runInstallRecoveryCleanup([]string{cleanup, identity}); err == nil {
				t.Fatal("replacement nested cleanup directory was accepted after pinning")
			}
			for path, want := range map[string]string{
				filepath.Join(nested, fixture.entry):   "foreign nested child\n",
				filepath.Join(reviewed, fixture.entry): "reviewed nested child\n",
				outside:                                "outside\n",
			} {
				if body, err := os.ReadFile(path); err != nil || string(body) != want {
					t.Fatalf("preserved nested sentinel %s = %q, %v", path, body, err)
				}
			}
			if _, err := os.Lstat(cleanup); err != nil {
				t.Fatalf("fail-closed cleanup residue was removed: %v", err)
			}
		})
	}
}

func TestInstallRecoveryCleanupPreservesFileReplacementAfterSnapshot(t *testing.T) {
	transactions := filepath.Join(t.TempDir(), "transactions")
	cleanup := filepath.Join(transactions, ".bundle-cleanup-reviewed")
	readme := filepath.Join(cleanup, "README.md")
	writeInstallHelperFile(t, readme, []byte("reviewed backup child\n"))
	identity := installHelperPathIdentity(t, cleanup)

	previousHook := installRemovalBeforeQuarantine
	hookCalled := false
	installRemovalBeforeQuarantine = func(root *os.Root, base string) {
		if base != "README.md" {
			return
		}
		hookCalled = true
		installRemovalBeforeQuarantine = nil
		if err := root.Remove(base); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(base, []byte("foreign replacement child\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { installRemovalBeforeQuarantine = previousHook })

	if err := runInstallRecoveryCleanup([]string{cleanup, identity}); err == nil {
		t.Fatal("recovery cleanup accepted a post-snapshot file replacement")
	}
	if !hookCalled {
		t.Fatal("recovery cleanup did not reach identity-safe quarantine")
	}
	if body, err := os.ReadFile(readme); err != nil || string(body) != "foreign replacement child\n" {
		t.Fatalf("replacement recovery child = %q, %v", body, err)
	}
}

func TestInstallRecoveryCleanupRejectsMountIdentityMismatch(t *testing.T) {
	transactions := filepath.Join(t.TempDir(), "transactions")
	cleanup := filepath.Join(transactions, ".bundle-cleanup-reviewed")
	writeInstallHelperFile(t, filepath.Join(cleanup, "README.md"), []byte("reviewed backup child\n"))

	transactionsRoot, transactionsPath, cleanupBase, err := openPinnedInstallHelperParent(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	defer transactionsRoot.Close()
	cleanupInfo, err := transactionsRoot.Lstat(cleanupBase)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRoot, err := transactionsRoot.OpenRoot(cleanupBase)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupRoot.Close()
	mount, err := installRootMountIdentity(cleanupRoot)
	if err != nil {
		t.Skipf("mount identity is unavailable: %v", err)
	}
	if err := validatePinnedInstallRecoveryCleanupRoot(
		transactionsRoot,
		transactionsPath,
		cleanupBase,
		cleanupRoot,
		cleanupInfo,
		mount^1,
	); err == nil || !strings.Contains(err.Error(), "mount boundary") {
		t.Fatalf("mount identity mismatch error = %v", err)
	}
}

func TestInstallJournalRecoveryRequiresBackupAndAcceptsPriorGeneration(t *testing.T) {
	if err := validateInstallJournalBackupPrior(installTransactionJournal{BackupRoot: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing recovery backup accepted")
	}
	fixture := newInstallHelperFixture(t)
	key := installJournalBundleKey(threadpointBundleEntries[0])
	target := fixture.journal.Targets[key]
	target.Recovery = target.Prior
	fixture.journal.Targets[key] = target
	if err := validateInstallJournalPhase(fixture.journal, "recovery"); err != nil {
		t.Fatalf("matching recovery phase: %v", err)
	}
}

func TestInstallTransactionRecoveryRejectsTamperedBackup(t *testing.T) {
	tests := []struct {
		name            string
		tamper          func(t *testing.T, fixture installTransactionFixture, backup *pinnedInstallTransactionBackup)
		wantErrContains string
	}{
		{
			name: "changed payload",
			tamper: func(t *testing.T, _ installTransactionFixture, backup *pinnedInstallTransactionBackup) {
				t.Helper()
				if err := backup.root.WriteFile("README.md", []byte("tampered\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErrContains: "checksum mismatch for README.md",
		},
		{
			name: "malformed checksum manifest",
			tamper: func(t *testing.T, _ installTransactionFixture, backup *pinnedInstallTransactionBackup) {
				t.Helper()
				if err := backup.root.WriteFile(installTransactionChecksumManifest, []byte("not-a-manifest\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErrContains: "checksum manifest is malformed",
		},
		{
			name: "incomplete checksum manifest",
			tamper: func(t *testing.T, _ installTransactionFixture, backup *pinnedInstallTransactionBackup) {
				t.Helper()
				digest, err := installRootFileSHA256(backup.root, "README.md")
				if err != nil {
					t.Fatal(err)
				}
				if err := backup.root.WriteFile(installTransactionChecksumManifest, []byte(digest+"  README.md\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErrContains: "checksum manifest is incomplete",
		},
		{
			name: "different managed identity",
			tamper: func(t *testing.T, fixture installTransactionFixture, backup *pinnedInstallTransactionBackup) {
				t.Helper()
				metadata := fixture.metadata
				metadata.LinkPath = filepath.Join(fixture.commandDir, "other-command")
				body, err := json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if err := backup.root.WriteFile(installTransactionMetadataBackup, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := backup.root.Remove(installTransactionChecksumManifest); err != nil {
					t.Fatal(err)
				}
				if err := writeInstallTransactionChecksums(backup.root); err != nil {
					t.Fatal(err)
				}
			},
			wantErrContains: "different managed identity",
		},
		{
			name: "malformed backup metadata",
			tamper: func(t *testing.T, _ installTransactionFixture, backup *pinnedInstallTransactionBackup) {
				t.Helper()
				if err := backup.root.WriteFile(installTransactionMetadataBackup, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := backup.root.Remove(installTransactionChecksumManifest); err != nil {
					t.Fatal(err)
				}
				if err := writeInstallTransactionChecksums(backup.root); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unowned backup metadata",
			tamper: func(t *testing.T, fixture installTransactionFixture, backup *pinnedInstallTransactionBackup) {
				t.Helper()
				metadata := fixture.metadata
				metadata.Repo = "foreign/repository"
				body, err := json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if err := backup.root.WriteFile(installTransactionMetadataBackup, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := backup.root.Remove(installTransactionChecksumManifest); err != nil {
					t.Fatal(err)
				}
				if err := writeInstallTransactionChecksums(backup.root); err != nil {
					t.Fatal(err)
				}
			},
			wantErrContains: "repo mismatch",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, true)
			lock := fixture.acquire(t)
			transaction := prepareFixtureTransaction(t, fixture, lock)
			backup := lock.transactionBackup
			if backup == nil {
				t.Fatal("prepared transaction did not retain its pinned backup")
			}
			test.tamper(t, fixture, backup)
			if err := os.WriteFile(fixture.binaryPath, []byte("partial replacement"), 0o755); err != nil {
				t.Fatal(err)
			}

			err := recoverInstallTransaction(fixture.productHome, fixture.commandPath, lock)
			if err == nil {
				t.Fatal("recovery succeeded with a tampered backup")
			}
			if test.wantErrContains != "" && !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("recovery error = %v, want error containing %q", err, test.wantErrContains)
			}
			if body, err := os.ReadFile(fixture.binaryPath); err != nil || string(body) != "partial replacement" {
				t.Fatalf("failed recovery changed live binary: %q, err=%v", body, err)
			}
			if _, err := os.Lstat(transaction.journalPath); err != nil {
				t.Fatalf("failed recovery removed journal: %v", err)
			}
			if info, err := os.Lstat(transaction.journal.BackupRoot); err != nil || !info.IsDir() {
				t.Fatalf("failed recovery removed backup: info=%v err=%v", info, err)
			}
		})
	}
}

func TestInstallTransactionRecoveryWithoutPriorLinkRestoresBundleAndKeepsLinkAbsent(t *testing.T) {
	fixture := newInstallTransactionFixture(t, false)
	lock := fixture.acquire(t)
	transaction := prepareFixtureTransaction(t, fixture, lock)
	if transaction.journal.LinkExisted {
		t.Fatal("prepared transaction recorded an absent command link as present")
	}
	if err := os.WriteFile(fixture.binaryPath, []byte("partial replacement"), 0o755); err != nil {
		t.Fatal(err)
	}
	binRoot, err := openPinnedBundleParent(lock.bundleRoot, "bin")
	if err != nil {
		t.Fatal(err)
	}
	produced, err := snapshotRegularInstallRemoval(binRoot, threadpointProductName)
	closeErr := binRoot.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if err := recordProducedInstallGeneration(lock, installJournalBundleKey("bin/threadpoint"), produced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fixture.productHome, "wrong"), fixture.commandPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	// Recovery must fail closed while a foreign link occupies the command name.
	if err := recoverInstallTransaction(fixture.productHome, fixture.commandPath, lock); err == nil {
		t.Fatal("recovery accepted a foreign command link")
	}
	if err := os.Remove(fixture.commandPath); err != nil {
		t.Fatal(err)
	}
	if err := recoverInstallTransaction(fixture.productHome, fixture.commandPath, lock); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(fixture.binaryPath); err != nil || string(body) != "previous binary" {
		t.Fatalf("recovered binary = %q, err=%v", body, err)
	}
	if _, err := os.Lstat(fixture.commandPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery recreated a deliberately absent command link: %v", err)
	}
	if _, err := os.Lstat(transaction.journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed recovery retained journal: %v", err)
	}
}

func TestInstallBackupCleanupRejectsForeignFilesystemShapes(t *testing.T) {
	fixture := newInstallTransactionFixture(t, false)
	lock := fixture.acquire(t)
	if err := removeInstallTransactionBackupChecked(lock, filepath.Join(fixture.productHome, "outside", ".bundle-entry")); err == nil {
		t.Fatal("cleanup accepted backup outside transactions root")
	}
	missing := filepath.Join(fixture.productHome, "installs", "transactions", ".bundle-missing")
	if err := removeInstallTransactionBackupChecked(lock, missing); err != nil {
		t.Fatalf("missing backup cleanup: %v", err)
	}
	transactions := filepath.Join(fixture.productHome, "installs", "transactions")
	filePath := filepath.Join(transactions, ".bundle-file")
	if err := os.WriteFile(filePath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeInstallTransactionBackupChecked(lock, filePath); err == nil || !strings.Contains(err.Error(), "changed before cleanup") {
		t.Fatalf("regular backup entry cleanup error = %v", err)
	}
	linkPath := filepath.Join(transactions, ".bundle-link")
	if err := os.Symlink(filePath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := removeInstallTransactionBackupChecked(lock, linkPath); err == nil || !strings.Contains(err.Error(), "changed before cleanup") {
		t.Fatalf("symlink backup entry cleanup error = %v", err)
	}
}

func TestUninstallRecoveryPreservesAlreadyMissingManagedBinary(t *testing.T) {
	fixture := newInstallTransactionFixture(t, true)
	if err := os.Remove(fixture.binaryPath); err != nil {
		t.Fatal(err)
	}

	first, err := acquireInstallLifecycleLock(context.Background(), fixture.productHome, fixture.commandPath)
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := prepareInstallTransaction(
		fixture.productHome, fixture.commandPath, updateOperationUninstall,
		fixture.metadataPath, fixture.metadata, first,
	)
	if err != nil {
		t.Fatal(err)
	}
	binaryTarget := transaction.journal.Targets[installJournalBundleKey(installManagedBinaryEntry)]
	if binaryTarget.Prior.Kind != installGenerationKindAbsent {
		t.Fatalf("missing binary prior = %#v, want absent", binaryTarget.Prior)
	}
	readmePath := filepath.Join(fixture.productHome, "README.md")
	if removed, err := removeBundleEntryChecked("README.md", first); err != nil || !removed {
		t.Fatalf("remove README.md = %v, err=%v", removed, err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	second, err := acquireInstallLifecycleLock(context.Background(), fixture.productHome, fixture.commandPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Release(); err != nil {
			t.Errorf("release recovered lifecycle lock: %v", err)
		}
	}()
	if err := recoverInstallTransaction(fixture.productHome, fixture.commandPath, second); err != nil {
		t.Fatalf("recover missing-binary uninstall: %v", err)
	}
	if got, err := os.ReadFile(readmePath); err != nil || string(got) != "old readme" {
		t.Fatalf("recovered README.md = %q, err=%v", got, err)
	}
	if _, err := os.Lstat(fixture.binaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery recreated already-missing binary: %v", err)
	}
}

func TestInstallTransactionRecoversInterruptedMixedBundle(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandDir := t.TempDir()
	commandPath := filepath.Join(commandDir, threadpointProductName)
	bundleRoot := productHome
	binaryPath := filepath.Join(bundleRoot, "bin", threadpointProductName)
	metadataPath := installerMetadataPath(productHome, binaryPath)
	writeThreadpointBundle(t, bundleRoot, "previous binary")
	if err := os.Symlink(binaryPath, commandPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	binarySHA, err := backup.FileSHA256(binaryPath)
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
		BundleRoot:    bundleRoot,
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
	previous := map[string][]byte{}
	for _, entry := range threadpointBundleEntries {
		body, err := os.ReadFile(filepath.Join(bundleRoot, filepath.FromSlash(entry)))
		if err != nil {
			t.Fatal(err)
		}
		previous[entry] = body
	}

	first, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := prepareInstallTransaction(productHome, commandPath, updateOperationUninstall, metadataPath, metadata, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordProducedAbsentInstallGeneration(first, installJournalBundleKey("bin/threadpoint")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(binaryPath); err != nil {
		t.Fatal(err)
	}
	if err := recordProducedAbsentInstallGeneration(first, installJournalBundleKey("README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundleRoot, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := recordProducedAbsentInstallGeneration(first, "metadata"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := recordProducedAbsentInstallGeneration(first, "link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(commandPath); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	second, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Release() }()
	if err := recoverInstallTransaction(productHome, commandPath, second); err != nil {
		t.Fatal(err)
	}
	for _, entry := range threadpointBundleEntries {
		body, err := os.ReadFile(filepath.Join(bundleRoot, filepath.FromSlash(entry)))
		if err != nil || !bytes.Equal(body, previous[entry]) {
			t.Fatalf("recovered %s = %q, err=%v, want %q", entry, body, err, previous[entry])
		}
	}
	recovered, err := readInstallMetadata(metadataPath)
	if err != nil || recovered.Version != metadata.Version || recovered.BinarySHA256 != metadata.BinarySHA256 {
		t.Fatalf("recovered metadata = %#v, err=%v", recovered, err)
	}
	if target, err := filepath.EvalSymlinks(commandPath); err != nil || target != binaryPath {
		t.Fatalf("recovered command link target = %q, err=%v", target, err)
	}
	if _, err := os.Lstat(transaction.journalPath); !os.IsNotExist(err) {
		t.Fatalf("transaction journal remains after recovery: %v", err)
	}
	if _, err := os.Lstat(transaction.journal.BackupRoot); !os.IsNotExist(err) {
		t.Fatalf("transaction backup remains after recovery: %v", err)
	}
}

func TestShellRecoversGoInstallTransaction(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandDir := t.TempDir()
	commandPath := filepath.Join(commandDir, threadpointProductName)
	binaryPath := filepath.Join(productHome, "bin", threadpointProductName)
	metadataPath := installerMetadataPath(productHome, binaryPath)
	writeThreadpointBundle(t, productHome, "previous binary")
	if err := os.Symlink(binaryPath, commandPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	binarySHA, err := backup.FileSHA256(binaryPath)
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
	lock, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := prepareInstallTransaction(productHome, commandPath, updateOperationUpdate, metadataPath, metadata, lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("partial new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := recordProducedInstallJournalGeneration(lock, installJournalBundleKey("bin/threadpoint"), testInstallJournalGeneration(t, binaryPath, false)); err != nil {
		t.Fatal(err)
	}
	if err := recordProducedAbsentInstallGeneration(lock, installJournalBundleKey("README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(productHome, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := recordProducedAbsentInstallGeneration(lock, "metadata"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := recordProducedAbsentInstallGeneration(lock, "link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(commandPath); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join("..", "..", "scripts", "install.sh")
	helperPath := buildThreadpointInstallLockHelper(t)
	command := exec.CommandContext(context.Background(), "sh", "-c", `
THREADPOINT_INSTALL_SH_TEST_MODE=1
export THREADPOINT_INSTALL_SH_TEST_MODE
. "$1"
acquire_install_lifecycle_lock "$2" "$3" "$4"
recover_install_transaction "$2" "$3"
release_install_lifecycle_lock
`, "sh", scriptPath, productHome, commandPath, helperPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("shell could not recover Go transaction: %v\n%s", err, output)
	}
	if got, err := os.ReadFile(binaryPath); err != nil || string(got) != "previous binary" {
		t.Fatalf("shell-recovered binary = %q, err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(productHome, "README.md")); err != nil || string(got) != "old readme" {
		t.Fatalf("shell-recovered README = %q, err=%v", got, err)
	}
	if target, err := filepath.EvalSymlinks(commandPath); err != nil || target != binaryPath {
		t.Fatalf("shell-recovered link target = %q, err=%v", target, err)
	}
	if _, err := readInstallMetadata(metadataPath); err != nil {
		t.Fatalf("shell did not restore metadata: %v", err)
	}
	if _, err := os.Lstat(transaction.journalPath); !os.IsNotExist(err) {
		t.Fatalf("shell retained Go transaction journal: %v", err)
	}
}

func TestGoRecoversShellInstallTransaction(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandDir := t.TempDir()
	commandPath := filepath.Join(commandDir, threadpointProductName)
	binaryPath := filepath.Join(productHome, "bin", threadpointProductName)
	metadataPath := installerMetadataPath(productHome, binaryPath)
	if err := os.MkdirAll(productHome, 0o700); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join("..", "..", "scripts", "install.sh")
	helperPath := buildThreadpointInstallLockHelper(t)
	command := exec.CommandContext(context.Background(), "sh", "-c", `
THREADPOINT_INSTALL_SH_TEST_MODE=1
export THREADPOINT_INSTALL_SH_TEST_MODE
. "$1"
acquire_install_lifecycle_lock "$2" "$3" "$5"
prepare_install_transaction "$2" "$3" "$2" "$4" install
mkdir -p "${2}/bin" "${4%/*}"
printf '%s\n' partial >"${2}/bin/.threadpoint.partial"
publish_install_target_generation 'bundle:bin/threadpoint' "${2}/bin/.threadpoint.partial" "${2}/bin/threadpoint" normal regular
printf '%s\n' partial >"${2}/.README.partial"
publish_install_target_generation 'bundle:README.md' "${2}/.README.partial" "${2}/README.md" normal regular
printf '%s\n' partial >"${4}.partial"
publish_install_target_generation metadata "${4}.partial" "$4" normal regular
ln -s "${2}/bin/threadpoint" "${3}.partial"
publish_install_target_generation link "${3}.partial" "$3" normal symlink
release_install_lifecycle_lock
`, "sh", scriptPath, productHome, commandPath, metadataPath, helperPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("shell could not prepare interrupted install transaction: %v\n%s", err, output)
	}

	journalPath := installLifecycleJournalPath(productHome, commandPath)
	body, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	var journal installTransactionJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.PreviousInstall || !strings.HasPrefix(filepath.Base(journal.BackupRoot), ".bundle-") {
		t.Fatalf("shell journal is not Go-compatible: %+v", journal)
	}

	lock, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	if err := recoverInstallTransaction(productHome, commandPath, lock); err != nil {
		t.Fatalf("Go could not recover shell transaction: %v", err)
	}
	for _, path := range []string{binaryPath, filepath.Join(productHome, "README.md"), metadataPath, commandPath, journalPath, journal.BackupRoot} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("shell-created partial install remains after Go recovery: %s: %v", path, err)
		}
	}
}

func TestPinnedSelfUpdateWorkspaceCleanupPreservesReplacement(t *testing.T) {
	parent := t.TempDir()
	removed, err := createPinnedSelfUpdateWorkspace(parent)
	if err != nil {
		t.Fatal(err)
	}
	removedPath := removed.path
	if err := removed.root.WriteFile("owned", []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removed.Remove(); err != nil {
		t.Fatalf("remove retained workspace: %v", err)
	}
	if _, err := os.Lstat(removedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retained workspace still exists: %v", err)
	}

	workspace, err := createPinnedSelfUpdateWorkspace(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.root.WriteFile("owned", []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-workspace")
	if err := os.Rename(workspace.path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace.path, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(workspace.path, "sentinel")
	if err := os.WriteFile(sentinel, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Remove(); err == nil {
		t.Fatal("workspace cleanup accepted a last-moment replacement")
	}
	if body, err := os.ReadFile(sentinel); err != nil || string(body) != "replacement" {
		t.Fatalf("replacement workspace changed: %q, err=%v", body, err)
	}
}

func TestInstallTemporaryCleanupPreservesLastMomentReplacement(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const name = ".threadpoint-owned.tmp"
	if err := root.WriteFile(name, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := root.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}

	hookCalled := false
	previousHook := installRemovalBeforeQuarantine
	installRemovalBeforeQuarantine = func(_ *os.Root, base string) {
		if base != name {
			return
		}
		hookCalled = true
		if err := os.Remove(filepath.Join(rootPath, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { installRemovalBeforeQuarantine = previousHook })

	if err := removeInstallTemporaryIfSame(root, name, expected); err == nil {
		t.Fatal("temporary cleanup accepted a last-moment replacement")
	}
	if !hookCalled {
		t.Fatal("temporary cleanup did not use identity-safe quarantine")
	}
	body, err := os.ReadFile(filepath.Join(rootPath, name))
	if err != nil || string(body) != "foreign" {
		t.Fatalf("temporary cleanup changed replacement: body=%q err=%v", body, err)
	}
}

func TestInstallerLinkRecoveryPreservesUnownedCommandEntries(t *testing.T) {
	for _, change := range []string{"outside-path", "regular-file", "foreign-symlink", "unowned-link", "missing-unexpected", "missing-journal-owner", "closed-root"} {
		t.Run(change, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, true)
			lock := fixture.acquire(t)
			transaction := prepareFixtureTransaction(t, fixture, lock)
			metadata := fixture.metadata
			switch change {
			case "outside-path":
				metadata.LinkPath = filepath.Join(t.TempDir(), "command")
			case "regular-file":
				if err := os.Remove(fixture.commandPath); err != nil {
					t.Fatal(err)
				}
				writeInstallHelperFile(t, fixture.commandPath, []byte("concurrent"))
			case "foreign-symlink", "unowned-link":
				if err := os.Rename(fixture.commandPath, fixture.commandPath+".prior"); err != nil {
					t.Fatal(err)
				}
				target := fixture.binaryPath
				if change == "foreign-symlink" {
					target += ".foreign"
				}
				if err := os.Symlink(target, fixture.commandPath); err != nil {
					t.Fatal(err)
				}
			case "missing-unexpected":
				if err := os.Remove(fixture.commandPath); err != nil {
					t.Fatal(err)
				}
			case "missing-journal-owner":
				delete(transaction.journal.Targets, "link")
			case "closed-root":
				original := lock.commandRoot
				closed, err := os.OpenRoot(filepath.Dir(fixture.commandPath))
				if err != nil {
					t.Fatal(err)
				}
				_ = closed.Close()
				lock.commandRoot = closed
				defer func() { lock.commandRoot = original }()
			}
			before, beforeErr := os.Lstat(fixture.commandPath)
			if err := recoverInstallerLink(metadata, lock); err == nil {
				t.Fatal("unowned recovery succeeded")
			}
			after, afterErr := os.Lstat(fixture.commandPath)
			if os.IsNotExist(beforeErr) {
				if !os.IsNotExist(afterErr) {
					t.Fatalf("missing command was recreated: %v", afterErr)
				}
			} else if beforeErr != nil || afterErr != nil || !os.SameFile(before, after) {
				t.Fatalf("recovery changed command identity: %v %v", beforeErr, afterErr)
			}
			if change == "regular-file" {
				if body, err := os.ReadFile(fixture.commandPath); err != nil || string(body) != "concurrent" {
					t.Fatalf("concurrent bytes changed: %q %v", body, err)
				}
			}
			if change == "outside-path" {
				if _, err := os.Lstat(metadata.LinkPath); !os.IsNotExist(err) {
					t.Fatalf("external link created: %v", err)
				}
			}
		})
	}
}
