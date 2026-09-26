// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAutomaticUpdateReminderHonorsScheduleDismissalAndForcedRelease(t *testing.T) {
	interactive, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer interactive.Close()
	if !interactiveOutput(interactive) {
		t.Skip("platform null device is not reported as an interactive character device")
	}

	home := t.TempDir()
	app := newCLI(&bytes.Buffer{}, interactive, strings.NewReader(""))
	app.threadpointHome = home
	app.release = releaseClient{currentVersion: func() string { return "v1.0.0" }}
	cachePath, ok := updateCachePathWithHome(home, defaultUpdateRepo, updateChannelStable)
	if !ok {
		t.Fatal("could not resolve update cache path")
	}
	release := releaseMetadata{
		TagName: "v2.0.0", HTMLURL: "https://github.com/threadgrid/threadpoint/releases/tag/v2.0.0",
		Published: time.Now().UTC().Add(-10 * 24 * time.Hour),
		Update: updateMetadata{
			SchemaVersion: expectedUpdateMetadataSchema,
			Product:       "threadpoint", Version: "v2.0.0", Channel: updateChannelStable,
			Summary: "reliability update",
		},
	}
	if err := writeUpdateCache(cachePath, defaultUpdateRepo, release, 1); err != nil {
		t.Fatal(err)
	}
	if err := writeUpdateReminderStateWithHome(home, updateReminderState{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	maybePrintAutomaticUpdateReminder(context.Background(), app, []string{"status"})
	state, err := readUpdateReminderStateWithHome(home)
	if err != nil || !state.LastAutomaticCheckAt.IsZero() {
		t.Fatalf("disabled reminder state = %#v, err=%v", state, err)
	}

	recent := time.Now().UTC()
	if err := writeUpdateReminderStateWithHome(home, updateReminderState{Enabled: true, LastAutomaticCheckAt: recent}); err != nil {
		t.Fatal(err)
	}
	maybePrintAutomaticUpdateReminder(context.Background(), app, []string{"status"})
	state, err = readUpdateReminderStateWithHome(home)
	if err != nil || !state.LastAutomaticCheckAt.Equal(recent) {
		t.Fatalf("scheduled reminder state = %#v, err=%v", state, err)
	}

	if err := writeUpdateReminderStateWithHome(home, updateReminderState{Enabled: true, DismissedVersion: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	maybePrintAutomaticUpdateReminder(context.Background(), app, []string{"status"})
	state, err = readUpdateReminderStateWithHome(home)
	if err != nil || state.LastAutomaticCheckAt.IsZero() || state.DismissedVersion != "v2.0.0" {
		t.Fatalf("dismissed reminder state = %#v, err=%v", state, err)
	}

	release.Update.Critical = true
	release.Published = time.Now().UTC().Add(-40 * 24 * time.Hour)
	if err := writeUpdateCache(cachePath, defaultUpdateRepo, release, 2); err != nil {
		t.Fatal(err)
	}
	if err := writeUpdateReminderStateWithHome(home, updateReminderState{Enabled: true, DismissedVersion: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	maybePrintAutomaticUpdateReminder(context.Background(), app, []string{"status"})
	state, err = readUpdateReminderStateWithHome(home)
	if err != nil || state.LastAutomaticCheckAt.IsZero() {
		t.Fatalf("forced reminder did not complete its check: %#v, err=%v", state, err)
	}
}

func TestAutomaticUpdateReminderFailsClosedForInvalidHomeAndEmptyCache(t *testing.T) {
	interactive, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer interactive.Close()
	if !interactiveOutput(interactive) {
		t.Skip("platform null device is not reported as an interactive character device")
	}
	app := newCLI(&bytes.Buffer{}, interactive, strings.NewReader(""))
	app.threadpointHome = "\x00"
	maybePrintAutomaticUpdateReminder(context.Background(), app, []string{"status"})

	home := t.TempDir()
	app.threadpointHome = home
	app.release = releaseClient{currentVersion: func() string { return "v1.0.0" }}
	if err := writeUpdateReminderStateWithHome(home, updateReminderState{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	maybePrintAutomaticUpdateReminder(context.Background(), app, []string{"status"})
	state, err := readUpdateReminderStateWithHome(home)
	if err != nil || state.LastAutomaticCheckAt.IsZero() {
		t.Fatalf("empty-cache reminder check state = %#v, err=%v", state, err)
	}
}

func TestAutomaticUpdateReminderCheckDoesNotFetchWhenCacheMissing(t *testing.T) {
	t.Setenv("THREADPOINT_HOME", t.TempDir())
	fetched := 0
	release := defaultReleaseClient()
	release.currentVersion = func() string {
		return "v0.1.0"
	}
	release.fetchReleases = func(_ context.Context, repo string) ([]releaseMetadata, error) {
		fetched++
		if repo != defaultUpdateRepo {
			t.Fatalf("repo = %q, want %q", repo, defaultUpdateRepo)
		}
		return []releaseMetadata{{
			TagName:   "v0.2.0",
			HTMLURL:   "https://github.com/threadgrid/threadpoint/releases/tag/v0.2.0",
			Published: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		}}, nil
	}

	report := runAutomaticUpdateReminderCheck(context.Background(), release, "")
	if fetched != 0 {
		t.Fatalf("automatic reminder fetched release metadata %d times, want 0", fetched)
	}
	if report.Source != "none" || report.UpdateAvailable || report.LatestVersion != "" {
		t.Fatalf("unexpected automatic reminder report: %#v", report)
	}
}

func TestThreadpointUpdateReminderDefaultDisabled(t *testing.T) {
	t.Setenv("THREADPOINT_HOME", t.TempDir())
	state, err := readUpdateReminderState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Enabled {
		t.Fatal("threadpoint reminders should default to disabled")
	}
}

func TestAutomaticUpdateReminderSuppressedForAutomationModes(t *testing.T) {
	app := newCLI(io.Discard, io.Discard, strings.NewReader(""))
	if shouldRunAutomaticUpdateReminder(app, []string{"status"}) {
		t.Fatal("non-TTY stderr should suppress reminders")
	}

	app.stderr = os.Stderr
	if interactiveOutput(os.Stderr) && !shouldRunAutomaticUpdateReminder(app, []string{"status"}) {
		t.Fatal("interactive stderr should allow reminders by default")
	}
	app.quiet = true
	if shouldRunAutomaticUpdateReminder(app, []string{"status"}) {
		t.Fatal("quiet mode should suppress reminders")
	}
	app.quiet = false
	app.nonInteractive = true
	if shouldRunAutomaticUpdateReminder(app, []string{"status"}) {
		t.Fatal("noninteractive mode should suppress reminders")
	}
	app.nonInteractive = false
	app.format = "json"
	app.formatExplicit = true
	if shouldRunAutomaticUpdateReminder(app, []string{"status"}) {
		t.Fatal("explicit JSON output should suppress reminders")
	}
}

func TestThreadpointUpdateBannerMatchesInstallSource(t *testing.T) {
	productHome := t.TempDir()
	t.Setenv("THREADPOINT_HOME", productHome)
	manual := bannerText(updateCheckReport{LatestVersion: "v0.2.3", Channel: updateChannelStable}, productHome)
	if strings.Contains(manual, "threadpoint update") {
		t.Fatalf("manual install banner should not advertise managed update: %q", manual)
	}
	if !strings.Contains(manual, "threadpoint installer") || !strings.Contains(manual, "manual release archive") {
		t.Fatalf("manual install banner should name installer and archive update paths: %q", manual)
	}

	bundleRoot := filepath.Join(productHome, "managed")
	binaryPath := filepath.Join(bundleRoot, "bin", "threadpoint")
	linkPath := filepath.Join(t.TempDir(), "threadpoint")
	metadataPath := installerMetadataPath(productHome, binaryPath)
	metadata := installMetadata{
		SchemaVersion: installerMetadataSchemaVersion,
		Channel:       installerChannelScript,
		Repo:          defaultUpdateRepo,
		InstallDir:    filepath.Dir(linkPath),
		BinaryPath:    binaryPath,
		LinkPath:      linkPath,
		BundleRoot:    bundleRoot,
		BundleEntries: threadpointBundleEntries,
		Version:       "v0.2.2",
		ReleaseBase:   "https://github.com/threadgrid/threadpoint/releases/download/v0.2.2",
		Archive:       "threadpoint_0.2.2_linux_amd64.tar.gz",
		ArchiveSHA256: strings.Repeat("a", 64),
		BinarySHA256:  strings.Repeat("b", 64),
	}
	if err := writeInstallMetadata(metadataPath, metadata); err != nil {
		t.Fatal(err)
	}
	source, metadataPath := detectInstallSource(binaryPath, productHome)
	if source != installerChannelScript || metadataPath == "" {
		t.Fatalf("expected installer-managed source, got source=%q metadataPath=%q", source, metadataPath)
	}
}

func TestReminderMutationsMergeIndependentFields(t *testing.T) {
	home := t.TempDir()
	checkedAt := time.Date(2026, time.August, 6, 1, 2, 3, 0, time.UTC)
	start := make(chan struct{})
	errs := make(chan error, 3)
	mutations := []updateReminderMutation{
		{Enabled: boolPointer(true)},
		{DismissedVersion: stringPointer("v1.2.3")},
		{LastAutomaticCheckAt: timePointer(checkedAt)},
	}
	var group sync.WaitGroup
	for _, mutation := range mutations {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := mergeUpdateReminderStateWithHome(home, mutation)
			errs <- err
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reminder mutation failed: %v", err)
		}
	}
	state, err := readUpdateReminderStateWithHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Enabled || state.DismissedVersion != "v1.2.3" || !state.LastAutomaticCheckAt.Equal(checkedAt) {
		t.Fatalf("independent reminder fields were lost: %#v", state)
	}
}

func TestReminderMutationRetriesCASAndMergesUncooperativeEdit(t *testing.T) {
	home := t.TempDir()
	if err := writeUpdateReminderStateWithHome(home, updateReminderState{}); err != nil {
		t.Fatal(err)
	}
	statePath, ok := updateReminderStatePathWithHome(home)
	if !ok {
		t.Fatal("could not resolve reminder state path")
	}
	originalHook := updateStateBeforeCompare
	t.Cleanup(func() { updateStateBeforeCompare = originalHook })
	updateStateBeforeCompare = func() {
		updateStateBeforeCompare = nil
		body := []byte("{\n  \"enabled\": false,\n  \"dismissed_version\": \"v9.9.9\"\n}\n")
		if err := os.WriteFile(statePath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state, err := mergeUpdateReminderStateWithHome(home, updateReminderMutation{Enabled: boolPointer(true)})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Enabled || state.DismissedVersion != "v9.9.9" {
		t.Fatalf("CAS retry lost the concurrent field: %#v", state)
	}
}

func TestUpdateReminderCommandsPersistPreferencesInTextAndJSON(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = home
	app.nonInteractive = true

	if err := runUpdateDismiss(app, []string{"--version", "v1.2.3", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "dismissed update reminder for v1.2.3") {
		t.Fatalf("dismiss output = %q", got)
	}
	stdout.Reset()
	if err := runUpdateReminders(app, []string{"status", "--format", "json"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, `"dismissed_version": "v1.2.3"`) {
		t.Fatalf("status JSON = %q", got)
	}
	stdout.Reset()
	if err := runUpdateReminders(app, []string{"enable", "--format", "json", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, `"enabled": true`) {
		t.Fatalf("enable JSON = %q", got)
	}
	stdout.Reset()
	if err := runUpdateReminders(app, []string{"disable", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "update reminders disabled") {
		t.Fatalf("disable output = %q", got)
	}
	stdout.Reset()
	if err := runUpdateReminders(app, []string{"status"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "update reminders enabled: false") {
		t.Fatalf("status output = %q", got)
	}
	if err := runUpdateReminders(app, []string{"unknown"}); err == nil || !strings.Contains(err.Error(), "unknown update reminder") {
		t.Fatalf("unknown reminders error = %v", err)
	}
	if _, _, err := parseUpdateReminderMutationArgs(app, "update reminder enable", []string{"unexpected"}); err == nil {
		t.Fatal("expected mutation argument validation error")
	}
}

func TestUpdateReminderCommandsPersistAndRenderState(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = home
	app.nonInteractive = true

	for _, args := range [][]string{
		{"disable", "--yes"},
		{"enable", "--yes"},
		{"dismiss", "--version", "v1.2.3", "--yes"},
	} {
		if err := runUpdateReminders(app, args); err != nil {
			t.Fatalf("runUpdateReminders(%v): %v", args, err)
		}
	}

	state, err := readUpdateReminderStateWithHome(home)
	if err != nil || !state.Enabled || state.DismissedVersion != "v1.2.3" {
		t.Fatalf("persisted state = %#v, %v", state, err)
	}
	stdout.Reset()
	if err := runUpdateReminders(app, []string{"status"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"update reminders enabled: true", "dismissed version: v1.2.3"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("status output missing %q: %q", want, stdout.String())
		}
	}

	stdout.Reset()
	if err := runUpdateReminders(app, []string{"disable", "--yes", "--format", "json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"operation": "disable"`) || !strings.Contains(stdout.String(), `"enabled": false`) {
		t.Fatalf("json mutation output = %q", stdout.String())
	}
}

func TestUpdateReminderValidationAndPresentation(t *testing.T) {
	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: nil, want: "usage:"},
		{args: []string{"unknown"}, want: "unknown update reminder"},
		{args: []string{"status", "unexpected"}, want: "does not accept positional"},
	} {
		if err := runUpdateReminders(app, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("runUpdateReminders(%v) error = %v, want %q", tc.args, err, tc.want)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"extra"}, want: "does not accept positional"},
		{args: []string{"--format", "invalid"}, want: "format"},
	} {
		if _, _, err := parseUpdateReminderMutationArgs(app, "update reminder enable", tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("mutation args %v error = %v, want %q", tc.args, err, tc.want)
		}
	}

	var rendered bytes.Buffer
	printUpdateReminderStateText(&rendered, updateReminderState{Enabled: true, DismissedVersion: "v1.2.3", LastAutomaticCheckAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)})
	for _, want := range []string{"update reminders enabled: true", "dismissed version: v1.2.3", "last automatic check: 2026-07-01T00:00:00Z"} {
		if !strings.Contains(rendered.String(), want) {
			t.Errorf("rendered state missing %q: %q", want, rendered.String())
		}
	}
	if text := automaticUpdateReminderText(updateCheckReport{Banner: "available", LatestVersion: "v1.2.3"}, false); !strings.Contains(text, "update reminder dismiss --version v1.2.3") {
		t.Fatalf("dismissible reminder = %q", text)
	}
	if text := automaticUpdateReminderText(updateCheckReport{Banner: "available", Critical: true}, true); !strings.Contains(text, "marked critical") {
		t.Fatalf("critical reminder = %q", text)
	}
}

func TestUpdateReminderStateSchedulingAndEligibility(t *testing.T) {
	home := t.TempDir()
	if path, ok := updateReminderStatePathWithHome("\x00"); ok || path != "" {
		t.Fatalf("invalid home path = %q, %v", path, ok)
	}
	if err := writeUpdateReminderStateWithHome(home, updateReminderState{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	state, err := readUpdateReminderStateWithHome(home)
	if err != nil || !state.Enabled {
		t.Fatalf("read persisted state = %#v, %v", state, err)
	}

	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	for _, args := range [][]string{nil, {"update"}} {
		if shouldRunAutomaticUpdateReminder(app, args) {
			t.Fatalf("args %v should not run automatic reminder", args)
		}
	}
	app.quiet = true
	if shouldRunAutomaticUpdateReminder(app, []string{"status"}) {
		t.Fatal("quiet app should not run automatic reminder")
	}
	app.quiet = false
	app.nonInteractive = true
	if shouldRunAutomaticUpdateReminder(app, []string{"status"}) {
		t.Fatal("noninteractive app should not run automatic reminder")
	}
	if report := runAutomaticUpdateReminderCheck(context.Background(), releaseClient{currentVersion: func() string { return "v1.0.0" }}, home); report.Source != "none" || report.UpdateAvailable {
		t.Fatalf("empty cache reminder report = %#v", report)
	}
}

func TestUpdateReminderForcedUsesCriticalReleaseAgeAndReleaseCount(t *testing.T) {
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		report updateCheckReport
		want   bool
	}{
		{name: "critical", report: updateCheckReport{Critical: true}, want: true},
		{name: "one release", report: updateCheckReport{NewerStableReleaseCount: 1, LatestPublishedAt: now.Add(-90 * 24 * time.Hour).Format(time.RFC3339)}, want: false},
		{name: "invalid publish time", report: updateCheckReport{NewerStableReleaseCount: 2, LatestPublishedAt: "not-a-time"}, want: false},
		{name: "recent", report: updateCheckReport{NewerStableReleaseCount: 2, LatestPublishedAt: now.Add(-29 * 24 * time.Hour).Format(time.RFC3339)}, want: false},
		{name: "old enough", report: updateCheckReport{NewerStableReleaseCount: 2, LatestPublishedAt: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := updateReminderForced(test.report, now); got != test.want {
				t.Fatalf("updateReminderForced(%#v) = %v, want %v", test.report, got, test.want)
			}
		})
	}
}

func TestReminderCommandPathsPersistStateAndKeepReportSchemas(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := t.TempDir()
	invoke := func(args ...string) string {
		t.Helper()
		argv := append([]string{"--home", home, "--non-interactive", "update", "reminder"}, args...)
		stdout, _, err := runTestCLI(t, argv...)
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		return stdout
	}
	for _, operation := range []string{"enable", "dismiss", "disable"} {
		args := []string{operation, "--yes", "--format", "json"}
		if operation == "dismiss" {
			args = append(args, "--version", "v1.2.3")
		}
		output := invoke(args...)
		var report updateReminderMutationReport
		if err := json.Unmarshal([]byte(output), &report); err != nil {
			t.Fatal(err)
		}
		if !report.OK || report.Operation != operation || report.Command != "threadpoint update reminder "+operation || report.SchemaVersion != "threadpoint.update_reminder.v1" {
			t.Fatalf("unexpected reminder report: %#v", report)
		}
	}
	var state updateReminderState
	if err := json.Unmarshal([]byte(invoke("status", "--format", "json")), &state); err != nil {
		t.Fatal(err)
	}
	if state.Enabled || state.DismissedVersion != "v1.2.3" {
		t.Fatalf("persisted reminder preferences = %#v", state)
	}
}

func TestReminderDismissValidatesArgumentsBeforeResolvingHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("THREADPOINT_HOME", "")
	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	err := runUpdateReminders(app, []string{"dismiss"})
	if err == nil || !strings.Contains(err.Error(), "requires --version") {
		t.Fatalf("dismiss validation was masked by home resolution: %v", err)
	}
}
