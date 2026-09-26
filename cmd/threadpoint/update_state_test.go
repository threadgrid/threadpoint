// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUpdateCachePathUsesThreadpointHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("THREADPOINT_HOME", home)

	cachePath, ok := updateCachePath("threadgrid/threadpoint")
	if !ok {
		t.Fatal("expected update cache path")
	}
	if !strings.HasPrefix(cachePath, filepath.Join(home, "updates", "cache")+string(os.PathSeparator)) {
		t.Fatalf("expected cache path under THREADPOINT_HOME updates/cache, got %s", cachePath)
	}
}

func TestConcurrentUpdateCacheWritersUsePrivateIdentityPinnedTemps(t *testing.T) {
	home := t.TempDir()
	path, ok := updateCachePathWithHome(home, defaultUpdateRepo, updateChannelStable)
	if !ok {
		t.Fatal("could not resolve cache path")
	}
	const writers = 24
	start := make(chan struct{})
	errs := make(chan error, writers)
	var group sync.WaitGroup
	for index := 0; index < writers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			tag := fmt.Sprintf("v1.2.%d", index)
			release := releaseMetadata{
				TagName:   tag,
				HTMLURL:   fmt.Sprintf("https://github.com/threadgrid/threadpoint/releases/tag/%s", tag),
				Published: time.Unix(int64(index+1), 0).UTC(),
				Update:    updateMetadata{Channel: updateChannelStable},
			}
			errs <- writeUpdateCache(path, defaultUpdateRepo, release, index)
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent cache writer failed: %v", err)
		}
	}
	if _, found, err := readUpdateCache(path); err != nil || !found {
		t.Fatalf("published cache = found %v, err %v", found, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") || strings.Contains(entry.Name(), ".publish-") {
			t.Fatalf("cache temporary residue remains: %s", entry.Name())
		}
	}
}

func TestUpdateStateWriteDoesNotFollowReplacedProductRoot(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "threadpoint-home")
	detached := filepath.Join(parent, "threadpoint-home-detached")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cachePath, ok := updateCachePathWithHome(home, defaultUpdateRepo, updateChannelStable)
	if !ok {
		t.Fatal("could not resolve cache path")
	}
	originalHook := updateStateAfterRootBorrow
	t.Cleanup(func() { updateStateAfterRootBorrow = originalHook })
	updateStateAfterRootBorrow = func() {
		updateStateAfterRootBorrow = nil
		if err := os.Rename(home, detached); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	err := writeUpdateCache(cachePath, defaultUpdateRepo, releaseMetadata{
		TagName: "v1.2.3", HTMLURL: "https://github.com/threadgrid/threadpoint/releases/tag/v1.2.3",
		Update: updateMetadata{Channel: updateChannelStable},
	}, 1)
	if err == nil || !strings.Contains(err.Error(), "locked generation") {
		t.Fatalf("replaced product root write error = %v", err)
	}
	rel, err := filepath.Rel(home, cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, rel)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement product root received cache state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(detached, rel)); err != nil {
		t.Fatalf("retained product root did not receive cache state: %v", err)
	}
}

func TestUpdateCacheRejectsFutureTimestamp(t *testing.T) {
	home := t.TempDir()
	repo := defaultUpdateRepo
	path, ok := updateCachePathWithHome(home, repo, updateChannelStable)
	if !ok {
		t.Fatal("could not resolve update cache path")
	}
	writeRawUpdateCache(t, path, map[string]any{
		"schema_version": "threadpoint.update-cache.v1",
		"repo":           repo,
		"tag":            "v1.2.3",
		"html_url":       "https://github.com/threadgrid/threadpoint/releases/tag/v1.2.3",
		"channel":        updateChannelStable,
		"checked_at":     time.Now().UTC().Add(time.Hour),
	})

	report := runUpdateCheckReportWithOptions(context.Background(), releaseClient{
		currentVersion: func() string { return "v1.0.0" },
	}, repo, updateCheckOptions{
		Channel: updateChannelStable, ThreadpointHome: home,
		UseCache: true, CacheOnly: true, MaxAge: 24 * time.Hour,
	})
	if report.Source != "none" || report.LatestVersion != "" || report.UpdateAvailable {
		t.Fatalf("future cache remained authoritative: %#v", report)
	}
	if !strings.Contains(report.Reason, "future") {
		t.Fatalf("future cache refusal was not visible: %q", report.Reason)
	}
}

func TestUpdateCacheRejectsNamespaceSwap(t *testing.T) {
	home := t.TempDir()
	wantedRepo := defaultUpdateRepo
	path, ok := updateCachePathWithHome(home, wantedRepo, updateChannelStable)
	if !ok {
		t.Fatal("could not resolve update cache path")
	}
	writeRawUpdateCache(t, path, map[string]any{
		"schema_version": "threadpoint.update-cache.v1",
		"repo":           "other/project",
		"tag":            "v1.2.3",
		"html_url":       "https://github.com/other/project/releases/tag/v1.2.3",
		"channel":        updateChannelStable,
		"checked_at":     time.Now().UTC(),
	})
	if record, found, err := readUpdateCache(path); err == nil || found {
		t.Fatalf("namespace-swapped cache was accepted: %#v found=%v err=%v", record, found, err)
	}
}

func TestUpdateCacheRequiresExactVersionedSchema(t *testing.T) {
	home := t.TempDir()
	path, ok := updateCachePathWithHome(home, defaultUpdateRepo, updateChannelStable)
	if !ok {
		t.Fatal("could not resolve update cache path")
	}
	base := map[string]any{
		"schema_version": "threadpoint.update-cache.v1",
		"repo":           defaultUpdateRepo,
		"tag":            "v1.2.3",
		"html_url":       "https://github.com/threadgrid/threadpoint/releases/tag/v1.2.3",
		"channel":        updateChannelStable,
		"checked_at":     time.Now().UTC(),
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing-schema", mutate: func(value map[string]any) { delete(value, "schema_version") }},
		{name: "unknown-field", mutate: func(value map[string]any) { value["future"] = true }},
		{name: "channel-mismatch", mutate: func(value map[string]any) { value["channel"] = updateChannelPreview }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := make(map[string]any, len(base))
			for key, item := range base {
				value[key] = item
			}
			test.mutate(value)
			writeRawUpdateCache(t, path, value)
			if record, found, err := readUpdateCache(path); err == nil || found {
				t.Fatalf("invalid cache was accepted: %#v found=%v err=%v", record, found, err)
			}
		})
	}
}

func TestUpdateCacheChannelPrefersCachedThenConfiguredDefault(t *testing.T) {
	for _, test := range []struct {
		cached            string
		configuredDefault string
		want              string
	}{
		{cached: "preview", configuredDefault: "stable", want: "preview"},
		{configuredDefault: "preview", want: "preview"},
		{want: updateChannelStable},
	} {
		if got := cacheChannel(test.cached, test.configuredDefault); got != test.want {
			t.Fatalf("cacheChannel(%q, %q) = %q, want %q", test.cached, test.configuredDefault, got, test.want)
		}
	}
}

func TestUpdateMetadataAndStateRejectNoncanonicalBoundaryValues(t *testing.T) {
	release := releaseMetadata{
		TagName: "v1.2.3",
		HTMLURL: updateReleaseTagURL(defaultUpdateRepo, "v1.2.3"),
	}
	valid := updateMetadata{
		SchemaVersion: expectedUpdateMetadataSchema,
		Product:       "threadpoint",
		Version:       release.TagName,
		Channel:       updateChannelStable,
		LearnMoreURL:  release.HTMLURL,
	}
	for name, mutate := range map[string]func(*updateMetadata, *releaseMetadata, *string){
		"wrong schema":      func(metadata *updateMetadata, _ *releaseMetadata, _ *string) { metadata.SchemaVersion = "v0" },
		"wrong product":     func(metadata *updateMetadata, _ *releaseMetadata, _ *string) { metadata.Product = "other" },
		"wrong version":     func(metadata *updateMetadata, _ *releaseMetadata, _ *string) { metadata.Version = "v9.9.9" },
		"wrong channel":     func(metadata *updateMetadata, _ *releaseMetadata, _ *string) { metadata.Channel = updateChannelPreview },
		"preview is stable": func(_ *updateMetadata, _ *releaseMetadata, channel *string) { *channel = updateChannelPreview },
		"stable is preview": func(_ *updateMetadata, candidate *releaseMetadata, _ *string) { candidate.Prerelease = true },
	} {
		t.Run(name, func(t *testing.T) {
			metadataCopy := valid
			releaseCopy := release
			channel := updateChannelStable
			mutate(&metadataCopy, &releaseCopy, &channel)
			if err := validateUpdateMetadata(defaultUpdateRepo, releaseCopy, channel, metadataCopy); err == nil {
				t.Fatal("noncanonical update metadata was accepted")
			}
		})
	}

	metadataRelease := releaseMetadata{TagName: "v1.2.3", HTMLURL: release.HTMLURL}
	applyReleaseMetadata(&metadataRelease, updateChannelStable)
	if metadataRelease.Update.Channel != updateChannelStable || metadataRelease.Update.Version != metadataRelease.TagName || metadataRelease.Update.LearnMoreURL != release.HTMLURL {
		t.Fatalf("release metadata defaults = %#v", metadataRelease.Update)
	}
	if got := formatOptionalTime(time.Time{}); got != "" {
		t.Fatalf("zero optional time = %q", got)
	}
	if got := firstReleaseBodyLine("\n ## First line \nsecond"); got != "First line" {
		t.Fatalf("first release body line = %q", got)
	}
	if err := writeUpdateCache("", defaultUpdateRepo, release, 0); err != nil {
		t.Fatalf("empty cache path should be disabled: %v", err)
	}

	for _, message := range []string{"file lock changed while opened", "file lock changed while read"} {
		if !updateStateLockContention(errors.New(message)) {
			t.Fatalf("lock contention was not recognized: %s", message)
		}
	}
	if updateStateLockContention(errors.New("ordinary failure")) {
		t.Fatal("ordinary failure was classified as lock contention")
	}
	if err := ensurePrivateUpdateStateDirectory(nil, "updates"); err == nil {
		t.Fatal("nil update-state root was accepted")
	}
	if _, _, err := updateStatePathLocation(""); err == nil {
		t.Fatal("empty update-state path was accepted")
	}
	if _, _, err := updateStatePathLocation(filepath.Join(t.TempDir(), "outside.json")); err == nil {
		t.Fatal("noncanonical update-state path was accepted")
	}

	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensurePrivateUpdateStateDirectory(root, "../outside"); err == nil {
		t.Fatal("escaping update-state directory was accepted")
	}
	if err := root.Mkdir("directory", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readUpdateStateGeneration(root, "directory"); err == nil {
		t.Fatal("directory was accepted as update state")
	}
	if err := sameUpdateStateGeneration(root, "missing", updateStateGeneration{exists: true}); !errors.Is(err, errUpdateStateGenerationChanged) {
		t.Fatalf("changed generation error = %v", err)
	}
	wantBuildError := errors.New("build failed")
	if err := writeUpdateStateCAS(root, "state.json", func(updateStateGeneration) ([]byte, error) { return nil, wantBuildError }); !errors.Is(err, wantBuildError) {
		t.Fatalf("CAS build error = %v", err)
	}

	for _, tag := range []string{" v1.2.3", "vv1.2.3", "v1.2.3+a+b", "v1.x.3", "v1.2.3-01", "v1.2.3+bad!"} {
		if _, err := parseCanonicalReleaseTag(tag); err == nil {
			t.Fatalf("noncanonical release tag was accepted: %q", tag)
		}
	}
	for _, repo := range []string{" owner/repo", "owner", "/repo", ".owner/repo", "owner./repo", "owner/repo!"} {
		if validUpdateRepository(repo) {
			t.Fatalf("noncanonical repository was accepted: %q", repo)
		}
	}
}

func writeRawUpdateCache(t *testing.T, path string, value map[string]any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCacheValidationRejectsInconsistentReleaseMetadata(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	home := t.TempDir()
	record := updateCacheMetadata{SchemaVersion: expectedUpdateCacheSchema, Repo: "owner/project", Tag: "v1.2.3", HTMLURL: "https://github.com/owner/project/releases/tag/v1.2.3", Channel: updateChannelStable, Published: now.Add(-time.Hour), CheckedAt: now}
	path, ok := updateCachePathWithHome(home, record.Repo, record.Channel)
	if !ok {
		t.Fatal("valid namespace rejected")
	}
	if err := validateUpdateCacheRecord(path, home, record); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	for _, failure := range []string{"schema", "repository", "channel", "path", "tag", "prerelease", "release-url", "learn-more", "summary", "negative-count", "missing-check", "local-check", "future-publication"} {
		t.Run(failure, func(t *testing.T) {
			record := record
			path := path
			switch failure {
			case "schema":
				record.SchemaVersion = "unknown"
			case "repository":
				record.Repo = "../escape"
			case "channel":
				record.Channel = "STABLE"
			case "path":
				path = filepath.Join(home, "other.json")
			case "tag":
				record.Tag = "1.2.3"
			case "prerelease":
				record.Prerelease = true
			case "release-url":
				record.HTMLURL = "https://example.test/foreign"
			case "learn-more":
				record.LearnMoreURL = "javascript:alert(1)"
			case "summary":
				record.Summary = "raw\x1b[31mterminal"
			case "negative-count":
				record.NewerStableReleaseCount = -1
			case "missing-check":
				record.CheckedAt = time.Time{}
			case "local-check":
				record.CheckedAt = now.In(time.FixedZone("offset", 3600))
			case "future-publication":
				record.Published = now.Add(time.Hour)
			}
			if err := validateUpdateCacheRecord(path, home, record); err == nil {
				t.Fatal("inconsistent cached release accepted")
			}
		})
	}
}
