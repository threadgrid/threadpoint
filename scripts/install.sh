#!/bin/sh
set -eu

repo="threadgrid/threadpoint"
binary="threadpoint"
release_archive_workflow="threadgrid/threadpoint/.github/workflows/release.yml"
release_checksum_workflow="threadgrid/threadpoint/.github/workflows/release-publish.yml"
release_policy_url="${THREADPOINT_RELEASE_POLICY_URL:-https://raw.githubusercontent.com/threadgrid/threadpoint/main/release-policy.json}"

if [ -n "${THREADPOINT_INSTALL_DIR:-}" ]; then
  install_dir=$THREADPOINT_INSTALL_DIR
elif [ -n "${HOME:-}" ]; then
  install_dir=$HOME/.local/bin
else
  install_dir=""
fi

if [ -n "${THREADPOINT_HOME:-}" ]; then
  threadpoint_home=$THREADPOINT_HOME
elif [ -n "${HOME:-}" ]; then
  threadpoint_home=$HOME/.threadpoint
else
  threadpoint_home=""
fi

version="${THREADPOINT_VERSION:-latest}"
skip_attestation="${THREADPOINT_SKIP_ATTESTATION:-0}"
tmpdir=""
install_lock_path=""
install_lock_anchor_path=""
install_lock_guard_path=""
install_lock_guard_open=0
install_lock_guard_helper_pid=""
install_lock_guard_control=""
install_lock_helper_path=""
install_lock_token=""
install_lock_product_home=""
install_lock_command=""
install_lock_created=""
install_lock_heartbeat_pid=""
install_transaction_journal=""
install_transaction_backup=""
install_transaction_backup_identity=""
install_transaction_product_home=""
install_transaction_journal_identity=""
install_transaction_journal_digest=""
max_release_metadata_bytes=262144
max_release_policy_bytes=262144
max_checksums_bytes=1048576
max_signature_bytes=65536
max_archive_bytes=134217728
max_expanded_archive_bytes=268435456
max_expanded_binary_bytes=134217728
max_expanded_support_file_bytes=8388608
max_expanded_bundle_bytes=176160768
if [ "${THREADPOINT_INSTALL_SH_TEST_MODE:-0}" = 1 ] && [ -n "${THREADPOINT_TEST_MAX_EXPANDED_ARCHIVE_BYTES:-}" ]; then
  max_expanded_archive_bytes=$THREADPOINT_TEST_MAX_EXPANDED_ARCHIVE_BYTES
fi

usage() {
  cat <<'USAGE'
Install threadpoint from official GitHub Release artifacts.

Usage:
  curl -fsSL https://raw.githubusercontent.com/threadgrid/threadpoint/main/scripts/install.sh | sh
  curl -fsSL https://raw.githubusercontent.com/threadgrid/threadpoint/main/scripts/install.sh | sh -s -- --version v0.1.0 --dir "$HOME/.local/bin"

Options:
  --dir DIR          Command directory for the threadpoint symlink. Defaults to $HOME/.local/bin.
  --version VERSION  Release tag to install, such as v0.1.0. Defaults to latest.
  --skip-attestation Skip GitHub Artifact Attestation verification.
  -h, --help         Show this help text.

Environment:
  THREADPOINT_INSTALL_DIR  Command directory override.
  THREADPOINT_HOME         Bundle, metadata, update, and local state directory override.
  THREADPOINT_VERSION      Release tag override.
  THREADPOINT_SKIP_ATTESTATION
                           Set to 1 to skip GitHub attestation verification.

The installer supports Linux and macOS on amd64 and arm64. It downloads the
release archive, checksums.txt, and checksums.txt.sig from official GitHub
Releases; verifies the checksum signature, archive checksum, and GitHub
Artifact Attestations; extracts into a temporary workspace; installs the
verified release bundle under THREADPOINT_HOME or $HOME/.threadpoint; and links
the bundled bin/threadpoint binary into the selected command directory.
USAGE
}

info() {
  printf '%s\n' "$*"
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# BEGIN INSTALLER PATH GUIDANCE
# Render a literal path without allowing shell expansion when it is copied.
quote_path_for_shell() {
  printf '%s\n' "$2" | awk -v shell="$1" '
    BEGIN { printf "%c", 39 }
    NR > 1 { printf "\n" }
    {
      for (i = 1; i <= length($0); i++) {
        c = substr($0, i, 1)
        if (c == sprintf("%c", 39)) {
          printf "%c\\%c%c", 39, 39, 39
        } else if (c == "\\" && shell == "fish") {
          printf "\\\\"
        } else {
          printf "%s", c
        }
      }
    }
    END { printf "%c", 39 }
  '
}

# Keep the logical command directory for customer instructions; install ownership
# continues to use the physical directory resolved later in the installer.
prepare_command_dir_for_path() {
  case "$install_dir" in
    /*) command_dir_for_path=$install_dir ;;
    *) command_dir_for_path="$(pwd -L)/$install_dir" ;;
  esac
  case "$command_dir_for_path" in
    *:*) fail "command directory contains a colon and cannot be a PATH entry; choose --dir without colons" ;;
  esac
}

command_dir_on_path() (
  physical_dir=$(CDPATH='' cd -- "$install_dir" 2>/dev/null && pwd -P) || return 1
  remaining_path=${PATH:-}
  while :; do
    case "$remaining_path" in
      *:*)
        path_entry=${remaining_path%%:*}
        remaining_path=${remaining_path#*:}
        more_entries=1
        ;;
      *)
        path_entry=$remaining_path
        more_entries=0
        ;;
    esac
    if [ -n "$path_entry" ]; then
      resolved_entry=$(CDPATH='' cd -- "$path_entry" 2>/dev/null && pwd -P) || resolved_entry=""
      [ "$resolved_entry" != "$physical_dir" ] || return 0
    fi
    [ "$more_entries" = 1 ] || return 1
  done
)

# shellcheck disable=SC2016,SC2088 # Shell setup examples must remain literal.
print_path_guidance() (
  display_dir=${command_dir_for_path:-$install_dir}
  case "$display_dir" in
    *:*)
      warn "command directory contains a colon and cannot be a PATH entry: $display_dir"
      info "Re-run the installer with --dir pointing to a directory without colons."
      return 0
      ;;
  esac
  if command_dir_on_path; then
    return 0
  fi

  warn "$display_dir is not on PATH"

  quoted_dir=$(quote_path_for_shell sh "$display_dir")
  fish_quoted_dir=$(quote_path_for_shell fish "$display_dir")
  info ""
  info "Add the command directory to PATH by running the command for your shell:"
  info ""
  info "Bash / Zsh (current terminal):"
  printf '  export PATH=%s"${PATH:+:$PATH}"\n' "$quoted_dir"
  info "For future Bash terminals, add that line to ~/.bashrc."
  info "For Bash login shells, ensure the first existing file among ~/.bash_profile,"
  info "~/.bash_login, and ~/.profile sources ~/.bashrc, or add the line there."
  info "If none exists, create ~/.bash_profile and add the line there."
  info 'For future Zsh terminals, add that line to ${ZDOTDIR:-$HOME}/.zshrc.'
  info ""
  info "Fish:"
  printf '  fish_add_path %s\n' "$fish_quoted_dir"
  info 'For future Fish terminals, add that line to ~/.config/fish/config.fish'
  info '(or $XDG_CONFIG_HOME/fish/config.fish when XDG_CONFIG_HOME is set).'
  info ""
  info "Run the command now for this terminal; after saving it in your shell"
  info "configuration, new terminals will also have the directory on PATH."
  info "The installer does not edit shell configuration."
  info ""
  info "Then verify:"
  info "  $binary version"
) >&2
# END INSTALLER PATH GUIDANCE

strip_trailing_slashes() {
  path=$1
  while [ "$path" != "/" ] && [ "${path%/}" != "$path" ]; do
    path=${path%/}
  done
  printf '%s' "$path"
}

validate_product_home() {
  product_home=$(strip_trailing_slashes "$1")
  label=$2

  [ -n "$product_home" ] || fail "$label is required"
  case "$product_home" in
    /*) ;;
    *) fail "$label must be absolute: $1" ;;
  esac
  [ "$product_home" != "/" ] || fail "$label cannot be filesystem root"
  if [ -n "${HOME:-}" ]; then
    home_path=$(strip_trailing_slashes "$HOME")
    [ "$product_home" != "$home_path" ] || fail "$label cannot be the home directory"
    [ "$product_home" != "$home_path/.agents" ] || fail "$label cannot be the entire .agents directory"
  fi
  [ ! -L "$product_home" ] || fail "$label cannot be a symlink: $product_home"
  if [ -e "$product_home" ] && [ ! -d "$product_home" ]; then
    fail "$label is not a directory: $product_home"
  fi
}

cleanup() {
  if [ -n "$install_transaction_journal" ] && [ -n "$install_lock_path" ]; then
    recover_install_transaction "$threadpoint_home" "$install_lock_command" ||
      warn "durable install recovery remains pending at $install_transaction_journal"
  fi
  if [ -n "$install_lock_path" ]; then
    release_install_lifecycle_lock || warn "install lifecycle guard release failed; lock state was retained"
  fi
  if [ -n "$tmpdir" ] && [ -d "$tmpdir" ]; then
    rm -rf "$tmpdir"
  fi
}

canonical_install_child_path() {
  child_path=$1
  case "$child_path" in
    /*) ;;
    *) child_path=$PWD/$child_path ;;
  esac
  child_base=${child_path##*/}
  child_parent=${child_path%/*}
  [ -n "$child_base" ] && [ "$child_base" != . ] && [ "$child_base" != .. ] || return 1
  child_parent=$(CDPATH='' cd -- "$child_parent" && pwd -P) || return 1
  printf '%s/%s' "$child_parent" "$child_base"
}

canonical_install_command_path() {
  requested_path=$1
  case "$requested_path" in
    /*) ;;
    *) return 1 ;;
  esac
  canonical_path=$(canonical_install_child_path "$requested_path") || return 1
  [ "$canonical_path" = "$requested_path" ] || return 1
  printf '%s' "$canonical_path"
}

install_helper_path() {
  if [ -n "$install_lock_helper_path" ]; then
    printf '%s' "$install_lock_helper_path"
  elif [ "${THREADPOINT_INSTALL_SH_TEST_MODE:-0}" = 1 ] && [ -n "${THREADPOINT_INSTALL_LOCK_HELPER:-}" ]; then
    printf '%s' "$THREADPOINT_INSTALL_LOCK_HELPER"
  else
    return 1
  fi
}

install_atomic_move_noreplace() {
  move_source=$(canonical_install_child_path "$1") || return 1
  move_target=$(canonical_install_child_path "$2") || return 1
  move_helper=$(install_helper_path) || return 1
  "$move_helper" __threadpoint-install-no-replace "$move_source" "$move_target" 8>&-
}

install_entry_identity_helper() {
  identity_path=$(canonical_install_child_path "$1") || return 1
  identity_helper=$(install_helper_path) || return 1
  identity_result=$("$identity_helper" __threadpoint-install-entry-identity "$identity_path" 8>&-) || return 1
  case "$identity_result" in
    *[!0-9:]* | *:*:*) return 1 ;;
    [0-9]*:[0-9]*) printf '%s' "$identity_result" ;;
    *) return 1 ;;
  esac
}

install_atomic_move_noreplace_as() {
  move_source=$(canonical_install_child_path "$1") || return 1
  move_target=$(canonical_install_child_path "$2") || return 1
  move_identity=$3
  move_helper=$(install_helper_path) || return 1
  "$move_helper" __threadpoint-install-no-replace-as "$move_source" "$move_target" "$move_identity" 8>&-
}

install_entry_inspect_with_helper() {
  inspect_helper=$1
  inspect_path=$(canonical_install_child_path "$2") || return 1
  [ -f "$inspect_helper" ] && [ ! -L "$inspect_helper" ] && [ -x "$inspect_helper" ] || return 1
  inspect_result=$("$inspect_helper" __threadpoint-install-entry-inspect "$inspect_path" 8>&-) || return 1
  # shellcheck disable=SC2086 # the helper emits three restricted tokens, validated immediately below.
  set -- $inspect_result
  [ "$#" -eq 3 ] || return 1
  case "$1" in *[!0-9:]* | *:*:* | :* | *:) return 1 ;; esac
  case "$1" in [0-9]*:[0-9]*) ;; *) return 1 ;; esac
  case "$2" in regular | symlink) [ "${#3}" -eq 64 ] || return 1 ;; directory) [ "$3" = - ] || return 1 ;; *) return 1 ;; esac
  case "$3" in -) ;; *[!0-9a-f]*) return 1 ;; esac
  printf '%s %s %s\n' "$1" "$2" "$3"
}

install_entry_inspect_helper() {
  inspect_helper=$(install_helper_path) || return 1
  install_entry_inspect_with_helper "$inspect_helper" "$1"
}

install_regular_digest_helper() {
  regular_inspection=$(install_entry_inspect_helper "$1") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $regular_inspection
  [ "$2" = regular ] && [ "${#3}" -eq 64 ] || return 1
  printf '%s\n' "$3"
}

install_regular_snapshot_helper() {
  snapshot_path=$(canonical_install_child_path "$1") || return 1
  snapshot_helper=$(install_helper_path) || return 1
  snapshot_result=$("$snapshot_helper" __threadpoint-install-regular-snapshot "$snapshot_path" 8>&-) || return 1
  # shellcheck disable=SC2086 # the helper emits three restricted tokens.
  set -- $snapshot_result
  [ "$#" -eq 3 ] && [ "${#2}" -eq 64 ] || return 1
  case "$1" in *[!0-9:]* | *:*:* | :* | *:) return 1 ;; esac
  case "$1" in [0-9]*:[0-9]*) ;; *) return 1 ;; esac
  case "$2" in *[!0-9a-f]*) return 1 ;; esac
  case "$3" in '' | *[!0-9]*) return 1 ;; esac
  printf '%s %s %s\n' "$1" "$2" "$3"
}

install_lock_owner_helper() {
  owner_path=$(canonical_install_child_path "$1") || return 1
  owner_helper=$(install_helper_path) || return 1
  owner_result=$("$owner_helper" __threadpoint-install-lock-owner "$owner_path" 8>&-) || return 1
  # shellcheck disable=SC2086 # the helper emits three restricted tokens, validated below.
  set -- $owner_result
  [ "$#" -eq 3 ] || return 1
  case "$1" in *[!0-9:]* | *:*:* | :* | *:) return 1 ;; esac
  case "$1" in [0-9]*:[0-9]*) ;; *) return 1 ;; esac
  case "$2" in '' | *[!0-9]*) return 1 ;; esac
  case "$3" in *[!0-9a-f]*) return 1 ;; esac
  [ "${#3}" -eq 32 ] || [ "${#3}" -eq 64 ] || return 1
  printf '%s %s %s\n' "$1" "$2" "$3"
}

install_lock_heartbeat_helper() {
  heartbeat_path=$(canonical_install_child_path "$1") || return 1
  heartbeat_helper=$(install_helper_path) || return 1
  "$heartbeat_helper" __threadpoint-install-lock-heartbeat \
    "$heartbeat_path" "$2" "$3" "$4" 8>&-
}

install_directory_identity_helper() {
  directory_inspection=$(install_entry_inspect_helper "$1") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $directory_inspection
  [ "$2" = directory ] && [ "$3" = - ] || return 1
  printf '%s\n' "$1"
}

install_remove_entry_as() {
  remove_path=$(canonical_install_child_path "$1") || return 1
  remove_helper=$(install_helper_path) || return 1
  "$remove_helper" __threadpoint-install-remove-as "$remove_path" "$2" "$3" "$4" 8>&-
}

install_recovery_cleanup_as() {
  recovery_cleanup_path=$(canonical_install_child_path "$1") || return 1
  recovery_cleanup_identity=$2
  case "$recovery_cleanup_identity" in *[!0-9:]* | *:*:* | :* | *:) return 1 ;; esac
  case "$recovery_cleanup_identity" in [0-9]*:[0-9]*) ;; *) return 1 ;; esac
  recovery_cleanup_helper=$(install_helper_path) || return 1
  "$recovery_cleanup_helper" __threadpoint-install-recovery-cleanup \
    "$recovery_cleanup_path" "$recovery_cleanup_identity" 8>&-
}

install_copy_bounded_as() {
  install_copy_bounded_generation_as "$1" "$2" "" 0600 >/dev/null
}

install_copy_bounded_expected_as() {
  expected_copy_digest=$3
  expected_copy_mode=$4
  [ "${#expected_copy_digest}" -eq 64 ] || return 1
  case "$expected_copy_digest" in *[!0-9a-f]*) return 1 ;; esac
  install_copy_bounded_generation_as "$1" "$2" "$expected_copy_digest" "$expected_copy_mode"
}

install_copy_bounded_expected_generation_as() {
  copy_source=$(canonical_install_child_path "$1") || return 1
  copy_destination=$(canonical_install_child_path "$2") || return 1
  copy_expected_identity=$3
  copy_expected_digest=$4
  copy_mode=$5
  case "$copy_expected_identity" in *[!0-9:]* | *:*:* | :* | *:) return 1 ;; esac
  case "$copy_expected_identity" in [0-9]*:[0-9]*) ;; *) return 1 ;; esac
  [ "${#copy_expected_digest}" -eq 64 ] || return 1
  case "$copy_expected_digest" in *[!0-9a-f]*) return 1 ;; esac
  case "$copy_mode" in 0600 | 0644 | 0755) ;; *) return 1 ;; esac
  copy_helper=$(install_helper_path) || return 1
  copy_result=$("$copy_helper" __threadpoint-install-copy-bounded-as \
    "$copy_source" "$copy_destination" "$copy_expected_identity" "$copy_expected_digest" "$copy_mode" 8>&-) || return 1
  # shellcheck disable=SC2086 # the helper emits two restricted tokens.
  set -- $copy_result
  [ "$#" -eq 2 ] && [ "$2" = "$copy_expected_digest" ] || return 1
  case "$1" in *[!0-9:]* | *:*:* | :* | *:) return 1 ;; esac
  case "$1" in [0-9]*:[0-9]*) ;; *) return 1 ;; esac
  printf '%s %s\n' "$1" "$2"
}

install_copy_bounded_generation_as() {
  copy_source=$(canonical_install_child_path "$1") || return 1
  copy_destination=$(canonical_install_child_path "$2") || return 1
  copy_expected_digest=$3
  copy_mode=$4
  case "$copy_mode" in 0600 | 0644 | 0755) ;; *) return 1 ;; esac
  copy_inspection=$(install_entry_inspect_helper "$copy_source") || return 1
  # shellcheck disable=SC2086 # the helper emits three restricted tokens.
  set -- $copy_inspection
  [ "$2" = regular ] && [ "${#3}" -eq 64 ] || return 1
  if [ -n "$copy_expected_digest" ]; then
    [ "$3" = "$copy_expected_digest" ] || return 1
  else
    copy_expected_digest=$3
  fi
  copy_helper=$(install_helper_path) || return 1
  copy_result=$("$copy_helper" __threadpoint-install-copy-bounded-as \
    "$copy_source" "$copy_destination" "$1" "$copy_expected_digest" "$copy_mode" 8>&-) || return 1
  # shellcheck disable=SC2086 # the helper emits two restricted tokens.
  set -- $copy_result
  [ "$#" -eq 2 ] && [ "$2" = "$copy_expected_digest" ] || return 1
  case "$1" in *[!0-9:]* | *:*:* | :* | *:) return 1 ;; esac
  case "$1" in [0-9]*:[0-9]*) ;; *) return 1 ;; esac
  printf '%s %s\n' "$1" "$2"
}

install_write_bounded_helper() {
  write_path=$(canonical_install_child_path "$1") || return 1
  write_mode=$2
  case "$write_mode" in 0600 | 0644 | 0700 | 0755) ;; *) return 1 ;; esac
  write_helper=$(install_helper_path) || return 1
  write_result=$("$write_helper" __threadpoint-install-write-bounded "$write_path" "$write_mode" 8>&-) || return 1
  # shellcheck disable=SC2086 # the helper emits two restricted tokens.
  set -- $write_result
  [ "$#" -eq 2 ] && [ "${#2}" -eq 64 ] || return 1
  case "$1" in *[!0-9:]* | *:*:* | :* | *:) return 1 ;; esac
  case "$1" in [0-9]*:[0-9]*) ;; *) return 1 ;; esac
  case "$2" in *[!0-9a-f]*) return 1 ;; esac
  printf '%s %s\n' "$1" "$2"
}

discard_install_written_generation() {
  discard_path=$1
  discard_generation=$2
  # shellcheck disable=SC2086 # the bounded writer emits two restricted tokens.
  set -- $discard_generation
  [ "$#" -eq 2 ] || return 1
  install_remove_entry_as "$discard_path" "$1" regular "$2"
}

install_journal_helper() {
  journal_file=$1
  journal_digest=$2
  shift 2
  journal_helper=$(install_helper_path) || return 1
  "$journal_helper" __threadpoint-install-journal "$journal_file" "$journal_digest" "$@" 8>&-
}

inspect_install_journal() {
  journal_file=$1
  expected_journal=$2
  journal_helper=$(install_helper_path) || return 1
  "$journal_helper" __threadpoint-install-journal-inspect "$journal_file" "$expected_journal" 8>&-
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

download() {
  url=$1
  output=$2
  max_bytes=$3

  case "$max_bytes" in
    "" | *[!0-9]* | 0) fail "download byte limit must be a positive integer" ;;
  esac
  download_temp=$(mktemp "$output.download.XXXXXX") || fail "could not create private download temporary"
  chmod 0600 "$download_temp" || fail "could not protect private download temporary"
  file_blocks=$(((max_bytes + 511) / 512))

  if command -v curl >/dev/null 2>&1; then
    if ! (ulimit -f "$file_blocks" && curl -fsSL --max-filesize "$max_bytes" "$url" -o "$download_temp"); then
      rm -f "$download_temp"
      fail "download failed or exceeded the $max_bytes byte limit: $url"
    fi
  elif command -v wget >/dev/null 2>&1; then
    if ! (ulimit -f "$file_blocks" && wget -q "$url" -O "$download_temp"); then
      rm -f "$download_temp"
      fail "download failed or exceeded the $max_bytes byte limit: $url"
    fi
  else
    rm -f "$download_temp"
    fail "required command not found: curl or wget"
  fi
  actual_bytes=$(wc -c <"$download_temp" | tr -d '[:space:]')
  if [ -z "$actual_bytes" ] || [ "$actual_bytes" -gt "$max_bytes" ]; then
    rm -f "$download_temp"
    fail "download exceeded the $max_bytes byte limit: $url"
  fi
  mv "$download_temp" "$output" || fail "could not publish bounded download: $output"
}

detect_os() {
  case "$(uname -s)" in
    Linux) printf 'linux' ;;
    Darwin) printf 'darwin' ;;
    *) fail "unsupported operating system: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) printf 'amd64' ;;
    arm64 | aarch64) printf 'arm64' ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
  esac
}

validate_version() {
  case "$version" in
    "") fail "version must not be empty" ;;
    latest) return 0 ;;
  esac

  case "$version" in
    *[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-]*)
      fail "version must be latest or a release tag like v0.1.0"
      ;;
  esac

  case "$version" in
    v*.*.*) return 0 ;;
    *) fail "version must be latest or a release tag like v0.1.0" ;;
  esac
}

resolve_latest_version() {
  latest_file=$tmpdir/latest-release.json
  download "https://api.github.com/repos/${repo}/releases/latest" "$latest_file" "$max_release_metadata_bytes"
  tag=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$latest_file" | sed -n '1p')
  [ -n "$tag" ] || fail "could not resolve latest release tag"
  printf '%s' "$tag"
}

archive_version_from_tag() {
  tag=$1
  case "$tag" in
    v*) printf '%s' "${tag#v}" ;;
    *) printf '%s' "$tag" ;;
  esac
}

json_string_value() {
  json_file=$1
  json_key=$2

  sed -n 's/^[[:space:]]*"'"$json_key"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$json_file" | sed -n '1p'
}

json_string_array_contains() {
  json_file=$1
  json_key=$2
  expected=$3

  awk -v key="$json_key" -v expected="$expected" '
		BEGIN {
			in_array = 0
			found = 0
		}
		{
			line = $0
			if (!in_array) {
				needle = "\"" key "\""
				key_pos = index(line, needle)
				if (key_pos == 0) {
					next
				}
				line = substr(line, key_pos + length(needle))
				bracket_pos = index(line, "[")
				if (bracket_pos == 0) {
					next
				}
				line = substr(line, bracket_pos + 1)
				in_array = 1
			}
			close_pos = index(line, "]")
			if (close_pos > 0) {
				line = substr(line, 1, close_pos - 1)
				in_array = 0
			}
			while (match(line, /"[^"]+"/)) {
				value = substr(line, RSTART + 1, RLENGTH - 2)
				if (value == expected) {
					found = 1
				}
				line = substr(line, RSTART + RLENGTH)
			}
		}
		END {
			if (found) {
				exit 0
			}
			exit 1
		}
	' "$json_file"
}

semver_less_than() {
  left=$1
  right=$2

  left_raw=${left#v}
  right_raw=${right#v}
  left_core=${left_raw%%-*}
  right_core=${right_raw%%-*}
  left_major=${left_core%%.*}
  left_rest=${left_core#*.}
  left_minor=${left_rest%%.*}
  left_patch=${left_rest#*.}
  right_major=${right_core%%.*}
  right_rest=${right_core#*.}
  right_minor=${right_rest%%.*}
  right_patch=${right_rest#*.}

  case "$left_major:$left_minor:$left_patch:$right_major:$right_minor:$right_patch" in
    *[!0123456789:]* | *::* | :* | *:) fail "release policy contains an invalid semver value" ;;
  esac

  [ "$left_major" -lt "$right_major" ] && return 0
  [ "$left_major" -gt "$right_major" ] && return 1
  [ "$left_minor" -lt "$right_minor" ] && return 0
  [ "$left_minor" -gt "$right_minor" ] && return 1
  [ "$left_patch" -lt "$right_patch" ] && return 0
  [ "$left_patch" -gt "$right_patch" ] && return 1

  if [ "$left_raw" != "$left_core" ] && [ "$right_raw" = "$right_core" ]; then
    return 0
  fi
  return 1
}

enforce_release_policy() {
  target_version=$1
  policy_path=$tmpdir/release-policy.json

  info "Checking threadpoint release policy"
  download "$release_policy_url" "$policy_path" "$max_release_policy_bytes"

  schema_version=$(json_string_value "$policy_path" "schema_version")
  product=$(json_string_value "$policy_path" "product")
  minimum_version=$(json_string_value "$policy_path" "minimum_install_version")
  replacement_version=$(json_string_value "$policy_path" "replacement_version")
  [ "$schema_version" = "threadpoint.release_policy.v1" ] || fail "release policy has an unexpected schema"
  [ "$product" = "threadpoint" ] || fail "release policy has an unexpected product"
  [ -n "$minimum_version" ] || fail "release policy is missing minimum_install_version"
  if [ -z "$replacement_version" ]; then
    replacement_version=$minimum_version
  fi

  if semver_less_than "$target_version" "$minimum_version"; then
    fail "threadpoint $target_version is no longer supported by the official installer; install $replacement_version or any release at or after $minimum_version"
  fi
  if json_string_array_contains "$policy_path" "blocked_versions" "$target_version"; then
    fail "threadpoint $target_version is blocked by the official installer release policy; install $replacement_version or a newer supported release"
  fi
}

checksum_line_for_archive() {
  checksums_file=$1
  archive_name=$2

  awk -v name="$archive_name" '
		$2 == name {
			print
			found = 1
			exit
		}
		END {
			if (!found) {
				exit 1
			}
		}
	' "$checksums_file"
}

verify_checksum() {
  archive_path=$1
  expected=$2
  archive_name=$3
  actual=$(sha256_file "$archive_path") || fail "could not hash $archive_name"
  [ "$expected" = "$actual" ] || fail "checksum verification failed for $archive_name"
}

base64_decode() {
  input=$1
  output=$2

  if printf '%s' "$input" | base64 -d >"$output" 2>/dev/null; then
    return 0
  fi
  if printf '%s' "$input" | base64 -D >"$output" 2>/dev/null; then
    return 0
  fi
  fail "could not decode checksum signature"
}

verify_checksum_signature() {
  checksums_file=$1
  signature_file=$2
  public_key=$tmpdir/release-signing-public-key.pem
  signature_raw=$tmpdir/checksums.txt.sig.raw

  need_cmd openssl
  need_cmd base64

  signature_body=$(cat "$signature_file") || fail "could not read retained checksum signature"
  printf '%s\n' "$signature_body" | grep -Fq '"schema_version": "threadpoint.release_signature.v1"' ||
    fail "checksums.txt.sig missing threadpoint release signature schema"
  printf '%s\n' "$signature_body" | grep -Fq '"algorithm": "ed25519"' ||
    fail "checksums.txt.sig must use Ed25519"
  printf '%s\n' "$signature_body" | grep -Fq '"key_id": "release-2026-10-09"' ||
    fail "checksums.txt.sig has an unexpected key id"

  sig=$(printf '%s\n' "$signature_body" |
    sed -n 's/^[[:space:]]*"signature":[[:space:]]*"\([^"]*\)".*/\1/p' | sed -n '1p')
  [ -n "$sig" ] || fail "checksums.txt.sig is missing signature material"
  base64_decode "$sig" "$signature_raw"

  cat >"$public_key" <<'KEY'
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEACW5cmRTXsxvwR+XlHxCY6y8m3DvqzV7OosYi59hv9CA=
-----END PUBLIC KEY-----
KEY

  openssl pkeyutl -verify -rawin -pubin -inkey "$public_key" -sigfile "$signature_raw" -in "$checksums_file" >/dev/null 2>&1 ||
    fail "checksum signature verification failed"
}

verify_attestations() {
  archive_path=$1
  checksums_file=$2
  version_tag=$3

  if [ "$skip_attestation" = "1" ]; then
    warn "skipping GitHub Artifact Attestation verification"
    return
  fi

  need_cmd gh
  gh attestation verify "$archive_path" \
    -R "$repo" \
    --signer-repo "$repo" \
    --signer-workflow "$release_archive_workflow" \
    --source-ref "refs/tags/${version_tag}" >/dev/null
  gh attestation verify "$checksums_file" \
    -R "$repo" \
    --signer-repo "$repo" \
    --signer-workflow "$release_checksum_workflow" \
    --source-ref "refs/heads/main" >/dev/null
}

sha256_file() {
  path=$1

  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$path" | awk '{ print $1 }'
  else
    fail "required command not found: sha256sum or shasum"
  fi
}

regular_file_size() {
  size_path=$1
  if size_value=$(stat -Lf '%z' "$size_path" 2>/dev/null); then
    printf '%s' "$size_value"
  else
    stat -L -c '%s' -- "$size_path" 2>/dev/null
  fi
}

sha256_file_bounded() {
  bounded_hash_path=$1
  bounded_hash_limit=$2
  case "$bounded_hash_limit" in
    '' | *[!0-9]* | 0) return 1 ;;
  esac
  bounded_hash_size=$(regular_file_size "$bounded_hash_path") || return 1
  case "$bounded_hash_size" in
    '' | *[!0-9]*) return 1 ;;
  esac
  [ "$bounded_hash_size" -le "$bounded_hash_limit" ] || return 1

  # Read at most one byte beyond the admitted size. The post-read descriptor
  # stat and the caller's expected digest reject in-place growth or rewrites,
  # while the finite head limit prevents hashing from becoming unbounded.
  bounded_hash_bytes=$((bounded_hash_limit + 1))
  if command -v sha256sum >/dev/null 2>&1; then
    bounded_hash_digest=$(head -c "$bounded_hash_bytes" "$bounded_hash_path" |
      sha256sum | awk '{ print $1 }') || return 1
  elif command -v shasum >/dev/null 2>&1; then
    bounded_hash_digest=$(head -c "$bounded_hash_bytes" "$bounded_hash_path" |
      shasum -a 256 | awk '{ print $1 }') || return 1
  else
    return 1
  fi
  bounded_hash_finished_size=$(regular_file_size "$bounded_hash_path") || return 1
  [ "$bounded_hash_finished_size" = "$bounded_hash_size" ] || return 1
  printf '%s\n' "$bounded_hash_digest"
}

sha256_expanded_archive() {
  compressed_archive_path=$1
  expanded_hash_bytes=$((max_expanded_archive_bytes + 1))
  if command -v sha256sum >/dev/null 2>&1; then
    gzip -dc "$compressed_archive_path" |
      head -c "$expanded_hash_bytes" |
      sha256sum | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    gzip -dc "$compressed_archive_path" |
      head -c "$expanded_hash_bytes" |
      shasum -a 256 | awk '{ print $1 }'
  else
    fail "required command not found: sha256sum or shasum"
  fi
}

install_entry_identity() {
  identity_path=$1
  if identity_value=$(stat -f '%d:%i' "$identity_path" 2>/dev/null); then
    printf '%s' "$identity_value"
  else
    stat -c '%d:%i' -- "$identity_path" 2>/dev/null
  fi
}

retained_regular_identity() {
  identity_path=$1
  if identity_value=$(stat -Lf '%d:%i' "$identity_path" 2>/dev/null); then
    printf '%s' "$identity_value"
  else
    stat -L -c '%d:%i' -- "$identity_path" 2>/dev/null
  fi
}

sha256_text() {
  value=$1

  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "$value" | sha256sum | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    printf '%s' "$value" | shasum -a 256 | awk '{ print $1 }'
  else
    fail "required command not found: sha256sum or shasum"
  fi
}

archive_bundle_dir() {
  archive_path=$1
  archive_name=${archive_path##*/}
  case "$archive_name" in
    *.tar.gz) printf '%s' "${archive_name%.tar.gz}" ;;
    *) fail "unsupported archive name: $archive_name" ;;
  esac
}

prepare_bounded_archive() {
  archive_path=$1
  expanded_path=$(mktemp "$tmpdir/archive.expanded.XXXXXX") || fail "could not create private expanded archive"
  chmod 0600 "$expanded_path" || {
    rm -f "$expanded_path"
    fail "could not protect private expanded archive"
  }
  expanded_blocks=$(((max_expanded_archive_bytes + 511) / 512))
  (
    ulimit -f "$expanded_blocks"
    gzip -dc "$archive_path" >"$expanded_path"
  ) || {
    rm -f "$expanded_path"
    fail "release archive exceeds the expanded-byte limit or is not valid gzip"
  }
  expanded_size=$(wc -c <"$expanded_path" | tr -d '[:space:]')
  case "$expanded_size" in
    '' | *[!0-9]*) fail "could not measure expanded release archive" ;;
  esac
  [ "$expanded_size" -le "$max_expanded_archive_bytes" ] || {
    rm -f "$expanded_path"
    fail "release archive exceeds the $max_expanded_archive_bytes-byte expanded limit"
  }
  printf '%s' "$expanded_path"
}

close_release_metadata_descriptors() {
  exec 3<&- 4<&- 5<&- 6<&- 7<&- 9<&- || true
}

retain_release_metadata() {
  checksums_path=$1
  signature_path=$2
  for metadata_path in "$checksums_path" "$signature_path"; do
    case "$metadata_path" in
      "$tmpdir"/*) ;;
      *) fail "release metadata is outside the private installer workspace" ;;
    esac
    [ -f "$metadata_path" ] && [ ! -L "$metadata_path" ] || fail "release metadata is not a regular file"
  done

  checksums_before=$(retained_regular_identity "$checksums_path") || fail "could not identify checksum manifest"
  signature_before=$(retained_regular_identity "$signature_path") || fail "could not identify checksum signature"
  exec 3<"$signature_path" 4<"$checksums_path" 5<"$checksums_path" 6<"$checksums_path" \
    7<"$checksums_path" 9<"$checksums_path" || {
    close_release_metadata_descriptors
    fail "could not retain release metadata"
  }
  signature_opened=$(retained_regular_identity /dev/fd/3) || {
    close_release_metadata_descriptors
    fail "could not identify retained checksum signature"
  }
  for checksums_descriptor in /dev/fd/4 /dev/fd/5 /dev/fd/6 /dev/fd/7 /dev/fd/9; do
    [ -f "$checksums_descriptor" ] || {
      close_release_metadata_descriptors
      fail "platform does not expose retained checksum descriptors"
    }
    checksums_opened=$(retained_regular_identity "$checksums_descriptor") || {
      close_release_metadata_descriptors
      fail "could not identify retained checksum manifest"
    }
    [ "$checksums_opened" = "$checksums_before" ] || {
      close_release_metadata_descriptors
      fail "checksum manifest changed while it was retained"
    }
  done
  checksums_after=$(retained_regular_identity "$checksums_path") || {
    close_release_metadata_descriptors
    fail "checksum manifest changed while it was retained"
  }
  signature_after=$(retained_regular_identity "$signature_path") || {
    close_release_metadata_descriptors
    fail "checksum signature changed while it was retained"
  }
  [ "$checksums_after" = "$checksums_before" ] &&
    [ "$signature_opened" = "$signature_before" ] && [ "$signature_after" = "$signature_before" ] || {
    close_release_metadata_descriptors
    fail "release metadata changed while it was retained"
  }
  rm -f "$checksums_path" "$signature_path" || {
    close_release_metadata_descriptors
    fail "could not detach retained release metadata pathnames"
  }
  [ ! -e "$checksums_path" ] && [ ! -L "$checksums_path" ] &&
    [ ! -e "$signature_path" ] && [ ! -L "$signature_path" ] || {
    close_release_metadata_descriptors
    fail "release metadata pathname changed while it was detached"
  }
}

close_release_archive_descriptors() {
  exec 3<&- 4<&- 5<&- 7<&- 8<&- || true
}

retain_release_archive() {
  archive_path=$1
  case "$archive_path" in
    "$tmpdir"/*) ;;
    *) fail "release archive is outside the private installer workspace" ;;
  esac
  [ -f "$archive_path" ] && [ ! -L "$archive_path" ] || fail "release archive is not a regular file"
  archive_before=$(retained_regular_identity "$archive_path") || fail "could not identify release archive"
  exec 3<"$archive_path" 4<"$archive_path" 5<"$archive_path" 7<"$archive_path" \
    8<"$archive_path" || {
    close_release_archive_descriptors
    fail "could not retain release archive"
  }
  for archive_descriptor in /dev/fd/3 /dev/fd/4 /dev/fd/5 /dev/fd/7 /dev/fd/8; do
    [ -f "$archive_descriptor" ] || {
      close_release_archive_descriptors
      fail "platform does not expose retained release archive descriptors"
    }
    archive_opened=$(retained_regular_identity "$archive_descriptor") || {
      close_release_archive_descriptors
      fail "could not identify retained release archive"
    }
    [ "$archive_opened" = "$archive_before" ] || {
      close_release_archive_descriptors
      fail "release archive changed while it was retained"
    }
  done
  archive_after=$(retained_regular_identity "$archive_path") || {
    close_release_archive_descriptors
    fail "release archive changed while it was retained"
  }
  [ "$archive_after" = "$archive_before" ] || {
    close_release_archive_descriptors
    fail "release archive changed while it was retained"
  }
  rm -f "$archive_path" || {
    close_release_archive_descriptors
    fail "could not detach retained release archive pathname"
  }
  [ ! -e "$archive_path" ] && [ ! -L "$archive_path" ] || {
    close_release_archive_descriptors
    fail "release archive pathname changed while it was detached"
  }
}

close_prepared_archive_descriptors() {
  exec 3<&- 4<&- 5<&- 6<&- 7<&- 9<&- || true
}

retain_prepared_archive() {
  prepared_path=$1
  case "$prepared_path" in
    "$tmpdir"/*) ;;
    *) fail "prepared archive is outside the private installer workspace" ;;
  esac
  [ -f "$prepared_path" ] && [ ! -L "$prepared_path" ] || fail "prepared archive is not a regular file"
  prepared_before=$(retained_regular_identity "$prepared_path") || fail "could not identify prepared archive"
  exec 3<"$prepared_path" 4<"$prepared_path" 5<"$prepared_path" 6<"$prepared_path" \
    7<"$prepared_path" 9<"$prepared_path" || {
    close_prepared_archive_descriptors
    fail "could not retain prepared archive"
  }
  for prepared_descriptor in /dev/fd/3 /dev/fd/4 /dev/fd/5 /dev/fd/6 /dev/fd/7 /dev/fd/9; do
    [ -f "$prepared_descriptor" ] || {
      close_prepared_archive_descriptors
      fail "platform does not expose retained prepared archive descriptors"
    }
    prepared_opened=$(retained_regular_identity "$prepared_descriptor") || {
      close_prepared_archive_descriptors
      fail "could not identify retained prepared archive"
    }
    [ "$prepared_opened" = "$prepared_before" ] || {
      close_prepared_archive_descriptors
      fail "prepared archive changed while it was retained"
    }
  done
  prepared_after=$(retained_regular_identity "$prepared_path") || {
    close_prepared_archive_descriptors
    fail "prepared archive changed while it was retained"
  }
  [ "$prepared_after" = "$prepared_before" ] || {
    close_prepared_archive_descriptors
    fail "prepared archive changed while it was retained"
  }
  rm -f "$prepared_path" || {
    close_prepared_archive_descriptors
    fail "could not detach retained prepared archive pathname"
  }
  [ ! -e "$prepared_path" ] && [ ! -L "$prepared_path" ] || {
    close_prepared_archive_descriptors
    fail "prepared archive pathname changed while it was detached"
  }
}

bundle_entries() {
  cat <<'ENTRIES'
bin/threadpoint
README.md
LICENSE
NOTICE
scripts/install.sh
scripts/uninstall.sh
ENTRIES
}

bundle_entry_mode() {
  case "$1" in
    bin/threadpoint | scripts/install.sh | scripts/uninstall.sh) printf '0755' ;;
    *) printf '0644' ;;
  esac
}

snapshot_extracted_bundle_generations() {
  extracted_root=$1
  verified_root=$2
  extracted_aggregate_size=0

  for entry in $(bundle_entries); do
    extracted_inspection=$(install_regular_snapshot_helper "$extracted_root/$entry") || return 1
    verified_inspection=$(install_regular_snapshot_helper "$verified_root/$entry") || return 1
    # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
    set -- $extracted_inspection
    [ "$#" -eq 3 ] || return 1
    extracted_identity=$1
    extracted_digest=$2
    extracted_size=$3
    # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
    set -- $verified_inspection
    [ "$#" -eq 3 ] && [ "$2" = "$extracted_digest" ] && [ "$3" = "$extracted_size" ] || return 1

    case "$entry" in
      bin/threadpoint) extracted_limit=$max_expanded_binary_bytes ;;
      *) extracted_limit=$max_expanded_support_file_bytes ;;
    esac
    [ "$extracted_size" -le "$extracted_limit" ] || return 1
    extracted_aggregate_size=$((extracted_aggregate_size + extracted_size))
    [ "$extracted_aggregate_size" -le "$max_expanded_bundle_bytes" ] || return 1

    case "$entry" in
      bin/threadpoint)
        expected_bin_threadpoint_identity=$extracted_identity
        expected_bin_threadpoint_digest=$extracted_digest
        ;;
      README.md)
        expected_readme_identity=$extracted_identity
        expected_readme_digest=$extracted_digest
        ;;
      LICENSE)
        expected_license_identity=$extracted_identity
        expected_license_digest=$extracted_digest
        ;;
      NOTICE)
        expected_notice_identity=$extracted_identity
        expected_notice_digest=$extracted_digest
        ;;
      scripts/install.sh)
        expected_install_script_identity=$extracted_identity
        expected_install_script_digest=$extracted_digest
        ;;
      scripts/uninstall.sh)
        expected_uninstall_script_identity=$extracted_identity
        expected_uninstall_script_digest=$extracted_digest
        ;;
      *) return 1 ;;
    esac
  done
}

expected_extracted_bundle_generation() {
  case "$1" in
    bin/threadpoint) printf '%s %s\n' "${expected_bin_threadpoint_identity:-}" "${expected_bin_threadpoint_digest:-}" ;;
    README.md) printf '%s %s\n' "${expected_readme_identity:-}" "${expected_readme_digest:-}" ;;
    LICENSE) printf '%s %s\n' "${expected_license_identity:-}" "${expected_license_digest:-}" ;;
    NOTICE) printf '%s %s\n' "${expected_notice_identity:-}" "${expected_notice_digest:-}" ;;
    scripts/install.sh) printf '%s %s\n' "${expected_install_script_identity:-}" "${expected_install_script_digest:-}" ;;
    scripts/uninstall.sh) printf '%s %s\n' "${expected_uninstall_script_identity:-}" "${expected_uninstall_script_digest:-}" ;;
    *) return 1 ;;
  esac
}

retain_bootstrap_helper() {
  bootstrap_path=$1
  [ -f "$bootstrap_path" ] && [ ! -L "$bootstrap_path" ] && [ -x "$bootstrap_path" ] || return 1
  bootstrap_before=$(retained_regular_identity "$bootstrap_path") || return 1
  exec 3<"$bootstrap_path" || return 1
  [ -f /dev/fd/3 ] && [ -x /dev/fd/3 ] || {
    exec 3<&-
    return 1
  }
  bootstrap_opened=$(retained_regular_identity /dev/fd/3) || {
    exec 3<&-
    return 1
  }
  bootstrap_after=$(retained_regular_identity "$bootstrap_path") || {
    exec 3<&-
    return 1
  }
  [ "$bootstrap_before" = "$bootstrap_opened" ] && [ "$bootstrap_opened" = "$bootstrap_after" ] || {
    exec 3<&-
    return 1
  }
  bootstrap_helper_executable=/dev/fd/3
  bootstrap_snapshot=$("$bootstrap_helper_executable" __threadpoint-install-regular-snapshot "$bootstrap_path" 8>&-) || {
    exec 3<&-
    return 1
  }
  # shellcheck disable=SC2086 # the pinned helper emits three restricted tokens.
  set -- $bootstrap_snapshot
  [ "$#" -eq 3 ] && [ "$1" = "$bootstrap_opened" ] && [ "${#2}" -eq 64 ] || {
    exec 3<&-
    return 1
  }
  case "$3" in '' | *[!0-9]*)
    exec 3<&-
    return 1
    ;;
  esac
  [ "$3" -le "$max_expanded_binary_bytes" ] || {
    exec 3<&-
    return 1
  }
  bootstrap_helper_identity=$1
  bootstrap_helper_digest=$2
}

validate_bundle_entry_ancestors() {
  bundle_root=$1
  entry=$2
  case "$entry" in
    "" | /* | .. | ../* | */.. | */../* | *\\*) return 1 ;;
  esac
  [ -d "$bundle_root" ] && [ ! -L "$bundle_root" ] || return 1
  entry_parent=${entry%/*}
  [ "$entry_parent" != "$entry" ] || return 0
  old_ifs=$IFS
  IFS=/
  # shellcheck disable=SC2086 # entry is a fixed slash-delimited allow-list path.
  set -- $entry_parent
  IFS=$old_ifs
  current=$bundle_root
  for component in "$@"; do
    [ -n "$component" ] || return 1
    current=$current/$component
    [ ! -L "$current" ] || return 1
    if [ ! -e "$current" ]; then
      return 0
    fi
    [ -d "$current" ] || return 1
    physical=$(CDPATH='' cd -- "$current" && pwd -P) || return 1
    [ "$physical" = "$current" ] || return 1
  done
}

ensure_bundle_entry_parent() {
  bundle_root=$1
  entry=$2
  validate_bundle_entry_ancestors "$bundle_root" "$entry" || return 1
  entry_parent=${entry%/*}
  [ "$entry_parent" != "$entry" ] || return 0
  old_ifs=$IFS
  IFS=/
  # shellcheck disable=SC2086 # entry is a fixed slash-delimited allow-list path.
  set -- $entry_parent
  IFS=$old_ifs
  current=$bundle_root
  for component in "$@"; do
    current=$current/$component
    if [ ! -e "$current" ]; then
      mkdir "$current" || return 1
    fi
    [ -d "$current" ] && [ ! -L "$current" ] || return 1
    physical=$(CDPATH='' cd -- "$current" && pwd -P) || return 1
    [ "$physical" = "$current" ] || return 1
  done
}

validate_physical_directory() {
  directory_path=$(strip_trailing_slashes "$1")
  [ -d "$directory_path" ] && [ ! -L "$directory_path" ] || return 1
  physical_path=$(CDPATH='' cd -- "$directory_path" && pwd -P) || return 1
  [ "$physical_path" = "$directory_path" ]
}

ensure_install_state_root() {
  product_home=$1
  validate_physical_directory "$product_home" || return 1
  install_state_root=$product_home/installs
  if [ ! -e "$install_state_root" ]; then
    [ ! -L "$install_state_root" ] || return 1
    mkdir "$install_state_root" || return 1
  fi
  validate_physical_directory "$install_state_root"
}

validate_install_state_root() {
  product_home=$1
  validate_physical_directory "$product_home" || return 1
  validate_physical_directory "$product_home/installs"
}

validate_install_lock_ownership() {
  [ -n "$install_lock_path" ] && [ -n "$install_lock_anchor_path" ] && [ -n "$install_lock_token" ] || return 1
  [ -n "$install_lock_product_home" ] || return 1
  validate_install_state_root "$install_lock_product_home" || return 1
  validate_physical_directory "${install_lock_anchor_path%/*}" || return 1
  lock_owner=$(install_lock_owner_helper "$install_lock_path") || return 1
  anchor_owner=$(install_lock_owner_helper "$install_lock_anchor_path") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $lock_owner
  [ "$3" = "$install_lock_token" ] || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $anchor_owner
  [ "$3" = "$install_lock_token" ]
}

ensure_install_transactions_root() {
  product_home=$1
  validate_install_state_root "$product_home" || return 1
  transactions_root=$product_home/installs/transactions
  if [ ! -e "$transactions_root" ]; then
    [ ! -L "$transactions_root" ] || return 1
    mkdir "$transactions_root" || return 1
  fi
  validate_physical_directory "$transactions_root"
}

json_string() {
  value=$1

  case "$value" in
    *\"* | *\\*)
      fail "metadata values must not contain quotes or backslashes: $value"
      ;;
  esac

  printf '%s' "$value" | awk '{
		gsub(/\t/, "\\t")
		gsub(/\r/, "\\r")
		printf "\"%s\"", $0
	}'
}

install_lifecycle_path() {
  product_home=$1
  identity_path=$2
  suffix=$3
  lifecycle_id=$(sha256_text "$identity_path")
  printf '%s/installs/%s%s' "$product_home" "$lifecycle_id" "$suffix"
}

install_lifecycle_lock_path() {
  product_home=$1
  command_path=$2
  lifecycle_id=$(sha256_text "$command_path")
  command_name=${command_path##*/}
  printf '%s/installs/.%s.install-%s.lock' "$product_home" "$command_name" "$lifecycle_id"
}

install_lifecycle_guard_path() {
  : "$1"
  command_path=$2
  lifecycle_id=$(sha256_text "$command_path")
  command_name=${command_path##*/}
  command_dir=${command_path%/*}
  printf '%s/.%s.install-%s.guard' "$command_dir" "$command_name" "$lifecycle_id"
}

install_lifecycle_anchor_path() {
  command_path=$1
  lifecycle_id=$(sha256_text "$command_path")
  command_name=${command_path##*/}
  command_dir=${command_path%/*}
  printf '%s/.%s.install-anchor-%s.lock' "$command_dir" "$command_name" "$lifecycle_id"
}

write_install_lock_owner() {
  heartbeat=$1
  printf '{"pid":%s,"created":"%s","heartbeat":"%s","command":%s,"token":"%s"}\n' \
    "$$" "$install_lock_created" "$heartbeat" "$(json_string "$install_lock_command")" "$install_lock_token"
}

start_install_lock_heartbeat() {
  (
    exec </dev/null >/dev/null 2>/dev/null
    exec 8>&- || true
    while :; do
      sleep 1
      lock_owner=$(install_lock_owner_helper "$install_lock_path") || exit 0
      # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
      set -- $lock_owner
      lock_identity=$1
      lock_token=$3
      [ "$lock_token" = "$install_lock_token" ] || exit 0
      heartbeat=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
      install_lock_heartbeat_helper "$install_lock_path" "$lock_identity" "$lock_token" "$heartbeat" || exit 0
      if [ -n "$install_lock_anchor_path" ]; then
        anchor_owner=$(install_lock_owner_helper "$install_lock_anchor_path") || continue
        # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
        set -- $anchor_owner
        anchor_identity=$1
        anchor_token=$3
        if [ "$anchor_token" = "$install_lock_token" ]; then
          install_lock_heartbeat_helper "$install_lock_anchor_path" "$anchor_identity" "$anchor_token" "$heartbeat" || exit 0
        fi
      fi
    done
  ) &
  install_lock_heartbeat_pid=$!
}

restore_detached_install_lock() {
  detached_path=$1
  canonical_path=$2
  detached_identity=$3
  detached_inspection=$(install_entry_inspect_helper "$detached_path") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $detached_inspection
  [ "$1" = "$detached_identity" ] && [ "$2" = regular ] || return 1
  [ ! -e "$canonical_path" ] && [ ! -L "$canonical_path" ] || return 1
  install_atomic_move_noreplace_as "$detached_path" "$canonical_path" "$detached_identity" || return 1
  [ ! -e "$detached_path" ] && [ ! -L "$detached_path" ] || return 1
  canonical_inspection=$(install_entry_inspect_helper "$canonical_path") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $canonical_inspection
  [ "$1" = "$detached_identity" ] && [ "$2" = regular ]
}

detach_install_lock_generation() {
  canonical_path=$1
  expected_token=$2
  expected_identity=$3
  purpose=$4
  detached_path=$canonical_path.$purpose.$install_lock_token.$$
  [ ! -e "$detached_path" ] && [ ! -L "$detached_path" ] || return 1
  install_atomic_move_noreplace_as "$canonical_path" "$detached_path" "$expected_identity" 2>/dev/null || return 1
  moved_owner=$(install_lock_owner_helper "$detached_path") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $moved_owner
  moved_identity=$1
  moved_token=$3
  if [ -z "$moved_identity" ] || [ "$moved_identity" != "$expected_identity" ] || [ "$moved_token" != "$expected_token" ]; then
    restore_detached_install_lock "$detached_path" "$canonical_path" "$moved_identity" ||
      warn "changed install lifecycle lock could not be restored; retained $detached_path"
    return 1
  fi
  detached_inspection=$(install_entry_inspect_helper "$detached_path") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $detached_inspection
  [ "$1" = "$moved_identity" ] && [ "$2" = regular ] || return 1
  install_remove_entry_as "$detached_path" "$1" "$2" "$3"
}

install_lock_test_pause() {
  phase=$1
  [ "${THREADPOINT_INSTALL_SH_TEST_MODE:-0}" = 1 ] || return 0
  case "$phase" in
    stale-review)
      ready_path=${THREADPOINT_INSTALL_LOCK_TEST_STALE_READY:-}
      continue_path=${THREADPOINT_INSTALL_LOCK_TEST_STALE_CONTINUE:-}
      ;;
    release-review)
      ready_path=${THREADPOINT_INSTALL_LOCK_TEST_RELEASE_READY:-}
      continue_path=${THREADPOINT_INSTALL_LOCK_TEST_RELEASE_CONTINUE:-}
      ;;
    *) return 0 ;;
  esac
  [ -n "$ready_path" ] && [ -n "$continue_path" ] || return 0
  : >"$ready_path" || return 1
  pause_attempts=0
  while [ ! -e "$continue_path" ]; do
    pause_attempts=$((pause_attempts + 1))
    [ "$pause_attempts" -lt 2000 ] || return 1
    sleep 0.01
  done
}

acquire_install_lock_file() {
  candidate_lock_path=$1
  max_attempts=$2
  attempts=0
  while [ "$attempts" -lt "$max_attempts" ]; do
    if created_lock=$(write_install_lock_owner "$install_lock_created" |
      install_write_bounded_helper "$candidate_lock_path" 0600 2>/dev/null); then
      # shellcheck disable=SC2086 # the helper wrapper emits two restricted tokens.
      set -- $created_lock
      created_identity=$1
      created_digest=$2
      created_owner=$(install_lock_owner_helper "$candidate_lock_path") || {
        install_remove_entry_as "$candidate_lock_path" "$created_identity" regular "$created_digest" 2>/dev/null || true
        return 1
      }
      # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
      set -- $created_owner
      if [ "$1" != "$created_identity" ] || [ "$3" != "$install_lock_token" ]; then
        install_remove_entry_as "$candidate_lock_path" "$created_identity" regular "$created_digest" 2>/dev/null || true
        return 1
      fi
      return 0
    fi
    captured_owner=$(install_lock_owner_helper "$candidate_lock_path" 2>/dev/null || true)
    # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
    set -- $captured_owner
    if [ "$#" -ne 3 ]; then
      # A live owner rewrites its heartbeat in place, so a contender can
      # briefly observe a truncated body. Malformed ownership fails closed;
      # only a complete record for a provably dead PID may be reclaimed.
      attempts=$((attempts + 1))
      sleep 1
      continue
    fi
    captured_identity=$1
    owner_pid=$2
    captured_token=$3
    if kill -0 "$owner_pid" 2>/dev/null; then
      attempts=$((attempts + 1))
      sleep 1
      continue
    fi
    install_lock_test_pause stale-review || return 1
    detach_install_lock_generation "$candidate_lock_path" "$captured_token" "$captured_identity" reclaim || return 1
    attempts=$((attempts + 1))
  done
  return 1
}

discard_install_kernel_guard_control() {
  if [ "$install_lock_guard_open" -eq 1 ]; then
    exec 8>&- || true
    install_lock_guard_open=0
  fi
  if [ -n "$install_lock_guard_helper_pid" ]; then
    kill "$install_lock_guard_helper_pid" 2>/dev/null || true
    wait "$install_lock_guard_helper_pid" 2>/dev/null || true
    install_lock_guard_helper_pid=""
  fi
  if [ -n "$install_lock_guard_control" ] && [ -d "$install_lock_guard_control" ]; then
    rm -f "$install_lock_guard_control/release.pipe" "$install_lock_guard_control/status" \
      "$install_lock_guard_control/error" "$install_lock_guard_control/helper" 2>/dev/null || true
    rmdir "$install_lock_guard_control" 2>/dev/null || true
  fi
  install_lock_guard_control=""
  install_lock_helper_path=""
}

acquire_install_kernel_guard() {
  helper_path=$1
  max_attempts=$2
  helper_executable=${3:-$helper_path}
  helper_source_identity=${4:-}
  helper_source_digest=${5:-}
  [ -f "$helper_path" ] && [ ! -L "$helper_path" ] && [ -x "$helper_path" ] || return 1
  [ -f "$helper_executable" ] && [ -x "$helper_executable" ] || return 1
  install_lock_guard_control=$(mktemp -d "${TMPDIR:-/tmp}/threadpoint-lock.XXXXXX") || return 1
  chmod 0700 "$install_lock_guard_control" || {
    discard_install_kernel_guard_control
    return 1
  }
  guard_pipe=$install_lock_guard_control/release.pipe
  guard_status=$install_lock_guard_control/status
  guard_error=$install_lock_guard_control/error
  mkfifo "$guard_pipe" || {
    discard_install_kernel_guard_control
    return 1
  }
  chmod 0600 "$guard_pipe" || {
    discard_install_kernel_guard_control
    return 1
  }
  : >"$guard_status" || {
    discard_install_kernel_guard_control
    return 1
  }
  : >"$guard_error" || {
    discard_install_kernel_guard_control
    return 1
  }
  chmod 0600 "$guard_status" "$guard_error" || {
    discard_install_kernel_guard_control
    return 1
  }
  if [ -z "$helper_source_identity" ] && [ -z "$helper_source_digest" ]; then
    helper_source_inspection=$(install_entry_inspect_with_helper "$helper_executable" "$helper_path") || {
      discard_install_kernel_guard_control
      return 1
    }
    # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
    set -- $helper_source_inspection
    helper_source_identity=$1
    helper_source_kind=$2
    helper_source_digest=$3
    [ "$helper_source_kind" = regular ] || {
      discard_install_kernel_guard_control
      return 1
    }
  else
    case "$helper_source_identity" in
      *[!0-9:]* | *:*:* | :* | *:)
        discard_install_kernel_guard_control
        return 1
        ;;
    esac
    case "$helper_source_identity" in
      [0-9]*:[0-9]*) ;;
      *)
        discard_install_kernel_guard_control
        return 1
        ;;
    esac
    [ "${#helper_source_digest}" -eq 64 ] || {
      discard_install_kernel_guard_control
      return 1
    }
    case "$helper_source_digest" in
      *[!0-9a-f]*)
        discard_install_kernel_guard_control
        return 1
        ;;
    esac
  fi
  install_lock_helper_path=$install_lock_guard_control/helper
  helper_copy_result=$("$helper_executable" __threadpoint-install-copy-bounded-as \
    "$helper_path" "$install_lock_helper_path" "$helper_source_identity" "$helper_source_digest" 0700 8>&-) || {
    discard_install_kernel_guard_control
    return 1
  }
  # shellcheck disable=SC2086 # the helper emits two restricted tokens.
  set -- $helper_copy_result
  [ "$#" -eq 2 ] && [ "$2" = "$helper_source_digest" ] || {
    discard_install_kernel_guard_control
    return 1
  }
  installed_helper_inspection=$(install_entry_inspect_helper "$install_lock_helper_path") || {
    discard_install_kernel_guard_control
    return 1
  }
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $installed_helper_inspection
  [ "$2" = regular ] && [ "$3" = "$helper_source_digest" ] || {
    discard_install_kernel_guard_control
    return 1
  }
  "$install_lock_helper_path" __threadpoint-install-lock-guard "$install_lock_guard_path" "$max_attempts" \
    <"$guard_pipe" >"$guard_status" 2>"$guard_error" &
  install_lock_guard_helper_pid=$!
  exec 8>"$guard_pipe" || {
    discard_install_kernel_guard_control
    return 1
  }
  install_lock_guard_open=1
  guard_waits=0
  guard_max_waits=$(((max_attempts + 2) * 20))
  while [ "$guard_waits" -lt "$guard_max_waits" ]; do
    if grep -qx ready "$guard_status" 2>/dev/null; then
      return 0
    fi
    [ ! -s "$guard_error" ] || break
    guard_waits=$((guard_waits + 1))
    sleep 0.05
  done
  discard_install_kernel_guard_control
  return 1
}

release_install_kernel_guard() {
  guard_release_failed=0
  if [ "$install_lock_guard_open" -eq 1 ]; then
    exec 8>&- || guard_release_failed=1
    install_lock_guard_open=0
  fi
  if [ -n "$install_lock_guard_helper_pid" ]; then
    wait "$install_lock_guard_helper_pid" 2>/dev/null || guard_release_failed=1
    install_lock_guard_helper_pid=""
  fi
  if [ -n "$install_lock_guard_control" ] && [ -s "$install_lock_guard_control/error" ]; then
    warn "$(sed -n '1p' "$install_lock_guard_control/error")"
    guard_release_failed=1
  fi
  if [ -n "$install_lock_guard_control" ] && [ -d "$install_lock_guard_control" ]; then
    rm -f "$install_lock_guard_control/release.pipe" "$install_lock_guard_control/status" "$install_lock_guard_control/error" "$install_lock_helper_path" || guard_release_failed=1
    rmdir "$install_lock_guard_control" 2>/dev/null || guard_release_failed=1
  fi
  install_lock_guard_control=""
  install_lock_helper_path=""
  [ "$guard_release_failed" -eq 0 ]
}

acquire_install_lifecycle_lock() {
  product_home=$1
  command_path=$(canonical_install_command_path "$2") || return 1
  helper_path=${3:-}
  helper_executable=${4:-}
  helper_source_identity=${5:-}
  helper_source_digest=${6:-}
  if [ -z "$helper_path" ] && [ "${THREADPOINT_INSTALL_SH_TEST_MODE:-0}" = 1 ]; then
    helper_path=${THREADPOINT_INSTALL_LOCK_HELPER:-}
  fi
  metadata_dir=$product_home/installs
  ensure_install_state_root "$product_home" || return 1
  validate_physical_directory "${command_path%/*}" || return 1
  chmod 0700 "$product_home" "$metadata_dir" 2>/dev/null || true
  install_lock_command=$command_path
  install_lock_product_home=$product_home
  install_lock_path=$(install_lifecycle_lock_path "$product_home" "$command_path")
  install_lock_anchor_path=$(install_lifecycle_anchor_path "$command_path")
  install_lock_guard_path=$(install_lifecycle_guard_path "$product_home" "$command_path")
  install_lock_created=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  install_lock_token=$(sha256_text "$(printf '%s:%s:%s' "$command_path" "$$" "$install_lock_created")")

  max_attempts=${THREADPOINT_INSTALL_LOCK_ATTEMPTS:-30}
  acquire_install_kernel_guard "$helper_path" "$max_attempts" "$helper_executable" \
    "$helper_source_identity" "$helper_source_digest" || return 1
  if ! acquire_install_lock_file "$install_lock_path" "$max_attempts"; then
    release_install_kernel_guard || true
    return 1
  fi
  start_install_lock_heartbeat
  if ! acquire_install_lock_file "$install_lock_anchor_path" "$max_attempts"; then
    release_install_lifecycle_lock || true
    return 1
  fi
  return 0
}

release_install_lifecycle_lock() {
  lifecycle_release_failed=0
  if [ -n "$install_lock_heartbeat_pid" ]; then
    kill "$install_lock_heartbeat_pid" 2>/dev/null || true
    wait "$install_lock_heartbeat_pid" 2>/dev/null || true
    install_lock_heartbeat_pid=""
  fi
  install_lock_test_pause release-review || lifecycle_release_failed=1
  if [ -n "$install_lock_path" ] && release_owner=$(install_lock_owner_helper "$install_lock_path"); then
    # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
    set -- $release_owner
    release_identity=$1
    release_token=$3
    [ "$release_token" = "$install_lock_token" ] &&
      detach_install_lock_generation "$install_lock_path" "$install_lock_token" "$release_identity" release || lifecycle_release_failed=1
  elif [ -n "$install_lock_path" ]; then
    lifecycle_release_failed=1
  fi
  if [ -n "$install_lock_anchor_path" ] && release_owner=$(install_lock_owner_helper "$install_lock_anchor_path"); then
    # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
    set -- $release_owner
    release_identity=$1
    release_token=$3
    [ "$release_token" = "$install_lock_token" ] &&
      detach_install_lock_generation "$install_lock_anchor_path" "$install_lock_token" "$release_identity" release || lifecycle_release_failed=1
  elif [ -n "$install_lock_anchor_path" ]; then
    lifecycle_release_failed=1
  fi
  release_install_kernel_guard || lifecycle_release_failed=1
  install_lock_path=""
  install_lock_anchor_path=""
  install_lock_guard_path=""
  install_lock_token=""
  install_lock_product_home=""
  [ "$lifecycle_release_failed" -eq 0 ]
}

write_install_transaction_checksums() {
  backup_root=$1
  manifest=$backup_root/checksums.txt
  manifest_generation=$(
    {
      for entry in $(bundle_entries); do
        entry_digest=$(install_regular_digest_helper "$backup_root/$entry") || exit 1
        printf '%s  %s\n' "$entry_digest" "$entry"
      done
      metadata_digest=$(install_regular_digest_helper "$backup_root/install-metadata.json") || exit 1
      printf '%s  %s\n' "$metadata_digest" 'install-metadata.json'
    } | install_write_bounded_helper "$manifest" 0600
  ) || return 1
  [ -n "$manifest_generation" ]
}

validate_install_transaction_checksums() {
  backup_root=$1
  [ "$backup_root" = "$(install_journal_helper "$install_transaction_journal" "$install_transaction_journal_digest" value backup_root)" ] || return 1
  install_journal_helper "$install_transaction_journal" "$install_transaction_journal_digest" \
    backup-prior-as "$install_transaction_journal"
}

install_generation_json() {
  generation_kind=$1
  generation_path=${2:-}
  case "$generation_kind" in
    absent)
      printf '{"kind":"absent"}'
      ;;
    regular)
      generation_inspection=$(install_entry_inspect_helper "$generation_path") || return 1
      # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
      set -- $generation_inspection
      [ "$2" = regular ] && [ "${#3}" -eq 64 ] || return 1
      printf '{"kind":"regular","identity":%s,"digest":%s}' \
        "$(json_string "$1")" "$(json_string "$3")"
      ;;
    *) return 1 ;;
  esac
}

install_generation_field() {
  generation_body=$1
  generation_name=$2
  printf '%s\n' "$generation_body" |
    sed -n 's/.*"'"$generation_name"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
    sed -n '1p'
}

install_generation_matches() {
  generation_body=$1
  generation_path=$2
  [ -n "$generation_body" ] || return 1
  generation_kind=$(install_generation_field "$generation_body" kind)
  case "$generation_kind" in
    absent)
      [ ! -e "$generation_path" ] && [ ! -L "$generation_path" ]
      ;;
    regular)
      generation_identity=$(install_generation_field "$generation_body" identity)
      generation_digest=$(install_generation_field "$generation_body" digest)
      [ -n "$generation_identity" ] && [ -n "$generation_digest" ] || return 1
      generation_inspection=$(install_entry_inspect_helper "$generation_path") || return 1
      # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
      set -- $generation_inspection
      [ "$2" = regular ] && [ "$1" = "$generation_identity" ] && [ "$3" = "$generation_digest" ]
      ;;
    *) return 1 ;;
  esac
}

install_journal_generation_matches() {
  generation_key=$1
  generation_field=$2
  generation_path=$3
  install_journal_helper "$install_transaction_journal" "$install_transaction_journal_digest" \
    matches "$generation_key" "$generation_field" "$generation_path"
}

install_journal_regular_digest() {
  generation_key=$1
  generation_field=$2
  generation_body=$(install_journal_helper "$install_transaction_journal" "$install_transaction_journal_digest" \
    generation "$generation_key" "$generation_field") || return 1
  [ "$(install_generation_field "$generation_body" kind)" = regular ] || return 1
  generation_digest=$(install_generation_field "$generation_body" digest)
  [ "${#generation_digest}" -eq 64 ] || return 1
  case "$generation_digest" in *[!0-9a-f]*) return 1 ;; esac
  printf '%s\n' "$generation_digest"
}

restore_detached_install_entry() {
  detached_path=$1
  canonical_path=$2
  detached_identity=$3
  [ ! -e "$canonical_path" ] && [ ! -L "$canonical_path" ] || return 1
  install_atomic_move_noreplace_as "$detached_path" "$canonical_path" "$detached_identity" || return 1
  [ ! -e "$detached_path" ] && [ ! -L "$detached_path" ] || return 1
  [ "$(install_entry_identity_helper "$canonical_path")" = "$detached_identity" ]
}

publish_reviewed_install_entry() {
  publish_temp=$1
  publish_target=$2
  allowed_key=$3
  allowed_fields=$4
  publish_field=$5
  publish_parent=${publish_target%/*}
  [ "$publish_parent" != "$publish_target" ] || publish_parent=.
  validate_physical_directory "$publish_parent" || return 1
  detached_dir=$(mktemp -d "$publish_parent/.install-detach-XXXXXX") || return 1
  detached_dir_identity=$(install_directory_identity_helper "$detached_dir") || return 1
  detached_path=$detached_dir/candidate
  detached_identity=""
  detached_field=""

  if [ -e "$publish_target" ] || [ -L "$publish_target" ]; then
    publish_allowed=0
    for allowed_field in $allowed_fields; do
      if install_journal_generation_matches "$allowed_key" "$allowed_field" "$publish_target"; then
        publish_allowed=1
        break
      fi
    done
    [ "$publish_allowed" -eq 1 ] || {
      install_remove_entry_as "$detached_dir" "$detached_dir_identity" directory -
      return 1
    }
    install_atomic_move_noreplace "$publish_target" "$detached_path" || {
      install_remove_entry_as "$detached_dir" "$detached_dir_identity" directory -
      return 1
    }
    [ ! -e "$publish_target" ] && [ ! -L "$publish_target" ] || return 1
    detached_identity=$(install_entry_identity_helper "$detached_path") || return 1
    detached_allowed=0
    for allowed_field in $allowed_fields; do
      if install_journal_generation_matches "$allowed_key" "$allowed_field" "$detached_path"; then
        detached_allowed=1
        detached_field=$allowed_field
        break
      fi
    done
    if [ "$detached_allowed" -ne 1 ]; then
      if restore_detached_install_entry "$detached_path" "$publish_target" "$detached_identity"; then
        install_remove_entry_as "$detached_dir" "$detached_dir_identity" directory - ||
          warn "empty install target quarantine changed before cleanup: $detached_dir"
      else
        warn "changed install target retained in private quarantine $detached_dir"
      fi
      return 1
    fi
  else
    publish_absent=0
    for allowed_field in $allowed_fields; do
      if install_journal_generation_matches "$allowed_key" "$allowed_field" "$publish_target"; then
        publish_absent=1
        break
      fi
    done
    [ "$publish_absent" -eq 1 ] || {
      install_remove_entry_as "$detached_dir" "$detached_dir_identity" directory -
      return 1
    }
  fi

  if ! install_atomic_move_noreplace "$publish_temp" "$publish_target" || [ -e "$publish_temp" ] || [ -L "$publish_temp" ] ||
    ! install_journal_generation_matches "$allowed_key" "$publish_field" "$publish_target"; then
    if [ -n "$detached_identity" ]; then
      if ! restore_detached_install_entry "$detached_path" "$publish_target" "$detached_identity"; then
        warn "prior install target retained in private quarantine $detached_dir"
        return 1
      fi
    fi
    install_remove_entry_as "$detached_dir" "$detached_dir_identity" directory - ||
      warn "empty install target quarantine changed before cleanup: $detached_dir"
    return 1
  fi
  if [ -n "$detached_identity" ]; then
    install_journal_helper "$install_transaction_journal" "$install_transaction_journal_digest" \
      remove-matched "$allowed_key" "$detached_field" "$detached_path" || return 1
  fi
  install_remove_entry_as "$detached_dir" "$detached_dir_identity" directory - || return 1
}

update_install_journal_generation() {
  update_key=$1
  update_field=$2
  update_kind=$3
  update_source=$4
  validate_install_lock_ownership || return 1
  update_result=$(install_journal_helper "$install_transaction_journal" "$install_transaction_journal_digest" \
    update-from "$install_transaction_journal_identity" "$update_key" "$update_field" "$update_kind" "$update_source") || return 1
  # shellcheck disable=SC2086 # the helper emits two restricted tokens, validated immediately below.
  set -- $update_result
  [ "$#" -eq 2 ] || return 1
  [ -n "$1" ] && [ "${#2}" -eq 64 ] || return 1
  case "$2" in *[!0-9a-f]*) return 1 ;; esac
  install_transaction_journal_identity=$1
  install_transaction_journal_digest=$2
}

update_install_journal_generation_expected() {
  update_key=$1
  update_field=$2
  update_kind=$3
  update_source=$4
  update_expected_identity=$5
  update_expected_digest=$6
  validate_install_lock_ownership || return 1
  update_result=$(install_journal_helper "$install_transaction_journal" "$install_transaction_journal_digest" \
    update-from-expected "$install_transaction_journal_identity" "$update_key" "$update_field" "$update_kind" \
    "$update_source" "$update_expected_identity" "$update_expected_digest") || return 1
  # shellcheck disable=SC2086 # the helper emits two restricted tokens, validated immediately below.
  set -- $update_result
  [ "$#" -eq 2 ] || return 1
  [ -n "$1" ] && [ "${#2}" -eq 64 ] || return 1
  case "$2" in *[!0-9a-f]*) return 1 ;; esac
  install_transaction_journal_identity=$1
  install_transaction_journal_digest=$2
}

publish_install_target_generation() {
  target_publish_key=$1
  target_publish_temp=$2
  target_publish_target=$3
  target_publish_phase=$4
  target_publish_kind=$5
  target_publish_expected_identity=${6:-}
  target_publish_expected_digest=${7:-}
  case "$target_publish_phase" in
    normal)
      update_install_journal_generation "$target_publish_key" produced "$target_publish_kind" "$target_publish_temp" || return 1
      target_publish_allowed_fields=prior
      target_publish_field=produced
      ;;
    recovery)
      if [ -n "$target_publish_expected_identity" ] || [ -n "$target_publish_expected_digest" ]; then
        [ -n "$target_publish_expected_identity" ] && [ -n "$target_publish_expected_digest" ] || return 1
        update_install_journal_generation_expected "$target_publish_key" recovery "$target_publish_kind" \
          "$target_publish_temp" "$target_publish_expected_identity" "$target_publish_expected_digest" || return 1
      else
        update_install_journal_generation "$target_publish_key" recovery "$target_publish_kind" "$target_publish_temp" || return 1
      fi
      target_publish_allowed_fields='prior produced recovery'
      target_publish_field=recovery
      ;;
    *) return 1 ;;
  esac
  publish_reviewed_install_entry "$target_publish_temp" "$target_publish_target" "$target_publish_key" \
    "$target_publish_allowed_fields" "$target_publish_field" || return 1
  if [ "$target_publish_phase" = recovery ]; then
    update_install_journal_generation "$target_publish_key" prior "$target_publish_kind" "$target_publish_target" || return 1
  fi
}

remove_install_target_generation() {
  remove_key=$1
  remove_target=$2
  remove_phase=$3
  case "$remove_phase" in
    normal)
      remove_allowed_fields=prior
      update_install_journal_generation "$remove_key" produced absent - || return 1
      ;;
    recovery) remove_allowed_fields='prior produced recovery' ;;
    *) return 1 ;;
  esac
  if [ ! -e "$remove_target" ] && [ ! -L "$remove_target" ]; then
    for remove_field in $remove_allowed_fields; do
      install_journal_generation_matches "$remove_key" "$remove_field" "$remove_target" && return 0
    done
    return 1
  fi
  remove_parent=${remove_target%/*}
  detached_dir=$(mktemp -d "$remove_parent/.install-remove-XXXXXX") || return 1
  detached_dir_identity=$(install_directory_identity_helper "$detached_dir") || return 1
  detached_path=$detached_dir/candidate
  install_atomic_move_noreplace "$remove_target" "$detached_path" || return 1
  remove_identity=$(install_entry_identity_helper "$detached_path") || return 1
  remove_allowed=0
  remove_matched_field=""
  for remove_field in $remove_allowed_fields; do
    if install_journal_generation_matches "$remove_key" "$remove_field" "$detached_path"; then
      remove_allowed=1
      remove_matched_field=$remove_field
      break
    fi
  done
  if [ "$remove_allowed" -ne 1 ]; then
    if restore_detached_install_entry "$detached_path" "$remove_target" "$remove_identity"; then
      install_remove_entry_as "$detached_dir" "$detached_dir_identity" directory - ||
        warn "empty install removal quarantine changed before cleanup: $detached_dir"
    else
      warn "changed install target retained in private quarantine $detached_dir"
    fi
    return 1
  fi
  install_journal_helper "$install_transaction_journal" "$install_transaction_journal_digest" \
    remove-matched "$remove_key" "$remove_matched_field" "$detached_path" || return 1
  install_remove_entry_as "$detached_dir" "$detached_dir_identity" directory - || return 1
}

# POSIX sh has no portable directory-fsync primitive. A whole-filesystem sync
# is the conservative durability barrier shared by transaction preparation and
# completion; tests may override this function to exercise failure handling.
sync_install_lifecycle_state() {
  sync
}

finish_install_transaction_state() {
  journal_path=$1
  backup_path=$2
  backup_identity=$3
  transaction_phase=$4
  [ "$journal_path" = "$install_transaction_journal" ] || return 1
  install_journal_helper "$journal_path" "$install_transaction_journal_digest" commit "$transaction_phase" "$install_transaction_journal_identity" || return 1
  # The helper's exact journal detach, phase validation, removal, and parent
  # sync are the semantic commit point. Backup cleanup happens only afterward.
  if ! remove_install_recovery_bundle_exact "$backup_path" "$backup_identity"; then
    warn "committed install retained changed or nonempty recovery backup at $backup_path"
  fi
  return 0
}

remove_install_recovery_file_exact() {
  recovery_file_path=$1
  recovery_file_inspection=$(install_entry_inspect_helper "$recovery_file_path") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $recovery_file_inspection
  recovery_file_identity=$1
  recovery_file_kind=$2
  recovery_file_proof=$3
  [ "$recovery_file_kind" = regular ] || return 1
  recovery_file_parent=${recovery_file_path%/*}
  recovery_file_quarantine=$(mktemp -d "$recovery_file_parent/.bundle-file-cleanup-XXXXXX") || return 1
  recovery_quarantine_inspection=$(install_entry_inspect_helper "$recovery_file_quarantine") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $recovery_quarantine_inspection
  recovery_quarantine_identity=$1
  [ "$2" = directory ] && [ "$3" = - ] || return 1
  recovery_file_candidate=$recovery_file_quarantine/candidate
  if ! install_atomic_move_noreplace_as "$recovery_file_path" "$recovery_file_candidate" "$recovery_file_identity"; then
    install_remove_entry_as "$recovery_file_quarantine" "$recovery_quarantine_identity" directory - 2>/dev/null || true
    return 1
  fi
  install_remove_entry_as "$recovery_file_candidate" "$recovery_file_identity" "$recovery_file_kind" "$recovery_file_proof" || return 1
  install_remove_entry_as "$recovery_file_quarantine" "$recovery_quarantine_identity" directory -
}

remove_install_recovery_directory_exact() {
  recovery_directory_path=$1
  recovery_directory_inspection=$(install_entry_inspect_helper "$recovery_directory_path") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $recovery_directory_inspection
  recovery_directory_identity=$1
  [ "$2" = directory ] && [ "$3" = - ] || return 1
  recovery_directory_parent=${recovery_directory_path%/*}
  recovery_directory_quarantine=$(mktemp -d "$recovery_directory_parent/.bundle-dir-cleanup-XXXXXX") || return 1
  recovery_directory_quarantine_inspection=$(install_entry_inspect_helper "$recovery_directory_quarantine") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $recovery_directory_quarantine_inspection
  recovery_directory_quarantine_identity=$1
  [ "$2" = directory ] && [ "$3" = - ] || return 1
  recovery_directory_candidate=$recovery_directory_quarantine/candidate
  if ! install_atomic_move_noreplace_as "$recovery_directory_path" "$recovery_directory_candidate" "$recovery_directory_identity"; then
    install_remove_entry_as "$recovery_directory_quarantine" "$recovery_directory_quarantine_identity" directory - 2>/dev/null || true
    return 1
  fi
  install_remove_entry_as "$recovery_directory_candidate" "$recovery_directory_identity" directory - || return 1
  install_remove_entry_as "$recovery_directory_quarantine" "$recovery_directory_quarantine_identity" directory -
}

remove_install_recovery_bundle_exact() {
  backup_path=$1
  expected_identity=$2
  transactions_root=${backup_path%/*}
  backup_base=${backup_path##*/}
  case "$backup_base" in
    .bundle-*) ;;
    *) return 1 ;;
  esac
  validate_physical_directory "$transactions_root" || return 1
  validate_physical_directory "$backup_path" || return 1
  [ "$(install_entry_identity "$backup_path" 2>/dev/null || true)" = "$expected_identity" ] || return 1
  cleanup_base=.bundle-cleanup-$install_lock_token-$$
  (
    CDPATH='' cd -- "$transactions_root" || exit 1
    [ ! -e "$cleanup_base" ] && [ ! -L "$cleanup_base" ] || exit 1
    install_atomic_move_noreplace_as "$backup_base" "$cleanup_base" "$expected_identity" || exit 1
    install_recovery_cleanup_as "$cleanup_base" "$expected_identity"
  )
}

recover_install_transaction() {
  product_home=$1
  command_path=$2
  validate_install_lock_ownership || return 1
  journal=$(install_lifecycle_path "$product_home" "$command_path" '.transaction.json')
  [ -e "$journal" ] || [ -L "$journal" ] || return 0
  install_transaction_journal=$journal
  journal_inspection=$(inspect_install_journal "$journal" "$journal") || return 1
  # The helper emits two fixed ASCII tokens from one bounded, nonblocking,
  # no-follow opened generation.
  # shellcheck disable=SC2086 # both restricted tokens are validated immediately below.
  set -- $journal_inspection
  [ "$#" -eq 2 ] || return 1
  install_transaction_journal_identity=$1
  install_transaction_journal_digest=$2
  schema=$(install_journal_helper "$journal" "$install_transaction_journal_digest" value schema_version) || return 1
  journal_command=$(install_journal_helper "$journal" "$install_transaction_journal_digest" value command_path) || return 1
  metadata_path=$(install_journal_helper "$journal" "$install_transaction_journal_digest" value metadata_path) || return 1
  bundle_root=$(install_journal_helper "$journal" "$install_transaction_journal_digest" value bundle_root) || return 1
  backup_root=$(install_journal_helper "$journal" "$install_transaction_journal_digest" value backup_root) || return 1
  operation=$(install_journal_helper "$journal" "$install_transaction_journal_digest" value operation) || return 1
  previous_install=$(install_journal_helper "$journal" "$install_transaction_journal_digest" value previous_install) || return 1
  link_existed=$(install_journal_helper "$journal" "$install_transaction_journal_digest" value link_existed) || return 1
  [ "$schema" = 'threadpoint.install_transaction.v1' ] || return 1
  case "$operation" in
    install | update | rollback | uninstall) ;;
    *) return 1 ;;
  esac
  canonical_recovery_command=$(canonical_install_command_path "$command_path") || return 1
  [ "$journal_command" = "$canonical_recovery_command" ] || return 1
  case "$backup_root" in
    "$product_home"/installs/transactions/*) ;;
    *) return 1 ;;
  esac
  validate_physical_directory "$product_home/installs/transactions" || return 1
  validate_physical_directory "$backup_root" || return 1
  install_transaction_backup_identity=$(install_entry_identity "$backup_root") || return 1
  binary_path=$bundle_root/bin/$binary
  expected_metadata=$(install_lifecycle_path "$product_home" "$binary_path" '.json')
  [ "$metadata_path" = "$expected_metadata" ] || return 1

  if [ "$previous_install" = true ]; then
    validate_install_transaction_checksums "$backup_root" || return 1
    recovery_failed=0
    for entry in $(bundle_entries); do
      target=$bundle_root/$entry
      ensure_bundle_entry_parent "$bundle_root" "$entry" || return 1
      expected_backup_digest=$(install_journal_regular_digest "bundle:$entry" prior) || return 1
      temp_target=$(mktemp "${target%/*}/.${target##*/}.recover-XXXXXX") || return 1
      remove_install_recovery_file_exact "$temp_target" || return 1
      copied_generation=$(install_copy_bounded_expected_as "$backup_root/$entry" "$temp_target" \
        "$expected_backup_digest" "$(bundle_entry_mode "$entry")") || return 1
      # shellcheck disable=SC2086 # the helper emits two restricted tokens.
      set -- $copied_generation
      [ "$#" -eq 2 ] || return 1
      validate_bundle_entry_ancestors "$bundle_root" "$entry" || return 1
      publish_install_target_generation "bundle:$entry" "$temp_target" "$target" recovery regular "$1" "$2" || recovery_failed=1
    done
    expected_metadata_digest=$(install_journal_regular_digest metadata prior) || return 1
    metadata_tmp=$(mktemp "${metadata_path%/*}/.metadata.recover-XXXXXX") || return 1
    remove_install_recovery_file_exact "$metadata_tmp" || return 1
    copied_generation=$(install_copy_bounded_expected_as "$backup_root/install-metadata.json" "$metadata_tmp" \
      "$expected_metadata_digest" 0600) || return 1
    # shellcheck disable=SC2086 # the helper emits two restricted tokens.
    set -- $copied_generation
    [ "$#" -eq 2 ] || return 1
    publish_install_target_generation metadata "$metadata_tmp" "$metadata_path" recovery regular "$1" "$2" || recovery_failed=1
    if [ "$link_existed" = true ]; then
      link_temp_dir=$(mktemp -d "${command_path%/*}/.link-recover-XXXXXX") || return 1
      link_temp_dir_identity=$(install_directory_identity_helper "$link_temp_dir") || return 1
      link_temp=$link_temp_dir/candidate
      ln -s "$binary_path" "$link_temp" || return 1
      publish_install_target_generation link "$link_temp" "$command_path" recovery symlink || recovery_failed=1
      install_remove_entry_as "$link_temp_dir" "$link_temp_dir_identity" directory - || recovery_failed=1
    else
      remove_install_target_generation link "$command_path" recovery || recovery_failed=1
    fi
    [ "$recovery_failed" -eq 0 ] || return 1
  else
    remove_install_target_generation link "$command_path" recovery || return 1
    for entry in $(bundle_entries); do
      target=$bundle_root/$entry
      validate_bundle_entry_ancestors "$bundle_root" "$entry" || return 1
      remove_install_target_generation "bundle:$entry" "$target" recovery || return 1
    done
    remove_install_target_generation metadata "$metadata_path" recovery || return 1
  fi
  validate_install_lock_ownership || return 1
  validate_physical_directory "$product_home/installs/transactions" || return 1
  validate_physical_directory "$backup_root" || return 1
  finish_install_transaction_state "$journal" "$backup_root" "$install_transaction_backup_identity" recovery || return 1
  install_transaction_journal=""
  install_transaction_backup=""
  install_transaction_backup_identity=""
  install_transaction_product_home=""
  install_transaction_journal_identity=""
  install_transaction_journal_digest=""
}

prepare_install_transaction() {
  product_home=$1
  command_path=$2
  bundle_root=$3
  metadata_path=$4
  transaction_operation=${5:-install}
  transactions_root=$product_home/installs/transactions
  validate_install_lock_ownership || return 1
  ensure_install_transactions_root "$product_home" || return 1
  chmod 0700 "$transactions_root" || return 1
  # Keep the shell-created recovery namespace byte-for-byte compatible with
  # the Go CLI validator, which admits only direct `.bundle-*` children.
  install_transaction_backup=$(mktemp -d "$transactions_root/.bundle-XXXXXX") || return 1
  validate_physical_directory "$install_transaction_backup" || return 1
  install_transaction_backup_identity=$(install_entry_identity "$install_transaction_backup") || return 1
  install_transaction_product_home=$product_home
  install_transaction_journal=$(install_lifecycle_path "$product_home" "$command_path" '.transaction.json')
  previous_install=true
  managed_count=0
  for entry in $(bundle_entries); do
    target=$bundle_root/$entry
    validate_bundle_entry_ancestors "$bundle_root" "$entry" || return 1
    if [ -L "$target" ] || { [ -e "$target" ] && [ ! -f "$target" ]; }; then
      return 1
    fi
    [ ! -f "$target" ] || managed_count=$((managed_count + 1))
  done
  if [ "$managed_count" -eq 0 ] && [ ! -e "$metadata_path" ] && [ ! -L "$command_path" ] && [ ! -e "$command_path" ]; then
    previous_install=false
  elif [ "$managed_count" -eq 6 ] && [ -f "$metadata_path" ] && [ ! -L "$metadata_path" ]; then
    for entry in $(bundle_entries); do
      validate_bundle_entry_ancestors "$bundle_root" "$entry" || return 1
      [ -f "$bundle_root/$entry" ] && [ ! -L "$bundle_root/$entry" ] || return 1
      entry_parent=${entry%/*}
      if [ "$entry_parent" != "$entry" ]; then
        mkdir -p "$install_transaction_backup/$entry_parent" || return 1
      fi
      install_copy_bounded_as "$bundle_root/$entry" "$install_transaction_backup/$entry" || return 1
    done
    install_copy_bounded_as "$metadata_path" "$install_transaction_backup/install-metadata.json" || return 1
    write_install_transaction_checksums "$install_transaction_backup" || return 1
  else
    warn 'refusing to replace an incomplete managed bundle without a recovery journal'
    return 1
  fi
  # The prior coherent generation must be durable before its journal can
  # authorize any bundle mutation.
  sync_install_lifecycle_state || return 1
  link_existed=false
  [ ! -L "$command_path" ] || link_existed=true
  created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  journal_tmp=$install_transaction_journal.tmp.$install_lock_token
  journal_tmp_write=$(
    {
      printf '{\n'
      printf '  "schema_version": "threadpoint.install_transaction.v1",\n'
      printf '  "operation": %s,\n' "$(json_string "$transaction_operation")"
      printf '  "command_path": %s,\n' "$(json_string "$command_path")"
      printf '  "metadata_path": %s,\n' "$(json_string "$metadata_path")"
      printf '  "bundle_root": %s,\n' "$(json_string "$bundle_root")"
      printf '  "backup_root": %s,\n' "$(json_string "$install_transaction_backup")"
      printf '  "previous_install": %s,\n' "$previous_install"
      printf '  "link_existed": %s,\n' "$link_existed"
      printf '  "created_at": "%s",\n' "$created_at"
      printf '  "targets": {\n'
      {
        for entry in $(bundle_entries); do
          printf 'bundle:%s|%s\n' "$entry" "$bundle_root/$entry"
        done
        printf 'metadata|%s\n' "$metadata_path"
        printf 'link|%s\n' "$command_path"
      } | while IFS='|' read -r target_key target_path; do
        if [ "${target_first:-1}" -eq 0 ]; then
          printf ',\n'
        fi
        target_first=0
        printf '    %s: {"prior": {' "$(json_string "$target_key")"
        if [ "$previous_install" = false ] || { [ ! -e "$target_path" ] && [ ! -L "$target_path" ]; }; then
          printf '"kind":"absent"'
        elif [ "$target_key" = link ]; then
          printf '"kind":"symlink","identity":%s,"link_target":%s' \
            "$(json_string "$(install_entry_identity "$target_path")")" "$(json_string "$(readlink "$target_path")")"
        else
          target_inspection=$(install_entry_inspect_helper "$target_path") || exit 1
          # shellcheck disable=SC2086 # the helper emits three restricted tokens.
          set -- $target_inspection
          [ "$2" = regular ] && [ "${#3}" -eq 64 ] || exit 1
          printf '"kind":"regular","identity":%s,"digest":%s' \
            "$(json_string "$1")" "$(json_string "$3")"
        fi
        printf '}}'
      done
      printf '\n  }\n'
      printf '}\n'
    } | install_write_bounded_helper "$journal_tmp" 0600
  ) || return 1
  # shellcheck disable=SC2086 # the bounded writer emits two restricted tokens.
  set -- $journal_tmp_write
  [ "$#" -eq 2 ] || return 1
  journal_tmp_identity=$1
  journal_tmp_digest=$2
  journal_tmp_generation=$(printf '{"kind":"regular","identity":%s,"digest":%s}' \
    "$(json_string "$journal_tmp_identity")" "$(json_string "$journal_tmp_digest")")
  if ! install_journal_helper "$journal_tmp" "$journal_tmp_digest" validate-as "$install_transaction_journal"; then
    discard_install_written_generation "$journal_tmp" "$journal_tmp_write" 2>/dev/null || true
    return 1
  fi
  if ! install_journal_helper "$journal_tmp" "$journal_tmp_digest" backup-prior-as "$install_transaction_journal"; then
    discard_install_written_generation "$journal_tmp" "$journal_tmp_write" 2>/dev/null || true
    return 1
  fi
  validate_install_lock_ownership || return 1
  [ ! -e "$install_transaction_journal" ] && [ ! -L "$install_transaction_journal" ] || return 1
  install_atomic_move_noreplace "$journal_tmp" "$install_transaction_journal" || return 1
  [ ! -e "$journal_tmp" ] && [ ! -L "$journal_tmp" ] || return 1
  expected_journal_identity=$(install_generation_field "$journal_tmp_generation" identity) || return 1
  expected_journal_digest=$(install_generation_field "$journal_tmp_generation" digest) || return 1
  journal_inspection=$(install_entry_inspect_helper "$install_transaction_journal") || return 1
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $journal_inspection
  [ "$2" = regular ] && [ "$1" = "$expected_journal_identity" ] && [ "$3" = "$expected_journal_digest" ] || return 1
  install_transaction_journal_identity=$1
  install_transaction_journal_digest=$3
  # Do not let the caller mutate the bundle until the journal itself is
  # durable. A failure leaves the still-unmodified bundle safe to retry.
  sync_install_lifecycle_state || return 1
}

complete_install_transaction() {
  [ -n "$install_transaction_journal" ] || return 1
  [ -n "$install_transaction_product_home" ] || return 1
  validate_install_lock_ownership || return 1
  validate_physical_directory "$install_transaction_product_home/installs/transactions" || return 1
  validate_physical_directory "$install_transaction_backup" || return 1
  finish_install_transaction_state "$install_transaction_journal" "$install_transaction_backup" "$install_transaction_backup_identity" normal || return 1
  install_transaction_journal=""
  install_transaction_backup=""
  install_transaction_backup_identity=""
  install_transaction_product_home=""
  install_transaction_journal_identity=""
  install_transaction_journal_digest=""
  return 0
}

write_install_metadata() {
  target_dir=$1
  target_binary=$2
  link_path=$3
  archive_name=$4
  archive_digest=$5
  release_url=$6
  installed_digest=$7
  version_label=$8
  bundle_root=$9

  metadata_dir=$bundle_root/installs
  metadata_id=$(sha256_text "$target_binary")
  metadata_path=$metadata_dir/$metadata_id.json
  metadata_tmp=$metadata_path.tmp.${install_lock_token:-$$}

  ensure_install_state_root "$bundle_root" || fail "installer metadata directory is not a physical directory: $metadata_dir"
  if [ -n "$install_lock_path" ]; then
    validate_install_lock_ownership || fail "install lifecycle lock changed before metadata write"
  fi
  metadata_tmp_write=$(
    {
      printf '{\n'
      printf '  "schema_version": 1,\n'
      printf '  "channel": "installer-script",\n'
      printf '  "repo": %s,\n' "$(json_string "$repo")"
      printf '  "install_dir": %s,\n' "$(json_string "$target_dir")"
      printf '  "binary_path": %s,\n' "$(json_string "$target_binary")"
      printf '  "link_path": %s,\n' "$(json_string "$link_path")"
      printf '  "bundle_root": %s,\n' "$(json_string "$bundle_root")"
      printf '  "bundle_entries": [\n'
      first=1
      bundle_entries | while IFS= read -r entry; do
        if [ "$first" -eq 0 ]; then
          printf ',\n'
        fi
        first=0
        printf '    %s' "$(json_string "$entry")"
      done
      printf '\n  ],\n'
      printf '  "version": %s,\n' "$(json_string "$version_label")"
      printf '  "release_base": %s,\n' "$(json_string "$release_url")"
      printf '  "archive": %s,\n' "$(json_string "$archive_name")"
      printf '  "archive_sha256": %s,\n' "$(json_string "$archive_digest")"
      printf '  "binary_sha256": %s\n' "$(json_string "$installed_digest")"
      printf '}\n'
    } | install_write_bounded_helper "$metadata_tmp" 0644
  ) || fail "could not safely write installer metadata"
  if [ -n "$install_lock_path" ]; then
    if ! validate_install_lock_ownership; then
      discard_install_written_generation "$metadata_tmp" "$metadata_tmp_write" 2>/dev/null || true
      fail "install lifecycle lock changed before metadata commit"
    fi
  else
    if ! validate_install_state_root "$bundle_root"; then
      discard_install_written_generation "$metadata_tmp" "$metadata_tmp_write" 2>/dev/null || true
      fail "installer metadata directory changed before commit: $metadata_dir"
    fi
  fi
  if [ -n "$install_transaction_journal" ]; then
    if ! publish_install_target_generation metadata "$metadata_tmp" "$metadata_path" normal regular; then
      discard_install_written_generation "$metadata_tmp" "$metadata_tmp_write" 2>/dev/null || true
      fail "installer metadata ownership changed before publication"
    fi
  elif [ "${THREADPOINT_INSTALL_SH_TEST_MODE:-0}" = 1 ]; then
    if ! install_atomic_move_noreplace "$metadata_tmp" "$metadata_path"; then
      discard_install_written_generation "$metadata_tmp" "$metadata_tmp_write" 2>/dev/null || true
      fail "could not publish test fixture installer metadata"
    fi
  else
    fail "durable install transaction is required before installer metadata mutation"
  fi
  info "Wrote installer metadata to $metadata_path"
}

require_regular_bundle_files() {
  bundle_root=$1

  bundle_entries | while IFS= read -r entry; do
    path=$bundle_root/$entry
    [ ! -L "$path" ] || fail "archive entry must not be a symlink: $entry"
    [ -f "$path" ] || fail "archive entry is not a regular file: $entry"
  done
}

require_regular_archive_entries() {
  archive_path=$1
  bundle_dir=$2
  table_file=$tmpdir/archive.table

  tar -tvf "$archive_path" >"$table_file" ||
    fail "could not inspect archive entry types"

  bundle_entries | while IFS= read -r entry; do
    full_entry=$bundle_dir/$entry
    status=0
    awk -v entry="$full_entry" '
			{
				needle = " " entry
				pos = index($0, needle)
				if (pos > 0) {
					rest = substr($0, pos + length(needle))
					found = 1
					if (substr($0, 1, 1) == "-" && rest == "") {
						ok = 1
					}
				}
			}
			END {
				if (!found) {
					exit 2
				}
				if (!ok) {
					exit 1
				}
			}
		' "$table_file" || status=$?
    case "$status" in
      0) ;;
      1) fail "archive entry is not a regular file: $entry" ;;
      2) fail "archive entry type listing is missing: $entry" ;;
      *) fail "could not inspect archive entry type: $entry" ;;
    esac
  done
}

validate_prepared_archive_entries() {
  expanded_archive=$1
  bundle_dir=$2
  expanded_types_archive=${3:-$expanded_archive}
  entries_file=$tmpdir/archive.entries
  header_count=0
  entry_count=0
  bundle_directory_count=0
  bin_directory_count=0
  scripts_directory_count=0
  binary_count=0
  readme_count=0
  license_count=0
  notice_count=0
  install_script_count=0
  uninstall_script_count=0

  tar -tf "$expanded_archive" >"$entries_file" ||
    fail "could not inspect archive contents"

  while IFS= read -r entry; do
    entry=${entry#./}
    header_count=$((header_count + 1))
    [ "$header_count" -le 9 ] || fail "archive contains too many headers"

    case "$entry" in
      "" | /* | .. | ../* | */.. | */../* | *\\*)
        fail "unsafe archive entry: $entry"
        ;;
    esac

    case "$entry" in
      "$bundle_dir/")
        bundle_directory_count=$((bundle_directory_count + 1))
        [ "$bundle_directory_count" -eq 1 ] || fail "archive contains a duplicate bundle directory header"
        continue
        ;;
      "$bundle_dir/bin/")
        bin_directory_count=$((bin_directory_count + 1))
        [ "$bin_directory_count" -eq 1 ] || fail "archive contains a duplicate bin directory header"
        continue
        ;;
      "$bundle_dir/scripts/")
        scripts_directory_count=$((scripts_directory_count + 1))
        [ "$scripts_directory_count" -eq 1 ] || fail "archive contains a duplicate scripts directory header"
        continue
        ;;
    esac

    entry_count=$((entry_count + 1))

    case "$entry" in
      "$bundle_dir/bin/threadpoint") binary_count=$((binary_count + 1)) ;;
      "$bundle_dir/README.md") readme_count=$((readme_count + 1)) ;;
      "$bundle_dir/LICENSE") license_count=$((license_count + 1)) ;;
      "$bundle_dir/NOTICE") notice_count=$((notice_count + 1)) ;;
      "$bundle_dir/scripts/install.sh") install_script_count=$((install_script_count + 1)) ;;
      "$bundle_dir/scripts/uninstall.sh") uninstall_script_count=$((uninstall_script_count + 1)) ;;
      *)
        fail "unexpected archive entry: $entry"
        ;;
    esac
  done <"$entries_file"

  [ "$entry_count" -eq 6 ] || fail "archive must contain exactly the bundled threadpoint binary, product docs, and installer scripts"
  [ "$binary_count" -eq 1 ] || fail "archive must contain $bundle_dir/bin/threadpoint"
  [ "$readme_count" -eq 1 ] || fail "archive must contain $bundle_dir/README.md"
  [ "$license_count" -eq 1 ] || fail "archive must contain $bundle_dir/LICENSE"
  [ "$notice_count" -eq 1 ] || fail "archive must contain $bundle_dir/NOTICE"
  [ "$install_script_count" -eq 1 ] || fail "archive must contain $bundle_dir/scripts/install.sh"
  [ "$uninstall_script_count" -eq 1 ] || fail "archive must contain $bundle_dir/scripts/uninstall.sh"

  require_regular_archive_entries "$expanded_types_archive" "$bundle_dir"
}

validate_archive_entries() {
  archive_path=$1
  bundle_dir=$(archive_bundle_dir "$archive_path")
  expanded_archive=$(prepare_bounded_archive "$archive_path") || fail "could not prepare bounded release archive"
  validate_prepared_archive_entries "$expanded_archive" "$bundle_dir"
}

validate_expanded_bundle_sizes() {
  bundle_root=$1
  aggregate_size=0
  bundle_entries | while IFS= read -r entry; do
    path=$bundle_root/$entry
    size=$(wc -c <"$path" | tr -d '[:space:]')
    case "$size" in
      '' | *[!0-9]*) fail "could not measure expanded archive entry: $entry" ;;
    esac
    case "$entry" in
      bin/threadpoint) limit=$max_expanded_binary_bytes ;;
      *) limit=$max_expanded_support_file_bytes ;;
    esac
    [ "$size" -le "$limit" ] || fail "expanded archive entry exceeds its $limit-byte limit: $entry"
    aggregate_size=$((aggregate_size + size))
    [ "$aggregate_size" -le "$max_expanded_bundle_bytes" ] ||
      fail "expanded archive bundle exceeds its $max_expanded_bundle_bytes-byte payload limit"
  done
}

extract_prepared_threadpoint_bundle() {
  expanded_archive=$1
  extract_dir=$2
  bundle_dir=$3
  extracted_bundle=$extract_dir/$bundle_dir

  mkdir -p "$extract_dir" || fail "could not create extraction directory"
  extraction_blocks=$(((max_expanded_binary_bytes + 511) / 512))
  (
    ulimit -f "$extraction_blocks"
    tar -xf "$expanded_archive" -C "$extract_dir"
  ) ||
    fail "could not extract threadpoint bundle"
  require_regular_bundle_files "$extracted_bundle"
  validate_expanded_bundle_sizes "$extracted_bundle"
}

validate_and_extract_prepared_archive() {
  retained_prepared_path=$1
  retained_primary_extract_dir=$2
  retained_bundle_dir=$3
  retained_verified_extract_dir=$4
  expected_prepared_digest=$5

  [ "${#expected_prepared_digest}" -eq 64 ] || fail "prepared archive digest is invalid"
  case "$expected_prepared_digest" in
    *[!0-9a-f]*) fail "prepared archive digest is invalid" ;;
  esac

  retain_prepared_archive "$retained_prepared_path"
  prepared_digest=$(sha256_file_bounded /dev/fd/3 "$max_expanded_archive_bytes") ||
    fail "retained prepared archive is not a bounded regular file"
  [ "$prepared_digest" = "$expected_prepared_digest" ] ||
    fail "prepared archive does not match the verified compressed generation"
  validate_prepared_archive_entries /dev/fd/5 "$retained_bundle_dir" /dev/fd/6
  extract_prepared_threadpoint_bundle /dev/fd/7 "$retained_primary_extract_dir" "$retained_bundle_dir"
  extract_prepared_threadpoint_bundle /dev/fd/9 "$retained_verified_extract_dir" "$retained_bundle_dir"
  [ "$(sha256_file_bounded /dev/fd/4 "$max_expanded_archive_bytes")" = "$prepared_digest" ] ||
    fail "retained prepared archive changed during validation or extraction"
  close_prepared_archive_descriptors
  extract_dir=$retained_primary_extract_dir
  bundle_dir=$retained_bundle_dir
  verified_extract_dir=$retained_verified_extract_dir
}

extract_threadpoint_bundle() {
  archive_path=$1
  extract_dir=$2
  bundle_dir=$(archive_bundle_dir "$archive_path")
  expanded_archive=$(prepare_bounded_archive "$archive_path") || fail "could not prepare bounded release archive"
  extract_prepared_threadpoint_bundle "$expanded_archive" "$extract_dir" "$bundle_dir"
}

extract_threadpoint_binary() {
  archive_path=$1
  extract_dir=$2
  bundle_dir=$(archive_bundle_dir "$archive_path")

  extract_threadpoint_bundle "$archive_path" "$extract_dir"
  [ -f "$extract_dir/$bundle_dir/bin/$binary" ] || fail "extracted threadpoint binary is missing"
}

install_bundle() {
  source_bundle=$1
  target_root=$2
  [ -n "$install_transaction_journal" ] || {
    printf 'error: durable install transaction is required before bundle mutation\n' >&2
    return 1
  }
  mkdir -p "$target_root" || fail "could not create bundle root: $target_root"
  entries_file=$(mktemp "${TMPDIR:-/tmp}/threadpoint-install-entries.XXXXXX") || fail "could not create bundle entry list"
  bundle_entries >"$entries_file"

  # Transaction preparation already retained and synced the coherent prior
  # bundle. This pass only validates sources and target types before mutation.
  validation_failed=0
  while IFS= read -r entry; do
    source=$source_bundle/$entry
    target=$target_root/$entry
    if ! ensure_bundle_entry_parent "$target_root" "$entry"; then
      printf 'error: bundle ancestor is not a physical directory: %s\n' "$entry" >&2
      validation_failed=1
      break
    fi

    expected_generation=$(expected_extracted_bundle_generation "$entry") || expected_generation=""
    # shellcheck disable=SC2086 # the snapshot emits two restricted tokens.
    set -- $expected_generation
    if [ "$#" -ne 2 ]; then
      printf 'error: bundle entry has no retained expected generation: %s\n' "$entry" >&2
      validation_failed=1
      break
    fi
    expected_identity=$1
    expected_digest=$2
    source_inspection=$(install_entry_inspect_helper "$source") || source_inspection=""
    # shellcheck disable=SC2086 # the helper emits three restricted tokens.
    set -- $source_inspection
    if [ "$#" -ne 3 ] || [ "$2" != regular ] || [ "$1" != "$expected_identity" ] || [ "$3" != "$expected_digest" ]; then
      printf 'error: bundle entry changed after its verified extraction snapshot: %s\n' "$entry" >&2
      validation_failed=1
      break
    fi
    if [ -L "$target" ] || { [ -e "$target" ] && [ ! -f "$target" ]; }; then
      printf 'error: refusing to replace non-regular bundle entry: %s\n' "$target" >&2
      validation_failed=1
      break
    fi
  done <"$entries_file"
  if [ "$validation_failed" -ne 0 ]; then
    rm -f "$entries_file"
    return 1
  fi

  install_failed=0
  while IFS= read -r entry; do
    source=$source_bundle/$entry
    target=$target_root/$entry
    mode=$(bundle_entry_mode "$entry")
    expected_generation=$(expected_extracted_bundle_generation "$entry") || expected_generation=""
    # shellcheck disable=SC2086 # the snapshot emits two restricted tokens.
    set -- $expected_generation
    if [ "$#" -ne 2 ]; then
      install_failed=1
      break
    fi
    expected_identity=$1
    expected_digest=$2

    if ! ensure_bundle_entry_parent "$target_root" "$entry"; then
      install_failed=1
      break
    fi
    temp_target=${target%/*}/.${target##*/}.install-${install_lock_token}
    if [ -e "$temp_target" ] || [ -L "$temp_target" ]; then
      install_failed=1
      break
    fi
    temp_generation=$(install_copy_bounded_expected_generation_as "$source" "$temp_target" \
      "$expected_identity" "$expected_digest" "$mode") || {
      install_failed=1
      break
    }
    if ! validate_bundle_entry_ancestors "$target_root" "$entry"; then
      discard_install_written_generation "$temp_target" "$temp_generation" 2>/dev/null || true
      install_failed=1
      break
    fi
    publish_install_target_generation "bundle:$entry" "$temp_target" "$target" normal regular || {
      discard_install_written_generation "$temp_target" "$temp_generation" 2>/dev/null || true
      install_failed=1
      break
    }
  done <"$entries_file"

  if [ "$install_failed" -eq 0 ]; then
    rm -f "$entries_file"
    return 0
  fi

  rm -f "$entries_file"
  printf 'error: could not install bundle; durable recovery journal retained at %s\n' "$install_transaction_journal" >&2
  return 1
}

link_binary() {
  target_binary=$1
  target_dir=$2
  link_path=$target_dir/$binary
  temp_link=$target_dir/.$binary.link.${install_lock_token:-$$}

  [ -n "$install_transaction_journal" ] || fail "durable install transaction is required before command link mutation"

  mkdir -p "$target_dir" || fail "could not create command directory: $target_dir"
  [ ! -d "$link_path" ] || fail "command path is a directory: $link_path"
  [ ! -e "$temp_link" ] && [ ! -L "$temp_link" ] || fail "command link temporary already exists: $temp_link"
  ln -s "$target_binary" "$temp_link" || {
    fail "could not create command symlink in $target_dir"
  }
  temp_link_inspection=$(install_entry_inspect_helper "$temp_link") || fail "could not retain command link temporary generation"
  # shellcheck disable=SC2086 # the helper wrapper emits three restricted tokens.
  set -- $temp_link_inspection
  temp_link_identity=$1
  temp_link_kind=$2
  temp_link_proof=$3
  [ "$temp_link_kind" = symlink ] || fail "command link temporary is not a symlink: $temp_link"
  publish_install_target_generation link "$temp_link" "$link_path" normal symlink || {
    if [ -e "$temp_link" ] || [ -L "$temp_link" ]; then
      install_remove_entry_as "$temp_link" "$temp_link_identity" "$temp_link_kind" "$temp_link_proof" 2>/dev/null ||
        warn "changed command link temporary retained at $temp_link"
    fi
    fail "command link ownership changed before publication: $link_path"
  }
}

if [ "${THREADPOINT_INSTALL_SH_TEST_MODE:-0}" = "1" ]; then
  # shellcheck disable=SC2317 # exit is the executed-script fallback when return is unavailable.
  return 0 2>/dev/null || exit 0
fi

while [ "$#" -gt 0 ]; do
  case "$1" in
    --dir)
      shift
      [ "$#" -gt 0 ] || fail "--dir requires a value"
      install_dir=$1
      ;;
    --dir=*)
      install_dir=${1#--dir=}
      ;;
    --version)
      shift
      [ "$#" -gt 0 ] || fail "--version requires a value"
      version=$1
      ;;
    --version=*)
      version=${1#--version=}
      ;;
    --skip-attestation)
      skip_attestation=1
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      fail "unknown option: $1"
      ;;
  esac
  shift
done

[ -n "$install_dir" ] || fail "command directory is empty; pass --dir or set THREADPOINT_INSTALL_DIR"
# BEGIN INSTALLER PATH VALIDATION
prepare_command_dir_for_path
# END INSTALLER PATH VALIDATION
[ -n "$threadpoint_home" ] || fail "THREADPOINT_HOME or HOME is required to locate the threadpoint bundle"
validate_product_home "$threadpoint_home" "threadpoint home"
validate_version

need_cmd awk
need_cmd chmod
need_cmd cp
need_cmd head
need_cmd ln
need_cmd mkdir
need_cmd mktemp
need_cmd mkfifo
need_cmd mv
need_cmd rm
need_cmd rmdir
need_cmd sed
need_cmd tar
need_cmd uname

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/threadpoint-install.XXXXXX")
trap cleanup EXIT HUP INT TERM

if [ "$version" = "latest" ]; then
  version=$(resolve_latest_version)
fi
enforce_release_policy "$version"

os=$(detect_os)
arch=$(detect_arch)
archive_version=$(archive_version_from_tag "$version")
archive="threadpoint_${archive_version}_${os}_${arch}.tar.gz"
release_base="https://github.com/${repo}/releases/download/${version}"

archive_path=$tmpdir/$archive
checksums_path=$tmpdir/checksums.txt
signature_path=$tmpdir/checksums.txt.sig
extract_dir=$tmpdir/extract
verified_extract_dir=$tmpdir/verified-extract

info "Downloading $archive from $release_base"
download "$release_base/$archive" "$archive_path" "$max_archive_bytes"
download "$release_base/checksums.txt" "$checksums_path" "$max_checksums_bytes"
download "$release_base/checksums.txt.sig" "$signature_path" "$max_signature_bytes"

info "Verifying checksum signature"
retain_release_metadata "$checksums_path" "$signature_path"
verify_checksum_signature /dev/fd/4 /dev/fd/3
checksums_generation_digest=$(sha256_file /dev/fd/5) || fail "could not hash retained checksum manifest"
archive_sha256=$(checksum_line_for_archive /dev/fd/7 "$archive" | awk '{ print $1 }') ||
  fail "retained checksums.txt does not contain an entry for $archive"
exec 3<&- 4<&- 5<&- 7<&-
[ "${#archive_sha256}" -eq 64 ] || fail "checksums.txt contains an invalid digest for $archive"
case "$archive_sha256" in
  *[!0-9a-f]*) fail "checksums.txt contains an invalid digest for $archive" ;;
esac

bundle_dir=$(archive_bundle_dir "$archive_path")
retain_release_archive "$archive_path"

info "Verifying checksum"
verify_checksum /dev/fd/3 "$archive_sha256" "$archive"

info "Verifying GitHub Artifact Attestations"
verify_attestations /dev/fd/4 /dev/fd/6 "$version"
[ "$(sha256_file /dev/fd/9)" = "$checksums_generation_digest" ] ||
  fail "retained checksum manifest changed during verification"
exec 6<&- 9<&-

info "Inspecting archive"
prepared_archive=$(prepare_bounded_archive /dev/fd/5) || fail "could not prepare bounded release archive"
prepared_archive_digest=$(sha256_expanded_archive /dev/fd/7) ||
  fail "could not hash the verified expanded archive generation"
[ "${#prepared_archive_digest}" -eq 64 ] || fail "verified expanded archive digest is invalid"
case "$prepared_archive_digest" in
  *[!0-9a-f]*) fail "verified expanded archive digest is invalid" ;;
esac
verify_checksum /dev/fd/8 "$archive_sha256" "$archive"
close_release_archive_descriptors
validate_and_extract_prepared_archive "$prepared_archive" "$extract_dir" "$bundle_dir" "$verified_extract_dir" \
  "$prepared_archive_digest"

[ -n "$threadpoint_home" ] || fail "THREADPOINT_HOME or HOME is required to locate the threadpoint bundle"
mkdir -p "$threadpoint_home" || fail "could not create bundle root: $threadpoint_home"
bundle_root=$(cd "$threadpoint_home" && pwd -P)
mkdir -p "$install_dir" || fail "could not create command directory: $install_dir"
# BEGIN INSTALLER PATH NORMALIZATION
command_dir_for_path=$(CDPATH='' cd -- "$command_dir_for_path" && pwd -L)
# END INSTALLER PATH NORMALIZATION
install_dir=$(cd "$install_dir" && pwd -P)
command_path=$install_dir/$binary
lock_helper=$verified_extract_dir/$bundle_dir/bin/$binary
retain_bootstrap_helper "$lock_helper" || fail "could not retain verified installer lifecycle helper"
acquire_install_lifecycle_lock "$bundle_root" "$command_path" "$lock_helper" \
  "$bootstrap_helper_executable" "$bootstrap_helper_identity" "$bootstrap_helper_digest" ||
  fail "could not acquire install lifecycle lock for $command_path"
exec 3<&-
recover_install_transaction "$bundle_root" "$command_path" || fail "could not recover an interrupted install transaction"
snapshot_extracted_bundle_generations "$extract_dir/$bundle_dir" "$verified_extract_dir/$bundle_dir" ||
  fail "extracted release bundle does not match its verified archive generation"
binary_path=$bundle_root/bin/$binary
metadata_path=$(install_lifecycle_path "$bundle_root" "$binary_path" '.json')
prepare_install_transaction "$bundle_root" "$command_path" "$bundle_root" "$metadata_path" || fail "could not prepare durable install transaction"
install_bundle "$extract_dir/$bundle_dir" "$bundle_root" || fail "could not install the release bundle"
managed_dir=$bundle_root/bin
managed_dir=$(cd "$managed_dir" && pwd -P)
bundle_root=$(cd "$bundle_root" && pwd -P)
binary_path=$managed_dir/$binary
link_binary "$binary_path" "$install_dir"
link_path=$install_dir/$binary
install_journal_generation_matches "bundle:bin/threadpoint" produced "$binary_path" ||
  fail "installed threadpoint binary no longer matches the published transaction generation"
binary_sha256=$(install_journal_regular_digest "bundle:bin/threadpoint" produced) ||
  fail "could not read the installed threadpoint binary digest from the transaction journal"
write_install_metadata "$install_dir" "$binary_path" "$link_path" "$archive" "$archive_sha256" "$release_base" "$binary_sha256" "$version" "$bundle_root"
complete_install_transaction || fail "could not finalize durable install transaction"
release_install_lifecycle_lock || fail "could not release the install lifecycle guard"

info "Installed threadpoint bundle to $bundle_root"
info "Linked threadpoint at $link_path"

# BEGIN INSTALLER PATH COMPLETION
print_path_guidance
# END INSTALLER PATH COMPLETION
