#!/bin/sh
# shellcheck disable=SC1090,SC2016,SC2030,SC2031,SC2034,SC2154
set -eu

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
INSTALL_SH="$ROOT_DIR/scripts/install.sh"
# BEGIN INSTALLER PATH TEST DEPENDENCIES
for test_shell in sh bash zsh fish; do
  if ! command -v "$test_shell" >/dev/null 2>&1; then
    printf 'installer tests require %s; install Bash, Zsh, and Fish before running this suite\n' "$test_shell" >&2
    exit 1
  fi
done
# END INSTALLER PATH TEST DEPENDENCIES

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/threadpoint-installer-safety.XXXXXX")

cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT HUP INT TERM

LOCK_HELPER=$tmpdir/threadpoint-lock-helper
(cd "$ROOT_DIR" && go build -o "$LOCK_HELPER" ./cmd/threadpoint)
THREADPOINT_INSTALL_LOCK_HELPER=$LOCK_HELPER
export THREADPOINT_INSTALL_LOCK_HELPER

fail() {
  printf 'installer archive safety test failed: %s\n' "$*" >&2
  exit 1
}

bounded_lifecycle_case=$tmpdir/bounded-lifecycle-inspection
mkdir -p "$bounded_lifecycle_case"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"

  regular=$bounded_lifecycle_case/regular
  printf '%s\n' reviewed >"$regular"
  [ "$(install_regular_digest_helper "$regular")" = "$(sha256_file "$regular")" ]

  fifo=$bounded_lifecycle_case/fifo
  mkfifo "$fifo"
  if install_regular_digest_helper "$fifo" >/dev/null 2>&1; then
    exit 1
  fi

  oversized=$bounded_lifecycle_case/oversized
  truncate -s 268435457 "$oversized"
  if install_regular_digest_helper "$oversized" >/dev/null 2>&1; then
    exit 1
  fi

  replacement=$bounded_lifecycle_case/replacement
  replacement_outside=$bounded_lifecycle_case/outside
  printf '%s\n' reviewed >"$replacement"
  printf '%s\n' outside >"$replacement_outside"
  expected_generation=$(install_generation_json regular "$replacement")
  replacement_injected=0
  # shellcheck disable=SC2329 # invoked by install_generation_matches below.
  install_entry_inspect_helper() {
    replacement_path=$1
    if [ "$replacement_injected" -eq 0 ]; then
      replacement_injected=1
      mv "$replacement_path" "$replacement_path.reviewed"
      ln -s "$replacement_outside" "$replacement_path"
    fi
    "$THREADPOINT_INSTALL_LOCK_HELPER" __threadpoint-install-entry-inspect "$replacement_path" 8>&-
  }
  if install_generation_matches "$expected_generation" "$replacement"; then
    exit 1
  fi
  [ "$(cat "$replacement.reviewed")" = reviewed ]
  [ "$(cat "$replacement_outside")" = outside ]
); then
  fail "lifecycle hashing accepted a FIFO, oversized file, or final-component replacement"
fi

recovery_function=$(sed -n '/^recover_install_transaction() {/,/^prepare_install_transaction() {/p' "$INSTALL_SH")
recovery_validation_function=$(sed -n '/^validate_install_transaction_checksums() {/,/^install_generation_json() {/p' "$INSTALL_SH")
printf '%s\n' "$recovery_function" | grep -Fq 'validate_install_transaction_checksums' ||
  fail 'shell recovery does not validate its backup generation'
printf '%s\n' "$recovery_validation_function" | grep -Fq 'backup-prior-as' ||
  fail "shell recovery validation is not bound to the journal's prior backup digests"
printf '%s\n' "$recovery_function" | grep -Fq 'install_copy_bounded_expected_as' ||
  fail 'shell recovery does not use bounded expected-generation copies'
if printf '%s\n' "$recovery_function" | grep -Eq '(^|[[:space:]])cp[[:space:]]'; then
  fail 'shell recovery still copies backup generations through ambient cp'
fi
if printf '%s\n' "$recovery_function" | grep -Fq 'sha256_file'; then
  fail 'shell recovery still hashes backup generations through ambient paths'
fi
archive_validation_function=$(sed -n '/^validate_prepared_archive_entries() {/,/^validate_archive_entries() {/p' "$INSTALL_SH")
printf '%s\n' "$archive_validation_function" | grep -Fq 'header_count=$((header_count + 1))' ||
  fail 'archive validation does not count every tar header'
printf '%s\n' "$archive_validation_function" | grep -Fq '[ "$header_count" -le 9 ]' ||
  fail 'archive validation does not enforce its total-header cap'
printf '%s\n' "$archive_validation_function" | grep -Fq 'duplicate bin directory header' ||
  fail 'archive validation does not reject duplicate allowed directory headers'
install_bundle_function=$(sed -n '/^install_bundle() {/,/^link_binary() {/p' "$INSTALL_SH")
printf '%s\n' "$install_bundle_function" | grep -Fq 'install_copy_bounded_expected_generation_as' ||
  fail 'bundle installation does not use the exact bounded-copy helper'
if printf '%s\n' "$install_bundle_function" | grep -Eq '(^|[[:space:]])cp[[:space:]]'; then
  fail 'bundle installation still copies extracted generations through ambient cp'
fi
installer_main=$(sed -n '/^info "Verifying checksum signature"/,/^complete_install_transaction/p' "$INSTALL_SH")
printf '%s\n' "$installer_main" | grep -Fq 'retain_release_metadata' ||
  fail 'installer does not retain the signed checksum generation'
printf '%s\n' "$installer_main" | grep -Fq 'validate_and_extract_prepared_archive' ||
  fail 'installer does not retain one prepared archive generation through extraction'
printf '%s\n' "$installer_main" | grep -Fq 'snapshot_extracted_bundle_generations' ||
  fail 'installer does not bind installed files to the verified extraction snapshot'

wait_for_file() {
  awaited=$1
  waits=0
  while [ ! -e "$awaited" ]; do
    waits=$((waits + 1))
    [ "$waits" -lt 2000 ] || fail "timed out waiting for $awaited"
    sleep 0.01
  done
}

make_managed_threadpoint_fixture() {
  fixture_root=$1
  fixture_home=$fixture_root/home
  fixture_bin=$fixture_root/bin
  fixture_command=$fixture_bin/threadpoint
  mkdir -p "$fixture_home/bin" "$fixture_home/scripts" "$fixture_bin"
  cp "$LOCK_HELPER" "$fixture_home/bin/threadpoint"
  printf '%s\n' 'fixture README' >"$fixture_home/README.md"
  printf '%s\n' 'fixture LICENSE' >"$fixture_home/LICENSE"
  printf '%s\n' 'fixture NOTICE' >"$fixture_home/NOTICE"
  cp "$ROOT_DIR/scripts/install.sh" "$fixture_home/scripts/install.sh"
  cp "$ROOT_DIR/scripts/uninstall.sh" "$fixture_home/scripts/uninstall.sh"
  chmod 0755 "$fixture_home/bin/threadpoint" "$fixture_home/scripts/install.sh" "$fixture_home/scripts/uninstall.sh"
  ln -s "$fixture_home/bin/threadpoint" "$fixture_command"
  (
    THREADPOINT_INSTALL_SH_TEST_MODE=1
    export THREADPOINT_INSTALL_SH_TEST_MODE
    . "$INSTALL_SH"
    fixture_binary=$fixture_home/bin/threadpoint
    write_install_metadata "$fixture_bin" "$fixture_binary" "$fixture_command" archive.tar.gz "$(sha256_file "$fixture_binary")" https://example.test/v1 "$(sha256_file "$fixture_binary")" v1 "$fixture_home"
  )
}

make_archive() {
  archive=$1
  shift
  source_dir=$tmpdir/archive-src
  rm -rf "$source_dir"
  mkdir -p "$source_dir"
  for name in "$@"; do
    parent=${name%/*}
    if [ "$parent" != "$name" ]; then
      mkdir -p "$source_dir/$parent"
    fi
    printf '%s\n' "fixture $name" >"$source_dir/$name"
  done
  (
    cd "$source_dir"
    tar -czf "$archive" "$@"
  )
}

make_python_archive() {
  archive=$1
  python3 - "$archive" <<'PY'
import io
import sys
import tarfile

archive = sys.argv[1]
with tarfile.open(archive, "w:gz") as tar:
    for raw in sys.stdin:
        raw = raw.rstrip("\n")
        if not raw:
            continue
        kind, name = raw.split(" ", 1)
        info = tarfile.TarInfo(name)
        if kind == "file":
            body = b"synthetic threadpoint fixture\n"
            info.size = len(body)
            info.mode = 0o755 if name.endswith("/bin/threadpoint") else 0o644
            tar.addfile(info, io.BytesIO(body))
        elif kind == "symlink":
            info.type = tarfile.SYMTYPE
            info.linkname = "/bin/sh"
            tar.addfile(info)
        elif kind == "hardlink":
            info.type = tarfile.LNKTYPE
            info.linkname = name.split("/", 1)[0] + "/README.md"
            tar.addfile(info)
        elif kind == "directory":
            info.type = tarfile.DIRTYPE
            info.mode = 0o755
            tar.addfile(info)
        else:
            raise SystemExit(f"unknown kind {kind}")
PY
}

make_expanded_bomb_archive() {
  archive=$1
  python3 - "$archive" <<'PY'
import io
import sys
import tarfile

class Zeros(io.RawIOBase):
    def __init__(self, remaining):
        self.remaining = remaining
    def readable(self):
        return True
    def readinto(self, target):
        if self.remaining <= 0:
            return 0
        count = min(len(target), self.remaining)
        target[:count] = b"\0" * count
        self.remaining -= count
        return count

archive = sys.argv[1]
bundle = "expanded-bomb"
with tarfile.open(archive, "w:gz") as tar:
    for name in ["README.md", "LICENSE", "NOTICE", "scripts/install.sh", "scripts/uninstall.sh"]:
        body = b"fixture\n"
        info = tarfile.TarInfo(f"{bundle}/{name}")
        info.size = len(body)
        info.mode = 0o755 if name.startswith("scripts/") else 0o644
        tar.addfile(info, io.BytesIO(body))
    info = tarfile.TarInfo(f"{bundle}/bin/threadpoint")
    info.size = 131072
    info.mode = 0o755
    tar.addfile(info, io.BufferedReader(Zeros(info.size)))
PY
}

with_installer_functions() {
  (
    THREADPOINT_INSTALL_SH_TEST_MODE=1 . "$INSTALL_SH"
    tmpdir=$1
    shift
    "$@"
  )
}

# BEGIN INSTALLER PATH TESTS
# Execute the printed commands as customers would, with synthetic homes only.
python3 - "$INSTALL_SH" "$tmpdir/path-guidance" <<'PYTEST'
import os
import pathlib
import shutil
import subprocess
import sys

installer = pathlib.Path(sys.argv[1]).resolve()
root = pathlib.Path(sys.argv[2])
root.mkdir()
product = "threadpoint"
home = root / "home"
home.mkdir()
config = home / ".config"
config.mkdir()
for name in (".bashrc", ".bash_profile", ".zshrc"):
    (home / name).write_text("# preserve existing shell configuration\n")
base_path = os.environ["PATH"]
shells = {name: shutil.which(name) for name in ("sh", "bash", "zsh", "fish")}
assert all(shells.values()), f"Install required test shells: {shells}"

def guidance(directory, search_path=base_path, logical_dir=""):
    original_files = {p: p.read_bytes() for p in home.rglob("*") if p.is_file()}
    result = subprocess.run(
        [shells["sh"], "-c", '. "$1"; command_dir_for_path=$TEST_LOGICAL_DIR; print_path_guidance', "installer", str(installer)],
        env={
            "HOME": str(home), "XDG_CONFIG_HOME": str(config), "PATH": search_path,
            "TEST_LOGICAL_DIR": str(logical_dir),
            f"{product.upper()}_INSTALL_DIR": str(directory),
            f"{product.upper()}_INSTALL_SH_TEST_MODE": "1",
        },
        cwd=root, input="", text=True, capture_output=True, check=True, timeout=10,
    )
    assert result.stdout == "", result.stdout
    assert {p: p.read_bytes() for p in home.rglob("*") if p.is_file()} == original_files
    return result.stderr

def fixture(directory):
    directory.mkdir(parents=True, exist_ok=True)
    binary = directory / product
    binary.write_text("#!/bin/sh\nprintf '%s\\n' fixture-version\n")
    binary.chmod(0o755)
    return binary

directory = home / ".local" / "bin"
fixture(directory)
for entry in (str(directory), str(directory) + "/", os.path.relpath(directory, root)):
    for search_path in (f"{entry}:{base_path}", f"{base_path}:{entry}", f"/before:{entry}:{base_path}"):
        assert guidance(directory, search_path) == "", search_path
assert "export PATH=" in guidance(directory, f"{directory}-other:{base_path}")
assert "export PATH=" in guidance(directory, f"{directory}/nested:{base_path}")
link = root / "logical-bin"
link.symlink_to(directory, target_is_directory=True)
assert guidance(directory, f"{link}:{base_path}") == ""
assert guidance(link, f"{directory}:{base_path}") == ""
assert str(link) in guidance(directory, logical_dir=link)
broken = root / "broken-bin"
broken.symlink_to(root / "missing", target_is_directory=True)
assert "export PATH=" in guidance(directory, f"{broken}:{base_path}")

colon = root / "colon:bin"
fixture(colon)
for search_path in (base_path, f"{colon}:{base_path}"):
    output = guidance(colon, search_path)
    assert "contains a colon" in output and "--dir" in output, output
    assert "export PATH=" not in output, output

# Invalid options, environment overrides, and defaults must fail before any work.
fakebin = root / "fakebin"
fakebin.mkdir()
network_marker = root / "network-called"
curl = fakebin / "curl"
curl.write_text('#!/bin/sh\nprintf called > "$PATH_TEST_NETWORK_MARKER"\nexit 97\n')
curl.chmod(0o755)
for mode in ("argument", "environment", "default"):
    invalid_dir = root / f"{mode}:bin"
    state_home = root / f"{mode}-state"
    env = {
        "HOME": str(home), "PATH": f"{fakebin}:{base_path}",
        "PATH_TEST_NETWORK_MARKER": str(network_marker),
        f"{product.upper()}_HOME": str(state_home),
    }
    args = [shells["sh"], str(installer)]
    if mode == "argument":
        args += ["--dir", str(invalid_dir)]
    elif mode == "environment":
        env[f"{product.upper()}_INSTALL_DIR"] = str(invalid_dir)
    else:
        env["HOME"] = str(invalid_dir)
    result = subprocess.run(args, env=env, cwd=root, input="", text=True, capture_output=True, timeout=10)
    assert result.returncode != 0 and "contains a colon" in result.stderr, result
    assert not network_marker.exists() and not invalid_dir.exists() and not state_home.exists()

special_name = """quote' "$HOME" $(touch expanded) """ + chr(96) + "touch expanded2" + chr(96) + r" \ bin"
for directory, logical_dir in ((directory, ""), (root / "custom bin", ""), (root / special_name, ""), (directory, link)):
    fixture(directory)
    command_directory = logical_dir or directory
    output = guidance(directory, logical_dir=logical_dir)
    assert f"  {product} version" in output, output
    commands = {
        "sh": next(line.strip() for line in output.splitlines() if line.startswith("  export PATH=")),
        "fish": next(line.strip() for line in output.splitlines() if line.startswith("  fish_add_path ")),
    }
    for name, executable in shells.items():
        for path_state in (("existing",) if name == "fish" else ("existing", "empty", "unset")):
            shell_home = root / f"{name}-home"
            shell_home.mkdir()
            command = commands["fish" if name == "fish" else "sh"]
            initial = {"existing": "", "empty": "PATH=\n", "unset": "unset PATH\n"}[path_state]
            print_path = "string join : $PATH" if name == "fish" else """printf '%s\\n' "$PATH" """
            # Fish needs its bundled startup handlers to sync fish_user_paths into PATH.
            flags = {"sh": [], "bash": ["--noprofile", "--norc"], "zsh": ["-f"], "fish": []}[name]
            result = subprocess.run(
                [executable, *flags, "-c", f"{initial}{command}\ncommand -v {product}\n{product} version\n{print_path}"],
                env={"HOME": str(shell_home), "XDG_CONFIG_HOME": str(shell_home / ".config"), "PATH": base_path},
                cwd=root, text=True, capture_output=True, check=True, timeout=10,
            )
            lines = result.stdout.splitlines()
            assert lines[:2] == [str(command_directory / product), "fixture-version"], (name, path_state, result.stdout, result.stderr)
            expected = f"{command_directory}:{base_path}" if path_state == "existing" else str(command_directory)
            assert lines[2] == expected, (name, path_state, lines[2])
            shutil.rmtree(shell_home)
    assert not (root / "expanded").exists()
    assert not (root / "expanded2").exists()

print(f"{product} PATH guidance passed in sh, Bash, Zsh, and Fish")
PYTEST
# END INSTALLER PATH TESTS

expect_validate_ok() {
  name=$1
  archive=$2
  case_tmp=$tmpdir/$name
  mkdir -p "$case_tmp"
  with_installer_functions "$case_tmp" validate_archive_entries "$archive" ||
    fail "$name should have passed validation"
}

expect_validate_fail() {
  name=$1
  archive=$2
  case_tmp=$tmpdir/$name
  mkdir -p "$case_tmp"
  if with_installer_functions "$case_tmp" validate_archive_entries "$archive"; then
    fail "$name should have failed validation"
  fi
}

expect_extract_fail() {
  name=$1
  archive=$2
  case_tmp=$tmpdir/$name
  mkdir -p "$case_tmp"
  if with_installer_functions "$case_tmp" extract_threadpoint_binary "$archive" "$case_tmp/extract"; then
    fail "$name should have failed extraction"
  fi
}

official=$tmpdir/official.tar.gz
make_archive "$official" \
  official/bin/threadpoint \
  official/README.md \
  official/LICENSE \
  official/NOTICE \
  official/scripts/install.sh \
  official/scripts/uninstall.sh
expect_validate_ok official "$official"

metadata_generation_case=$tmpdir/metadata-generation
mkdir -p "$metadata_generation_case"
metadata_digest=$(printf '%064d' 0 | tr 0 a)
metadata_checksums=$metadata_generation_case/checksums.txt
metadata_signature=$metadata_generation_case/checksums.txt.sig
printf '%s  %s\n' "$metadata_digest" bound.tar.gz >"$metadata_checksums"
printf '%s\n' signed-generation >"$metadata_signature"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  tmpdir=$metadata_generation_case
  retain_release_metadata "$metadata_checksums" "$metadata_signature"
  [ "$(cat /dev/fd/3)" = signed-generation ]
  cat /dev/fd/4 >/dev/null
  retained_metadata_digest=$(sha256_file /dev/fd/5)
  printf '%s  %s\n' "$(printf '%064d' 0 | tr 0 b)" bound.tar.gz >"$metadata_checksums"
  printf '%s\n' replacement >"$metadata_signature"
  [ "$(checksum_line_for_archive /dev/fd/7 bound.tar.gz | awk '{ print $1 }')" = "$metadata_digest" ]
  grep -Fq "$metadata_digest" /dev/fd/6
  [ "$(sha256_file /dev/fd/9)" = "$retained_metadata_digest" ]
  close_release_metadata_descriptors
); then
  fail 'signed checksum generation was reopened through a replaceable pathname'
fi

prepared_generation_case=$tmpdir/prepared-generation
mkdir -p "$prepared_generation_case/work" "$prepared_generation_case/replacement/bound/bin" "$prepared_generation_case/replacement/bound/scripts"
bound_archive=$prepared_generation_case/bound.tar.gz
replacement_archive=$prepared_generation_case/replacement.tar.gz
make_archive "$bound_archive" \
  bound/bin/threadpoint \
  bound/README.md \
  bound/LICENSE \
  bound/NOTICE \
  bound/scripts/install.sh \
  bound/scripts/uninstall.sh
printf '%s\n' replacement >"$prepared_generation_case/replacement/bound/bin/threadpoint"
printf '%s\n' replacement >"$prepared_generation_case/replacement/bound/README.md"
printf '%s\n' replacement >"$prepared_generation_case/replacement/bound/LICENSE"
printf '%s\n' replacement >"$prepared_generation_case/replacement/bound/NOTICE"
printf '%s\n' replacement >"$prepared_generation_case/replacement/bound/scripts/install.sh"
printf '%s\n' replacement >"$prepared_generation_case/replacement/bound/scripts/uninstall.sh"
(cd "$prepared_generation_case/replacement" && tar -czf "$replacement_archive" bound)
cp "$bound_archive" "$prepared_generation_case/verified-copy.tar.gz"
cp "$replacement_archive" "$prepared_generation_case/replacement-copy.tar.gz"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  tmpdir=$prepared_generation_case
  retain_release_archive "$bound_archive"
  mv "$replacement_archive" "$bound_archive"
  prepared_archive=$(prepare_bounded_archive /dev/fd/5)
  close_release_archive_descriptors
  retain_prepared_archive "$prepared_archive"
  validate_prepared_archive_entries /dev/fd/5 bound /dev/fd/6
  gzip -dc "$bound_archive" >"$prepared_archive"
  extract_prepared_threadpoint_bundle /dev/fd/7 "$prepared_generation_case/extract" bound
  close_prepared_archive_descriptors
  grep -Fq 'fixture bound/README.md' "$prepared_generation_case/extract/bound/README.md"
); then
  fail 'retained archive generation was reopened through a replaceable pathname'
fi

prepared_binding_case=$tmpdir/prepared-generation-binding
mkdir -p "$prepared_binding_case/work"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  tmpdir=$prepared_binding_case/work
  prepared_archive=$(prepare_bounded_archive "$prepared_generation_case/verified-copy.tar.gz")
  expected_prepared_digest=$(sha256_file "$prepared_archive")
  rm -f "$prepared_archive"
  gzip -dc "$prepared_generation_case/replacement-copy.tar.gz" >"$prepared_archive"
  validate_and_extract_prepared_archive "$prepared_archive" \
    "$prepared_binding_case/extract" bound "$prepared_binding_case/verified-extract" \
    "$expected_prepared_digest"
); then
  fail 'prepared archive replacement before descriptor retention escaped its verified generation'
fi

prepared_hash_bound_case=$tmpdir/prepared-hash-bound
mkdir -p "$prepared_hash_bound_case"
truncate -s 65537 "$prepared_hash_bound_case/oversized.tar"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  sha256_file_bounded "$prepared_hash_bound_case/oversized.tar" 65536 >/dev/null
); then
  fail 'retained prepared archive hashing accepted a sparse file beyond its byte limit'
fi

extracted_generation_case=$tmpdir/extracted-generation
mkdir -p "$extracted_generation_case/primary/bin" "$extracted_generation_case/primary/scripts" \
  "$extracted_generation_case/verified/bin" "$extracted_generation_case/verified/scripts" \
  "$extracted_generation_case/destination"
for entry in README.md LICENSE NOTICE scripts/install.sh scripts/uninstall.sh; do
  printf 'verified %s\n' "$entry" >"$extracted_generation_case/primary/$entry"
  printf 'verified %s\n' "$entry" >"$extracted_generation_case/verified/$entry"
done
cp "$LOCK_HELPER" "$extracted_generation_case/primary/bin/threadpoint"
cp "$LOCK_HELPER" "$extracted_generation_case/verified/bin/threadpoint"
chmod 0755 "$extracted_generation_case/primary/bin/threadpoint" \
  "$extracted_generation_case/verified/bin/threadpoint" \
  "$extracted_generation_case/primary/scripts/install.sh" \
  "$extracted_generation_case/primary/scripts/uninstall.sh" \
  "$extracted_generation_case/verified/scripts/install.sh" \
  "$extracted_generation_case/verified/scripts/uninstall.sh"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  tmpdir=$extracted_generation_case
  install_lock_helper_path=$LOCK_HELPER
  snapshot_extracted_bundle_generations "$extracted_generation_case/primary" "$extracted_generation_case/verified"
  expected_readme=$(expected_extracted_bundle_generation README.md)
  # shellcheck disable=SC2086 # the snapshot emits two restricted tokens.
  set -- $expected_readme
  mv "$extracted_generation_case/primary/README.md" "$extracted_generation_case/primary/README.md.reviewed"
  printf '%s\n' replacement >"$extracted_generation_case/primary/README.md"
  if install_copy_bounded_expected_generation_as \
    "$extracted_generation_case/primary/README.md" "$extracted_generation_case/destination/README.md" \
    "$1" "$2" 0644; then
    exit 1
  fi
  [ ! -e "$extracted_generation_case/destination/README.md" ]

  retain_bootstrap_helper "$extracted_generation_case/verified/bin/threadpoint"
  mv "$extracted_generation_case/verified/bin/threadpoint" \
    "$extracted_generation_case/verified/bin/threadpoint.reviewed"
  cat >"$extracted_generation_case/verified/bin/threadpoint" <<EOF
#!/bin/sh
: >"$extracted_generation_case/replacement-executed"
EOF
  chmod 0755 "$extracted_generation_case/verified/bin/threadpoint"
  if "$bootstrap_helper_executable" __threadpoint-install-copy-bounded-as \
    "$extracted_generation_case/verified/bin/threadpoint" \
    "$extracted_generation_case/destination/helper" \
    "$bootstrap_helper_identity" "$bootstrap_helper_digest" 0700 8>&-; then
    exit 1
  fi
  exec 3<&-
  [ ! -e "$extracted_generation_case/replacement-executed" ]
); then
  fail 'extracted bundle or bootstrap helper replacement escaped its retained generation'
fi

expanded_bomb=$tmpdir/expanded-bomb.tar.gz
make_expanded_bomb_archive "$expanded_bomb"
THREADPOINT_TEST_MAX_EXPANDED_ARCHIVE_BYTES=65536
export THREADPOINT_TEST_MAX_EXPANDED_ARCHIVE_BYTES
expect_validate_fail expanded-bomb "$expanded_bomb"
unset THREADPOINT_TEST_MAX_EXPANDED_ARCHIVE_BYTES

traversal=$tmpdir/traversal.tar.gz
printf '%s\n' \
  'file traversal/bin/threadpoint' \
  'file traversal/README.md' \
  'file traversal/LICENSE' \
  'file traversal/NOTICE' \
  'file traversal/scripts/install.sh' \
  'file traversal/scripts/uninstall.sh' \
  'file ../evil' | make_python_archive "$traversal"
expect_validate_fail traversal "$traversal"

absolute=$tmpdir/absolute.tar.gz
printf '%s\n' \
  'file absolute/bin/threadpoint' \
  'file absolute/README.md' \
  'file absolute/LICENSE' \
  'file absolute/NOTICE' \
  'file absolute/scripts/install.sh' \
  'file absolute/scripts/uninstall.sh' \
  'file /evil' | make_python_archive "$absolute"
expect_validate_fail absolute "$absolute"

backslash=$tmpdir/backslash.tar.gz
printf '%s\n' \
  'file backslash/bin/threadpoint' \
  'file backslash/README.md' \
  'file backslash/LICENSE' \
  'file backslash/NOTICE' \
  'file backslash/scripts/install.sh' \
  'file backslash/scripts/uninstall.sh' \
  'file dir\evil' | make_python_archive "$backslash"
expect_validate_fail backslash "$backslash"

missing_bundle=$tmpdir/missing_bundle.tar.gz
printf '%s\n' \
  'file bin/threadpoint' \
  'file README.md' \
  'file LICENSE' \
  'file NOTICE' \
  'file scripts/install.sh' \
  'file scripts/uninstall.sh' | make_python_archive "$missing_bundle"
expect_validate_fail missing-bundle "$missing_bundle"

unexpected=$tmpdir/unexpected.tar.gz
make_archive "$unexpected" \
  unexpected/bin/threadpoint \
  unexpected/README.md \
  unexpected/LICENSE \
  unexpected/NOTICE \
  unexpected/scripts/install.sh \
  unexpected/scripts/uninstall.sh \
  unexpected/CHANGELOG.md
expect_validate_fail unexpected "$unexpected"

duplicate=$tmpdir/duplicate.tar.gz
printf '%s\n' \
  'file duplicate/bin/threadpoint' \
  'file duplicate/bin/threadpoint' \
  'file duplicate/README.md' \
  'file duplicate/LICENSE' \
  'file duplicate/NOTICE' \
  'file duplicate/scripts/install.sh' \
  'file duplicate/scripts/uninstall.sh' | make_python_archive "$duplicate"
expect_validate_fail duplicate "$duplicate"

header_flood=$tmpdir/header-flood.tar.gz
{
  printf '%s\n' \
    'file header-flood/bin/threadpoint' \
    'file header-flood/README.md' \
    'file header-flood/LICENSE' \
    'file header-flood/NOTICE' \
    'file header-flood/scripts/install.sh' \
    'file header-flood/scripts/uninstall.sh'
  header_index=0
  while [ "$header_index" -lt 64 ]; do
    printf '%s\n' 'directory header-flood/bin/'
    header_index=$((header_index + 1))
  done
} | make_python_archive "$header_flood"
expect_validate_fail header-flood "$header_flood"

symlink=$tmpdir/symlink.tar.gz
printf '%s\n' \
  'symlink symlink/bin/threadpoint' \
  'file symlink/README.md' \
  'file symlink/LICENSE' \
  'file symlink/NOTICE' \
  'file symlink/scripts/install.sh' \
  'file symlink/scripts/uninstall.sh' | make_python_archive "$symlink"
expect_validate_fail symlink-listing "$symlink"
expect_extract_fail symlink "$symlink"

hardlink=$tmpdir/hardlink.tar.gz
printf '%s\n' \
  'hardlink hardlink/bin/threadpoint' \
  'file hardlink/README.md' \
  'file hardlink/LICENSE' \
  'file hardlink/NOTICE' \
  'file hardlink/scripts/install.sh' \
  'file hardlink/scripts/uninstall.sh' | make_python_archive "$hardlink"
expect_validate_fail hardlink-listing "$hardlink"

install_case=$tmpdir/install
mkdir -p "$install_case/source/bin" "$install_case/source/scripts" "$install_case/target/bin" "$install_case/target/backups" "$install_case/target/updates/cache"
printf '%s\n' old >"$install_case/target/bin/threadpoint"
printf '%s\n' keep-backup >"$install_case/target/backups/run.json"
printf '%s\n' keep-cache >"$install_case/target/updates/cache/latest.json"
printf '%s\n' new >"$install_case/source/bin/threadpoint"
printf '%s\n' readme >"$install_case/source/README.md"
printf '%s\n' license >"$install_case/source/LICENSE"
printf '%s\n' notice >"$install_case/source/NOTICE"
printf '%s\n' install >"$install_case/source/scripts/install.sh"
printf '%s\n' uninstall >"$install_case/source/scripts/uninstall.sh"
if with_installer_functions "$install_case" install_bundle "$install_case/source" "$install_case/target"; then
  fail "install_bundle accepted mutation without a durable journal"
fi
[ "$(cat "$install_case/target/bin/threadpoint")" = "old" ] || fail "journal refusal changed the existing binary"
[ ! -e "$install_case/target/README.md" ] || fail "journal refusal installed a bundle entry"
[ "$(cat "$install_case/target/backups/run.json")" = "keep-backup" ] || fail "reinstall removed residual backup"
[ "$(cat "$install_case/target/updates/cache/latest.json")" = "keep-cache" ] || fail "reinstall removed residual update cache"

ancestor_install_case=$tmpdir/install-ancestor-symlink
mkdir -p "$ancestor_install_case/target" "$ancestor_install_case/outside"
printf '%s\n' outside >"$ancestor_install_case/outside/install.sh"
ln -s "$ancestor_install_case/outside" "$ancestor_install_case/target/scripts"
if with_installer_functions "$ancestor_install_case" install_bundle "$install_case/source" "$ancestor_install_case/target"; then
  fail "install_bundle followed a replaced scripts ancestor"
fi
[ "$(cat "$ancestor_install_case/outside/install.sh")" = outside ] || fail "install_bundle changed a file outside the physical bundle"
[ ! -e "$ancestor_install_case/outside/uninstall.sh" ] || fail "install_bundle created a file outside the physical bundle"

failure_case=$tmpdir/install-failure
mkdir -p "$failure_case/source/bin" "$failure_case/source/scripts" "$failure_case/target/bin" "$failure_case/fakebin"
printf '%s\n' old >"$failure_case/target/bin/threadpoint"
printf '%s\n' new >"$failure_case/source/bin/threadpoint"
printf '%s\n' readme >"$failure_case/source/README.md"
printf '%s\n' license >"$failure_case/source/LICENSE"
printf '%s\n' notice >"$failure_case/source/NOTICE"
printf '%s\n' install >"$failure_case/source/scripts/install.sh"
printf '%s\n' uninstall >"$failure_case/source/scripts/uninstall.sh"
cat >"$failure_case/fakebin/chmod" <<'SH'
#!/bin/sh
exit 1
SH
chmod +x "$failure_case/fakebin/chmod"
if (
  PATH="$failure_case/fakebin:$PATH"
  THREADPOINT_INSTALL_SH_TEST_MODE=1 . "$INSTALL_SH"
  tmpdir=$failure_case
  install_bundle "$failure_case/source" "$failure_case/target"
); then
  fail "install_bundle accepted mutation without a durable journal"
fi
[ "$(cat "$failure_case/target/bin/threadpoint")" = "old" ] || fail "failed install replaced existing binary"
[ ! -e "$failure_case/target/bin/threadpoint.tmp.$$" ] || fail "failed install left temp target"

explicit_home_case=$tmpdir/explicit-home
mkdir -p "$explicit_home_case/home" "$explicit_home_case/bin"
if ! (
  unset HOME
  THREADPOINT_HOME="$explicit_home_case/home"
  THREADPOINT_INSTALL_DIR="$explicit_home_case/bin"
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_HOME THREADPOINT_INSTALL_DIR THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  [ "$threadpoint_home" = "$THREADPOINT_HOME" ] || exit 1
  [ "$install_dir" = "$THREADPOINT_INSTALL_DIR" ] || exit 1
  validate_product_home "$threadpoint_home" "threadpoint home"
); then
  fail "installer should accept explicit THREADPOINT_HOME and THREADPOINT_INSTALL_DIR without HOME"
fi

expect_home_fail() {
  name=$1
  path=$2
  case_tmp=$tmpdir/home-case-$name
  mkdir -p "$case_tmp"
  if with_installer_functions "$case_tmp" validate_product_home "$path" "threadpoint home"; then
    fail "$name threadpoint home should have failed validation"
  fi
}

expect_home_fail relative relative/threadpoint
expect_home_fail root /
home_target=$tmpdir/home-target
home_link=$tmpdir/home-link
mkdir -p "$home_target"
ln -s "$home_target" "$home_link" || fail "could not create symlink home fixture"
expect_home_fail symlink "$home_link"
home_file=$tmpdir/product-home-file
printf '%s\n' "not a directory" >"$home_file"
expect_home_fail file "$home_file"

policy_case=$tmpdir/release-policy
mkdir -p "$policy_case"
policy_file=$policy_case/policy.json
printf '%s\n' \
  '{' \
  '  "schema_version": "threadpoint.release_policy.v1",' \
  '  "product": "threadpoint",' \
  '  "minimum_install_version": "v0.1.0",' \
  '  "replacement_version": "v0.1.0",' \
  '  "blocked_versions": []' \
  '}' >"$policy_file"
if ! dash -c '
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$1"
  tmpdir=$2
  policy_file=$3
  download() { cp "$policy_file" "$2"; }
  enforce_release_policy v0.1.0
' dash "$INSTALL_SH" "$policy_case" "$policy_file"; then
  fail "dash should accept a valid release policy semver"
fi

lifecycle_case=$tmpdir/lifecycle
lifecycle_home=$lifecycle_case/home
lifecycle_bin=$lifecycle_case/bin
lifecycle_command=$lifecycle_bin/threadpoint
mkdir -p "$lifecycle_home/bin" "$lifecycle_home/scripts" "$lifecycle_bin"
for entry in \
  bin/threadpoint \
  README.md \
  LICENSE \
  NOTICE \
  scripts/install.sh \
  scripts/uninstall.sh; do
  mkdir -p "$(dirname "$lifecycle_home/$entry")"
  printf 'old %s\n' "$entry" >"$lifecycle_home/$entry"
done
chmod 0755 "$lifecycle_home/bin/threadpoint" "$lifecycle_home/scripts/install.sh" "$lifecycle_home/scripts/uninstall.sh"
ln -s "$lifecycle_home/bin/threadpoint" "$lifecycle_command"

if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  binary_path=$lifecycle_home/bin/threadpoint
  metadata_path=$(install_lifecycle_path "$lifecycle_home" "$binary_path" '.json')
  write_install_metadata "$lifecycle_bin" "$binary_path" "$lifecycle_command" archive.tar.gz "$(sha256_file "$binary_path")" https://example.test/v1 "$(sha256_file "$binary_path")" v1 "$lifecycle_home"
  acquire_install_lifecycle_lock "$lifecycle_home" "$lifecycle_command"
  prepare_install_transaction "$lifecycle_home" "$lifecycle_command" "$lifecycle_home" "$metadata_path" update
  partial_binary=$lifecycle_home/bin/.partial-threadpoint
  printf '%s\n' 'partial new binary' >"$partial_binary"
  publish_install_target_generation 'bundle:bin/threadpoint' "$partial_binary" "$binary_path" normal regular
  remove_install_target_generation 'bundle:README.md' "$lifecycle_home/README.md" normal
  remove_install_target_generation metadata "$metadata_path" normal
  remove_install_target_generation link "$lifecycle_command" normal
  release_install_lifecycle_lock
  acquire_install_lifecycle_lock "$lifecycle_home" "$lifecycle_command"
  recover_install_transaction "$lifecycle_home" "$lifecycle_command"
  [ "$(cat "$binary_path")" = 'old bin/threadpoint' ]
  [ "$(cat "$lifecycle_home/README.md")" = 'old README.md' ]
  [ -f "$metadata_path" ]
  [ "$(readlink "$lifecycle_command")" = "$binary_path" ]

  # A successful helper commit validates the exact normal-phase target set,
  # removes and syncs the journal, then permits exact backup cleanup.
  prepare_install_transaction "$lifecycle_home" "$lifecycle_command" "$lifecycle_home" "$metadata_path" update
  retained_journal=$install_transaction_journal
  retained_backup=$install_transaction_backup
  committed_binary=$lifecycle_home/bin/.committed-threadpoint
  printf '%s\n' 'committed new binary' >"$committed_binary"
  publish_install_target_generation 'bundle:bin/threadpoint' "$committed_binary" "$binary_path" normal regular
  complete_install_transaction
  [ ! -e "$retained_journal" ]
  [ ! -e "$retained_backup" ]
  release_install_lifecycle_lock
); then
  fail "shell lifecycle journal should recover a mixed interrupted install"
fi

cleanup_function=$(sed -n '/^remove_install_recovery_bundle_exact() {/,/^recover_install_transaction() {/p' "$INSTALL_SH")
printf '%s\n' "$cleanup_function" | grep -Fq 'install_recovery_cleanup_as' ||
  fail 'recovery-bundle cleanup does not use the single rooted helper'
if printf '%s\n' "$cleanup_function" | grep -Eq 'remove_install_recovery_(file|directory)_exact'; then
  fail 'recovery-bundle cleanup still resolves children through separate ambient helpers'
fi

cleanup_root_race_case=$tmpdir/recovery-cleanup-root-race
cleanup_root_race_transactions=$cleanup_root_race_case/installs/transactions
cleanup_root_race_backup=$cleanup_root_race_transactions/.bundle-reviewed
mkdir -p "$cleanup_root_race_backup"
printf '%s\n' 'reviewed backup child' >"$cleanup_root_race_backup/README.md"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  install_lock_token=root-race
  cleanup_root_race_injected=0
  cleanup_root_race_path=$cleanup_root_race_transactions/.bundle-cleanup-root-race-$$
  # shellcheck disable=SC2329 # invoked through the installer cleanup helper.
  inject_cleanup_root_replacement() {
    cleanup_root_replaced_path=$1
    command mv "$cleanup_root_replaced_path" "$cleanup_root_replaced_path.reviewed" || return 1
    command mkdir "$cleanup_root_replaced_path" || return 1
    printf '%s\n' 'foreign replacement child' >"$cleanup_root_replaced_path/README.md" || return 1
    cleanup_root_race_injected=1
  }
  # The legacy shell cleanup inspected the retained cwd and then resolved
  # children through its replaced ambient pathname. Keep that exact race as
  # the red-phase repro while the rooted helper below covers the fixed path.
  install_entry_identity() {
    cleanup_root_identity_path=$1
    if cleanup_root_identity_value=$(stat -f '%d:%i' "$cleanup_root_identity_path" 2>/dev/null); then
      :
    else
      cleanup_root_identity_value=$(stat -c '%d:%i' -- "$cleanup_root_identity_path" 2>/dev/null) || return 1
    fi
    if [ "$cleanup_root_identity_path" = . ] && [ "$cleanup_root_race_injected" -eq 0 ]; then
      inject_cleanup_root_replacement "$PWD" || return 1
    fi
    printf '%s' "$cleanup_root_identity_value"
  }
  # shellcheck disable=SC2329 # invoked by the rooted recovery cleanup path.
  install_recovery_cleanup_as() {
    cleanup_root_helper_path=$(canonical_install_child_path "$1") || return 1
    if [ "$cleanup_root_race_injected" -eq 0 ]; then
      inject_cleanup_root_replacement "$cleanup_root_helper_path" || return 1
    fi
    "$THREADPOINT_INSTALL_LOCK_HELPER" __threadpoint-install-recovery-cleanup \
      "$cleanup_root_helper_path" "$2" 8>&-
  }
  cleanup_root_race_identity=$(install_entry_identity "$cleanup_root_race_backup")
  if remove_install_recovery_bundle_exact "$cleanup_root_race_backup" "$cleanup_root_race_identity"; then
    exit 1
  fi
  [ "$(cat "$cleanup_root_race_path/README.md")" = 'foreign replacement child' ] || exit 1
  [ "$(cat "$cleanup_root_race_path.reviewed/README.md")" = 'reviewed backup child' ] || exit 1
); then
  fail "recovery cleanup should preserve children beneath a replacement cleanup root"
fi

ancestor_recovery_case=$tmpdir/lifecycle-ancestor-recovery
ancestor_recovery_home=$ancestor_recovery_case/home
ancestor_recovery_bin=$ancestor_recovery_case/bin
ancestor_recovery_command=$ancestor_recovery_bin/threadpoint
mkdir -p "$ancestor_recovery_home/bin" "$ancestor_recovery_home/scripts" "$ancestor_recovery_bin"
for entry in \
  bin/threadpoint \
  README.md \
  LICENSE \
  NOTICE \
  scripts/install.sh \
  scripts/uninstall.sh; do
  mkdir -p "$(dirname "$ancestor_recovery_home/$entry")"
  printf 'old %s\n' "$entry" >"$ancestor_recovery_home/$entry"
done
chmod 0755 "$ancestor_recovery_home/bin/threadpoint" "$ancestor_recovery_home/scripts/install.sh" "$ancestor_recovery_home/scripts/uninstall.sh"
ln -s "$ancestor_recovery_home/bin/threadpoint" "$ancestor_recovery_command"
(
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  binary_path=$ancestor_recovery_home/bin/threadpoint
  metadata_path=$(install_lifecycle_path "$ancestor_recovery_home" "$binary_path" '.json')
  write_install_metadata "$ancestor_recovery_bin" "$binary_path" "$ancestor_recovery_command" archive.tar.gz "$(sha256_file "$binary_path")" https://example.test/v1 "$(sha256_file "$binary_path")" v1 "$ancestor_recovery_home"
  acquire_install_lifecycle_lock "$ancestor_recovery_home" "$ancestor_recovery_command"
  prepare_install_transaction "$ancestor_recovery_home" "$ancestor_recovery_command" "$ancestor_recovery_home" "$metadata_path" update
  release_install_lifecycle_lock
)
mv "$ancestor_recovery_home/scripts" "$ancestor_recovery_home/scripts-old"
mkdir -p "$ancestor_recovery_case/outside"
printf '%s\n' outside >"$ancestor_recovery_case/outside/install.sh"
ln -s "$ancestor_recovery_case/outside" "$ancestor_recovery_home/scripts"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$ancestor_recovery_home" "$ancestor_recovery_command"
  recovered=0
  recover_install_transaction "$ancestor_recovery_home" "$ancestor_recovery_command" || recovered=$?
  release_install_lifecycle_lock
  [ "$recovered" -eq 0 ]
); then
  fail "shell recovery followed a replaced scripts ancestor"
fi
[ "$(cat "$ancestor_recovery_case/outside/install.sh")" = outside ] || fail "shell recovery changed a file outside the physical bundle"
[ ! -e "$ancestor_recovery_case/outside/uninstall.sh" ] || fail "shell recovery created a file outside the physical bundle"

initial_recovery_case=$tmpdir/initial-recovery
mkdir -p "$initial_recovery_case/home" "$initial_recovery_case/bin"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  product_home=$initial_recovery_case/home
  command_path=$initial_recovery_case/bin/threadpoint
  binary_path=$product_home/bin/threadpoint
  metadata_path=$(install_lifecycle_path "$product_home" "$binary_path" '.json')
  acquire_install_lifecycle_lock "$product_home" "$command_path"
  prepare_install_transaction "$product_home" "$command_path" "$product_home" "$metadata_path" install
  mkdir -p "$product_home/bin" "${metadata_path%/*}"
  printf '%s\n' partial >"$product_home/bin/.partial-threadpoint"
  publish_install_target_generation 'bundle:bin/threadpoint' "$product_home/bin/.partial-threadpoint" "$binary_path" normal regular
  printf '%s\n' partial >"$product_home/.partial-README"
  publish_install_target_generation 'bundle:README.md' "$product_home/.partial-README" "$product_home/README.md" normal regular
  printf '%s\n' partial >"${metadata_path%/*}/.partial-metadata"
  publish_install_target_generation metadata "${metadata_path%/*}/.partial-metadata" "$metadata_path" normal regular
  link_temp_dir=$(mktemp -d "$initial_recovery_case/bin/.link-recover-XXXXXX")
  ln -s "$binary_path" "$link_temp_dir/candidate"
  publish_install_target_generation link "$link_temp_dir/candidate" "$command_path" normal symlink
  rmdir "$link_temp_dir"
  release_install_lifecycle_lock
  acquire_install_lifecycle_lock "$product_home" "$command_path"
  recover_install_transaction "$product_home" "$command_path"
  [ ! -e "$binary_path" ]
  [ ! -e "$product_home/README.md" ]
  [ ! -e "$metadata_path" ]
  [ ! -e "$command_path" ] && [ ! -L "$command_path" ]
  release_install_lifecycle_lock
); then
  fail "shell lifecycle journal should remove a partial initial install"
fi

crafted_prior_case=$tmpdir/crafted-prior-link
make_managed_threadpoint_fixture "$crafted_prior_case"
crafted_prior_target=$crafted_prior_case/'foreign"target\segment
second-line'
rm -f "$crafted_prior_case/bin/threadpoint"
ln -s "$crafted_prior_target" "$crafted_prior_case/bin/threadpoint"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  crafted_home=$crafted_prior_case/home
  crafted_command=$crafted_prior_case/bin/threadpoint
  crafted_binary=$crafted_home/bin/threadpoint
  crafted_metadata=$(install_lifecycle_path "$crafted_home" "$crafted_binary" '.json')
  crafted_journal=$(install_lifecycle_path "$crafted_home" "$crafted_command" '.transaction.json')
  acquire_install_lifecycle_lock "$crafted_home" "$crafted_command"
  if (prepare_install_transaction "$crafted_home" "$crafted_command" "$crafted_home" "$crafted_metadata" update); then
    exit 1
  fi
  [ -L "$crafted_command" ] && [ "$(readlink "$crafted_command")" = "$crafted_prior_target" ]
  [ ! -e "$crafted_journal" ] && [ ! -L "$crafted_journal" ]
  release_install_lifecycle_lock
); then
  fail "crafted prior symlink target was accepted or changed during journal preparation"
fi

strict_journal_case=$tmpdir/strict-journal-recovery
make_managed_threadpoint_fixture "$strict_journal_case"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  strict_home=$strict_journal_case/home
  strict_command=$strict_journal_case/bin/threadpoint
  strict_binary=$strict_home/bin/threadpoint
  strict_metadata=$(install_lifecycle_path "$strict_home" "$strict_binary" '.json')
  acquire_install_lifecycle_lock "$strict_home" "$strict_command"
  prepare_install_transaction "$strict_home" "$strict_command" "$strict_home" "$strict_metadata" update
  strict_journal=$install_transaction_journal
  strict_backup=$install_transaction_backup
  strict_valid=$strict_journal_case/valid-journal.json
  cp "$strict_journal" "$strict_valid"
  for malformed_kind in duplicate unknown missing trailing; do
    case "$malformed_kind" in
      duplicate)
        {
          sed -n '1p' "$strict_valid"
          printf '  "schema_version": "threadpoint.install_transaction.v1",\n'
          sed -n '2,$p' "$strict_valid"
        } >"$strict_journal"
        ;;
      unknown)
        {
          sed -n '1p' "$strict_valid"
          printf '  "unknown": true,\n'
          sed -n '2,$p' "$strict_valid"
        } >"$strict_journal"
        ;;
      missing) sed '/^[[:space:]]*"operation":/d' "$strict_valid" >"$strict_journal" ;;
      trailing)
        cp "$strict_valid" "$strict_journal"
        printf '%s\n' trailing >>"$strict_journal"
        ;;
    esac
    if recover_install_transaction "$strict_home" "$strict_command"; then
      exit 1
    fi
    [ "$(cat "$strict_home/README.md")" = 'fixture README' ]
    [ -d "$strict_backup" ]
  done

  rm -f "$strict_journal"
  mkfifo "$strict_journal"
  rm -f "$strict_journal_case/recovery-accepted" "$strict_journal_case/recovery-refused"
  (
    if recover_install_transaction "$strict_home" "$strict_command"; then
      : >"$strict_journal_case/recovery-accepted"
    else
      : >"$strict_journal_case/recovery-refused"
    fi
  ) &
  strict_recovery_pid=$!
  strict_waits=0
  while [ ! -e "$strict_journal_case/recovery-accepted" ] && [ ! -e "$strict_journal_case/recovery-refused" ]; do
    strict_waits=$((strict_waits + 1))
    if [ "$strict_waits" -ge 300 ]; then
      kill "$strict_recovery_pid" 2>/dev/null || true
      wait "$strict_recovery_pid" 2>/dev/null || true
      exit 1
    fi
    sleep 0.01
  done
  wait "$strict_recovery_pid" 2>/dev/null || true
  [ -e "$strict_journal_case/recovery-refused" ] && [ ! -e "$strict_journal_case/recovery-accepted" ]

  rm -f "$strict_journal"
  dd if=/dev/zero of="$strict_journal" bs=262145 count=1 2>/dev/null
  if recover_install_transaction "$strict_home" "$strict_command"; then
    exit 1
  fi
  [ "$(cat "$strict_home/README.md")" = 'fixture README' ]
  [ -d "$strict_backup" ]
  release_install_lifecycle_lock
); then
  fail "shell recovery accepted malformed, FIFO, or oversized transaction state"
fi

state_symlink_case=$tmpdir/lifecycle-state-symlink
mkdir -p "$state_symlink_case/home" "$state_symlink_case/bin" "$state_symlink_case/outside"
printf '%s\n' untouched >"$state_symlink_case/outside/marker"
ln -s "$state_symlink_case/outside" "$state_symlink_case/home/installs"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$state_symlink_case/home" "$state_symlink_case/bin/threadpoint"
); then
  fail "shell lifecycle lock followed an installs-directory symlink"
fi
[ "$(cat "$state_symlink_case/outside/marker")" = untouched ] || fail "state symlink target was modified"
[ ! -e "$state_symlink_case/outside/transactions" ] || fail "transaction state escaped through an installs symlink"

state_replacement_case=$tmpdir/lifecycle-state-replacement
mkdir -p "$state_replacement_case/home" "$state_replacement_case/bin"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$state_replacement_case/home" "$state_replacement_case/bin/threadpoint"
  mv "$state_replacement_case/home/installs" "$state_replacement_case/home/installs-old"
  mkdir "$state_replacement_case/home/installs"
  ownership_changed=0
  validate_install_lock_ownership || ownership_changed=$?
  release_install_lifecycle_lock
  [ "$ownership_changed" -eq 0 ]
); then
  fail "shell lifecycle lock accepted a replaced installs directory"
fi

anchor_symlink_case=$tmpdir/lifecycle-anchor-symlink
mkdir -p "$anchor_symlink_case/home" "$anchor_symlink_case/outside-bin"
ln -s "$anchor_symlink_case/outside-bin" "$anchor_symlink_case/bin"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$anchor_symlink_case/home" "$anchor_symlink_case/bin/threadpoint"
); then
  fail "shell lifecycle lock followed a command-directory symlink"
fi
[ -z "$(find "$anchor_symlink_case/outside-bin" -mindepth 1 -print -quit)" ] || fail "anchor lock escaped through a command-directory symlink"

lock_case=$tmpdir/lifecycle-lock
mkdir -p "$lock_case/home" "$lock_case/bin"
(
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$lock_case/home" "$lock_case/bin/threadpoint"
  : >"$lock_case/ready"
  sleep 3
  release_install_lifecycle_lock
) &
lock_owner_pid=$!
ready_attempts=0
while [ ! -f "$lock_case/ready" ]; do
  ready_attempts=$((ready_attempts + 1))
  if [ "$ready_attempts" -ge 200 ]; then
    kill "$lock_owner_pid" 2>/dev/null || true
    wait "$lock_owner_pid" 2>/dev/null || true
    fail "shell lifecycle lock owner did not become ready"
  fi
  sleep 0.05
done
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  THREADPOINT_INSTALL_LOCK_ATTEMPTS=1
  export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_ATTEMPTS
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$lock_case/home" "$lock_case/bin/threadpoint"
); then
  kill "$lock_owner_pid" 2>/dev/null || true
  wait "$lock_owner_pid" 2>/dev/null || true
  fail "shell lifecycle lock should serialize concurrent installers"
fi
wait "$lock_owner_pid"

malformed_lock_case=$tmpdir/lifecycle-malformed-lock
mkdir -p "$malformed_lock_case/home" "$malformed_lock_case/bin"
malformed_lock_path=$(
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  install_lifecycle_lock_path "$malformed_lock_case/home" "$malformed_lock_case/bin/threadpoint"
)
mkdir -p "${malformed_lock_path%/*}"
printf '{"pid":%s' "$$" >"$malformed_lock_path"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  THREADPOINT_INSTALL_LOCK_ATTEMPTS=1
  export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_ATTEMPTS
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$malformed_lock_case/home" "$malformed_lock_case/bin/threadpoint"
); then
  fail "shell lifecycle lock reclaimed a truncated live-owner heartbeat"
fi
[ -f "$malformed_lock_path" ] || fail "truncated live-owner lock was removed"
rm -f "$malformed_lock_path"

backup_prior_case=$tmpdir/backup-prior-generation
make_managed_threadpoint_fixture "$backup_prior_case"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  backup_home=$backup_prior_case/home
  backup_command=$backup_prior_case/bin/threadpoint
  backup_binary=$backup_home/bin/threadpoint
  backup_metadata=$(install_lifecycle_path "$backup_home" "$backup_binary" '.json')
  acquire_install_lifecycle_lock "$backup_home" "$backup_command"
  backup_sync_calls=0
  # Replace canonical A with B after A was copied but before Prior is rendered.
  # shellcheck disable=SC2329
  sync_install_lifecycle_state() {
    backup_sync_calls=$((backup_sync_calls + 1))
    if [ "$backup_sync_calls" -eq 1 ]; then
      printf '%s\n' 'canonical generation B' >"$backup_home/README.md"
    fi
    return 0
  }
  if prepare_install_transaction "$backup_home" "$backup_command" "$backup_home" "$backup_metadata" update; then
    exit 1
  fi
  [ "$(cat "$backup_home/README.md")" = 'canonical generation B' ]
  [ "$(cat "$install_transaction_backup/README.md")" = 'fixture README' ]
  [ ! -e "$install_transaction_journal" ]
  set -- "$install_transaction_journal".tmp.*
  [ "$#" -eq 1 ] && [ -f "$1" ]
  release_install_lifecycle_lock
); then
  fail "journal publication did not reject backup-A/prior-B"
fi

completion_replacement_case=$tmpdir/completion-replacement
make_managed_threadpoint_fixture "$completion_replacement_case"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  completion_home=$completion_replacement_case/home
  completion_command=$completion_replacement_case/bin/threadpoint
  completion_binary=$completion_home/bin/threadpoint
  completion_metadata=$(install_lifecycle_path "$completion_home" "$completion_binary" '.json')
  acquire_install_lifecycle_lock "$completion_home" "$completion_command"
  prepare_install_transaction "$completion_home" "$completion_command" "$completion_home" "$completion_metadata" update
  completion_journal=$install_transaction_journal
  completion_backup=$install_transaction_backup
  completion_temp=$completion_home/.README.produced
  printf '%s\n' 'produced README' >"$completion_temp"
  publish_install_target_generation 'bundle:README.md' "$completion_temp" "$completion_home/README.md" normal regular
  rm -f "$completion_home/README.md"
  printf '%s\n' 'foreign final replacement' >"$completion_home/README.md"
  if complete_install_transaction; then
    exit 1
  fi
  [ "$(cat "$completion_home/README.md")" = 'foreign final replacement' ]
  [ -f "$completion_journal" ]
  [ -d "$completion_backup" ]
  release_install_lifecycle_lock
); then
  fail "normal completion accepted a final target replacement"
fi

release_failure_case=$tmpdir/guard-release-failure
mkdir -p "$release_failure_case/home" "$release_failure_case/bin"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$release_failure_case/home" "$release_failure_case/bin/threadpoint"
  printf '%s\n' 'injected guard release failure' >"$install_lock_guard_control/error"
  if release_install_lifecycle_lock; then
    exit 1
  fi
); then
  fail "shell lifecycle release swallowed a guard-helper failure"
fi

canonical_command_case=$tmpdir/canonical-command
mkdir -p "$canonical_command_case/home" "$canonical_command_case/bin/child"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$canonical_command_case/home" "$canonical_command_case/bin/child/../threadpoint"
); then
  fail "shell lifecycle accepted a noncanonical command path"
fi

cross_home_case=$tmpdir/cross-home-guard
cross_home_command=$cross_home_case/bin/threadpoint
mkdir -p "$cross_home_case/home-a" "$cross_home_case/home-b" "$cross_home_case/bin"
(
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$cross_home_case/home-a" "$cross_home_command"
  : >"$cross_home_case/acquired"
  wait_for_file "$cross_home_case/release"
  release_install_lifecycle_lock
) &
cross_home_owner=$!
wait_for_file "$cross_home_case/acquired"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  THREADPOINT_INSTALL_LOCK_ATTEMPTS=1
  export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_ATTEMPTS
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$cross_home_case/home-b" "$cross_home_command"
); then
  fail "different product homes entered one command lifecycle concurrently"
fi
: >"$cross_home_case/release"
wait "$cross_home_owner" || fail "cross-home lifecycle owner failed to release"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  THREADPOINT_INSTALL_LOCK_ATTEMPTS=3
  export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_ATTEMPTS
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$cross_home_case/home-b" "$cross_home_command"
  release_install_lifecycle_lock
); then
  fail "cross-home lifecycle guard was not reusable after release"
fi

descendant_guard_case=$tmpdir/descendant-guard
descendant_guard_command=$descendant_guard_case/bin/threadpoint
mkdir -p "$descendant_guard_case/home" "$descendant_guard_case/bin"
THREADPOINT_INSTALL_SH_TEST_MODE=1 sh -c '
  . "$1"
  acquire_install_lifecycle_lock "$2" "$3"
  (
    : >"$4"
    child_waits=0
    while [ ! -e "$5" ] && [ "$child_waits" -lt 1000 ]; do
      child_waits=$((child_waits + 1))
      sleep 0.01
    done
    : >"$6"
  ) </dev/null >/dev/null 2>&1 &
  : >"$7"
' sh "$INSTALL_SH" "$descendant_guard_case/home" "$descendant_guard_command" \
  "$descendant_guard_case/child-ready" "$descendant_guard_case/child-exit" \
  "$descendant_guard_case/child-done" "$descendant_guard_case/owner-ready" &
descendant_owner=$!
wait_for_file "$descendant_guard_case/owner-ready"
wait "$descendant_owner" || fail "crashing lifecycle owner did not exit cleanly"
wait_for_file "$descendant_guard_case/child-ready"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  THREADPOINT_INSTALL_LOCK_ATTEMPTS=1
  export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_ATTEMPTS
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$descendant_guard_case/home" "$descendant_guard_command"
); then
  fail "kernel guard released while a crashed owner's descendant retained the FIFO writer"
fi
: >"$descendant_guard_case/child-exit"
wait_for_file "$descendant_guard_case/child-done"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  THREADPOINT_INSTALL_LOCK_ATTEMPTS=4
  export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_ATTEMPTS
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$descendant_guard_case/home" "$descendant_guard_command"
  release_install_lifecycle_lock
); then
  fail "kernel guard was not recoverable after the final descendant writer exited"
fi

lock_tombstone_case=$tmpdir/lock-tombstones
mkdir -p "$lock_tombstone_case/home" "$lock_tombstone_case/bin"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  tombstone_iteration=0
  while [ "$tombstone_iteration" -lt 3 ]; do
    acquire_install_lifecycle_lock "$lock_tombstone_case/home" "$lock_tombstone_case/bin/threadpoint"
    release_install_lifecycle_lock
    tombstone_iteration=$((tombstone_iteration + 1))
  done
); then
  fail "repeated lifecycle acquire/release failed"
fi
if find "$lock_tombstone_case/home/installs" "$lock_tombstone_case/bin" -type f \
  \( -name '*.release.*' -o -name '*.reclaim.*' \) -print | grep -q .; then
  fail "repeated lifecycle release accumulated detached lock tombstones"
fi

if grep -F '.threadpoint-install.' "$INSTALL_SH" >/dev/null; then
  fail "installer still contains the redundant ad hoc bundle snapshot namespace"
fi

bounded_download_case=$tmpdir/bounded-download
bounded_download_fakebin=$bounded_download_case/fakebin
mkdir -p "$bounded_download_fakebin"
cat >"$bounded_download_fakebin/curl" <<'SH'
#!/bin/sh
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    output=$2
    shift 2
  else
    shift
  fi
done
/bin/dd if=/dev/zero of="$output" bs=1 count="${THREADPOINT_TEST_DOWNLOAD_BYTES:?}" 2>/dev/null
SH
chmod +x "$bounded_download_fakebin/curl"
if (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  THREADPOINT_TEST_DOWNLOAD_BYTES=2048
  export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_TEST_DOWNLOAD_BYTES
  . "$INSTALL_SH"
  PATH="$bounded_download_fakebin:/usr/bin:/bin"
  download https://example.test/oversized "$bounded_download_case/body" 1024
); then
  fail "shell download accepted an oversized response"
fi
[ ! -e "$bounded_download_case/body" ] || fail "oversized shell download was published"

lock_generation_case=$tmpdir/lock-generation-race
mkdir -p "$lock_generation_case/home/installs"
if ! (
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  install_lock_token=$(printf '%064d' 0 | tr 0 c)
  install_lock_created=2026-08-06T00:00:00Z
  raced_lock=$lock_generation_case/home/installs/raced.lock
  printf '{"pid":1073741824,"created":"2026-08-06T00:00:00Z","heartbeat":"2026-08-06T00:00:00Z","command":"%s","token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\n' \
    "$lock_generation_case/bin/threadpoint" >"$raced_lock"
  THREADPOINT_TEST_LOCK_PATH=$raced_lock
  THREADPOINT_TEST_LOCK_MARKER=$lock_generation_case/replaced
  export THREADPOINT_TEST_LOCK_PATH THREADPOINT_TEST_LOCK_MARKER
  # shellcheck disable=SC2329 # invoked through stale-lock reclamation.
  install_atomic_move_noreplace_as() {
    race_source=$(canonical_install_child_path "$1") || return 1
    race_target=$(canonical_install_child_path "$2") || return 1
    if [ "$race_source" = "$THREADPOINT_TEST_LOCK_PATH" ] && [ ! -e "$THREADPOINT_TEST_LOCK_MARKER" ]; then
      : >"$THREADPOINT_TEST_LOCK_MARKER"
      /bin/rm -f "$race_source"
      printf '{"pid":%s,"created":"2026-08-06T00:00:00Z","heartbeat":"2026-08-06T00:00:00Z","command":"%s","token":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}\n' \
        "$$" "$lock_generation_case/bin/threadpoint" >"$race_source"
    fi
    "$THREADPOINT_INSTALL_LOCK_HELPER" __threadpoint-install-no-replace-as "$race_source" "$race_target" "$3"
  }
  if acquire_install_lock_file "$raced_lock" 1; then
    exit 1
  fi
  grep -Fq '"token":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' "$raced_lock"
); then
  fail "stale lock reclaim did not restore a raced live generation"
fi

three_contender_case=$tmpdir/three-contender-lock
three_contender_home=$three_contender_case/home
three_contender_bin=$three_contender_case/bin
three_contender_command=$three_contender_bin/threadpoint
mkdir -p "$three_contender_home/installs" "$three_contender_bin"
three_contender_lock=$(
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  export THREADPOINT_INSTALL_SH_TEST_MODE
  . "$INSTALL_SH"
  install_lifecycle_lock_path "$three_contender_home" "$three_contender_command"
)
printf '{"pid":1073741824,"created":"2026-08-06T00:00:00Z","heartbeat":"2026-08-06T00:00:00Z","command":"%s","token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\n' \
  "$three_contender_command" >"$three_contender_lock"
(
  THREADPOINT_INSTALL_SH_TEST_MODE=1
  THREADPOINT_INSTALL_LOCK_TEST_STALE_READY=$three_contender_case/stale-ready
  THREADPOINT_INSTALL_LOCK_TEST_STALE_CONTINUE=$three_contender_case/stale-continue
  THREADPOINT_INSTALL_LOCK_TEST_RELEASE_READY=$three_contender_case/release-ready
  THREADPOINT_INSTALL_LOCK_TEST_RELEASE_CONTINUE=$three_contender_case/release-continue
  export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_TEST_STALE_READY \
    THREADPOINT_INSTALL_LOCK_TEST_STALE_CONTINUE THREADPOINT_INSTALL_LOCK_TEST_RELEASE_READY \
    THREADPOINT_INSTALL_LOCK_TEST_RELEASE_CONTINUE
  . "$INSTALL_SH"
  acquire_install_lifecycle_lock "$three_contender_home" "$three_contender_command"
  : >"$three_contender_case/acquired"
  wait_for_file "$three_contender_case/start-release"
  release_install_lifecycle_lock
) &
three_contender_owner=$!
wait_for_file "$three_contender_case/stale-ready"
expect_three_contender_refused() {
  if (
    THREADPOINT_INSTALL_SH_TEST_MODE=1
    THREADPOINT_INSTALL_LOCK_ATTEMPTS=1
    export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_ATTEMPTS
    . "$INSTALL_SH"
    acquire_install_lifecycle_lock "$three_contender_home" "$three_contender_command"
  ); then
    kill "$three_contender_owner" 2>/dev/null || true
    fail "a contender entered during stale-lock review"
  fi
}
expect_three_contender_refused
expect_three_contender_refused
: >"$three_contender_case/stale-continue"
wait_for_file "$three_contender_case/acquired"
: >"$three_contender_case/start-release"
wait_for_file "$three_contender_case/release-ready"
expect_three_contender_refused
expect_three_contender_refused
: >"$three_contender_case/release-continue"
wait "$three_contender_owner"

printf '%s\n' "installer archive safety tests passed"
