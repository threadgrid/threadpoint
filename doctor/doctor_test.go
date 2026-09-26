// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/layout"
	"github.com/threadgrid/threadpoint/provider"
)

func TestRunKeepsWarningsExitClean(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("expected report to remain OK with warnings, got %#v", report)
	}
	if report.Summary.ValidationIssues != 0 {
		t.Fatalf("expected no validation issues, got %#v", report.Summary)
	}
}

func TestRunDetectsMissingLayout(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	report, err := Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK {
		t.Fatalf("expected missing layout to fail, got %#v", report)
	}
	if report.Summary.ValidationIssues == 0 {
		t.Fatalf("expected validation issues, got %#v", report.Summary)
	}
	if !hasSuggestedCommand(report, "threadpoint init --root") {
		t.Fatalf("expected init suggestion, got %#v", report.SuggestedCommands)
	}
}

func TestRunIgnoresArtifactsOutsideSelectedRoot(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	root := filepath.Join(parent, "project")
	sibling := filepath.Join(base, "sibling")
	home := filepath.Join(base, "home")
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(parent, "CLAUDE.md"), "parent claude")
	mustWrite(t, filepath.Join(sibling, ".agents", "imports", "agent-artifacts", "claude", "sibling.md"), "sibling import")
	mustWrite(t, filepath.Join(home, ".codex", "AGENTS.md"), "user codex")

	report, err := Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"provider_artifacts_detected", "review_stages_pending", "prune_ready"} {
		if hasFinding(report, code) {
			t.Fatalf("doctor should ignore out-of-root artifacts, found %s in %#v", code, report.Findings)
		}
	}
	for _, finding := range report.Findings {
		if filepath.Clean(finding.Root) == filepath.Clean(parent) || filepath.Clean(finding.Root) == filepath.Clean(sibling) || filepath.Clean(finding.Root) == filepath.Clean(home) {
			t.Fatalf("doctor finding escaped selected root: %#v", finding)
		}
		if strings.Contains(finding.Message, "parent claude") || strings.Contains(finding.Message, "sibling import") || strings.Contains(finding.Message, "user codex") {
			t.Fatalf("doctor finding mentioned out-of-root content: %#v", finding)
		}
	}
}

func TestRunReportsBackupAndPruneState(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(root, "CLAUDE.md")
	mustWrite(t, native, "claude guidance")
	sourceBody, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	targetRel := filepath.ToSlash(filepath.Join(".agents", "prune-fixture.md"))
	targetBody := []byte("canonical fixture\n")
	sourceHash := sha256.Sum256(sourceBody)
	targetHash := sha256.Sum256(targetBody)
	store, err := backup.NewStore(backup.StoreOptions{
		ProjectRoot: root, HomeDir: home, Operation: backup.PendingCommitOperation, RunID: "commit-doctor-fixture", Now: func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadManifestGeneration(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := store.ConfigurePendingCommitGeneration(
		"doctor-fixture", "CLAUDE.md", hex.EncodeToString(sourceHash[:]), targetRel, hex.EncodeToString(targetHash[:]), int64(len(targetBody)),
	); err != nil {
		t.Fatal(err)
	}
	store = publishPendingFixture(t, store, readPendingFixtureSource(t, native), pendingFixtureSourceMode(t, native), nil, os.FileMode(0), false)

	mustWrite(t, filepath.Join(root, filepath.FromSlash(targetRel)), string(targetBody))
	if err := store.FinalizePendingCommit(); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Workflow.Complete || !hasFinding(report, "backup_review_due") || hasFinding(report, "prune_warning") {
		t.Fatalf("old backup incorrectly affects inspection: %+v", report)
	}
	if report.Summary.BackupRuns != 1 || report.Summary.LatestBackupRun != store.RunID {
		t.Fatalf("expected one backup run, got %#v", report.Summary)
	}
	if report.Summary.PruneReady != 1 {
		t.Fatalf("expected one ready prune candidate, got %#v", report.Summary)
	}
	if !hasSuggestedCommand(report, "threadpoint prune --root") {
		t.Fatalf("expected prune suggestion, got %#v", report.SuggestedCommands)
	}
}

func TestDoctorRejectsInvalidRootsAndRegistries(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	registry := provider.BuiltinRegistry()
	for _, opts := range []Options{
		{Root: root, HomeDir: home, Registry: registry, Providers: []provider.ID{"missing"}},
		{Root: root, HomeDir: home, Registry: registry, ExcludeProviders: []provider.ID{"missing"}},
		{Root: root, HomeDir: home, SkipDirNames: []string{"nested/path"}},
	} {
		if _, err := Run(context.Background(), opts); err == nil {
			t.Fatalf("invalid doctor options were accepted: %#v", opts)
		}
	}
}

func TestDoctorReportsInvalidHomeAndCanceledChecks(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	registry := provider.BuiltinRegistry()

	builder := reportBuilder{
		report: &Report{Root: root, OK: true},
		seen:   map[string]bool{},
		cmds:   map[string]bool{},
		assets: map[string]map[string]bool{},
	}
	invalidHome := Options{Root: root, HomeDir: home, ThreadpointHome: "relative"}
	builder.addStages(context.Background(), invalidHome)
	builder.addBackups(invalidHome)
	if !hasFinding(builder.report, "stage_list_failed") || !hasFinding(builder.report, "backup_list_failed") {
		t.Fatalf("home-boundary failures were not reported: %#v", builder.report.Findings)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	builder.addDiscovery(canceled, Options{Root: root, HomeDir: home, Registry: registry})
	builder.addPruning(canceled, Options{Root: root, HomeDir: home})
	if !hasFinding(builder.report, "discovery_failed") || !hasFinding(builder.report, "prune_plan_failed") {
		t.Fatalf("canceled diagnostics were not reported: %#v", builder.report.Findings)
	}
}

func TestRunInventoryIncludesBothCanonicalScopesWithoutMutation(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".agents/knowledge/catalog.json", ".agents.local/knowledge/local.md", "AGENTS.local.md"} {
		mustWrite(t, filepath.Join(root, rel), "private body must stay out of report")
	}
	report, err := Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil || !report.OK || !report.Inventory.Complete || !report.Validation.Complete || !report.Workflow.Complete {
		t.Fatalf("%+v %v", report, err)
	}
	text := FormatText(report)
	for _, rel := range []string{".agents/knowledge/catalog.json", ".agents.local/knowledge/local.md", "AGENTS.local.md"} {
		if !strings.Contains(text, rel) {
			t.Errorf("text inventory missing %s: %s", rel, text)
		}
	}
	if strings.Contains(text, "private body must stay out of report") {
		t.Fatal("inventory exposed file body")
	}
	if _, err := os.Stat(filepath.Join(home, ".threadpoint")); !os.IsNotExist(err) {
		t.Fatal("status created state")
	}
}

func TestRunInventoryRemainsCompleteWhenSharedDiscoveryExcluded(t *testing.T) {
	root := t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	const canonical = ".agents/knowledge/catalog.md"
	mustWrite(t, filepath.Join(root, canonical), "canonical knowledge")
	report, err := Run(context.Background(), Options{
		Root: root, HomeDir: t.TempDir(), Providers: []provider.ID{provider.Claude},
		ProviderSelectionExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Inventory.Complete || !strings.Contains(FormatText(report), canonical) {
		t.Fatalf("provider selection excluded canonical inventory: %+v", report.Inventory)
	}
	for _, source := range report.Workflow.Sources {
		if source.Provider == provider.Shared {
			t.Fatalf("shared source bypassed discovery filter: %+v", source)
		}
	}
}

func TestRunInventoryUninspectableIsNotEmptySuccess(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	if err := layout.EnsureShared(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".agents.local")); err != nil {
		t.Skip(err)
	}
	report, err := Run(context.Background(), Options{Root: root, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if report.Inventory.Complete || !hasFinding(report, "inventory_incomplete") {
		t.Fatalf("unsafe scope reported complete: %+v", report.Inventory)
	}
}
