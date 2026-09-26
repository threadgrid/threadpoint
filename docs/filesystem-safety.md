# Filesystem Safety

Threadpoint resolves and pins one selected root for each project command. It
rejects roots that are user home or a user `.agents` directory, refuses paths
that escape the pinned root, and treats unsafe symlink or non-regular-file
conditions as refusal errors.

Stage reads bounded regular files into private review storage. Commit, prune,
and restore validate their complete plans before mutation, record backups, use
no-replace operations where the platform supports them, and retain recovery
paths when cleanup cannot be proven safe.

Lock paths are regular files. An unexpected lock directory, incomplete lock
record, or competing live lock is treated as held until it can be safely
reclaimed.

New user-visible files and directories honor the active umask; existing regular
canonical targets retain their modes when replaced. Private state and lock
containers use explicit private modes.

The filesystem implementation is built only for the supported Linux and macOS
release matrix. Unsupported operating systems have no permissive filesystem
implementation. On the supported matrix, if no-replace link publication is not
available, a guarded exclusive direct-create path preserves the no-overwrite
contract after identity validation.

## Directory skip policy

Project discovery and shared-agent scans skip known high-volume or generated
directory basenames. Matching is case-insensitive. The default set is:

```text
.angular, .astro, .aws-sam, .bundle, .bzr, .cache, .dart_tool, .docusaurus,
.eggs, .fleet, .fossil, .git, .gradle, .hg, .hypothesis, .idea,
.ipynb_checkpoints, .jj, .m2, .mypy_cache, .next, .nox, .npm, .nuxt, .output,
.parcel-cache, .pijul, .pnpm-store, .pytest_cache, .ruff_cache, .serverless,
.stack-work, .svelte-kit, .svn, .swiftpm, .terraform, .terragrunt-cache, .tox,
.turbo, .venv, .vite, .vs, .vscode, .webpack, .yarn, __pycache__,
bower_components, build, carthage, coverage, deriveddata, dist, node_modules,
out, pods, target, vendor, venv
```

Use `--skip-dirs name1,name2` with `threadpoint status` or `threadpoint stage` to add directory basenames for one command. The flag may
be repeated. Names must be plain basenames: paths, glob patterns, empty names,
`.` and `..` are rejected. Additions cannot re-enable a built-in skipped
directory or a threadpoint-managed recovery entry.
