#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT_DIR"

DIST_DIR="${THREADPOINT_DIST_DIR:-dist}"
SMOKE_DIR="${THREADPOINT_SMOKE_REPORT_DIR:-${DIST_DIR}/smoke}"
CHECKSUMS="${DIST_DIR}/checksums.txt"
SIGNATURE="${DIST_DIR}/checksums.txt.sig"
SMOKE_VERSION="${THREADPOINT_SMOKE_VERSION:-0.0.0-SNAPSHOT}"
SMOKE_VERSION="${SMOKE_VERSION#v}"
SKIP_SIGNATURE="${THREADPOINT_SMOKE_SKIP_SIGNATURE:-0}"
EXPECTED_ARCHIVES="threadpoint_${SMOKE_VERSION}_linux_amd64.tar.gz threadpoint_${SMOKE_VERSION}_linux_arm64.tar.gz threadpoint_${SMOKE_VERSION}_darwin_amd64.tar.gz threadpoint_${SMOKE_VERSION}_darwin_arm64.tar.gz"

fail() {
  printf 'release smoke failed: %s\n' "$*" >&2
  exit 1
}

host_goos() {
  if command -v go >/dev/null 2>&1; then
    go env GOOS
    return
  fi
  case "$(uname -s)" in
    Linux) printf '%s\n' linux ;;
    Darwin) printf '%s\n' darwin ;;
    *) uname -s | tr '[:upper:]' '[:lower:]' ;;
  esac
}

host_goarch() {
  if command -v go >/dev/null 2>&1; then
    go env GOARCH
    return
  fi
  case "$(uname -m)" in
    x86_64 | amd64) printf '%s\n' amd64 ;;
    arm64 | aarch64) printf '%s\n' arm64 ;;
    *) uname -m ;;
  esac
}

archive_checksum() {
  path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$path" | awk '{print $1}'
  else
    fail "sha256sum or shasum is required"
  fi
}

expected_checksum() {
  name="$1"
  awk -v file="$name" '$2 == file { print $1; found = 1 } END { if (!found) exit 1 }' "$CHECKSUMS"
}

archive_is_expected() {
  name="$1"
  for expected in $EXPECTED_ARCHIVES; do
    if [ "$name" = "$expected" ]; then
      return 0
    fi
  done
  return 1
}

check_release_footer() {
  grep -q 'go install github.com/threadgrid/threadpoint/cmd/threadpoint@{{ .Tag }}' .goreleaser.yml || fail ".goreleaser.yml release footer is missing the documented Go install path"
  grep -q 'Release archives, `checksums.txt`, and `checksums.txt.sig` are attached' .goreleaser.yml || fail ".goreleaser.yml release footer is missing signature attachment wording"
  grep -q 'threadpoint-release-signature@{{ .Tag }} --verify' .goreleaser.yml || fail ".goreleaser.yml release footer is missing checksum signature verification command"
  grep -q 'gh attestation verify "${archive}"' .goreleaser.yml || fail ".goreleaser.yml release footer is missing attestation verification command"
  grep -q 'gh attestation verify checksums.txt' .goreleaser.yml || fail ".goreleaser.yml release footer is missing checksum attestation verification command"
  grep -q 'threadgrid/threadpoint/.github/workflows/release.yml' .goreleaser.yml || fail ".goreleaser.yml release footer is missing tag candidate signer workflow"
  grep -q 'threadgrid/threadpoint/.github/workflows/release-publish.yml' .goreleaser.yml || fail ".goreleaser.yml release footer is missing checksum publisher workflow"
  grep -q 'cmd/threadpoint-release-signature' .github/workflows/release-publish.yml || fail "trusted release workflow is missing checksum signing"
  grep -q 'THREADPOINT_RELEASE_SIGNING_PRIVATE_KEY' .github/workflows/release-publish.yml || fail "trusted release workflow is missing threadpoint signing secret"
  grep -q 'candidate/checksums.txt.sig' .github/workflows/release-publish.yml || fail "trusted release workflow does not upload checksum signature"
  grep -q 'subject-checksums: dist/checksums.txt' .github/workflows/release.yml || fail "tag candidate workflow is missing archive attestation from checksums"
  grep -q 'Verify tag-context archive provenance' .github/workflows/release-publish.yml || fail "trusted release workflow is missing archive provenance verification"
  grep -q 'subject-path: candidate/checksums.txt' .github/workflows/release-publish.yml || fail "trusted release workflow is missing checksum file attestation"
}

require_archive_entry() {
  archive="$1"
  pattern="$2"
  description="$3"
  if ! tar -tzf "$archive" | grep -Eq "$pattern"; then
    fail "$(basename "$archive") missing ${description}"
  fi
}

require_exact_archive_entries() {
  archive="$1"
  bundle_dir="$(basename "$archive" .tar.gz)"
  actual="${WORK_DIR}/$(basename "$archive").entries.actual"
  expected="${WORK_DIR}/$(basename "$archive").entries.expected"

  printf '%s\n' \
    "$bundle_dir/LICENSE" \
    "$bundle_dir/NOTICE" \
    "$bundle_dir/README.md" \
    "$bundle_dir/bin/threadpoint" \
    "$bundle_dir/scripts/install.sh" \
    "$bundle_dir/scripts/uninstall.sh" |
    sort >"$expected"

  if ! tar -tzf "$archive" | sed 's#^\./##' | sed '/^$/d' | sed '/\/$/d' | sort >"$actual"; then
    fail "could not inspect archive contents for $(basename "$archive")"
  fi

  if ! cmp -s "$expected" "$actual"; then
    printf '%s\n' "expected archive entries:" >&2
    cat "$expected" >&2
    printf '%s\n' "actual archive entries:" >&2
    cat "$actual" >&2
    fail "$(basename "$archive") archive contents do not match the v0 release contract"
  fi
}

run_threadpoint() {
  label="$1"
  shift
  log="${SMOKE_DIR}/${TARGET_NAME}-${label}.log"
  printf '$ %s' "$BINARY" >"$log"
  for arg in "$@"; do
    printf ' %s' "$arg" >>"$log"
  done
  printf '\n\n' >>"$log"
  "$BINARY" "$@" >>"$log" 2>&1
}

try_threadpoint() {
  label="$1"
  shift
  log="${SMOKE_DIR}/${TARGET_NAME}-${label}.log"
  printf '$ %s' "$BINARY" >"$log"
  for arg in "$@"; do
    printf ' %s' "$arg" >>"$log"
  done
  printf '\n\n' >>"$log"
  "$BINARY" "$@" >>"$log" 2>&1
}

run_host_smoke() {
  archive="$1"
  target="$2"
  work="$3"
  extract_dir="${work}/extract-${target}"
  fixture_dir="${work}/fixtures-${target}"
  mkdir -p "$extract_dir" "$fixture_dir"

  tar -xzf "$archive" -C "$extract_dir"
  BINARY="${extract_dir}/${target}/bin/threadpoint"
  if [ -z "$BINARY" ] || [ ! -f "$BINARY" ]; then
    fail "${target} archive did not extract a threadpoint binary"
  fi
  if [ ! -x "$BINARY" ]; then
    fail "${target} threadpoint binary is not executable"
  fi

  cp -R docs/example-fixtures/bare-init "${fixture_dir}/bare-init"
  cp -R docs/example-fixtures/v0-baseline "${fixture_dir}/v0-baseline"

  export HOME="${work}/home-${target}"
  export THREADPOINT_HOME="${work}/threadpoint-home-${target}"
  mkdir -p "$HOME" "$THREADPOINT_HOME"

  if ! try_threadpoint help-flag --help; then
    run_threadpoint help-command help
  fi
  run_threadpoint version version
  run_threadpoint init init --root "${fixture_dir}/bare-init" --yes
  run_threadpoint status status --root "${fixture_dir}/bare-init"
  # Keep plan-only classification grounded in the checkout's tracked fixture;
  # a copied fixture has no Git ownership evidence and correctly requires
  # explicit classifications.
  run_threadpoint stage-plan stage --root "${ROOT_DIR}/docs/example-fixtures/v0-baseline" --plan
  run_threadpoint doctor doctor --offline
  run_threadpoint restore-list restore list --root "${fixture_dir}/v0-baseline"
}

[ -d "$DIST_DIR" ] || fail "${DIST_DIR} does not exist; run GoReleaser first"
[ -f "$CHECKSUMS" ] || fail "${CHECKSUMS} is missing"
case "$SKIP_SIGNATURE" in
  0)
    [ -f "$SIGNATURE" ] || fail "${SIGNATURE} is missing"
    go run ./cmd/threadpoint-release-signature --verify --in "$CHECKSUMS" --out "$SIGNATURE"
    ;;
  1) ;;
  *) fail "THREADPOINT_SMOKE_SKIP_SIGNATURE must be 0 or 1" ;;
esac
check_release_footer

rm -rf "$SMOKE_DIR"
mkdir -p "$SMOKE_DIR"

HOST_OS="${THREADPOINT_SMOKE_GOOS:-$(host_goos)}"
HOST_ARCH="${THREADPOINT_SMOKE_GOARCH:-$(host_goarch)}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/threadpoint-release-smoke.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

archive_count=0
smoked_count=0

for expected_archive in $EXPECTED_ARCHIVES; do
  [ -f "${DIST_DIR}/${expected_archive}" ] || fail "expected release archive is missing: ${expected_archive}"
  expected="$(expected_checksum "$expected_archive" || true)"
  [ -n "$expected" ] || fail "checksums.txt missing ${expected_archive}"
done

for archive in "$DIST_DIR"/threadpoint_*_*.tar.gz; do
  [ -f "$archive" ] || continue
  archive_count=$((archive_count + 1))
  archive_name="$(basename "$archive")"
  archive_is_expected "$archive_name" || fail "unexpected release archive: ${archive_name}"
  TARGET_NAME="${archive_name%.tar.gz}"
  target_parts="${TARGET_NAME#threadpoint_}"
  target_arch="${target_parts##*_}"
  target_without_arch="${target_parts%_*}"
  target_os="${target_without_arch##*_}"

  expected="$(expected_checksum "$archive_name" || true)"
  [ -n "$expected" ] || fail "checksums.txt missing ${archive_name}"
  actual="$(archive_checksum "$archive")"
  [ "$expected" = "$actual" ] || fail "${archive_name} checksum mismatch"

  require_exact_archive_entries "$archive"

  if [ "$target_os" = "$HOST_OS" ] && [ "$target_arch" = "$HOST_ARCH" ]; then
    run_host_smoke "$archive" "$TARGET_NAME" "$WORK_DIR"
    smoked_count=$((smoked_count + 1))
  fi
done

[ "$archive_count" -gt 0 ] || fail "no threadpoint release archives found in ${DIST_DIR}"
[ "$archive_count" -eq 4 ] || fail "expected exactly 4 v0 release archives, found ${archive_count}"
[ "$smoked_count" -gt 0 ] || fail "no host-compatible archives found for ${HOST_OS}/${HOST_ARCH}"

printf 'release smoke passed: checked %s archive(s), executed %s host-compatible archive(s)\n' "$archive_count" "$smoked_count"
