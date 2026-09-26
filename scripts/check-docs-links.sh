#!/usr/bin/env sh
set -eu

# Check local Markdown links and heading fragments in root documents and docs/,
# and require every top-level docs page in the docs index. Example fixtures and
# code examples are excluded. Requires Python 3; no external URLs are fetched.

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
exec python3 "$ROOT_DIR/scripts/check-docs-links.py" "$ROOT_DIR"
