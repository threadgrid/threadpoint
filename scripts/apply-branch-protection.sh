#!/usr/bin/env sh
set -eu

# Apply the threadpoint main-branch protection policy. Requires the GitHub CLI
# and a token with admin access to the repository. The v* tag-protection ruleset
# is applied separately through the GitHub rulesets API (see CONTRIBUTING.md).

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
POLICY="${ROOT_DIR}/.github/branch-protection/main.json"
REPO="${THREADPOINT_PUBLIC_REPO:-threadgrid/threadpoint}"
BRANCH="${THREADPOINT_PUBLIC_BRANCH:-main}"

fail() {
  printf 'threadpoint branch protection apply failed: %s\n' "$*" >&2
  exit 1
}

command -v gh >/dev/null 2>&1 || fail "GitHub CLI is required"
[ -f "$POLICY" ] || fail "policy file is missing: $POLICY"

gh api \
  --method PUT \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "repos/${REPO}/branches/${BRANCH}/protection" \
  --input "$POLICY"

printf 'applied branch protection for %s %s\n' "$REPO" "$BRANCH"
