// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hasSuggestedCommand(report *Report, prefix string) bool {
	for _, command := range report.SuggestedCommands {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}
	return false
}

func hasFinding(report *Report, code string) bool {
	for _, finding := range report.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func mustWrite(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
