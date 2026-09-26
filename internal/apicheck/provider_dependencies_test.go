// SPDX-License-Identifier: Apache-2.0

package apicheck_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestProviderPackageKeepsLeafDependencies(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "list", "-deps", "./provider")
	command.Dir = moduleRoot(t)
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list provider dependencies: %v\n%s", err, output)
	}

	dependencies := map[string]bool{}
	for _, dependency := range strings.Fields(string(output)) {
		dependencies[dependency] = true
	}
	for _, forbidden := range []string{
		"github.com/threadgrid/threadpoint/backup",
		"github.com/threadgrid/threadpoint/internal/home",
		"github.com/threadgrid/threadpoint/layout",
		"github.com/threadgrid/threadpoint/safefs",
		"github.com/threadgrid/threadpoint/safepath",
	} {
		if dependencies[forbidden] {
			t.Errorf("provider transitively depends on %s", forbidden)
		}
	}
}
