// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/doctor"
	"github.com/threadgrid/threadpoint/layout"
)

func TestToolDoctorReportHandlesOfflineChecksAndTextFormatting(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	home := t.TempDir()
	report := buildToolDoctorReport(context.Background(), app, home, true)
	if !report.OK || len(report.Findings) < 3 {
		t.Fatalf("offline doctor report = %#v", report)
	}
	if got := formatToolDoctorText(report); !strings.Contains(got, "Threadpoint doctor: OK") || !strings.Contains(got, "skipped (offline)") {
		t.Fatalf("offline doctor text = %q", got)
	}
	text := formatToolDoctorText(toolDoctorReport{
		OK:                false,
		Findings:          []toolDoctorFinding{{Severity: "error", Code: "fixture", Message: "needs repair"}},
		SuggestedCommands: []string{"threadpoint update"},
	})
	if !strings.Contains(text, "errors found") || !strings.Contains(text, "Suggested next commands:") || !strings.Contains(text, "threadpoint update") {
		t.Fatalf("failed doctor text = %q", text)
	}
	app.release = releaseClient{
		currentVersion: func() string { return "v1.0.0" },
		fetchReleases: func(context.Context, string) ([]releaseMetadata, error) {
			return nil, nil
		},
	}
	online := buildToolDoctorReport(context.Background(), app, home, false)
	if !online.OK || !strings.Contains(formatToolDoctorText(online), "could not determine update availability") {
		t.Fatalf("no-metadata doctor report = %#v", online)
	}
}

func TestDoctorJSONReportsValidAndInvalidLayouts(t *testing.T) {
	t.Setenv("THREADPOINT_HOME", t.TempDir())
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
		t.Fatalf("doctor emitted invalid JSON: %v\n%s", err, stdout)
	}
	if !report.OK || report.Root != validRoot {
		t.Fatalf("expected valid doctor report, got %#v", report)
	}

	invalidRoot := t.TempDir()
	stdout, _, err = runTestCLI(t, "status", "--root", invalidRoot, "--format", "json")
	if err == nil {
		t.Fatal("expected doctor to fail for missing layout")
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("failed doctor emitted invalid JSON: %v\n%s", err, stdout)
	}
	if report.OK || len(report.Findings) == 0 {
		t.Fatalf("expected invalid doctor report with findings, got %#v", report)
	}
}

func TestDoctorTextReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("THREADPOINT_HOME", home)
	root := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runTestCLI(t, "status", "--root", root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Threadpoint status for $ROOT",
		"Status: OK",
		"State summary:",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("doctor text missing %q:\n%s", want, stdout)
		}
	}
	for _, privatePath := range []string{root, home} {
		if strings.Contains(stdout, privatePath) {
			t.Fatalf("doctor text leaked private path %q:\n%s", privatePath, stdout)
		}
	}

	jsonOutput, _, err := runTestCLI(t, "status", "--root", root, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var report doctor.Report
	if err := json.Unmarshal([]byte(jsonOutput), &report); err != nil {
		t.Fatalf("status JSON is invalid: %v\n%s", err, jsonOutput)
	}
	if report.Root != root {
		t.Fatalf("status JSON root = %q, want raw root %q", report.Root, root)
	}
}

func TestToolDoctorOfflineReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("THREADPOINT_HOME", home)
	stdout, _, err := runTestCLI(t, "doctor", "--offline", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		SchemaVersion string `json:"schema_version"`
		Command       string `json:"command"`
		OK            bool   `json:"ok"`
		Findings      []struct {
			Severity string `json:"severity"`
			Code     string `json:"code"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("tool doctor emitted invalid JSON: %v\n%s", err, stdout)
	}
	if report.SchemaVersion != "threadpoint.doctor.v1" {
		t.Fatalf("schema = %q, want threadpoint.doctor.v1", report.SchemaVersion)
	}
	if report.Command != "threadpoint doctor" {
		t.Fatalf("command = %q, want \"threadpoint doctor\"", report.Command)
	}
	if !report.OK {
		t.Fatalf("expected a healthy offline tool doctor, got %#v", report)
	}
	foundOffline := false
	for _, finding := range report.Findings {
		if finding.Code == "update-availability" {
			foundOffline = true
		}
	}
	if !foundOffline {
		t.Fatalf("expected an update-availability finding, got %#v", report.Findings)
	}

	textOutput, _, err := runTestCLI(t, "doctor", "--offline")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(textOutput, home) {
		t.Fatalf("tool doctor text leaked private home %q:\n%s", home, textOutput)
	}
	if !strings.Contains(textOutput, "threadpoint home resolves to $THREADPOINT_HOME") {
		t.Fatalf("tool doctor text did not redact home:\n%s", textOutput)
	}
}

func TestToolDoctorOfflineReportsLocalStateWithoutNetwork(t *testing.T) {
	home := t.TempDir()
	if err := writeUpdateReminderStateWithHome(home, updateReminderState{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = home
	report := buildToolDoctorReport(context.Background(), app, home, true)
	if !report.OK || len(report.Findings) < 4 || !strings.Contains(formatToolDoctorText(report), "update-availability: skipped (offline)") {
		t.Fatalf("offline doctor report = %#v", report)
	}
	if err := os.WriteFile(filepath.Join(home, "updates", "reminders.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	report = buildToolDoctorReport(context.Background(), app, home, true)
	for _, finding := range report.Findings {
		if finding.Code == "update-reminders" {
			t.Fatalf("invalid reminder state should be omitted, report = %#v", report)
		}
	}
}
