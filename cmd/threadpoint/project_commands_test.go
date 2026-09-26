// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/doctor"
	"github.com/threadgrid/threadpoint/layout"
)

func TestStatusInventoryUsesRootFlag(t *testing.T) {
	root := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "shared")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "architecture.md"), "# Architecture")

	stdout, _, err := runTestCLI(t, "status", "--root", root, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}

	var kb doctor.Report
	if err := json.Unmarshal([]byte(stdout), &kb); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if kb.Root != root {
		t.Fatalf("expected root %q, got %q", root, kb.Root)
	}
	if len(kb.Inventory.Records) == 0 {
		t.Fatalf("expected records, got %#v", kb)
	}
}

func TestStatusInventorySkipsConfiguredDirectories(t *testing.T) {
	root := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "shared")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "architecture.md"), "# Architecture")
	mustWrite(t, filepath.Join(root, ".agents", "knowledge", "generated", "ignored.md"), "# Ignored")

	stdout, _, err := runTestCLI(t, "status", "--root", root, "--skip-dirs", "Generated,other", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var kb doctor.Report
	if err := json.Unmarshal([]byte(stdout), &kb); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	for _, record := range kb.Inventory.Records {
		if strings.Contains(strings.Join(record.Paths, ","), "generated") {
			t.Fatalf("configured directory was mapped: %#v", record)
		}
	}
}

func TestStatusValidationReportsSuccessAndFailure(t *testing.T) {
	validRoot := t.TempDir()
	if err := layout.EnsureShared(validRoot); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runTestCLI(t, "status", "--root", validRoot, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var report doctor.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if !report.OK {
		t.Fatalf("expected valid report, got %#v", report)
	}

	invalidRoot := t.TempDir()
	stdout, _, err = runTestCLI(t, "status", "--root", invalidRoot, "--format", "json")
	if err == nil {
		t.Fatal("expected status to fail for missing layout")
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("failed status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if report.OK || len(report.Validation.Issues) == 0 {
		t.Fatalf("expected invalid report with issues, got %#v", report)
	}
}

func TestImplicitRootUsesCanonicalLocalMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(project, ".agents.local"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(project, "src")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	stdout, _, err := runTestCLI(t, "status", "--format", "json")
	if err == nil {
		t.Fatal("expected invalid shared layout")
	}
	var knowledgebase doctor.Report
	if err := json.Unmarshal([]byte(stdout), &knowledgebase); err != nil {
		t.Fatalf("status emitted invalid JSON: %v\n%s", err, stdout)
	}
	if knowledgebase.Root != project {
		t.Fatalf("status root = %q, want local marker root %q", knowledgebase.Root, project)
	}
}

func TestStatusRetiredCommandsAreUnknown(t *testing.T) {
	for _, command := range []string{"scan", "validate", "diagnose", "ls", "list"} {
		_, _, err := runTestCLI(t, command)
		if err == nil || exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("%s: %v", command, err)
		}
	}
}
