#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GATE="$ROOT_DIR/scripts/check-coverage.sh"
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

cat >"$TMP_DIR/exactly-eighty-five.out" <<'EOF'
mode: set
example.go:1.1,1.2 85 1
example.go:2.1,2.2 15 0
EOF

sh "$GATE" "$TMP_DIR/exactly-eighty-five.out"

cat >"$TMP_DIR/below-eighty-five.out" <<'EOF'
mode: set
example.go:1.1,1.2 84 1
example.go:2.1,2.2 16 0
EOF

if sh "$GATE" "$TMP_DIR/below-eighty-five.out"; then
  printf 'coverage gate accepted 84%% coverage\n' >&2
  exit 1
fi

cat >"$TMP_DIR/hidden-package.out" <<'EOF'
mode: count
example/large/ok.go:1.1,1.2 1000 1
example/small/missing.go:1.1,1.2 1 0
EOF
if sh "$GATE" "$TMP_DIR/hidden-package.out"; then
  printf 'coverage gate hid an uncovered package behind module totals\n' >&2
  exit 1
fi
cat >"$TMP_DIR/duplicate-blocks.out" <<'EOF'
mode: atomic
example/pkg/file.go:1.1,1.2 85 0
example/pkg/file.go:1.1,1.2 85 3
example/pkg/file.go:2.1,2.2 15 0
example/pkg/file.go:2.1,2.2 15 0
EOF
sh "$GATE" "$TMP_DIR/duplicate-blocks.out"
printf 'Go coverage gate tests passed\n'
