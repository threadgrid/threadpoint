// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/threadgrid/threadpoint/clierror"
	"github.com/threadgrid/threadpoint/diagnostic"
	"github.com/threadgrid/threadpoint/doctor"
	"github.com/threadgrid/threadpoint/layout"
)

func TestCLIErrorUnwrapAndExitCodesStayStable(t *testing.T) {
	cause := errors.New("invalid input")
	wrapped := usageError(cause)
	if !errors.Is(wrapped, cause) || exitCode(wrapped) != ExitUsage {
		t.Fatalf("usage error = %v, exit=%d", wrapped, exitCode(wrapped))
	}
	for _, test := range []struct {
		err  error
		code int
	}{
		{err: validationError(cause), code: ExitValidation},
		{err: refusedError(cause), code: ExitRefused},
		{err: interruptedError(cause), code: ExitInterrupted},
		{err: cause, code: ExitInternal},
	} {
		if got := exitCode(test.err); got != test.code {
			t.Fatalf("exitCode(%v) = %d, want %d", test.err, got, test.code)
		}
	}
}

func TestCLIErrorCategoriesAndDiagnosticOutcomesStayStable(t *testing.T) {
	cause := errors.New("fixture failure")
	for _, test := range []struct {
		err      error
		category string
		code     string
		exit     int
	}{
		{err: cause, category: "internal", code: "internal_failure", exit: ExitInternal},
		{err: refusedError(cause), category: "refused", code: "operation_refused", exit: ExitRefused},
		{err: &cliError{kind: cliErrorPartial, err: cause}, category: "partial", code: "operation_partial", exit: ExitPartial},
		{err: &cliError{kind: cliErrorInternal, err: cause}, category: "internal", code: "internal_failure", exit: ExitInternal},
	} {
		if got := cliErrorCategory(test.err); got != test.category {
			t.Errorf("cliErrorCategory(%v) = %q, want %q", test.err, got, test.category)
		}
		if got := cliErrorOutcome(test.err); got.Code != test.code || got.Message != cause.Error() {
			t.Errorf("cliErrorOutcome(%v) = %#v, want code %q", test.err, got, test.code)
		}
		if got := exitCode(test.err); got != test.exit {
			t.Errorf("exitCode(%v) = %d, want %d", test.err, got, test.exit)
		}
	}

	coded := diagnosticCoverageError{outcome: diagnostic.Outcome{Code: "fixture_refused", Message: "safe fixture message"}}
	if got := cliErrorOutcome(coded); got != coded.outcome {
		t.Fatalf("coded diagnostic outcome = %#v, want %#v", got, coded.outcome)
	}
	invalid := diagnosticCoverageError{outcome: diagnostic.Outcome{Code: "INVALID", Message: "unsafe"}}
	if got := cliErrorOutcome(invalid); got.Code != "internal_failure" || got.Message != invalid.Error() {
		t.Fatalf("invalid coded diagnostic should fall back: %#v", got)
	}
}

func TestCLIErrorWrappingPreservesExistingClassification(t *testing.T) {
	classified := refusedError(errors.New("already classified"))
	if wrapCLIError(cliErrorUsage, nil) != nil {
		t.Fatal("nil error should remain nil")
	}
	if got := wrapCLIError(cliErrorUsage, classified); !errors.Is(got, classified) {
		t.Fatalf("existing classification was replaced: %v", got)
	}
}

func TestRuntimeErrorClassificationPreservesDomainErrorKinds(t *testing.T) {
	usage := clierror.Usage(errors.New("bad domain arguments"))
	if got := classifyRuntimeError(usage); exitCode(got) != ExitUsage {
		t.Fatalf("domain usage classification = %v, exit=%d", got, exitCode(got))
	}
	refused := clierror.Refused(errors.New("unsafe mutation"))
	if got := classifyRuntimeError(refused); exitCode(got) != ExitRefused {
		t.Fatalf("domain refusal classification = %v, exit=%d", got, exitCode(got))
	}
}

func TestExecuteWritesStructuredJSONErrorsWithoutStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), &stdout, &stderr, strings.NewReader(""), []string{"--format", "json", "unknown"})
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	var report struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		ExitCode      int    `json:"exit_code"`
		Code          string `json:"code"`
		Message       string `json:"message"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &report); err != nil {
		t.Fatalf("JSON error = %q: %v", stderr.String(), err)
	}
	if report.SchemaVersion != "threadpoint.error.v1" || report.OK || report.ExitCode != ExitUsage || report.Code != "usage_invalid_request" || report.Message == "" || strings.Contains(stderr.String(), `"category"`) {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestExecuteMapsCLIContractExitCodes(t *testing.T) {
	stdout, stderr, code := runTestExecute(t, "--help")
	if code != ExitOK {
		t.Fatalf("expected help exit %d, got %d", ExitOK, code)
	}
	if stdout != "" {
		t.Fatalf("expected help stdout to be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("expected help on stderr, got %q", stderr)
	}

	stdout, stderr, code = runTestExecute(t, "unknown")
	if code != ExitUsage {
		t.Fatalf("expected unknown command exit %d, got %d", ExitUsage, code)
	}
	if stdout != "" {
		t.Fatalf("expected unknown command stdout to be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, `unknown command "unknown"`) {
		t.Fatalf("expected unknown command on stderr, got %q", stderr)
	}

	stdout, stderr, code = runTestExecute(t, "status", "unexpected")
	if code != ExitUsage {
		t.Fatalf("expected positional argument exit %d, got %d", ExitUsage, code)
	}
	if stdout != "" {
		t.Fatalf("expected positional argument stdout to be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, "status does not accept positional arguments") {
		t.Fatalf("expected positional argument error on stderr, got %q", stderr)
	}

	stdout, stderr, code = runTestExecute(t, "doctor", "--format", "yaml")
	if code != ExitUsage {
		t.Fatalf("expected invalid format exit %d, got %d", ExitUsage, code)
	}
	if stdout != "" {
		t.Fatalf("expected invalid format stdout to be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, `unknown doctor format "yaml"`) {
		t.Fatalf("expected invalid format on stderr, got %q", stderr)
	}
}

func TestCanceledRootContextMapsToInterrupted(t *testing.T) {
	rootCtx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := execute(rootCtx, &stdout, &stderr, strings.NewReader(""), []string{"version"})
	if code != ExitInterrupted {
		t.Fatalf("expected canceled context exit %d, got %d\nstderr:\n%s", ExitInterrupted, code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("expected no stdout for canceled context, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), context.Canceled.Error()) {
		t.Fatalf("expected canceled context on stderr, got %q", stderr.String())
	}
}

func TestExecuteKeepsJSONStdoutValidOnValidationFailures(t *testing.T) {
	root := t.TempDir()

	stdout, stderr, code := runTestExecute(t, "status", "--root", root, "--format", "json")
	if code != ExitValidation {
		t.Fatalf("expected status exit %d, got %d", ExitValidation, code)
	}
	var validationReport layout.Report
	if err := json.Unmarshal([]byte(stdout), &validationReport); err != nil {
		t.Fatalf("status stdout is not valid JSON: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if validationReport.OK {
		t.Fatalf("expected invalid validation report, got %#v", validationReport)
	}
	if !strings.Contains(stderr, "threadpoint status found errors") {
		t.Fatalf("expected validation error on stderr, got %q", stderr)
	}

	stdout, stderr, code = runTestExecute(t, "status", "--root", root, "--format", "json")
	if code != ExitValidation {
		t.Fatalf("expected status exit %d, got %d", ExitValidation, code)
	}
	var doctorReport doctor.Report
	if err := json.Unmarshal([]byte(stdout), &doctorReport); err != nil {
		t.Fatalf("doctor stdout is not valid JSON: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if doctorReport.OK {
		t.Fatalf("expected invalid doctor report, got %#v", doctorReport)
	}
	if !strings.Contains(stderr, "threadpoint status found errors") {
		t.Fatalf("expected status error on stderr, got %q", stderr)
	}
}

func runTestExecute(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(context.Background(), &stdout, &stderr, strings.NewReader(""), args)
	return stdout.String(), stderr.String(), code
}
