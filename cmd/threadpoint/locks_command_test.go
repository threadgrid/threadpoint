// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/safefs"
)

func TestLocksClearForceRemovesSelectedProjectLock(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	locks, err := safefs.AcquireLocks(home, []string{root}, "holder")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locks.Release() }()

	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = home
	err = app.run(context.Background(), []string{"--home", home, "lock", "clear", "--root", root, "--force", "--yes", "--format", "json"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"cleared": true`) || !strings.Contains(stdout.String(), `"forced": true`) {
		t.Fatalf("unexpected clear output: %s", stdout.String())
	}
	fresh, err := safefs.AcquireLocks(home, []string{root}, "fresh")
	if err != nil {
		t.Fatalf("expected lock to be removed, got %v", err)
	}
	_ = fresh.Release()
}

func TestLocksCommandsReportEmptyProjectsAndRejectUnsafeConfirmation(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = t.TempDir()

	if err := runLocks(context.Background(), app, []string{"clear", "--root", root}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "No threadpoint project lock was present") {
		t.Fatalf("empty lock output = %q", got)
	}
	stdout.Reset()
	if err := runLocksClear(app, []string{"--root", root, "--format", "json"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, `"cleared": false`) {
		t.Fatalf("empty lock JSON = %q", got)
	}
	for _, args := range [][]string{
		{"clear", "--yes"},
		{"clear", "--force"},
		{"unknown"},
		{},
	} {
		if err := runLocks(context.Background(), app, args); err == nil {
			t.Fatalf("runLocks(%v) unexpectedly succeeded", args)
		}
	}
}

func TestLockCleanupRejectsMalformedRequestsBeforeStateMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown-flag", []string{"--unknown"}},
		{"positional-argument", []string{"extra"}},
		{"unsupported-format", []string{"--format", "yaml"}},
		{"missing-root", []string{"--root", filepath.Join(t.TempDir(), "missing")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			var output, diagnostics bytes.Buffer
			app := newCLI(&output, &diagnostics, strings.NewReader(""))
			app.threadpointHome = home
			if err := runLocksClear(app, tc.args); err == nil {
				t.Fatal("invalid cleanup accepted")
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid cleanup changed state: %v %v", entries, err)
			}
		})
	}
}
