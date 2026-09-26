#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT_DIR"

fail() {
  printf 'public repository sweep failed: %s\n' "$*" >&2
  exit 1
}

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/threadpoint-public-sweep.XXXXXX")
trap 'rm -rf "$tmpdir"' EXIT INT TERM

files="$tmpdir/files"
findings="$tmpdir/findings"
generated="$tmpdir/generated"
: >"$findings"

find . \( \
  -path './.git' -o \
  -path './dist' -o \
  -path './tmp' -o \
  -path './.cache' -o \
  -path './.pytest_cache' -o \
  -path './node_modules' -o \
  -path './vendor' -o \
  -path './target' -o \
  -path './coverage' -o \
  -path './.ua' -o \
  -path './graphify-out' -o \
  -path './.tmp' \
  \) -prune -o -type f -print | while IFS= read -r file; do
  case "$file" in
    *.go | *.md | *.sh | *.yml | *.yaml | *.json | *.toml | *.txt | *.required | *.mod | *.sum | *.html | *.css | *.js | *.ts | */CODEOWNERS | */LICENSE | */NOTICE)
      printf '%s\n' "$file"
      ;;
  esac
done | sort >"$files"

append_matches() {
  description=$1
  pattern=$2
  while IFS= read -r file; do
    grep -nE "$pattern" "$file" 2>/dev/null | sed "s#^#${description}: ${file}:#" >>"$findings" || true
  done <"$files"
}

append_literal_matches() {
  description=$1
  needle=$2
  while IFS= read -r file; do
    grep -nF "$needle" "$file" 2>/dev/null | sed "s#^#${description}: ${file}:#" >>"$findings" || true
  done <"$files"
}

append_literal_matches_excluding() {
  description=$1
  needle=$2
  shift 2
  while IFS= read -r file; do
    skip=0
    for exclude_file in "$@"; do
      [ "$file" = "$exclude_file" ] && skip=1
    done
    [ "$skip" -eq 1 ] && continue
    grep -nF "$needle" "$file" 2>/dev/null | sed "s#^#${description}: ${file}:#" >>"$findings" || true
  done <"$files"
}

append_matches_excluding() {
  description=$1
  pattern=$2
  shift 2
  while IFS= read -r file; do
    skip=0
    for exclude_file in "$@"; do
      [ "$file" = "$exclude_file" ] && skip=1
    done
    [ "$skip" -eq 1 ] && continue
    grep -nE "$pattern" "$file" 2>/dev/null | sed "s#^#${description}: ${file}:#" >>"$findings" || true
  done <"$files"
}

find . -maxdepth 2 \( \
  -path './.tmp' \
  \) -prune -o \( \
  -name dist -o \
  -name tmp -o \
  -name .cache -o \
  -name .pytest_cache -o \
  -name coverage \
  \) -print | sort >"$generated"

if [ -s "$generated" ]; then
  sed 's#^#generated artifact directory: #' "$generated" >>"$findings"
fi

for planning_doc in \
  docs/vision.md \
  docs/features.md \
  docs/requirements.md \
  docs/first-release-scope.md; do
  if [ -e "$planning_doc" ]; then
    printf 'parent-owned planning document: %s\n' "$planning_doc" >>"$findings"
  fi
done

self='./scripts/check-public-repo-sweep.sh'
redaction_test='./redact/redact_test.go'
# Keep synthetic restore redaction fixtures together in this dedicated file.
# Other restore tests remain subject to every public-repo guard.
restoration_test='./restore/redaction_test.go'

# Redaction tests deliberately use synthetic absolute paths to prove that
# product-owned previews replace them with stable placeholders.
append_matches_excluding "private local path" '(^|[^[:alnum:]_])(/home/|/Users/|[A-Z]:[\\/])' "$redaction_test" "$restoration_test" "$self"
append_literal_matches_excluding "private parent repository wording" "private parent repo" "$self"
append_literal_matches_excluding "private parent repository wording" "parent repo only" "$self"
append_literal_matches_excluding "private parent repository wording" "parent-repo-only" "$self"
append_matches_excluding "non-public repository reference" '(^|[^@])threadgrid/([^t]|t[^h]|th[^r]|thr[^e]|thre[^a]|threa[^d]|thread[^p]|threadp[^o]|threadpo[^i]|threadpoi[^n]|threadpoin[^t])' "$self"
append_matches_excluding "private product name reference" '(^|[^[:alnum:]_/@.-])([Tt]hreadgrid)([^[:alnum:]_/-]|$)' "$self" './NOTICE'
append_matches_excluding "private hosted marketing surface" 'threadgrid\.dev' "$self"
append_matches_excluding "private product surface wording" '(buy\.threadgrid|docs\.threadgrid|billing|entitlement)' "$self"
append_matches_excluding "private key fixture outside redaction tests" '-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----' "$redaction_test" "$restoration_test" "$self"
append_matches_excluding "secret-looking token outside redaction tests" '(ghp_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|AKIA[0-9A-Z]{16})' "$redaction_test" "$restoration_test" "$self"
append_matches_excluding "secret assignment outside redaction tests" '(api[_-]?key|token|password|secret)[[:space:]]*[:=][[:space:]]*["'\'']?[A-Za-z0-9_./+=-]{12,}' "$redaction_test" "$restoration_test" "$self"

for prose_file in \
  README.md \
  CONTRIBUTING.md \
  docs \
  .github \
  testdata; do
  if [ -f "$prose_file" ]; then
    grep -nEi 'session|transcript' "$prose_file" 2>/dev/null | sed "s#^#session/transcript wording: ${prose_file}:#" >>"$findings" || true
  elif [ -d "$prose_file" ]; then
    find "$prose_file" \( \
      -path '*/.git' -o \
      -path '*/dist' -o \
      -path '*/tmp' -o \
      -path './.tmp' \
      \) -prune -o -type f -print | while IFS= read -r file; do
      case "$file" in
        *.md | *.yml | *.yaml | *.json | *.toml | *.txt | *.sh | *.go)
          grep -nEi 'session|transcript' "$file" 2>/dev/null | sed "s#^#session/transcript wording: ${file}:#" >>"$findings" || true
          ;;
      esac
    done
  fi
done

if [ -s "$findings" ]; then
  cat "$findings" >&2
  if grep -Eq '^(private local path|private key fixture|secret-looking token|secret assignment)' "$findings"; then
    printf '%s\n' 'Synthetic redaction fixtures belong in redact/redact_test.go or restore/redaction_test.go; other test files are intentionally checked.' >&2
  fi
  fail "public repository contains private or release-blocking content"
fi

printf '%s\n' "public repository sweep passed"
