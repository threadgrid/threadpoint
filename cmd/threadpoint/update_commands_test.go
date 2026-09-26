// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateCheckTextAndJSONOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("THREADPOINT_HOME", home)
	repo := "threadgrid/threadpoint"
	cachePath, ok := updateCachePath(repo)
	if !ok {
		t.Fatalf("expected cache path for %s", repo)
	}
	releaseRecord := releaseMetadata{
		TagName: "v0.1.0",
		HTMLURL: "https://github.com/threadgrid/threadpoint/releases/tag/v0.1.0",
	}
	releaseRecord.Update = defaultUpdateMetadata(repo, releaseRecord, updateChannelStable)
	if err := writeUpdateCache(cachePath, repo, releaseRecord, 0); err != nil {
		t.Fatal(err)
	}
	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		t.Fatal("cache should satisfy update check without network call")
		return releaseMetadata{}, nil
	}
	release.currentVersion = func() string {
		return "v0.0.0"
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "check", "--repo", repo, "--format", "text")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "command: threadpoint update check") {
		t.Fatalf("text output missing command header:\n%s", stdout)
	}
	if !strings.Contains(stdout, "update available: true") {
		t.Fatalf("expected update available in text output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "A newer threadpoint stable release is available") {
		t.Fatalf("expected notification banner in text output:\n%s", stdout)
	}

	stdout, err = runTestCLIWithRelease(t, release, "update", "check", "--repo", repo, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var report updateCheckReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("update check JSON invalid: %v\n%s", err, stdout)
	}
	if report.Source != "cache" || report.Repo != repo {
		t.Fatalf("expected cached source and repo in report: %#v", report)
	}
	if !report.UpdateAvailable || report.LatestVersion != "v0.1.0" {
		t.Fatalf("expected available update from cache: %#v", report)
	}
}

func TestUpdateCheckCacheOnlyDoesNotFetchNetwork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("THREADPOINT_HOME", home)
	fetched := false
	release := defaultReleaseClient()
	release.currentVersion = func() string {
		return "v0.1.0"
	}
	release.fetchReleases = func(context.Context, string) ([]releaseMetadata, error) {
		fetched = true
		return []releaseMetadata{{TagName: "v0.2.0"}}, nil
	}

	report := runUpdateCheckReportWithOptions(context.Background(), release, "threadgrid/threadpoint", updateCheckOptions{
		Channel:         updateChannelStable,
		ThreadpointHome: home,
		UseCache:        true,
		CacheOnly:       true,
		MaxAge:          defaultAutomaticReminderCacheMaxAge,
	})
	if fetched {
		t.Fatal("cache-only update check must not fetch release metadata")
	}
	if report.Source != "none" || report.LatestVersion != "" || report.UpdateAvailable {
		t.Fatalf("unexpected cache-only report: %#v", report)
	}
	if !strings.Contains(report.Reason, "no fresh cached update metadata") {
		t.Fatalf("expected cache-only reason, got %q", report.Reason)
	}
}

func TestUpdateCheckFallsBackToStaleCacheOnNetworkError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("THREADPOINT_HOME", home)
	repo := "threadgrid/threadpoint"
	cachePath, ok := updateCachePath(repo)
	if !ok {
		t.Fatalf("expected cache path for %s", repo)
	}
	record := updateCacheMetadata{
		SchemaVersion: expectedUpdateCacheSchema,
		Repo:          repo,
		Tag:           "v0.3.0",
		HTMLURL:       "https://github.com/threadgrid/threadpoint/releases/tag/v0.3.0",
		Channel:       updateChannelStable,
		CheckedAt:     time.Now().UTC().Add(-time.Hour).Add(-time.Minute),
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		return releaseMetadata{}, errors.New("offline")
	}
	release.currentVersion = func() string {
		return "v0.2.0"
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "check", "--repo", repo, "--format", "json", "--max-age", "1m")
	if err != nil {
		t.Fatal(err)
	}
	var report updateCheckReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("update check JSON invalid: %v\n%s", err, stdout)
	}
	if report.Source != "stale-cache" {
		t.Fatalf("expected stale cache source, got %q", report.Source)
	}
	if !report.UpdateAvailable {
		t.Fatalf("expected stale cache to report available update: %#v", report)
	}
	if report.Code != "update_available" || report.Message == "" {
		t.Fatalf("expected coded stale-cache outcome, got: %#v", report)
	}
}

func TestUpdateCheckHandlesNoNetworkWithoutCache(t *testing.T) {
	t.Parallel()
	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		return releaseMetadata{}, errors.New("offline")
	}
	release.currentVersion = func() string {
		return "v0.1.0"
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "check", "--repo", "threadgrid/threadpoint", "--format", "json", "--no-cache")
	if err != nil {
		t.Fatal(err)
	}
	var report updateCheckReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("update check JSON invalid: %v\n%s", err, stdout)
	}
	if report.Source != "none" {
		t.Fatalf("expected no source when no cache is available, got %#v", report.Source)
	}
	if report.LatestVersion != "" {
		t.Fatalf("expected no latest version when network and cache fail, got %#v", report.LatestVersion)
	}
	if report.UpdateAvailable {
		t.Fatalf("expected no update availability without metadata, got %#v", report)
	}
	if report.Code != "update_metadata_unavailable" || report.Message == "" {
		t.Fatalf("expected coded network outcome, got %#v", report)
	}
}

func TestUpdateCheckHandlesMalformedMetadataWithoutCache(t *testing.T) {
	t.Parallel()
	release := defaultReleaseClient()
	release.fetchLatest = func(context.Context, string) (releaseMetadata, error) {
		return releaseMetadata{}, errors.New("release metadata is missing tag_name")
	}
	release.currentVersion = func() string {
		return "v0.1.0"
	}

	stdout, err := runTestCLIWithRelease(t, release, "update", "check", "--repo", "threadgrid/threadpoint", "--format", "json", "--no-cache")
	if err != nil {
		t.Fatal(err)
	}
	var report updateCheckReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("update check JSON invalid: %v\n%s", err, stdout)
	}
	if report.UpdateAvailable {
		t.Fatalf("expected no update availability without valid metadata, got %#v", report)
	}
	if report.Code != "update_metadata_unavailable" || report.Message == "" {
		t.Fatalf("expected coded metadata outcome, got %#v", report)
	}
}

func TestUpdateRollbackRejectsChannelFlag(t *testing.T) {
	_, _, err := runTestCLI(t, "update", "rollback", "--channel", "preview")
	if err == nil || !strings.Contains(err.Error(), "does not accept --channel") {
		t.Fatalf("expected rollback channel usage error, got %v", err)
	}
}

func TestUpdateCommandDispatchAndCheckValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = t.TempDir()
	if err := app.run(context.Background(), []string{"update", "check", "--help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Check for newer official releases") {
		t.Fatalf("check help = %q", stderr.String())
	}
	stderr.Reset()
	if err := app.run(context.Background(), []string{"update", "--help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "installer-managed") {
		t.Fatalf("update help = %q", stderr.String())
	}
	if err := runUpdate(context.Background(), app, []string{"unknown"}); err == nil || !strings.Contains(err.Error(), "unknown update subcommand") {
		t.Fatalf("unknown update error = %v", err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"unexpected"}, want: "does not accept positional"},
		{args: []string{"--repo", " "}, want: "requires --repo"},
		{args: []string{"--max-age", "invalid"}, want: "invalid --max-age"},
		{args: []string{"--max-age", "-1s"}, want: "zero or positive"},
		{args: []string{"--channel", "unsupported"}, want: "unknown update channel"},
	} {
		if err := runUpdateCheck(context.Background(), app, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("runUpdateCheck(%v) error = %v, want %q", tc.args, err, tc.want)
		}
	}
	if err := runUpdateDismiss(app, []string{"--version", ""}); err == nil || !strings.Contains(err.Error(), "requires --version") {
		t.Fatalf("dismiss validation error = %v", err)
	}
}

func TestUpdateCheckUsesReleaseMetadataAndFallsBackToStaleCache(t *testing.T) {
	home := t.TempDir()
	client := releaseClient{
		currentVersion: func() string { return "v1.0.0" },
		fetchReleases: func(context.Context, string) ([]releaseMetadata, error) {
			return []releaseMetadata{
				{TagName: "v1.0.1", Draft: true},
				{TagName: "v1.1.0-rc.1", Prerelease: true},
				{TagName: "v1.2.0", Name: "A newer release", HTMLURL: "https://github.com/threadgrid/threadpoint/releases/tag/v1.2.0", Assets: []releaseAssetMetadata{{Name: "update.json", BrowserDownloadURL: "https://updates.test/update.json"}}},
			}, nil
		},
		fetchUpdate: func(context.Context, string) (updateMetadata, error) {
			return updateMetadata{
				SchemaVersion: expectedUpdateMetadataSchema,
				Product:       "threadpoint",
				Version:       "v1.2.0",
				Channel:       updateChannelStable,
				Critical:      true,
				Summary:       "Security and reliability fixes",
				LearnMoreURL:  "https://github.com/threadgrid/threadpoint/releases/tag/v1.2.0",
			}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	app.threadpointHome = home
	app.release = client
	if err := runUpdateCheck(context.Background(), app, []string{"--no-cache"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "latest: v1.2.0") || !strings.Contains(got, "This update is marked critical") {
		t.Fatalf("network update check output = %q", got)
	}

	cachePath, ok := updateCachePathWithHome(home, defaultUpdateRepo, updateChannelStable)
	if !ok {
		t.Fatal("expected usable update cache path")
	}
	if err := writeUpdateCache(cachePath, defaultUpdateRepo, releaseMetadata{TagName: "v1.3.0", HTMLURL: "https://github.com/threadgrid/threadpoint/releases/tag/v1.3.0", Update: updateMetadata{Channel: updateChannelStable, Summary: "Cached update"}}, 2); err != nil {
		t.Fatal(err)
	}
	stale, found, err := readUpdateCache(cachePath)
	if err != nil || !found {
		t.Fatalf("read cache = %#v, %v, %v", stale, found, err)
	}
	stale.CheckedAt = time.Now().UTC().Add(-time.Hour)
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	report := runUpdateCheckReportWithOptions(context.Background(), releaseClient{
		currentVersion: func() string { return "v1.0.0" },
		fetchReleases:  func(context.Context, string) ([]releaseMetadata, error) { return nil, errors.New("offline") },
	}, defaultUpdateRepo, updateCheckOptions{Channel: updateChannelStable, ThreadpointHome: home, UseCache: true, MaxAge: time.Minute})
	if report.Source != "stale-cache" || !report.UpdateAvailable || !strings.Contains(report.Reason, "offline") {
		t.Fatalf("stale cache report = %#v", report)
	}
	cacheOnly := runUpdateCheckReportWithOptions(context.Background(), client, defaultUpdateRepo, updateCheckOptions{Channel: updateChannelStable, ThreadpointHome: t.TempDir(), UseCache: true, CacheOnly: true, MaxAge: time.Minute})
	if !strings.Contains(cacheOnly.Reason, "no fresh cached update metadata") {
		t.Fatalf("cache-only report = %#v", cacheOnly)
	}
}

func TestUpdateCheckStateClassifiesReleaseAvailability(t *testing.T) {
	for _, test := range []struct {
		current string
		latest  string
		want    string
	}{
		{current: "v1.2.3", latest: "v1.2.3", want: "already up to date"},
		{current: "v2.0.0", latest: "v1.2.3", want: "running newer"},
		{current: "v1.2.3", latest: "not-semver", want: "latest version"},
		{current: "not-semver", latest: "v1.2.3", want: "current version"},
	} {
		report := updateCheckReport{Reason: "cached"}
		updateCheckState(&report, test.current, test.latest, t.TempDir())
		if !strings.Contains(report.Reason, test.want) {
			t.Fatalf("update state (%s, %s) = %#v", test.current, test.latest, report)
		}
	}
}

func TestUpdateCheckStateWarnsForAvailablePreview(t *testing.T) {
	metadata := sanitizeUpdateMetadata(updateMetadata{
		SchemaVersion: " threadpoint.update.v1 ",
		Product:       " threadpoint ",
		Version:       " v1.2.3 ",
		Channel:       " stable ",
		Summary:       " summary\nwith control\x00 ",
		LearnMoreURL:  " https://github.com/threadgrid/threadpoint/releases/tag/v1.2.3\n ",
	})

	available := updateCheckReport{Channel: updateChannelPreview, LatestVersion: "v1.2.3-rc.1", Summary: "preview summary", LearnMoreURL: metadata.LearnMoreURL}
	updateCheckState(&available, "v1.2.2", available.LatestVersion, t.TempDir())
	if !available.UpdateAvailable || !strings.Contains(available.Banner, "Preview releases may be unstable") {
		t.Fatalf("preview update report = %#v", available)
	}
}
