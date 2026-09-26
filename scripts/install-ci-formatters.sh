#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 1 || -z "$1" ]]; then
  echo "usage: install-ci-formatters.sh BIN_DIR" >&2
  exit 2
fi

bin_dir=$1
tool_root="${bin_dir}.tools"
mkdir -p "${bin_dir}" "${tool_root}"

GOBIN="${bin_dir}" go install mvdan.cc/sh/v3/cmd/shfmt@v3.13.1
cargo install --locked --version 0.10.0 --root "${tool_root}/taplo" taplo-cli
cp "${tool_root}/taplo/bin/taplo" "${bin_dir}/taplo"
python3 -m venv "${tool_root}/ruff"
"${tool_root}/ruff/bin/python" -m pip install --disable-pip-version-check ruff==0.16.1
cp "${tool_root}/ruff/bin/ruff" "${bin_dir}/ruff"

for tool in shfmt taplo ruff; do
  test -x "${bin_dir}/${tool}"
done

if [[ -n "${GITHUB_PATH:-}" ]]; then
  printf '%s\n' "${bin_dir}" >>"${GITHUB_PATH}"
else
  printf '%s\n' "${bin_dir}"
fi
