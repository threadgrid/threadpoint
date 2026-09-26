// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/threadgrid/threadpoint/internal/abspath"
)

const unknownBuildValue = "unknown"

var (
	version = "devel"
	commit  = unknownBuildValue
	date    = unknownBuildValue
)

type versionReport struct {
	Command       string `json:"command"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	BuildDate     string `json:"build_date"`
	GoVersion     string `json:"go_version"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	InstallSource string `json:"install_source"`
	InstallPath   string `json:"install_path,omitempty"`
	MetadataPath  string `json:"metadata_path,omitempty"`
}

func runVersion(_ context.Context, app *cli, args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	format := fs.String("format", app.format, "text or json")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("version does not accept positional arguments")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "version")
	if err != nil {
		return usageError(err)
	}
	productHome, err := app.productHome()
	if err != nil {
		return err
	}

	report := buildVersionReport(productHome)
	switch outputFormat {
	case outputFormatText:
		printVersionText(app.stdout, report)
	case outputFormatJSON:
		if err := app.printJSON(report); err != nil {
			return err
		}
	}
	return nil
}

func buildVersionReport(productHome string) versionReport {
	installPath := currentExecutablePath()
	installSource, metadataPath := detectInstallSource(installPath, productHome)
	return versionReport{
		Command:       "threadpoint version",
		Version:       buildVersion(),
		Commit:        buildCommit(),
		BuildDate:     buildDate(),
		GoVersion:     runtime.Version(),
		GOOS:          runtime.GOOS,
		GOARCH:        runtime.GOARCH,
		InstallSource: installSource,
		InstallPath:   installPath,
		MetadataPath:  metadataPath,
	}
}

func printVersionText(writer interface {
	Write([]byte) (int, error)
}, report versionReport) {
	fmt.Fprintf(writer, "command: %s\n", report.Command)
	fmt.Fprintf(writer, "version: %s\n", report.Version)
	fmt.Fprintf(writer, "commit: %s\n", report.Commit)
	fmt.Fprintf(writer, "build date: %s\n", report.BuildDate)
	fmt.Fprintf(writer, "go version: %s\n", report.GoVersion)
	fmt.Fprintf(writer, "platform: %s/%s\n", report.GOOS, report.GOARCH)
	fmt.Fprintf(writer, "install source: %s\n", report.InstallSource)
	if report.InstallPath != "" {
		fmt.Fprintf(writer, "install path: %s\n", report.InstallPath)
	}
	if report.MetadataPath != "" {
		fmt.Fprintf(writer, "metadata: %s\n", report.MetadataPath)
	}
}

func buildVersion() string {
	if value := cleanBuildValue(version); value != "" && value != "devel" {
		return value
	}
	if value := buildInfoMainVersion(); value != "" && value != "devel" {
		return value
	}
	return "devel"
}

func buildCommit() string {
	if value := cleanBuildValue(commit); value != "" && value != unknownBuildValue {
		return value
	}
	if value := buildInfoSetting("vcs.revision"); value != "" {
		return value
	}
	return unknownBuildValue
}

func buildDate() string {
	if value := cleanBuildValue(date); value != "" && value != unknownBuildValue {
		return value
	}
	if value := buildInfoSetting("vcs.time"); value != "" {
		return value
	}
	return unknownBuildValue
}

func buildInfoMainVersion() string {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	value := cleanBuildValue(buildInfo.Main.Version)
	if value == "(devel)" {
		return "devel"
	}
	return value
}

func buildInfoSetting(key string) string {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range buildInfo.Settings {
		if setting.Key == key {
			return cleanBuildValue(setting.Value)
		}
	}
	return ""
}

func cleanBuildValue(value string) string {
	return strings.TrimSpace(value)
}

func currentExecutablePath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	path, err = abspath.Abs(path)
	if err != nil {
		return ""
	}
	if evaluated, err := filepath.EvalSymlinks(path); err == nil {
		return evaluated
	}
	return path
}

func detectInstallSource(binaryPath string, productHome string) (string, string) {
	if source, metadataPath := installerMetadataSource(binaryPath, productHome); source != "" {
		return source, metadataPath
	}
	if buildVersion() == "devel" {
		return "source-build", ""
	}
	if value := buildInfoMainVersion(); value != "" && value != "devel" {
		return "go-install", ""
	}
	return "release-build", ""
}

func installerMetadataSource(binaryPath string, productHome string) (string, string) {
	if binaryPath == "" || strings.TrimSpace(productHome) == "" {
		return "", ""
	}
	metadataPath := installerMetadataPath(productHome, binaryPath)
	metadata, err := readInstallMetadata(metadataPath)
	if err != nil {
		return "", ""
	}
	if err := verifyInstallOwnership(metadata, binaryPath); err != nil {
		return "", ""
	}
	return metadata.Channel, metadataPath
}
