// SPDX-License-Identifier: Apache-2.0

// Package clierror lets domain packages classify an error by its intended CLI
// exit semantics so command or embedding layers can map it to stable exit
// codes via errors.As rather than matching human-readable message strings.
package clierror

// Kind is the CLI exit classification an error carries.
type Kind int

const (
	// KindRefused marks a refused mutation or unmet precondition.
	KindRefused Kind = iota
	// KindUsage marks misuse of flags or arguments.
	KindUsage
)

// Error wraps an error with a CLI exit Kind.
type Error struct {
	Kind Kind
	Err  error
}

// Error returns the wrapped error message.
func (e *Error) Error() string { return e.Err.Error() }

// Unwrap returns the wrapped error.
func (e *Error) Unwrap() error { return e.Err }

// Refused tags err as a refused mutation or unmet precondition.
func Refused(err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: KindRefused, Err: err}
}

// Usage tags err as flag or argument misuse.
func Usage(err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: KindUsage, Err: err}
}
