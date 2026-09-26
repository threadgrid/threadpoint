// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/threadgrid/threadpoint/stage"
)

func TestInspectProjectStageSourceChangesAndCorruption(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	git(t, root, "init")
	mustWrite(t, filepath.Join(root, "notes.md"), "original")
	created, err := stage.Create(context.Background(), stage.Options{
		Root: root, ThreadpointHome: state,
		Classify: map[string]stage.Scope{"notes.md": stage.ScopeProjectShared},
		Inputs:   []stage.Input{{Provider: "fixture", Source: "notes.md", Kind: stage.KindKnowledge}},
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Root: root, ThreadpointHome: state, HomeDir: t.TempDir()}
	for _, want := range []string{"unchanged", "changed", "missing"} {
		switch want {
		case "changed":
			mustWrite(t, filepath.Join(root, "notes.md"), "updated")
		case "missing":
			if err := os.Remove(filepath.Join(root, "notes.md")); err != nil {
				t.Fatal(err)
			}
		}
		items, err := inspectProjectStages(context.Background(), opts)
		if err != nil || len(items) != 1 || items[0].SourceState != want {
			t.Fatalf("got %+v %v, want %s", items, err, want)
		}
	}
	mustWrite(t, filepath.Join(created.Stages[0].Dir, "content.md"), "corrupt review")
	items, err := inspectProjectStages(context.Background(), opts)
	if err != nil || len(items) != 1 || items[0].SourceState != "unavailable" {
		t.Fatalf("corrupt review: %+v %v", items, err)
	}
	if _, err := os.Stat(created.Stages[0].Dir); err != nil {
		t.Fatal("inspection removed stage:", err)
	}
}

func TestInspectProjectStagesRejectsSourceChangedDuringInspection(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	git(t, root, "init")
	sourcePath := filepath.Join(root, "notes.md")
	mustWrite(t, sourcePath, "original")
	_, err := stage.Create(context.Background(), stage.Options{Root: root, ThreadpointHome: state, Classify: map[string]stage.Scope{"notes.md": stage.ScopeProjectShared}, Inputs: []stage.Input{{Provider: "fixture", Source: "notes.md", Kind: stage.KindKnowledge}}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	read := func(root, home, id string) ([]byte, error) {
		body, err := stage.ReadCurrentSource(root, home, id)
		calls++
		if calls == 1 {
			mustWrite(t, sourcePath, "concurrent edit")
		}
		return body, err
	}
	items, err := inspectProjectStagesWithSourceReader(context.Background(), Options{Root: root, ThreadpointHome: state, HomeDir: t.TempDir()}, read)
	if err != nil || calls != 2 || len(items) != 1 || items[0].SourceState != "unavailable" {
		t.Fatalf("inconsistent source accepted: %+v %v (%d reads)", items, err, calls)
	}
}
