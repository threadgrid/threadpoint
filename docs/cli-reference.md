# CLI Reference

All project commands operate on one selected project root. Project content is
read and written only below that root; private review state and backups live
under the separate [state home](environment-and-state-policy.md#threadpoint-state-home).
`--root` is optional: run the commands below inside your project to discover its
root automatically.
Use `--format json` where a command supports structured output.

To override discovery, put the flag after the command:

```bash
threadpoint status --root /path/to/project
```

`--root .` selects exactly the current directory; omitting the flag allows
ancestor discovery. See [root selection](environment-and-state-policy.md#selected-root)
for markers, ambiguity handling, and protected roots.

| Command                                                                    | Purpose                                                                   |
| -------------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| `init --yes`                                                               | Create missing shared and local layout files.                             |
| `stage --plan`                                                             | Discover supported native artifacts and preview review copies.            |
| `stage --apply --yes`                                                      | Create private review copies.                                             |
| `stage list` \| `diff` \| `edit` \| `difftool` \| `mergetool` \| `discard` | Inspect, compare, edit, source-refresh merge, or discard one review copy. |
| `commit ID --plan`                                                         | Preview the canonical target for one review copy.                         |
| `commit ID --apply --yes`                                                  | Write one reviewed target and record its source snapshot.                 |
| `prune --plan`                                                             | Find native sources unchanged since their reviewed commit.                |
| `restore list`                                                             | List backups for the root.                                                |
| `status`                                                                   | Report layout, review stages, backup, prune, and Git state.               |
| `lock clear`                                                               | Clear a stale project mutation lock.                                      |
| `doctor`                                                                   | Check the tool itself.                                                    |
| `version`                                                                  | Show build and install metadata.                                          |
| `update`                                                                   | Update an installer-managed bundle after confirmation.                    |
| `update check`                                                             | Inspect release availability.                                             |
| `update rollback`                                                          | Return a preview installation to stable.                                  |
| `update reminder status`                                                   | Show reminder preferences.                                                |
| `update reminder enable` / `disable`                                       | Change reminder preferences after confirmation.                           |
| `update reminder dismiss --version VERSION`                                | Dismiss one release reminder after confirmation.                          |
| `uninstall --plan`                                                         | Preview removal of an installer-managed bundle.                           |

Use `threadpoint help <command path>` or append `--help` to any command for
its flags, for example `threadpoint help update reminder dismiss`. Bare `lock`
and `update reminder` show their group help in text mode; in JSON mode, they
require a subcommand and return a nonzero JSON error. See the
[naming and help contract](cli-contract.md).

`stage` accepts `--providers`, `--exclude-providers`, and repeated
`--classify PATH=project-shared|project-local`. `CLAUDE.local.md` always maps
to the local lane.

`stage diff ID` prints the unredacted local unified diff and warns on standard
error. `stage difftool ID` requires `$DIFF` and passes the frozen source
snapshot then review copy as arguments. `stage mergetool ID` requires `$MERGE`;
when the current source has changed, it passes base, local review, remote
current source, and merged output arguments, then replaces the review copy only
when the configured tool exits successfully. Both tool commands require an
interactive terminal.

Status lists canonical records grouped by project-shared and project-local scope.
Its JSON report separates `inventory`, `validation`, and `workflow`, each with
a completeness indicator. It hashes current sources and checks saved stages,
commit-backed prune readiness, backups, and scoped Git state. Pending work is
informational; warnings degrade readiness; errors block readiness and exit 3.
A missing optional scope is empty; an unreadable scope is incomplete.
Status does not modify files. The reusable `scan.Run`, `layout.Validate`, and
`doctor` Go APIs remain available.

Use `doctor` for tool health, `stage list` for saved review copies, and an
operation's `--plan` for its proposed changes.
