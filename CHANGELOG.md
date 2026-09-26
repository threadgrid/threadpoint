# Changelog

## Unreleased

### Added

- Documents the public pre-v1 Go API with pkg.go.dev guidance, runnable
  examples, local documentation preview, and automated comment enforcement.

### Changed

- Improves installer PATH guidance for Bash, Zsh, and Fish, including custom
  command directories.

- Renames the public Go packages to consistent singular, action-oriented names:
  `provider`, `backup`, `clierror`, `diagnostic`, `discover`, `scan`,
  `safepath`, `project`, `prune`, `redact`, `restore`, `skill`,
  `stage`, and `walk`.
- Separates canonical path names and layout creation/validation into `layout`,
  keeps scan records in `knowledgebase.Catalog`, and uses concise entry points
  such as `discover.Run`, `scan.Run`, `doctor.Run`, and `stage.Create`.
- Makes module-local test, vet, lint, build, and documentation commands ignore
  any enclosing Go workspace.

### Command contract

Project-scoped command behavior is the v0.1.0 contract. Run each command from
the selected `--root` project (for example, `--root <project>`); stage creates
review copies and commit writes one reviewed item to its project target.

- Native provider artifacts follow the explicit stage and commit workflow.
- Commit records the reviewed native source snapshot; prune uses the latest
  unchanged committed snapshot.
- Review copies, backups, restore, and lock handling use current strict safety
  contracts.

### Fixed

- Rejects colon-containing command directories before installation and
  recognizes equivalent PATH directories, including symlinks.

- Keeps stage, commit, backup, prune, and restore operations bound to the
  selected project and exact generation, preventing concurrent replacement or
  cleanup.
- Makes managed install, update, rollback, and uninstall operations
  transactional and recoverable after interruption.
- Preserves private file permissions through prune and restore, and rejects
  unsafe knowledgebase inputs.
- Restores symlink targets on macOS without relying on Darwin's unsupported
  rooted symlink-handle combination while retaining rooted identity checks.
- Supports case-insensitive filesystems in backup collision coverage.
