# Public Launch Surface Verification

Public launch verification combines the repository public-boundary sweep with
an operator's release-candidate review. It does not freeze README or policy
prose through a dedicated grep script.

## Repository Check

Run the public-boundary sweep from the threadpoint repository root:

```bash
scripts/check-public-repo-sweep.sh
```

It blocks private-parent, commerce, access-control, task, secret, and private-path
references from the public repository. Documentation links are verified
separately by `scripts/check-docs-links.sh`.

## Release-Candidate Review

After a public release-candidate tag exists, an operator confirms unauthenticated
repository access, raw installer access, release archives and checksums, and the
published Go module. For example:

```bash
curl -fsS https://github.com/threadgrid/threadpoint >/dev/null
curl -fsS https://raw.githubusercontent.com/threadgrid/threadpoint/main/scripts/install.sh >/dev/null
go install github.com/threadgrid/threadpoint/cmd/threadpoint@v0.1.0-rc.1
```

## Release Use

Run the repository check before public release preparation. Perform the manual
release-candidate review after the candidate tag and assets are public, before
announcing a release. External visibility is intentionally not part of ordinary
CI because unreleased tags, private repositories, or draft releases would fail
by design.
