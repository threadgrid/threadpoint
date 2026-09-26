# Example Workflows And Fixtures

This page describes deterministic project-root fixture trees for the core
the initial threadpoint release workflows. All fixtures are synthetic and intentionally small.

## Fixture Set

- `docs/example-fixtures/bare-init/`: clean project for `init`,
  `status`, and `doctor`.
- `docs/example-fixtures/v0-baseline/`: mixed native provider artifacts
  for staging, commit, pruning, restore, and status examples.

Use a temporary copy for apply examples so the checked-in fixtures remain clean.
Each copied directory is passed as an explicit project root; the examples do not
read provider state from surrounding directories or the host home directory:

```bash
example_root="$(mktemp -d)"
cp -R docs/example-fixtures/bare-init "$example_root/bare-init"
cp -R docs/example-fixtures/v0-baseline "$example_root/v0-baseline"
export THREADPOINT_HOME="$example_root/threadpoint-home"
```

## Bare Init

Start with a project that has no shared agent layout:

```bash
threadpoint init --root "$example_root/bare-init"
threadpoint status --root "$example_root/bare-init"
threadpoint doctor --root "$example_root/bare-init"
```

Expected result: `init` creates the shared `AGENTS.md` and `.agents/` layout,
the ignored `state/skills/` runtime skeleton, and the `/state/` ignore rule;
`status` reports the initialized project inventory and valid layout.

## Stage Native Provider Artifacts

Review the example project fixture before writing:

```bash
threadpoint stage --root "$example_root/v0-baseline" --plan
```

Create private review copies for selected providers:

```bash
threadpoint stage --root "$example_root/v0-baseline" \
  --providers codex,claude,antigravity,kiro,opencode,cursor,openclaw \
  --apply --yes
```

Expected result: safe native artifacts become private review stages under
private threadpoint review state. Git-tracked sources map to `project-shared`,
while pre-existing ignored native sources map to `project-local` with a warning.
Threadpoint never adds or recommends native artifact ignores; other sources
require an explicit `--classify` mapping.

## Commit, Prune, And Restore

List a stage and review it before making canonical changes:

```bash
threadpoint stage list --root "$example_root/v0-baseline"
threadpoint stage diff <stage-id> --root "$example_root/v0-baseline"
```

Commit one reviewed stage, then inspect prune and restore paths:

```bash
threadpoint commit <stage-id> --root "$example_root/v0-baseline" --apply --yes
threadpoint prune --root "$example_root/v0-baseline" --plan
threadpoint restore list --root "$example_root/v0-baseline"
threadpoint restore --root "$example_root/v0-baseline" --plan --latest
```

Expected result: commit writes the reviewed stage to the matching project lane
without changing the Git index, and leaves prune as a dry run until `--apply`;
restore can list and plan from the commit backup.

## Inspect The Canonical Layout

After commit, use the same fixture for local checks and JSON record output:

```bash
threadpoint doctor --root "$example_root/v0-baseline"
threadpoint status --root "$example_root/v0-baseline"
```

Expected result: `status` lists the shared and local inventory, validates the
layout, and reports workflow readiness. `doctor` checks tool health.

## Updating Fixtures

When updating fixture content:

1. Keep the tree deterministic and free of secrets or personal data.
2. Refresh any command examples that depend on line-by-line fixture output.
3. Keep malformed or manual-review files explicit so diagnostics remain
   meaningful.
4. Keep examples rooted at the copied project fixture rather than neighboring
   directories or host-level provider state.
5. Update docs and tests that reference fixture paths and outcomes.
