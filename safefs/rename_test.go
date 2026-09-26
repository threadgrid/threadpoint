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

func TestAtomicWriteRootFileUsesPinnedDirectoryAfterRename(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	moved := filepath.Join(base, "moved-project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(project, moved); err != nil {
		t.Skipf("platform does not permit renaming an opened root: %v", err)
	}
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteRootFile(root, "AGENTS.md", []byte("pinned\n"), 0o644, nil); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(moved, "AGENTS.md")); err != nil || string(body) != "pinned\n" {
		t.Fatalf("pinned root body=%q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(project, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("replacement pathname must remain untouched: %v", err)
	}
}

func TestAtomicWriteRootFileValidatesAtRenameBoundary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("planned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	wantErr := errors.New("target changed")
	err = AtomicWriteRootFile(root, "AGENTS.md", []byte("threadpoint\n"), 0o644, func(parent *os.Root, base string) error {
		if err := parent.WriteFile(base, []byte("concurrent\n"), 0o644); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected final validation error, got %v", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "concurrent\n" {
		t.Fatalf("validation failure should preserve concurrent content, body=%q err=%v", body, err)
	}
}

func TestRenameRootNoReplaceRenamesNestedEntryAtomically(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "old", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "new", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "old", "nested", "item"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := RenameRootNoReplace(root, "old/nested/item", "new/nested/item"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(project, "old", "nested", "item")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists after rename: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(project, "new", "nested", "item")); err != nil || string(body) != "original" {
		t.Fatalf("renamed body=%q err=%v", body, err)
	}
}

func TestRenamePinnedRootNoReplaceUsesExactOpenedParentsAfterPathSwap(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("native no-replace rename is unavailable")
	}
	project := t.TempDir()
	sourcePath := filepath.Join(project, "source")
	recoveryPath := filepath.Join(project, "recovery")
	movedRecoveryPath := filepath.Join(project, "reviewed-recovery")
	for _, path := range []string{sourcePath, recoveryPath} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "item"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	recoveryRoot, err := os.OpenRoot(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer recoveryRoot.Close()
	if err := os.Rename(recoveryPath, movedRecoveryPath); err != nil {
		t.Skipf("platform does not permit renaming an opened directory: %v", err)
	}
	if err := os.Mkdir(recoveryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RenamePinnedRootNoReplace(sourceRoot, "item", recoveryRoot, "original"); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(movedRecoveryPath, "original")); err != nil || string(body) != "reviewed\n" {
		t.Fatalf("pinned destination body=%q err=%v", body, err)
	}
	if entries, err := os.ReadDir(recoveryPath); err != nil || len(entries) != 0 {
		t.Fatalf("replacement destination was mutated: entries=%v err=%v", entries, err)
	}
}

func TestRenameRootNoReplaceAllowsSymlinkSourceAndPreservesExistingDestination(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, "target", filepath.Join(project, "source", "item"))
	if err := os.WriteFile(filepath.Join(project, "destination", "item"), []byte("concurrent"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	err = RenameRootNoReplace(root, "source/item", "destination/item")
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected atomic destination collision, got %v", err)
	}
	if target, err := os.Readlink(filepath.Join(project, "source", "item")); err != nil || target != "target" {
		t.Fatalf("source symlink changed, target=%q err=%v", target, err)
	}
	if body, err := os.ReadFile(filepath.Join(project, "destination", "item")); err != nil || string(body) != "concurrent" {
		t.Fatalf("destination changed, body=%q err=%v", body, err)
	}
}

func TestRenameRootNoReplacePreservesExistingDirectoryDestination(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "source", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "source", "child", "body"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	err = RenameRootNoReplace(root, "source", "destination")
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected atomic directory collision, got %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(project, "source", "child", "body")); err != nil || string(body) != "source" {
		t.Fatalf("source directory changed, body=%q err=%v", body, err)
	}
	entries, err := os.ReadDir(filepath.Join(project, "destination"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("existing destination directory changed: %#v", entries)
	}
}

func TestRenameRootNoReplaceSameEntryIsANonMutatingCapabilityProbe(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "item"), []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	err = RenameRootNoReplace(root, "item", "item")
	if err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatalf("same-entry capability probe failed: %v", err)
	}
	if body, err := root.ReadFile("item"); err != nil || string(body) != "unchanged" {
		t.Fatalf("same-entry capability probe mutated source, body=%q err=%v", body, err)
	}
}

func TestRenameRootNoReplaceRejectsSymlinkAncestors(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "item"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(project, "source"))
	if err := os.Mkdir(filepath.Join(project, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	err = RenameRootNoReplace(root, "source/item", "destination/item")
	if !errors.Is(err, ErrUnsafeSymlink) {
		t.Fatalf("expected symlink-ancestor refusal, got %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(outside, "item")); err != nil || string(body) != "outside" {
		t.Fatalf("outside source changed, body=%q err=%v", body, err)
	}
}

func TestRenameRootNoReplaceRacesDestinationCreationWithoutReplacement(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	for iteration := 0; iteration < 100; iteration++ {
		project := t.TempDir()
		if err := os.WriteFile(filepath.Join(project, "source"), []byte("source"), 0o644); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(project)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		renameErr := make(chan error, 1)
		createErr := make(chan error, 1)
		go func() {
			<-start
			renameErr <- RenameRootNoReplace(root, "source", "destination")
		}()
		go func() {
			<-start
			file, err := root.OpenFile("destination", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err == nil {
				_, writeErr := file.WriteString("concurrent")
				err = errors.Join(writeErr, file.Close())
			}
			createErr <- err
		}()
		close(start)
		renameResult := <-renameErr
		createResult := <-createErr

		switch {
		case renameResult == nil:
			if !errors.Is(createResult, os.ErrExist) {
				t.Fatalf("iteration %d: rename won but create error=%v", iteration, createResult)
			}
			if body, err := root.ReadFile("destination"); err != nil || string(body) != "source" {
				t.Fatalf("iteration %d: renamed body=%q err=%v", iteration, body, err)
			}
		case createResult == nil:
			if !errors.Is(renameResult, os.ErrExist) {
				t.Fatalf("iteration %d: create won but rename error=%v", iteration, renameResult)
			}
			if body, err := root.ReadFile("destination"); err != nil || string(body) != "concurrent" {
				t.Fatalf("iteration %d: concurrent body=%q err=%v", iteration, body, err)
			}
			if body, err := root.ReadFile("source"); err != nil || string(body) != "source" {
				t.Fatalf("iteration %d: source body=%q err=%v", iteration, body, err)
			}
		default:
			t.Fatalf("iteration %d: neither operation won: rename=%v create=%v", iteration, renameResult, createResult)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProbeRenameRootNoReplaceUsesAnAbsentDestinationAndCleansSource(t *testing.T) {
	project := t.TempDir()
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	wantErr := errors.New("runtime filesystem refuses no-replace rename")
	original := probeRenameRootNoReplaceOperation
	defer func() { probeRenameRootNoReplaceOperation = original }()
	var source string
	var destination string
	probeRenameRootNoReplaceOperation = func(probeRoot *os.Root, oldName string, newName string) error {
		source = oldName
		destination = newName
		if oldName == newName {
			t.Fatal("probe used the same entry as source and destination")
		}
		if _, err := probeRoot.Lstat(oldName); err != nil {
			t.Fatalf("probe source was not present: %v", err)
		}
		if _, err := probeRoot.Lstat(newName); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("probe destination was not absent: %v", err)
		}
		return wantErr
	}

	err = ProbeRenameRootNoReplace(root)
	if !errors.Is(err, wantErr) {
		t.Fatalf("probe error = %v, want %v", err, wantErr)
	}
	for _, name := range []string{source, destination} {
		if _, statErr := root.Lstat(name); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("probe residue %q was not cleaned: %v", name, statErr)
		}
	}
}

func TestProbeRenameRootNoReplaceRetainsChangedResidue(t *testing.T) {
	project := t.TempDir()
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	original := probeRenameRootNoReplaceOperation
	defer func() { probeRenameRootNoReplaceOperation = original }()
	var source string
	probeRenameRootNoReplaceOperation = func(probeRoot *os.Root, oldName string, _ string) error {
		source = oldName
		if err := probeRoot.Remove(oldName); err != nil {
			return err
		}
		if err := probeRoot.WriteFile(oldName, []byte("concurrent"), 0o600); err != nil {
			return err
		}
		return errors.New("injected probe failure")
	}

	err = ProbeRenameRootNoReplace(root)
	if err == nil || !strings.Contains(err.Error(), "retained") || !strings.Contains(err.Error(), source) {
		t.Fatalf("changed probe residue was not reported: %v", err)
	}
	if body, readErr := root.ReadFile(source); readErr != nil || string(body) != "concurrent" {
		t.Fatalf("changed probe residue was removed, body=%q err=%v", body, readErr)
	}
}

func TestProbeRenameRootNoReplaceRetainsConcurrentDestination(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	original := probeRenameRootNoReplaceOperation
	defer func() { probeRenameRootNoReplaceOperation = original }()
	var sourceName, destinationName string
	probeRenameRootNoReplaceOperation = func(probeRoot *os.Root, oldName string, newName string) error {
		sourceName, destinationName = oldName, newName
		if err := probeRoot.WriteFile(newName, []byte("concurrent destination"), 0o600); err != nil {
			t.Fatal(err)
		}
		return RenameRootNoReplace(probeRoot, oldName, newName)
	}
	err = ProbeRenameRootNoReplace(root)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("concurrent probe destination was not reported: %v", err)
	}
	if _, err := root.Lstat(sourceName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchanged probe source was not cleaned: %v", err)
	}
	if body, err := root.ReadFile(destinationName); err != nil || string(body) != "concurrent destination" {
		t.Fatalf("concurrent probe destination was removed, body=%q err=%v", body, err)
	}
}

func TestProbeRenameRootNoReplaceExercisesNativeOperation(t *testing.T) {
	if !RenameRootNoReplaceSupported() {
		t.Skip("atomic rooted no-replace rename is unsupported on this platform")
	}
	project := t.TempDir()
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := ProbeRenameRootNoReplace(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("successful probe left residue: %#v", entries)
	}
}

func TestPinnedRenameHelpersRejectMissingRootsAndNestedNames(t *testing.T) {
	if err := RenamePinnedRootNoReplace(nil, "source", nil, "destination"); err == nil {
		t.Fatal("nil pinned rename roots should fail")
	}
	for _, name := range []string{"", "../outside", "nested/file"} {
		if _, err := cleanPinnedRenameBase(name); err == nil {
			t.Fatalf("pinned rename base %q should fail", name)
		}
	}
}

func TestPinnedRenamePrimitivesRejectUnsafeShapes(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := RenameRootNoReplace(nil, "source", "target"); err == nil {
		t.Fatal("nil rename root was accepted")
	}
	for _, names := range [][2]string{{"../source", "target"}, {"source", "../target"}, {".", "target"}} {
		if err := RenameRootNoReplace(root, names[0], names[1]); err == nil {
			t.Fatalf("unsafe rename names were accepted: %q", names)
		}
	}
	if err := root.WriteFile("source", []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("target", []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameRootNoReplace(root, "source", "target"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("no-replace collision error = %v", err)
	}
	if body, err := root.ReadFile("target"); err != nil || !bytes.Equal(body, []byte("target")) {
		t.Fatalf("no-replace collision target = %q, err=%v", body, err)
	}
	if err := RenamePinnedRootNoReplace(nil, "source", root, "target"); err == nil {
		t.Fatal("nil pinned source root was accepted")
	}
	if err := RenamePinnedRootNoReplace(root, "../source", root, "new-target"); err == nil {
		t.Fatal("escaping pinned source name was accepted")
	}
	if err := root.Symlink("source", "source-link"); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := RenamePinnedRootNoReplace(root, "source-link", root, "new-target"); err != nil {
		t.Fatalf("rename of symlink object failed: %v", err)
	}
	if info, err := root.Lstat("new-target"); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pinned rename followed rather than moved symlink object: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "source")); err != nil {
		t.Fatalf("refused renames removed source: %v", err)
	}
}

func TestRootRenamesRejectInvalidEndpointsWithoutMovingEntries(t *testing.T) {
	for name, rename := range map[string]func(*os.Root, string, string) error{
		"exchange":          ExchangeRoot,
		"no-replace":        RenameRootNoReplace,
		"pinned-no-replace": func(root *os.Root, from, to string) error { return RenamePinnedRootNoReplace(root, from, root, to) },
	} {
		for _, damage := range []string{"nil-root", "closed-root", "empty-source", "empty-target", "escaping-source", "escaping-target", "missing-source-parent", "missing-target-parent", "file-source-parent", "file-target-parent"} {
			t.Run(name+"/"+damage, func(t *testing.T) {
				dir := t.TempDir()
				source := filepath.Join(dir, "source")
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
					t.Fatal(err)
				}
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				from, to := "source", "target"
				switch damage {
				case "nil-root":
					root = nil
				case "closed-root":
					if err := root.Close(); err != nil {
						t.Fatal(err)
					}
				case "empty-source":
					from = ""
				case "empty-target":
					to = ""
				case "escaping-source":
					from = "../source"
				case "escaping-target":
					to = "../target"
				case "missing-source-parent":
					from = "missing/source"
				case "missing-target-parent":
					to = "missing/target"
				case "file-source-parent":
					from = "source/child"
				case "file-target-parent":
					to = "target/child"
				}
				if err := rename(root, from, to); err == nil {
					t.Fatal("invalid rename accepted")
				}
				for path, want := range map[string]string{source: "source", target: "target"} {
					if body, err := os.ReadFile(path); err != nil || string(body) != want {
						t.Fatalf("rename changed %s: %q %v", path, body, err)
					}
				}
			})
		}
	}
}

func TestNoReplaceProbeRefusesUnavailableNamespaceAndInvalidResults(t *testing.T) {
	for _, failure := range []string{"nil-root", "closed-root", "unwritable-root", "source-collision", "destination-collision", "no-rename", "closed-during-rename"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			selected := root
			originalReader, originalRename := rand.Reader, probeRenameRootNoReplaceOperation
			defer func() { rand.Reader = originalReader; probeRenameRootNoReplaceOperation = originalRename }()
			rand.Reader = bytes.NewReader(bytes.Repeat([]byte{0x2a}, 4096))
			collision := ""
			switch failure {
			case "nil-root":
				selected = nil
			case "closed-root":
				_ = root.Close()
			case "unwritable-root":
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(dir, 0o700) }()
			case "source-collision", "destination-collision":
				kind := "source"
				if failure == "destination-collision" {
					kind = "destination"
				}
				collision = ".threadpoint-noreplace-probe-" + kind + "-" + strings.Repeat("2a", 12)
				if err := root.WriteFile(collision, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "no-rename":
				probeRenameRootNoReplaceOperation = func(*os.Root, string, string) error { return nil }
			case "closed-during-rename":
				probeRenameRootNoReplaceOperation = func(root *os.Root, _, _ string) error { return root.Close() }
			}
			if err := ProbeRenameRootNoReplace(selected); err == nil {
				t.Fatal("invalid no-replace probe succeeded")
			}
			if collision != "" {
				body, err := os.ReadFile(filepath.Join(dir, collision))
				if err != nil || string(body) != "foreign" {
					t.Fatalf("probe changed foreign entry: %q %v", body, err)
				}
			}
			if failure != "closed-during-rename" {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.Name() != collision {
						t.Fatalf("probe leaked temporary %s", entry.Name())
					}
				}
			}
		})
	}
}
