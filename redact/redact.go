// SPDX-License-Identifier: Apache-2.0

// Package redact removes obvious secrets and configured private values from
// product-owned rendered output. It never changes source artifacts or stored
// memory payloads.
package redact

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const marker = "[REDACTED]"

var (
	privateKeyBlock = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	authorization   = regexp.MustCompile(`(?im)(\bauthorization\s*:\s*(?:bearer|basic)\s+)[^\s]+`)
	assignment      = regexp.MustCompile(`(?im)(\b[A-Z0-9_.-]*(?:api[_-]?key|token|secret|password|passwd|pwd|credential)[A-Z0-9_.-]*\b\s*[:=]\s*)("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s"',;]+)`)
	commonToken     = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{12,}|ghp_[A-Za-z0-9_]{12,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16})\b`)
)

// Prefix describes a private path prefix and its safe rendered label.
type Prefix struct {
	Value string
	Label string
}

// Options configures best-effort redaction for one product-owned output.
type Options struct {
	Prefixes        []Prefix
	SensitiveValues []string
	Marker          string
}

type normalizedPrefix struct {
	value string
	label string
}

// String applies best-effort secret-pattern redaction without caller
// supplied values or private path prefixes.
func String(input string) (string, bool) {
	return StringWithOptions(input, Options{})
}

// StringWithOptions applies configured value and private-path redaction
// before the built-in obvious-secret patterns. It is for rendering only.
func StringWithOptions(input string, opts Options) (string, bool) {
	output := input
	replacement := strings.TrimSpace(opts.Marker)
	if replacement == "" {
		replacement = marker
	}
	for _, prefix := range normalizedPrefixes(opts.Prefixes, replacement) {
		output = strings.ReplaceAll(output, prefix.value, prefix.label)
		slash := filepath.ToSlash(prefix.value)
		if slash != prefix.value {
			output = strings.ReplaceAll(output, slash, prefix.label)
		}
	}
	for _, sensitive := range opts.SensitiveValues {
		if sensitive = strings.TrimSpace(sensitive); len(sensitive) >= 3 {
			output = strings.ReplaceAll(output, sensitive, replacement)
		}
	}
	output = privateKeyBlock.ReplaceAllString(output, replacement)
	output = authorization.ReplaceAllString(output, `${1}`+replacement)
	output = assignment.ReplaceAllStringFunc(output, func(value string) string { return redactAssignment(value, replacement) })
	output = commonToken.ReplaceAllString(output, replacement)
	return output, output != input
}

func normalizedPrefixes(prefixes []Prefix, replacement string) []normalizedPrefix {
	result := make([]normalizedPrefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		value := strings.TrimSpace(prefix.Value)
		if value == "" {
			continue
		}
		clean := filepath.Clean(value)
		if clean == "." {
			continue
		}
		label := strings.TrimSpace(prefix.Label)
		if label == "" {
			label = replacement
		}
		result = append(result, normalizedPrefix{value: clean, label: label})
	}
	sort.SliceStable(result, func(i, j int) bool { return len(result[i].value) > len(result[j].value) })
	return result
}

func redactAssignment(value string, replacement string) string {
	parts := assignment.FindStringSubmatch(value)
	if len(parts) != 3 {
		return replacement
	}
	secret := parts[2]
	if len(secret) >= 2 && ((secret[0] == '"' && secret[len(secret)-1] == '"') || (secret[0] == '\'' && secret[len(secret)-1] == '\'')) {
		return parts[1] + secret[:1] + replacement + secret[len(secret)-1:]
	}
	return parts[1] + replacement
}
