// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstallJournalHelperRemoveMatchedHandlesOpaqueCraftedSymlink(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := installHelperDigest(body)
	identity := installHelperPathIdentity(t, fixture.journalPath)
	privateDir := filepath.Join(filepath.Dir(fixture.commandPath), ".install-remove-A1b2C3")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(privateDir, "candidate")
	craftedTarget := "foreign\\target\"with-quote\nand-newline"
	if err := os.Symlink(craftedTarget, candidate); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runInstallJournalHelper(&output, []string{
		fixture.journalPath, digest, "update-from", identity, "link", "produced", "symlink", candidate,
	}); err != nil {
		t.Fatal(err)
	}
	_, digest = parseInstallHelperUpdateOutput(t, output.String())
	saved := filepath.Join(privateDir, "saved")
	if err := os.Rename(candidate, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("different", candidate); err != nil {
		t.Fatal(err)
	}
	if err := runInstallJournalHelper(io.Discard, []string{
		fixture.journalPath, digest, "remove-matched", "link", "produced", candidate,
	}); err == nil {
		t.Fatal("mismatched crafted symlink was removed")
	}
	if target, err := os.Readlink(candidate); err != nil || target != "different" {
		t.Fatalf("mismatched symlink changed: %q, %v", target, err)
	}
	if err := os.Remove(candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, candidate); err != nil {
		t.Fatal(err)
	}
	if err := runInstallJournalHelper(io.Discard, []string{
		fixture.journalPath, digest, "remove-matched", "link", "produced", candidate,
	}); err != nil {
		t.Fatalf("remove exact crafted symlink: %v", err)
	}
	if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("matched crafted symlink remains: %v", err)
	}
}

func TestInstallJournalHelperValidatesBackupAndExactPhaseBeforeCommit(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body := writeInstallHelperJournal(t, fixture.journalPath, fixture.journal)
	loaded, err := loadInstallHelperJournal(fixture.journalPath, installHelperDigest(body), fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstallJournalBackupPrior(loaded.journal); err != nil {
		t.Fatalf("valid prior backup: %v", err)
	}
	if err := loaded.root.Close(); err != nil {
		t.Fatal(err)
	}

	entry := threadpointBundleEntries[0]
	key := installJournalBundleKey(entry)
	path := filepath.Join(fixture.bundleRoot, filepath.FromSlash(entry))
	priorBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeInstallHelperFile(t, path, []byte("produced generation\n"))
	target := fixture.journal.Targets[key]
	target.Produced = installHelperGeneration(t, path)
	fixture.journal.Targets[key] = target
	body = writeInstallHelperJournal(t, fixture.journalPath, fixture.journal)
	loaded, err = loadInstallHelperJournal(fixture.journalPath, installHelperDigest(body), fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstallJournalPhase(loaded.journal, "normal"); err != nil {
		t.Fatalf("produced phase rejected: %v", err)
	}
	if err := loaded.root.Close(); err != nil {
		t.Fatal(err)
	}

	// Restore the exact prior inode and bytes. Normal completion must still
	// reject it because a recorded Produced generation takes precedence.
	writeInstallHelperFile(t, path, priorBody)
	if !installGenerationMatches(mustInstallHelperIdentity(t, path), target.Prior) {
		t.Fatal("test did not restore the exact prior generation")
	}
	loaded, err = loadInstallHelperJournal(fixture.journalPath, installHelperDigest(body), fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := installRemovalIdentityString(loaded.info)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitInstallHelperJournal(loaded, "normal"); err == nil {
		t.Fatal("normal commit accepted a target reverted from Produced to Prior")
	}
	if err := loaded.root.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(fixture.journalPath); err != nil {
		t.Fatalf("failed commit removed journal: %v", err)
	}

	loaded, err = loadInstallHelperJournal(fixture.journalPath, installHelperDigest(body), fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstallJournalPhase(loaded.journal, "recovery"); err != nil {
		t.Fatalf("recovery did not fall back to Prior: %v", err)
	}
	if err := runInstallJournalHelper(io.Discard, []string{fixture.journalPath, installHelperDigest(body), "commit", "recovery", identity}); err != nil {
		t.Fatalf("recovery commit: %v", err)
	}
	if err := loaded.root.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(fixture.journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed journal remains: %v", err)
	}
}

func TestInstallJournalHelperRejectsBackupMismatchSymlinkAndFIFO(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadInstallHelperJournal(fixture.journalPath, installHelperDigest(body), fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := threadpointBundleEntries[0]
	writeInstallHelperFile(t, filepath.Join(fixture.backupRoot, filepath.FromSlash(entry)), []byte("tampered backup\n"))
	if err := validateInstallJournalBackupPrior(loaded.journal); err == nil {
		t.Fatal("tampered prior backup accepted")
	}
	if err := loaded.root.Close(); err != nil {
		t.Fatal(err)
	}

	symlinkPath := filepath.Join(filepath.Dir(fixture.journalPath), ".journal-link")
	if err := os.Symlink(fixture.journalPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstallHelperJournal(symlinkPath, installHelperDigest(body), fixture.journalPath); err == nil {
		t.Fatal("symlink journal accepted")
	}
	fifoPath := filepath.Join(filepath.Dir(fixture.journalPath), ".journal-fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := loadInstallHelperJournal(fifoPath, installHelperDigest(body), fixture.journalPath)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO journal accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO journal blocked helper validation")
	}
}

func TestInstallJournalHelperReportsExactValuesAndGenerations(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	key := installJournalBundleKey(threadpointBundleEntries[0])
	target := fixture.journal.Targets[key]
	target.Produced = target.Prior
	target.Recovery = target.Prior
	fixture.journal.Targets[key] = target
	body := writeInstallHelperJournal(t, fixture.journalPath, fixture.journal)
	digest := installHelperDigest(body)

	values := map[string]string{
		"schema_version":   fixture.journal.SchemaVersion,
		"operation":        string(fixture.journal.Operation),
		"command_path":     fixture.journal.CommandPath,
		"metadata_path":    fixture.journal.MetadataPath,
		"bundle_root":      fixture.journal.BundleRoot,
		"backup_root":      fixture.journal.BackupRoot,
		"previous_install": "true",
		"link_existed":     "true",
	}
	for field, want := range values {
		t.Run("value "+field, func(t *testing.T) {
			var output bytes.Buffer
			if err := runInstallJournalHelper(&output, []string{fixture.journalPath, digest, "value", field}); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(output.String()); got != want {
				t.Fatalf("journal %s = %q, want %q", field, got, want)
			}
		})
	}

	for field, want := range map[string]installJournalGeneration{
		"prior": target.Prior, installJournalFieldProduced: target.Produced, "recovery": target.Recovery,
	} {
		t.Run("generation "+field, func(t *testing.T) {
			var output bytes.Buffer
			if err := runInstallJournalHelper(&output, []string{fixture.journalPath, digest, "generation", key, field}); err != nil {
				t.Fatal(err)
			}
			var got installJournalGeneration
			if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("journal %s generation = %#v, want %#v", field, got, want)
			}
		})
	}

	for name, args := range map[string][]string{
		"invalid validate":       {fixture.journalPath, digest, "validate", "extra"},
		"invalid validate as":    {fixture.journalPath, digest, "validate-as", fixture.journalPath, "extra"},
		"invalid value":          {fixture.journalPath, digest, "value"},
		"unknown value":          {fixture.journalPath, digest, "value", "unknown"},
		"invalid generation":     {fixture.journalPath, digest, "generation", key},
		"unknown generation key": {fixture.journalPath, digest, "generation", "unknown", "prior"},
		"unknown generation":     {fixture.journalPath, digest, "generation", key, "unknown"},
		"empty generation":       {fixture.journalPath, digest, "generation", "link", installJournalFieldProduced},
		"invalid backup":         {fixture.journalPath, digest, "backup-prior", "extra"},
		"invalid backup as":      {fixture.journalPath, digest, "backup-prior-as", fixture.journalPath, "extra"},
		"invalid matches":        {fixture.journalPath, digest, "matches"},
		"invalid removal":        {fixture.journalPath, digest, "remove-matched"},
		"invalid update":         {fixture.journalPath, digest, installJournalActionUpdateFrom},
		"invalid commit":         {fixture.journalPath, digest, "commit", "invalid", installHelperPathIdentity(t, fixture.journalPath)},
		"unknown action":         {fixture.journalPath, digest, "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runInstallJournalHelper(io.Discard, args); err == nil {
				t.Fatal("malformed journal helper operation was accepted")
			}
		})
	}
	if err := writeInstallJournalValue(rejectedInstallHelperWrite{}, fixture.journal, "operation"); err == nil {
		t.Fatal("journal value ignored output failure")
	}
	if err := writeInstallJournalGeneration(rejectedInstallHelperWrite{}, fixture.journal, key, "prior"); err == nil {
		t.Fatal("journal generation ignored output failure")
	}
}

func TestInstallHelperJSONValidationRejectsMalformedFields(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"malformed":          json.RawMessage(`[`),
		"missing kind":       json.RawMessage(`{}`),
		"empty kind":         json.RawMessage(`{"kind":""}`),
		"unsupported kind":   json.RawMessage(`{"kind":"device"}`),
		"extra absent field": json.RawMessage(`{"kind":"absent","identity":"x"}`),
		"missing identity":   json.RawMessage(`{"kind":"regular","digest":"00","other":"x"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateInstallHelperGenerationJSON(raw); err == nil {
				t.Fatal("malformed generation JSON accepted")
			}
		})
	}
	if err := rejectDuplicateInstallJSONKeys([]byte(`[{"nested":true}]`)); err != nil {
		t.Fatalf("valid JSON array rejected: %v", err)
	}
	if err := rejectDuplicateInstallJSONKeys([]byte(`{"truncated":`)); err == nil {
		t.Fatal("truncated JSON accepted")
	}
}

func TestInstallHelperCandidatePathsRequireKnownTargets(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	if got, err := installHelperTargetPath(fixture.journal, "metadata"); err != nil || got != fixture.metadata {
		t.Fatalf("metadata target = %q, %v", got, err)
	}
	if _, err := installHelperTargetPath(fixture.journal, "unknown"); err == nil {
		t.Fatal("unknown helper target accepted")
	}
	if err := validateInstallHelperCandidatePath(fixture.journal, "metadata", "relative"); err == nil {
		t.Fatal("relative helper candidate accepted")
	}
	if err := validateInstallHelperCandidatePath(fixture.journal, "unknown", fixture.metadata); err == nil {
		t.Fatal("candidate for unknown target accepted")
	}
	if err := validateInstallHelperCandidatePath(fixture.journal, "metadata", filepath.Join(filepath.Dir(fixture.metadata), ".bad-private", "candidate")); err == nil {
		t.Fatal("candidate in unrecognized private directory accepted")
	}
}

func TestInstallHelperStrictJournalContractErrors(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	marshalTop := func(t *testing.T, mutate func(map[string]json.RawMessage)) []byte {
		t.Helper()
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			t.Fatal(err)
		}
		mutate(top)
		result, err := json.Marshal(top)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	mutateTarget := func(t *testing.T, top map[string]json.RawMessage, mutate func(string, map[string]json.RawMessage)) {
		t.Helper()
		var targets map[string]json.RawMessage
		if err := json.Unmarshal(top["targets"], &targets); err != nil {
			t.Fatal(err)
		}
		for key, raw := range targets {
			if key == "link" {
				continue
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			mutate(key, fields)
			encoded, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			targets[key] = encoded
			break
		}
		encoded, err := json.Marshal(targets)
		if err != nil {
			t.Fatal(err)
		}
		top["targets"] = encoded
	}
	tests := []struct {
		name    string
		journal installTransactionJournal
		body    []byte
		want    string
	}{
		{name: "malformed top", journal: fixture.journal, body: []byte(`[]`), want: "cannot unmarshal"},
		{name: "wrong top field count", journal: fixture.journal, body: []byte(`{}`), want: "top-level fields are not exact"},
		{name: "missing required field", journal: fixture.journal, body: marshalTop(t, func(top map[string]json.RawMessage) {
			delete(top, "schema_version")
			top["unknown"] = json.RawMessage(`true`)
		}), want: "missing schema_version"},
		{name: "invalid raw targets", journal: fixture.journal, body: marshalTop(t, func(top map[string]json.RawMessage) {
			top["targets"] = json.RawMessage(`[]`)
		}), want: "cannot unmarshal"},
		{name: "invalid raw target", journal: fixture.journal, body: marshalTop(t, func(top map[string]json.RawMessage) {
			var targets map[string]json.RawMessage
			if err := json.Unmarshal(top["targets"], &targets); err != nil {
				t.Fatal(err)
			}
			targets["metadata"] = json.RawMessage(`[]`)
			encoded, err := json.Marshal(targets)
			if err != nil {
				t.Fatal(err)
			}
			top["targets"] = encoded
		}), want: "cannot unmarshal"},
		{name: "missing target prior", journal: fixture.journal, body: marshalTop(t, func(top map[string]json.RawMessage) {
			mutateTarget(t, top, func(_ string, fields map[string]json.RawMessage) { delete(fields, "prior") })
		}), want: "target fields are not exact"},
		{name: "unknown target field", journal: fixture.journal, body: marshalTop(t, func(top map[string]json.RawMessage) {
			mutateTarget(t, top, func(_ string, fields map[string]json.RawMessage) { fields["unknown"] = json.RawMessage(`true`) })
		}), want: "unknown field"},
		{name: "invalid target generation", journal: fixture.journal, body: marshalTop(t, func(top map[string]json.RawMessage) {
			mutateTarget(t, top, func(_ string, fields map[string]json.RawMessage) {
				fields["prior"] = json.RawMessage(`{"kind":"device"}`)
			})
		}), want: "unsupported generation kind"},
	}
	zeroCreated := fixture.journal
	zeroCreated.CreatedAt = time.Time{}
	tests = append(tests, struct {
		name    string
		journal installTransactionJournal
		body    []byte
		want    string
	}{"missing creation time", zeroCreated, body, "creation time is required"})
	badPath := fixture.journal
	badPath.CommandPath = "relative"
	tests = append(tests, struct {
		name    string
		journal installTransactionJournal
		body    []byte
		want    string
	}{"non-canonical path", badPath, body, "path is not canonical"})
	badPrior := fixture.journal
	badPrior.Targets = make(map[string]installJournalTarget, len(fixture.journal.Targets))
	for key, target := range fixture.journal.Targets {
		badPrior.Targets[key] = target
	}
	firstKey := installJournalBundleKey(threadpointBundleEntries[0])
	badPriorTarget := badPrior.Targets[firstKey]
	badPriorTarget.Prior = installJournalGeneration{Kind: installGenerationKindAbsent}
	badPrior.Targets[firstKey] = badPriorTarget
	tests = append(tests, struct {
		name    string
		journal installTransactionJournal
		body    []byte
		want    string
	}{"incomplete prior", badPrior, body, "prior generation is incomplete"})
	initialWithPrior := fixture.journal
	initialWithPrior.PreviousInstall = false
	tests = append(tests, struct {
		name    string
		journal installTransactionJournal
		body    []byte
		want    string
	}{"initial install with prior", initialWithPrior, body, "unexpectedly owns a prior"})
	badLink := fixture.journal
	badLink.LinkExisted = false
	tests = append(tests, struct {
		name    string
		journal installTransactionJournal
		body    []byte
		want    string
	}{"contradictory link state", badLink, body, "link state contradicts"})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateInstallHelperJournalContract(test.journal, test.body)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("contract error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestInstallHelperJournalReadRequiresDigestAndCanonicalPath(t *testing.T) {
	if _, err := loadInstallHelperJournal("ignored", "short", "/tmp/journal"); err == nil {
		t.Fatal("short journal digest accepted")
	}
	if _, err := loadInstallHelperJournal("ignored", "", "relative"); err == nil {
		t.Fatal("relative expected journal path accepted")
	}
}

func TestInstallTransactionJournalValidationRejectsForeignIdentities(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	bundleRoot := filepath.Join(t.TempDir(), "bundle")
	metadataPath := installerMetadataPath(productHome, filepath.Join(bundleRoot, "bin", threadpointProductName))
	journalPath := installLifecycleJournalPath(productHome, commandPath)
	valid := installTransactionJournal{
		SchemaVersion:   installTransactionSchemaVersion,
		Operation:       updateOperationInstall,
		CommandPath:     commandPath,
		MetadataPath:    metadataPath,
		BundleRoot:      bundleRoot,
		BackupRoot:      filepath.Join(productHome, "installs", "transactions", ".bundle-valid"),
		PreviousInstall: false,
		CreatedAt:       time.Now().UTC(),
		Targets:         absentInstallJournalTargets(),
	}
	if err := validateInstallTransactionJournal(productHome, commandPath, journalPath, valid); err != nil {
		t.Fatalf("valid journal rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*installTransactionJournal) (string, string)
		want   string
	}{
		{name: "schema", mutate: func(j *installTransactionJournal) (string, string) {
			j.SchemaVersion = "other"
			return commandPath, journalPath
		}, want: "identity is invalid"},
		{name: "command", mutate: func(j *installTransactionJournal) (string, string) {
			j.CommandPath += "-other"
			return commandPath, journalPath
		}, want: "identity is invalid"},
		{name: "journal path", mutate: func(*installTransactionJournal) (string, string) { return commandPath, journalPath + "-other" }, want: "identity is invalid"},
		{name: "operation", mutate: func(j *installTransactionJournal) (string, string) {
			j.Operation = "foreign"
			return commandPath, journalPath
		}, want: "unsupported install transaction operation"},
		{name: "relative bundle", mutate: func(j *installTransactionJournal) (string, string) {
			j.BundleRoot = "relative"
			return commandPath, journalPath
		}, want: "paths must be absolute"},
		{name: "relative metadata", mutate: func(j *installTransactionJournal) (string, string) {
			j.MetadataPath = "relative"
			return commandPath, journalPath
		}, want: "paths must be absolute"},
		{name: "foreign metadata", mutate: func(j *installTransactionJournal) (string, string) {
			j.MetadataPath = filepath.Join(productHome, "installs", "foreign.json")
			return commandPath, journalPath
		}, want: "metadata is outside"},
		{name: "foreign backup", mutate: func(j *installTransactionJournal) (string, string) {
			j.BackupRoot = filepath.Join(productHome, "elsewhere", ".bundle-valid")
			return commandPath, journalPath
		}, want: "backup is outside"},
		{name: "nested backup", mutate: func(j *installTransactionJournal) (string, string) {
			j.BackupRoot = filepath.Join(productHome, "installs", "transactions", "nested", ".bundle-valid")
			return commandPath, journalPath
		}, want: "backup is outside"},
		{name: "wrong backup name", mutate: func(j *installTransactionJournal) (string, string) {
			j.BackupRoot = filepath.Join(productHome, "installs", "transactions", "backup")
			return commandPath, journalPath
		}, want: "backup is outside"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := valid
			command, path := test.mutate(&journal)
			if err := validateInstallTransactionJournal(productHome, command, path, journal); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDecodeInstallTransactionJournalStrictRejectsDuplicateKeys(t *testing.T) {
	var journal installTransactionJournal
	body := []byte(`{"schema_version":"` + installTransactionSchemaVersion + `","schema_version":"` + installTransactionSchemaVersion + `"}`)
	if err := decodeInstallTransactionJournalStrict(body, &journal); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate install transaction journal key error = %v", err)
	}
}

func TestRecoverInstallTransactionRejectsMalformedJournalFiles(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, fixture installTransactionFixture, journalPath string)
	}{
		{
			name: "symlink",
			write: func(t *testing.T, fixture installTransactionFixture, journalPath string) {
				t.Helper()
				if err := os.Symlink(fixture.metadataPath, journalPath); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
		},
		{
			name: "directory",
			write: func(t *testing.T, _ installTransactionFixture, journalPath string) {
				t.Helper()
				if err := os.Mkdir(journalPath, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized",
			write: func(t *testing.T, _ installTransactionFixture, journalPath string) {
				t.Helper()
				if err := os.WriteFile(journalPath, bytes.Repeat([]byte("x"), int(maxInstallTransactionBytes)+1), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed json",
			write: func(t *testing.T, _ installTransactionFixture, journalPath string) {
				t.Helper()
				if err := os.WriteFile(journalPath, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invalid identity",
			write: func(t *testing.T, fixture installTransactionFixture, journalPath string) {
				t.Helper()
				journal := installTransactionJournal{
					SchemaVersion: "foreign", Operation: updateOperationUpdate,
					CommandPath: fixture.commandPath, MetadataPath: fixture.metadataPath,
					BundleRoot:      fixture.productHome,
					BackupRoot:      filepath.Join(fixture.productHome, "installs", "transactions", ".bundle-missing"),
					PreviousInstall: true,
				}
				if err := writeInstallTransactionJournal(journalPath, journal); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing previous backup",
			write: func(t *testing.T, fixture installTransactionFixture, journalPath string) {
				t.Helper()
				journal := installTransactionJournal{
					SchemaVersion: installTransactionSchemaVersion, Operation: updateOperationUpdate,
					CommandPath: fixture.commandPath, MetadataPath: fixture.metadataPath,
					BundleRoot:      fixture.productHome,
					BackupRoot:      filepath.Join(fixture.productHome, "installs", "transactions", ".bundle-missing"),
					PreviousInstall: true,
				}
				if err := writeInstallTransactionJournal(journalPath, journal); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, false)
			lock := fixture.acquire(t)
			journalPath := installLifecycleJournalPath(fixture.productHome, fixture.commandPath)
			test.write(t, fixture, journalPath)
			if err := recoverInstallTransaction(fixture.productHome, fixture.commandPath, lock); err == nil {
				t.Fatal("malformed install transaction journal was accepted")
			}
			if _, err := os.Lstat(journalPath); err != nil {
				t.Fatalf("failed recovery removed journal: %v", err)
			}
		})
	}
}

func TestInstallTransactionRecoversPartialInitialInstallJournal(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	binaryPath := filepath.Join(productHome, "bin", threadpointProductName)
	metadataPath := installerMetadataPath(productHome, binaryPath)
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{binaryPath, filepath.Join(productHome, "README.md"), metadataPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(binaryPath, commandPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	transactionsRoot := filepath.Join(productHome, "installs", "transactions")
	if err := os.MkdirAll(transactionsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	backupRoot, err := os.MkdirTemp(transactionsRoot, ".bundle-")
	if err != nil {
		t.Fatal(err)
	}
	journalPath := installLifecycleJournalPath(productHome, commandPath)
	targets := absentInstallJournalTargets()
	for key, path := range map[string]string{
		installJournalBundleKey("bin/threadpoint"): binaryPath,
		installJournalBundleKey("README.md"):       filepath.Join(productHome, "README.md"),
		"metadata":                                 metadataPath,
	} {
		target := targets[key]
		target.Produced = testInstallJournalGeneration(t, path, false)
		targets[key] = target
	}
	linkTarget := targets["link"]
	linkTarget.Produced = testInstallJournalGeneration(t, commandPath, true)
	targets["link"] = linkTarget
	if err := writeInstallTransactionJournal(journalPath, installTransactionJournal{
		SchemaVersion:   installTransactionSchemaVersion,
		Operation:       updateOperationInstall,
		CommandPath:     commandPath,
		MetadataPath:    metadataPath,
		BundleRoot:      productHome,
		BackupRoot:      backupRoot,
		PreviousInstall: false,
		CreatedAt:       time.Now().UTC(),
		Targets:         targets,
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	if err := recoverInstallTransaction(productHome, commandPath, lock); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{binaryPath, filepath.Join(productHome, "README.md"), metadataPath, commandPath, journalPath, backupRoot} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("partial initial-install path remains after recovery: %s: %v", path, err)
		}
	}
}

func writeInstallTransactionJournal(path string, journal installTransactionJournal) error {
	return writeInstallFixtureJSON(path, journal, 0o600)
}

func absentInstallJournalTargets() map[string]installJournalTarget {
	targets := make(map[string]installJournalTarget, len(threadpointBundleEntries)+2)
	for _, entry := range threadpointBundleEntries {
		targets[installJournalBundleKey(entry)] = installJournalTarget{Prior: installJournalGeneration{Kind: "absent"}}
	}
	targets["metadata"] = installJournalTarget{Prior: installJournalGeneration{Kind: "absent"}}
	targets["link"] = installJournalTarget{Prior: installJournalGeneration{Kind: "absent"}}
	return targets
}
