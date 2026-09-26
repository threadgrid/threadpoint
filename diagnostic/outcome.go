// SPDX-License-Identifier: Apache-2.0

// Package diagnostic defines small, provider-neutral outcome contracts for
// threadpoint command and library boundaries.
package diagnostic

import "regexp"
import "strings"

// Outcome is a safe, finite operational result. Code is machine-readable;
// Message and Remediation are human-readable local text. Detail is optional,
// bounded, redacted operator context and must never contain an arbitrary error.
type Outcome struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// CodedError lets command boundaries preserve an explicitly safe outcome
// through Go error wrapping without matching human-readable error text.
type CodedError interface {
	error
	DiagnosticOutcome() Outcome
}

var outcomeCodePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)

// ValidCode reports whether code uses the stable unprefixed snake_case grammar.
func ValidCode(code string) bool {
	return outcomeCodePattern.MatchString(code) && !strings.HasPrefix(code, "threadpoint_")
}
