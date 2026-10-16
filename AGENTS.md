# Threadpoint Agent Guide

## Overview

Threadpoint is the public Apache-2.0-licensed Go library and CLI for mapping
native agent memory, instructions, and skills under a selected project root into
a shared, provider-neutral knowledgebase, and for a scope-isolated project
`stage`/`commit` lifecycle on shared and ignored local surfaces.

## Commands

Run these commands from the repository root. Project commands discover their
root automatically; `--root` is an optional command flag. See
[root selection](docs/environment-and-state-policy.md#selected-root).

```bash
go test ./...

# Map and inspect the selected project:
go run ./cmd/threadpoint status
go run ./cmd/threadpoint init
go run ./cmd/threadpoint doctor

# Project scope-isolated review lifecycle:
go run ./cmd/threadpoint stage --plan
go run ./cmd/threadpoint stage --apply --yes
go run ./cmd/threadpoint commit <stage-id> --apply --yes
go run ./cmd/threadpoint prune
go run ./cmd/threadpoint restore

# Distribution:
go run ./cmd/threadpoint version
go run ./cmd/threadpoint update check
```

## Boundaries

- Keep this repository provider-neutral and open source.
- Preserve source metadata when mapping provider-native files into shared records.
- In prose, prefer lowercase `threadpoint` unless it begins a sentence, appears
  in a heading or title, or is part of code, legal text, or an identifier.

## Go Test Organization

- Group tests by the behavior or operation they verify. Keep successful cases,
  validation failures, and regressions together; do not create review-round,
  finding-number, or coverage-follow-up test files.
- Keep CLI `main_test.go` focused on dispatch and help. Put command, installer,
  and state behavior in their corresponding test files.
- Split independent scenarios into focused tests. Use named table cases for
  variations of one behavior, and keep transaction/recovery sequences intact.
- Keep single-use fixtures beside their tests. Put fixtures shared across files
  in package-local `helpers_test.go` or `<topic>_helpers_test.go`.
- Platform-specific tests and helpers must end in `_test.go`, for example
  `<behavior>_posix_test.go`, and carry explicit build constraints. Preserve
  subprocess isolation for umask changes and cleanup for mutable hooks.

## Source Comments and Headers

- Maintained Go production and test files carry the single-line
  `// SPDX-License-Identifier: Apache-2.0` declaration, referring to `LICENSE`.
  Preserve existing attribution; do not add full license or copyright banners.
  Scripts and other formats do not require blanket SPDX headers.
- Give each production package one useful introduction, attached directly to
  its package declaration. Begin command introductions with the command name.
  Keep exported API documentation beside its declarations.
- Add short file introductions only for non-obvious responsibilities,
  boundaries, or execution context. Document operational script invocation,
  prerequisites, and significant side effects where they aid safe use.
- Omit author/date histories, filename banners, decorative separators, and
  comments that merely restate names. Git records change history.
- Preserve shebangs, build constraints, lint directives, generated-file
  notices, and third-party attribution. Leave generated files and
  external-content fixtures out of header cleanup.
- Use existing formatting, documentation, and test checks; no new header
  checker is required.

## Contracts

- Feature catalog: `.agents/knowledge/feature-catalog.json`
- Requirements manifest: `.agents/knowledge/requirements-manifest.json`
- Repository rules: `.agents/rules/threadpoint.md`

## Skill Layout

Keep portable skill definitions under `.agents/skills/<skill-name>/`. Use
`scripts/` for executable helpers; do not create skill-local `bin/`
directories. Store mutable skill data only at
`state/skills/<skill-name>/`, which is project-local and entirely ignored by
Git. Do not place runtime state inside a skill definition.
