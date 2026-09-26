# Release Candidate Smoke Tests

Release-candidate smoke tests verify packaged threadpoint archives, not only
`go test` or `go run` behavior.

The artifact matrix and no-publish dry-run contract are documented in
[`release-artifact-dry-run.md`](release-artifact-dry-run.md).

## What The Smoke Test Checks

`scripts/smoke-release-artifacts.sh` expects GoReleaser output under `dist/`.
It verifies:

- each `threadpoint_*_*.tar.gz` archive has a matching `checksums.txt` entry;
- each archive checksum matches the recorded digest;
- each archive wraps its payload in a top-level directory named after the
  archive stem and contains exactly `bin/threadpoint`, `README.md`, `LICENSE`,
  `NOTICE`, `scripts/install.sh`, and `scripts/uninstall.sh` inside it;
- the archive matching the current runner `GOOS/GOARCH` can be extracted and
  executed.

For host-compatible binaries, the smoke test runs:

```bash
threadpoint --help
threadpoint version
threadpoint init --root <clean fixture> --yes
threadpoint status --root <initialized fixture>
threadpoint stage --root <tracked provider fixture> --plan
threadpoint doctor --offline
threadpoint restore list --root <provider fixture>
```

If `threadpoint --help` is not supported by a pre-release binary, the smoke test
falls back to `threadpoint help` so release validation still exercises help
output.

## Local Release-Candidate Flow

From the threadpoint repository root:

```bash
goreleaser release --snapshot --clean --skip=publish
THREADPOINT_SMOKE_SKIP_SIGNATURE=1 scripts/smoke-release-artifacts.sh
```

The smoke script writes command logs under `dist/smoke/`. It copies fixtures to
temporary directories before running mutating commands, so checked-in fixtures
stay clean.

## CI Release Flow

The release workflows run the smoke test in three places:

- manual dry-runs build snapshot artifacts and smoke them before upload;
- tag-triggered candidates build no-publish artifacts once, smoke them, and
  attest the archives in tag context without signing or release-write authority;
- the trusted default-branch publisher downloads and verifies those archive
  attestations against the exact tag commit, signs and re-smokes the same
  bytes, then creates a draft, attests the signed checksum manifest, and
  publishes it.

Smoke logs and candidate archives are uploaded as workflow artifacts for review.
Generated license reports, changelogs, caches, and temporary files are not part
of the initial-release archive payload unless a later release task deliberately changes the
archive contract.

## Platform Matrix

The initial threadpoint release archives are built for:

- `linux/amd64`
- `linux/arm64`
- `darwin/amd64`
- `darwin/arm64`

CI can inspect every archive on the Linux runner, but it only executes the
Linux host-compatible binary. macOS execution is covered when maintainers run
the local smoke flow on macOS or add a macOS smoke runner. Windows remains
deferred for the initial release and is not included in the release artifact matrix.
