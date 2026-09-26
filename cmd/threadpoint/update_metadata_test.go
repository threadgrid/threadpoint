// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUpdateMetadataValidationPinsSchemaAndLearnMoreURL(t *testing.T) {
	release := releaseMetadata{
		TagName: "v0.2.0",
		HTMLURL: "https://github.com/threadgrid/threadpoint/releases/tag/v0.2.0",
	}
	metadata := updateMetadata{
		SchemaVersion: "invalid-schema",
		Product:       "threadpoint",
		Version:       "v0.2.0",
		Channel:       updateChannelStable,
		LearnMoreURL:  "https://github.com/threadgrid/threadpoint/releases/tag/v0.2.0",
	}
	if err := validateUpdateMetadata("threadgrid/threadpoint", release, updateChannelStable, metadata); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("expected schema validation error, got %v", err)
	}

	metadata.SchemaVersion = expectedUpdateMetadataSchema
	metadata.LearnMoreURL = "https://example.invalid/threadgrid/threadpoint/releases/tag/v0.2.0"
	if err := validateUpdateMetadata("threadgrid/threadpoint", release, updateChannelStable, metadata); err == nil || !strings.Contains(err.Error(), "learn_more_url") {
		t.Fatalf("expected learn_more_url validation error, got %v", err)
	}

	metadata.LearnMoreURL = "https://github.com/threadgrid/threadpoint/releases/tag/v0.2.0"
	metadata.Summary = "First line\nsecond line\x1b"
	sanitized := sanitizeUpdateMetadata(metadata)
	if sanitized.Summary != "First linesecond line" {
		t.Fatalf("control characters were not stripped: %q", sanitized.Summary)
	}
	if err := validateUpdateMetadata("threadgrid/threadpoint", release, updateChannelStable, sanitized); err != nil {
		t.Fatalf("expected sanitized metadata to validate: %v", err)
	}
}

func TestProductFromRepoAlwaysUsesPublicProductName(t *testing.T) {
	for _, repo := range []string{"threadgrid/threadpoint", "owner/other"} {
		if got := productFromRepo(repo); got != "threadpoint" {
			t.Fatalf("productFromRepo(%q) = %q, want threadpoint", repo, got)
		}
	}
}

func TestUpdateMetadataSanitizesSummaryAndTrustsOnlyReleaseURLs(t *testing.T) {
	metadata := sanitizeUpdateMetadata(updateMetadata{
		SchemaVersion: " threadpoint.update.v1 ",
		Product:       " threadpoint ",
		Version:       " v1.2.3 ",
		Channel:       " stable ",
		Summary:       " summary\nwith control\x00 ",
		LearnMoreURL:  " https://github.com/threadgrid/threadpoint/releases/tag/v1.2.3\n ",
	})
	if metadata.Summary != "summarywith control" || metadata.LearnMoreURL != "https://github.com/threadgrid/threadpoint/releases/tag/v1.2.3" {
		t.Fatalf("sanitized metadata = %#v", metadata)
	}
	if got := trustedUpdateLearnMoreURL("threadgrid/threadpoint", metadata.LearnMoreURL); got != metadata.LearnMoreURL {
		t.Fatalf("trusted learn-more URL = %q", got)
	}
	if got := trustedUpdateLearnMoreURL("threadgrid/threadpoint", "http://example.test/release"); got != "" {
		t.Fatalf("untrusted learn-more URL = %q", got)
	}
	for _, raw := range []string{"", "https://github.com/threadgrid/threadpoint/releases", "https://github.com/threadgrid/threadpoint/releases/tag/v1.2.3"} {
		if err := validateUpdateLearnMoreURL("threadgrid/threadpoint", raw); err != nil {
			t.Fatalf("validateUpdateLearnMoreURL(%q) = %v", raw, err)
		}
	}
	if err := validateUpdateLearnMoreURL("threadgrid/threadpoint", "https://github.com/other/project/releases/tag/v1.2.3"); err == nil {
		t.Fatal("unexpected acceptance of another repository's release URL")
	}
}

func TestManagedUpdateApplyValidatesAllLocalArgumentsBeforeNetworkAccess(t *testing.T) {
	app := newCLI(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	app.threadpointHome = t.TempDir()
	for _, test := range []struct {
		operation updateOperation
		args      []string
		want      string
	}{
		{operation: updateOperationUpdate, args: []string{"unexpected"}, want: "does not accept positional"},
		{operation: updateOperationUpdate, args: []string{"--repo", " "}, want: "requires --repo"},
		{operation: updateOperationUpdate, args: []string{"--dir", " "}, want: "requires --dir"},
		{operation: updateOperationUpdate, args: []string{"--channel", "unsupported"}, want: "unknown update channel"},
		{operation: updateOperationUpdate, args: []string{"--max-age", "invalid"}, want: "invalid --max-age"},
		{operation: updateOperationUpdate, args: []string{"--max-age", "-1s"}, want: "zero or positive"},
		{operation: updateOperationRollback, args: []string{"--channel", "preview"}, want: "does not accept --channel"},
	} {
		t.Run(test.want, func(t *testing.T) {
			if err := runUpdateApply(context.Background(), app, test.args, test.operation); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runUpdateApply(%q, %v) error = %v, want %q", test.operation, test.args, err, test.want)
			}
		})
	}
	if err := runUpdateApply(context.Background(), app, []string{"--dir", t.TempDir()}, updateOperationUpdate); err == nil || !strings.Contains(err.Error(), "no installer metadata") {
		t.Fatalf("unmanaged binary update error = %v", err)
	}
}

func TestUpdateNetworkReadersSendExpectedRequestsAndRejectBadResponses(t *testing.T) {
	originalClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = originalClient })
	http.DefaultClient = &http.Client{Transport: updateNetworkRoundTripper(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("User-Agent"); got != "threadpoint-update-check" {
			t.Errorf("User-Agent = %q", got)
		}
		var body string
		switch request.URL.Path {
		case "/repos/threadgrid/threadpoint/releases/latest":
			if got := request.Header.Get("Accept"); got != "application/vnd.github+json" {
				t.Errorf("release Accept = %q", got)
			}
			body = `{"tag_name":"v1.2.0","html_url":"https://github.com/threadgrid/threadpoint/releases/tag/v1.2.0"}`
		case "/repos/threadgrid/threadpoint/releases":
			if got := request.URL.Query().Get("per_page"); got != "30" {
				t.Errorf("release page size = %q", got)
			}
			body = `[{"tag_name":"v1.2.0"},{"tag_name":"v1.3.0-rc.1","prerelease":true}]`
		case "/update.json":
			if got := request.Header.Get("Accept"); got != "application/json" {
				t.Errorf("metadata Accept = %q", got)
			}
			body = `{"schema_version":"threadpoint.update.v1","product":"threadpoint","version":"v1.2.0","channel":"stable","summary":"Update summary"}`
		default:
			t.Fatalf("unexpected update request: %s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	latest, err := fetchLatestReleaseFromGitHub(context.Background(), "threadgrid/threadpoint")
	if err != nil || latest.TagName != "v1.2.0" {
		t.Fatalf("latest release = %#v, %v", latest, err)
	}
	releases, err := fetchReleasesFromGitHub(context.Background(), "threadgrid/threadpoint")
	if err != nil || len(releases) != 2 {
		t.Fatalf("releases = %#v, %v", releases, err)
	}
	metadata, err := fetchUpdateMetadataFromNetwork(context.Background(), "https://example.test/update.json")
	if err != nil || metadata.Summary != "Update summary" {
		t.Fatalf("update metadata = %#v, %v", metadata, err)
	}

	http.DefaultClient = &http.Client{Transport: updateNetworkRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})}
	if _, err := fetchLatestReleaseFromGitHub(context.Background(), "threadgrid/threadpoint"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("latest error = %v", err)
	}
	if _, err := fetchReleasesFromGitHub(context.Background(), "threadgrid/threadpoint"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("releases error = %v", err)
	}
	if _, err := fetchUpdateMetadataFromNetwork(context.Background(), "https://example.test/update.json"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("metadata error = %v", err)
	}
	if _, err := fetchUpdateMetadataFromNetwork(context.Background(), " "); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing metadata asset error = %v", err)
	}
}
