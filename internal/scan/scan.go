// SPDX-License-Identifier: Apache-2.0

// Package scan scans the shared and local canonical surfaces of a workspace and maps them
// into provider-neutral knowledgebase records.
package scan

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/threadgrid/threadpoint/knowledgebase"
	"github.com/threadgrid/threadpoint/provider"
	"github.com/threadgrid/threadpoint/skill"
	"github.com/threadgrid/threadpoint/walk"
)

// Options selects the shared-layout root and directories excluded from scans.
type Options struct {
	Root         string
	SkipDirNames []string
}

// Run maps the shared and local layout beneath opts.Root into a sorted knowledgebase.
func Run(ctx context.Context, opts Options) (*knowledgebase.Catalog, error) {
	root := opts.Root
	if root == "" {
		root = "."
	}

	kb := &knowledgebase.Catalog{Root: root}
	for _, bridge := range provider.DetectBridges(root) {
		kb.Add(knowledgebase.Record{
			ID:    "bridge:" + string(bridge.ID) + ":" + filepath.ToSlash(bridge.Path),
			Kind:  knowledgebase.RecordBridge,
			Title: string(bridge.ID) + " bridge",
			Sources: []knowledgebase.Source{{
				Provider: string(bridge.ID),
				Path:     filepath.ToSlash(bridge.Path),
				Native:   bridge.Native,
			}},
		})
	}

	if info, err := os.Lstat(filepath.Join(root, "AGENTS.local.md")); err == nil {
		if info.Mode().IsRegular() {
			kb.Add(fileRecord("AGENTS.local.md", knowledgebase.RecordBridge))
		} else {
			kb.Warnings = append(kb.Warnings, "AGENTS.local.md is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		kb.Warnings = append(kb.Warnings, "cannot inspect AGENTS.local.md: "+err.Error())
	}
	for _, surface := range []string{".agents", ".agents.local"} {
		info, err := os.Lstat(filepath.Join(root, surface))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			kb.Warnings = append(kb.Warnings, "cannot inspect "+surface+": "+err.Error())
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			kb.Warnings = append(kb.Warnings, surface+" is not a regular directory")
			continue
		}
		warnings, err := walk.Dir(ctx, walk.Options{
			Root:         root,
			Start:        filepath.Join(root, surface),
			SkipDirNames: opts.SkipDirNames,
		}, func(path string, entry fs.DirEntry, rel string) error {
			if entry.IsDir() {
				return nil
			}

			switch {
			case strings.HasSuffix(rel, "/SKILL.md"):
				record := fileRecord(rel, knowledgebase.RecordSkill)
				if body, warning, ok, err := walk.ReadSmallTextFile(path, walk.DefaultMaxFileSize); err != nil {
					return err
				} else if !ok {
					if warning != "" {
						kb.Warnings = append(kb.Warnings, strings.Replace(warning, path, rel, 1))
					}
				} else if meta, err := skill.ParseFrontmatter(body); err == nil {
					record.Title = meta.Name
					record.Summary = meta.Description
				}
				kb.Add(record)
			case strings.HasPrefix(rel, surface+"/knowledge/"):
				kb.Add(fileRecord(rel, knowledgebase.RecordKnowledge))
			case strings.HasPrefix(rel, surface+"/rules/"):
				kb.Add(fileRecord(rel, knowledgebase.RecordRule))
			case strings.HasPrefix(rel, surface+"/adapters/"):
				kb.Add(fileRecord(rel, knowledgebase.RecordConfig))
			case strings.HasPrefix(rel, surface+"/plugins/"):
				kb.Add(fileRecord(rel, knowledgebase.RecordPlugin))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		kb.Warnings = append(kb.Warnings, warnings...)
	}

	kb.Sort()
	return kb, nil
}

func fileRecord(rel string, kind knowledgebase.RecordKind) knowledgebase.Record {
	title := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	return knowledgebase.Record{
		ID:    string(kind) + ":" + rel,
		Kind:  kind,
		Title: title,
		Sources: []knowledgebase.Source{{
			Provider: string(provider.Shared),
			Path:     rel,
			Native:   false,
		}},
		Paths: []string{rel},
	}
}
