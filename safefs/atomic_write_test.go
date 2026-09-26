// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicWriteFileReplacesWholeFileAndCleansTemp(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "nested", "file.txt")

	if err := AtomicWriteFile(target, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(target, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "second" {
		t.Fatalf("expected replaced body, got %q", body)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file was not cleaned up: %s", entry.Name())
		}
	}
}

func TestMissingFileAtomicWritesAndDirectCreatePreserveExistingContent(t *testing.T) {
	root := t.TempDir()
	atomicTarget := filepath.Join(root, "nested", "atomic.txt")
	if err := AtomicWriteFileIfMissing(atomicTarget, []byte("first"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFileIfMissing(atomicTarget, []byte("second"), 0o640); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(atomicTarget); err != nil || string(body) != "first" {
		t.Fatalf("atomic missing-file body = %q, err=%v", body, err)
	}
	directTarget := filepath.Join(root, "direct.txt")
	if err := writeFileIfMissingDirect(directTarget, []byte("created"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileIfMissingDirect(directTarget, []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(directTarget); err != nil || string(body) != "created" {
		t.Fatalf("direct missing-file body = %q, err=%v", body, err)
	}
	if _, err := ResolveThreadpointHome(root); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicTempSubstitutionIsRetainedAndNeverPublished(t *testing.T) {
	tests := []struct {
		name         string
		targetExists bool
		invoke       func(string, string) error
	}{
		{
			name:         "write",
			targetExists: true,
			invoke: func(_ string, target string) error {
				return AtomicWriteFile(target, []byte("planned\n"), 0o644)
			},
		},
		{
			name:         "copy",
			targetExists: true,
			invoke: func(source string, target string) error {
				return AtomicCopyFile(source, target, 0o644)
			},
		},
		{
			name: "write-if-missing",
			invoke: func(_ string, target string) error {
				return AtomicWriteFileIfMissing(target, []byte("planned\n"), 0o644)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source")
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(source, []byte("source\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if test.targetExists {
				if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var racedTemp string
			var hookErr error
			originalHook := atomicTempBeforePublish
			atomicTempBeforePublish = func(path string) {
				racedTemp = path
				if err := os.Remove(path); err != nil {
					hookErr = err
					return
				}
				hookErr = os.WriteFile(path, []byte("raced\n"), 0o600)
			}
			defer func() { atomicTempBeforePublish = originalHook }()
			err := test.invoke(source, target)
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if err == nil || !strings.Contains(err.Error(), "temporary") {
				t.Fatalf("expected substituted temporary refusal, got %v", err)
			}
			if test.targetExists {
				if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "original\n" {
					t.Fatalf("target body=%q err=%v", body, readErr)
				}
			} else if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("substituted temporary was published: %v", statErr)
			}
			if body, readErr := os.ReadFile(racedTemp); readErr != nil || string(body) != "raced\n" {
				t.Fatalf("raced temporary was not retained, body=%q err=%v", body, readErr)
			}
		})
	}
}

func TestAtomicWriteRootFileRetainsSubstitutedTemporary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var racedName string
	var hookErr error
	originalHook := atomicRootTempBeforePublish
	atomicRootTempBeforePublish = func(parent *os.Root, name string) {
		racedName = name
		if err := parent.Remove(name); err != nil {
			hookErr = err
			return
		}
		hookErr = parent.WriteFile(name, []byte("raced\n"), 0o600)
	}
	defer func() { atomicRootTempBeforePublish = originalHook }()
	err = AtomicWriteRootFile(root, "target", []byte("planned\n"), 0o644, nil)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || !strings.Contains(err.Error(), "temporary") {
		t.Fatalf("expected substituted rooted temporary refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "original\n" {
		t.Fatalf("target body=%q err=%v", body, readErr)
	}
	if body, readErr := root.ReadFile(racedName); readErr != nil || string(body) != "raced\n" {
		t.Fatalf("raced rooted temporary was not retained, body=%q err=%v", body, readErr)
	}
}

func TestWriteFileIfMissingDirectRetainsReplacementOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target")
	injected := errors.New("stop after direct create")
	var hookErr error
	originalHook := writeFileIfMissingAfterCreate
	writeFileIfMissingAfterCreate = func(path string, _ *os.File) error {
		if err := os.Remove(path); err != nil {
			hookErr = err
			return injected
		}
		hookErr = os.WriteFile(path, []byte("replacement\n"), 0o600)
		return injected
	}
	defer func() { writeFileIfMissingAfterCreate = originalHook }()
	err := writeFileIfMissingDirect(path, []byte("planned\n"), 0o644)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("expected identity-bound cleanup refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(path); readErr != nil || string(body) != "replacement\n" {
		t.Fatalf("replacement body=%q err=%v", body, readErr)
	}
}

func TestWriteFileIfMissingDirectRejectsReplacementAtCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target")
	var hookErr error
	originalHook := writeFileIfMissingAfterCreate
	writeFileIfMissingAfterCreate = func(path string, _ *os.File) error {
		if err := os.Remove(path); err != nil {
			hookErr = err
			return err
		}
		hookErr = os.WriteFile(path, []byte("replacement\n"), 0o600)
		return hookErr
	}
	defer func() { writeFileIfMissingAfterCreate = originalHook }()
	err := writeFileIfMissingDirect(path, []byte("planned\n"), 0o644)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || !strings.Contains(err.Error(), "changed before completion") || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("expected direct-write identity refusal, got %v", err)
	}
	if body, readErr := os.ReadFile(path); readErr != nil || string(body) != "replacement\n" {
		t.Fatalf("replacement body=%q err=%v", body, readErr)
	}
}

func TestPublishRootTempFailsClosedOnRacedTarget(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("no-replace rename unsupported on this platform")
	}
	t.Run("competitor creates a missing target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "AGENTS.md")
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		atomicRootBeforePublishRename = func(*os.Root, string) {
			_ = os.WriteFile(target, []byte("competitor\n"), 0o644)
		}
		defer func() { atomicRootBeforePublishRename = nil }()
		if err := AtomicWriteRootFile(root, "AGENTS.md", []byte("mine\n"), 0o644, nil); err == nil {
			t.Fatal("expected fail-closed when a competitor created the target")
		}
		if body, _ := os.ReadFile(target); string(body) != "competitor\n" {
			t.Fatalf("competitor content was overwritten: %q", body)
		}
	})
	t.Run("competitor replaces an existing target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "AGENTS.md")
		if err := os.WriteFile(target, []byte("reviewed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		fired := false
		atomicRootBeforePublishRename = func(*os.Root, string) {
			if fired {
				return
			}
			fired = true
			_ = os.Remove(target)
			_ = os.WriteFile(target, []byte("competitor\n"), 0o644)
		}
		defer func() { atomicRootBeforePublishRename = nil }()
		if err := AtomicWriteRootFile(root, "AGENTS.md", []byte("mine\n"), 0o644, nil); err == nil {
			t.Fatal("expected fail-closed when a competitor replaced the target")
		}
		if body, _ := os.ReadFile(target); string(body) != "competitor\n" {
			t.Fatalf("competitor content was overwritten: %q", body)
		}
	})
}

func TestAtomicWriteRootFileCarriesPreValidationTargetIdentity(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("no-replace rename unsupported on this platform")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = AtomicWriteRootFile(root, "AGENTS.md", []byte("threadpoint\n"), 0o644, func(parent *os.Root, base string) error {
		if err := parent.Remove(base); err != nil {
			return err
		}
		return parent.WriteFile(base, []byte("replacement\n"), 0o644)
	})
	if err == nil {
		t.Fatal("expected a replacement created during validation to be rejected")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "replacement\n" {
		t.Fatalf("replacement was overwritten: body=%q err=%v", body, err)
	}
}

func TestAtomicWriteRootFileRejectsInPlaceMutationDuringValidation(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("no-replace rename unsupported on this platform")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = AtomicWriteRootFile(root, "AGENTS.md", []byte("threadpoint\n"), 0o644, func(parent *os.Root, base string) error {
		file, err := parent.OpenFile(base, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteAt([]byte("changed!\n"), 0)
		return errors.Join(writeErr, file.Close())
	})
	if err == nil {
		t.Fatal("expected an in-place mutation during validation to be rejected")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "changed!\n" {
		t.Fatalf("concurrent in-place edit was overwritten: body=%q err=%v", body, err)
	}
}

func TestAtomicWriteRootFileRejectsAncestorReplacedByInternalSymlinkAlias(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("directory symlink setup is platform-dependent on Windows")
	}
	project := t.TempDir()
	rules := filepath.Join(project, ".agents", "rules")
	rulesReal := filepath.Join(project, ".agents", "rules-real")
	if err := os.MkdirAll(rules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rules, rulesReal); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("rules-real", rules); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	err = AtomicWriteRootFile(root, ".agents/rules/security.md", []byte("updated\n"), 0o644, nil)
	if !errors.Is(err, ErrUnsafeSymlink) {
		t.Fatalf("expected internal symlink-alias refusal, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(rulesReal, "security.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("symlink-alias race must not publish target, got %v", statErr)
	}
}

func TestAtomicWriteFileRejectsSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside.txt")
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, target)

	if err := AtomicWriteFile(target, []byte("updated"), 0o644); !errors.Is(err, ErrUnsafeSymlink) {
		t.Fatalf("expected symlink refusal, got %v", err)
	}
	body, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "outside" {
		t.Fatalf("symlink destination was modified: %q", body)
	}
	if linkTarget, err := os.Readlink(target); err != nil || linkTarget != outside {
		t.Fatalf("expected symlink to remain unchanged, target=%q err=%v", linkTarget, err)
	}
}

func TestAtomicWriteFileRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	outsideDir := filepath.Join(root, "outside")
	linkDir := filepath.Join(root, "link")
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outsideDir, linkDir)

	target := filepath.Join(linkDir, "file.txt")
	if err := AtomicWriteFile(target, []byte("updated"), 0o644); !errors.Is(err, ErrUnsafeSymlink) {
		t.Fatalf("expected symlink-parent refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "file.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no write through symlink parent, got %v", err)
	}
}

func TestAtomicWriteHelpersRejectSymlinks(t *testing.T) {
	sourceRoot := t.TempDir()
	source := filepath.Join(sourceRoot, "source.txt")
	if err := os.WriteFile(source, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		call func(string) error
	}{
		{
			name: "write-if-missing",
			call: func(target string) error {
				return AtomicWriteFileIfMissing(target, []byte("updated"), 0o644)
			},
		},
		{
			name: "copy",
			call: func(target string) error {
				return AtomicCopyFile(source, target, 0o644)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name+"/target", func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(root, "outside.txt")
			target := filepath.Join(root, "target.txt")
			if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
				t.Fatal(err)
			}
			symlinkOrSkip(t, outside, target)
			if err := test.call(target); !errors.Is(err, ErrUnsafeSymlink) {
				t.Fatalf("expected symlink refusal, got %v", err)
			}
		})
		t.Run(test.name+"/parent", func(t *testing.T) {
			root := t.TempDir()
			outsideDir := filepath.Join(root, "outside")
			linkDir := filepath.Join(root, "link")
			if err := os.Mkdir(outsideDir, 0o755); err != nil {
				t.Fatal(err)
			}
			symlinkOrSkip(t, outsideDir, linkDir)
			if err := test.call(filepath.Join(linkDir, "file.txt")); !errors.Is(err, ErrUnsafeSymlink) {
				t.Fatalf("expected symlink-parent refusal, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(outsideDir, "file.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected no write through symlink parent, got %v", err)
			}
		})
	}
}

func TestAtomicWriteFileIfMissingUsesDirectCreateWhenHardLinksUnavailable(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "missing.txt")
	originalLinkFile := linkFile
	linkFile = func(string, string) error {
		return fmt.Errorf("operation not supported")
	}
	defer func() {
		linkFile = originalLinkFile
	}()

	if err := AtomicWriteFileIfMissing(target, []byte("created"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "created" {
		t.Fatalf("expected direct-created body, got %q", body)
	}
	if err := AtomicWriteFileIfMissing(target, []byte("updated"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "created" {
		t.Fatalf("if-missing write should not replace existing file, got %q", body)
	}
}

func TestAtomicWritersRejectFinalSymlinksAndKeepConcurrentMissingFileWinners(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := AtomicWriteRootFile(nil, "guide.md", []byte("guide"), 0o644, nil); err == nil {
		t.Fatal("nil rooted writer should fail")
	}
	if err := AtomicWriteRootFile(root, "../guide.md", []byte("guide"), 0o644, nil); err == nil {
		t.Fatal("escaping rooted writer should fail")
	}
	validationErr := errors.New("reviewed state no longer matches")
	if err := AtomicWriteRootFile(root, "guide.md", []byte("guide"), 0o644, func(*os.Root, string) error { return validationErr }); !errors.Is(err, validationErr) {
		t.Fatalf("rooted validation error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "guide.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed rooted validation published a file: %v", err)
	}

	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, writer := range []struct {
		name string
		call func(path string) error
	}{
		{name: "atomic", call: func(path string) error { return AtomicWriteFile(path, []byte("replacement"), 0o644) }},
		{name: "umask", call: func(path string) error { return AtomicWriteFileUsingUmask(path, []byte("replacement"), 0o666, 0o777) }},
	} {
		t.Run(writer.name, func(t *testing.T) {
			path := filepath.Join(dir, writer.name+"-link")
			if err := os.Symlink(outside, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := writer.call(path); !errors.Is(err, ErrUnsafeSymlink) {
				t.Fatalf("%s final symlink error = %v", writer.name, err)
			}
			if body, err := os.ReadFile(outside); err != nil || string(body) != "outside" {
				t.Fatalf("%s followed final symlink, body=%q err=%v", writer.name, body, err)
			}
		})
	}

	for _, writer := range []struct {
		name string
		call func(path string) error
	}{
		{name: "atomic", call: func(path string) error { return AtomicWriteFileIfMissing(path, []byte("planned"), 0o644) }},
		{name: "umask", call: func(path string) error {
			return AtomicWriteFileIfMissingUsingUmask(path, []byte("planned"), 0o666, 0o777)
		}},
	} {
		t.Run("concurrent winner "+writer.name, func(t *testing.T) {
			path := filepath.Join(dir, "winner-"+writer.name)
			originalLink := linkFile
			linkFile = func(_, destination string) error {
				if err := os.WriteFile(destination, []byte("winner"), 0o644); err != nil {
					return err
				}
				return os.ErrExist
			}
			defer func() { linkFile = originalLink }()
			if err := writer.call(path); err != nil {
				t.Fatal(err)
			}
			if body, err := os.ReadFile(path); err != nil || string(body) != "winner" {
				t.Fatalf("%s concurrent winner = %q, err=%v", writer.name, body, err)
			}
		})
	}
}

func TestRootFileOperationsRejectUntrustedPaths(t *testing.T) {
	generation := RootFileGeneration{Exists: true, Body: []byte("original"), Mode: 0o600}
	operations := map[string]func(*os.Root, string) error{
		"write": func(root *os.Root, name string) error {
			return AtomicWriteRootFile(root, name, []byte("replacement"), 0o600, nil)
		},
		"write-umask": func(root *os.Root, name string) error {
			return AtomicWriteRootFileUsingUmask(root, name, []byte("replacement"), 0o600, 0o700, nil)
		},
		"restore": func(root *os.Root, name string) error {
			return RestoreRootFileTransaction(root, name, generation, RootFileGeneration{})
		},
		"cleanup": func(root *os.Root, name string) error { return CleanupRootFileTransaction(root, name, generation) },
	}
	for operation, run := range operations {
		for _, damage := range []string{"nil-root", "closed-root", "empty-name", "escape", "symlink-parent", "file-parent", "symlink-target", "directory-target"} {
			t.Run(operation+"/"+damage, func(t *testing.T) {
				dir := t.TempDir()
				outside := t.TempDir()
				original := filepath.Join(outside, "target")
				if err := os.WriteFile(original, generation.Body, 0o600); err != nil {
					t.Fatal(err)
				}
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				name := "target"
				switch damage {
				case "nil-root":
					root = nil
				case "closed-root":
					if err := root.Close(); err != nil {
						t.Fatal(err)
					}
				case "empty-name":
					name = ""
				case "escape":
					name = "../target"
				case "symlink-parent":
					if err := os.Symlink(outside, filepath.Join(dir, "parent")); err != nil {
						t.Fatal(err)
					}
					name = "parent/target"
				case "file-parent":
					if err := os.WriteFile(filepath.Join(dir, "parent"), []byte("parent"), 0o600); err != nil {
						t.Fatal(err)
					}
					name = "parent/target"
				case "symlink-target":
					if err := os.Symlink(original, filepath.Join(dir, name)); err != nil {
						t.Fatal(err)
					}
				case "directory-target":
					if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := run(root, name); err == nil {
					t.Fatal("untrusted path accepted")
				}
				if body, err := os.ReadFile(original); err != nil || string(body) != "original" {
					t.Fatalf("outside file changed: %q, %v", body, err)
				}
			})
		}
	}
}

func TestDirectMissingWriteRetainsUnverifiableDescriptorResidue(t *testing.T) {
	for _, finalMode := range []bool{false, true} {
		t.Run(fmt.Sprint(finalMode), func(t *testing.T) {
			previous := writeFileIfMissingAfterCreate
			t.Cleanup(func() { writeFileIfMissingAfterCreate = previous })
			writeFileIfMissingAfterCreate = func(_ string, file *os.File) error { return file.Close() }
			path := filepath.Join(t.TempDir(), "target")
			if err := writeFileIfMissingDirectWithMode(path, []byte("body"), 0o600, finalMode); err == nil {
				t.Fatal("closed descriptor accepted")
			}
			if body, err := os.ReadFile(path); err != nil || len(body) != 0 {
				t.Fatalf("unverifiable creation should remain empty: %q, %v", body, err)
			}
		})
	}
}

func TestAtomicWritersRetainTargetsWhenTemporaryCannotBeCreated(t *testing.T) {
	for _, writer := range []struct {
		name  string
		write func(*os.Root, string, string) error
	}{
		{"replace", func(_ *os.Root, path, _ string) error { return AtomicWriteFile(path, []byte("new"), 0o600) }},
		{"replace with umask", func(_ *os.Root, path, _ string) error {
			return AtomicWriteFileUsingUmask(path, []byte("new"), 0o666, 0o777)
		}},
		{"create", func(_ *os.Root, path, _ string) error { return AtomicWriteFileIfMissing(path, []byte("new"), 0o600) }},
		{"create with umask", func(_ *os.Root, path, _ string) error {
			return AtomicWriteFileIfMissingUsingUmask(path, []byte("new"), 0o666, 0o777)
		}},
		{"copy", func(_ *os.Root, path, source string) error { return AtomicCopyFile(source, path, 0o600) }},
		{"rooted replace", func(root *os.Root, path, _ string) error {
			return AtomicWriteRootFile(root, filepath.Base(path), []byte("new"), 0o600, nil)
		}},
		{"rooted transaction", func(root *os.Root, path, _ string) error {
			return AtomicWriteRootFileUsingUmaskTransaction(root, filepath.Base(path), []byte("new"), 0o666, 0o777, nil, nil)
		}},
	} {
		t.Run(writer.name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			source := filepath.Join(dir, "source")
			if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The final component fits the filesystem limit, but leaves no room
			// for the private temporary suffix needed for atomic publication.
			target := filepath.Join(dir, strings.Repeat("x", 250))
			if err := writer.write(root, target, source); err == nil {
				t.Fatal("publication unexpectedly succeeded")
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed publication created target: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "source" {
				t.Fatalf("temporary residue: %v, %v", entries, err)
			}
			if body, err := os.ReadFile(source); err != nil || string(body) != "source" {
				t.Fatalf("source changed: %q, %v", body, err)
			}
		})
	}
}

func TestPrivateNamespaceAllocationBoundsCollisions(t *testing.T) {
	for _, allocator := range []struct {
		name     string
		allocate func(*os.Root) (string, error)
	}{

		{"exclusive", func(root *os.Root) (string, error) { return rootExclusiveTempName(root, "entry") }},
		{"journal", func(root *os.Root) (string, error) { return rootPublishName(root, "entry") }},
		{"lock-quarantine", func(root *os.Root) (string, error) { return fileLockQuarantineName(root, "entry") }},
		{"publication-cleanup", func(root *os.Root) (string, error) { return rootCleanupName(root, "entry") }},
	} {
		t.Run(allocator.name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			original := rand.Reader
			defer func() { rand.Reader = original }()
			reset := func() { rand.Reader = bytes.NewReader(bytes.Repeat([]byte{0x2a}, 4096)) }
			reset()
			name, err := allocator.allocate(root)
			if err != nil {
				t.Fatal(err)
			}
			marker := name
			if info, err := root.Lstat(name); err == nil && info.IsDir() {
				marker = filepath.Join(name, "sentinel")
			}
			if err := root.WriteFile(marker, []byte("retained"), 0o600); err != nil {
				t.Fatal(err)
			}
			reset()
			if got, err := allocator.allocate(root); err == nil || got != "" {
				t.Fatalf("colliding name accepted: %q %v", got, err)
			}
			if body, err := root.ReadFile(marker); err != nil || string(body) != "retained" {
				t.Fatalf("collision destroyed prior entry: %q %v", body, err)
			}
			if err := root.RemoveAll(name); err != nil {
				t.Fatal(err)
			}
			reset()
			if got, err := allocator.allocate(root); err != nil || got != name {
				t.Fatalf("allocation did not recover: %q %v", got, err)
			}
			_ = root.Close()
			reset()
			if _, err := allocator.allocate(root); err == nil {
				t.Fatal("closed namespace accepted")
			}
		})
	}
}

func TestAtomicWritersPreserveConcurrentTargetsBeforePublication(t *testing.T) {
	writers := []struct {
		name  string
		write func(string, []byte) error
	}{
		{"replace", func(path string, body []byte) error { return AtomicWriteFile(path, body, 0o600) }},
		{"replace-umask", func(path string, body []byte) error { return AtomicWriteFileUsingUmask(path, body, 0o600, 0o700) }},
		{"missing", func(path string, body []byte) error { return AtomicWriteFileIfMissing(path, body, 0o600) }},
		{"missing-umask", func(path string, body []byte) error {
			return AtomicWriteFileIfMissingUsingUmask(path, body, 0o600, 0o700)
		}},
	}
	for _, writer := range writers {
		for _, change := range []string{"symlink", "directory", "unwritable"} {
			t.Run(writer.name+"/"+change, func(t *testing.T) {
				dir := t.TempDir()
				target := filepath.Join(dir, "target")
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
				old := atomicTempBeforePublish
				defer func() { atomicTempBeforePublish = old }()
				defer func() { _ = os.Chmod(dir, 0o700) }()
				atomicTempBeforePublish = func(string) {
					var err error
					switch change {
					case "symlink":
						err = os.Symlink(outside, target)
					case "directory":
						err = os.Mkdir(target, 0o700)
					case "unwritable":
						err = os.Chmod(dir, 0o500)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				err := writer.write(target, []byte("new"))
				if change == "directory" && strings.HasPrefix(writer.name, "missing") {
					if err != nil {
						t.Fatalf("existing directory should be preserved: %v", err)
					}
				} else if err == nil {
					t.Fatal("unsafe publication accepted")
				}
				body, err := os.ReadFile(outside)
				if err != nil || string(body) != "foreign" {
					t.Fatalf("outside changed: %q %v", body, err)
				}
				if change == "directory" {
					info, err := os.Lstat(target)
					if err != nil || !info.IsDir() {
						t.Fatalf("concurrent directory changed: %v", err)
					}
				}
			})
		}
	}
}
