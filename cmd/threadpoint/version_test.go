// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionCommandTextAndJSONOutput(t *testing.T) {
	originalVersion := version
	originalCommit := commit
	originalDate := date
	defer func() {
		version = originalVersion
		commit = originalCommit
		date = originalDate
	}()
	version = "v9.8.7"
	commit = "abc123"
	date = "2026-06-07T00:00:00Z"

	stdout, stderr, err := runTestCLI(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
	for _, want := range []string{
		"command: threadpoint version",
		"version: v9.8.7",
		"commit: abc123",
		"build date: 2026-06-07T00:00:00Z",
		"go version:",
		"platform:",
		"install source: release-build",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("version text missing %q:\n%s", want, stdout)
		}
	}

	stdout, stderr, err = runTestCLI(t, "version", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
	var report versionReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("version JSON invalid: %v\n%s", err, stdout)
	}
	if report.Command != "threadpoint version" || report.Version != "v9.8.7" || report.Commit != "abc123" {
		t.Fatalf("unexpected version report: %#v", report)
	}
	if report.BuildDate != "2026-06-07T00:00:00Z" || report.InstallSource != "release-build" {
		t.Fatalf("unexpected version metadata: %#v", report)
	}
	if report.GoVersion == "" || report.GOOS == "" || report.GOARCH == "" {
		t.Fatalf("missing runtime metadata: %#v", report)
	}
}

func TestCurrentBinaryVersionUsesBuildVersion(t *testing.T) {
	if currentBinaryVersion() != buildVersion() {
		t.Fatal("current binary version should use the build version")
	}
}
