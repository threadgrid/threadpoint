// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/threadgrid/threadpoint/backup"
)

// installMetadata is the cross-language ownership record shared by the shell
// installer and Go update and uninstall commands.
type installMetadata struct {
	SchemaVersion int      `json:"schema_version"`
	Channel       string   `json:"channel"`
	Repo          string   `json:"repo"`
	InstallDir    string   `json:"install_dir"`
	BinaryPath    string   `json:"binary_path"`
	LinkPath      string   `json:"link_path,omitempty"`
	BundleRoot    string   `json:"bundle_root,omitempty"`
	BundleEntries []string `json:"bundle_entries,omitempty"`
	Version       string   `json:"version"`
	ReleaseBase   string   `json:"release_base"`
	Archive       string   `json:"archive"`
	ArchiveSHA256 string   `json:"archive_sha256"`
	BinarySHA256  string   `json:"binary_sha256"`
}

const (
	installerMetadataSchemaVersion = 1
	installerChannelScript         = "installer-script"
)

var threadpointBundleEntries = []string{
	"bin/threadpoint",
	"README.md",
	"LICENSE",
	"NOTICE",
	"scripts/install.sh",
	"scripts/uninstall.sh",
}

type uninstallReport struct {
	Mode         string   `json:"mode"`
	InstallDir   string   `json:"install_dir"`
	BinaryPath   string   `json:"binary_path"`
	LinkPath     string   `json:"link_path,omitempty"`
	BundleRoot   string   `json:"bundle_root,omitempty"`
	MetadataPath string   `json:"metadata_path"`
	WouldRemove  []string `json:"would_remove,omitempty"`
	Removed      []string `json:"removed,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

var threadpointUninstallNotes = []string{
	"Uninstall removes only installer-managed threadpoint release files, the command symlink, and installer metadata.",
	"Local threadpoint backups, update cache and reminders, non-installer product-home files, project files, provider artifacts, generated records, and selected-project guidance are preserved.",
	"For full local cleanup after uninstall, manually remove THREADPOINT_HOME or $HOME/.threadpoint; that deletes preserved threadpoint state.",
}

type managedInstallPaths struct {
	InstallDir  string
	CommandPath string
	BinaryPath  string
}

func runUninstall(ctx context.Context, app *cli, args []string) error {
	productHome, err := app.productHome()
	if err != nil {
		return err
	}
	defaultDir := defaultUninstallDir(productHome)
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	installDir := fs.String("dir", defaultDir, "command directory")
	planOnly := fs.Bool("plan", true, "print the uninstall plan")
	apply := fs.Bool("apply", false, "remove the installer-managed bundle and metadata")
	yes := fs.Bool("yes", false, "confirm uninstall apply")
	format := fs.String("format", app.format, "text or json")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("uninstall does not accept positional arguments")
	}
	if !*planOnly && !*apply {
		return usageErrorf("uninstall requires --plan or --apply")
	}
	if flagProvided(fs, "plan") && *planOnly && *apply {
		return usageErrorf("uninstall accepts only one of --plan or --apply")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "uninstall")
	if err != nil {
		return usageError(err)
	}
	if *apply && !*yes {
		return refusedError(fmt.Errorf("refusing to uninstall without --yes"))
	}
	var installLock *installLifecycleLock
	var commandPath string
	if *apply {
		paths, err := resolveManagedInstallPaths(*installDir)
		if err != nil {
			return refusedError(err)
		}
		commandPath = paths.CommandPath
		installLock, err = acquireInstallLifecycleLock(ctx, productHome, commandPath)
		if err != nil {
			return refusedError(fmt.Errorf("could not acquire install lifecycle lock: %w", err))
		}
		defer func() { _ = installLock.Release() }()
		if err := recoverInstallTransaction(productHome, commandPath, installLock); err != nil {
			return refusedError(fmt.Errorf("could not recover interrupted install transaction: %w", err))
		}
	}
	report, err := buildUninstallReportWithLock(productHome, *installDir, *apply, installLock)
	if err != nil {
		return refusedError(err)
	}
	if *apply {
		metadata, err := readInstallMetadata(report.MetadataPath)
		if err != nil {
			return refusedError(err)
		}
		transaction, err := prepareInstallTransaction(productHome, commandPath, updateOperationUninstall, report.MetadataPath, metadata, installLock)
		if err != nil {
			return refusedError(fmt.Errorf("could not prepare durable uninstall transaction: %w", err))
		}
		rollback := func(cause error) error {
			recoveryErr := recoverInstallTransaction(productHome, commandPath, installLock)
			if recoveryErr != nil {
				return errors.Join(cause, fmt.Errorf("durable uninstall recovery failed: %w", recoveryErr))
			}
			return cause
		}
		removed := []string{}
		// Keep both recovery paths until all other removal work succeeds. The
		// bundled uninstaller must survive a metadata-removal failure, and its
		// command symlink must survive until that recovery script is gone.
		uninstallerPath := filepath.Join(report.BundleRoot, "scripts", "uninstall.sh")
		for _, path := range report.WouldRemove {
			if path == "" || path == report.LinkPath || path == report.MetadataPath || path == uninstallerPath {
				continue
			}
			rel, err := filepath.Rel(report.BundleRoot, path)
			if err != nil || !safeBundleEntry(filepath.ToSlash(rel)) {
				return rollback(fmt.Errorf("managed uninstall path is outside the bundle: %s", path))
			}
			removedPath, err := removeBundleEntryChecked(filepath.ToSlash(rel), installLock)
			if err != nil {
				return rollback(err)
			}
			if removedPath {
				removed = append(removed, path)
			}
		}
		if removedPath, err := removeManagedPathChecked(report.MetadataPath, installLock); err != nil {
			return rollback(err)
		} else if removedPath {
			removed = append(removed, report.MetadataPath)
		}
		if removedPath, err := removeBundleEntryChecked("scripts/uninstall.sh", installLock); err != nil {
			return rollback(err)
		} else if removedPath {
			removed = append(removed, uninstallerPath)
		}
		if removedLink, err := removeInstallerLinkChecked(report.LinkPath, report.BinaryPath, installLock); err != nil {
			return rollback(err)
		} else if removedLink {
			removed = append(removed, report.LinkPath)
		}
		if err := completeInstallTransaction(transaction, installLock); err != nil {
			return rollback(err)
		}
		report.Removed = removed
		report.WouldRemove = nil
	}
	if outputFormat == outputFormatText {
		printUninstallText(app.stdout, report)
		return nil
	}
	return app.printJSON(report)
}

func printUninstallText(writer io.Writer, report uninstallReport) {
	fmt.Fprintf(writer, "command: threadpoint uninstall\n")
	fmt.Fprintf(writer, "mode: %s\n", report.Mode)
	fmt.Fprintf(writer, "install dir: %s\n", report.InstallDir)
	fmt.Fprintf(writer, "binary path: %s\n", report.BinaryPath)
	if report.LinkPath != "" {
		fmt.Fprintf(writer, "link path: %s\n", report.LinkPath)
	}
	if len(report.WouldRemove) > 0 {
		fmt.Fprintf(writer, "would remove: %d\n", len(report.WouldRemove))
		for _, path := range report.WouldRemove {
			fmt.Fprintf(writer, "  - %s\n", path)
		}
	}
	if len(report.Removed) > 0 {
		fmt.Fprintf(writer, "removed: %d\n", len(report.Removed))
		for _, path := range report.Removed {
			fmt.Fprintf(writer, "  - %s\n", path)
		}
	}
	if len(report.Notes) > 0 {
		fmt.Fprintf(writer, "notes:\n")
		for _, note := range report.Notes {
			fmt.Fprintf(writer, "  - %s\n", note)
		}
	}
}

func defaultUninstallDir(productHome string) string {
	if installDir := strings.TrimSpace(os.Getenv("THREADPOINT_INSTALL_DIR")); installDir != "" {
		return installDir
	}
	executable := currentExecutablePath()
	if linkDir := metadataLinkDirForBinary(productHome, executable); linkDir != "" {
		return linkDir
	}
	if filepath.Base(executable) == threadpointProductName {
		return filepath.Dir(executable)
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "bin")
	}
	return ""
}

func buildUninstallReportWithLock(productHome string, installDir string, apply bool, lock *installLifecycleLock) (uninstallReport, error) {
	if strings.TrimSpace(installDir) == "" {
		return uninstallReport{}, fmt.Errorf("refusing to uninstall: command directory is empty; pass --dir or set THREADPOINT_INSTALL_DIR")
	}
	paths, err := resolveManagedInstallPaths(installDir)
	if err != nil {
		return uninstallReport{}, err
	}
	metadataPath := installerMetadataPath(productHome, paths.BinaryPath)
	metadata, err := readInstallMetadata(metadataPath)
	if err != nil {
		return uninstallReport{}, err
	}
	if err := verifyInstallOwnership(metadata, paths.BinaryPath); err != nil {
		return uninstallReport{}, err
	}
	linkPath, linkExists, err := verifiedInstallerLinkPath(metadata, paths.BinaryPath)
	if err != nil {
		return uninstallReport{}, err
	}
	missingBinary := false
	currentHash, err := installerManagedBinarySHA256(metadata, lock)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return uninstallReport{}, fmt.Errorf("refusing to uninstall: %w", err)
		}
		missingBinary = true
	} else if currentHash != metadata.BinarySHA256 {
		return uninstallReport{}, fmt.Errorf("refusing to uninstall: current binary checksum does not match installer metadata for %s", paths.BinaryPath)
	}
	mode := "plan"
	if apply {
		mode = "apply"
	}
	wouldRemove := []string{}
	if linkExists {
		wouldRemove = append(wouldRemove, linkPath)
	}
	bundleRoot := filepath.Clean(metadata.BundleRoot)
	for _, entry := range threadpointBundleEntries {
		wouldRemove = append(wouldRemove, filepath.Join(bundleRoot, filepath.FromSlash(entry)))
	}
	wouldRemove = append(wouldRemove, metadataPath)
	notes := append([]string(nil), threadpointUninstallNotes...)
	if missingBinary {
		notes = append(notes, "The installer-managed binary is already missing; removing the remaining recorded install files.")
	}
	return uninstallReport{
		Mode:         mode,
		InstallDir:   paths.InstallDir,
		BinaryPath:   paths.BinaryPath,
		LinkPath:     linkPath,
		BundleRoot:   bundleRoot,
		MetadataPath: metadataPath,
		WouldRemove:  wouldRemove,
		Notes:        notes,
	}, nil
}

func installerMetadataPath(productHome string, binaryPath string) string {
	sum := sha256.Sum256([]byte(binaryPath))
	return filepath.Join(productHome, "installs", hex.EncodeToString(sum[:])+".json")
}

func readInstallMetadata(path string) (installMetadata, error) {
	parentPath := filepath.Dir(filepath.Clean(path))
	parent, _, err := pinInstallDirectory(parentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return installMetadata{}, fmt.Errorf("refusing to uninstall: installer metadata not found at %s; uninstall through the original install channel", path)
		}
		return installMetadata{}, err
	}
	defer parent.Close()
	body, err := backup.ReadRootRegularFileBounded(parent, filepath.Base(path), maxInstallTransactionBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return installMetadata{}, fmt.Errorf("refusing to uninstall: installer metadata not found at %s; uninstall through the original install channel", path)
		}
		return installMetadata{}, fmt.Errorf("refusing to uninstall: could not safely read installer metadata at %s: %w", path, err)
	}
	metadata, err := decodeInstallMetadataStrict(body)
	if err != nil {
		return installMetadata{}, fmt.Errorf("refusing to uninstall: malformed installer metadata at %s", path)
	}
	return metadata, nil
}

func verifyInstallOwnership(metadata installMetadata, binaryPath string) error {
	switch {
	case metadata.SchemaVersion != installerMetadataSchemaVersion:
		return fmt.Errorf("refusing to uninstall: unsupported installer metadata schema %d", metadata.SchemaVersion)
	case metadata.Channel != installerChannelScript:
		return fmt.Errorf("refusing to uninstall: unsupported install channel %q", metadata.Channel)
	case metadata.Repo != "threadgrid/threadpoint":
		return fmt.Errorf("refusing to uninstall: installer metadata repo mismatch %q", metadata.Repo)
	case metadata.BinaryPath != binaryPath:
		return fmt.Errorf("refusing to uninstall: installer metadata points at %s, not %s", metadata.BinaryPath, binaryPath)
	case strings.TrimSpace(metadata.LinkPath) == "":
		return fmt.Errorf("refusing to uninstall: installer metadata is missing link path")
	case metadata.BinarySHA256 == "":
		return fmt.Errorf("refusing to uninstall: installer metadata is missing binary checksum")
	}
	if err := verifyBundleOwnership(metadata, binaryPath); err != nil {
		return err
	}
	return nil
}

func verifyBundleOwnership(metadata installMetadata, binaryPath string) error {
	bundleRoot := filepath.Clean(strings.TrimSpace(metadata.BundleRoot))
	if bundleRoot == "" || !filepath.IsAbs(bundleRoot) {
		return fmt.Errorf("refusing to manage install: installer metadata bundle_root must be absolute: %s", metadata.BundleRoot)
	}
	if filepath.Clean(filepath.Join(bundleRoot, "bin", threadpointProductName)) != filepath.Clean(binaryPath) {
		return fmt.Errorf("refusing to manage install: installer metadata bundle root %s does not own %s", bundleRoot, binaryPath)
	}
	if !sameStringSet(metadata.BundleEntries, threadpointBundleEntries) {
		return fmt.Errorf("refusing to manage install: installer metadata bundle_entries do not match threadpoint release bundle")
	}
	for _, entry := range metadata.BundleEntries {
		if !safeBundleEntry(entry) {
			return fmt.Errorf("refusing to manage install: unsafe bundle entry %q", entry)
		}
	}
	return nil
}

func safeBundleEntry(entry string) bool {
	if entry == "" || strings.HasPrefix(entry, "/") || strings.Contains(entry, "\\") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(entry))
	return clean == entry && clean != "." && !strings.HasPrefix(clean, "../") && !strings.Contains(clean, "/../")
}

func sameStringSet(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := map[string]int{}
	for _, value := range left {
		seen[value]++
	}
	for _, value := range right {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}

func resolveManagedInstallPaths(installDir string) (managedInstallPaths, error) {
	cleanInstallDir, err := filepath.Abs(installDir)
	if err != nil {
		return managedInstallPaths{}, err
	}
	if evaluatedInstallDir, err := filepath.EvalSymlinks(cleanInstallDir); err == nil {
		cleanInstallDir = evaluatedInstallDir
	}
	if info, err := os.Stat(cleanInstallDir); err != nil {
		return managedInstallPaths{}, fmt.Errorf("command directory does not exist: %s", cleanInstallDir)
	} else if !info.IsDir() {
		return managedInstallPaths{}, fmt.Errorf("command path is not a directory: %s", cleanInstallDir)
	}
	commandPath := filepath.Join(cleanInstallDir, threadpointProductName)
	binaryPath := commandPath
	if info, err := os.Lstat(commandPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		evaluatedBinaryPath, err := filepath.EvalSymlinks(commandPath)
		switch {
		case err == nil:
			binaryPath = evaluatedBinaryPath
		case errors.Is(err, os.ErrNotExist):
			target, readErr := os.Readlink(commandPath)
			if readErr != nil {
				return managedInstallPaths{}, fmt.Errorf("could not read command symlink %s: %w", commandPath, readErr)
			}
			if filepath.IsAbs(target) {
				binaryPath = target
			} else {
				binaryPath = filepath.Join(filepath.Dir(commandPath), target)
			}
		default:
			return managedInstallPaths{}, fmt.Errorf("command symlink cannot be resolved: %s", commandPath)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return managedInstallPaths{}, fmt.Errorf("could not inspect command path %s: %w", commandPath, err)
	}
	return managedInstallPaths{
		InstallDir:  cleanInstallDir,
		CommandPath: commandPath,
		BinaryPath:  filepath.Clean(binaryPath),
	}, nil
}

func metadataLinkDirForBinary(productHome string, binaryPath string) string {
	if strings.TrimSpace(productHome) == "" || binaryPath == "" {
		return ""
	}
	metadata, err := readInstallMetadata(installerMetadataPath(productHome, binaryPath))
	if err != nil {
		return ""
	}
	linkPath, exists, err := verifiedInstallerLinkPath(metadata, binaryPath)
	if err != nil || !exists {
		return ""
	}
	return filepath.Dir(linkPath)
}

func verifiedInstallerLinkPath(metadata installMetadata, binaryPath string) (string, bool, error) {
	linkPath := strings.TrimSpace(metadata.LinkPath)
	if linkPath == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(linkPath) {
		return "", false, fmt.Errorf("refusing to manage install: installer metadata link path must be absolute: %s", linkPath)
	}
	linkPath = filepath.Clean(linkPath)
	info, err := os.Lstat(linkPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return linkPath, false, nil
		}
		return "", false, fmt.Errorf("refusing to manage install: could not inspect installer link path %s: %w", linkPath, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", false, fmt.Errorf("refusing to manage install: installer link path is not a symlink: %s", linkPath)
	}
	target, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("refusing to manage install: installer link path cannot be resolved: %s", linkPath)
		}
		rawTarget, readErr := os.Readlink(linkPath)
		if readErr != nil {
			return "", false, fmt.Errorf("refusing to manage install: could not read installer link path %s: %w", linkPath, readErr)
		}
		if filepath.IsAbs(rawTarget) {
			target = rawTarget
		} else {
			target = filepath.Join(filepath.Dir(linkPath), rawTarget)
		}
	}
	if filepath.Clean(target) != filepath.Clean(binaryPath) {
		return "", false, fmt.Errorf("refusing to manage install: installer link path %s points at %s, not %s", linkPath, target, binaryPath)
	}
	return linkPath, true, nil
}
