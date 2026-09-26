#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT_DIR"

failures=0

go_files() {
  find . -type f -name '*.go' ! -name '*_test.go' \
    ! -path './.git/*' \
    ! -path './dist/*' \
    ! -path './vendor/*'
}

script_files() {
  find scripts -type f -name '*.sh' ! -name 'check-network-egress.sh'
}

is_allowed() {
  file="$1"
  shift
  file="${file#./}"
  for allowed in "$@"; do
    allowed="${allowed#./}"
    if [ "$file" = "$allowed" ]; then
      return 0
    fi
  done
  return 1
}

print_allowed() {
  if [ "$#" -eq 0 ]; then
    printf '  (none)\n' >&2
    return
  fi
  for allowed in "$@"; do
    printf '  %s\n' "$allowed" >&2
  done
}

check_pattern() {
  scanner="$1"
  description="$2"
  pattern="$3"
  shift 3
  unexpected="$(mktemp "${TMPDIR:-/tmp}/threadpoint-network-egress.XXXXXX")"

  "$scanner" | while IFS= read -r file; do
    if grep -Eq "$pattern" "$file"; then
      if ! is_allowed "$file" "$@"; then
        printf '%s\n' "$file" >>"$unexpected"
      fi
    fi
  done

  if [ -s "$unexpected" ]; then
    failures=1
    printf '%s\n' "Unexpected ${description}:" >&2
    sed 's/^/  /' "$unexpected" >&2
    printf '%s\n' 'Allowed files:' >&2
    print_allowed "$@"
    printf '\n' >&2
  fi

  rm -f "$unexpected"
}

check_pattern \
  go_files \
  'Go network client surface' \
  '"net/http"|http\.DefaultClient|http\.Client|http\.Get|http\.Post|http\.NewRequest|NewRequestWithContext|net\.Dial|DialContext|websocket|grpc' \
  './cmd/threadpoint/update.go' \
  './cmd/threadpoint/self_update.go'

check_pattern \
  go_files \
  'hard-coded production HTTP(S) URL' \
  'https?://' \
  './provider/provider.go' \
  './cmd/threadpoint/update.go' \
  './cmd/threadpoint/self_update.go' \
  './cmd/threadpoint/cli.go'

check_pattern \
  script_files \
  'shell downloader' \
  '(curl|wget)[[:space:]]' \
  'scripts/install.sh'

check_pattern \
  script_files \
  'SonarQube Cloud upload destination in shell tooling' \
  'SONAR_HOST_URL=https?://' \
  'scripts/check-sonar-configuration.sh' \
  'scripts/run-sonar.sh'

check_pattern \
  go_files \
  'external process execution surface' \
  'exec\.Command(Context)?' \
  './doctor/doctor.go' \
  './internal/editor/editor.go' \
  './stage/stage.go' \
  './restore/restore.go' \
  './cmd/threadpoint/self_update.go' \
  './cmd/threadpoint/flags.go' \
  './internal/shellcmd/shellcmd.go'

check_pattern \
  go_files \
  'telemetry or analytics keyword in production Go' \
  'telemetry|analytics|phone[ -]?home|tracking[[:space:]_-]?id|usage[[:space:]_-]?event'

if [ "$failures" -ne 0 ]; then
  printf '%s\n' 'Network egress allowlist check failed.' >&2
  printf '%s\n' 'If this is an intentional explicit network or external-tool surface, update scripts/check-network-egress.sh and docs/network-egress-and-no-telemetry.md together.' >&2
  exit 1
fi

printf '%s\n' 'network egress allowlist check passed'
