// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/safefs"
)

func TestInstallJournalHelperStrictValidationAndPrepublicationIdentity(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstallHelperJournal(fixture.journalPath, installHelperDigest(body), fixture.journalPath); err != nil {
		t.Fatalf("valid canonical journal: %v", err)
	}
	var inspect bytes.Buffer
	if err := runInstallJournalInspect(&inspect, []string{fixture.journalPath, fixture.journalPath}); err != nil {
		t.Fatalf("secure journal inspection: %v", err)
	}
	fields := strings.Fields(strings.TrimSpace(inspect.String()))
	if len(fields) != 2 || fields[1] != installHelperDigest(body) || !strings.Contains(fields[0], ":") {
		t.Fatalf("journal inspection output = %q", inspect.String())
	}
	if err := runInstallJournalHelper(io.Discard, []string{fixture.journalPath, "", "validate"}); err == nil {
		t.Fatal("digest-bound action accepted an empty digest")
	}
	tempPath := filepath.Join(filepath.Dir(fixture.journalPath), ".journal-reviewed.tmp")
	writeInstallHelperFile(t, tempPath, body)
	if err := runInstallJournalHelper(io.Discard, []string{tempPath, installHelperDigest(body), "validate-as", fixture.journalPath}); err != nil {
		t.Fatalf("valid prepublication journal: %v", err)
	}
	if err := runInstallJournalHelper(io.Discard, []string{tempPath, installHelperDigest(body), "validate"}); err == nil {
		t.Fatal("temporary journal validated as the canonical journal")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "targets")
	missing, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	unknown := append(bytes.TrimSuffix(body, []byte("}")), []byte(`,"unknown":true}`)...)
	duplicate := bytes.Replace(body, []byte(`"schema_version":`), []byte(`"schema_version":"duplicate","schema_version":`), 1)
	for name, malformed := range map[string][]byte{
		"missing":   missing,
		"unknown":   unknown,
		"duplicate": duplicate,
		"trailing":  append(append([]byte(nil), body...), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			writeInstallHelperFile(t, fixture.journalPath, malformed)
			if _, err := loadInstallHelperJournal(fixture.journalPath, installHelperDigest(malformed), fixture.journalPath); err == nil {
				t.Fatal("malformed journal accepted")
			}
		})
	}
}

func TestInstallJournalHelperUpdateFromOwnsGenerationJSONAndCAS(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	oldBody, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := installHelperDigest(oldBody)
	oldIdentity := installHelperPathIdentity(t, fixture.journalPath)
	if linkAt, metadataAt := bytes.Index(oldBody, []byte(`"link":{`)), bytes.Index(oldBody, []byte(`"metadata":{`)); linkAt < 0 || metadataAt < 0 || linkAt > metadataAt {
		t.Fatalf("fixture does not exercise Go's link-before-metadata map order: %s", oldBody)
	}
	entry := threadpointBundleEntries[0]
	key := installJournalBundleKey(entry)
	targetPath := filepath.Join(fixture.bundleRoot, filepath.FromSlash(entry))
	sourcePath := filepath.Join(filepath.Dir(targetPath), ".produced-helper")
	writeInstallHelperFile(t, sourcePath, []byte("produced helper generation\n"))
	sourceGeneration := installHelperGeneration(t, sourcePath)

	assertUnchanged := func(t *testing.T) {
		t.Helper()
		body, err := os.ReadFile(fixture.journalPath)
		if err != nil || !bytes.Equal(body, oldBody) {
			t.Fatalf("rejected update changed journal: %v", err)
		}
	}
	for name, args := range map[string][]string{
		"wrong digest":   {fixture.journalPath, strings.Repeat("0", 64), "update-from", oldIdentity, key, "produced", "regular", sourcePath},
		"wrong identity": {fixture.journalPath, oldDigest, "update-from", "0:0", key, "produced", "regular", sourcePath},
		"unknown key":    {fixture.journalPath, oldDigest, "update-from", oldIdentity, "bundle:unknown", "produced", "regular", sourcePath},
		"unknown field":  {fixture.journalPath, oldDigest, "update-from", oldIdentity, key, "unknown", "regular", sourcePath},
		"foreign source": {fixture.journalPath, oldDigest, "update-from", oldIdentity, key, "produced", "regular", filepath.Join(t.TempDir(), "foreign")},
		"wrong copied generation": {
			fixture.journalPath, oldDigest, installJournalActionUpdateExpect, oldIdentity, key, "produced", "regular", sourcePath,
			"0:0", sourceGeneration.Digest,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runInstallJournalHelper(io.Discard, args); err == nil {
				t.Fatal("invalid update accepted")
			}
			assertUnchanged(t)
		})
	}

	var output bytes.Buffer
	if err := runInstallJournalHelper(&output, []string{
		fixture.journalPath, oldDigest, "update-from", oldIdentity, key, "produced", "regular", sourcePath,
	}); err != nil {
		t.Fatalf("update produced generation: %v", err)
	}
	newIdentity, newDigest := parseInstallHelperUpdateOutput(t, output.String())
	if newIdentity != installHelperPathIdentity(t, fixture.journalPath) {
		t.Fatal("update returned a different journal identity")
	}
	newBody, err := os.ReadFile(fixture.journalPath)
	if err != nil || installHelperDigest(newBody) != newDigest {
		t.Fatalf("updated journal digest mismatch: %v", err)
	}
	loaded, err := loadInstallHelperJournal(fixture.journalPath, newDigest, fixture.journalPath)
	if err != nil {
		t.Fatalf("helper-owned updated JSON is not interoperable: %v", err)
	}
	if !installGenerationMatches(mustInstallHelperIdentity(t, sourcePath), loaded.journal.Targets[key].Produced) {
		t.Fatal("helper did not record its rooted source generation")
	}
	if err := loaded.root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runInstallJournalHelper(io.Discard, []string{fixture.journalPath, oldDigest, "matches", key, "produced", sourcePath}); err == nil {
		t.Fatal("stale digest matched after CAS update")
	}
	if err := runInstallJournalHelper(io.Discard, []string{fixture.journalPath, newDigest, "matches", key, "produced", sourcePath}); err != nil {
		t.Fatalf("recorded generation match: %v", err)
	}
	detachedDir := filepath.Join(filepath.Dir(targetPath), ".install-detach-Ab12x9")
	if err := os.Mkdir(detachedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	detachedPath := filepath.Join(detachedDir, "candidate")
	if err := os.Rename(sourcePath, detachedPath); err != nil {
		t.Fatal(err)
	}
	if err := runInstallJournalHelper(io.Discard, []string{fixture.journalPath, newDigest, "matches", key, "produced", detachedPath}); err != nil {
		t.Fatalf("detached private candidate did not match: %v", err)
	}
	if err := os.Rename(detachedPath, sourcePath); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	if err := runInstallJournalHelper(&output, []string{
		fixture.journalPath, newDigest, "update-from", newIdentity, key, "recovery", "regular", sourcePath,
	}); err != nil {
		t.Fatal(err)
	}
	newIdentity, newDigest = parseInstallHelperUpdateOutput(t, output.String())
	output.Reset()
	if err := runInstallJournalHelper(&output, []string{
		fixture.journalPath, newDigest, "update-from", newIdentity, key, "prior", "regular", targetPath,
	}); err != nil {
		t.Fatal(err)
	}
	newIdentity, newDigest = parseInstallHelperUpdateOutput(t, output.String())
	loaded, err = loadInstallHelperJournal(fixture.journalPath, newDigest, fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.journal.Targets[key].Recovery.Kind != "" {
		t.Fatal("updating Prior did not atomically clear Recovery")
	}
	if err := loaded.root.Close(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runInstallJournalHelper(&output, []string{
		fixture.journalPath, newDigest, "update-from", newIdentity, key, "produced", "absent", "-",
	}); err != nil {
		t.Fatalf("record absent generation: %v", err)
	}
	_, newDigest = parseInstallHelperUpdateOutput(t, output.String())
	if err := runInstallJournalHelper(io.Discard, []string{fixture.journalPath, newDigest, "matches", key, "produced", targetPath}); err == nil {
		t.Fatal("absent generation matched an existing target")
	}
	if err := os.Remove(targetPath); err != nil {
		t.Fatal(err)
	}
	if err := runInstallJournalHelper(io.Discard, []string{fixture.journalPath, newDigest, "matches", key, "produced", targetPath}); err != nil {
		t.Fatalf("absent generation did not match removed target: %v", err)
	}
}

func TestInstallJournalHelperUpdateFromAcceptsPrivateRecoveryLinkCandidate(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := installHelperDigest(body)
	identity := installHelperPathIdentity(t, fixture.journalPath)
	privateDir := filepath.Join(filepath.Dir(fixture.commandPath), ".link-recover-Z9y8X7")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(privateDir, "candidate")
	linkTarget := filepath.Join(fixture.bundleRoot, "bin", threadpointProductName)
	if err := os.Symlink(linkTarget, candidate); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runInstallJournalHelper(&output, []string{
		fixture.journalPath, digest, "update-from", identity, "link", "recovery", "symlink", candidate,
	}); err != nil {
		t.Fatalf("private recovery link update: %v", err)
	}
	_, newDigest := parseInstallHelperUpdateOutput(t, output.String())
	loaded, err := loadInstallHelperJournal(fixture.journalPath, newDigest, fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := installHelperGenerationMatchesPath(candidate, loaded.journal.Targets["link"].Recovery); err != nil {
		t.Fatalf("recorded recovery link mismatch: %v", err)
	}
	if err := loaded.root.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallJournalHelperUpdateFromRejectsRacedFIFOAndOversizedSources(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := installHelperDigest(body)
	identity := installHelperPathIdentity(t, fixture.journalPath)
	entry := threadpointBundleEntries[0]
	key := installJournalBundleKey(entry)
	targetPath := filepath.Join(fixture.bundleRoot, filepath.FromSlash(entry))
	parent := filepath.Dir(targetPath)

	fifoPath := filepath.Join(parent, ".source-fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- runInstallJournalHelper(io.Discard, []string{
			fixture.journalPath, digest, "update-from", identity, key, "produced", "regular", fifoPath,
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO source accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO source blocked generation update")
	}

	oversized := filepath.Join(parent, ".source-oversized")
	writeInstallHelperFile(t, oversized, nil)
	if err := os.Truncate(oversized, maxInstallManagedFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := runInstallJournalHelper(io.Discard, []string{
		fixture.journalPath, digest, "update-from", identity, key, "produced", "regular", oversized,
	}); err == nil {
		t.Fatal("oversized generation source accepted")
	}

	raced := filepath.Join(parent, ".source-raced")
	writeInstallHelperFile(t, raced, []byte("reviewed\n"))
	displaced := raced + ".displaced"
	inheritedInstallHelperAfterGenerationSnapshot = func(string) {
		if err := os.Rename(raced, displaced); err != nil {
			t.Fatal(err)
		}
		writeInstallHelperFile(t, raced, []byte("replacement\n"))
	}
	err = runInstallJournalHelper(io.Discard, []string{
		fixture.journalPath, digest, "update-from", identity, key, "produced", "regular", raced,
	})
	inheritedInstallHelperAfterGenerationSnapshot = nil
	if err == nil {
		t.Fatal("last-moment source replacement accepted")
	}
	if current, readErr := os.ReadFile(fixture.journalPath); readErr != nil || !bytes.Equal(current, body) {
		t.Fatalf("rejected source changed journal: %v", readErr)
	}
}

func TestInstallJournalHelperUpdateExchangeMismatchRestoresCanonicalJournal(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := installHelperDigest(body)
	identity := installHelperPathIdentity(t, fixture.journalPath)
	entry := threadpointBundleEntries[0]
	key := installJournalBundleKey(entry)
	targetPath := filepath.Join(fixture.bundleRoot, filepath.FromSlash(entry))
	source := filepath.Join(filepath.Dir(targetPath), ".exchange-source")
	writeInstallHelperFile(t, source, []byte("new generation\n"))
	inheritedInstallHelperAfterJournalExchange = func(root *os.Root, base, _ string) {
		if err := root.Remove(base); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(base, []byte("foreign journal"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err = runInstallJournalHelper(io.Discard, []string{
		fixture.journalPath, digest, "update-from", identity, key, "produced", "regular", source,
	})
	inheritedInstallHelperAfterJournalExchange = nil
	if err == nil {
		t.Fatal("exchange identity mismatch accepted")
	}
	current, readErr := os.ReadFile(fixture.journalPath)
	if readErr != nil || !bytes.Equal(current, body) {
		t.Fatalf("exchange mismatch did not restore canonical journal: %v", readErr)
	}
}

func TestInstallJournalHelperUpdateSyncAmbiguityPreservesBothGenerations(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := installHelperDigest(body)
	identity := installHelperPathIdentity(t, fixture.journalPath)
	entry := threadpointBundleEntries[0]
	key := installJournalBundleKey(entry)
	targetPath := filepath.Join(fixture.bundleRoot, filepath.FromSlash(entry))
	source := filepath.Join(filepath.Dir(targetPath), ".sync-source")
	writeInstallHelperFile(t, source, []byte("new generation\n"))
	originalSync := syncInstallHelperJournalRoot
	syncInstallHelperJournalRoot = func(*os.Root) error { return errors.New("injected journal parent sync failure") }
	err = runInstallJournalHelper(io.Discard, []string{
		fixture.journalPath, digest, "update-from", identity, key, "produced", "regular", source,
	})
	syncInstallHelperJournalRoot = originalSync
	if err == nil || !strings.Contains(err.Error(), "injected journal parent sync failure") {
		t.Fatalf("sync ambiguity error = %v", err)
	}
	current, readErr := os.ReadFile(fixture.journalPath)
	if readErr != nil || bytes.Equal(current, body) {
		t.Fatalf("new canonical journal was not preserved: %v", readErr)
	}
	residue, err := filepath.Glob(filepath.Join(filepath.Dir(fixture.journalPath), ".transaction-update-*"))
	if err != nil || len(residue) != 1 {
		t.Fatalf("displaced journal residue = %v, %v", residue, err)
	}
	displaced, err := os.ReadFile(residue[0])
	if err != nil || !bytes.Equal(displaced, body) {
		t.Fatalf("recoverable displaced journal changed: %v", err)
	}
	var inspect bytes.Buffer
	if err := runInstallJournalInspect(&inspect, []string{fixture.journalPath, fixture.journalPath}); err != nil {
		t.Fatalf("ambiguous canonical journal is not discoverable: %v", err)
	}
}

func TestInstallJournalHelperCommitFirstSyncAmbiguityRetainsExactJournal(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := installHelperDigest(body)
	identity := installHelperPathIdentity(t, fixture.journalPath)
	originalSync := syncInstallHelperJournalRoot
	syncInstallHelperJournalRoot = func(*os.Root) error { return errors.New("injected commit parent sync failure") }
	err = runInstallJournalHelper(io.Discard, []string{
		fixture.journalPath, digest, "commit", "recovery", identity,
	})
	syncInstallHelperJournalRoot = originalSync
	if err == nil || !strings.Contains(err.Error(), "injected commit parent sync failure") {
		t.Fatalf("commit sync ambiguity error = %v", err)
	}
	if _, err := os.Lstat(fixture.journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical journal unexpectedly remains after verified detach: %v", err)
	}
	quarantines, err := filepath.Glob(filepath.Join(filepath.Dir(fixture.journalPath), ".transaction-finalize-*"))
	if err != nil || len(quarantines) != 1 {
		t.Fatalf("commit quarantine = %v, %v", quarantines, err)
	}
	quarantined, err := os.ReadFile(quarantines[0])
	if err != nil || !bytes.Equal(quarantined, body) {
		t.Fatalf("commit quarantine changed: %v", err)
	}
	var inspect bytes.Buffer
	if err := runInstallJournalInspect(&inspect, []string{quarantines[0], fixture.journalPath}); err != nil {
		t.Fatalf("retained commit journal is not recoverable: %v", err)
	}
	if info, err := os.Stat(fixture.backupRoot); err != nil || !info.IsDir() {
		t.Fatalf("commit ambiguity removed backup: %v", err)
	}
}

func TestInstallHelperTargetUpdatesRequireKnownGenerationFields(t *testing.T) {
	fixture := newInstallHelperFixture(t)

	key := installJournalBundleKey(threadpointBundleEntries[0])
	generation := fixture.journal.Targets[key].Prior
	if err := updateInstallHelperTarget(nil, key, "prior", generation); err == nil {
		t.Fatal("nil journal update accepted")
	}
	if err := updateInstallHelperTarget(&fixture.journal, "unknown", "prior", generation); err == nil {
		t.Fatal("unknown target update accepted")
	}
	if err := updateInstallHelperTarget(&fixture.journal, key, "unknown", generation); err == nil {
		t.Fatal("unknown generation field update accepted")
	}
}

func TestInstallGenerationRecordingRollsBackFailedJournalRewrite(t *testing.T) {
	rootPath := t.TempDir()
	writeInstallHelperFile(t, filepath.Join(rootPath, "generation"), []byte("reviewed\n"))
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	identity, err := snapshotRegularInstallRemoval(root, "generation")
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := recordedInstallGeneration(identity)
	if err != nil {
		t.Fatal(err)
	}
	const key = "metadata"
	transaction := &preparedInstallTransaction{
		journalPath:     "/outside/journal.json",
		journalIdentity: identity,
		journal: installTransactionJournal{Targets: map[string]installJournalTarget{
			key: {Prior: recorded},
		}},
	}
	lock := &installLifecycleLock{activeTransaction: transaction, recoveringTransaction: true}
	if err := recordRecoveryInstallGeneration(lock, key, identity); err == nil {
		t.Fatal("recovery generation ignored failed journal rewrite")
	}
	if transaction.journal.Targets[key].Recovery.Kind != "" {
		t.Fatal("failed recovery rewrite was not rolled back")
	}
	if err := recordRecoveredPriorInstallGeneration(lock, key, identity); err == nil {
		t.Fatal("recovered-prior generation ignored failed journal rewrite")
	}
	if transaction.journal.Targets[key].Prior != recorded {
		t.Fatal("failed prior rewrite was not rolled back")
	}
	lock.recoveringTransaction = false
	if err := recordProducedInstallJournalGeneration(lock, key, recorded); err == nil {
		t.Fatal("produced generation ignored failed journal rewrite")
	}
	if transaction.journal.Targets[key].Produced.Kind != "" {
		t.Fatal("failed produced rewrite was not rolled back")
	}
	if err := recordProducedInstallJournalGeneration(lock, "unknown", recorded); err == nil {
		t.Fatal("unknown produced-generation target accepted")
	}
}

func TestInstallTransactionRetainsBackupWhenJournalUnlinkSyncFails(t *testing.T) {
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
	transaction, err := prepareInstallTransaction(productHome, commandPath, updateOperationUpdate, metadataPath, metadata, lock)
	if err != nil {
		t.Fatal(err)
	}
	journalBody, err := os.ReadFile(transaction.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	previousSync := syncInstallJournalRoot
	syncInstallJournalRoot = func(*os.Root) error { return errors.New("injected directory sync failure") }
	t.Cleanup(func() { syncInstallJournalRoot = previousSync })
	if err := completeInstallTransaction(transaction, lock); err != nil {
		t.Fatalf("post-unlink sync failure was reported as uncommitted: %v", err)
	}
	if _, err := os.Lstat(transaction.journalPath); !os.IsNotExist(err) {
		t.Fatalf("committed journal remains: %v", err)
	}
	if info, err := os.Lstat(transaction.journal.BackupRoot); err != nil || !info.IsDir() {
		t.Fatalf("recovery backup was not retained: info=%v err=%v", info, err)
	}

	// Model a crash where the unsynced unlink is lost. The retained backup must
	// make the resurrected journal fully recoverable.
	syncInstallJournalRoot = previousSync
	if err := os.WriteFile(transaction.journalPath, journalBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverInstallTransaction(productHome, commandPath, lock); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(transaction.journalPath); !os.IsNotExist(err) {
		t.Fatalf("resurrected journal remains after recovery: %v", err)
	}
	if _, err := os.Lstat(transaction.journal.BackupRoot); !os.IsNotExist(err) {
		t.Fatalf("durably reconciled backup remains: %v", err)
	}
}

func TestInstallJournalTargetOmitsInactiveGenerationsForShellInterop(t *testing.T) {
	target := installJournalTarget{Prior: installJournalGeneration{Kind: "absent"}}
	body, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), `{"prior":{"kind":"absent"}}`; got != want {
		t.Fatalf("inactive journal generation JSON = %s; want %s", got, want)
	}
	var decoded installJournalTarget
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Prior.Kind != "absent" || decoded.Produced.Kind != "" || decoded.Recovery.Kind != "" {
		t.Fatalf("journal target round trip = %+v", decoded)
	}
}

func TestPrepareInstallTransactionRetainsRecoveryStateWhenJournalPublishSyncIsAmbiguous(t *testing.T) {
	fixture := newInstallTransactionFixture(t, true)
	lock := fixture.acquire(t)
	previousSync := syncInstallJournalRoot
	syncInstallJournalRoot = func(*os.Root) error { return errors.New("injected journal publication sync failure") }
	t.Cleanup(func() { syncInstallJournalRoot = previousSync })

	transaction, err := prepareInstallTransaction(
		fixture.productHome,
		fixture.commandPath,
		updateOperationUpdate,
		fixture.metadataPath,
		fixture.metadata,
		lock,
	)
	if err == nil || !strings.Contains(err.Error(), "injected journal publication sync failure") {
		t.Fatalf("journal publication sync error = %v", err)
	}
	if transaction != nil {
		t.Fatalf("ambiguous journal publication returned an active transaction: %#v", transaction)
	}

	journalPath := installLifecycleJournalPath(fixture.productHome, fixture.commandPath)
	body, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("ambiguous publication did not retain journal: %v", err)
	}
	var journal installTransactionJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		t.Fatalf("retained journal is not recoverable: %v", err)
	}
	backupInfo, err := os.Lstat(journal.BackupRoot)
	if err != nil || !backupInfo.IsDir() {
		t.Fatalf("ambiguous publication did not retain exact backup: info=%v err=%v", backupInfo, err)
	}
	if lock.transactionBackup == nil || filepath.Clean(lock.transactionBackup.path) != filepath.Clean(journal.BackupRoot) {
		t.Fatalf("ambiguous publication did not retain its pinned backup: %#v", lock.transactionBackup)
	}
	if err := validateInstallTransactionChecksumsRoot(lock.transactionBackup.root); err != nil {
		t.Fatalf("retained recovery backup is incomplete: %v", err)
	}

	// Model immediate recovery while both possible durable outcomes remain safe.
	syncInstallJournalRoot = previousSync
	if err := recoverInstallTransaction(fixture.productHome, fixture.commandPath, lock); err != nil {
		t.Fatalf("retained journal and backup could not be recovered: %v", err)
	}
	if _, err := os.Lstat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered journal remains: %v", err)
	}
	if _, err := os.Lstat(journal.BackupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered backup remains: %v", err)
	}
}

func TestRewriteActiveInstallTransactionJournalSyncAmbiguityPreservesBothGenerations(t *testing.T) {
	fixture := newInstallTransactionFixture(t, true)
	lock := fixture.acquire(t)
	transaction := prepareFixtureTransaction(t, fixture, lock)
	originalBody, err := os.ReadFile(transaction.journalPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(fixture.commandPath); err != nil {
		t.Fatal(err)
	}

	previousSync := syncInstallJournalRoot
	syncInstallJournalRoot = func(*os.Root) error { return errors.New("injected journal rewrite parent sync failure") }
	t.Cleanup(func() { syncInstallJournalRoot = previousSync })
	err = recordProducedAbsentInstallGeneration(lock, "link")
	syncInstallJournalRoot = previousSync
	if err == nil || !strings.Contains(err.Error(), "injected journal rewrite parent sync failure") {
		t.Fatalf("journal rewrite sync ambiguity error = %v", err)
	}
	if transaction.journal.Targets["link"].Produced.Kind != installGenerationKindAbsent {
		t.Fatalf("in-memory journal rolled back the visible canonical generation: %+v", transaction.journal.Targets["link"])
	}

	canonicalBody, err := os.ReadFile(transaction.journalPath)
	if err != nil {
		t.Fatalf("ambiguous rewrite did not retain canonical journal: %v", err)
	}
	canonical, err := decodeInstallHelperJournalStrict(canonicalBody)
	if err != nil {
		t.Fatalf("ambiguous canonical journal is malformed: %v", err)
	}
	if canonical.Targets["link"].Produced.Kind != installGenerationKindAbsent {
		t.Fatalf("canonical journal does not contain rewritten generation: %+v", canonical.Targets["link"])
	}
	canonicalIdentity, err := snapshotRegularInstallRemoval(lock.lockParent, filepath.Base(transaction.journalPath))
	if err != nil || !os.SameFile(transaction.journalIdentity.info, canonicalIdentity.info) || transaction.journalIdentity.digest != canonicalIdentity.digest {
		t.Fatalf("in-memory journal identity does not match visible canonical generation: identity=%+v err=%v", transaction.journalIdentity, err)
	}

	residue, err := filepath.Glob(filepath.Join(filepath.Dir(transaction.journalPath), ".transaction-update-*.tmp"))
	if err != nil || len(residue) != 1 {
		t.Fatalf("displaced journal residue = %v, %v", residue, err)
	}
	displacedBody, err := os.ReadFile(residue[0])
	if err != nil || string(displacedBody) != string(originalBody) {
		t.Fatalf("displaced exact journal changed: err=%v", err)
	}
	if info, err := os.Lstat(transaction.journal.BackupRoot); err != nil || !info.IsDir() {
		t.Fatalf("ambiguous rewrite did not retain recovery backup: info=%v err=%v", info, err)
	}

	// Model the alternate durable crash outcome: the old journal remains at the
	// canonical name even though the produced target mutation already happened.
	if err := safefs.ExchangeRoot(lock.lockParent, filepath.Base(transaction.journalPath), filepath.Base(residue[0])); err != nil {
		t.Fatal(err)
	}
	if err := syncInstallRootDirectory(lock.lockParent); err != nil {
		t.Fatal(err)
	}
	recoveryErr := recoverInstallTransaction(fixture.productHome, fixture.commandPath, lock)
	if !errors.Is(recoveryErr, errInstallRecoveryOwnershipMismatch) {
		t.Fatalf("old-journal crash outcome recovery error = %v, want ownership refusal", recoveryErr)
	}
	if _, err := os.Lstat(fixture.commandPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fail-safe recovery changed the already-mutated target: %v", err)
	}
	if info, err := os.Lstat(transaction.journal.BackupRoot); err != nil || !info.IsDir() {
		t.Fatalf("fail-safe recovery did not retain its exact backup: info=%v err=%v", info, err)
	}
}

func TestActiveInstallPublicationsRejectChangedJournalAuthority(t *testing.T) {
	for _, operation := range []string{"metadata", "journal"} {
		for _, damage := range []string{"missing", "symlink", "directory", "changed-generation", "outside-parent", "read-only-parent"} {
			t.Run(operation+"/"+damage, func(t *testing.T) {
				fixture := newInstallTransactionFixture(t, true)
				lock := fixture.acquire(t)
				transaction, err := prepareInstallTransaction(fixture.productHome, fixture.commandPath, updateOperationUpdate, fixture.metadataPath, fixture.metadata, lock)
				if err != nil {
					t.Fatal(err)
				}
				path := fixture.metadataPath
				if operation == "journal" {
					path = transaction.journalPath
				}
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "missing":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "symlink", "directory":
					if err := os.Rename(path, path+"-retained"); err != nil {
						t.Fatal(err)
					}
					if damage == "directory" {
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Symlink(path+"-retained", path); err != nil {
						t.Fatal(err)
					}
				case "changed-generation":
					if err := os.Rename(path, path+"-retained"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("third party"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "outside-parent":
					path = filepath.Join(t.TempDir(), "outside.json")
					// #nosec G703 -- this outside-parent fixture is still confined to a fresh t.TempDir.
					if err := os.WriteFile(path, original, 0o600); err != nil {
						t.Fatal(err)
					}
					if operation == "journal" {
						transaction.journalPath = path
					}
				case "read-only-parent":
					parent := filepath.Dir(path)
					if err := os.Chmod(parent, 0o500); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
					if err := os.WriteFile(filepath.Join(parent, "permission-probe"), nil, 0o600); err == nil {
						t.Skip("filesystem does not enforce write permissions")
					}
				}
				before, beforeErr := os.ReadFile(path)
				if operation == "journal" {
					err = rewriteActiveInstallTransactionJournal(transaction, lock)
				} else {
					err = writeInstallMetadataChecked(path, fixture.metadata, lock)
				}
				if err == nil {
					t.Fatal("untrusted install publication accepted")
				}
				after, afterErr := os.ReadFile(path)
				if !bytes.Equal(before, after) || (beforeErr == nil) != (afterErr == nil) {
					t.Fatal("failed publication changed existing entry")
				}
			})
		}
	}
}

func TestJournalReplacementPreservesRecoverableGenerationsOnFailure(t *testing.T) {
	for _, failure := range []string{"exchange", "sync-published", "sync-cleaned", "closed-root", "changed-journal", "missing-journal", "invalid-schema", "invalid-path"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newInstallHelperFixture(t)
			before, err := os.ReadFile(fixture.journalPath)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := loadInstallHelperJournal(fixture.journalPath, installHelperDigest(before), fixture.journalPath)
			if err != nil {
				t.Fatal(err)
			}
			defer loaded.root.Close()
			loaded.journal.CreatedAt = loaded.journal.CreatedAt.Add(time.Second)
			after, err := json.Marshal(loaded.journal)
			if err != nil {
				t.Fatal(err)
			}
			originalExchange, originalSync := exchangeInstallHelperRoot, syncInstallHelperJournalRoot
			t.Cleanup(func() { exchangeInstallHelperRoot = originalExchange; syncInstallHelperJournalRoot = originalSync })
			sentinel := errors.New("journal filesystem failure")
			switch failure {
			case "exchange":
				exchangeInstallHelperRoot = func(*os.Root, string, string) error { return sentinel }
			case "sync-published", "sync-cleaned":
				calls, stop := 0, 1
				if failure == "sync-cleaned" {
					stop = 2
				}
				syncInstallHelperJournalRoot = func(root *os.Root) error {
					calls++
					if calls == stop {
						return sentinel
					}
					return originalSync(root)
				}
			case "closed-root":
				_ = loaded.root.Close()
			case "changed-journal":
				if err := os.WriteFile(fixture.journalPath, []byte("concurrent"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-journal":
				if err := os.Remove(fixture.journalPath); err != nil {
					t.Fatal(err)
				}
			case "invalid-schema":
				loaded.journal.SchemaVersion = "invalid"
			case "invalid-path":
				loaded.journal.CommandPath = filepath.Join(t.TempDir(), "foreign")
			}
			identity, digest, err := replaceInstallHelperJournal(loaded)
			if err == nil || identity != "" || digest != "" {
				t.Fatalf("failed update returned success identity=%q digest=%q err=%v", identity, digest, err)
			}
			if strings.HasPrefix(failure, "sync-") || failure == "exchange" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("lost filesystem error: %v", err)
				}
			}
			got, readErr := os.ReadFile(fixture.journalPath)
			switch failure {
			case "missing-journal":
				if !os.IsNotExist(readErr) {
					t.Fatalf("removed journal recreated: %v", readErr)
				}
			case "changed-journal":
				if readErr != nil || string(got) != "concurrent" {
					t.Fatalf("concurrent journal changed: %q %v", got, readErr)
				}
			case "sync-published", "sync-cleaned":
				if readErr != nil || !bytes.Equal(got, after) {
					t.Fatalf("published journal incomplete: %q %v", got, readErr)
				}
			default:
				if readErr != nil || !bytes.Equal(got, before) {
					t.Fatalf("failed publication changed journal: %q %v", got, readErr)
				}
			}
		})
	}
}

func TestActiveJournalRewritePreservesEvidenceWhenPreparationFails(t *testing.T) {
	for _, failure := range []string{"missing-transaction", "missing-identity", "invalid-timestamp", "outside-path", "closed-root", "changed-journal", "missing-journal", "cleanup-sync"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, true)
			lock := fixture.acquire(t)
			transaction := prepareFixtureTransaction(t, fixture, lock)
			journalPath := transaction.journalPath
			before, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			transaction.journal.CreatedAt = transaction.journal.CreatedAt.Add(time.Second)
			published, err := json.MarshalIndent(transaction.journal, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			published = append(published, '\n')
			originalSync := syncInstallJournalRoot
			defer func() { syncInstallJournalRoot = originalSync }()
			sentinel := errors.New("final journal durability failed")
			switch failure {
			case "missing-transaction":
				transaction = nil
			case "missing-identity":
				transaction.journalIdentity.info = nil
			case "invalid-timestamp":
				transaction.journal.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "outside-path":
				transaction.journalPath = filepath.Join(t.TempDir(), "journal")
			case "closed-root":
				original := lock.lockParent
				closed, err := os.OpenRoot(filepath.Dir(journalPath))
				if err != nil {
					t.Fatal(err)
				}
				_ = closed.Close()
				lock.lockParent = closed
				defer func() { lock.lockParent = original }()
			case "changed-journal":
				if err := os.WriteFile(journalPath, []byte("concurrent"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-journal":
				if err := os.Rename(journalPath, journalPath+".retained"); err != nil {
					t.Fatal(err)
				}
			case "cleanup-sync":
				calls := 0
				syncInstallJournalRoot = func(root *os.Root) error {
					calls++
					if calls == 2 {
						return sentinel
					}
					return originalSync(root)
				}
			}
			err = rewriteActiveInstallTransactionJournal(transaction, lock)
			if err == nil {
				t.Fatal("failed journal rewrite succeeded")
			}
			body, readErr := os.ReadFile(journalPath)
			switch failure {
			case "missing-journal":
				if !os.IsNotExist(readErr) {
					t.Fatalf("missing journal recreated: %v", readErr)
				}
			case "changed-journal":
				if readErr != nil || string(body) != "concurrent" {
					t.Fatalf("concurrent journal changed: %q %v", body, readErr)
				}
			case "cleanup-sync":
				if !errors.Is(err, sentinel) || readErr != nil || !bytes.Equal(body, published) {
					t.Fatalf("published journal lost on sync failure: %q %v %v", body, readErr, err)
				}
			default:
				if readErr != nil || !bytes.Equal(body, before) {
					t.Fatalf("failed rewrite changed journal: %q %v", body, readErr)
				}
			}
			if _, err := os.Stat(fixture.binaryPath); err != nil {
				t.Fatalf("rewrite changed installed binary: %v", err)
			}
		})
	}
}

func TestInstallerRecoveryRefusesUnownedMetadataAndBundleGenerations(t *testing.T) {
	for _, operation := range []string{"metadata", "bundle"} {
		for _, failure := range []string{"missing-journal-target", "unrecorded-generation", "prior-digest-mismatch", "directory", "symlink", "closed-source"} {
			if operation == "metadata" && failure == "closed-source" {
				continue
			}
			t.Run(operation+"/"+failure, func(t *testing.T) {
				fixture := newInstallTransactionFixture(t, true)
				lock := fixture.acquire(t)
				transaction := prepareFixtureTransaction(t, fixture, lock)
				lock.recoveringTransaction = true
				path, key := fixture.metadataPath, "metadata"
				entry := threadpointBundleEntries[0]
				if operation == "bundle" {
					path = filepath.Join(fixture.metadata.BundleRoot, filepath.FromSlash(entry))
					key = installJournalBundleKey(entry)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				metadata := fixture.metadata
				sourceDir := t.TempDir()
				sourcePath := filepath.Join(sourceDir, filepath.FromSlash(entry))
				if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
					t.Fatal(err)
				}
				sourceBody := before
				if failure == "prior-digest-mismatch" {
					metadata.Version = "v9.9.9"
					sourceBody = []byte("different prior generation")
				}
				// #nosec G703 -- the source path combines a fresh t.TempDir with a fixed managed bundle entry.
				if err := os.WriteFile(sourcePath, sourceBody, 0o600); err != nil {
					t.Fatal(err)
				}
				switch failure {
				case "missing-journal-target":
					delete(transaction.journal.Targets, key)
				case "unrecorded-generation":
					transaction.journal.Targets[key] = installJournalTarget{}
				case "directory", "symlink":
					if err := os.Rename(path, path+".retained"); err != nil {
						t.Fatal(err)
					}
					if failure == "directory" {
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.Symlink(path+".retained", path); err != nil {
							t.Fatal(err)
						}
					}
				}
				if operation == "metadata" {
					err = writeInstallMetadataChecked(path, metadata, lock)
				} else {
					source, openErr := os.OpenRoot(sourceDir)
					if openErr != nil {
						t.Fatal(openErr)
					}
					defer source.Close()
					if failure == "closed-source" {
						_ = source.Close()
					}
					err = copyBundleEntryBetweenPinnedRootsWithPolicy(source, entry, lock, true)
				}
				if err == nil {
					t.Fatal("recovery accepted an unowned generation")
				}
				retained := path
				if failure == "directory" || failure == "symlink" {
					retained = path + ".retained"
				}
				after, err := os.ReadFile(retained)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("prior generation changed: %q %v", after, err)
				}
			})
		}
	}
}

func TestInstallPreparationRefusesChangedInputsBeforePublishingJournal(t *testing.T) {
	for _, failure := range []string{"missing-lock", "missing-metadata", "mismatched-metadata", "unsupported-operation", "wrong-command", "non-regular-binary", "occupied-journal", "directory-journal", "missing-bundle-entry", "foreign-link"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, true)
			lock := fixture.acquire(t)
			selected := lock
			metadata := fixture.metadata
			metadataPath, commandPath := fixture.metadataPath, fixture.commandPath
			operation := updateOperationUpdate
			journalPath := installLifecycleJournalPath(fixture.productHome, commandPath)
			switch failure {
			case "missing-lock":
				selected = nil
			case "missing-metadata":
				metadataPath = filepath.Join(filepath.Dir(metadataPath), "missing.json")
			case "mismatched-metadata":
				metadata.Version = "v9.9.9"
			case "unsupported-operation":
				operation = updateOperation("invalid")
			case "wrong-command":
				commandPath = filepath.Join(filepath.Dir(commandPath), "other")
			case "non-regular-binary":
				if err := os.Rename(metadata.BinaryPath, metadata.BinaryPath+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(metadata.BinaryPath, 0o700); err != nil {
					t.Fatal(err)
				}
			case "occupied-journal":
				if err := os.WriteFile(journalPath, []byte("unresolved prior journal"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory-journal":
				if err := os.Mkdir(journalPath, 0o700); err != nil {
					t.Fatal(err)
				}
			case "missing-bundle-entry":
				if err := os.Rename(filepath.Join(metadata.BundleRoot, "README.md"), filepath.Join(metadata.BundleRoot, "README.retained")); err != nil {
					t.Fatal(err)
				}
			case "foreign-link":
				if err := os.Remove(commandPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("foreign", commandPath); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(fixture.metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prepareInstallTransaction(fixture.productHome, commandPath, operation, metadataPath, metadata, selected); err == nil {
				t.Fatal("unsafe transaction preparation succeeded")
			}
			after, err := os.ReadFile(fixture.metadataPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("preparation changed metadata: %q %v", after, err)
			}
			if failure == "occupied-journal" {
				body, err := os.ReadFile(journalPath)
				if err != nil || string(body) != "unresolved prior journal" {
					t.Fatalf("prior journal changed: %q %v", body, err)
				}
			} else if failure != "directory-journal" {
				if _, err := os.Lstat(journalPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused preparation published journal: %v", err)
				}
			}
		})
	}
}

func TestInstallJournalCommitRetainsEvidenceWhenFinalizationFails(t *testing.T) {
	for _, failure := range []string{"closed-root", "missing-journal", "wrong-identity", "wrong-parent", "quarantine-collision", "unwritable-parent", "unlink-failure"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newInstallHelperFixture(t)
			before, err := os.ReadFile(fixture.journalPath)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := loadInstallHelperJournal(fixture.journalPath, installHelperDigest(before), fixture.journalPath)
			if err != nil {
				t.Fatal(err)
			}
			defer loaded.root.Close()
			originalReader, originalSync := rand.Reader, syncInstallHelperJournalRoot
			defer func() { rand.Reader = originalReader; syncInstallHelperJournalRoot = originalSync }()
			switch failure {
			case "closed-root":
				_ = loaded.root.Close()
			case "missing-journal":
				if err := os.Rename(fixture.journalPath, fixture.journalPath+".retained"); err != nil {
					t.Fatal(err)
				}
			case "wrong-identity":
				loaded.info, err = loaded.root.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
			case "wrong-parent":
				loaded.parent = t.TempDir()
			case "quarantine-collision":
				rand.Reader = bytes.NewReader(bytes.Repeat([]byte{0x2a}, 4096))
				if err := loaded.root.WriteFile(".transaction-finalize-"+strings.Repeat("2a", 12), []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unwritable-parent":
				if err := os.Chmod(loaded.parent, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(loaded.parent, 0o700) }()
			case "unlink-failure":
				syncInstallHelperJournalRoot = func(root *os.Root) error { return root.Chmod(".", 0o500) }
				defer func() { _ = os.Chmod(loaded.parent, 0o700) }()
			}
			if err := commitInstallHelperJournal(loaded, "normal"); err == nil {
				t.Fatal("interrupted journal commit succeeded")
			}
			dir := filepath.Dir(fixture.journalPath)
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err == nil && bytes.Equal(body, before) {
					found = true
				}
			}
			if !found {
				t.Fatal("failed finalization lost the recoverable journal")
			}
		})
	}
}

func TestInitialInstallBackupRequiresAnEmptyCanonicalDirectory(t *testing.T) {
	for _, scenario := range []string{"empty", "nonempty", "noncanonical", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			journal := installTransactionJournal{BackupRoot: dir, PreviousInstall: false}
			switch scenario {
			case "nonempty":
				if err := os.WriteFile(filepath.Join(dir, "foreign"), []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "noncanonical":
				journal.BackupRoot = dir + string(filepath.Separator) + "."
			case "missing":
				journal.BackupRoot = filepath.Join(dir, "missing")
			}
			err := validateInstallJournalBackupPrior(journal)
			if (err == nil) != (scenario == "empty") {
				t.Fatalf("initial backup validation: %v", err)
			}
			if scenario == "nonempty" {
				body, err := os.ReadFile(filepath.Join(dir, "foreign"))
				if err != nil || string(body) != "preserve" {
					t.Fatalf("backup validation changed evidence: %q %v", body, err)
				}
			}
		})
	}
}

func TestJournalPublicationRefusesUnusableNamespaceAndLostValidation(t *testing.T) {
	for _, failure := range []string{"wrong-parent", "closed-parent", "unwritable-parent", "collisions", "validation", "occupied-target"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, true)
			lock := fixture.acquire(t)
			path := filepath.Join(lock.lockParent.Name(), "fixture-journal")
			parentPath := lock.lockParent.Name()
			sentinel := errors.New("ownership changed")
			var validate func() error
			oldParent := lock.lockParent
			defer func() { lock.lockParent = oldParent }()
			switch failure {
			case "wrong-parent":
				path = filepath.Join(t.TempDir(), "journal")
			case "closed-parent":
				closed, err := os.OpenRoot(parentPath)
				if err != nil {
					t.Fatal(err)
				}
				_ = closed.Close()
				lock.lockParent = closed
			case "unwritable-parent":
				if err := os.Chmod(parentPath, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(parentPath, 0o700) }()
			case "collisions":
				old := rand.Reader
				rand.Reader = bytes.NewReader(bytes.Repeat([]byte{0x2a}, 4096))
				defer func() { rand.Reader = old }()
				if err := lock.lockParent.WriteFile(".fixture-"+strings.Repeat("2a", 12)+".tmp", []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "validation":
				validate = func() error { return sentinel }
			case "occupied-target":
				if err := lock.lockParent.WriteFile("fixture-journal", []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := writeInstallJournalFileChecked(lock, path, []byte("new journal"), 0o600, ".fixture-", validate)
			if err == nil {
				t.Fatal("invalid journal publication succeeded")
			}
			if failure == "validation" && !errors.Is(err, sentinel) {
				t.Fatalf("validation cause lost: %v", err)
			}
			if failure == "occupied-target" {
				body, err := os.ReadFile(path)
				if err != nil || string(body) != "foreign" {
					t.Fatalf("foreign journal changed: %q %v", body, err)
				}
			} else if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed journal became visible: %v", err)
			}
		})
	}
}
