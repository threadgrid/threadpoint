# Supply Chain Trust

The initial threadpoint release trust is based on official GitHub Releases, signed
checksums, and GitHub Artifact Attestations.

Release archives are built and attested by the public candidate workflow from
immutable semantic version tags. GoReleaser builds archives for the supported
platform matrix and writes a `checksums.txt` file for each release. A separate
protected default-branch publisher verifies those archive attestations and
signs `checksums.txt` into `checksums.txt.sig` with the threadpoint Ed25519
release signing key.

Checksums verify that a downloaded archive matches the digest published with
the same GitHub Release. The detached `checksums.txt.sig` envelope verifies the
checksum file. GitHub Artifact Attestations verify GitHub Actions provenance for
release archives and `checksums.txt`.

## User Verification

Users should download release artifacts only from the official
`github.com/threadgrid/threadpoint` GitHub Releases page.

Every release publishes `checksums.txt` and `checksums.txt.sig` next to the
supported release archives:

- `threadpoint_0.1.0_linux_amd64.tar.gz`
- `threadpoint_0.1.0_linux_arm64.tar.gz`
- `threadpoint_0.1.0_darwin_amd64.tar.gz`
- `threadpoint_0.1.0_darwin_arm64.tar.gz`

Download the archive for your platform, `checksums.txt`, and
`checksums.txt.sig` from the same GitHub Release before extracting or
installing the archive.

On Linux, verify the selected archive with `sha256sum`:

```bash
tag=v0.1.0
archive=threadpoint_0.1.0_linux_amd64.tar.gz
base="https://github.com/threadgrid/threadpoint/releases/download/$tag"

curl -fsSLO "$base/$archive"
curl -fsSLO "$base/checksums.txt"
curl -fsSLO "$base/checksums.txt.sig"
go run github.com/threadgrid/threadpoint/cmd/threadpoint-release-signature@$tag --verify --in checksums.txt --out checksums.txt.sig
awk -v file="$archive" '$2 == file { print; found = 1 } END { if (!found) exit 1 }' checksums.txt | sha256sum --check -
```

On macOS, compare the selected archive with the matching `checksums.txt` entry
using `shasum`:

```bash
tag=v0.1.0
archive=threadpoint_0.1.0_darwin_arm64.tar.gz
base="https://github.com/threadgrid/threadpoint/releases/download/$tag"

curl -fsSLO "$base/$archive"
curl -fsSLO "$base/checksums.txt"
curl -fsSLO "$base/checksums.txt.sig"
go run github.com/threadgrid/threadpoint/cmd/threadpoint-release-signature@$tag --verify --in checksums.txt --out checksums.txt.sig
expected="$(awk -v file="$archive" '$2 == file { print $1; found = 1 } END { if (!found) exit 1 }' checksums.txt)"
actual="$(shasum -a 256 "$archive" | awk '{ print $1 }')"
[ "$expected" = "$actual" ]
```

The checksum signature must verify before trusting `checksums.txt`. The expected
archive name and digest must match the corresponding line in `checksums.txt`.
If either verification command fails, do not extract or install the archive.

Verify the archive's GitHub Actions provenance with GitHub CLI:

```bash
gh attestation verify "$archive" \
  -R threadgrid/threadpoint \
  --signer-repo threadgrid/threadpoint \
  --signer-workflow threadgrid/threadpoint/.github/workflows/release.yml \
  --source-ref "refs/tags/$tag"
```

Verify the checksum file itself with the protected publisher identity. A
`workflow_run` publisher executes in default-branch context, so its truthful
source ref is `refs/heads/main`, not the candidate tag:

```bash
gh attestation verify checksums.txt \
  -R threadgrid/threadpoint \
  --signer-repo threadgrid/threadpoint \
  --signer-workflow threadgrid/threadpoint/.github/workflows/release-publish.yml \
  --source-ref refs/heads/main
```

Users who prefer not to download release archives can build from source through
the public module path:

```bash
go install github.com/threadgrid/threadpoint/cmd/threadpoint@v0.1.0
```

Replace `v0.1.0` with the release tag you intend to install.

## Release Retention

Threadpoint clips old release assets per channel. Stable releases and preview
releases are counted separately; a release is clipped only after it is at least
90 days old and has three newer releases in the same channel.

Clipping updates `release-policy.json` so official installers refuse clipped
versions through `blocked_versions`, and maintainers remove GitHub Release API
assets where possible. Tags, release pages, GitHub-generated source archives,
copied old binaries, and source builds from public tags can remain available.

## Provenance Status

The initial release workflows emit GitHub Artifact Attestations for public
release archives and checksum material. Archive verification constrains the
signer to `.github/workflows/release.yml` and the source ref to the release tag.
Checksum-manifest verification constrains the signer to
`.github/workflows/release-publish.yml` and the source ref to `refs/heads/main`.
Both constrain the repository to `threadgrid/threadpoint`.

Release candidates are built and archive-attested exactly once by a tag
workflow with no signing key or release-write authority. The publisher retains
the immutable default-branch workflow generation that
received the completion event, requires successful push-to-main CI for both the
tagged candidate and that controller generation, verifies the candidate is in
the controller generation's history, downloads those exact candidate bytes,
verifies their tag-context provenance and exact candidate commit, signs and
re-smokes them inside the protected release environment, and keeps the GitHub
Release in draft state until the signed checksum manifest is attested. The
final publish transition therefore exposes the same archives that were built,
smoked, attested, verified, and signed.

Cosign signatures and SBOM publication are not part of the initial-release policy unless a
later release note explicitly says they were added.

## Installer And Update Requirements

Installer and managed-update flows must:

- download artifacts only from official GitHub Releases;
- check the current `release-policy.json` before downloading a requested
  release through the official installer;
- fetch the release `checksums.txt` file from the same release;
- verify the selected archive checksum before extracting or replacing binaries;
- limit installer archive contents to a single top-level bundle containing the
  expected binary, bundled public README, license, notice files, and installer
  scripts;
- extract and install only the expected bundle files;
- fail closed when checksums are missing, malformed, or mismatched;
- avoid trusting mirrors, redirects to unofficial artifact hosts, or unsigned
  metadata as substitutes for the release checksum file.

The installer script must not silently fall back to unverified downloads.

## License Compliance

Dependency license review is part of release readiness. Maintainers should run
`scripts/check-licenses.sh` before release and follow
[`license-compliance.md`](license-compliance.md) when adding dependencies or
updating notices.

## Dependency Maintenance

Dependabot checks Go modules and GitHub Actions weekly. Maintainers should
review dependency update pull requests for release-tooling, CI, security, and
runtime impact before merging.

Security-sensitive updates to release tooling or artifact handling should be
validated with a GoReleaser check and a snapshot release before tagging.

## Deferred Hardening

The following controls are intentionally deferred for the initial release and should be planned
as follow-up hardening work:

- per-archive signatures beyond the signed checksum file;
- SBOM generation;
- native attestation verification without the GitHub CLI runtime dependency.

Signed checksums and attestation enforcement are mandatory for initial release
artifacts and installer-managed install/update flows. SBOMs and package-manager
trust channels are not initial-release installer blockers.

SBOM generation is a tracked, low-effort follow-up (GoReleaser's `sboms`/syft
integration) rather than an abandoned idea; it was deferred deliberately so initial-release trust rests on signed checksums plus published attestations. The same applies to
detached cosign signatures.
