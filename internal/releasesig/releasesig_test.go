// SPDX-License-Identifier: Apache-2.0

package releasesig

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignRejectsMalformedAndNonEd25519PrivateKeys(t *testing.T) {
	tests := map[string]string{
		"missing PEM":   "not a PEM document",
		"malformed DER": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not DER")})),
	}
	for name, privateKey := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Sign([]byte("checksums"), privateKey, SigningKeyID); err == nil {
				t.Fatal("Sign unexpectedly accepted an invalid private key")
			}
		})
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	rsaPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if _, err := Sign([]byte("checksums"), rsaPEM, SigningKeyID); err == nil || !strings.Contains(err.Error(), "not Ed25519") {
		t.Fatalf("RSA private key error = %v", err)
	}
}

func TestVerifyRejectsMalformedBase64Signature(t *testing.T) {
	body, err := json.Marshal(Envelope{
		SchemaVersion: SchemaVersion,
		Algorithm:     Algorithm,
		KeyID:         SigningKeyID,
		Signature:     "%%%not-base64%%%",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify([]byte("checksums"), body); err == nil || !strings.Contains(err.Error(), "malformed signature value") {
		t.Fatalf("malformed signature error = %v", err)
	}
}

func TestSignProducesEnvelopeVerifiableByItsKey(t *testing.T) {
	privPEM, pub := ed25519PrivateKeyPEM(t)
	checksums := []byte("abc123  threadpoint_0.1.0_linux_amd64.tar.gz\n")
	env, err := Sign(checksums, privPEM, "")
	if err != nil {
		t.Fatal(err)
	}
	if env.SchemaVersion != SchemaVersion || env.Algorithm != Algorithm || env.KeyID != SigningKeyID {
		t.Fatalf("unexpected envelope metadata: %+v", env)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, checksums, sig) {
		t.Fatal("Sign output did not verify under its own signing key")
	}
}

func TestVerifyRejectsSignatureFromForeignKey(t *testing.T) {
	privPEM, _ := ed25519PrivateKeyPEM(t)
	checksums := []byte("abc123  threadpoint_0.1.0_linux_amd64.tar.gz\n")
	env, err := Sign(checksums, privPEM, "")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(checksums, body); err == nil || !strings.Contains(err.Error(), "signature mismatch") {
		t.Fatalf("expected signature mismatch for a foreign signing key, got %v", err)
	}
}

func TestVerifyRejectsUnsupportedEnvelope(t *testing.T) {
	privPEM, _ := ed25519PrivateKeyPEM(t)
	checksums := []byte("abc")
	base, err := Sign(checksums, privPEM, "")
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(Envelope) Envelope{
		"schema":    func(e Envelope) Envelope { e.SchemaVersion = "other"; return e },
		"algorithm": func(e Envelope) Envelope { e.Algorithm = "rsa"; return e },
		"key_id":    func(e Envelope) Envelope { e.KeyID = "release-1999-01-01"; return e },
		"signature": func(e Envelope) Envelope { e.Signature = ""; return e },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(mutate(base))
			if err != nil {
				t.Fatal(err)
			}
			if err := Verify(checksums, body); err == nil || !strings.Contains(err.Error(), "unsupported signature envelope") {
				t.Fatalf("expected unsupported envelope, got %v", err)
			}
		})
	}
}

func TestVerifyRejectsMalformedJSON(t *testing.T) {
	if err := Verify([]byte("abc"), []byte("{not json")); err == nil ||
		!strings.Contains(err.Error(), "malformed signature envelope") {
		t.Fatalf("expected malformed envelope error, got %v", err)
	}
}

func TestPublicKeyIsEd25519(t *testing.T) {
	pub, err := PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("public key size = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
}

func ed25519PrivateKeyPEM(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), pub
}

func TestReleaseTrustAnchorCopiesAgree(t *testing.T) {
	installer, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installer), "<<'KEY'\n"+signingPublicKeyPEM+"\nKEY\n") {
		t.Fatal("scripts/install.sh embeds a different release public key than internal/releasesig")
	}
	if !strings.Contains(string(installer), `"key_id": "`+SigningKeyID+`"`) {
		t.Fatalf("scripts/install.sh does not expect release key id %s", SigningKeyID)
	}
	publisher, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release-publish.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(publisher), "--key-id "+SigningKeyID+"\n") {
		t.Fatalf("release-publish.yml does not sign with release key id %s", SigningKeyID)
	}
}
