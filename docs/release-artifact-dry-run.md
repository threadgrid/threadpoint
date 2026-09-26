# Release Artifact Dry Run

The release artifact dry run verifies that GoReleaser can build the documented
initial-release distribution surface without publishing a GitHub Release.

## Expected Initial Release Artifacts

The initial-release dry run must produce exactly these archives under `dist/`:

- `threadpoint_0.1.0_linux_amd64.tar.gz`
- `threadpoint_0.1.0_linux_arm64.tar.gz`
- `threadpoint_0.1.0_darwin_amd64.tar.gz`
- `threadpoint_0.1.0_darwin_arm64.tar.gz`

The same run must also produce `dist/checksums.txt`, and the checksum file must
cover every expected archive.

Each archive must contain exactly these entries:

- `<archive-stem>/bin/threadpoint`
- `<archive-stem>/README.md`
- `<archive-stem>/LICENSE`
- `<archive-stem>/NOTICE`
- `<archive-stem>/scripts/install.sh`
- `<archive-stem>/scripts/uninstall.sh`

No generated reports, changelog files, cache output, temporary files, or other
repository content should be bundled into initial-release archives. Windows archives are
intentionally absent from the initial-release matrix.

## Local Dry Run

From the threadpoint repository root:

```bash
goreleaser release --snapshot --clean --skip=publish
THREADPOINT_SMOKE_SKIP_SIGNATURE=1 scripts/smoke-release-artifacts.sh
```

The smoke script enforces the archive matrix, checksum coverage, exact archive
contents, and release-footer install guidance. It also extracts and runs the
archive matching the current host platform.

## GitHub Actions Dry Run

Use the `Release candidate` workflow's manual `workflow_dispatch` dry run before
tagging a release candidate. The dry-run job:

1. checks out the requested ref;
2. runs dependency license checks;
3. builds snapshot artifacts through GoReleaser;
4. runs `scripts/smoke-release-artifacts.sh`;
5. uploads archives, `checksums.txt`, and smoke logs as workflow artifacts.

Tag-triggered candidates perform the same no-publish build and smoke check,
then attest the archives in tag-push context before upload. The trusted
publisher consumes those exact workflow artifacts, verifies their tag-context
provenance, and never rebuilds them before signing, re-smoke, checksum-manifest
attestation, and draft publication.

## Troubleshooting

- Missing archives usually mean `.goreleaser.yml` drifted from the supported
  platform matrix.
- Checksum failures mean the archive and `checksums.txt` are not from the same
  GoReleaser run.
- Missing or extra archive entries mean the archive contents no longer match the
  documented install and license surface.
- A missing Go install path or checksum wording in `.goreleaser.yml` means
  release notes no longer match public install guidance.

Release-candidate command smoke coverage is documented in
[`release-candidate-smoke-tests.md`](release-candidate-smoke-tests.md).
