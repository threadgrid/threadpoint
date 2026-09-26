// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/discover"
	"github.com/threadgrid/threadpoint/knowledgebase"
	"github.com/threadgrid/threadpoint/layout"
	"github.com/threadgrid/threadpoint/prune"
	"github.com/threadgrid/threadpoint/scan"
	"github.com/threadgrid/threadpoint/stage"
)

// InventorySection retains the records from the canonical scanner, including
// partial results when its bounded traversal cannot inspect every path.
type InventorySection struct {
	Complete bool                   `json:"complete"`
	Records  []knowledgebase.Record `json:"records"`
	Warnings []string               `json:"warnings,omitempty"`
}

// ValidationSection reports structural validity separately from workflow health.
type ValidationSection struct {
	Complete bool `json:"complete"`
	layout.Report
}

// WorkflowSection describes observed work; pending work is not itself an error.
type WorkflowSection struct {
	Complete bool                `json:"complete"`
	Summary  Summary             `json:"summary"`
	Sources  []discover.Artifact `json:"sources"`
	Stages   []StageObservation  `json:"stages"`
	Prune    []prune.Candidate   `json:"prune"`
	Backups  []BackupObservation `json:"backups"`
	Git      []GitReport         `json:"git"`
}

// StageObservation binds a saved review copy to its current source assessment.
// Embedders supply only selected-project observations to Options.StageReader.
type StageObservation struct {
	ID          string `json:"id"`
	Scope       string `json:"scope"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	SourceState string `json:"source_state"`
	Edited      bool   `json:"edited"`
	Code        string `json:"code,omitempty"`
	Message     string `json:"message,omitempty"`
}

// BackupObservation contains metadata only, never backup payloads.
type BackupObservation struct {
	ID        string    `json:"id"`
	Operation string    `json:"operation"`
	CreatedAt time.Time `json:"created_at"`
}

func (b *reportBuilder) addInventory(ctx context.Context, opts Options) {
	catalog, err := scan.Run(ctx, scan.Options{Root: opts.Root, SkipDirNames: opts.SkipDirNames})
	if err != nil {
		b.addFinding(SeverityError, "inventory_failed", opts.Root, "", fmt.Sprintf("failed inspecting canonical memory: %v", err))
		return
	}
	b.report.Inventory = InventorySection{Complete: len(catalog.Warnings) == 0, Records: catalog.Records, Warnings: catalog.Warnings}
	for _, warning := range catalog.Warnings {
		b.addFinding(SeverityWarning, "inventory_incomplete", opts.Root, "", warning)
	}
}

func inspectProjectStages(ctx context.Context, opts Options) ([]StageObservation, error) {
	return inspectProjectStagesWithSourceReader(ctx, opts, stage.ReadCurrentSource)
}

func inspectProjectStagesWithSourceReader(ctx context.Context, opts Options, readSource func(string, string, string) ([]byte, error)) ([]StageObservation, error) {
	home, err := backup.ResolveThreadpointHomeWithOverride(opts.HomeDir, opts.ThreadpointHome)
	if err != nil {
		return nil, err
	}
	records, err := stage.List(opts.Root, home)
	if err != nil {
		return nil, err
	}
	observations := make([]StageObservation, 0, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var sourceDigest string
		item := StageObservation{ID: record.ID, Scope: string(record.Scope), Source: record.Source, Target: record.Target, SourceState: "unavailable"}
		currentRecord, source, content, err := stage.ReadReview(opts.Root, home, record.ID)
		if err == nil && (currentRecord != record || digestBytes(source) != record.SourceSHA256 || digestBytes(content) != record.ContentSHA256) {
			err = errors.New("stage generation changed or its payload digest is invalid")
		}
		if err != nil {
			item.Code, item.Message = "stage_source_unavailable", fmt.Sprintf("cannot inspect stage %s: %v", record.ID, err)
		} else {
			item.Edited = digestBytes(content) != digestBytes(source)
			current, readErr := readSource(opts.Root, home, record.ID)
			sourceDigest = digestBytes(current)
			switch {
			case errors.Is(readErr, os.ErrNotExist):
				item.SourceState, item.Code, item.Message = "missing", "stage_source_missing", "staged source no longer exists"
			case readErr != nil:
				item.Code, item.Message = "stage_source_unavailable", fmt.Sprintf("cannot inspect staged source: %v", readErr)
			case digestBytes(current) != record.SourceSHA256:
				item.SourceState, item.Code, item.Message = "changed", "stage_source_changed", "source changed since staging; review or refresh the saved stage"
			default:
				item.SourceState = "unchanged"
			}
		}
		if item.SourceState != "unavailable" {
			latest, source, content, err := stage.ReadReview(opts.Root, home, record.ID)
			if err != nil || latest != record || digestBytes(source) != record.SourceSHA256 || digestBytes(content) != record.ContentSHA256 {
				item.SourceState, item.Code, item.Message = "unavailable", "stage_source_unavailable", "stage changed during inspection"
			}
		}

		if item.SourceState != "unavailable" {
			current, err := readSource(opts.Root, home, record.ID)
			same := (item.SourceState == "missing" && errors.Is(err, os.ErrNotExist)) || (item.SourceState != "missing" && err == nil && digestBytes(current) == sourceDigest)
			if !same {
				item.SourceState, item.Code, item.Message = "unavailable", "stage_source_unavailable", "source changed during inspection; run status again"
			}
		}
		observations = append(observations, item)
	}
	return observations, nil
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func formatInventory(section InventorySection) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Inventory: %d records (complete: %v)\n", len(section.Records), section.Complete)
	for _, scope := range []string{"project-shared", "project-local"} {
		fmt.Fprintf(&out, "  %s:\n", scope)
		for _, record := range section.Records {
			path := record.ID
			if len(record.Paths) > 0 {
				path = record.Paths[0]
			} else if len(record.Sources) > 0 {
				path = record.Sources[0].Path
			}
			local := layout.IsProjectLocalPath(path)
			if local != (scope == "project-local") {
				continue
			}
			fmt.Fprintf(&out, "    %s %s (%s)\n", record.Kind, record.ID, path)
		}
	}
	return out.String()
}
