// SPDX-License-Identifier: Apache-2.0

package apicheck_test

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestPublicPackageSet(t *testing.T) {
	root := moduleRoot(t)
	got, unexpected, err := publicPackageDirs(root, publicPackageNames, nonPublicTopLevelDirectories)
	if err != nil {
		t.Fatal(err)
	}
	if len(unexpected) > 0 {
		t.Fatalf("top-level directories must be a public package or an explicit non-package exclusion: %v", unexpected)
	}
	if !reflect.DeepEqual(got, publicPackageNames) {
		t.Fatalf("public package set mismatch:\n got: %v\nwant: %v", got, publicPackageNames)
	}
}

func TestPublicPackageSetFindsNestedPackagesAndUnknownDirectories(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"alpha/nested", "promotion", "mystery", ".agents.local", ".pnpm-store", ".scannerwork"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "alpha", "nested", "nested.go"), []byte("package nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A package that exists only below the top level must be allowlistable by its
	// real path: the same list has to satisfy the walk and the directory scan.
	publicNames := []string{"alpha/nested"}
	got, unexpected, err := publicPackageDirs(root, publicNames, map[string]bool{"promotion": true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, publicNames) {
		t.Fatalf("nested package set mismatch: got %v, want %v", got, publicNames)
	}
	if want := []string{"mystery"}; !reflect.DeepEqual(unexpected, want) {
		t.Fatalf("unexpected directory mismatch: got %v, want %v", unexpected, want)
	}
}

// publicPackageNames is the exact set of importable package paths below the
// module root, relative and slash-separated. Entries may be nested; a
// directory counts as accounted for when it heads at least one entry here.
var publicPackageNames = []string{
	"backup",
	"clierror",
	"diagnostic",
	"discover",
	"doctor",
	"knowledgebase",
	"layout",
	"project",
	"provider",
	"prune",
	"redact",
	"restore",
	"review",
	"safefs",
	"safepath",
	"scan",
	"skill",
	"stage",
	"walk",
}

// nonPublicTopLevelDirectories names tracked directories that hold no importable
// package. Dot-prefixed directories are skipped wholesale and must not be listed
// here: they are ignored or generated, so enumerating them only goes stale.
var nonPublicTopLevelDirectories = map[string]bool{
	"cmd":          true,
	"dist":         true,
	"docs":         true,
	"graphify-out": true,
	"internal":     true,
	"node_modules": true,
	"promotion":    true,
	"reviews":      true,
	"scripts":      true,
	"testdata":     true,
}

func publicPackageDirs(root string, publicNames []string, excluded map[string]bool) ([]string, []string, error) {
	// Both halves of the contract resolve against publicNames: the walk reports
	// package paths, and a top-level directory is accounted for when it heads one
	// of those paths. Keying them separately makes a nested-only public package
	// impossible to allowlist.
	publicHeads := make(map[string]bool, len(publicNames))
	for _, name := range publicNames {
		publicHeads[strings.SplitN(name, "/", 2)[0]] = true
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	var unexpected []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && !publicHeads[entry.Name()] && !excluded[entry.Name()] {
			unexpected = append(unexpected, entry.Name())
		}
	}

	packages := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel == "." {
				return nil
			}
			top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
			if excluded[top] || entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			packages[filepath.ToSlash(filepath.Dir(rel))] = true
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	got := make([]string, 0, len(packages))
	for path := range packages {
		got = append(got, path)
	}
	sort.Strings(got)
	sort.Strings(unexpected)
	return got, unexpected, nil
}
