// SPDX-License-Identifier: Apache-2.0

package scan_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/threadgrid/threadpoint/layout"
	"github.com/threadgrid/threadpoint/scan"
)

func ExampleRun() {
	root, err := os.MkdirTemp("", "threadpoint-scan-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	if err := layout.EnsureShared(root); err != nil {
		panic(err)
	}
	note := filepath.Join(root, ".agents", "knowledge", "project.md")
	if err := os.WriteFile(note, []byte("# Project\n"), 0o600); err != nil {
		panic(err)
	}

	kb, err := scan.Run(context.Background(), scan.Options{Root: root})
	if err != nil {
		panic(err)
	}
	fmt.Println(len(kb.Records) > 0)

	// Output:
	// true
}
