// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/threadgrid/threadpoint/clierror"
	"github.com/threadgrid/threadpoint/diagnostic"
)

// ExitOK through ExitInterrupted are stable process outcomes used by CLI callers
// to distinguish usage, validation, refusal, partial, and signal exits.
const (
	ExitOK          = 0
	ExitInternal    = 1
	ExitUsage       = 2
	ExitValidation  = 3
	ExitRefused     = 4
	ExitPartial     = 5
	ExitInterrupted = 130
)

type cliErrorKind int

const (
	cliErrorInternal cliErrorKind = iota
	cliErrorUsage
	cliErrorValidation
	cliErrorRefused
	cliErrorPartial
	cliErrorInterrupted
)

type cliError struct {
	kind cliErrorKind
	err  error
}

type errorReport struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	ExitCode      int    `json:"exit_code"`
	Code          string `json:"code"`
	Message       string `json:"message"`
	Command       string `json:"command,omitempty"`
}

func (err *cliError) Error() string {
	return err.err.Error()
}

func (err *cliError) Unwrap() error {
	return err.err
}

func exitCode(err error) int {
	var cliErr *cliError
	if !errors.As(err, &cliErr) {
		return ExitInternal
	}
	switch cliErr.kind {
	case cliErrorUsage:
		return ExitUsage
	case cliErrorValidation:
		return ExitValidation
	case cliErrorRefused:
		return ExitRefused
	case cliErrorPartial:
		return ExitPartial
	case cliErrorInterrupted:
		return ExitInterrupted
	default:
		return ExitInternal
	}
}

func cliErrorCategory(err error) string {
	var cliErr *cliError
	if !errors.As(err, &cliErr) {
		return "internal"
	}
	switch cliErr.kind {
	case cliErrorUsage:
		return "usage"
	case cliErrorValidation:
		return "validation"
	case cliErrorRefused:
		return "refused"
	case cliErrorPartial:
		return "partial"
	case cliErrorInterrupted:
		return "interrupted"
	default:
		return "internal"
	}
}

func cliErrorOutcome(err error) diagnostic.Outcome {
	code := "internal_failure"
	switch cliErrorCategory(err) {
	case "usage":
		code = "usage_invalid_request"
	case "validation":
		code = "validation_failed"
	case "refused":
		code = "operation_refused"
	case "partial":
		code = "operation_partial"
	case "interrupted":
		code = "operation_interrupted"
	}
	outcome := diagnostic.Outcome{Code: code, Message: err.Error()}
	var coded diagnostic.CodedError
	if errors.As(err, &coded) {
		candidate := coded.DiagnosticOutcome()
		if diagnostic.ValidCode(candidate.Code) && strings.TrimSpace(candidate.Message) != "" {
			return candidate
		}
	}
	return outcome
}

func writeCLIError(writer io.Writer, app *cli, args []string, err error) error {
	outcome := cliErrorOutcome(err)
	if shouldEmitJSONError(app, args) {
		report := errorReport{
			SchemaVersion: "threadpoint.error.v1",
			OK:            false,
			ExitCode:      exitCode(err),
			Code:          outcome.Code,
			Message:       outcome.Message,
			Command:       commandNameFromArgs(args),
		}
		encoder := json.NewEncoder(writer)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	_, writeErr := fmt.Fprintf(writer, "%s (code: %s)\n", outcome.Message, outcome.Code)
	return writeErr
}

func shouldEmitJSONError(app *cli, args []string) bool {
	if app != nil && app.formatExplicit && app.format == outputFormatJSON {
		return true
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == longFormatFlag || arg == "-format":
			return index+1 < len(args) && strings.EqualFold(strings.TrimSpace(args[index+1]), outputFormatJSON)
		case strings.HasPrefix(arg, longFormatFlag+"="):
			return strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(arg, longFormatFlag+"=")), outputFormatJSON)
		case strings.HasPrefix(arg, "-format="):
			return strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(arg, "-format=")), outputFormatJSON)
		}
	}
	return false
}

func commandNameFromArgs(args []string) string {
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == longRootFlag || arg == "-root" || arg == "--home" || arg == "-home" || arg == longFormatFlag || arg == "-format" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

func usageError(err error) error {
	return wrapCLIError(cliErrorUsage, err)
}

func usageErrorf(format string, args ...any) error {
	return usageError(fmt.Errorf(format, args...))
}

func validationError(err error) error {
	return wrapCLIError(cliErrorValidation, err)
}

func refusedError(err error) error {
	return wrapCLIError(cliErrorRefused, err)
}

func interruptedError(err error) error {
	return wrapCLIError(cliErrorInterrupted, err)
}

func wrapCLIError(kind cliErrorKind, err error) error {
	if err == nil {
		return nil
	}
	var cliErr *cliError
	if errors.As(err, &cliErr) {
		return err
	}
	return &cliError{kind: kind, err: err}
}

func classifyRuntimeError(err error) error {
	if err == nil {
		return nil
	}
	var cliErr *cliError
	if errors.As(err, &cliErr) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return interruptedError(err)
	}
	var classified *clierror.Error
	if errors.As(err, &classified) {
		switch classified.Kind {
		case clierror.KindUsage:
			return usageError(err)
		default:
			return refusedError(err)
		}
	}
	return err
}
