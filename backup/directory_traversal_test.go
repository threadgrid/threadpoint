// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBoundedDirectoryWalkStopsEnumerationAtBudgetPlusOne(t *testing.T) {
	directoryPath := t.TempDir()
	for index := 0; index < 10; index++ {
		mustWriteFile(t, filepath.Join(directoryPath, fmt.Sprintf("entry-%02d", index)), "")
	}
	directory, err := os.Open(directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if _, err := readRootDirectoryEntries(directory, 2, 1, "bounded test walk"); err == nil || !strings.Contains(err.Error(), "exceeds 2 filesystem entries") {
		t.Fatalf("bounded enumeration error = %v", err)
	}
	remaining, err := directory.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 8 {
		t.Fatalf("bounded enumeration consumed %d entries, want exactly budget+1 (2)", 10-len(remaining))
	}
}

func TestWalkRootDirectoryBoundedIsLexicographicAndCountsRoot(t *testing.T) {
	directoryPath := t.TempDir()
	mustWriteFile(t, filepath.Join(directoryPath, "z.md"), "z")
	mustWriteFile(t, filepath.Join(directoryPath, "a", "nested.md"), "nested")
	mustWriteFile(t, filepath.Join(directoryPath, "b.md"), "b")
	root, err := os.OpenRoot(directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var visited []string
	err = WalkRootDirectoryBounded(root, ".", 5, func(accessRoot *os.Root, accessName string, walkPath string, info fs.FileInfo) error {
		visited = append(visited, walkPath)
		if info.Mode().IsRegular() {
			body, err := ReadRootRegularFileBounded(accessRoot, accessName, 32)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("empty body for %s", walkPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".", "a", "a/nested.md", "b.md", "z.md"}
	if !reflect.DeepEqual(visited, want) {
		t.Fatalf("bounded walk order = %#v, want %#v", visited, want)
	}
	if err := WalkRootDirectoryBounded(root, ".", 4, func(*os.Root, string, string, fs.FileInfo) error { return nil }); err == nil || !strings.Contains(err.Error(), "exceeds 4 filesystem entries") {
		t.Fatalf("root-counting error = %v", err)
	}
}

func TestWalkRootDirectoryBindsFinalChildEnumeration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*os.Root) error
	}{
		{
			name: "addition",
			mutate: func(root *os.Root) error {
				file, err := root.OpenFile("late.md", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
				if err != nil {
					return err
				}
				return file.Close()
			},
		},
		{
			name: "replacement",
			mutate: func(root *os.Root) error {
				if err := root.Remove("original.md"); err != nil {
					return err
				}
				file, err := root.OpenFile("original.md", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
				if err != nil {
					return err
				}
				if _, err := file.WriteString("replacement"); err != nil {
					return errors.Join(err, file.Close())
				}
				return file.Close()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directoryPath := t.TempDir()
			mustWriteFile(t, filepath.Join(directoryPath, "original.md"), "original")
			root, err := os.OpenRoot(directoryPath)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			mutated := false
			err = walkRootDirectoryWithHooks(root, ".", RootWalkLimits{
				MaxEntries:       10,
				MaxDepth:         4,
				MaxPathBytes:     1024,
				MaxMetadataBytes: 4096,
			}, "test directory walk", rootWalkHooks{
				afterChildren: func(directory *os.Root, walkPath string) error {
					if walkPath != "." || mutated {
						return nil
					}
					mutated = true
					return test.mutate(directory)
				},
			}, func(*os.Root, string, string, fs.FileInfo) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "changed during test directory walk") {
				t.Fatalf("post-enumeration %s error = %v", test.name, err)
			}
			if !mutated {
				t.Fatal("post-children mutation hook was not called")
			}
		})
	}
}

func TestWalkRootDirectoryEnforcesStructuralBudgetsOnDirfdTree(t *testing.T) {
	directoryPath := t.TempDir()
	createDirfdTree(t, directoryPath, DefaultDirectoryMaxDepth+1)
	root, err := os.OpenRoot(directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := WalkRootDirectoryBounded(root, ".", DefaultDirectoryMaxDepth+2, func(*os.Root, string, string, fs.FileInfo) error { return nil }); err == nil || !strings.Contains(err.Error(), "exceeds maximum depth 128") {
		t.Fatalf("default depth error = %v", err)
	}

	shallowPath := t.TempDir()
	mustWriteFile(t, filepath.Join(shallowPath, "long-name.md"), "body")
	shallow, err := os.OpenRoot(shallowPath)
	if err != nil {
		t.Fatal(err)
	}
	defer shallow.Close()
	if err := WalkRootDirectoryWithLimits(shallow, ".", RootWalkLimits{
		MaxEntries:       4,
		MaxDepth:         2,
		MaxPathBytes:     5,
		MaxMetadataBytes: 4096,
	}, func(*os.Root, string, string, fs.FileInfo) error { return nil }); err == nil || !strings.Contains(err.Error(), "exceeds 5 bytes of emitted paths") {
		t.Fatalf("path budget error = %v", err)
	}
	if err := WalkRootDirectoryWithLimits(shallow, ".", RootWalkLimits{
		MaxEntries:       4,
		MaxDepth:         2,
		MaxPathBytes:     1024,
		MaxMetadataBytes: 130,
	}, func(*os.Root, string, string, fs.FileInfo) error { return nil }); err == nil || !strings.Contains(err.Error(), "exceeds 130 bytes of entry metadata") {
		t.Fatalf("metadata budget error = %v", err)
	}
}

func TestRootedReadAndWalkValidateInputsAndBounds(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(rootPath, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if _, err := ReadRootRegularFileBounded(nil, "file.txt", 5); err == nil {
		t.Fatal("nil rooted reader should fail")
	}
	for _, test := range []struct {
		name  string
		limit int64
		want  string
	}{
		{name: "negative", limit: -1, want: "negative"},
		{name: "outside root", limit: 5, want: "escapes rooted"},
		{name: "directory", limit: 5, want: "not a regular"},
		{name: "symlink", limit: 5, want: "unsafe symlink"},
		{name: "too small", limit: 4, want: "larger than"},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := "file.txt"
			switch test.name {
			case "outside root":
				name = "../outside"
			case "directory":
				name = "directory"
			case "symlink":
				name = "link"
			}
			if _, err := ReadRootRegularFileBounded(root, name, test.limit); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadRootRegularFileBounded(%q, %d) error = %v, want %q", name, test.limit, err, test.want)
			}
		})
	}
	body, err := ReadRootRegularFileBounded(root, "file.txt", 5)
	if err != nil || string(body) != "hello" {
		t.Fatalf("bounded read = %q, %v", body, err)
	}

	if err := WalkRootDirectoryWithLimits(nil, ".", RootWalkLimits{}, func(*os.Root, string, string, os.FileInfo) error { return nil }); err == nil {
		t.Fatal("nil rooted walk should fail")
	}
	if err := WalkRootDirectoryWithLimits(root, ".", RootWalkLimits{}, nil); err == nil {
		t.Fatal("nil rooted walk callback should fail")
	}
	if err := WalkRootDirectoryWithLimits(root, "../outside", RootWalkLimits{}, func(*os.Root, string, string, os.FileInfo) error { return nil }); err == nil {
		t.Fatal("walk outside root should fail")
	}
	if err := WalkRootDirectoryWithLimits(root, "file.txt", RootWalkLimits{}, func(*os.Root, string, string, os.FileInfo) error { return nil }); err == nil {
		t.Fatal("walk of regular file should fail")
	}
	if err := WalkRootDirectoryWithLimits(root, ".", RootWalkLimits{MaxEntries: 1}, func(*os.Root, string, string, os.FileInfo) error { return nil }); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("bounded walk error = %v", err)
	}

	var walked []string
	err = WalkRootDirectoryWithLimits(root, ".", RootWalkLimits{}, func(_ *os.Root, _ string, walkPath string, _ os.FileInfo) error {
		walked = append(walked, walkPath)
		return nil
	})
	if err != nil || len(walked) != 4 || walked[0] != "." {
		t.Fatalf("walked = %#v, err=%v", walked, err)
	}
}

func TestRootedDigestHelpersRejectInvalidRootsAndRespectRegularFileAllowance(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if _, err := RootDirSHA256WithLimits(nil, ".", DirectoryLimits{}); err == nil {
		t.Fatal("nil digest root should fail")
	}
	if _, err := RootDirSHA256WithLimits(root, "../outside", DirectoryLimits{}); err == nil {
		t.Fatal("digest outside root should fail")
	}
	if _, err := RootDirSHA256WithLimits(root, "file.txt", DirectoryLimits{}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("regular-file digest error = %v", err)
	}
	info, err := root.Lstat("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hashRootRegularFile(root, "file.txt", info, 4); err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("allowance error = %v", err)
	}
	if sum, size, err := hashRootRegularFile(root, "file.txt", info, 5); err != nil || sum == "" || size != 5 {
		t.Fatalf("rooted hash = %q, %d, %v", sum, size, err)
	}
}

func createDirfdTree(t *testing.T, directoryPath string, depth int) {
	t.Helper()
	current, err := os.OpenRoot(directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < depth; index++ {
		name := fmt.Sprintf("depth-%03d-%s", index, strings.Repeat("x", 48))
		if err := current.Mkdir(name, 0o755); err != nil {
			_ = current.Close()
			t.Fatal(err)
		}
		next, err := current.OpenRoot(name)
		if err != nil {
			_ = current.Close()
			t.Fatal(err)
		}
		if err := current.Close(); err != nil {
			_ = next.Close()
			t.Fatal(err)
		}
		current = next
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
}
