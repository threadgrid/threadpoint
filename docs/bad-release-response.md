# Bad Release Response

This is the lightweight initial-release response procedure for a bad public threadpoint
release. It covers incorrect release notes, broken installers, checksum or
archive mistakes, unsafe artifacts, and accidentally bad public tags.

The full long-term release rollback, yank, and deprecation policy remains
deferred until v1.0.

## Principles

- Public release tags are immutable. Do not move, delete, or retag a published
  initial-release tag to correct a release.
- Prefer a corrected follow-up release over rewriting release history.
- Keep the original release record visible so users can understand what
  happened.
- Warn users plainly when a release is superseded, broken, or unsafe.
- Treat compromised or unsafe artifacts as security-sensitive incidents.
- Keep package-manager rollback details out of the initial release until package-manager
  channels exist.

## Normal Broken Release

Use this path when a release has a bad note, broken command, installer bug,
wrong archive contents, missing checksum entry, or other non-compromise issue.

1. Mark the GitHub Release as superseded in the release text.
2. Add a short warning near the top of the release notes that names the
   replacement release when one exists.
3. Add or update the relevant `CHANGELOG.md` entry with the superseded status
   and corrected guidance.
4. Create a corrected follow-up tag and release, such as `v0.1.1`.
5. Update `release-policy.json` when the official installer should stop
   installing the bad tag through `minimum_install_version` or
   `blocked_versions`.
6. Keep the original tag and release record published unless the attached
   artifacts are unsafe to download.

Do not replace binaries or archives in place for a public initial release. Users
should be able to rely on the release asset set that belonged to the tag they
downloaded.

## Incorrect Release Notes

If the release artifacts are correct but release text is wrong:

- edit the GitHub Release text to correct the statement;
- update `CHANGELOG.md` if the incorrect statement also appears there;
- leave the tag and artifacts unchanged;
- add a dated correction note when the mistake could affect install, update,
  installation, security, or support decisions.

## Broken Installer Or Archive

If the installer, archive layout, checksums, or documented install command is
wrong:

- warn users in the affected GitHub Release;
- document whether `go install` remains a safe workaround;
- publish a corrected follow-up release after a release dry run and packaged
  artifact smoke test pass;
- update `release-policy.json` so the official installer refuses the broken
  tag when users request it explicitly;
- update install, verification, or support docs if the failure exposed a docs
  gap.

Do not point the installer at an unofficial asset host or an unpublished
replacement archive.

## Unsafe Or Compromised Artifact

Use this path when an artifact may expose private data, include malicious or
compromised content, fail integrity expectations in a security-sensitive way,
or otherwise be unsafe to download or execute.

1. Add a prominent warning to the affected GitHub Release.
2. Remove unsafe downloadable assets where possible.
3. Preserve the public tag and release record for auditability.
4. Update `release-policy.json` so the official installer blocks the unsafe
   tag before download.
5. Follow [`../SECURITY.md`](../SECURITY.md) for private coordination and
   disclosure handling.
6. Publish a corrected follow-up release only after rebuilding from a trusted
   state and rerunning release verification.
7. Update `CHANGELOG.md`, install docs, and release verification docs with
   safe user guidance.

If removal is not possible, make the warning explicit that the release assets
must not be downloaded or executed.

## User Notification Surfaces

Use the smallest set of public surfaces that reaches affected users:

- affected GitHub Release text for release-specific warnings;
- `CHANGELOG.md` for superseded or corrected release history;
- README release guidance when the response changes general release policy;
- install and supply-chain docs when the issue affects download,
  verification, installer, or update behavior;
- `SECURITY.md` for private reporting and coordination of vulnerabilities.

Update checks and managed update behavior should continue to prefer newer
corrected releases through normal release metadata. The initial release does not use background
update checks or package-manager channels.
The official installer additionally checks `release-policy.json` before
artifact download. That policy cannot prevent public source builds, copied
archives, or manual old-tag installs, so bad-release notes must still tell users
what not to run.

## Release Status Terms

Use these terms consistently in release notes and changelog entries:

- `supported`: the release remains valid for users of the initial release.
- `superseded`: users should install a newer corrected release.
- `unsafe`: users should not download or execute the release assets.
- `corrected`: a newer release fixes the bad release condition.

Avoid vague labels such as deprecated, yanked, or withdrawn in the initial release unless the
release note explains exactly what users should do.
