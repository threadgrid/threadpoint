// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicWriteRootFileRecoversExchangeCrashResidue(t *testing.T) {
	if !RenameRootExchangeSupported() {
		t.Skip("atomic rooted exchange is unsupported on this platform")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	crash := errors.New("simulated crash after exchange")
	atomicRootAfterPublishExchange = func(*os.Root, string, string) error { return crash }
	err = AtomicWriteRootFile(root, "AGENTS.md", []byte("new\n"), 0o644, nil)
	atomicRootAfterPublishExchange = nil
	if !errors.Is(err, crash) {
		t.Fatalf("exchange crash error = %v", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "new\n" {
		t.Fatalf("canonical target was unavailable after exchange crash: body=%q err=%v", body, err)
	}
	if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 1 {
		t.Fatalf("exchange crash artifacts = %#v, want displaced prior generation", artifacts)
	}

	validated := false
	err = AtomicWriteRootFile(root, "AGENTS.md", []byte("newest\n"), 0o644, func(parent *os.Root, base string) error {
		body, err := parent.ReadFile(base)
		validated = err == nil && string(body) == "new\n"
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validated {
		t.Fatal("recovery did not preserve the exchanged canonical generation")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "newest\n" {
		t.Fatalf("recovered canonical body=%q err=%v", body, err)
	}
	if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
		t.Fatalf("exchange recovery left artifacts: %#v", artifacts)
	}
}

func TestListRootPublishArtifactsUsesBoundedBatches(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{
		"unrelated-a", "unrelated-b",
		".AGENTS.md.publish-0123456789abcdef01234567",
		".AGENTS.md.tmp-abcdef0123456789abcdef01",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := listRootPublishArtifactsBounded(root, "AGENTS.md", 2, 1<<20); err == nil || !strings.Contains(err.Error(), "directory budget") {
		t.Fatalf("bounded artifact scan error = %v", err)
	}
	oldNames, tmpNames, err := listRootPublishArtifactsBounded(root, "AGENTS.md", 8, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldNames) != 1 || len(tmpNames) != 1 {
		t.Fatalf("publication artifacts = old %#v temp %#v", oldNames, tmpNames)
	}
}

func TestAtomicWriteRootFileRecoversJournaledMissingCanonicalCrash(t *testing.T) {
	if !RenameRootNoReplaceSupported() || !RenameRootExchangeSupported() {
		t.Skip("rooted rename primitives are unsupported on this platform")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	oldName := ".AGENTS.md.publish-" + strings.Repeat("a", 24)
	tmpName := ".AGENTS.md.tmp-" + strings.Repeat("b", 24)
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tmpName), []byte("interrupted-new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, filepath.Join(dir, oldName)); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	validated := false
	err = AtomicWriteRootFile(root, "AGENTS.md", []byte("recovered-new\n"), 0o644, func(parent *os.Root, base string) error {
		body, err := parent.ReadFile(base)
		validated = err == nil && string(body) == "old\n"
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validated {
		t.Fatal("journaled crash recovery did not restore the exact prior canonical generation before validation")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "recovered-new\n" {
		t.Fatalf("journaled recovered canonical body=%q err=%v", body, err)
	}
	if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
		t.Fatalf("journaled recovery left artifacts: %#v", artifacts)
	}
}

func TestAtomicWriteRootFileJournalFallbackRecoversDetachedCanonical(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	originalExchange := exchangeRootOperation
	exchangeRootOperation = func(*os.Root, string, string) error { return ErrRenameExchangeUnsupported }
	t.Cleanup(func() {
		exchangeRootOperation = originalExchange
		atomicRootAfterPublishDetach = nil
	})
	crash := errors.New("simulated crash after durable canonical detach")
	atomicRootAfterPublishDetach = func(*os.Root, string, string) error { return crash }
	err = AtomicWriteRootFile(root, "AGENTS.md", []byte("interrupted new\n"), 0o640, nil)
	atomicRootAfterPublishDetach = nil
	if !errors.Is(err, crash) {
		t.Fatalf("journal detach crash error = %v", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("simulated fallback crash state kept canonical entry: %v", err)
	}
	if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 1 || !strings.Contains(artifacts[0], ".publish-") {
		t.Fatalf("journal crash artifacts = %#v, want exact prior generation", artifacts)
	}

	validated := false
	err = AtomicWriteRootFile(root, "AGENTS.md", []byte("recovered new\n"), 0o640, func(parent *os.Root, base string) error {
		body, err := parent.ReadFile(base)
		validated = err == nil && string(body) == "old\n"
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validated {
		t.Fatal("fallback recovery did not restore the prior canonical generation before validation")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "recovered new\n" {
		t.Fatalf("journal recovered canonical body=%q err=%v", body, err)
	}
	if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
		t.Fatalf("journal recovery left artifacts: %#v", artifacts)
	}
}

func TestAtomicWriteRootFileJournalFallbackRollsBackPostValidation(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	originalExchange := exchangeRootOperation
	exchangeRootOperation = func(*os.Root, string, string) error { return ErrRenameExchangeUnsupported }
	t.Cleanup(func() { exchangeRootOperation = originalExchange })
	validationErr := errors.New("post-publication source changed")
	err = AtomicWriteRootFileUsingUmaskTransaction(root, "AGENTS.md", []byte("stale new\n"), 0o666, 0o777, nil, func(*os.Root, string) error {
		return validationErr
	})
	if !errors.Is(err, validationErr) {
		t.Fatalf("journal post-validation error = %v", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "old\n" {
		t.Fatalf("journal rollback canonical body=%q err=%v", body, err)
	}
	if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
		t.Fatalf("journal rollback left artifacts: %#v", artifacts)
	}
}

func TestAtomicWriteRootFileTransactionRollsBackFailedPostValidation(t *testing.T) {
	if !RenameRootExchangeSupported() {
		t.Skip("atomic rooted exchange is unsupported on this platform")
	}
	validationErr := errors.New("post-publication source changed")
	for _, test := range []struct {
		name       string
		old        string
		wantExists bool
	}{
		{name: "existing target", old: "old\n", wantExists: true},
		{name: "missing target", wantExists: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "AGENTS.md")
			if test.wantExists {
				if err := os.WriteFile(target, []byte(test.old), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			err = AtomicWriteRootFileUsingUmaskTransaction(root, "AGENTS.md", []byte("stale proposed\n"), 0o666, 0o777, nil, func(*os.Root, string) error {
				return validationErr
			})
			if !errors.Is(err, validationErr) {
				t.Fatalf("transaction validation error = %v", err)
			}
			body, readErr := os.ReadFile(target)
			if test.wantExists {
				if readErr != nil || string(body) != test.old {
					t.Fatalf("prior canonical generation was not restored: body=%q err=%v", body, readErr)
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("new canonical target was not rolled back to missing: body=%q err=%v", body, readErr)
			}
			if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
				t.Fatalf("post-validation rollback left artifacts: %#v", artifacts)
			}
		})
	}
}

func TestAtomicWriteRootFileTransactionDoesNotFailAfterDurableFinalize(t *testing.T) {
	if !RenameRootExchangeSupported() {
		t.Skip("atomic rooted exchange is unsupported on this platform")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	finalized := 0
	var displacedName string
	atomicRootAfterPublishExchange = func(_ *os.Root, _ string, displaced string) error {
		displacedName = displaced
		return nil
	}
	t.Cleanup(func() { atomicRootAfterPublishExchange = nil })
	err = AtomicWriteRootFileUsingUmaskTransaction(root, "AGENTS.md", []byte("committed\n"), 0o666, 0o777, nil, func(parent *os.Root, _ string) error {
		finalized++
		// Substitute the private displaced-target name only after the external
		// finalizer has committed. Cleanup must refuse this replacement without
		// changing the transaction's successful result.
		displaced := displacedName
		if err := parent.Remove(displaced); err != nil {
			return err
		}
		file, err := parent.OpenFile(displaced, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteString("replacement residue\n")
		return errors.Join(writeErr, file.Close())
	})
	atomicRootAfterPublishExchange = nil
	if err != nil {
		t.Fatalf("post-finalize cleanup changed committed success into failure: %v", err)
	}
	if finalized != 1 {
		t.Fatalf("finalizer calls=%d, want 1", finalized)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "committed\n" {
		t.Fatalf("committed canonical generation changed: body=%q err=%v", body, err)
	}
	if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 1 {
		t.Fatalf("failed exact cleanup did not retain one recoverable artifact: %#v", artifacts)
	}
	if err := AtomicWriteRootFile(root, "AGENTS.md", []byte("next\n"), 0o640, nil); err != nil {
		t.Fatalf("recover post-finalize residue: %v", err)
	}
	if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
		t.Fatalf("next transaction did not reconcile residue: %#v", artifacts)
	}
}

func TestAtomicWriteRootFileTransactionRefusesChangedRollbackGeneration(t *testing.T) {
	if !RenameRootExchangeSupported() {
		t.Skip("atomic rooted exchange is unsupported on this platform")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	atomicRootAfterPublishExchange = func(parent *os.Root, _ string, displaced string) error {
		if err := parent.Remove(displaced); err != nil {
			return err
		}
		file, err := parent.OpenFile(displaced, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteString("untrusted replacement\n")
		return errors.Join(writeErr, file.Close())
	}
	t.Cleanup(func() { atomicRootAfterPublishExchange = nil })
	finalized := false
	err = AtomicWriteRootFileUsingUmaskTransaction(root, "AGENTS.md", []byte("proposed\n"), 0o666, 0o777, nil, func(*os.Root, string) error {
		finalized = true
		return errors.New("must not run")
	})
	atomicRootAfterPublishExchange = nil
	if err == nil || !strings.Contains(err.Error(), "prior canonical generation changed before transaction finalization") {
		t.Fatalf("changed rollback generation error = %v", err)
	}
	if finalized {
		t.Fatal("transaction finalized after its exact rollback generation changed")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "proposed\n" {
		t.Fatalf("untrusted rollback generation became canonical: body=%q err=%v", body, err)
	}
}

func TestRestoreRootFileTransactionReconcilesBoundRestartStates(t *testing.T) {
	t.Run("published canonical restores exact prior", func(t *testing.T) {
		if !RenameRootExchangeSupported() {
			t.Skip("atomic rooted exchange is unsupported on this platform")
		}
		dir := t.TempDir()
		target := filepath.Join(dir, "AGENTS.md")
		if err := os.WriteFile(target, []byte("prior\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		crash := errors.New("simulated process loss")
		atomicRootAfterPublishExchange = func(*os.Root, string, string) error { return crash }
		err = AtomicWriteRootFileUsingUmaskTransaction(root, "AGENTS.md", []byte("published\n"), 0o666, 0o777, nil, func(*os.Root, string) error { return nil })
		atomicRootAfterPublishExchange = nil
		if !errors.Is(err, crash) {
			t.Fatalf("crash setup error = %v", err)
		}
		prior := RootFileGeneration{Exists: true, Body: []byte("prior\n"), Mode: 0o640}
		if err := RestoreRootFileTransaction(root, "AGENTS.md",
			RootFileGeneration{Exists: true, Body: []byte("published\n")}, prior); err != nil {
			t.Fatal(err)
		}
		if body, err := os.ReadFile(target); err != nil || string(body) != "prior\n" {
			t.Fatalf("restored canonical body=%q err=%v", body, err)
		}
		if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("restored canonical mode=%v err=%v", info, err)
		}
		if artifacts := rootPublishArtifacts(t, dir); len(artifacts) == 0 {
			t.Fatal("restore removed residue before the durable transaction tombstone")
		}
		if err := CleanupRootFileTransaction(root, "AGENTS.md", prior); err != nil {
			t.Fatal(err)
		}
		if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
			t.Fatalf("transaction cleanup left residue: %#v", artifacts)
		}
	})

	t.Run("missing fallback canonical restores sealed prior", func(t *testing.T) {
		if !RenameRootNoReplaceSupported() {
			t.Skip("atomic rooted no-replace rename is unsupported on this platform")
		}
		dir := t.TempDir()
		target := filepath.Join(dir, "AGENTS.md")
		if err := os.WriteFile(target, []byte("prior\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		originalExchange := exchangeRootOperation
		exchangeRootOperation = func(*os.Root, string, string) error { return ErrRenameExchangeUnsupported }
		crash := errors.New("simulated fallback detach crash")
		atomicRootAfterPublishDetach = func(*os.Root, string, string) error { return crash }
		t.Cleanup(func() {
			exchangeRootOperation = originalExchange
			atomicRootAfterPublishDetach = nil
		})
		err = AtomicWriteRootFileUsingUmaskTransaction(root, "AGENTS.md", []byte("published\n"), 0o666, 0o777, nil, func(*os.Root, string) error { return nil })
		atomicRootAfterPublishDetach = nil
		if !errors.Is(err, crash) {
			t.Fatalf("fallback crash setup error = %v", err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fallback crash canonical state = %v", err)
		}
		prior := RootFileGeneration{Exists: true, Body: []byte("prior\n"), Mode: 0o600}
		if err := RestoreRootFileTransaction(root, "AGENTS.md",
			RootFileGeneration{Exists: true, Body: []byte("published\n")}, prior); err != nil {
			t.Fatal(err)
		}
		if body, err := os.ReadFile(target); err != nil || string(body) != "prior\n" {
			t.Fatalf("fallback restored canonical body=%q err=%v", body, err)
		}
		if artifacts := rootPublishArtifacts(t, dir); len(artifacts) == 0 {
			t.Fatal("fallback restore removed residue before transaction tombstone")
		}
		if err := CleanupRootFileTransaction(root, "AGENTS.md", prior); err != nil {
			t.Fatal(err)
		}
		if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
			t.Fatalf("fallback cleanup left residue: %#v", artifacts)
		}
	})

	t.Run("already prior is idempotent and discards residue", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("prior\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".AGENTS.md.tmp-"+strings.Repeat("a", 24)), []byte("unrelated residue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		prior := RootFileGeneration{Exists: true, Body: []byte("prior\n"), Mode: 0o600}
		if err := RestoreRootFileTransaction(root, "AGENTS.md",
			RootFileGeneration{Exists: true, Body: []byte("published\n")}, prior); err != nil {
			t.Fatal(err)
		}
		if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 1 {
			t.Fatalf("idempotent restore changed residue before tombstone: %#v", artifacts)
		}
		if err := CleanupRootFileTransaction(root, "AGENTS.md", prior); err != nil {
			t.Fatal(err)
		}
		if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
			t.Fatalf("idempotent cleanup left residue: %#v", artifacts)
		}
	})

	t.Run("neither generation fails closed", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "AGENTS.md")
		if err := os.WriteFile(target, []byte("unrelated\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		residue := ".AGENTS.md.tmp-" + strings.Repeat("b", 24)
		if err := os.WriteFile(filepath.Join(dir, residue), []byte("retain me\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		err = RestoreRootFileTransaction(root, "AGENTS.md",
			RootFileGeneration{Exists: true, Body: []byte("published\n")},
			RootFileGeneration{Exists: true, Body: []byte("prior\n"), Mode: 0o600})
		if err == nil || !strings.Contains(err.Error(), "neither") {
			t.Fatalf("unrelated canonical recovery error = %v", err)
		}
		if body, err := os.ReadFile(target); err != nil || string(body) != "unrelated\n" {
			t.Fatalf("unrelated canonical changed: body=%q err=%v", body, err)
		}
		if body, err := os.ReadFile(filepath.Join(dir, residue)); err != nil || string(body) != "retain me\n" {
			t.Fatalf("unrelated residue changed: body=%q err=%v", body, err)
		}
	})

	t.Run("prior absence removes only exact published canonical", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "AGENTS.md")
		if err := os.WriteFile(target, []byte("published\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".AGENTS.md.publish-"+strings.Repeat("c", 24)), []byte("residue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		prior := RootFileGeneration{}
		if err := RestoreRootFileTransaction(root, "AGENTS.md",
			RootFileGeneration{Exists: true, Body: []byte("published\n")}, prior); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prior-absent recovery left canonical: %v", err)
		}
		if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 1 {
			t.Fatalf("prior-absent restore changed residue before tombstone: %#v", artifacts)
		}
		if err := CleanupRootFileTransaction(root, "AGENTS.md", prior); err != nil {
			t.Fatal(err)
		}
		if artifacts := rootPublishArtifacts(t, dir); len(artifacts) != 0 {
			t.Fatalf("prior-absent cleanup left residue: %#v", artifacts)
		}
	})
}

func TestRootTransactionAPIsRejectUntrustedTargets(t *testing.T) {
	if err := AtomicWriteRootFileUsingUmaskTransaction(nil, "target", []byte("body"), 0o666, 0o777, nil, nil); err == nil {
		t.Fatal("nil rooted transaction target was accepted")
	}
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := AtomicWriteRootFileUsingUmaskTransaction(root, "../target", []byte("body"), 0o666, 0o777, nil, nil); err == nil {
		t.Fatal("escaping rooted transaction target was accepted")
	}
	if err := root.WriteFile("ancestor", []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteRootFileUsingUmaskTransaction(root, "ancestor/target", []byte("body"), 0o666, 0o777, nil, nil); err == nil {
		t.Fatal("regular-file ancestor was accepted")
	}
	if err := root.Symlink(".", "linked"); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := AtomicWriteRootFileUsingUmaskTransaction(root, "linked/target", []byte("body"), 0o666, 0o777, nil, nil); err == nil {
		t.Fatal("symlink ancestor was accepted")
	}
	if err := root.Mkdir("directory-target", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteRootFileUsingUmaskTransaction(root, "directory-target", []byte("body"), 0o666, 0o777, nil, nil); err == nil {
		t.Fatal("directory canonical target was accepted")
	}

	validationErr := errors.New("pre-publication validation failed")
	if err := AtomicWriteRootFileUsingUmaskTransaction(root, "validated", []byte("body"), 0o666, 0o777,
		func(*os.Root, string) error { return validationErr }, nil); !errors.Is(err, validationErr) {
		t.Fatalf("validation error = %v", err)
	}
	if _, err := root.Lstat("validated"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-validation failure published target: %v", err)
	}
}

func rootPublishArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	const base = "AGENTS.md"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+base+".tmp-") || strings.HasPrefix(entry.Name(), "."+base+".publish-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestRootPublicationRejectsRacesWithoutRemovingThirdPartyFiles(t *testing.T) {
	for _, mechanism := range []string{"exchange", "journal"} {
		for _, race := range []string{"prior-changed", "prior-removed", "canonical-replaced", "prior-replaced"} {
			t.Run(mechanism+"/"+race, func(t *testing.T) {
				if !RenameRootNoReplaceSupported() || (mechanism == "exchange" && !RenameRootExchangeSupported()) {
					t.Skip("required rename unavailable")
				}
				dir := t.TempDir()
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				if err := os.WriteFile(filepath.Join(dir, "target"), []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
				oldExchange, oldBefore, oldAfterExchange, oldAfterDetach := exchangeRootOperation, atomicRootBeforePublishRename, atomicRootAfterPublishExchange, atomicRootAfterPublishDetach
				t.Cleanup(func() {
					exchangeRootOperation, atomicRootBeforePublishRename, atomicRootAfterPublishExchange, atomicRootAfterPublishDetach = oldExchange, oldBefore, oldAfterExchange, oldAfterDetach
				})
				if mechanism == "journal" {
					exchangeRootOperation = func(*os.Root, string, string) error { return ErrRenameExchangeUnsupported }
				}
				retained := "target"
				if race == "prior-changed" || race == "prior-removed" {
					atomicRootBeforePublishRename = func(parent *os.Root, base string) {
						if race == "prior-removed" {
							if err := parent.Remove(base); err != nil {
								t.Fatal(err)
							}
							return
						}
						if err := os.WriteFile(filepath.Join(parent.Name(), base), []byte("third party"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					mutate := func(parent *os.Root, base, prior string) error {
						retained = base
						if race == "prior-replaced" {
							retained = prior
						}
						if err := parent.Remove(retained); err != nil && !errors.Is(err, os.ErrNotExist) {
							return err
						}
						return os.WriteFile(filepath.Join(parent.Name(), retained), []byte("third party"), 0o600)
					}
					if mechanism == "exchange" {
						atomicRootAfterPublishExchange = mutate
					} else {
						atomicRootAfterPublishDetach = mutate
					}
				}
				err = AtomicWriteRootFileUsingUmaskTransaction(root, "target", []byte("replacement"), 0o600, 0o700, nil, func(*os.Root, string) error { return nil })
				if err == nil {
					t.Fatal("changed transaction accepted")
				}
				if race == "prior-removed" {
					if _, err := root.Lstat("target"); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("removed prior was recreated: %v", err)
					}
				} else if body, err := os.ReadFile(filepath.Join(dir, retained)); err != nil || string(body) != "third party" {
					t.Fatalf("third-party generation lost at %s: %q, %v", retained, body, err)
				}
			})
		}
	}
}

func TestRootPublicationSyncFailuresPreserveCompleteRecoverableGeneration(t *testing.T) {
	for _, mechanism := range []string{"exchange", "journal", "create"} {
		t.Run(mechanism, func(t *testing.T) {
			if !RenameRootNoReplaceSupported() {
				t.Skip("native no-replace rename unavailable")
			}
			originalSync, originalExchange := syncFileLockParent, exchangeRootOperation
			t.Cleanup(func() { syncFileLockParent = originalSync; exchangeRootOperation = originalExchange })
			if mechanism == "journal" {
				exchangeRootOperation = func(*os.Root, string, string) error { return ErrRenameExchangeUnsupported }
			}
			for stop := 1; stop <= 12; stop++ {
				dir := t.TempDir()
				target := filepath.Join(dir, "target")
				if mechanism != "create" {
					if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				sentinel := errors.New("directory sync failed")
				calls := 0
				syncFileLockParent = func(parent *os.Root) error {
					calls++
					if calls == stop {
						return sentinel
					}
					return originalSync(parent)
				}
				err = AtomicWriteRootFileUsingUmaskTransaction(root, "target", []byte("replacement"), 0o600, 0o700, nil, func(*os.Root, string) error { return nil })
				syncFileLockParent = originalSync
				_ = root.Close()
				if calls >= stop && !errors.Is(err, sentinel) {
					// Sync after finalized prior-generation cleanup is best effort; the
					// committed replacement remains authoritative even if that cleanup fails.
					if err != nil {
						t.Fatalf("sync failure lost: %v", err)
					}
				} else if calls < stop && err != nil {
					t.Fatal(err)
				}
				body, readErr := os.ReadFile(target)
				if mechanism == "create" && errors.Is(readErr, os.ErrNotExist) {
				} else if readErr != nil || (string(body) != "original" && string(body) != "replacement") {
					t.Fatalf("publication lost complete generation: %q %v", body, readErr)
				}
				if err == nil && (readErr != nil || string(body) != "replacement") {
					t.Fatalf("successful publication not visible: %q %v", body, readErr)
				}
				if calls < stop {
					break
				}
				if stop == 12 {
					t.Fatal("sync sweep did not reach success")
				}
			}
		})
	}
}

func TestRestoreTransactionCanRetryAfterEveryDirectorySyncFailure(t *testing.T) {
	originalSync, originalExchange := syncFileLockParent, exchangeRootOperation
	t.Cleanup(func() { syncFileLockParent = originalSync; exchangeRootOperation = originalExchange })
	for _, mechanism := range []string{"exchange", "journal", "remove-created", "restore-missing"} {
		t.Run(mechanism, func(t *testing.T) {
			exchangeRootOperation = originalExchange
			if mechanism == "journal" {
				exchangeRootOperation = func(*os.Root, string, string) error { return ErrRenameExchangeUnsupported }
			}
			for stop := 1; stop <= 12; stop++ {
				dir := t.TempDir()
				path := filepath.Join(dir, "target")
				published := RootFileGeneration{Exists: true, Body: []byte("published"), Mode: 0o600}
				prior := RootFileGeneration{Exists: true, Body: []byte("prior"), Mode: 0o600}
				if mechanism == "remove-created" {
					prior = RootFileGeneration{}
				}
				if mechanism != "restore-missing" {
					if err := os.WriteFile(path, published.Body, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				sentinel := errors.New("recovery sync failed")
				syncFileLockParent = func(parent *os.Root) error {
					calls++
					if calls == stop {
						return sentinel
					}
					return originalSync(parent)
				}
				err = RestoreRootFileTransaction(root, "target", published, prior)
				syncFileLockParent = originalSync
				if calls >= stop && !errors.Is(err, sentinel) {
					_ = root.Close()
					t.Fatalf("sync %d error lost: %v", stop, err)
				}
				if calls < stop && err != nil {
					_ = root.Close()
					t.Fatal(err)
				}
				if err := RestoreRootFileTransaction(root, "target", published, prior); err != nil {
					_ = root.Close()
					t.Fatalf("recovery retry failed: %v", err)
				}
				if err := CleanupRootFileTransaction(root, "target", prior); err != nil {
					_ = root.Close()
					t.Fatal(err)
				}
				_ = root.Close()
				body, readErr := os.ReadFile(path)
				if !prior.Exists {
					if !errors.Is(readErr, os.ErrNotExist) {
						t.Fatalf("created file survived rollback: %v", readErr)
					}
				} else if readErr != nil || string(body) != "prior" {
					t.Fatalf("prior generation was not restored: %q %v", body, readErr)
				}
				if calls < stop {
					break
				}
				if stop == 12 {
					t.Fatal("sync sweep did not finish")
				}
			}
		})
	}
}

func TestPublicationRecoveryRefusesAmbiguousOrInaccessibleEvidence(t *testing.T) {
	for _, failure := range []string{"closed-root", "canonical-directory", "multiple-priors", "prior-directory", "prior-symlink", "prior-unreadable", "unwritable-parent"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			prior := ".memory.publish-" + strings.Repeat("a", 24)
			if err := root.WriteFile(prior, []byte("prior"), 0o600); err != nil {
				t.Fatal(err)
			}
			retained := prior
			switch failure {
			case "closed-root":
				_ = root.Close()
			case "canonical-directory":
				if err := root.Mkdir("memory", 0o700); err != nil {
					t.Fatal(err)
				}
			case "multiple-priors":
				if err := root.WriteFile(".memory.publish-"+strings.Repeat("b", 24), []byte("second"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "prior-directory", "prior-symlink":
				retained = "retained"
				if err := root.Rename(prior, retained); err != nil {
					t.Fatal(err)
				}
				if failure == "prior-directory" {
					if err := root.Mkdir(prior, 0o700); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := root.Symlink(retained, prior); err != nil {
						t.Fatal(err)
					}
				}
			case "prior-unreadable":
				if err := root.Chmod(prior, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(filepath.Join(dir, prior), 0o600) }()
			case "unwritable-parent":
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(dir, 0o700) }()
			}
			if err := recoverRootPublishArtifacts(root, "memory"); err == nil {
				t.Fatal("ambiguous recovery succeeded")
			}
			if failure == "prior-unreadable" {
				if err := os.Chmod(filepath.Join(dir, prior), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			body, err := os.ReadFile(filepath.Join(dir, retained))
			if err != nil || string(body) != "prior" {
				t.Fatalf("prior evidence lost: %q %v", body, err)
			}
		})
	}
}

func TestPublicationArtifactCleanupRejectsUnsafeEntries(t *testing.T) {
	for _, operation := range []string{"remove", "discard"} {
		for _, failure := range []string{"closed-root", "directory", "symlink", "unreadable", "unwritable-parent"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				name := ".memory.tmp-" + strings.Repeat("a", 24)
				if err := root.WriteFile(name, []byte("evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
				retained := name
				switch failure {
				case "closed-root":
					_ = root.Close()
				case "directory", "symlink":
					retained = "retained"
					if err := root.Rename(name, retained); err != nil {
						t.Fatal(err)
					}
					if failure == "directory" {
						if err := root.Mkdir(name, 0o700); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := root.Symlink(retained, name); err != nil {
							t.Fatal(err)
						}
					}
				case "unreadable":
					if err := root.Chmod(name, 0); err != nil {
						t.Fatal(err)
					}
					defer func() { _ = os.Chmod(filepath.Join(dir, name), 0o600) }()
				case "unwritable-parent":
					if err := os.Chmod(dir, 0o500); err != nil {
						t.Fatal(err)
					}
					defer func() { _ = os.Chmod(dir, 0o700) }()
				}
				if operation == "remove" {
					err = removeRootPublishArtifact(root, name)
				} else {
					err = discardRootPublishArtifacts(root, "memory")
				}
				if err == nil {
					t.Fatal("unsafe artifact removal succeeded")
				}
				if failure == "unreadable" {
					if err := os.Chmod(filepath.Join(dir, name), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				body, err := os.ReadFile(filepath.Join(dir, retained))
				if err != nil || string(body) != "evidence" {
					t.Fatalf("unowned evidence removed: %q %v", body, err)
				}
			})
		}
	}
}

func TestRecoveryGenerationPreparationBoundsNameCollisions(t *testing.T) {
	for _, failure := range []string{"collisions", "closed-root", "unwritable-root"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.WriteFile("memory", []byte("canonical"), 0o600); err != nil {
				t.Fatal(err)
			}
			original := rand.Reader
			defer func() { rand.Reader = original }()
			rand.Reader = bytes.NewReader(bytes.Repeat([]byte{0x2a}, 4096))
			name := ".memory.tmp-" + strings.Repeat("2a", 12)
			switch failure {
			case "collisions":
				if err := root.WriteFile(name, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "closed-root":
				_ = root.Close()
			case "unwritable-root":
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(dir, 0o700) }()
			}
			if err := writeRootTransactionGeneration(root, "memory", []byte("recovered"), 0o600, nil); err == nil {
				t.Fatal("unavailable recovery temporary accepted")
			}
			body, err := os.ReadFile(filepath.Join(dir, "memory"))
			if err != nil || string(body) != "canonical" {
				t.Fatalf("recovery changed canonical generation: %q %v", body, err)
			}
			if failure == "collisions" {
				body, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(body) != "foreign" {
					t.Fatalf("collision overwrote foreign entry: %q %v", body, err)
				}
			}
		})
	}
}
