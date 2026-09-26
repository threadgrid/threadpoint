// SPDX-License-Identifier: Apache-2.0

package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/threadgrid/threadpoint/layout"
)

func TestScanPublicAPI(t *testing.T) {
	root := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agents", "knowledge", "note.md"), []byte("# Note\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	kb, err := Run(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(kb.Records) == 0 {
		t.Fatal("expected public mapping wrapper to return records")
	}
}
