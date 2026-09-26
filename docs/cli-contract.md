# CLI Contract

Threadpoint writes plans, reports, and JSON to stdout. Diagnostics and prompts
go to stderr. `--format json` selects machine-readable output for commands that
support it and does not persist local CLI diagnostic logs.

Command groups use singular nouns (`lock`, `update reminder`); actions use
verbs (`stage`, `commit`, `restore`). Collection inspection uses a `list`
subcommand: `stage list` and `restore list`. Reminder operations are
`update reminder status`, `enable`, `disable`, and `dismiss --version VERSION`.
There are no aliases for retired command spellings.

`help <command path>`, `<command path> --help`, and `<command path> -h`
show the same dedicated help at every depth. Bare `lock` and `update reminder`
show group help and exit successfully in text mode. In JSON mode, bare groups
require a subcommand and return a JSON error on stderr with exit 2. Explicit
help remains text on stderr even with `--format json`, never prompts, contacts
the network, or writes local state. Unknown paths are usage errors. Direct
workflow commands retain their normal default behavior.

Help flag arity is generated from actual flag declarations. After changing
flags, run `go generate ./cmd/threadpoint`; CLI tests check metadata freshness.

`restore list` accepts `--root`, `--backup-namespace`, and `--format`, plus
global options before the command. Restoration selection and mutation flags
belong to `restore` and are rejected by `restore list`.
Its JSON output labels absolute project and state-home paths as `$ROOT` and
`$THREADPOINT_HOME`; backup manifests on disk retain their original paths.

`init` names the selected root in its confirmation prompt. On text success it
reports that root on stderr (unless `--quiet`), keeping stdout empty. With
`--format json`, its report includes `root` on stdout. `--yes` skips the prompt
but retains the success report.

Mutating commands require explicit confirmation: `--yes` for scripted runs or
an interactive prompt. `stage edit` requires an interactive terminal.

Machine-readable outcomes use the exact `code` and `message` contract in
[diagnostic-code-contract.md](diagnostic-code-contract.md). Public output does
not emit legacy `reason`, `errorCode`, or `category` fields.

| Exit code | Meaning                                                                                                                    |
| --------- | -------------------------------------------------------------------------------------------------------------------------- |
| 0         | Command completed successfully.                                                                                            |
| 1         | Unexpected runtime failure.                                                                                                |
| 2         | Invalid command or flags.                                                                                                  |
| 3         | A completed diagnostic report has blocking findings.                                                                       |
| 4         | A safe operation was refused, including stale review copies, invalid scope, changed prune source, or missing confirmation. |

`stage`, `commit`, `prune`, and `restore` are plan-first. `commit` changes one
canonical target only. `prune` requires the current native artifact to equal
the source snapshot recorded by its latest commit.
