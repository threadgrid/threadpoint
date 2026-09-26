# Threadpoint Docs

Threadpoint normalizes and consolidates the memory and skill files that AI
coding agents leave in a project into one shared, provider-neutral knowledgebase.
These docs cover what it does, how to use it safely, and how it is built and
released. Start at the top and go as deep as you need.

## Start here

- [`glossary.md`](glossary.md) — canonical terminology used by docs and CLI help
- [`install.md`](install.md) — install, update, and uninstall flows

## Using threadpoint

- [`workflows.md`](workflows.md) — common local workflows, end to end
- [`cli-reference.md`](cli-reference.md) — implemented commands and usage
- [`cli-contract.md`](cli-contract.md) — output streams, exit codes, and JSON failure contract
- [`diagnostic-code-contract.md`](diagnostic-code-contract.md) — finite machine-readable outcome codes and messages
- [`canonical-layout-guide.md`](canonical-layout-guide.md) — the user-facing shared project layout
- [`threadpoint-examples.md`](threadpoint-examples.md) — worked examples backed by synthetic fixtures
- [`support-boundary.md`](support-boundary.md) — supported providers, and what is out of scope
- [`platform-support.md`](platform-support.md) — supported platforms and the release matrix

## How it works

- [`go-api.md`](go-api.md) — public Go packages, examples, and pre-1.0 compatibility
- [`canonical-layout-and-bridge-contract-spec.md`](canonical-layout-and-bridge-contract-spec.md) — who owns which files, and the bridge contract
- [`provider-registry-extension-guide.md`](provider-registry-extension-guide.md) — add support for a new agent provider
- [`environment-and-state-policy.md`](environment-and-state-policy.md) — state directories and environment variables
- [`lock-recovery.md`](lock-recovery.md) — recover a stale project mutation lock safely

## Safety, privacy, and trust

- [`security-privacy.md`](security-privacy.md) — local-first trust and destructive-action safeguards
- [`filesystem-safety.md`](filesystem-safety.md) — traversal boundaries and safety limits
- [`network-egress-and-no-telemetry.md`](network-egress-and-no-telemetry.md) — no telemetry, with network-allowlist verification
- [`redaction-and-diff-safety.md`](redaction-and-diff-safety.md) — best-effort redaction for user-facing diffs and reports
- [`license-compliance.md`](license-compliance.md) — Apache-2.0 review expectations for releases

## Releasing and operating

- [`release-artifact-dry-run.md`](release-artifact-dry-run.md) — validate release artifacts before tagging
- [`release-candidate-smoke-tests.md`](release-candidate-smoke-tests.md) — smoke-test packaged artifacts
- [`supply-chain.md`](supply-chain.md) — release artifact provenance and verification
- [`bad-release-response.md`](bad-release-response.md) — the lightweight response when a release goes wrong
- [`public-launch-surface-verification.md`](public-launch-surface-verification.md) — public repository and URL launch checks
- [`public-repository-settings.md`](public-repository-settings.md) — maintainer-facing GitHub settings requirements
