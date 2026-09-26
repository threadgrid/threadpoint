// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
)

func TestInstallTransactionTypedErrorsPreserveTheirCause(t *testing.T) {
	cause := errors.New("durability failure")
	wrapped := []interface {
		error
		Unwrap() error
	}{
		&installJournalPublicationSyncError{err: cause},
		&installJournalRewritePublishedError{err: cause},
		&installEntryRemovalCommittedError{err: cause},
	}
	for _, current := range wrapped {
		if current.Error() != cause.Error() || !errors.Is(current.Unwrap(), cause) || !errors.Is(current, cause) {
			t.Fatalf("typed install error did not preserve its cause: %T: %v", current, current)
		}
	}
}

func TestInstallTransactionMutationHelpersFailClosedWithoutPinnedState(t *testing.T) {
	if err := recordRecoveryInstallGeneration(nil, "target", installRemovalIdentity{}); err != nil {
		t.Fatalf("inactive recovery generation = %v", err)
	}
	if err := recordProducedInstallJournalGeneration(nil, "target", installJournalGeneration{}); err != nil {
		t.Fatalf("inactive produced generation = %v", err)
	}
	if err := recordRecoveredPriorInstallGeneration(nil, "target", installRemovalIdentity{}); err != nil {
		t.Fatalf("inactive recovered-prior generation = %v", err)
	}
	if _, err := createPinnedInstallTransactionBackup(nil); err == nil {
		t.Fatal("transaction backup accepted a nil lifecycle lock")
	}
	if _, err := openPinnedInstallTransactionBackup(nil, "/tmp/transactions/.bundle-test"); err == nil {
		t.Fatal("transaction backup open accepted a nil lifecycle lock")
	}
	if err := validatePinnedInstallChildDirectory(nil, "child", nil, nil); err == nil {
		t.Fatal("child validation accepted unpinned state")
	}
	if _, err := ensurePinnedInstallBackupParent(nil, "."); err == nil {
		t.Fatal("backup parent accepted a nil root")
	}
	if err := (*pinnedSelfUpdateWorkspace)(nil).Remove(); err != nil {
		t.Fatalf("nil workspace cleanup = %v", err)
	}
	if err := (&pinnedSelfUpdateWorkspace{}).Remove(); err == nil {
		t.Fatal("incomplete workspace cleanup was accepted")
	}
	if err := writeInstallBackupRootFile(nil, "entry", strings.NewReader("body")); err == nil {
		t.Fatal("backup write accepted a nil root")
	}
	remaining := 1
	if err := removePinnedInstallTransactionContentsBounded(nil, 0, &remaining); err == nil {
		t.Fatal("bounded cleanup accepted a nil root")
	}
	if err := removePinnedInstallTransactionContentsBounded(nil, maxInstallTransactionCleanupDepth+1, &remaining); err == nil {
		t.Fatal("bounded cleanup accepted excessive depth")
	}
	if err := rewriteActiveInstallTransactionJournal(nil, nil); err == nil {
		t.Fatal("active journal rewrite accepted missing identity")
	}
}

func TestInitialInstallBackupValidationRequiresAnEmptyPrivateDirectory(t *testing.T) {
	backupRoot := t.TempDir()
	journal := installTransactionJournal{BackupRoot: backupRoot}
	if err := validateInstallJournalBackupPrior(journal); err != nil {
		t.Fatalf("empty initial-install backup: %v", err)
	}
	writeInstallHelperFile(t, filepath.Join(backupRoot, "unexpected"), []byte("residue\n"))
	if err := validateInstallJournalBackupPrior(journal); err == nil {
		t.Fatal("non-empty initial-install backup accepted")
	}
	journal.Targets = map[string]installJournalTarget{"unknown": {Prior: installJournalGeneration{Kind: installGenerationKindAbsent}}}
	if err := validateInstallJournalPhase(journal, "normal"); err == nil {
		t.Fatal("unknown install journal phase target accepted")
	}
}

func TestPrepareInstallTransactionRejectsUnsealedInputs(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, fixture installTransactionFixture, lock *installLifecycleLock) error
	}{
		{
			name: "malformed metadata",
			prepare: func(t *testing.T, fixture installTransactionFixture, lock *installLifecycleLock) error {
				t.Helper()
				if err := os.WriteFile(fixture.metadataPath, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := prepareInstallTransaction(fixture.productHome, fixture.commandPath, updateOperationUpdate, fixture.metadataPath, fixture.metadata, lock)
				return err
			},
		},
		{
			name: "metadata argument changed",
			prepare: func(_ *testing.T, fixture installTransactionFixture, lock *installLifecycleLock) error {
				provided := fixture.metadata
				provided.Version = "v9.9.9"
				_, err := prepareInstallTransaction(fixture.productHome, fixture.commandPath, updateOperationUpdate, fixture.metadataPath, provided, lock)
				return err
			},
		},
		{
			name: "unsupported operation",
			prepare: func(_ *testing.T, fixture installTransactionFixture, lock *installLifecycleLock) error {
				_, err := prepareInstallTransaction(fixture.productHome, fixture.commandPath, "foreign", fixture.metadataPath, fixture.metadata, lock)
				return err
			},
		},
		{
			name: "mismatched command identity",
			prepare: func(t *testing.T, fixture installTransactionFixture, lock *installLifecycleLock) error {
				t.Helper()
				metadata := fixture.metadata
				metadata.LinkPath += "-foreign"
				if err := writeInstallMetadata(fixture.metadataPath, metadata); err != nil {
					t.Fatal(err)
				}
				_, err := prepareInstallTransaction(fixture.productHome, fixture.commandPath, updateOperationUpdate, fixture.metadataPath, metadata, lock)
				return err
			},
		},
		{
			name: "existing journal",
			prepare: func(t *testing.T, fixture installTransactionFixture, lock *installLifecycleLock) error {
				t.Helper()
				if err := os.WriteFile(installLifecycleJournalPath(fixture.productHome, fixture.commandPath), []byte("existing"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := prepareInstallTransaction(fixture.productHome, fixture.commandPath, updateOperationUpdate, fixture.metadataPath, fixture.metadata, lock)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, false)
			lock := fixture.acquire(t)
			if err := test.prepare(t, fixture, lock); err == nil {
				t.Fatal("unsealed transaction input was accepted")
			}
		})
	}
}

func TestResolveManagedInstallPathsHandlesMissingAndBrokenCommands(t *testing.T) {
	if _, err := resolveManagedInstallPaths(filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing command directory error = %v", err)
	}
	regular := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(regular, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveManagedInstallPaths(regular); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("regular command directory error = %v", err)
	}
	commandDir := t.TempDir()
	paths, err := resolveManagedInstallPaths(commandDir)
	if err != nil {
		t.Fatal(err)
	}
	if paths.BinaryPath != filepath.Join(commandDir, threadpointProductName) {
		t.Fatalf("missing command binary path = %s", paths.BinaryPath)
	}
	if err := os.Symlink(filepath.Join("..", "missing-binary"), filepath.Join(commandDir, threadpointProductName)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	paths, err = resolveManagedInstallPaths(commandDir)
	if err != nil {
		t.Fatal(err)
	}
	if paths.BinaryPath != filepath.Clean(filepath.Join(commandDir, "..", "missing-binary")) {
		t.Fatalf("broken relative command target = %s", paths.BinaryPath)
	}
}

func TestPinnedInstallBackupNamespaceRejectsForeignEntries(t *testing.T) {
	fixture := newInstallTransactionFixture(t, false)
	lock := fixture.acquire(t)
	if _, err := openPinnedInstallTransactionBackup(nil, filepath.Join(fixture.productHome, ".bundle-test")); err == nil {
		t.Fatal("backup open without lifecycle lock succeeded")
	}
	if _, err := openPinnedInstallTransactionBackup(lock, filepath.Join(fixture.productHome, "outside", ".bundle-test")); err == nil {
		t.Fatal("backup outside transactions namespace succeeded")
	}

	backup, err := createPinnedInstallTransactionBackup(lock)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := backup.path
	if err := backup.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(fixture.productHome, "installs", "transactions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openPinnedInstallTransactionBackup(lock, backupPath); err == nil || !strings.Contains(err.Error(), "owner-private") {
		t.Fatalf("world-readable transactions root error = %v", err)
	}
	if err := os.Chmod(filepath.Join(fixture.productHome, "installs", "transactions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backupPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openPinnedInstallTransactionBackup(lock, backupPath); err == nil || !strings.Contains(err.Error(), "owner-private") {
		t.Fatalf("world-readable backup root error = %v", err)
	}
	if err := os.Chmod(backupPath, 0o700); err != nil {
		t.Fatal(err)
	}
	reopened, err := openPinnedInstallTransactionBackup(lock, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.path != backupPath {
		t.Fatalf("reopened backup path = %s", reopened.path)
	}
	if _, err := openPinnedInstallTransactionBackup(lock, filepath.Join(filepath.Dir(backupPath), ".bundle-other")); err == nil || !strings.Contains(err.Error(), "different transaction backup") {
		t.Fatalf("different retained backup error = %v", err)
	}
	other := &pinnedInstallTransactionBackup{path: filepath.Join(filepath.Dir(backupPath), ".bundle-other")}
	if err := lock.retainTransactionBackup(other); err == nil {
		t.Fatal("second retained backup was accepted")
	}
	if err := lock.clearTransactionBackup(other, false); err == nil {
		t.Fatal("different retained backup was cleared")
	}
	if err := lock.clearTransactionBackup(reopened, true); err != nil {
		t.Fatal(err)
	}
	if err := removeInstallTransactionBackupChecked(lock, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Remove(); err == nil {
		t.Fatal("closed backup capability was accepted for removal")
	}
}

func TestPinnedBundleParentRejectsMountBoundary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux mount IDs provide the mount-boundary assertion")
	}
	if _, err := os.Stat("/proc"); err != nil {
		t.Skipf("procfs unavailable: %v", err)
	}
	root, err := os.OpenRoot("/")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	opened, err := openPinnedBundleParent(root, "proc")
	if err == nil {
		_ = opened.Close()
		t.Fatal("pinned bundle traversal crossed into a mounted filesystem")
	}
	if !strings.Contains(err.Error(), "mount boundary") {
		t.Fatalf("unexpected mount-boundary error: %v", err)
	}
}

func TestPrepareInstallTransactionRejectsReplacedTransactionsRootBeforeBackupWrites(t *testing.T) {
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
	defer func() { _ = lock.Release() }()

	transactionsPath := filepath.Join(productHome, "installs", "transactions")
	detachedPath := transactionsPath + "-detached"
	var hookErr error
	installTransactionAfterBackupRootOpen = func() {
		if err := os.Rename(transactionsPath, detachedPath); err != nil {
			hookErr = err
			return
		}
		if err := os.Mkdir(transactionsPath, 0o700); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(filepath.Join(transactionsPath, "sentinel"), []byte("replacement"), 0o600)
	}
	t.Cleanup(func() { installTransactionAfterBackupRootOpen = nil })
	transaction, err := prepareInstallTransaction(productHome, commandPath, updateOperationUpdate, metadataPath, metadata, lock)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || transaction != nil {
		t.Fatal("transaction preparation accepted a replaced transactions root")
	}
	if got, readErr := os.ReadFile(filepath.Join(transactionsPath, "sentinel")); readErr != nil || string(got) != "replacement" {
		t.Fatalf("replacement transactions root was modified: %q, err=%v", got, readErr)
	}
	if _, statErr := os.Lstat(installLifecycleJournalPath(productHome, commandPath)); !os.IsNotExist(statErr) {
		t.Fatalf("transaction journal was published after the namespace changed: %v", statErr)
	}
	entries, readErr := os.ReadDir(detachedPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("detached transaction backup was not cleaned exactly: %v", entries)
	}
}

func TestInstallerPathHelpersResolveAndVerifyManagedLinks(t *testing.T) {
	installDir := filepath.Join(t.TempDir(), "bin")
	bundleRoot := filepath.Join(t.TempDir(), "bundle")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(installDir, "threadpoint")
	if err := os.Symlink(binaryPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	paths, err := resolveManagedInstallPaths(installDir)
	if err != nil || paths.CommandPath != linkPath || paths.BinaryPath != binaryPath {
		t.Fatalf("managed paths = %#v, err=%v", paths, err)
	}
	verified, exists, err := verifiedInstallerLinkPath(installMetadata{LinkPath: linkPath}, binaryPath)
	if err != nil || !exists || verified != linkPath {
		t.Fatalf("verified link = %q, %v, %v", verified, exists, err)
	}
	if _, err := resolveManagedInstallPaths(filepath.Join(installDir, "missing")); err == nil {
		t.Fatal("missing command directory should fail")
	}
	filePath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(filePath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveManagedInstallPaths(filePath); err == nil {
		t.Fatal("file command directory should fail")
	}
}

func TestInstallBackupCreationRejectsUnsafeNamespace(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "public-mode"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, true)
			lock := fixture.acquire(t)
			namespace := filepath.Join(lock.lockParentPath, "transactions")
			retained := namespace + "-retained"
			if err := os.Rename(namespace, retained); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(namespace); _ = os.Rename(retained, namespace) })
			switch kind {
			case "file":
				if err := os.WriteFile(namespace, []byte("unrelated"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), namespace); err != nil {
					t.Fatal(err)
				}
			case "public-mode":
				if err := os.Mkdir(namespace, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(namespace, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			created, err := createPinnedInstallTransactionBackup(lock)
			if created != nil {
				_ = created.Close()
			}
			if err == nil {
				t.Fatal("unsafe backup namespace accepted")
			}
			if kind == "file" {
				if body, err := os.ReadFile(namespace); err != nil || string(body) != "unrelated" {
					t.Fatalf("unrelated namespace changed: %q %v", body, err)
				}
			}
		})
	}
}
