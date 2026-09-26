// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThreadpointDoesNotPersistLocalDiagnosticLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("THREADPOINT_LOG_LEVEL", "debug")

	stdout, stderr, err := runTestCLI(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
	if !strings.Contains(stdout, "command: threadpoint version") {
		t.Fatalf("version output missing command header:\n%s", stdout)
	}

	logDir := filepath.Join(home, ".threadpoint", "logs")
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("threadpoint should not create local log state, stat err=%v", err)
	}
}
