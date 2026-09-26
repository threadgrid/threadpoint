// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallBackupCleanupRefusesUnavailableOrOverBudgetTree(t *testing.T) {
	for _, failure := range []string{"nil-root", "nil-budget", "closed-root", "depth", "entries", "nested-budget", "unreadable-child", "unwritable-child", "symlink"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "child"), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "child", "evidence")
			if err := os.WriteFile(path, []byte("prior"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			selected := root
			remaining, depth := maxInstallTransactionCleanupEntries, 0
			budget := &remaining
			switch failure {
			case "nil-root":
				selected = nil
			case "nil-budget":
				budget = nil
			case "closed-root":
				_ = root.Close()
			case "depth":
				depth = maxInstallTransactionCleanupDepth + 1
			case "entries":
				remaining = 0
			case "nested-budget":
				remaining = 1
			case "unreadable-child":
				if err := root.Chmod("child", 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(filepath.Join(dir, "child"), 0o700) }()
			case "unwritable-child":
				if err := root.Chmod("child", 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(filepath.Join(dir, "child"), 0o700) }()
			case "symlink":
				outside := t.TempDir()
				if err := os.Rename(filepath.Join(dir, "child"), filepath.Join(outside, "retained")); err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink(filepath.Join(outside, "retained"), "child"); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(outside, "retained", "evidence")
			}
			if err := removePinnedInstallTransactionContentsBounded(selected, depth, budget); err == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			if failure == "unreadable-child" {
				if err := os.Chmod(filepath.Join(dir, "child"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			body, err := os.ReadFile(path)
			if err != nil || string(body) != "prior" {
				t.Fatalf("cleanup lost backup evidence: %q %v", body, err)
			}
		})
	}
}

func TestInstallBackupEntryCleanupRequiresExactIdentityAndEmptyDirectory(t *testing.T) {
	for _, failure := range []string{"nil-root", "nil-identity", "unsafe-name", "closed-root", "wrong-identity", "name-collision", "unwritable-parent", "nonempty-directory", "unreadable-directory"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			name := "entry"
			directory := failure == "nonempty-directory" || failure == "unreadable-directory"
			if directory {
				if err := root.Mkdir(name, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := root.WriteFile(filepath.Join(name, "evidence"), []byte("prior"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := root.WriteFile(name, []byte("prior"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := root.Lstat(name)
			if err != nil {
				t.Fatal(err)
			}
			selected := root
			original := rand.Reader
			defer func() { rand.Reader = original }()
			switch failure {
			case "nil-root":
				selected = nil
			case "nil-identity":
				info = nil
			case "unsafe-name":
				name = "../entry"
			case "closed-root":
				_ = root.Close()
			case "wrong-identity":
				info, err = root.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
			case "name-collision":
				rand.Reader = bytes.NewReader(bytes.Repeat([]byte{0x2a}, 4096))
				if err := root.WriteFile(".backup-cleanup-"+strings.Repeat("2a", 12), []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unwritable-parent":
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(dir, 0o700) }()
			case "unreadable-directory":
				if err := root.Chmod(name, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := removePinnedInstallBackupEntryExact(selected, name, info); err == nil {
				t.Fatal("unsafe exact cleanup succeeded")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				path := filepath.Join(dir, entry.Name())
				if entry.IsDir() {
					if err := os.Chmod(path, 0o700); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(path, "evidence")
				}
				body, err := os.ReadFile(path)
				if err == nil && string(body) == "prior" {
					found = true
				}
			}
			if !found {
				t.Fatal("failed cleanup lost exact prior evidence")
			}
		})
	}
}

func TestRecoveryCleanupHelperRefusesInvalidNamespaceAndOwnership(t *testing.T) {
	for _, failure := range []string{"arity", "identity-format", "wrong-identity", "missing-parent", "namespace", "missing-root", "file-root", "symlink-root", "unreadable-root"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".bundle-cleanup-fixture")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			evidence := filepath.Join(path, "evidence")
			if err := os.WriteFile(evidence, []byte("prior"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := installRemovalIdentityString(info)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{path, identity}
			switch failure {
			case "arity":
				args = args[:1]
			case "identity-format":
				args[1] = "invalid"
			case "wrong-identity":
				info, err := os.Stat(dir)
				if err != nil {
					t.Fatal(err)
				}
				args[1], err = installRemovalIdentityString(info)
				if err != nil {
					t.Fatal(err)
				}
			case "missing-parent":
				args[0] = filepath.Join(dir, "missing", ".bundle-cleanup-fixture")
			case "namespace":
				args[0] = filepath.Join(dir, ".bundle-cleanup-")
			case "missing-root", "file-root", "symlink-root":
				if err := os.Rename(path, path+".retained"); err != nil {
					t.Fatal(err)
				}
				evidence = filepath.Join(path+".retained", "evidence")
				if failure == "file-root" {
					if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "symlink-root" {
					if err := os.Symlink(path+".retained", path); err != nil {
						t.Fatal(err)
					}
				}
			case "unreadable-root":
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(path, 0o700) }()
			}
			if err := runInstallRecoveryCleanup(args); err == nil {
				t.Fatal("invalid recovery cleanup accepted")
			}
			if failure == "unreadable-root" {
				if err := os.Chmod(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			body, err := os.ReadFile(evidence)
			if err != nil || string(body) != "prior" {
				t.Fatalf("cleanup lost retained evidence: %q %v", body, err)
			}
		})
	}
}

func TestBackupRemovalRefusesInvalidPhysicalNamespace(t *testing.T) {
	for _, failure := range []string{"outside", "transactions-file", "transactions-symlink", "transactions-unreadable", "backup-file", "backup-symlink", "backup-unreadable"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, true)
			lock := fixture.acquire(t)
			transactions := filepath.Join(lock.lockParentPath, "transactions")
			path := filepath.Join(transactions, ".bundle-fixture")
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			changed := path
			if strings.HasPrefix(failure, "transactions-") {
				changed = transactions
			}
			switch failure {
			case "outside":
				path = filepath.Join(t.TempDir(), ".bundle-fixture")
			case "transactions-file", "backup-file", "transactions-symlink", "backup-symlink":
				if err := os.Rename(changed, changed+".retained"); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(failure, "file") {
					if err := os.WriteFile(changed, []byte("foreign"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(changed+".retained", changed); err != nil {
					t.Fatal(err)
				}
			case "transactions-unreadable", "backup-unreadable":
				if err := os.Chmod(changed, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(changed, 0o700) }()
			}
			if err := removeInstallTransactionBackupChecked(lock, path); err == nil {
				t.Fatal("invalid cleanup accepted")
			}
			if failure != "outside" {
				if _, err := os.Lstat(changed); err != nil {
					t.Fatalf("foreign namespace removed: %v", err)
				}
			}
		})
	}
}
