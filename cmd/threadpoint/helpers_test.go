// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/diagnostic"
)

type diagnosticCoverageError struct {
	outcome diagnostic.Outcome
}

func (err diagnosticCoverageError) Error() string { return "fallback diagnostic message" }

func (err diagnosticCoverageError) DiagnosticOutcome() diagnostic.Outcome { return err.outcome }

func (fixture installTransactionFixture) acquire(t *testing.T) *installLifecycleLock {
	t.Helper()
	lock, err := acquireInstallLifecycleLock(context.Background(), fixture.productHome, fixture.commandPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release install lifecycle lock: %v", err)
		}
	})
	return lock
}

func prepareFixtureTransaction(t *testing.T, fixture installTransactionFixture, lock *installLifecycleLock) *preparedInstallTransaction {
	t.Helper()
	transaction, err := prepareInstallTransaction(
		fixture.productHome, fixture.commandPath, updateOperationUpdate,
		fixture.metadataPath, fixture.metadata, lock,
	)
	if err != nil {
		t.Fatal(err)
	}
	return transaction
}

func runTestCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	err := app.run(context.Background(), args)
	return stdout.String(), stderr.String(), err
}

// runTestCLIWithRelease runs the CLI with an injected releaseClient so the
// update tests can supply fake network/version/rename seams without
// mutating package-level state (which lets them run in parallel).
func runTestCLIWithRelease(t *testing.T, release releaseClient, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := newCLI(&stdout, &stderr, strings.NewReader(""))
	if release.fetchLatest != nil {
		release.fetchReleases = func(ctx context.Context, repo string) ([]releaseMetadata, error) {
			latest, err := release.fetchLatest(ctx, repo)
			if err != nil {
				return nil, err
			}
			return []releaseMetadata{latest}, nil
		}
	}
	release.verifySignature = func(string, string) error { return nil }
	release.verifyAttestation = func(context.Context, string, string, string) error { return nil }
	app.release = release
	err := app.run(context.Background(), args)
	return stdout.String(), err
}

func mustWrite(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func initTestGitRepo(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cmd := exec.CommandContext(context.Background(), "git", "-C", root, "init")
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git init failed: %v\n%s", err, body)
	}
}

func writeThreadpointReleaseAssets(
	t *testing.T,
	dir string,
	payload string,
) (string, string, error) {
	t.Helper()
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	t.Log("building release assets", goos, goarch)
	archive := fmt.Sprintf("threadpoint_%s_%s_%s.tar.gz", archiveVersionFromTag("v0.1.0"), goos, goarch)
	if goos == "" || goarch == "" {
		return "", "", fmt.Errorf("os and arch are required")
	}
	archivePath := filepath.Join(dir, archive)
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return "", "", err
	}
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		return "", "", err
	}
	gzipWriter := gzip.NewWriter(archiveFile)
	tarWriter := tar.NewWriter(gzipWriter)
	bundleDir := strings.TrimSuffix(archive, ".tar.gz")
	for _, entry := range []struct {
		name string
		mode int64
		body string
	}{
		{name: bundleDir + "/README.md", mode: 0o644, body: "# threadpoint\n"},
		{name: bundleDir + "/LICENSE", mode: 0o644, body: "license\n"},
		{name: bundleDir + "/NOTICE", mode: 0o644, body: "notice\n"},
		{name: bundleDir + "/scripts/install.sh", mode: 0o755, body: "#!/bin/sh\n"},
		{name: bundleDir + "/scripts/uninstall.sh", mode: 0o755, body: "#!/bin/sh\n"},
		{name: bundleDir + "/bin/threadpoint", mode: 0o755, body: payload},
	} {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: entry.name,
			Mode: entry.mode,
			Size: int64(len(entry.body)),
		}); err != nil {
			return "", "", err
		}
		if _, err := io.WriteString(tarWriter, entry.body); err != nil {
			return "", "", err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return "", "", err
	}
	if err := gzipWriter.Close(); err != nil {
		return "", "", err
	}
	if err := archiveFile.Close(); err != nil {
		return "", "", err
	}

	sum, err := backup.FileSHA256(archivePath)
	if err != nil {
		return "", "", err
	}
	checksumPath := filepath.Join(dir, "checksums.txt")
	checksumData := fmt.Sprintf("%s %s\n", sum, archive)
	if err := os.WriteFile(checksumPath, []byte(checksumData), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(checksumPath+".sig", []byte("test signature\n"), 0o644); err != nil {
		return "", "", err
	}
	return archivePath, checksumPath, nil
}

func copyFileContents(source string, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer output.Close()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	return output.Sync()
}

func envFlagEnabledForTest(t *testing.T, name string) bool {
	t.Helper()
	t.Setenv(name, " 1 ")
	return envFlagEnabled(name)
}

func gitCommand(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root, "-c", "commit.gpgSign=false"}, args...)...) //nolint:noctx // test git helper; no context cancellation needed.
	body, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, body)
	}
	return string(body)
}

type updateNetworkRoundTripper func(*http.Request) (*http.Response, error)

func (fn updateNetworkRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
