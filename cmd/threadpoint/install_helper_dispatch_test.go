// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInheritedInstallHelperDispatchesEveryPrivateCommand(t *testing.T) {
	commands := []string{
		inheritedInstallLockCommand,
		inheritedInstallNoReplaceCommand,
		inheritedInstallNoReplaceAs,
		inheritedInstallJournalCommand,
		inheritedInstallInspectCommand,
		inheritedInstallEntryIdentity,
		inheritedInstallEntryInspect,
		inheritedInstallRegularSnapshot,
		inheritedInstallRemoveAs,
		inheritedInstallCopyBoundedAs,
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			var stderr bytes.Buffer
			handled, code := runInheritedInstallLockHelper(io.Discard, &stderr, strings.NewReader(""), []string{command})
			if !handled || code != ExitInternal || !strings.Contains(stderr.String(), "installer helper failed") {
				t.Fatalf("malformed private command result: handled=%v code=%d stderr=%q", handled, code, stderr.String())
			}
		})
	}
	if handled, code := runInheritedInstallLockHelper(io.Discard, io.Discard, strings.NewReader(""), nil); handled || code != 0 {
		t.Fatalf("empty command was handled: handled=%v code=%d", handled, code)
	}
	if handled, code := runInheritedInstallLockHelper(io.Discard, io.Discard, strings.NewReader(""), []string{"ordinary-command"}); handled || code != 0 {
		t.Fatalf("ordinary command was handled: handled=%v code=%d", handled, code)
	}

	fixture := newInstallHelperFixture(t)
	body, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	handled, code := runInheritedInstallLockHelper(&stdout, &stderr, strings.NewReader(""), []string{
		inheritedInstallJournalCommand, fixture.journalPath, installHelperDigest(body), "value", "schema_version",
	})
	if !handled || code != ExitOK || stderr.Len() != 0 || strings.TrimSpace(stdout.String()) != installTransactionSchemaVersion {
		t.Fatalf("valid private command result: handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
}

func TestInstallHelperRejectsMalformedEntryOperations(t *testing.T) {
	if err := runInstallLockGuard(io.Discard, strings.NewReader(""), []string{"only-one"}); err == nil {
		t.Fatal("short lock invocation accepted")
	}
	if err := runInstallLockGuard(io.Discard, strings.NewReader(""), []string{filepath.Join(t.TempDir(), ".guard"), "0"}); err == nil {
		t.Fatal("invalid lock timeout accepted")
	}
	if err := runInstallNoReplaceExpected([]string{"only-one"}, false); err == nil {
		t.Fatal("short no-replace invocation accepted")
	}
	if err := runInstallNoReplaceExpected([]string{"relative", filepath.Join(t.TempDir(), "target")}, false); err == nil {
		t.Fatal("relative no-replace source accepted")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := runInstallEntryIdentity(io.Discard, []string{missing}); err == nil {
		t.Fatal("missing identity entry accepted")
	}
	if err := runInstallEntryInspect(io.Discard, []string{missing}); err == nil {
		t.Fatal("missing inspected entry accepted")
	}
	if err := runInstallRegularSnapshot(io.Discard, []string{missing}); err == nil {
		t.Fatal("missing regular snapshot entry accepted")
	}
	if err := runInstallRemoveAs([]string{missing, "0:0", installGenerationKindRegular, strings.Repeat("0", 64)}); err == nil {
		t.Fatal("missing removal entry accepted")
	}

	regular := filepath.Join(t.TempDir(), "regular")
	writeInstallHelperFile(t, regular, []byte("reviewed\n"))
	id, _, proof := inspectInstallHelperEntry(t, regular)
	for name, args := range map[string][]string{
		"wrong identity": {regular, "0:0", installGenerationKindRegular, proof},
		"wrong kind":     {regular, id, installGenerationKindSymlink, proof},
		"wrong proof":    {regular, id, installGenerationKindRegular, strings.Repeat("0", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runInstallRemoveAs(args); err == nil {
				t.Fatal("malformed removal authorization accepted")
			}
		})
	}
	directory := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	directoryID, _, _ := inspectInstallHelperEntry(t, directory)
	if err := runInstallRemoveAs([]string{directory, directoryID, "directory", "invalid"}); err == nil {
		t.Fatal("invalid directory proof accepted")
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	if err := runInstallEntryIdentity(io.Discard, []string{fifo}); err == nil {
		t.Fatal("FIFO identity accepted")
	}
	if err := runInstallEntryInspect(io.Discard, []string{fifo}); err == nil {
		t.Fatal("FIFO inspection accepted")
	}
	if err := runInstallRegularSnapshot(io.Discard, []string{fifo}); err == nil {
		t.Fatal("FIFO regular snapshot accepted")
	}
	if err := runInstallRemoveAs([]string{fifo, installHelperPathIdentity(t, fifo), installGenerationKindRegular, strings.Repeat("0", 64)}); err == nil {
		t.Fatal("FIFO removal accepted")
	}
}

func TestJournalHelperRejectsMalformedActionsWithoutChangingEvidence(t *testing.T) {
	fixture := newInstallHelperFixture(t)
	before, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := installHelperDigest(before)
	for _, test := range []struct {
		name   string
		action []string
	}{
		{"validate-arity", []string{"validate", "extra"}},
		{"validate-as-arity", []string{"validate-as"}},
		{"backup-arity", []string{"backup-prior", "extra"}},
		{"backup-as-arity", []string{"backup-prior-as"}},
		{"matches-arity", []string{"matches"}},
		{"matches-key", []string{"matches", "unknown", "prior", fixture.commandPath}},
		{"matches-path", []string{"matches", "metadata", "prior", filepath.Join(t.TempDir(), "foreign")}},
		{"remove-arity", []string{"remove-matched"}},
		{"update-arity", []string{"update-from"}},
		{"update-identity", []string{"update-from", "invalid", "metadata", "produced", "regular", fixture.metadata}},
		{"commit-arity", []string{"commit"}},
		{"commit-mode", []string{"commit", "invalid", "identity"}},
		{"commit-identity", []string{"commit", "normal", "invalid"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := runInstallJournalHelper(&output, append([]string{fixture.journalPath, digest}, test.action...)); err == nil {
				t.Fatal("malformed action accepted")
			}
			if output.Len() != 0 {
				t.Fatalf("failed action emitted success data: %q", output.String())
			}
			after, err := os.ReadFile(fixture.journalPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed helper changed journal: %v", err)
			}
		})
	}
}
