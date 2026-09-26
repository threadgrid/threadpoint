// SPDX-License-Identifier: Apache-2.0

// Package releasesig owns the Ed25519 trust anchor and envelope shared by the
// release signer and managed-update verifier.
package releasesig

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// SchemaVersion, SigningKeyID, and Algorithm define the only release signature
// envelope accepted by Verify.
const (
	SchemaVersion = "threadpoint.release_signature.v1"
	SigningKeyID  = "release-2026-10-09"
	Algorithm     = "ed25519"

	signingPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEACW5cmRTXsxvwR+XlHxCY6y8m3DvqzV7OosYi59hv9CA=
-----END PUBLIC KEY-----`
)

// Envelope is the JSON document written next to checksums.txt as
// checksums.txt.sig.
type Envelope struct {
	SchemaVersion string `json:"schema_version"`
	Algorithm     string `json:"algorithm"`
	KeyID         string `json:"key_id"`
	Signature     string `json:"signature"`
}

// Sign produces a detached signature envelope over checksums using an Ed25519
// PEM private key. An empty keyID defaults to SigningKeyID.
func Sign(checksums []byte, privateKeyPEM string, keyID string) (Envelope, error) {
	privateKey, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return Envelope{}, err
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		keyID = SigningKeyID
	}
	return Envelope{
		SchemaVersion: SchemaVersion,
		Algorithm:     Algorithm,
		KeyID:         keyID,
		Signature:     base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, checksums)),
	}, nil
}

// Verify checks a signature envelope over checksums against the compiled-in
// release public key, enforcing the schema version, algorithm, and key id.
func Verify(checksums []byte, envelopeJSON []byte) error {
	var envelope Envelope
	if err := json.Unmarshal(envelopeJSON, &envelope); err != nil {
		return fmt.Errorf("malformed signature envelope: %w", err)
	}
	if envelope.SchemaVersion != SchemaVersion ||
		envelope.Algorithm != Algorithm ||
		envelope.KeyID != SigningKeyID ||
		strings.TrimSpace(envelope.Signature) == "" {
		return errors.New("unsupported signature envelope")
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil {
		return fmt.Errorf("malformed signature value: %w", err)
	}
	publicKey, err := PublicKey()
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, checksums, signature) {
		return errors.New("signature mismatch")
	}
	return nil
}

// PublicKey returns the compiled-in release verification key.
func PublicKey() (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(signingPublicKeyPEM)))
	if block == nil {
		return nil, errors.New("missing release public key PEM block")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	publicKey, ok := key.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("release public key is not Ed25519")
	}
	return publicKey, nil
}

func parsePrivateKey(pemText string) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("missing private key PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("private key is not Ed25519")
	}
	return privateKey, nil
}
