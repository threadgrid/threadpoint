#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT_DIR"

fail() {
  printf 'SonarQube configuration check failed: %s\n' "$*" >&2
  exit 1
}

require_literal() {
  file=$1
  literal=$2
  description=$3
  grep -Fq -- "$literal" "$file" || fail "$description"
}

require_pattern() {
  file=$1
  pattern=$2
  description=$3
  grep -Eq -- "$pattern" "$file" || fail "$description"
}

require_csv_property_entry() {
  file=$1
  property=$2
  expected=$3
  description=$4
  line=$(grep -F -- "${property}=" "$file") || fail "$description"
  case ",${line#*=}," in
    *",${expected},"*) ;;
    *) fail "$description" ;;
  esac
}

require_literal sonar-project.properties 'sonar.projectKey=threadpoint' 'project key is missing'
require_literal sonar-project.properties 'sonar.inclusions=**/*.go,go.mod' 'analysis must remain scoped to Go source'
require_literal sonar-project.properties 'sonar.go.coverage.reportPaths=coverage.out' 'Go coverage import is missing'
test_inclusions=$(grep -F -- 'sonar.test.inclusions=' sonar-project.properties) ||
  fail 'test inclusion patterns are missing'
printf '%s\n' "${test_inclusions#*=}" | tr ',' '\n' |
  while IFS= read -r test_pattern; do
    require_csv_property_entry sonar-project.properties sonar.exclusions "$test_pattern" "test pattern $test_pattern must be excluded from main sources"
  done
if grep -Eq '^sonar\.organization=' sonar-project.properties; then
  fail 'the organization key must remain external configuration'
fi

require_pattern .sonar/Dockerfile '^FROM sonarsource/sonar-scanner-cli:[^[:space:]@]+@sha256:[0-9a-f]{64}$' 'scanner image must use a literal digest-pinned FROM line'
require_pattern .sonar/Dockerfile '^FROM golang:[^[:space:]@]+@sha256:[0-9a-f]{64}[[:space:]]+AS[[:space:]]+go-toolchain$' 'Go image must use a literal digest-pinned FROM line'
require_literal .sonar/Dockerfile 'COPY --from=go-toolchain /usr/local/go /usr/local/go' 'scanner image must include Go for coverage import'

scanner_image_tag=$(sed -n 's|^FROM sonarsource/sonar-scanner-cli:\([^@]*\)@sha256:.*$|\1|p' .sonar/Dockerfile)
go_image_tag=$(sed -n 's|^FROM golang:\([^@]*\)@sha256:.*$|\1|p' .sonar/Dockerfile)
case "$scanner_image_tag" in
  '' | *'
'*) fail 'scanner Docker tag must resolve to one value' ;;
esac
case "$go_image_tag" in
  '' | *'
'*) fail 'Go Docker tag must resolve to one value' ;;
esac
scanner_cli_version=${scanner_image_tag##*_}
go_version=${go_image_tag%%-*}
printf '%s\n' "$scanner_cli_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || fail 'scanner Docker tag must end with its CLI release'
printf '%s\n' "$go_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || fail 'Go Docker tag must begin with its Go release'
expected_local_image_tag="${scanner_image_tag}-go${go_version}"
make_local_image_tag=$(sed -n 's/^SONAR_SCANNER_IMAGE := threadpoint-sonar-scanner:\(.*\)$/\1/p' Makefile)
runner_local_image_tag=$(sed -n 's/^readonly DEFAULT_IMAGE="threadpoint-sonar-scanner:\([^"]*\)"$/\1/p' scripts/run-sonar.sh)
[ "$make_local_image_tag" = "$expected_local_image_tag" ] || fail 'Makefile scanner image tag has drifted from the Dockerfile bases'
[ "$runner_local_image_tag" = "$expected_local_image_tag" ] || fail 'local runner image tag has drifted from the Dockerfile bases'

require_literal scripts/run-sonar.sh '--read-only' 'local scans must use a read-only container'
require_literal scripts/run-sonar.sh '--cap-drop ALL' 'local scans must drop Linux capabilities'
require_literal scripts/run-sonar.sh '--env SONAR_TOKEN' 'the local token must be passed through the environment'
require_literal scripts/run-sonar.sh 'SONAR_HOST_URL=https://sonarcloud.io' 'the local upload destination must remain explicit'
require_literal scripts/run-sonar.sh 'status --porcelain --untracked-files=normal' 'local scans must reject dirty worktrees'
require_literal scripts/run-sonar.sh 'PR_NUMBER must be a positive integer' 'local pull-request attribution guard is missing'
require_literal scripts/run-sonar.sh '../.git/modules/threadpoint' 'canonical submodule Git metadata is not mounted safely'
require_literal scripts/run-sonar.sh 'archive --format=tar HEAD' 'local scans must use a tracked revision snapshot'
require_literal scripts/run-sonar.sh 'rev-parse --absolute-git-dir' 'linked-worktree detection is missing'
require_literal scripts/run-sonar.sh 'commondir' 'linked Git worktrees must be rejected'
if grep -Eq -- '--tmpfs .*\b(uid|gid)=' scripts/run-sonar.sh; then
  fail 'the writable tmpfs must not depend on the scanner image user ID'
fi
if grep -Fq -- '--volume "${ROOT_DIR}:/workspace' scripts/run-sonar.sh; then
  fail 'the local scanner must not mount ignored working-tree files'
fi

require_literal .github/workflows/ci.yml 'name: SonarQube' 'blocking SonarQube job is missing'
require_literal .github/workflows/ci.yml 'name: go-quality-artifacts' 'SonarQube job does not reuse Go coverage'
require_literal .github/workflows/ci.yml 'SonarSource/sonarqube-scan-action@22918119ff8e1ca75a623e15c8296b6ea4fbe28f' 'SonarQube action is not SHA-pinned'
ci_scanner_version=$(sed -n 's/^[[:space:]]*scannerVersion:[[:space:]]*\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)[[:space:]]*$/\1/p' .github/workflows/ci.yml)
case "$ci_scanner_version" in
  '' | *'
'*) fail 'CI scanner version must resolve to one four-part release' ;;
esac
[ "${ci_scanner_version%.*}" = "$scanner_cli_version" ] || fail 'CI and local scanner CLI versions have drifted'
require_literal .github/workflows/ci.yml 'name: Resolve Go version from the scanner image' 'CI must derive Go from the scanner Dockerfile'
require_literal .github/workflows/ci.yml 'id: sonar_go' 'CI Go version output is missing'
require_literal .github/workflows/ci.yml 's|^FROM golang:' 'CI must read the Go image tag from the scanner Dockerfile'
require_literal .github/workflows/ci.yml 'go_version="${go_image_tag%%-*}"' 'CI must derive the Go patch release from its image tag'
require_literal .github/workflows/ci.yml 'go-version: ${{ steps.sonar_go.outputs.version }}' 'CI setup-go must use the Dockerfile-derived Go version'
require_literal .github/workflows/ci.yml "github.event_name != 'pull_request' ||" 'main pushes must always run SonarQube analysis'
require_literal .github/workflows/ci.yml 'github.event.pull_request.head.repo.full_name == github.repository' 'fork pull requests are not isolated from the SonarQube token'
require_literal .github/workflows/ci.yml "github.actor != 'dependabot[bot]'" 'dependency-bot pull requests are not isolated from the SonarQube token'
require_literal .github/workflows/ci.yml 'test -n "$SONAR_ORGANIZATION"' 'CI must fail before setup when the organization key is missing'
if grep -Fq 'pull_request_target:' .github/workflows/ci.yml; then
  fail 'pull_request_target must not expose the SonarQube token to pull-request code'
fi

require_literal .github/dependabot.yml 'directory: /.sonar' 'Docker dependency updates are not configured'
require_literal .github/branch-protection/main.json 'CI / SonarQube' 'the SonarQube job is not in source-controlled branch protection'

require_literal CONTRIBUTING.md '## Scanner Toolchain Upgrades' 'public scanner toolchain upgrade procedure is missing'
require_literal CONTRIBUTING.md 'scripts/check-sonar-configuration.sh' 'the public upgrade procedure must name its verification command'
if grep -Eq 'SonarScanner CLI `?[0-9]+\.[0-9]+' CONTRIBUTING.md; then
  fail 'scanner CLI versions must not be duplicated in prose'
fi

printf 'SonarQube configuration check passed\n'
