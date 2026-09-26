# Support Boundary

The initial threadpoint release is a local, provider-neutral tool for a single selected project
root. It supports the registry's project-native artifacts for Codex, Claude,
Copilot, Antigravity, Kiro, OpenCode, Cursor, OpenClaw, and shared `AGENTS`
paths.

Supported operating systems are Linux and macOS on amd64 and arm64. WSL runs the Linux binary as a Linux runtime.

The initial-release workflow is `init`, `stage`, `commit`, `prune`, `restore`,
and `status`. Native sources remain in place until an explicit
prune succeeds after their reviewed source snapshot is committed.

Threadpoint excludes machine-wide provider configuration, home-directory
artifact discovery, Windows-native provider-home discovery, and any root other than the selected project. Provider
entrypoints and native file formats remain provider-owned technical surfaces.

## Requesting Additional Support

Request support for a new agent/provider or an additional project-native
artifact through the repository's feature request form. Include public vendor
documentation and synthetic, project-relative artifact details only; do not
post private project content, absolute local paths, or sensitive data. Requests
must fit the selected-root, local-first, provider-neutral boundary and do not
promise support.
