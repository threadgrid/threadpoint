# Contributing to threadpoint

Threadpoint is Apache-2.0-licensed, provider-neutral, and local-first. By
contributing, you license your contribution under the Apache License, Version 2.0.
Every commit must carry a Developer Certificate of Origin sign-off:

    git commit -s

Follow the [source comments and headers policy](#source-comments-and-headers)
for SPDX declarations, package documentation, and selective file introductions.

Run focused checks from the repository root before opening a pull request.
Project commands discover their root automatically; `--root` is an optional
override:

    make install
    make format-check
    make docs-links
    make test
    go run ./cmd/threadpoint status
    scripts/check-public-repo-sweep.sh

Installer changes also require `sh scripts/test-installer-archive-safety.sh`.
This suite requires Bash, Zsh, Fish, Python 3, and Go, and fails if a shell is
missing. Install Fish and Zsh with your OS package manager (on Debian/Ubuntu:
`sudo apt-get install fish zsh`). Use `TMPDIR` to select a larger scratch
directory when the default temporary filesystem is small.

Preview the public Go package documentation locally with:

    make docs-go

Maintainers can run the same SonarQube Cloud analysis used by CI with a full,
clean clone at a pushed commit:

    export SONAR_ORGANIZATION=<organization-key>
    read -rsp 'Sonar token: ' SONAR_TOKEN && export SONAR_TOKEN
    make sonar
    unset SONAR_TOKEN

On a non-main branch, also pass `PR_NUMBER=<number>`; `PR_BASE` defaults to
`main`. This explicit command uploads public source and coverage to the
configured quality project. Ordinary threadpoint commands and tests remain
local and do not invoke the service.

Apply the same source, documentation, configuration, and fixture formatting
locally with:

    make format

`make install` provides the pinned Prettier dependency. Install the remaining
native formatters before using the Make targets:

    cargo install --locked taplo-cli --version 0.10.0
    go install mvdan.cc/sh/v3/cmd/shfmt@v3.13.1
    python3 -m pip install --user ruff==0.16.1

Keep changes bounded to one selected root, preserve provider neutrality, and add
tests plus public documentation when a command contract changes. Do not add
runtime external-service dependencies, private dependencies, provider-home
discovery, or generated credentials to the repository.

Public issues and pull requests must not contain secrets, private paths, backup
payloads, or proprietary guidance. Follow SECURITY.md for security reports.

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

## Scanner Toolchain Upgrades

The literal `FROM` references in `.sonar/Dockerfile` are the source of truth for
the local scanner and Go releases. A digest-only Dependabot update needs no
mirror changes. When an image tag changes, update the matching local image tag
in `Makefile` and `scripts/run-sonar.sh`; for a scanner release, also update the
full `scannerVersion` build in `.github/workflows/ci.yml`. CI derives the Go
patch release directly from the Dockerfile, so a Go tag update needs no workflow
version edit.

Verify the mirrors with:

    scripts/check-sonar-configuration.sh

Then build the local image and complete one analysis before merging a toolchain
release update.

## Public Issue Intake

Use the [bug report form](https://github.com/threadgrid/threadpoint/issues/new/choose)
for reproducible public defects. Include the installed version, platform,
workflow, and a synthetic or redacted reproduction.

Use the [feature request form](https://github.com/threadgrid/threadpoint/issues/new/choose)
for workflow improvements, including support for a new agent/provider or an
additional project-native artifact. For support requests, provide public vendor
documentation and only synthetic, project-relative artifact details. A request
does not promise that the agent or artifact will be supported.

Report vulnerabilities through [SECURITY.md](SECURITY.md), not a public issue.
