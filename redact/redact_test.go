// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"strings"
	"testing"
)

func TestRedactStringRedactsRepresentativeSecrets(t *testing.T) {
	input := strings.Join([]string{
		`OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwxyz`,
		`password: "correct-horse-battery-staple"`,
		`Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz`,
		"-----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY-----",
	}, "\n")

	output, redacted := String(input)
	if !redacted {
		t.Fatal("expected redaction")
	}
	for _, leaked := range []string{
		"sk-abcdefghijklmnopqrstuvwxyz",
		"correct-horse-battery-staple",
		"ghp_abcdefghijklmnopqrstuvwxyz",
		"abc123",
	} {
		if strings.Contains(output, leaked) {
			t.Fatalf("secret value leaked in %q", output)
		}
	}
	if count := strings.Count(output, marker); count < 4 {
		t.Fatalf("expected redaction markers, got %d in %q", count, output)
	}
}

func TestRedactStringAvoidsPlainFalsePositiveText(t *testing.T) {
	input := "The tokenization rules and password policy are documented without values."

	output, redacted := String(input)
	if redacted {
		t.Fatalf("did not expect redaction: %q", output)
	}
	if output != input {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestRedactStringRedactsEntireQuotedAssignment(t *testing.T) {
	output, redacted := String(`PASSWORD="correct horse battery"`)
	if !redacted || strings.Contains(output, "correct horse battery") {
		t.Fatalf("quoted secret leaked: %q", output)
	}
	if output != `PASSWORD="[REDACTED]"` {
		t.Fatalf("quoted redaction = %q", output)
	}
}

func TestRedactStringWithOptionsRedactsPrivatePrefixesAndKnownValues(t *testing.T) {
	input := "token=custom-secret path=/home/example/project/.agents/knowledge.md"
	output, redacted := StringWithOptions(input, Options{
		Prefixes:        []Prefix{{Value: "/home/example/project", Label: "$ROOT"}, {Value: "/home/example", Label: "$HOME"}},
		SensitiveValues: []string{"custom-secret"},
	})
	if !redacted || strings.Contains(output, "custom-secret") || strings.Contains(output, "/home/example") {
		t.Fatalf("configured values leaked in %q", output)
	}
	if !strings.Contains(output, "$ROOT/.agents/knowledge.md") {
		t.Fatalf("private root was not replaced with its stable label: %q", output)
	}
}
