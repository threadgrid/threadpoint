# Security Policy

Threadpoint reads local agent instructions, memory-like files, project-private
stores, and backups. Report security issues through a private channel whenever
possible, and do not post sensitive local content in public issues.

## Supported Versions

The current initial release line and the default branch are supported for security
reports before the first stable release.

## What To Report

Use the security reporting path for issues such as:

- path traversal, unsafe archive extraction, or unsafe filesystem writes;
- secret, token, private memory, backup, or diff disclosure;
- unexpected network egress or telemetry behavior;
- installer, managed-update, checksum, or release artifact integrity problems;
- destructive-command behavior that can remove or overwrite files unexpectedly.

Normal bugs, documentation issues, and feature requests can use public issue
forms. Requests to support a new agent/provider or project-native artifact are
feature requests, not vulnerability reports.

## What Not To Post Publicly

Do not include secrets, credentials, private memory, local filesystem paths,
backup payloads, proprietary project content, or sensitive diffs in public
issues, pull requests, discussions, or logs.

If you need to share command output, redact local paths and sensitive content
first. Threadpoint includes best-effort redaction in some report surfaces, but
you remain responsible for reviewing anything you share publicly.

## How To Report

Preferred route: use GitHub private vulnerability reporting from this
repository's Security tab when it is available.

If private vulnerability reporting is not available, open a public issue titled
`Security contact request` and include only:

- a brief non-sensitive summary;
- affected platform or install method if relevant;
- how maintainers can contact you.

Do not include exploit details, proof-of-concept payloads, private files,
secrets, local paths, or sensitive command output in that public issue. A
maintainer will move the discussion to a private channel.

## Response Expectations

Maintainers will acknowledge valid security reports as soon as practical,
request private reproduction details when needed, and coordinate fixes before
public disclosure when the issue could expose user data, local files, or release
integrity.
