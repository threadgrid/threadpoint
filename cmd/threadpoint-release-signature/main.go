// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/threadgrid/threadpoint/internal/releasesig"
)

func main() {
	input := flag.String("in", "", "checksums.txt path")
	output := flag.String("out", "", "checksums.txt.sig path")
	keyEnv := flag.String("key-env", "THREADPOINT_RELEASE_SIGNING_PRIVATE_KEY", "environment variable containing an Ed25519 PEM private key")
	keyID := flag.String("key-id", releasesig.SigningKeyID, "release signing key id")
	verify := flag.Bool("verify", false, "verify --out against --in using the compiled-in public key instead of signing")
	allowUntrustedKey := flag.Bool("allow-untrusted-key", false, "skip the post-signing self-verify against the compiled-in public key")
	flag.Parse()

	var err error
	if *verify {
		err = verifyChecksums(*input, *output)
	} else {
		err = signChecksums(*input, *output, *keyEnv, *keyID, *allowUntrustedKey)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func signChecksums(input string, output string, keyEnv string, keyID string, allowUntrustedKey bool) error {
	if strings.TrimSpace(input) == "" || strings.TrimSpace(output) == "" {
		return errors.New("--in and --out are required")
	}
	keyPEM := strings.TrimSpace(os.Getenv(keyEnv))
	if keyPEM == "" {
		return fmt.Errorf("%s is required", keyEnv)
	}
	// #nosec G304 -- release signer reads the operator-selected checksums file.
	checksums, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	envelope, err := releasesig.Sign(checksums, strings.ReplaceAll(keyPEM, `\n`, "\n"), keyID)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	// #nosec G301 -- release signature envelopes are public release artifacts.
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	// #nosec G306 G703 -- release signature envelopes are public release metadata.
	if err := os.WriteFile(output, body, 0o644); err != nil {
		return err
	}
	if allowUntrustedKey {
		return nil
	}
	if err := releasesig.Verify(checksums, body); err != nil {
		return fmt.Errorf("signed envelope does not verify against the compiled-in release public key: %w", err)
	}
	return nil
}

func verifyChecksums(input string, output string) error {
	if strings.TrimSpace(input) == "" || strings.TrimSpace(output) == "" {
		return errors.New("--in and --out are required")
	}
	// #nosec G304 -- verifier reads the operator-selected checksums file.
	checksums, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	// #nosec G304 -- verifier reads the operator-selected signature envelope.
	envelopeJSON, err := os.ReadFile(output)
	if err != nil {
		return err
	}
	if err := releasesig.Verify(checksums, envelopeJSON); err != nil {
		return fmt.Errorf("verify %s: %w", output, err)
	}
	return nil
}
