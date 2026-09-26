# Go API

Threadpoint is both a command-line tool and an importable Go module. Reference
documentation for published versions is available on
[pkg.go.dev](https://pkg.go.dev/github.com/threadgrid/threadpoint). Run
`make docs-go` from the repository root to preview the current source locally.

## Package Guide

The top-level packages are the supported public API:

- Provider model and discovery: `provider`, `discover`, `knowledgebase`,
  `layout`, `scan`, and `skill`.
- Project workflows: `backup`, `doctor`, `prune`, `restore`, `review`,
  and `stage`.
- Embedding and safety: `clierror`, `diagnostic`, `project`, `redact`,
  `safefs`, `safepath`, and `walk`.

Packages below `internal/` are implementation details. Packages below `cmd/`
define executable programs and are not libraries.

## Compatibility Before v1

Top-level packages are supported public APIs, but threadpoint is still pre-1.0.
Their exported Go symbols and JSON-backed structs may change between minor
releases before v1. Breaking changes and upgrade notes are recorded in
[`CHANGELOG.md`](../CHANGELOG.md).

Do not depend on `internal/` implementation details or import `cmd/` packages.
No compatibility is promised for either surface. A v1 release will define the
stable compatibility boundary separately.

## Explicit I/O And Backup Publication

Review tools require `review.Streams` with explicit stdin, stdout, and stderr.
`review.Edit`, `review.Diff`, and `review.Merge` pass those streams to the
configured subprocess; they never select process streams for an embedder.

Backup manifests use schema version 1 for the initial release. Readers reject
other versions and incomplete records without rewriting them. After constructing
an ordinary backup store, call `LoadManifestGeneration` (or its rooted variant)
and handle either a validated existing manifest or `os.ErrNotExist` before
mutating entries. Loading is read-only. Subsequent saves replace only the exact
reviewed generation; an unreviewed store cannot publish.

Create commit evidence with `PublishPendingCommitGenerationFromRoot`, which
publishes the source and prior canonical generation together. Entry APIs cannot
incrementally assemble or modify commit evidence. Exact recovery and retry
transitions remain supported.

## Examples

Runnable examples are attached to the package documentation for:

- constructing and sorting a `knowledgebase.Catalog`;
- validating the canonical shared layout through `layout.Validate`;
- scanning a workspace through `scan.Run`; and
- parsing portable skill frontmatter through `skill.ParseFrontmatter`.

The examples are compiled and executed by the normal Go test suite.
