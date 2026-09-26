#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly EXPECTED_REPOSITORY="threadgrid/threadpoint"
readonly DEFAULT_IMAGE="threadpoint-sonar-scanner:12.1.0.3233_8.0.1-go1.26.8"
readonly CACHE_VOLUME="threadpoint-sonar-cache"

fail() {
  printf 'threadpoint SonarQube scan failed: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

usage() {
  cat <<'EOF'
Usage: make sonar

Required environment:
  SONAR_TOKEN          SonarQube Cloud analysis token
  SONAR_ORGANIZATION   SonarQube Cloud organization key

On a non-main branch, also set PR_NUMBER. PR_BASE defaults to main.
The branch commit must already be pushed to origin.
EOF
}

if [[ $# -ne 0 ]]; then
  usage >&2
  exit 2
fi

require_command docker
require_command git
require_command make
require_command tar

[[ -n "${SONAR_TOKEN:-}" ]] || fail "SONAR_TOKEN is required"
[[ -n "${SONAR_ORGANIZATION:-}" ]] || fail "SONAR_ORGANIZATION is required"

git_root="$(git -C "${ROOT_DIR}" rev-parse --show-toplevel 2>/dev/null)" ||
  fail "repository metadata is unavailable"
[[ "${git_root}" == "${ROOT_DIR}" ]] || fail "run the scanner from the canonical repository clone"
git_dir="$(git -C "${ROOT_DIR}" rev-parse --absolute-git-dir 2>/dev/null)" ||
  fail "Git metadata is unavailable"
[[ -d "${git_dir}" ]] || fail "Git metadata is unavailable"
[[ ! -f "${git_dir}/commondir" ]] ||
  fail "linked Git worktrees are unsupported; use a standalone clone or the canonical submodule"

origin_url="$(git -C "${ROOT_DIR}" remote get-url origin 2>/dev/null)" ||
  fail "the origin remote is unavailable"
case "${origin_url}" in
  "https://github.com/${EXPECTED_REPOSITORY}" | \
    "https://github.com/${EXPECTED_REPOSITORY}.git" | \
    "git@github.com:${EXPECTED_REPOSITORY}" | \
    "git@github.com:${EXPECTED_REPOSITORY}.git" | \
    "ssh://git@github.com/${EXPECTED_REPOSITORY}" | \
    "ssh://git@github.com/${EXPECTED_REPOSITORY}.git") ;;
  *) fail "origin does not identify the canonical GitHub repository" ;;
esac

branch="$(git -C "${ROOT_DIR}" symbolic-ref --quiet --short HEAD 2>/dev/null)" ||
  fail "detached HEAD scans are not allowed"
[[ "$(git -C "${ROOT_DIR}" rev-parse --is-shallow-repository)" == "false" ]] ||
  fail "shallow clones are not allowed; fetch the complete Git history"
[[ -z "$(git -C "${ROOT_DIR}" status --porcelain --untracked-files=normal)" ]] ||
  fail "the working tree must be clean before coverage is generated"

remote_branch="refs/remotes/origin/${branch}"
git -C "${ROOT_DIR}" show-ref --verify --quiet "${remote_branch}" ||
  fail "origin/${branch} is unavailable; push the branch and fetch origin"
revision="$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null)" ||
  fail "HEAD revision is unavailable"
readonly revision
[[ "${revision}" == "$(git -C "${ROOT_DIR}" rev-parse "${remote_branch}")" ]] ||
  fail "HEAD must match origin/${branch} so the scan is attributable"

scanner_args=(
  "-Dsonar.organization=${SONAR_ORGANIZATION}"
  "-Dsonar.qualitygate.wait=true"
  "-Dsonar.qualitygate.timeout=300"
  "-Dsonar.scm.revision=${revision}"
  "-Dsonar.working.directory=/tmp/.scannerwork"
)

if [[ "${branch}" == "main" ]]; then
  [[ -z "${PR_NUMBER:-}" ]] || fail "PR_NUMBER must be unset when scanning main"
else
  [[ "${PR_NUMBER:-}" =~ ^[1-9][0-9]*$ ]] ||
    fail "PR_NUMBER must be a positive integer on non-main branches"
  readonly pr_base="${PR_BASE:-main}"
  git check-ref-format --branch "${pr_base}" >/dev/null 2>&1 || fail "PR_BASE is not a valid branch name"
  git -C "${ROOT_DIR}" show-ref --verify --quiet "refs/remotes/origin/${pr_base}" ||
    fail "origin/${pr_base} is unavailable; fetch the pull-request base"
  scanner_args+=(
    "-Dsonar.pullrequest.key=${PR_NUMBER}"
    "-Dsonar.pullrequest.branch=${branch}"
    "-Dsonar.pullrequest.base=${pr_base}"
  )
fi

make -C "${ROOT_DIR}" coverage
make -C "${ROOT_DIR}" sonar-image

scan_root="$(mktemp -d "${TMPDIR:-/tmp}/threadpoint-sonar.XXXXXX")"
cleanup() {
  rm -rf -- "${scan_root}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "${scan_root}/threadpoint" "${scan_root}/.git/modules/threadpoint"
git -C "${ROOT_DIR}" archive --format=tar HEAD | tar -xf - -C "${scan_root}/threadpoint"
cp "${ROOT_DIR}/coverage.out" "${scan_root}/threadpoint/coverage.out"
printf 'gitdir: ../.git/modules/threadpoint\n' >"${scan_root}/threadpoint/.git"

docker_mounts=(
  --volume "${scan_root}:/workspace:ro"
  --volume "${git_dir}:/workspace/.git/modules/threadpoint:ro"
)

readonly scanner_image="${SONAR_SCANNER_IMAGE:-${DEFAULT_IMAGE}}"
docker volume create "${CACHE_VOLUME}" >/dev/null
docker run \
  --rm \
  --platform linux/amd64 \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
  --env SONAR_HOST_URL=https://sonarcloud.io \
  --env SONAR_TOKEN \
  --volume "${CACHE_VOLUME}:/opt/sonar-scanner/.sonar/cache" \
  "${docker_mounts[@]}" \
  --workdir /workspace/threadpoint \
  "${scanner_image}" \
  "${scanner_args[@]}"
