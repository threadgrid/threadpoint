// SPDX-License-Identifier: Apache-2.0

package apicheck_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		contents, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
		if readErr == nil && hasModuleDirective(contents, "github.com/threadgrid/threadpoint") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("locate github.com/threadgrid/threadpoint module root")
		}
		dir = parent
	}
}

func hasModuleDirective(contents []byte, want string) bool {
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1] == want
		}
	}
	return false
}
