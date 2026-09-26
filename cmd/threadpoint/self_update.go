// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/internal/releasesig"
)

type updateOperation string

const (
	updateOperationUpdate   updateOperation = "update"
	updateOperationRollback updateOperation = "rollback"
)

type selfUpdateReport struct {
	Command               string `json:"command"`
	Operation             string `json:"operation,omitempty"`
	Repo                  string `json:"repo"`
	Channel               string `json:"channel,omitempty"`
	InstallDir            string `json:"install_dir"`
	BinaryPath            string `json:"binary_path"`
	LinkPath              string `json:"link_path,omitempty"`
	MetadataPath          string `json:"metadata_path"`
	CurrentVersion        string `json:"current_version"`
	LatestVersion         string `json:"latest_version"`
	CandidateVersion      string `json:"candidate_version,omitempty"`
	RollbackTargetVersion string `json:"rollback_target_version,omitempty"`
	CheckedAt             string `json:"checked_at"`
	Source                string `json:"source"`
	UpdateAvailable       bool   `json:"update_available"`
	Updated               bool   `json:"updated"`
	Archive               string `json:"archive"`
	ArchiveSHA256         string `json:"archive_sha256,omitempty"`
	Critical              bool   `json:"critical,omitempty"`
	Summary               string `json:"summary,omitempty"`
	LearnMoreURL          string `json:"learn_more_url,omitempty"`
	Code                  string `json:"code"`
	Message               string `json:"message"`
	Reason                string `json:"-"`
}

func defaultDeriveReleaseBase(repo, tag string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, tag)
}

func runUpdateApply(ctx context.Context, app *cli, args []string, operation updateOperation) error {
	commandName := "update"
	if operation == updateOperationRollback {
		commandName = "update rollback"
	}
	fs := flag.NewFlagSet(commandName, flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	repo := fs.String("repo", defaultUpdateRepo, "release repository")
	productHome, err := app.productHome()
	if err != nil {
		return err
	}
	installDir := fs.String("dir", defaultUninstallDir(productHome), "command directory")
	channel := fs.String("channel", updateChannelStable, "release channel")
	format := fs.String("format", app.format, "text or json")
	maxAgeRaw := fs.String("max-age", defaultExplicitUpdateCacheMaxAge.String(), "maximum accepted cache age")
	noCache := fs.Bool("no-cache", false, "skip cache and force direct metadata check")
	yes := fs.Bool("yes", false, "confirm preview updates for scripted use")
	skipAttestation := fs.Bool("skip-attestation", envFlagEnabled("THREADPOINT_SKIP_ATTESTATION"), "skip GitHub Artifact Attestation verification")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("%s does not accept positional arguments", commandName)
	}
	if operation == updateOperationRollback && flagProvided(fs, "channel") {
		return usageErrorf("update rollback does not accept --channel; rollback always installs the latest stable release")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "update")
	if err != nil {
		return usageError(err)
	}

	cleanRepo := strings.TrimSpace(*repo)
	if cleanRepo == "" {
		return usageErrorf("%s requires --repo", commandName)
	}
	cleanDir := strings.TrimSpace(*installDir)
	if cleanDir == "" {
		return usageErrorf("%s requires --dir", commandName)
	}
	cleanChannel := updateChannelStable
	if operation != updateOperationRollback {
		var err error
		cleanChannel, err = normalizeUpdateChannel(*channel)
		if err != nil {
			return usageError(err)
		}
	}
	maxAge, err := time.ParseDuration(*maxAgeRaw)
	if err != nil {
		return usageErrorf("invalid --max-age: %v", err)
	}
	if maxAge < 0 {
		return usageErrorf("invalid --max-age: must be zero or positive")
	}

	paths, err := resolveManagedInstallPaths(cleanDir)
	if err != nil {
		return usageError(err)
	}
	installLock, err := acquireInstallLifecycleLock(ctx, productHome, paths.CommandPath)
	if err != nil {
		return refusedError(fmt.Errorf("could not acquire install lifecycle lock: %w", err))
	}
	defer func() { _ = installLock.Release() }()
	if err := recoverInstallTransaction(productHome, paths.CommandPath, installLock); err != nil {
		return refusedError(fmt.Errorf("could not recover interrupted install transaction: %w", err))
	}
	paths, err = resolveManagedInstallPaths(cleanDir)
	if err != nil {
		return refusedError(fmt.Errorf("could not resolve recovered managed install: %w", err))
	}

	metadataPath := installerMetadataPath(productHome, paths.BinaryPath)
	if _, statErr := os.Stat(metadataPath); errors.Is(statErr, os.ErrNotExist) {
		return refusedError(fmt.Errorf("refusing to update %s: no installer metadata at %s. This binary was not installed by the threadpoint install script (for example a `go install` or distro-packaged build), so the managed update command cannot manage it. Update it with: go install github.com/threadgrid/threadpoint/cmd/threadpoint@latest", paths.BinaryPath, metadataPath))
	}
	metadata, err := readInstallMetadata(metadataPath)
	if err != nil {
		return refusedError(err)
	}
	if err := verifyInstallOwnership(metadata, paths.BinaryPath); err != nil {
		return refusedError(err)
	}
	linkPath, _, err := verifiedInstallerLinkPath(metadata, paths.BinaryPath)
	if err != nil {
		return refusedError(err)
	}
	if metadata.Repo != cleanRepo {
		return refusedError(fmt.Errorf("refusing to update: installer metadata repo %q does not match --repo %q", metadata.Repo, cleanRepo))
	}
	currentHash, err := installerManagedBinarySHA256(metadata, installLock)
	if err != nil {
		return refusedError(fmt.Errorf("refusing to update: %w", err))
	}
	if currentHash != metadata.BinarySHA256 {
		return refusedError(fmt.Errorf("refusing to update: current binary checksum does not match installer metadata for %s", paths.BinaryPath))
	}

	app.release = normalizeReleaseClient(app.release)
	checkReport := runUpdateCheckReportWithOptions(ctx, app.release, cleanRepo, updateCheckOptions{
		Channel:         cleanChannel,
		ThreadpointHome: productHome,
		UseCache:        !*noCache,
		MaxAge:          maxAge,
	})
	checkReport.Command = "threadpoint " + commandName
	checkReport.CurrentVersion = app.release.currentVersion()
	checkReport.Repo = cleanRepo
	if operation == updateOperationRollback && checkReport.LatestVersion != "" {
		checkReport.UpdateAvailable = checkReport.CurrentVersion != checkReport.LatestVersion
		checkReport.Reason = "latest stable release selected for rollback"
	}
	report := selfUpdateReport{
		Command:          "threadpoint " + commandName,
		Operation:        string(operation),
		Repo:             cleanRepo,
		Channel:          cleanChannel,
		InstallDir:       paths.InstallDir,
		BinaryPath:       paths.BinaryPath,
		LinkPath:         linkPath,
		MetadataPath:     metadataPath,
		CurrentVersion:   checkReport.CurrentVersion,
		LatestVersion:    checkReport.LatestVersion,
		CandidateVersion: checkReport.CandidateVersion,
		CheckedAt:        checkReport.CheckedAt,
		Source:           checkReport.Source,
		UpdateAvailable:  checkReport.UpdateAvailable,
		Critical:         checkReport.Critical,
		Summary:          checkReport.Summary,
		LearnMoreURL:     checkReport.LearnMoreURL,
		Reason:           checkReport.Reason,
	}
	if operation == updateOperationRollback {
		report.RollbackTargetVersion = checkReport.LatestVersion
	}

	if checkReport.LatestVersion == "" || checkReport.Source == "none" || checkReport.Source == "stale-cache" {
		report.UpdateAvailable = false
		switch {
		case report.Reason == "":
			report.Reason = "latest release metadata unavailable; update skipped"
		case checkReport.Source == "stale-cache":
			report.Reason = fmt.Sprintf("latest release metadata unavailable; update skipped; stale cache was not used for installation: %s", report.Reason)
		default:
			report.Reason = fmt.Sprintf("latest release metadata unavailable; update skipped: %s", report.Reason)
		}
		if err := printSelfUpdateReport(app, outputFormat, report); err != nil {
			return err
		}
		return nil
	}
	if !checkReport.UpdateAvailable {
		report.Archive = detectSelfUpdateArchiveName(checkReport.LatestVersion)
		if err := printSelfUpdateReport(app, outputFormat, report); err != nil {
			return err
		}
		return nil
	}
	switch {
	case cleanChannel == updateChannelPreview && operation == updateOperationUpdate:
		if err := confirmPreviewUpdate(app, *yes, report); err != nil {
			return err
		}
	case operation == updateOperationUpdate || operation == updateOperationRollback:
		if err := confirmManagedUpdate(app, *yes, report); err != nil {
			return err
		}
	}

	updatedReport, err := runSelfUpdateInstall(ctx, app.release, report, metadata, metadataPath, metadata.Repo, *skipAttestation, app.stderr, installLock)
	if err != nil {
		return err
	}
	report = *updatedReport
	return printSelfUpdateReport(app, outputFormat, report)
}

func runUpdateApplyHelp(app *cli, operation updateOperation) {
	if operation == updateOperationRollback {
		fmt.Fprintln(app.stderr, "Rollback an installer-managed threadpoint binary to the latest stable release.")
		fmt.Fprintln(app.stderr)
		fmt.Fprintln(app.stderr, "Usage:")
		fmt.Fprintln(app.stderr, "  threadpoint update rollback [--repo owner/repo] [--dir path] [--max-age duration] [--no-cache] [--format text|json] [--yes]")
		fmt.Fprintln(app.stderr)
		fmt.Fprintln(app.stderr, "Options:")
		fmt.Fprintln(app.stderr, `  --repo owner/repo            release repository to inspect (default "threadgrid/threadpoint")`)
		fmt.Fprintln(app.stderr, `  --dir path                   command directory`)
		fmt.Fprintln(app.stderr, `  --max-age duration           cache freshness window (default "15m")`)
		fmt.Fprintln(app.stderr, `  --no-cache                   skip update cache`)
		fmt.Fprintln(app.stderr, `  --format text|json           output format (default "text")`)
		fmt.Fprintln(app.stderr, `  --yes                        confirm rollback for noninteractive use`)
		return
	}
	fmt.Fprintln(app.stderr, "Update an installer-managed threadpoint binary from official release metadata.")
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Usage:")
	fmt.Fprintln(app.stderr, "  threadpoint update [--repo owner/repo] [--channel stable|preview] [--dir path] [--max-age duration] [--no-cache] [--format text|json] [--yes]")
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Options:")
	fmt.Fprintln(app.stderr, `  --repo owner/repo            release repository to inspect (default "threadgrid/threadpoint")`)
	fmt.Fprintln(app.stderr, `  --channel stable|preview     release channel to update from (default "stable")`)
	fmt.Fprintln(app.stderr, `  --dir path                   command directory`)
	fmt.Fprintln(app.stderr, `  --max-age duration           cache freshness window (default "15m")`)
	fmt.Fprintln(app.stderr, `  --no-cache                   skip update cache`)
	fmt.Fprintln(app.stderr, `  --format text|json           output format (default "text")`)
	fmt.Fprintln(app.stderr, `  --yes                        confirm update for noninteractive use`)
}

func confirmManagedUpdate(app *cli, yes bool, report selfUpdateReport) error {
	if yes {
		return nil
	}
	if app.nonInteractive || !interactiveInput(app.stdin) {
		return refusedError(errors.New("updates require explicit confirmation; rerun with --yes for noninteractive use"))
	}
	fmt.Fprintf(app.stderr, "Threadpoint release %s is available.\n", report.LatestVersion)
	if report.Summary != "" {
		fmt.Fprintf(app.stderr, "What's new: %s\n", report.Summary)
	}
	if report.LearnMoreURL != "" {
		fmt.Fprintf(app.stderr, "Learn more: %s\n", report.LearnMoreURL)
	}
	fmt.Fprint(app.stderr, "Proceed with the installer-managed update? [y/N]: ")
	line, err := bufio.NewReader(app.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return refusedError(fmt.Errorf("read update confirmation: %w", err))
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return refusedError(errors.New("update confirmation declined"))
	}
}

func confirmPreviewUpdate(app *cli, yes bool, report selfUpdateReport) error {
	if yes {
		return nil
	}
	if app.nonInteractive || !interactiveInput(app.stdin) {
		return refusedError(errors.New("preview updates require explicit confirmation; rerun with --yes for noninteractive use"))
	}
	fmt.Fprintf(app.stderr, "Preview release %s may be unstable.\n", report.LatestVersion)
	if report.Summary != "" {
		fmt.Fprintf(app.stderr, "What's new: %s\n", report.Summary)
	}
	if report.LearnMoreURL != "" {
		fmt.Fprintf(app.stderr, "Learn more: %s\n", report.LearnMoreURL)
	}
	fmt.Fprint(app.stderr, `Type "preview" to continue: `)
	line, err := bufio.NewReader(app.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return refusedError(fmt.Errorf("read preview confirmation: %w", err))
	}
	if strings.TrimSpace(line) != "preview" {
		return refusedError(errors.New("preview update confirmation declined"))
	}
	return nil
}

func runSelfUpdateInstall(
	ctx context.Context,
	client releaseClient,
	report selfUpdateReport,
	metadata installMetadata,
	metadataPath string,
	repo string,
	skipAttestation bool,
	stderr io.Writer,
	installLock *installLifecycleLock,
) (*selfUpdateReport, error) {
	client = normalizeReleaseClient(client)
	if report.LatestVersion == "" {
		return nil, refusedError(fmt.Errorf("refusing to update: latest version is empty"))
	}
	archive := detectSelfUpdateArchiveName(report.LatestVersion)
	if archive == "" {
		return nil, refusedError(fmt.Errorf("refusing to update: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH))
	}
	releaseBase := client.deriveReleaseBase(repo, report.LatestVersion)
	archiveURL := fmt.Sprintf("%s/%s", releaseBase, archive)
	checksumURL := fmt.Sprintf("%s/checksums.txt", releaseBase)
	signatureURL := fmt.Sprintf("%s/checksums.txt.sig", releaseBase)

	workspace, err := createPinnedSelfUpdateWorkspace(os.TempDir())
	if err != nil {
		return nil, refusedError(fmt.Errorf("could not create update workspace: %w", err))
	}
	defer func() { _ = workspace.Remove() }()

	archivePath := filepath.Join(workspace.path, archive)
	checksumsPath := filepath.Join(workspace.path, "checksums.txt")
	signaturePath := filepath.Join(workspace.path, "checksums.txt.sig")

	if err := client.download(ctx, archiveURL, archivePath); err != nil {
		return nil, refusedError(err)
	}
	if err := client.download(ctx, checksumURL, checksumsPath); err != nil {
		return nil, refusedError(err)
	}
	if err := client.download(ctx, signatureURL, signaturePath); err != nil {
		return nil, refusedError(err)
	}
	if err := client.verifySignature(checksumsPath, signaturePath); err != nil {
		return nil, refusedError(fmt.Errorf("checksum signature verification failed: %w", err))
	}
	archiveSHA, err := extractChecksumFromFile(checksumsPath, archive)
	if err != nil {
		return nil, refusedError(fmt.Errorf("checksum metadata invalid for %s: %w", archive, err))
	}
	if err := verifySHA256(archivePath, archiveSHA); err != nil {
		return nil, refusedError(fmt.Errorf("archive checksum verification failed for %s: %w", archive, err))
	}
	if err := verifyReleaseAttestations(ctx, client, repo, report.LatestVersion, archivePath, checksumsPath, skipAttestation, stderr); err != nil {
		return nil, refusedError(err)
	}
	return runSelfUpdateBundleInstall(report, metadata, metadataPath, archivePath, archive, archiveSHA, releaseBase, workspace.path, installLock)
}

func runSelfUpdateBundleInstall(
	report selfUpdateReport,
	metadata installMetadata,
	metadataPath string,
	archivePath string,
	archive string,
	archiveSHA string,
	releaseBase string,
	workspace string,
	installLock *installLifecycleLock,
) (*selfUpdateReport, error) {
	if err := installLock.Validate(); err != nil {
		return nil, refusedError(err)
	}
	extractRoot := filepath.Join(workspace, "bundle")
	extractedBundleRoot, err := extractThreadpointBundleFromArchive(archivePath, extractRoot)
	if err != nil {
		return nil, refusedError(err)
	}
	extractedBinary := filepath.Join(extractedBundleRoot, "bin", threadpointProductName)
	if err := ensureThreadpointBinaryMode(extractedBinary, 0o755); err != nil {
		return nil, refusedError(err)
	}
	downloadedHash, err := backup.FileSHA256(extractedBinary)
	if err != nil {
		return nil, refusedError(err)
	}
	if downloadedHash == metadata.BinarySHA256 {
		return nil, refusedError(fmt.Errorf("update produced an unchanged binary checksum; refusing to replace %s", metadata.BinaryPath))
	}

	bundleRoot := filepath.Clean(metadata.BundleRoot)
	newMetadata := metadata
	newMetadata.SchemaVersion = installerMetadataSchemaVersion
	newMetadata.Version = report.LatestVersion
	newMetadata.ReleaseBase = releaseBase
	newMetadata.Archive = archive
	newMetadata.ArchiveSHA256 = archiveSHA
	newMetadata.BinarySHA256 = downloadedHash
	newMetadata.InstallDir = report.InstallDir
	newMetadata.BinaryPath = report.BinaryPath
	newMetadata.LinkPath = report.LinkPath
	newMetadata.BundleRoot = bundleRoot
	newMetadata.BundleEntries = append([]string(nil), threadpointBundleEntries...)
	operation := updateOperation(report.Operation)
	if operation != updateOperationRollback {
		operation = updateOperationUpdate
	}
	productHome := filepath.Dir(filepath.Dir(metadataPath))
	transaction, err := prepareInstallTransaction(productHome, report.LinkPath, operation, metadataPath, metadata, installLock)
	if err != nil {
		return nil, refusedError(err)
	}
	rollback := func(cause error) (*selfUpdateReport, error) {
		recoveryErr := recoverInstallTransaction(productHome, report.LinkPath, installLock)
		if recoveryErr != nil {
			return nil, refusedError(errors.Join(cause, fmt.Errorf("durable install transaction recovery failed: %w", recoveryErr)))
		}
		return nil, refusedError(cause)
	}
	if err := installBundleEntriesChecked(extractedBundleRoot, bundleRoot, threadpointBundleEntries, installLock); err != nil {
		return rollback(fmt.Errorf("could not install update bundle: %w", err))
	}
	if err := installLock.Validate(); err != nil {
		return rollback(err)
	}
	if err := writeInstallMetadataChecked(metadataPath, newMetadata, installLock); err != nil {
		return rollback(fmt.Errorf("could not write installer metadata: %w", err))
	}
	if err := completeInstallTransaction(transaction, installLock); err != nil {
		return rollback(err)
	}
	report.Updated = true
	report.Archive = archive
	report.ArchiveSHA256 = archiveSHA
	report.Reason = "updated"
	return &report, nil
}

func printSelfUpdateReport(app *cli, format string, report selfUpdateReport) error {
	normalizeSelfUpdateOutcome(&report)
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		return printSelfUpdateText(app.stdout, report)
	case outputFormatJSON:
		return app.printJSON(report)
	default:
		return usageErrorf("unknown update format %q", format)
	}
}

func printSelfUpdateText(writer io.Writer, report selfUpdateReport) error {
	normalizeSelfUpdateOutcome(&report)
	fmt.Fprintf(writer, "command: %s\n", report.Command)
	fmt.Fprintf(writer, "  repo: %s\n", report.Repo)
	fmt.Fprintf(writer, "  install dir: %s\n", report.InstallDir)
	fmt.Fprintf(writer, "  binary: %s\n", report.BinaryPath)
	if report.LinkPath != "" {
		fmt.Fprintf(writer, "  link: %s\n", report.LinkPath)
	}
	fmt.Fprintf(writer, "  metadata: %s\n", report.MetadataPath)
	fmt.Fprintf(writer, "  current: %s\n", report.CurrentVersion)
	fmt.Fprintf(writer, "  latest: %s\n", report.LatestVersion)
	fmt.Fprintf(writer, "  source: %s\n", report.Source)
	fmt.Fprintf(writer, "  checked at: %s\n", report.CheckedAt)
	fmt.Fprintf(writer, "  update available: %v\n", report.UpdateAvailable)
	fmt.Fprintf(writer, "  updated: %v\n", report.Updated)
	if report.Archive != "" {
		fmt.Fprintf(writer, "  archive: %s\n", report.Archive)
		if report.ArchiveSHA256 != "" {
			fmt.Fprintf(writer, "  archive sha256: %s\n", report.ArchiveSHA256)
		}
	}
	if report.Code != "" {
		fmt.Fprintf(writer, "  code: %s\n", report.Code)
	}
	if report.Message != "" {
		fmt.Fprintf(writer, "  message: %s\n", report.Message)
	}
	return nil
}

func normalizeSelfUpdateOutcome(report *selfUpdateReport) {
	if report.Updated {
		report.Code = "update_applied"
		report.Message = "The update was applied."
		return
	}
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

func detectSelfUpdateArchiveName(version string) string {
	return selfUpdateArchiveNameForPlatform(version, runtime.GOOS, runtime.GOARCH)
}

func selfUpdateArchiveNameForPlatform(version string, osName string, arch string) string {
	archiveVersion := archiveVersionFromTag(version)
	if archiveVersion == "" {
		return ""
	}
	if arch != "amd64" && arch != "arm64" {
		return ""
	}
	switch osName {
	case "linux", "darwin":
	default:
		return ""
	}
	return fmt.Sprintf("threadpoint_%s_%s_%s.tar.gz", archiveVersion, osName, arch)
}

func archiveVersionFromTag(version string) string {
	version = strings.TrimSpace(version)
	return strings.TrimPrefix(version, "v")
}

func extractChecksumFromFile(path string, archive string) (string, error) {
	// #nosec G304 -- checksum file is downloaded into a controlled update workspace.
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[1] == archive {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("missing checksum for %s", archive)
}

func verifySHA256(path string, expected string) error {
	actual, err := backup.FileSHA256(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("checksum mismatch (expected %s, got %s)", expected, actual)
	}
	return nil
}

func verifyReleaseChecksumsSignature(checksumsPath string, signaturePath string) error {
	// #nosec G304 -- checksums path is a downloaded release artifact in the update workspace.
	checksums, err := os.ReadFile(checksumsPath)
	if err != nil {
		return err
	}
	// #nosec G304 -- signature path is a downloaded release artifact in the update workspace.
	body, err := os.ReadFile(signaturePath)
	if err != nil {
		return err
	}
	return releasesig.Verify(checksums, body)
}

func verifyReleaseAttestations(ctx context.Context, client releaseClient, repo string, tag string, archivePath string, checksumsPath string, skip bool, stderr io.Writer) error {
	if skip {
		if stderr != nil {
			fmt.Fprintln(stderr, "warning: skipping GitHub Artifact Attestation verification")
		}
		return nil
	}
	if err := client.verifyAttestation(ctx, repo, tag, archivePath); err != nil {
		return fmt.Errorf("artifact attestation verification failed for %s: %w", filepath.Base(archivePath), err)
	}
	if err := client.verifyAttestation(ctx, repo, tag, checksumsPath); err != nil {
		return fmt.Errorf("artifact attestation verification failed for checksums.txt: %w", err)
	}
	return nil
}

func verifyGitHubArtifactAttestation(ctx context.Context, repo string, tag string, artifactPath string) error {
	workflow, sourceRef, err := githubArtifactAttestationIdentity(repo, tag, artifactPath)
	if err != nil {
		return err
	}
	// The executable and subcommands are fixed; repo, tag, and artifact path are
	// release-verification inputs passed as argv, not through a shell.
	//nolint:gosec
	cmd := exec.CommandContext(ctx, "gh", "attestation", "verify", artifactPath,
		"-R", repo,
		"--signer-repo", repo,
		"--signer-workflow", workflow,
		"--source-ref", sourceRef,
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func githubArtifactAttestationIdentity(repo string, tag string, artifactPath string) (workflow string, sourceRef string, err error) {
	switch base := filepath.Base(artifactPath); {
	case base == "checksums.txt":
		return fmt.Sprintf("%s/.github/workflows/release-publish.yml", repo), "refs/heads/main", nil
	case strings.HasPrefix(base, "threadpoint_") && strings.HasSuffix(base, ".tar.gz"):
		return fmt.Sprintf("%s/.github/workflows/release.yml", repo), "refs/tags/" + tag, nil
	default:
		return "", "", fmt.Errorf("unsupported release attestation subject %q", base)
	}
}

func envFlagEnabled(name string) bool {
	return strings.TrimSpace(os.Getenv(name)) == "1"
}

func extractThreadpointBinaryFromArchive(path string, destination string) error {
	// #nosec G304 -- archive path is the downloaded update artifact in a controlled workspace.
	archive, err := os.Open(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)

	expectedEntry := threadpointArchiveBinaryEntry(path)
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name, err := archiveEntryName(header.Name)
		if err != nil {
			return err
		}
		switch {
		case name == "":
			continue
		case name != expectedEntry:
			continue
		case found:
			return fmt.Errorf("archive contains multiple threadpoint entries")
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("unexpected archive entry type for %s", header.Name)
		}
		if header.Size < 0 || header.Size > maxSelfUpdateExtractedBinaryBytes {
			return fmt.Errorf("threadpoint binary in archive exceeds maximum size")
		}
		// #nosec G304 -- extraction target is a controlled temp path and archive entry name is constrained above.
		target, err := os.Create(destination)
		if err != nil {
			return err
		}
		if _, err := copySelfUpdateBinary(target, reader, maxSelfUpdateExtractedBinaryBytes); err != nil {
			_ = target.Close()
			return err
		}
		if err := target.Close(); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return fmt.Errorf("archive does not contain threadpoint binary")
	}
	return nil
}

func threadpointArchiveBinaryEntry(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".tar.gz") + "/bin/threadpoint"
}

func extractThreadpointBundleFromArchive(path string, destination string) (string, error) {
	// #nosec G304 -- archive path is the downloaded update artifact in a controlled workspace.
	archive, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return "", err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)

	bundleDir := strings.TrimSuffix(filepath.Base(path), ".tar.gz")
	expected := map[string]string{}
	seen := map[string]bool{}
	for _, entry := range threadpointBundleEntries {
		expected[bundleDir+"/"+entry] = entry
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		name, err := archiveEntryName(header.Name)
		if err != nil {
			return "", err
		}
		if name == "" {
			continue
		}
		if header.Typeflag == tar.TypeDir {
			if threadpointArchiveDirectoryAllowed(bundleDir, strings.TrimSuffix(name, "/")) {
				continue
			}
			return "", fmt.Errorf("unexpected archive entry: %s", header.Name)
		}
		rel, ok := expected[name]
		if !ok {
			return "", fmt.Errorf("unexpected archive entry: %s", header.Name)
		}
		if seen[name] {
			return "", fmt.Errorf("archive contains duplicate entry: %s", header.Name)
		}
		if header.Typeflag != tar.TypeReg {
			return "", fmt.Errorf("unexpected archive entry type for %s", header.Name)
		}
		if header.Size < 0 || header.Size > maxSelfUpdateExtractedBinaryBytes {
			return "", fmt.Errorf("archive entry %s exceeds maximum size", header.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(rel))
		// #nosec G301 -- extraction workspace is temporary and receives verified release files.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		// #nosec G304 -- extraction target is under a controlled temp directory and rel is from a fixed allow-list.
		output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return "", err
		}
		written, err := io.Copy(output, io.LimitReader(reader, maxSelfUpdateExtractedBinaryBytes+1))
		if err != nil {
			return "", errors.Join(err, output.Close())
		}
		if written > maxSelfUpdateExtractedBinaryBytes {
			return "", errors.Join(fmt.Errorf("archive entry %s exceeds maximum size", header.Name), output.Close())
		}
		if err := output.Close(); err != nil {
			return "", err
		}
		seen[name] = true
	}
	for entry := range expected {
		if !seen[entry] {
			return "", fmt.Errorf("archive does not contain %s", entry)
		}
	}
	return destination, nil
}

func threadpointArchiveDirectoryAllowed(bundleDir string, entry string) bool {
	switch entry {
	case bundleDir,
		bundleDir + "/bin",
		bundleDir + "/scripts":
		return true
	default:
		return false
	}
}

func copySelfUpdateBinary(target io.Writer, source io.Reader, maxBytes int64) (int64, error) {
	written, err := io.Copy(target, io.LimitReader(source, maxBytes+1))
	if err != nil {
		return written, err
	}
	if written > maxBytes {
		return written, fmt.Errorf("threadpoint binary in archive exceeds maximum size")
	}
	return written, nil
}

// archiveEntryName normalizes a tar entry name and rejects unsafe paths
// (absolute, parent traversal, or backslashes). It returns the cleaned name
// (empty for entries the caller should skip) or an error for unsafe entries.
func archiveEntryName(raw string) (string, error) {
	name := strings.TrimPrefix(strings.TrimSpace(raw), "./")
	if strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.Contains(name, "\\") {
		return "", fmt.Errorf("unsafe archive entry: %s", raw)
	}
	return name, nil
}

func ensureThreadpointBinaryMode(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}

func threadpointBundleEntryMode(entry string) os.FileMode {
	switch entry {
	case "bin/threadpoint", "scripts/install.sh", "scripts/uninstall.sh":
		return 0o755
	default:
		return 0o644
	}
}

// defaultMaxDownloadBytes caps release archive and checksum downloads so a
// misbehaving or compromised endpoint cannot fill the disk before checksum
// verification runs. Callers pass it explicitly so tests can supply a small
// limit without mutating shared state.
const (
	defaultMaxDownloadBytes           int64 = 64 << 20
	maxSelfUpdateExtractedBinaryBytes int64 = 64 << 20
)

func downloadToPathFromNetwork(ctx context.Context, source string, destination string, maxBytes int64) error {
	ctx, cancel := context.WithTimeout(ctx, defaultUpdateNetworkTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %s for %s", response.Status, source)
	}
	// #nosec G304 -- download destination is a controlled update workspace path.
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer output.Close()
	written, err := io.Copy(output, io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if written > maxBytes {
		return fmt.Errorf("download for %s exceeded the %d byte limit", source, maxBytes)
	}
	return output.Sync()
}
