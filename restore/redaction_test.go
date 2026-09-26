// SPDX-License-Identifier: Apache-2.0

package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPlanJSONRedactsConflictDiff(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	backupSecret := "sk-backupabcdefghijklmnopqrstuvwxyz"
	currentSecret := "sk-currentabcdefghijklmnopqrstuvwxyz"
	mustWriteFile(t, target, "OPENAI_API_KEY="+backupSecret+"\n")
	runID := seedFileBackup(t, root, home, target, "")
	mustWriteFile(t, target, "OPENAI_API_KEY="+currentSecret+"\n")

	plan, err := BuildPlan(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || !strings.Contains(plan.Candidates[0].Diff, currentSecret) {
		t.Fatalf("expected raw in-memory conflict diff, got %#v", plan.Candidates)
	}

	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), currentSecret) || strings.Contains(string(body), backupSecret) {
		t.Fatalf("JSON restore plan leaked secret: %s", body)
	}
	if !strings.Contains(string(body), "[REDACTED]") || !strings.Contains(string(body), `"redacted":true`) {
		t.Fatalf("expected redacted JSON restore candidate, got %s", body)
	}
}

func TestCandidateMarshalJSONRedactsAtJSONBoundary(t *testing.T) {
	t.Parallel()
	candidate := Candidate{
		Path:       "/home/example/project/AGENTS.md",
		Root:       "/home/example/project",
		BackupPath: "/home/example/project/.threadpoint-backups/AGENTS.md",
		Diff:       "+ Authorization: Bearer sk-abcdef0123456789",
	}
	body, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, forbidden := range []string{"sk-abcdef0123456789", "/home/example/project"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("candidate JSON leaked %q: %s", forbidden, got)
		}
	}
	for _, want := range []string{"[REDACTED]", "$ROOT"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected redaction marker %q in output: %s", want, got)
		}
	}
	if !strings.Contains(got, `"redacted":true`) {
		t.Fatalf("expected redacted flag to be set: %s", got)
	}
}

func TestRunInteractiveRedactsConflictDiff(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	backupSecret := "sk-backupabcdefghijklmnopqrstuvwxyz"
	currentSecret := "sk-currentabcdefghijklmnopqrstuvwxyz"
	mustWriteFile(t, target, "OPENAI_API_KEY="+backupSecret+"\n")
	runID := seedFileBackup(t, root, home, target, "")
	mustWriteFile(t, target, "OPENAI_API_KEY="+currentSecret+"\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	_, err := RunInteractive(context.Background(), Options{Root: root, HomeDir: home, BackupID: runID}, strings.NewReader("s\n"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), currentSecret) || strings.Contains(stdout.String(), backupSecret) {
		t.Fatalf("interactive restore preview leaked secret:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "[REDACTED]") {
		t.Fatalf("expected redacted restore preview:\n%s", stdout.String())
	}
}
