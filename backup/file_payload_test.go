// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupBytesFromRootKeepsCallerPinnedHomeAfterPathReplacement(t *testing.T) {
	project := t.TempDir()
	parent := t.TempDir()
	productHome := filepath.Join(parent, "product-home")
	if err := os.Mkdir(productHome, 0o700); err != nil {
		t.Fatal(err)
	}
	pinned, err := os.OpenRoot(productHome)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	store, err := NewStore(StoreOptions{ProjectRoot: project, ThreadpointHome: productHome, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	moved := productHome + ".moved"
	if err := os.Rename(productHome, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(productHome, 0o700); err != nil {
		t.Fatal(err)
	}
	entry, err := store.BackupBytesWithModeFromRoot(pinned, productHome, "CLAUDE.md", []byte("reviewed\n"), "stage-source", 0o640)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(productHome, entry.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(moved, rel))
	if err != nil || string(body) != "reviewed\n" {
		t.Fatalf("pinned backup payload = %q, err=%v", body, err)
	}
	if entries, err := os.ReadDir(productHome); err != nil || len(entries) != 0 {
		t.Fatalf("replacement product home was mutated: entries=%v err=%v", entries, err)
	}
}

func TestBackupFileReplacesEntryForSameRoleAndPath(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	now := time.Date(2026, 5, 30, 1, 2, 3, 0, time.UTC)
	source := filepath.Join(root, "AGENTS.md")
	mustWriteFile(t, source, "v1")

	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	first, err := store.BackupFile("AGENTS.md", source, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, source, "v2")
	second, err := store.BackupFile("AGENTS.md", source, "fixture-target")
	if err != nil {
		t.Fatal(err)
	}
	if first.BackupPath == second.BackupPath {
		t.Fatalf("replacement reused mutable payload path %q", first.BackupPath)
	}
	if body, err := os.ReadFile(first.BackupPath); err != nil || string(body) != "v1" {
		t.Fatalf("replacement destroyed the prior payload, body=%q err=%v", body, err)
	}
	if got := len(store.Manifest.Entries); got != 1 {
		t.Fatalf("same role+path should replace, expected 1 entry, got %d", got)
	}
	if _, err := store.BackupFile("AGENTS.md", source, "fixture-secondary"); err != nil {
		t.Fatal(err)
	}
	if got := len(store.Manifest.Entries); got != 2 {
		t.Fatalf("a different role is a distinct entry, expected 2, got %d", got)
	}
}

func TestBackupFileUsesUniquePayloadPathsForSanitizedCollisions(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	upper := filepath.Join(root, "upper", "source.md")
	lower := filepath.Join(root, "lower", "source.md")
	mustWriteFile(t, upper, "upper")
	mustWriteFile(t, lower, "lower")

	store, err := NewStore(StoreOptions{ProjectRoot: root, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	first, err := store.BackupFile("Review.md", upper, "stage-source")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.BackupFile("review.md", lower, "stage-source")
	if err != nil {
		t.Fatal(err)
	}
	if first.BackupPath == second.BackupPath {
		t.Fatalf("distinct paths collided on backup payload %s", first.BackupPath)
	}
	for path, want := range map[string]string{
		first.BackupPath:  "upper",
		second.BackupPath: "lower",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Fatalf("backup %s = %q, want %q", path, body, want)
		}
	}
}

func TestCopyFileCreatesParentAndPreservesContent(t *testing.T) {
	root := t.TempDir()

	source := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(source, []byte("guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	copyTarget := filepath.Join(t.TempDir(), "nested", "copy.md")
	if err := CopyFile(source, copyTarget); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(copyTarget); err != nil || string(body) != "guide\n" {
		t.Fatalf("copied file = %q, %v", body, err)
	}
}

func TestBackupFileFromRootRecordsRegularFilesAndSymlinkMarkers(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	store, err := NewStore(StoreOptions{ProjectRoot: project, HomeDir: home, Operation: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	source := filepath.Join(project, "source.txt")
	if err := os.WriteFile(source, []byte("source bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o640); err != nil {
		t.Fatal(err)
	}
	entry, err := store.BackupFileFromRoot(FileSpec{Rel: "config/source.txt", Source: source, Role: "fixture", Root: project})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Root != project || entry.Mode != 0o640 || !entry.ModeKnown || entry.Size != int64(len("source bytes")) {
		t.Fatalf("regular backup entry = %#v", entry)
	}
	if body, err := os.ReadFile(entry.BackupPath); err != nil || string(body) != "source bytes" {
		t.Fatalf("regular backup payload = %q, %v", body, err)
	}

	link := filepath.Join(project, "source-link")
	if err := os.Symlink("source.txt", link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	linkEntry, err := store.BackupFileFromRoot(FileSpec{Rel: "config/source-link", Source: link, Role: "fixture"})
	if err != nil || !linkEntry.Symlink || !strings.HasSuffix(linkEntry.BackupPath, ".symlink") {
		t.Fatalf("symlink backup = %#v, %v", linkEntry, err)
	}
	if body, err := os.ReadFile(linkEntry.BackupPath); err != nil || string(body) != "source.txt\n" {
		t.Fatalf("symlink marker = %q, %v", body, err)
	}
	if _, err := store.BackupFileFromRoot(FileSpec{Rel: "config", Source: project, Role: "fixture"}); err == nil || !strings.Contains(err.Error(), "cannot back up directory") {
		t.Fatalf("directory backup error = %v", err)
	}
}

func TestRootPayloadCopyAndDigestRejectStaleSourceMetadata(t *testing.T) {
	for _, operation := range []string{"copy", "digest"} {
		for _, change := range []string{"replace", "remove", "directory", "symlink", "grow", "shrink", "budget", "closed-root"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				source, err := os.OpenRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer source.Close()
				target, err := os.OpenRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer target.Close()
				if err := source.WriteFile("source", []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
				expected, err := source.Lstat("source")
				if err != nil {
					t.Fatal(err)
				}
				allowance := int64(64)
				switch change {
				case "replace":
					if err := source.Rename("source", "retained"); err != nil {
						t.Fatal(err)
					}
					err = source.WriteFile("source", []byte("replacement"), 0o600)
				case "remove":
					err = source.Remove("source")
				case "directory":
					if err := source.Remove("source"); err != nil {
						t.Fatal(err)
					}
					err = source.Mkdir("source", 0o700)
				case "symlink":
					if err := source.Remove("source"); err != nil {
						t.Fatal(err)
					}
					err = source.Symlink("elsewhere", "source")
				case "grow":
					err = source.WriteFile("source", []byte("longer source generation"), 0o600)
				case "shrink":
					err = source.WriteFile("source", []byte("x"), 0o600)
				case "budget":
					allowance = 2
				case "closed-root":
					err = source.Close()
				}
				if err != nil {
					t.Fatal(err)
				}
				if operation == "copy" {
					_, err = copyRootRegularFile(source, "source", expected, target, "payload", allowance)
				} else {
					_, _, err = hashRootRegularFile(source, "source", expected, allowance)
				}
				if err == nil {
					t.Fatal("stale source accepted")
				}
				if _, err := target.Lstat("payload"); !os.IsNotExist(err) {
					t.Fatalf("failed copy retained partial payload: %v", err)
				}
			})
		}
	}
}

func TestContentAddressedPayloadPublicationVerifiesExistingBytesAndMode(t *testing.T) {
	for _, existing := range []string{"missing", "matching", "different", "oversized", "public-mode", "directory", "symlink", "closed-root", "outside"} {
		t.Run(existing, func(t *testing.T) {
			home := t.TempDir()
			root, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			target := filepath.Join(home, "payload")
			body := []byte("sealed")
			switch existing {
			case "missing":
			case "matching":
				err = os.WriteFile(target, body, 0o600)
			case "different":
				err = os.WriteFile(target, []byte("change"), 0o600)
			case "oversized":
				err = os.WriteFile(target, []byte("unexpected larger body"), 0o600)
			case "public-mode":
				err = os.WriteFile(target, body, 0o644)
			case "directory":
				err = os.Mkdir(target, 0o700)
			case "symlink":
				err = os.Symlink("elsewhere", target)
			case "closed-root":
				err = root.Close()
			case "outside":
				target = filepath.Join(t.TempDir(), "payload")
			}
			if err != nil {
				t.Fatal(err)
			}
			err = publishBackupRegularPayloadRoot(root, home, target, body)
			if existing == "missing" || existing == "matching" {
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(target)
				if err != nil || string(got) != string(body) {
					t.Fatalf("payload=%q %v", got, err)
				}
			} else if err == nil {
				t.Fatal("invalid content-addressed publication accepted")
			}
			if existing == "different" {
				got, err := os.ReadFile(target)
				if err != nil || string(got) != "change" {
					t.Fatalf("existing payload overwritten: %q %v", got, err)
				}
			}
		})
	}
}
