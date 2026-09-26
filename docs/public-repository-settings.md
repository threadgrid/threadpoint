# Public Repository Settings

This document records the intended initial-release GitHub repository settings for the
public threadpoint repository. It is a maintainer checklist and audit target,
not proof that remote GitHub settings have already been applied.

Apply and audit these settings before announcing the initial release.

## Repository Metadata

| Setting        | Initial Release requirements                                                                                                                   |
| -------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| Repository     | `github.com/threadgrid/threadpoint`                                                                                                            |
| Visibility     | Public                                                                                                                                         |
| Default branch | `main`                                                                                                                                         |
| License        | Apache-2.0                                                                                                                                     |
| Homepage       | README and `docs/README.md` entry point                                                                                                        |
| Description    | Provider-neutral local agent memory, instruction, and skill layout tooling                                                                     |
| Topics         | `agent-memory`, `agent-instructions`, `agent-skills`, `agents-md`, `provider-neutral`, `local-first`, `cli`, `go`, `golang`, `developer-tools` |

The repository homepage and description should route users to the README first.
Longer product details, install behavior, support boundaries, and trust posture
remain in `docs/`.

## Default Branch Protection

Protect `main` with the following initial-release requirements:

| Control                   | Initial Release requirements                                                                                          |
| ------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| Pull request before merge | Required                                                                                                              |
| Approving reviews         | Require 1 approving review; dismiss approvals when new commits change the pull request                                |
| CODEOWNERS review         | Disabled; ownership assignments remain advisory                                                                       |
| Conversation resolution   | Required before merge                                                                                                 |
| Required status checks    | `Format`; `Go quality`; `SonarQube`; `Lint`; `Go compatibility (latest stable)`; `Vulnerability scan`; `DCO sign-off` |
| Direct pushes             | Blocked by the pull-request rule for non-administrators; administrators may bypass branch protection for an exception |
| Fork syncing              | Allowed                                                                                                               |

The required status check names intentionally match the public CI and DCO
workflow job names. GitHub matches a required check against the check-run name,
which is the job name alone; the `CI / Format` form shown on pull requests adds
the workflow name and does not match. If those job names change, update these
requirements before changing branch protection. When an administrator bypasses
protection, record the reason in the pull request and re-run the full CI gate
after service recovery when GitHub or a provider prevents the normal path.

Enable required code-owner review only after the owners team resolves on GitHub,
has access to the repository, and has at least two eligible reviewers. Otherwise
a pull request author cannot obtain the required approval without an
administrator bypass.

Bootstrap the public quality-project main analysis before adding
`SonarQube` to live branch protection. Configure the repository Actions
variable `SONAR_ORGANIZATION` and the repository secret `SONAR_TOKEN`, disable
provider automatic analysis, verify pull-request decoration, and then apply the
checked-in policy. Fork and Dependabot pull requests intentionally skip this
token-bearing job because Actions secrets are unavailable in those trust
contexts; the ordinary public checks still run.

## Release And Tag Policy

Public release tags are part of the user-facing install and verification
surface.

- Use semantic v-tags for public releases, including prerelease tags such as
  `v0.1.0-rc.1`.
- Public semantic tags are immutable after publication.
- Release and tag creation are maintainer-only.
- Do not move, retarget, or delete a public semantic tag after users may have
  consumed it.
- If a bad release is published, create a corrected follow-up release and use
  the documented bad-release response procedure.
- Signed-tag policy remains deferred to v1.0 hardening.

## Protected Release Environment

Create a GitHub Actions environment named `release` before provisioning release
authority. It is the only location for the
`THREADPOINT_RELEASE_SIGNING_PRIVATE_KEY` secret; do not create a repository- or
organization-level copy that tag-selected workflows could reference.

Require at least one maintainer reviewer, disallow administrator bypass, and
limit the environment to the protected `main` branch. While the project has a
single maintainer, that maintainer approves their own releases, so self-review
stays allowed; enable prevent-self-review once a second maintainer can approve.
The default-branch `Publish release` workflow retains its immutable controller
generation and enters this environment only after it has bound the candidate
tag to an exact successful main CI SHA in that generation's history and
verified successful push-to-main CI for the controller generation itself.
Arbitrary-ref dry runs have no signing, release-write, or attestation authority.
Tag-selected candidate jobs have only the OIDC and attestation permissions
needed to record provenance for the archives they build; they have no signing
key or release-write authority.

## Repository Feature Toggles

| Feature                         | Initial Release requirements                              |
| ------------------------------- | --------------------------------------------------------- |
| Issues                          | Enabled with structured forms                             |
| Blank issues                    | Disabled                                                  |
| Discussions                     | Disabled for the initial release                          |
| Wiki                            | Disabled                                                  |
| Projects                        | Disabled unless maintainers explicitly use them privately |
| Sponsorship                     | Disabled for the initial release                          |
| Private vulnerability reporting | Enabled when available                                    |

Security reports must route through the public security policy and private
vulnerability reporting when available. Public issues and pull requests must
not contain secrets, private memory, local paths, backup payloads, proprietary
project content, or sensitive diffs.

## Audit Checklist

Before announcing the initial release, maintainers should verify:

- repository metadata matches these requirements;
- branch protection on `main` uses the listed review, conversation, direct
  push, and status-check controls;
- release and tag creation remain limited to maintainers;
- the `release` environment has required review and owns the only production
  signing-key secret;
- public semantic tags have not been moved after publication;
- optional GitHub features match the feature-toggle table;
- private vulnerability reporting is enabled when GitHub exposes it for the
  repository.

TP-TASK-017 remains the v1.0 hardening task for stronger repository rules,
signed-tag policy, and longer-term governance decisions.
