# Threadpoint

[![CI](https://github.com/threadgrid/threadpoint/actions/workflows/ci.yml/badge.svg)](https://github.com/threadgrid/threadpoint/actions/workflows/ci.yml)
[![Release](https://github.com/threadgrid/threadpoint/actions/workflows/release-publish.yml/badge.svg)](https://github.com/threadgrid/threadpoint/actions/workflows/release-publish.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/threadgrid/threadpoint.svg)](https://pkg.go.dev/github.com/threadgrid/threadpoint)

Threadpoint keeps one selected project's agent guidance in a shared,
provider-neutral layout. It discovers supported provider-native artifacts,
stages private review copies, and commits one reviewed item at a time to the
canonical agent layout.

Run these commands inside your project. The optional `--root` flag overrides
[project discovery](docs/environment-and-state-policy.md#selected-root).
An unmarked subdirectory may select an ancestor project. To initialize the
current directory as a separate project, use `threadpoint init --root . --yes`.
Init reports the selected root on success.

    threadpoint init --yes
    threadpoint stage --plan
    threadpoint stage --apply --yes
    threadpoint stage list
    threadpoint stage diff STAGE_ID
    threadpoint stage edit STAGE_ID
    threadpoint stage difftool STAGE_ID
    threadpoint stage mergetool STAGE_ID
    threadpoint commit STAGE_ID --apply --yes
    threadpoint prune --plan

A commit records the exact source snapshot it reviewed. Prune removes a native
artifact only when it still matches that snapshot.

Threadpoint is local-first, sends no telemetry during ordinary project commands,
and is licensed under Apache-2.0.

Install with Go or the release installer:

    go install github.com/threadgrid/threadpoint/cmd/threadpoint@latest
    curl -fsSL https://raw.githubusercontent.com/threadgrid/threadpoint/main/scripts/install.sh | sh

After installing, follow [Add to PATH](docs/install.md#add-to-path) for Bash,
Zsh, or Fish, then run `threadpoint version` to verify the command is available.

## Documentation

- Product docs: docs/README.md
- Go API and compatibility: docs/go-api.md
- Canonical layout: docs/canonical-layout-guide.md
- CLI reference: docs/cli-reference.md
- Workflows: docs/workflows.md
- Security and privacy: docs/security-privacy.md
- Install: docs/install.md
- Contributing: CONTRIBUTING.md
- Report a bug or request a feature: https://github.com/threadgrid/threadpoint/issues/new/choose
- Report a vulnerability privately: SECURITY.md
