// SPDX-License-Identifier: Apache-2.0

// Package scan provides the public workspace scan API for consumers that need
// provider-neutral knowledgebase records.
package scan

import (
	"context"

	internalscan "github.com/threadgrid/threadpoint/internal/scan"
	"github.com/threadgrid/threadpoint/knowledgebase"
)

// Options configures a workspace scan.
type Options struct {
	Root         string
	SkipDirNames []string
}

// Run maps the shared .agents and project-local .agents.local surfaces of a workspace into knowledgebase records.
func Run(ctx context.Context, opts Options) (*knowledgebase.Catalog, error) {
	return internalscan.Run(ctx, internalscan.Options{Root: opts.Root, SkipDirNames: opts.SkipDirNames})
}
