#!/usr/bin/env sh
# shellcheck disable=SC2016 # YAML expressions below are intentionally literal.
set -eu

fail() {
  printf 'workflow supply-chain test failed: %s\n' "$*" >&2
  exit 1
}

ci=.github/workflows/ci.yml
candidate=.github/workflows/release.yml
publisher=.github/workflows/release-publish.yml

grep -Fq 'scripts/install-ci-formatters.sh' "$ci" || fail 'CI does not use the repository-owned formatter installer'
if grep -Fq 'taiki-e/install-action' "$ci"; then
  fail 'CI still depends on an action excluded by the selected-actions policy'
fi

grep -Fq 'workflow_run:' "$publisher" || fail 'trusted publication is not triggered from the powerless candidate workflow'
grep -Fq 'environment: release' "$publisher" || fail 'publication does not use the protected release environment'
grep -Fq 'ref: ${{ github.workflow_sha }}' "$publisher" || fail 'publisher does not retain its immutable controller generation'
grep -Fq 'test "$(git rev-parse HEAD)" = "${CONTROLLER_SHA}"' "$publisher" || fail 'publisher does not verify its retained controller generation'
grep -Fq 'git merge-base --is-ancestor "${HEAD_SHA}" "${CONTROLLER_SHA}"' "$publisher" || fail 'publisher does not bind the tag SHA to its trusted controller generation'
grep -Fq 'git merge-base --is-ancestor "${CONTROLLER_SHA}" origin/main' "$publisher" || fail 'publisher does not bind its trusted controller generation to main'
grep -Fq 'required_green_shas=("${HEAD_SHA}")' "$publisher" || fail 'publisher does not require green main CI for the candidate SHA'
grep -Fq 'if [[ "${CONTROLLER_SHA}" != "${HEAD_SHA}" ]]' "$publisher" || fail 'publisher does not deduplicate equal candidate and controller CI gates'
grep -Fq 'required_green_shas+=("${CONTROLLER_SHA}")' "$publisher" || fail 'publisher does not require green main CI for an advanced controller SHA'
grep -Fq 'for required_sha in "${required_green_shas[@]}"' "$publisher" || fail 'publisher does not verify every required green SHA'
grep -Fq -- '-f head_sha="${required_sha}"' "$publisher" || fail 'publisher CI lookup is not bound to each required SHA'
grep -Fq 'head_branch == "main"' "$publisher" || fail 'publisher does not require successful main CI for the exact SHA'
grep -Fq 'THREADPOINT_RELEASE_SIGNING_PRIVATE_KEY' "$publisher" || fail 'trusted publisher is missing checksum signing'
publisher_no_credentials=$(grep -Fc 'persist-credentials: false' "$publisher")
[ "$publisher_no_credentials" -eq 1 ] || fail 'trusted controller checkout persists a write-capable credential'
candidate_no_credentials=$(grep -Fc 'persist-credentials: false' "$candidate")
[ "$candidate_no_credentials" -eq 2 ] || fail 'powerless candidate checkout persists a credential'
grep -Fq 'go build -trimpath' "$publisher" || fail 'trusted signer is not compiled before the secret-bearing step'
if grep -Fq 'go run ./cmd/threadpoint-release-signature' "$publisher"; then
  fail 'compiler or dependency machinery still runs with the production signing key'
fi
if grep -Fq 'THREADPOINT_RELEASE_SIGNING_PRIVATE_KEY' "$candidate"; then
  fail 'arbitrary-ref or tag-selected candidate code can access the production signing key'
fi
candidate_attestation_permissions=$(grep -Fc 'attestations: write' "$candidate")
[ "$candidate_attestation_permissions" -eq 1 ] || fail 'tag candidate must have exactly one archive-attestation permission grant'
candidate_oidc_permissions=$(grep -Fc 'id-token: write' "$candidate")
[ "$candidate_oidc_permissions" -eq 1 ] || fail 'tag candidate must have exactly one OIDC permission grant'
grep -Fq 'name: Attest exact tag-built release archives' "$candidate" || fail 'tag candidate does not attest the exact archives it built'
grep -Fq 'subject-checksums: dist/checksums.txt' "$candidate" || fail 'tag candidate archive attestation is not bound to its checksum manifest'
if grep -Eq 'args:[[:space:]]+release --clean[[:space:]]*$' "$candidate" "$publisher"; then
  fail 'release workflow still invokes a second publishing GoReleaser build'
fi

candidate_smoke_line=$(grep -n 'name: Smoke unsigned release candidate' "$candidate" | cut -d: -f1)
candidate_attest_line=$(grep -n 'name: Attest exact tag-built release archives' "$candidate" | cut -d: -f1)
candidate_upload_line=$(grep -n 'name: Upload exact release candidate' "$candidate" | cut -d: -f1)
[ -n "$candidate_smoke_line" ] && [ -n "$candidate_attest_line" ] && [ -n "$candidate_upload_line" ] || fail 'candidate smoke/attest/upload gates are incomplete'
[ "$candidate_smoke_line" -lt "$candidate_attest_line" ] && [ "$candidate_attest_line" -lt "$candidate_upload_line" ] || fail 'candidate archives are uploaded before tag-context attestation completes'

grep -Fq 'name: Verify tag-context archive provenance' "$publisher" || fail 'publisher does not verify candidate archive provenance'
grep -Fq -- '--signer-workflow "$GITHUB_REPOSITORY/.github/workflows/release.yml"' "$publisher" || fail 'publisher does not require the tag candidate workflow as archive signer'
grep -Fq -- '--source-ref "refs/tags/${RELEASE_TAG}"' "$publisher" || fail 'publisher archive verification is not bound to the release tag ref'
grep -Fq -- '--source-digest "${RELEASE_SHA}"' "$publisher" || fail 'publisher archive verification is not bound to the candidate commit'
if grep -Fq 'subject-checksums: candidate/checksums.txt' "$publisher"; then
  fail 'default-branch publisher still creates misleading tag archive attestations'
fi
verify_line=$(grep -n 'name: Verify tag-context archive provenance' "$publisher" | cut -d: -f1)
sign_line=$(grep -n 'name: Sign candidate checksums with protected key' "$publisher" | cut -d: -f1)
[ -n "$verify_line" ] && [ -n "$sign_line" ] && [ "$verify_line" -lt "$sign_line" ] || fail 'publisher signs before verifying tag-context archive provenance'

draft_line=$(grep -n 'gh release create' "$publisher" | cut -d: -f1)
attest_line=$(grep -n 'name: Attest signed checksum manifest' "$publisher" | cut -d: -f1)
publish_line=$(grep -n 'name: Publish fully verified draft' "$publisher" | cut -d: -f1)
[ -n "$draft_line" ] && [ -n "$attest_line" ] && [ -n "$publish_line" ] || fail 'draft/attest/publish gates are incomplete'
[ "$draft_line" -lt "$attest_line" ] && [ "$attest_line" -lt "$publish_line" ] || fail 'release is public before attestation completes'
grep -Fq -- '--draft' "$publisher" || fail 'release assets are not held in a draft during verification'

printf 'workflow supply-chain tests passed\n'
