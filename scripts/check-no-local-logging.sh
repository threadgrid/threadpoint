#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT_DIR"

fail() {
  printf 'threadpoint no-local-logging check failed: %s\n' "$*" >&2
  exit 1
}

production_go_files() {
  find . -type f -name '*.go' ! -name '*_test.go' \
    ! -path './.git/*' \
    ! -path './dist/*' \
    ! -path './vendor/*'
}

check_absent_from_production_go() {
  pattern="$1"
  description="$2"
  unexpected="$(mktemp "${TMPDIR:-/tmp}/threadpoint-no-local-logging.XXXXXX")"
  production_go_files | while IFS= read -r file; do
    if grep -Eq "$pattern" "$file"; then
      printf '%s\n' "$file" >>"$unexpected"
    fi
  done
  if [ -s "$unexpected" ]; then
    printf '%s\n' "Unexpected ${description}:" >&2
    sed 's/^/  /' "$unexpected" >&2
    rm -f "$unexpected"
    exit 1
  fi
  rm -f "$unexpected"
}

check_absent_from_production_go 'THREADPOINT_LOG_LEVEL' 'threadpoint log-level environment control in production Go'
check_absent_from_production_go 'threadpoint\.log\.jsonl|LocalLogWriter|NewLocalLogWriter' 'threadpoint persistent local log writer in production Go'

printf '%s\n' 'threadpoint no-local-logging check passed'
