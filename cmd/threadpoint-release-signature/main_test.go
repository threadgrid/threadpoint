// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/internal/releasesig"
)

func TestMainSignsChecksumsWhenGivenAValidOperatorKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "checksums.txt")
	output := filepath.Join(dir, "checksums.txt.sig")
	if err := os.WriteFile(input, []byte("deadbeef  threadpoint.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("THREADPOINT_TEST_MAIN_SIGNING_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))

	oldArgs, oldFlags := os.Args, flag.CommandLine
	os.Args = []string{"threadpoint-release-signature", "--in", input, "--out", output, "--key-env", "THREADPOINT_TEST_MAIN_SIGNING_KEY", "--allow-untrusted-key"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlags
	})
	main()
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("signature output was not written: %v", err)
	}
}

func TestSignChecksumsFailsClosedForInvalidInputs(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(input, []byte("deadbeef  threadpoint.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyEnv := "THREADPOINT_TEST_ERROR_SIGNING_KEY"
	output := filepath.Join(dir, "checksums.txt.sig")

	t.Run("missing output", func(t *testing.T) {
		if err := signChecksums(input, " ", keyEnv, "test", true); err == nil || !strings.Contains(err.Error(), "--in and --out") {
			t.Fatalf("missing output error = %v", err)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		t.Setenv(keyEnv, "")
		if err := signChecksums(input, output, keyEnv, "test", true); err == nil || !strings.Contains(err.Error(), keyEnv+" is required") {
			t.Fatalf("missing key error = %v", err)
		}
	})

	t.Run("missing checksums", func(t *testing.T) {
		t.Setenv(keyEnv, releaseTestPrivateKeyPEM(t))
		if err := signChecksums(filepath.Join(dir, "missing"), output, keyEnv, "test", true); err == nil || !os.IsNotExist(err) {
			t.Fatalf("missing checksums error = %v", err)
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		t.Setenv(keyEnv, "not a PEM key")
		if err := signChecksums(input, output, keyEnv, "test", true); err == nil || !strings.Contains(err.Error(), "private key PEM") {
			t.Fatalf("invalid key error = %v", err)
		}
	})
}

func TestSignChecksumsReportsPublicationFailures(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(input, []byte("deadbeef  threadpoint.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyEnv := "THREADPOINT_TEST_PUBLICATION_SIGNING_KEY"
	t.Setenv(keyEnv, releaseTestPrivateKeyPEM(t))

	t.Run("output parent is not a directory", func(t *testing.T) {
		parent := filepath.Join(dir, "parent-file")
		if err := os.WriteFile(parent, []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := signChecksums(input, filepath.Join(parent, "signature"), keyEnv, "test", true)
		if err == nil {
			t.Fatal("expected signature parent creation to fail")
		}
	})

	t.Run("output is a directory", func(t *testing.T) {
		output := filepath.Join(dir, "signature-directory")
		if err := os.Mkdir(output, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := signChecksums(input, output, keyEnv, "test", true); err == nil {
			t.Fatal("expected writing a signature over a directory to fail")
		}
	})
}

func TestSignChecksumsRejectsKeyOutsideCompiledTrustAnchor(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "checksums.txt")
	output := filepath.Join(dir, "checksums.txt.sig")
	if err := os.WriteFile(input, []byte("deadbeef  threadpoint.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyEnv := "THREADPOINT_TEST_UNTRUSTED_SIGNING_KEY"
	t.Setenv(keyEnv, releaseTestPrivateKeyPEM(t))

	err := signChecksums(input, output, keyEnv, releasesig.SigningKeyID, false)
	if err == nil || !strings.Contains(err.Error(), "does not verify against the compiled-in release public key") {
		t.Fatalf("untrusted signing key error = %v", err)
	}
}

func TestVerifyChecksumsReportsInvalidInputsAndArtifacts(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "checksums.txt")
	output := filepath.Join(dir, "checksums.txt.sig")
	if err := os.WriteFile(input, []byte("deadbeef  threadpoint.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := verifyChecksums(input, " "); err == nil || !strings.Contains(err.Error(), "--in and --out") {
		t.Fatalf("missing output error = %v", err)
	}
	if err := verifyChecksums(filepath.Join(dir, "missing-checksums"), output); err == nil || !os.IsNotExist(err) {
		t.Fatalf("missing checksums error = %v", err)
	}
	if err := verifyChecksums(input, output); err == nil || !os.IsNotExist(err) {
		t.Fatalf("missing envelope error = %v", err)
	}
	if err := os.WriteFile(output, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksums(input, output); err == nil || !strings.Contains(err.Error(), "verify "+output) {
		t.Fatalf("malformed envelope error = %v", err)
	}
}

func TestSignChecksumsWritesSnapshotEnvelope(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("THREADPOINT_TEST_SIGNING_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	input := filepath.Join(t.TempDir(), "checksums.txt")
	output := filepath.Join(t.TempDir(), "signatures", "checksums.txt.sig")
	if err := os.WriteFile(input, []byte("deadbeef  threadpoint.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := signChecksums(input, output, "THREADPOINT_TEST_SIGNING_KEY", "snapshot-key", true); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"key_id": "snapshot-key"`) {
		t.Fatalf("signature envelope = %s", body)
	}
	if err := signChecksums("", output, "THREADPOINT_TEST_SIGNING_KEY", "snapshot-key", true); err == nil {
		t.Fatal("expected missing input to fail")
	}
	if err := verifyChecksums(input, output); err == nil {
		t.Fatal("snapshot signature must not verify against the production trust anchor")
	}
}

func releaseTestPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
