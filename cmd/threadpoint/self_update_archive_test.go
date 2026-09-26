// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadToPathRejectsOversizedBody(t *testing.T) {
	t.Parallel()
	const limit int64 = 1024

	body := strings.Repeat("x", int(limit)*2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "archive.tar.gz")
	err := downloadToPathFromNetwork(context.Background(), srv.URL, dest, limit)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestDownloadToPathAcceptsBodyAtLimit(t *testing.T) {
	t.Parallel()
	const limit int64 = 1024

	body := strings.Repeat("y", int(limit))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := downloadToPathFromNetwork(context.Background(), srv.URL, dest, limit); err != nil {
		t.Fatalf("download at the limit should succeed, got %v", err)
	}
}

func TestSelfUpdateArchiveNamesRejectUnsupportedPlatforms(t *testing.T) {
	if selfUpdateArchiveNameForPlatform("", "linux", "amd64") != "" {
		t.Fatal("empty version should not yield an archive")
	}
	if selfUpdateArchiveNameForPlatform("v1.2.3", "linux", "386") != "" || selfUpdateArchiveNameForPlatform("v1.2.3", "windows", "amd64") != "" {
		t.Fatal("unsupported platform should not yield an archive")
	}
}

func TestSelfUpdateArchiveEntriesStayInsideExpectedBundle(t *testing.T) {
	if !threadpointArchiveDirectoryAllowed("bundle", "bundle/scripts") || threadpointArchiveDirectoryAllowed("bundle", "bundle/other") {
		t.Fatal("archive directory validation is incorrect")
	}
	if _, err := archiveEntryName("../unsafe"); err == nil {
		t.Fatal("unsafe archive entry should fail")
	}
	if name, err := archiveEntryName(" ./bundle/bin/threadpoint "); err != nil || name != "bundle/bin/threadpoint" {
		t.Fatalf("archive entry normalization = %q, %v", name, err)
	}
}

func TestSelfUpdateBundleArchiveExtractionPreservesEveryExpectedEntry(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "threadpoint.tar.gz")
	writeTestThreadpointReleaseArchive(t, archive, []byte("binary"))
	destination := filepath.Join(dir, "extracted")
	got, err := extractThreadpointBundleFromArchive(archive, destination)
	if err != nil || got != destination {
		t.Fatalf("bundle extraction = %q, %v", got, err)
	}
	for _, entry := range threadpointBundleEntries {
		if _, err := os.Stat(filepath.Join(destination, filepath.FromSlash(entry))); err != nil {
			t.Fatalf("extracted entry %s: %v", entry, err)
		}
	}
}

func TestSelfUpdateArchiveReadersRejectMalformedAndIncompleteEntries(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries []tar.Header
		bundle  bool
		want    string
	}{
		{name: "binary missing", want: "does not contain threadpoint binary"},
		{name: "binary duplicate", entries: []tar.Header{{Name: "threadpoint/bin/threadpoint", Typeflag: tar.TypeReg}, {Name: "threadpoint/bin/threadpoint", Typeflag: tar.TypeReg}}, want: "multiple threadpoint entries"},
		{name: "binary symlink", entries: []tar.Header{{Name: "threadpoint/bin/threadpoint", Typeflag: tar.TypeSymlink}}, want: "unexpected archive entry type"},
		{name: "binary unsafe path", entries: []tar.Header{{Name: "../threadpoint/bin/threadpoint", Typeflag: tar.TypeReg}}, want: "unsafe archive entry"},
		{name: "bundle missing", bundle: true, want: "archive does not contain"},
		{name: "bundle duplicate", bundle: true, entries: []tar.Header{{Name: "threadpoint/LICENSE", Typeflag: tar.TypeReg}, {Name: "threadpoint/LICENSE", Typeflag: tar.TypeReg}}, want: "duplicate entry"},
		{name: "bundle directory", bundle: true, entries: []tar.Header{{Name: "threadpoint/unexpected", Typeflag: tar.TypeDir}}, want: "unexpected archive entry"},
		{name: "bundle special file", bundle: true, entries: []tar.Header{{Name: "threadpoint/LICENSE", Typeflag: tar.TypeSymlink}}, want: "unexpected archive entry type"},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "threadpoint.tar.gz")
			writeSelfUpdateCoverageArchive(t, archive, test.entries)
			if test.bundle {
				_, err := extractThreadpointBundleFromArchive(archive, filepath.Join(t.TempDir(), "output"))
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("bundle extraction error = %v, want %q", err, test.want)
				}
				return
			}
			err := extractThreadpointBinaryFromArchive(archive, filepath.Join(t.TempDir(), "threadpoint"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("binary extraction error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSelfUpdateExtractsBinaryFromReleaseArchive(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "threadpoint.tar.gz")
	binaryPayload := []byte("threadpoint-release-binary")
	writeTestThreadpointReleaseArchive(t, archivePath, binaryPayload)

	destination := filepath.Join(dir, "threadpoint")
	if err := extractThreadpointBinaryFromArchive(archivePath, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binaryPayload) {
		t.Fatalf("extracted binary = %q, want %q", got, binaryPayload)
	}
}

func TestSelfUpdateArchiveRejectsOversizedBinaryEntry(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "threadpoint.tar.gz")
	writeTestThreadpointArchiveHeader(t, archivePath, tar.TypeReg, maxSelfUpdateExtractedBinaryBytes+1, nil)

	err := extractThreadpointBinaryFromArchive(archivePath, filepath.Join(t.TempDir(), "threadpoint"))
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected archive size cap error, got %v", err)
	}
}

func TestSelfUpdateArchiveCopyRejectsRuntimeOversizedBinary(t *testing.T) {
	var target bytes.Buffer
	written, err := copySelfUpdateBinary(&target, bytes.NewReader([]byte("12345")), 4)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected runtime copy size cap error, got written=%d err=%v", written, err)
	}
	if written != 5 {
		t.Fatalf("written = %d, want 5", written)
	}
}

func (rejectedInstallHelperWrite) Write([]byte) (int, error) {
	return 0, errors.New("injected helper output failure")
}

func writeSelfUpdateCoverageArchive(t *testing.T, path string, entries []tar.Header) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, header := range entries {
		if header.Mode == 0 {
			header.Mode = 0o644
		}
		if err := tarWriter.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTestThreadpointReleaseArchive(t *testing.T, path string, binary []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range []struct {
		name string
		mode int64
		body []byte
	}{
		{name: "threadpoint/LICENSE", mode: 0o644, body: []byte("license\n")},
		{name: "threadpoint/NOTICE", mode: 0o644, body: []byte("notice\n")},
		{name: "threadpoint/README.md", mode: 0o644, body: []byte("# readme\n")},
		{name: "threadpoint/scripts/install.sh", mode: 0o755, body: []byte("#!/bin/sh\n")},
		{name: "threadpoint/scripts/uninstall.sh", mode: 0o755, body: []byte("#!/bin/sh\n")},
		{name: "threadpoint/bin/threadpoint", mode: 0o755, body: binary},
	} {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name:     entry.name,
			Mode:     entry.mode,
			Size:     int64(len(entry.body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tarWriter, bytes.NewReader(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTestThreadpointArchiveHeader(t *testing.T, path string, typeflag byte, size int64, body []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{
		Name:     threadpointArchiveBinaryEntry(path),
		Mode:     0o755,
		Size:     size,
		Typeflag: typeflag,
	}); err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		if _, err := tarWriter.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if int64(len(body)) < size {
		if err := gzipWriter.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
