#!/bin/sh
set -eu

# Keep parsing, metadata validation, and mutation in the Go implementation so
# every uninstall path uses the same strict, bounded, no-follow decoder and
# durable transaction owner. Prefer an executable managed binary. If it is
# missing or cannot execute, an available Go toolchain resolves the latest
# module version through Go's checksum-backed module channel.
if [ -n "${THREADPOINT_HOME:-}" ]; then
  delegate_home=$THREADPOINT_HOME
elif [ -n "${HOME:-}" ]; then
  delegate_home=$HOME/.threadpoint
else
  delegate_home=""
fi

delegate=""
fallback_install_dir=${THREADPOINT_INSTALL_DIR:-}
if [ -z "$fallback_install_dir" ] && [ -n "${HOME:-}" ]; then
  fallback_install_dir=$HOME/.local/bin
fi
if [ -n "$delegate_home" ] && [ -f "$delegate_home/bin/threadpoint" ] &&
  [ ! -L "$delegate_home/bin/threadpoint" ] && [ -x "$delegate_home/bin/threadpoint" ]; then
  delegate=$delegate_home/bin/threadpoint
elif command -v threadpoint >/dev/null 2>&1; then
  delegate=$(command -v threadpoint)
  if [ -z "$fallback_install_dir" ]; then
    fallback_install_dir=${delegate%/*}
  fi
fi

if [ -n "$delegate" ]; then
  if "$delegate" uninstall "$@"; then
    exit 0
  else
    delegate_status=$?
    case "$delegate_status" in
      126 | 127) ;;
      *) exit "$delegate_status" ;;
    esac
  fi
fi

if command -v go >/dev/null 2>&1; then
  if [ -n "$fallback_install_dir" ]; then
    THREADPOINT_INSTALL_DIR=$fallback_install_dir
    export THREADPOINT_INSTALL_DIR
  fi
  exec go run github.com/threadgrid/threadpoint/cmd/threadpoint@latest uninstall "$@"
fi

printf '%s\n' 'error: uninstall requires an executable managed threadpoint binary or an available Go toolchain for the checksum-backed fallback' >&2
exit 1
