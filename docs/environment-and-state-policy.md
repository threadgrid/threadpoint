# Environment and State Policy

## Selected root

Project commands use one selected root at a time. `--root` is optional. Run
`threadpoint status` inside your project to discover the root from the current
directory, or `threadpoint status --root /path/to/project` to select a root
explicitly. The flag belongs after the command.

Without `--root`, discovery walks upward to the nearest `.git`, `AGENTS.md`,
`.agents/`, `AGENTS.local.md`, or `.agents.local/` marker, stopping before the
user home. A Git worktree or submodule `.git` file is also a marker. Curated
provider workspace markers are fallbacks: a provider marker below a strong
ancestor marker is ambiguous and requires explicit `--root`. When no markers
are found, the current directory is the selected root.

An explicit root disables ancestor discovery and remains its logical absolute
path, including symlink paths. Its physical target is used for safety checks
and filesystem pinning. In particular, `--root .` selects exactly the current
directory; it is not equivalent to omitting the flag from a subdirectory.
An empty or whitespace-only explicit root is an error.

For both explicit roots and implicit-discovery starting directories, the
filesystem root, resolved user home, and paths under the user's `.agents`
directory are rejected. Project content reads and writes stay below the
selected root; product-owned review state and backups use the separate state
home described below.

`init` follows the same discovery rules, so an unmarked subdirectory can select
an ancestor project. Its interactive prompt names the selected root; text
success reports that root on stderr, including with `--yes`, unless `--quiet`
is set. JSON success includes `root` on stdout. To initialize the current
directory as a separate project, use `threadpoint init --root . --yes`.

## Threadpoint state home

The home flag or THREADPOINT_HOME selects private threadpoint state; otherwise
it defaults to the user home plus .threadpoint. It contains review copies,
backups, locks, update metadata, and release bundles. Changing the home never
changes the selected project root.

## Canonical paths

The shared lane is AGENTS.md and .agents. The local lane is AGENTS.local.md and
.agents.local. Review copies are private threadpoint state, not project files.

Threadpoint does not persist ambient local diagnostic logs and does not support
THREADPOINT_LOG_LEVEL. Under WSL, threadpoint still uses the Linux path model.
