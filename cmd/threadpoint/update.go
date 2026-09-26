// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/threadgrid/threadpoint/internal/home"
)

const (
	defaultUpdateRepo                   = "threadgrid/threadpoint"
	defaultExplicitUpdateCacheMaxAge    = 15 * time.Minute
	defaultAutomaticReminderInterval    = 24 * time.Hour
	defaultAutomaticReminderCacheMaxAge = 24 * time.Hour
	defaultUpdateNetworkTimeout         = 15 * time.Second

	maxReleaseMetadataBytes = 1 << 20
	maxUpdateMetadataBytes  = 64 << 10

	expectedUpdateMetadataSchema = "threadpoint.update.v1"
	expectedUpdateCacheSchema    = "threadpoint.update-cache.v1"

	updateChannelStable  = "stable"
	updateChannelPreview = "preview"
)

type releaseAssetMetadata struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseMetadata struct {
	TagName    string                 `json:"tag_name"`
	Name       string                 `json:"name"`
	HTMLURL    string                 `json:"html_url"`
	Body       string                 `json:"body"`
	Prerelease bool                   `json:"prerelease"`
	Draft      bool                   `json:"draft"`
	Published  time.Time              `json:"published_at"`
	Assets     []releaseAssetMetadata `json:"assets"`
	Update     updateMetadata         `json:"-"`
}

type updateMetadata struct {
	SchemaVersion string `json:"schema_version"`
	Product       string `json:"product"`
	Version       string `json:"version"`
	Channel       string `json:"channel"`
	Critical      bool   `json:"critical"`
	Summary       string `json:"summary"`
	LearnMoreURL  string `json:"learn_more_url"`
}

type updateCacheMetadata struct {
	SchemaVersion           string    `json:"schema_version"`
	Repo                    string    `json:"repo"`
	Tag                     string    `json:"tag"`
	HTMLURL                 string    `json:"html_url"`
	Channel                 string    `json:"channel,omitempty"`
	Critical                bool      `json:"critical,omitempty"`
	Summary                 string    `json:"summary,omitempty"`
	LearnMoreURL            string    `json:"learn_more_url,omitempty"`
	Prerelease              bool      `json:"prerelease,omitempty"`
	Published               time.Time `json:"published_at,omitempty"`
	NewerStableReleaseCount int       `json:"newer_stable_release_count,omitempty"`
	CheckedAt               time.Time `json:"checked_at"`
}

type updateCheckReport struct {
	Command                 string `json:"command"`
	Operation               string `json:"operation,omitempty"`
	Repo                    string `json:"repo"`
	Channel                 string `json:"channel"`
	CurrentVersion          string `json:"current_version"`
	LatestVersion           string `json:"latest_version"`
	CandidateVersion        string `json:"candidate_version,omitempty"`
	CheckedAt               string `json:"checked_at"`
	Source                  string `json:"source"`
	UpdateAvailable         bool   `json:"update_available"`
	Critical                bool   `json:"critical,omitempty"`
	Summary                 string `json:"summary,omitempty"`
	LearnMoreURL            string `json:"learn_more_url,omitempty"`
	ReleaseURL              string `json:"release_url,omitempty"`
	LatestPublishedAt       string `json:"latest_published_at,omitempty"`
	NewerStableReleaseCount int    `json:"newer_stable_release_count,omitempty"`
	ForcedReminder          bool   `json:"forced_reminder,omitempty"`
	Dismissed               bool   `json:"dismissed,omitempty"`
	Code                    string `json:"code,omitempty"`
	Message                 string `json:"message,omitempty"`
	Reason                  string `json:"-"`
	Banner                  string `json:"notification_banner,omitempty"`
}

// releaseClient injects update dependencies so command tests avoid mutable
// package globals.
type releaseClient struct {
	currentVersion    func() string
	fetchLatest       func(ctx context.Context, repo string) (releaseMetadata, error)
	fetchReleases     func(ctx context.Context, repo string) ([]releaseMetadata, error)
	fetchUpdate       func(ctx context.Context, source string) (updateMetadata, error)
	deriveReleaseBase func(repo, tag string) string
	download          func(ctx context.Context, source, destination string) error
	verifySignature   func(checksumsPath, signaturePath string) error
	verifyAttestation func(ctx context.Context, repo, tag, artifactPath string) error
	rename            func(oldpath, newpath string) error
}

func defaultReleaseClient() releaseClient {
	return releaseClient{
		currentVersion:    currentBinaryVersion,
		fetchLatest:       fetchLatestReleaseFromGitHub,
		fetchReleases:     fetchReleasesFromGitHub,
		fetchUpdate:       fetchUpdateMetadataFromNetwork,
		deriveReleaseBase: defaultDeriveReleaseBase,
		download: func(ctx context.Context, source, destination string) error {
			return downloadToPathFromNetwork(ctx, source, destination, defaultMaxDownloadBytes)
		},
		verifySignature:   verifyReleaseChecksumsSignature,
		verifyAttestation: verifyGitHubArtifactAttestation,
		rename:            os.Rename,
	}
}

func normalizeReleaseClient(client releaseClient) releaseClient {
	if client.currentVersion == nil {
		client.currentVersion = currentBinaryVersion
	}
	if client.fetchLatest == nil {
		client.fetchLatest = fetchLatestReleaseFromGitHub
	}
	if client.fetchReleases == nil {
		client.fetchReleases = func(ctx context.Context, repo string) ([]releaseMetadata, error) {
			latest, err := client.fetchLatest(ctx, repo)
			if err != nil {
				return nil, err
			}
			return []releaseMetadata{latest}, nil
		}
	}
	if client.fetchUpdate == nil {
		client.fetchUpdate = fetchUpdateMetadataFromNetwork
	}
	if client.deriveReleaseBase == nil {
		client.deriveReleaseBase = defaultDeriveReleaseBase
	}
	if client.download == nil {
		client.download = func(ctx context.Context, source, destination string) error {
			return downloadToPathFromNetwork(ctx, source, destination, defaultMaxDownloadBytes)
		}
	}
	if client.verifySignature == nil {
		client.verifySignature = verifyReleaseChecksumsSignature
	}
	if client.verifyAttestation == nil {
		client.verifyAttestation = verifyGitHubArtifactAttestation
	}
	if client.rename == nil {
		client.rename = os.Rename
	}
	return client
}

type updateCheckOptions struct {
	Channel         string
	ThreadpointHome string
	UseCache        bool
	CacheOnly       bool
	MaxAge          time.Duration
}

func runUpdate(ctx context.Context, app *cli, args []string) error {
	if len(args) == 0 {
		return runUpdateApply(ctx, app, args, updateOperationUpdate)
	}
	switch args[0] {
	case "check":
		return runUpdateCheck(ctx, app, args[1:])
	case "rollback":
		return runUpdateApply(ctx, app, args[1:], updateOperationRollback)
	case "reminder":
		return runUpdateReminders(app, args[1:])
	default:
		if strings.HasPrefix(args[0], "-") {
			return runUpdateApply(ctx, app, args, updateOperationUpdate)
		}
		return usageErrorf("unknown update subcommand %q", args[0])
	}
}

func runUpdateCheckHelp(app *cli) error {
	fmt.Fprintln(app.stderr, "Check for newer official releases and display non-blocking update notices.")
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Usage:")
	fmt.Fprintln(app.stderr, "  threadpoint update check [--repo owner/repo] [--channel stable|preview] [--format text|json] [--max-age duration] [--no-cache]")
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Options:")
	fmt.Fprintln(app.stderr, `  --repo owner/repo       release repository to inspect (default "threadgrid/threadpoint")`)
	fmt.Fprintln(app.stderr, `  --channel stable|preview release channel to inspect (default "stable")`)
	fmt.Fprintln(app.stderr, `  --format text|json      output format (default "text")`)
	fmt.Fprintln(app.stderr, `  --max-age duration      cache freshness window (default "15m")`)
	fmt.Fprintln(app.stderr, `  --no-cache              skip any update cache`)
	return nil
}

func runUpdateCheck(ctx context.Context, app *cli, args []string) error {
	fs := flag.NewFlagSet("update check", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	repo := fs.String("repo", defaultUpdateRepo, "release repository")
	channel := fs.String("channel", updateChannelStable, "release channel")
	format := fs.String("format", app.format, "text or json")
	maxAgeRaw := fs.String("max-age", defaultExplicitUpdateCacheMaxAge.String(), "maximum accepted cache age")
	noCache := fs.Bool("no-cache", false, "skip cache and force direct metadata check")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("update check does not accept positional arguments")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "update")
	if err != nil {
		return usageError(err)
	}
	cleanRepo := strings.TrimSpace(*repo)
	if cleanRepo == "" {
		return usageErrorf("update check requires --repo")
	}
	maxAge, err := time.ParseDuration(*maxAgeRaw)
	if err != nil {
		return usageErrorf("invalid --max-age: %v", err)
	}
	if maxAge < 0 {
		return usageErrorf("invalid --max-age: must be zero or positive")
	}

	cleanChannel, err := normalizeUpdateChannel(*channel)
	if err != nil {
		return usageError(err)
	}
	productHome, err := app.productHome()
	if err != nil {
		return err
	}

	report := runUpdateCheckReportWithOptions(ctx, app.release, cleanRepo, updateCheckOptions{
		Channel:         cleanChannel,
		ThreadpointHome: productHome,
		UseCache:        !*noCache,
		MaxAge:          maxAge,
	})
	if report.Reason == "" && report.LatestVersion == "" {
		report.Reason = "could not determine latest release metadata"
	}
	if report.CheckedAt == "" {
		report.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	}
	report.CurrentVersion = app.release.currentVersion()
	report.Command = "threadpoint update check"
	report.Operation = "check"
	report.Channel = cleanChannel
	normalizeUpdateCheckOutcome(&report)

	switch outputFormat {
	case outputFormatText:
		printUpdateCheckText(app.stdout, report)
	case outputFormatJSON:
		if err := app.printJSON(report); err != nil {
			return err
		}
	}
	return nil
}

func runUpdateCheckReportWithOptions(ctx context.Context, client releaseClient, repo string, opts updateCheckOptions) updateCheckReport {
	client = normalizeReleaseClient(client)
	channel := opts.Channel
	if channel == "" {
		channel = updateChannelStable
	}
	now := time.Now().UTC()
	report := updateCheckReport{
		Repo:      repo,
		Channel:   channel,
		Source:    "none",
		CheckedAt: now.Format(time.RFC3339),
	}

	cachePath, canCache := updateCachePathWithHome(opts.ThreadpointHome, repo, channel)
	var cached *updateCacheMetadata
	var cacheRecordErr error
	var cacheFresh bool

	if opts.UseCache && canCache {
		record, found, err := readUpdateCache(cachePath)
		if err != nil {
			cacheRecordErr = err
		} else if found {
			age := now.Sub(record.CheckedAt)
			if age < 0 {
				cacheRecordErr = errors.New("cached update metadata has a future checked_at timestamp")
			} else {
				cached = &record
				cacheFresh = age <= opts.MaxAge && opts.MaxAge > 0
			}
		}
	}

	freshCache := cached != nil && cacheFresh
	if freshCache {
		report.Source = "cache"
		report.LatestVersion = cached.Tag
		report.CandidateVersion = cached.Tag
		report.Channel = cacheChannel(cached.Channel, channel)
		report.Critical = cached.Critical
		report.Summary = sanitizeUpdateText(cached.Summary)
		report.LearnMoreURL = trustedUpdateLearnMoreURL(repo, cached.LearnMoreURL)
		report.ReleaseURL = cached.HTMLURL
		report.LatestPublishedAt = formatOptionalTime(cached.Published)
		report.NewerStableReleaseCount = cached.NewerStableReleaseCount
		report.Reason = ""
		updateCheckState(&report, client.currentVersion(), cached.Tag, opts.ThreadpointHome)
		return report
	}

	if opts.CacheOnly {
		report.Reason = "no fresh cached update metadata"
		if cacheRecordErr != nil {
			report.Reason = appendReason(report.Reason, fmt.Sprintf("cache disabled or unreadable: %v", cacheRecordErr))
		}
		return report
	}

	releases, err := client.fetchReleases(ctx, repo)
	if err != nil {
		if cached != nil {
			report.Source = "stale-cache"
			report.LatestVersion = cached.Tag
			report.CandidateVersion = cached.Tag
			report.Channel = cacheChannel(cached.Channel, channel)
			report.Critical = cached.Critical
			report.Summary = sanitizeUpdateText(cached.Summary)
			report.LearnMoreURL = trustedUpdateLearnMoreURL(repo, cached.LearnMoreURL)
			report.ReleaseURL = cached.HTMLURL
			report.LatestPublishedAt = formatOptionalTime(cached.Published)
			report.NewerStableReleaseCount = cached.NewerStableReleaseCount
			report.Reason = appendReason(report.Reason, fmt.Sprintf("could not fetch latest release metadata: %v", err))
			updateCheckState(&report, client.currentVersion(), cached.Tag, opts.ThreadpointHome)
			return report
		}
		reason := fmt.Sprintf("could not fetch latest release metadata: %v", err)
		if cacheRecordErr != nil {
			reason = fmt.Sprintf("%s (cache disabled or unreadable: %v)", reason, cacheRecordErr)
		}
		report.Reason = reason
		return report
	}
	latest, found := selectReleaseForChannel(releases, channel)
	if !found {
		report.Reason = fmt.Sprintf("could not determine latest %s release metadata", channel)
		return report
	}
	if metadata, err := loadReleaseUpdateMetadata(ctx, client, repo, latest, channel); err == nil {
		latest.Update = metadata
	} else {
		report.Reason = appendReason(report.Reason, fmt.Sprintf("could not fetch update metadata: %v", err))
	}
	applyReleaseMetadata(&latest, channel)

	report.Source = "network"
	report.LatestVersion = latest.TagName
	report.CandidateVersion = latest.TagName
	report.Channel = channel
	report.Critical = latest.Update.Critical
	report.Summary = sanitizeUpdateText(latest.Update.Summary)
	report.LearnMoreURL = trustedUpdateLearnMoreURL(repo, latest.Update.LearnMoreURL)
	report.ReleaseURL = latest.HTMLURL
	report.LatestPublishedAt = formatOptionalTime(latest.Published)
	report.NewerStableReleaseCount = countNewerStableReleases(releases, client.currentVersion())
	if canCache {
		_ = writeUpdateCache(cachePath, repo, latest, report.NewerStableReleaseCount)
	}
	updateCheckState(&report, client.currentVersion(), latest.TagName, opts.ThreadpointHome)
	return report
}

func updateCheckState(report *updateCheckReport, currentVersion, latestVersion string, productHome string) {
	existingReason := report.Reason
	latestSemVer, latestErr := parseSemVer(latestVersion)
	currentSemVer, currentErr := parseSemVer(currentVersion)
	if latestErr != nil {
		report.Reason = appendReason(existingReason, fmt.Sprintf("latest version %q is not semver", latestVersion))
		return
	}
	if currentErr != nil {
		report.Reason = appendReason(existingReason, fmt.Sprintf("current version %q is not semver", currentVersion))
		return
	}
	switch compareSemVer(currentSemVer, latestSemVer) {
	case 0:
		report.Reason = appendReason(existingReason, "already up to date")
	case 1:
		report.Reason = appendReason(existingReason, "running newer release than published metadata")
	case -1:
		report.UpdateAvailable = true
		report.Banner = bannerText(*report, productHome)
		report.Reason = appendReason(existingReason, "newer release available")
	}
}

func appendReason(existing string, extra string) string {
	if existing == "" {
		return extra
	}
	return fmt.Sprintf("%s; %s", existing, extra)
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func bannerText(report updateCheckReport, productHome string) string {
	version := report.LatestVersion
	if version == "" {
		version = report.CandidateVersion
	}
	channel := report.Channel
	if channel == "" {
		channel = updateChannelStable
	}
	lines := []string{fmt.Sprintf("A newer threadpoint %s release is available: %s.", channel, version)}
	if report.Critical {
		lines = append(lines, "This update is marked critical.")
	}
	if strings.TrimSpace(report.Summary) != "" {
		lines = append(lines, fmt.Sprintf("What's new: %s", strings.TrimSpace(report.Summary)))
	}
	if strings.TrimSpace(report.LearnMoreURL) != "" {
		lines = append(lines, fmt.Sprintf("Learn more: %s", strings.TrimSpace(report.LearnMoreURL)))
	}
	if channel == updateChannelPreview {
		lines = append(lines, "Preview releases may be unstable. Run `threadpoint update --channel preview` only if you explicitly want preview builds.")
		return strings.Join(lines, "\n")
	}
	if currentBinaryIsInstallerManaged(productHome) {
		lines = append(lines, "Run `threadpoint update` to update this installer-managed install.")
	} else {
		lines = append(lines, "Install it with the threadpoint installer or replace this manual release archive.")
	}
	return strings.Join(lines, "\n")
}

func currentBinaryIsInstallerManaged(productHome string) bool {
	source, _ := detectInstallSource(currentExecutablePath(), productHome)
	return source == installerChannelScript
}

func printUpdateCheckText(stdout io.Writer, report updateCheckReport) {
	normalizeUpdateCheckOutcome(&report)
	fmt.Fprintf(stdout, "command: %s\n", report.Command)
	fmt.Fprintf(stdout, "  repo: %s\n", report.Repo)
	fmt.Fprintf(stdout, "  current: %s\n", report.CurrentVersion)
	if report.LatestVersion != "" {
		fmt.Fprintf(stdout, "  latest: %s\n", report.LatestVersion)
	}
	if report.Channel != "" {
		fmt.Fprintf(stdout, "  channel: %s\n", report.Channel)
	}
	fmt.Fprintf(stdout, "  source: %s\n", report.Source)
	fmt.Fprintf(stdout, "  checked at: %s\n", report.CheckedAt)
	fmt.Fprintf(stdout, "  update available: %v\n", report.UpdateAvailable)
	if report.Critical {
		fmt.Fprintf(stdout, "  critical: true\n")
	}
	if report.Summary != "" {
		fmt.Fprintf(stdout, "  what's new: %s\n", report.Summary)
	}
	if report.LearnMoreURL != "" {
		fmt.Fprintf(stdout, "  learn more: %s\n", report.LearnMoreURL)
	}
	if report.Code != "" {
		fmt.Fprintf(stdout, "  code: %s\n", report.Code)
	}
	if report.Message != "" {
		fmt.Fprintf(stdout, "  message: %s\n", report.Message)
	}
	if report.Banner != "" {
		fmt.Fprintf(stdout, "\n%s\n", report.Banner)
	}
}

func normalizeUpdateCheckOutcome(report *updateCheckReport) {
	if report.UpdateAvailable {
		report.Code = "update_available"
		report.Message = "A newer release is available."
		return
	}
	if report.LatestVersion == "" {
		report.Code = "update_metadata_unavailable"
		report.Message = "Update metadata is unavailable."
		return
	}
	report.Code = "update_current"
	report.Message = "This installation is current."
}

func parseSemVer(raw string) (versionParts, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return versionParts{}, errors.New("version is empty")
	}
	raw = strings.TrimPrefix(raw, "v")
	if i := strings.IndexByte(raw, '+'); i >= 0 {
		raw = raw[:i] // build metadata does not affect precedence
	}
	core, pre, hasPre := strings.Cut(raw, "-")
	digits := strings.Split(core, ".")
	if len(digits) < 3 {
		return versionParts{}, errors.New("version must include major, minor, and patch")
	}
	major, err := strconv.Atoi(digits[0])
	if err != nil {
		return versionParts{}, err
	}
	minor, err := strconv.Atoi(digits[1])
	if err != nil {
		return versionParts{}, err
	}
	patch, err := strconv.Atoi(digits[2])
	if err != nil {
		return versionParts{}, err
	}
	var prerelease []string
	if hasPre {
		if strings.TrimSpace(pre) == "" {
			return versionParts{}, errors.New("version has an empty prerelease")
		}
		prerelease = strings.Split(pre, ".")
	}
	return versionParts{major: major, minor: minor, patch: patch, prerelease: prerelease}, nil
}

type versionParts struct {
	major      int
	minor      int
	patch      int
	prerelease []string
}

func compareSemVer(left, right versionParts) int {
	if c := compareInt(left.major, right.major); c != 0 {
		return c
	}
	if c := compareInt(left.minor, right.minor); c != 0 {
		return c
	}
	if c := compareInt(left.patch, right.patch); c != 0 {
		return c
	}
	return comparePrerelease(left.prerelease, right.prerelease)
}

func compareInt(left, right int) int {
	switch {
	case left > right:
		return 1
	case left < right:
		return -1
	default:
		return 0
	}
}

// comparePrerelease implements semver §11 precedence among prerelease tags: a
// version with no prerelease outranks one that has a prerelease; otherwise the
// dot-separated identifiers are compared left to right (numeric identifiers rank
// below non-numeric, numerics compared numerically, others by ASCII), and when
// one list is a prefix of the other the longer list wins.
func comparePrerelease(left, right []string) int {
	if len(left) == 0 && len(right) == 0 {
		return 0
	}
	if len(left) == 0 {
		return 1
	}
	if len(right) == 0 {
		return -1
	}
	for i := 0; i < len(left) && i < len(right); i++ {
		if c := compareIdentifier(left[i], right[i]); c != 0 {
			return c
		}
	}
	return compareInt(len(left), len(right))
}

func compareIdentifier(left, right string) int {
	leftNum, leftErr := strconv.Atoi(left)
	rightNum, rightErr := strconv.Atoi(right)
	leftNumeric := leftErr == nil
	rightNumeric := rightErr == nil
	switch {
	case leftNumeric && rightNumeric:
		return compareInt(leftNum, rightNum)
	case leftNumeric:
		return -1
	case rightNumeric:
		return 1
	default:
		return strings.Compare(left, right)
	}
}

func currentBinaryVersion() string {
	return buildVersion()
}

func updateCachePath(repo string, parts ...string) (string, bool) {
	return updateCachePathWithHome("", repo, parts...)
}

func updateCachePathWithHome(productHome string, repo string, parts ...string) (string, bool) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", false
	}
	threadpointHome, err := resolveThreadpointHomeForPath(productHome)
	if err != nil {
		return "", false
	}
	key := repo
	for _, part := range parts {
		if clean := strings.TrimSpace(part); clean != "" {
			if clean == updateChannelStable {
				continue
			}
			key += "\x00" + clean
		}
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(threadpointHome, "updates", "cache", hex.EncodeToString(sum[:])+".json"), true
}

func resolveThreadpointHomeForPath(productHome string) (string, error) {
	return home.ResolveWithOverride("", productHome)
}

func readUpdateCache(path string) (updateCacheMetadata, bool, error) {
	return readUpdateCacheLocked(path)
}

func writeUpdateCache(path string, repo string, release releaseMetadata, newerStableReleaseCount int) error {
	if path == "" {
		return nil
	}
	release.Update = sanitizeUpdateMetadata(release.Update)
	channel := release.Update.Channel
	if channel == "" {
		channel = updateChannelStable
		if release.Prerelease {
			channel = updateChannelPreview
		}
	}
	record := updateCacheMetadata{
		SchemaVersion:           expectedUpdateCacheSchema,
		Repo:                    repo,
		Tag:                     release.TagName,
		HTMLURL:                 release.HTMLURL,
		Channel:                 channel,
		Critical:                release.Update.Critical,
		Summary:                 release.Update.Summary,
		LearnMoreURL:            release.Update.LearnMoreURL,
		Prerelease:              release.Prerelease,
		Published:               release.Published,
		NewerStableReleaseCount: newerStableReleaseCount,
		CheckedAt:               time.Now().UTC(),
	}
	return writeUpdateCacheLocked(path, record)
}

func fetchLatestReleaseFromGitHub(ctx context.Context, repo string) (releaseMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultUpdateNetworkTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return releaseMetadata{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "threadpoint-update-check")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return releaseMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return releaseMetadata{}, fmt.Errorf("release metadata request failed with status %s", response.Status)
	}
	var payload releaseMetadata
	if err := json.NewDecoder(io.LimitReader(response.Body, maxReleaseMetadataBytes)).Decode(&payload); err != nil {
		return releaseMetadata{}, err
	}
	if strings.TrimSpace(payload.TagName) == "" {
		return releaseMetadata{}, errors.New("release metadata is missing tag_name")
	}
	return payload, nil
}

func fetchReleasesFromGitHub(ctx context.Context, repo string) ([]releaseMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultUpdateNetworkTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=30", repo)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "threadpoint-update-check")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release metadata request failed with status %s", response.Status)
	}
	var payload []releaseMetadata
	if err := json.NewDecoder(io.LimitReader(response.Body, maxReleaseMetadataBytes)).Decode(&payload); err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, errors.New("release metadata is empty")
	}
	return payload, nil
}

func fetchUpdateMetadataFromNetwork(ctx context.Context, source string) (updateMetadata, error) {
	if strings.TrimSpace(source) == "" {
		return updateMetadata{}, errors.New("update metadata asset is missing")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultUpdateNetworkTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return updateMetadata{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "threadpoint-update-check")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return updateMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return updateMetadata{}, fmt.Errorf("update metadata request failed with status %s", response.Status)
	}
	var metadata updateMetadata
	if err := json.NewDecoder(io.LimitReader(response.Body, maxUpdateMetadataBytes)).Decode(&metadata); err != nil {
		return updateMetadata{}, err
	}
	return metadata, nil
}

func normalizeUpdateChannel(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", updateChannelStable:
		return updateChannelStable, nil
	case updateChannelPreview:
		return updateChannelPreview, nil
	default:
		return "", fmt.Errorf("unknown update channel %q", raw)
	}
}

func cacheChannel(cached string, configuredDefault string) string {
	if strings.TrimSpace(cached) != "" {
		return cached
	}
	if strings.TrimSpace(configuredDefault) != "" {
		return configuredDefault
	}
	return updateChannelStable
}

func selectReleaseForChannel(releases []releaseMetadata, channel string) (releaseMetadata, bool) {
	var selected releaseMetadata
	found := false
	for _, release := range releases {
		if release.Draft || strings.TrimSpace(release.TagName) == "" {
			continue
		}
		switch channel {
		case updateChannelPreview:
			if !release.Prerelease {
				continue
			}
		default:
			if release.Prerelease {
				continue
			}
		}
		if !found || releaseSemVerGreater(release.TagName, selected.TagName) {
			selected = release
			found = true
		}
	}
	return selected, found
}

func releaseSemVerGreater(left string, right string) bool {
	leftVersion, leftErr := parseSemVer(left)
	rightVersion, rightErr := parseSemVer(right)
	switch {
	case leftErr == nil && rightErr == nil:
		return compareSemVer(leftVersion, rightVersion) > 0
	case leftErr == nil:
		return true
	case rightErr == nil:
		return false
	default:
		return strings.Compare(left, right) > 0
	}
}

func loadReleaseUpdateMetadata(ctx context.Context, client releaseClient, repo string, release releaseMetadata, channel string) (updateMetadata, error) {
	assetURL := ""
	for _, asset := range release.Assets {
		if asset.Name == "update.json" {
			assetURL = asset.BrowserDownloadURL
			break
		}
	}
	if assetURL == "" {
		return defaultUpdateMetadata(repo, release, channel), nil
	}
	metadata, err := client.fetchUpdate(ctx, assetURL)
	if err != nil {
		return updateMetadata{}, err
	}
	metadata = sanitizeUpdateMetadata(metadata)
	if err := validateUpdateMetadata(repo, release, channel, metadata); err != nil {
		return updateMetadata{}, err
	}
	return metadata, nil
}

func defaultUpdateMetadata(repo string, release releaseMetadata, channel string) updateMetadata {
	summary := strings.TrimSpace(release.Name)
	if summary == "" {
		summary = firstReleaseBodyLine(release.Body)
	}
	return updateMetadata{
		SchemaVersion: expectedUpdateMetadataSchema,
		Product:       productFromRepo(repo),
		Version:       release.TagName,
		Channel:       channel,
		Summary:       sanitizeUpdateText(summary),
		LearnMoreURL:  sanitizeUpdateText(release.HTMLURL),
	}
}

func validateUpdateMetadata(repo string, release releaseMetadata, channel string, metadata updateMetadata) error {
	metadata = sanitizeUpdateMetadata(metadata)
	if strings.TrimSpace(metadata.SchemaVersion) == "" {
		return errors.New("update metadata is missing schema_version")
	}
	if metadata.SchemaVersion != expectedUpdateMetadataSchema {
		return fmt.Errorf("update metadata schema_version %q does not match %q", metadata.SchemaVersion, expectedUpdateMetadataSchema)
	}
	if product := productFromRepo(repo); strings.TrimSpace(metadata.Product) != "" && metadata.Product != product {
		return fmt.Errorf("update metadata product %q does not match %q", metadata.Product, product)
	}
	if strings.TrimSpace(metadata.Version) != "" && metadata.Version != release.TagName {
		return fmt.Errorf("update metadata version %q does not match release %q", metadata.Version, release.TagName)
	}
	if metadata.Channel != "" && metadata.Channel != channel {
		return fmt.Errorf("update metadata channel %q does not match selected %q", metadata.Channel, channel)
	}
	if channel == updateChannelPreview && !release.Prerelease {
		return errors.New("preview update metadata points at a non-prerelease GitHub release")
	}
	if channel == updateChannelStable && release.Prerelease {
		return errors.New("stable update metadata points at a prerelease GitHub release")
	}
	if err := validateUpdateLearnMoreURL(repo, metadata.LearnMoreURL); err != nil {
		return err
	}
	return nil
}

func applyReleaseMetadata(release *releaseMetadata, channel string) {
	if release.Update.Channel == "" {
		release.Update.Channel = channel
	}
	if release.Update.Version == "" {
		release.Update.Version = release.TagName
	}
	if release.Update.LearnMoreURL == "" {
		release.Update.LearnMoreURL = sanitizeUpdateText(release.HTMLURL)
	}
	release.Update = sanitizeUpdateMetadata(release.Update)
}

func sanitizeUpdateMetadata(metadata updateMetadata) updateMetadata {
	metadata.SchemaVersion = strings.TrimSpace(metadata.SchemaVersion)
	metadata.Product = strings.TrimSpace(metadata.Product)
	metadata.Version = strings.TrimSpace(metadata.Version)
	metadata.Channel = strings.TrimSpace(metadata.Channel)
	metadata.Summary = sanitizeUpdateText(metadata.Summary)
	metadata.LearnMoreURL = sanitizeUpdateText(metadata.LearnMoreURL)
	return metadata
}

func sanitizeUpdateText(raw string) string {
	raw = strings.TrimSpace(raw)
	var builder strings.Builder
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			continue
		}
		builder.WriteRune(r)
	}
	return strings.TrimSpace(builder.String())
}

func trustedUpdateLearnMoreURL(repo string, raw string) string {
	clean := sanitizeUpdateText(raw)
	if err := validateUpdateLearnMoreURL(repo, clean); err != nil {
		return ""
	}
	return clean
}

func updateReleaseTagURL(repo, tag string) string {
	return fmt.Sprintf("https://github.com/%s/releases/tag/%s", repo, tag)
}

func validateUpdateLearnMoreURL(repo string, raw string) error {
	clean := sanitizeUpdateText(raw)
	if clean == "" {
		return nil
	}
	parsed, err := url.Parse(clean)
	if err != nil {
		return fmt.Errorf("update metadata learn_more_url is invalid: %w", err)
	}
	repoPath := "/" + strings.Trim(strings.TrimSpace(repo), "/")
	releasesPath := repoPath + "/releases"
	if parsed.Scheme != "https" || parsed.Host != "github.com" || (parsed.Path != releasesPath && !strings.HasPrefix(parsed.Path, releasesPath+"/")) {
		return fmt.Errorf("update metadata learn_more_url must use https://github.com%s", releasesPath)
	}
	return nil
}

func productFromRepo(_ string) string {
	return "threadpoint"
}

func firstReleaseBodyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		clean := strings.TrimSpace(strings.Trim(line, "#*- "))
		if clean != "" {
			return clean
		}
	}
	return ""
}

func countNewerStableReleases(releases []releaseMetadata, currentVersion string) int {
	current, err := parseSemVer(currentVersion)
	if err != nil {
		return 0
	}
	count := 0
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		version, err := parseSemVer(release.TagName)
		if err != nil {
			continue
		}
		if compareSemVer(version, current) > 0 {
			count++
		}
	}
	return count
}
