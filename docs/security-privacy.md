# Security, Privacy, and Trust

Threadpoint is local-first. It works below one selected project root and keeps
review copies, backups, locks, and recovery records under its private home.

## Content handling

Stage reads only bounded regular files declared by the provider registry. Commit
writes one scope-fixed canonical target. Prune requires the current native source
to equal the reviewed source snapshot recorded by its latest commit. Restore
accepts only the current backup manifest format and validates selected payloads
and targets before writing.

Text `threadpoint status` and `threadpoint doctor` reports redact their selected
project and threadpoint-home paths so they are safer to copy into support
requests. This is best-effort presentation redaction, not a data transform.
JSON status and doctor reports, other operational reports, and previews can
still contain local paths; treat those outputs as sensitive before sharing them.

Threadpoint does not persist ambient local diagnostic logs. Its state writes do not include local CLI diagnostic logs.

## Filesystem safety

The selected root and mutation targets are pinned. Symlink escapes, unsafe file
types, unexpected lock directories, oversized inputs, and concurrent target
replacement cause the operation to stop. Commit, prune, and restore create
backups before mutation, use no-replace filesystem operations where available,
and retain recovery paths if cleanup cannot be proved safe.

New canonical files and missing parent directories honor the process umask.
Existing regular canonical files retain their permission bits when replaced.
Private state, backup, lock, and recovery containers use explicit private modes.

## Network and execution

Ordinary project commands do not transmit project content or emit telemetry.
Release installation and update may use the network as documented in
network-egress-and-no-telemetry.md. Stage edit executes
only the editor explicitly selected through VISUAL or EDITOR. Interactive stage
difftool and mergetool invoke only the explicitly configured DIFF or MERGE
programs with private review bytes; `stage diff` prints those bytes locally and
warns before doing so.

Report suspected vulnerabilities through SECURITY.md, not a public issue.
