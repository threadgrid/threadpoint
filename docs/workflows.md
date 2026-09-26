# Workflows

Threadpoint works on one selected project root per command. It writes only the
shared and local lanes described in the [layout guide](canonical-layout-guide.md).

Run the examples inside your project. `--root` is optional; when omitted,
threadpoint [discovers the project](environment-and-state-policy.md#selected-root)
from the current directory. Use `--root /path/to/project` to select another
project, or `--root .` to select exactly the current directory without ancestor
discovery.

## Stage and commit

```bash
threadpoint init --yes
threadpoint stage --plan
threadpoint stage --apply --yes
threadpoint stage list
threadpoint stage diff <stage-id>
threadpoint stage edit <stage-id>
threadpoint stage difftool <stage-id>
threadpoint stage mergetool <stage-id>
threadpoint commit <stage-id> --plan
threadpoint commit <stage-id> --apply --yes
```

`stage` discovers supported provider-native artifacts below the selected root
and creates private, scope-pinned review copies. A tracked source maps to the
shared lane; an already ignored source maps to the local lane. Sources with no
clear Git classification require `--classify path=project-shared` or
`--classify path=project-local`.

`stage diff` writes the exact unredacted local unified diff to standard output
and warns on standard error; its lines can include raw artifact content, so do
not paste that output into support or public
channels. `stage edit` uses `$VISUAL`, then `$EDITOR`. `stage difftool` requires
`$DIFF` and invokes it with the source snapshot and review copy. `stage
mergetool` requires `$MERGE` and opens only after the provider-native source
has changed: its base is the frozen source snapshot, local is the review copy,
remote is the current source, and merged replaces the review copy on success.
Both external-tool commands require an interactive terminal. `commit` writes
exactly one reviewed target, records the reviewed source snapshot, and removes
that exact review copy after success. It never runs `git add` or `git commit`.

Use `threadpoint stage discard <stage-id> --yes` to abandon a review
copy.

## Prune and restore

After a successful commit, `prune` can remove the native source only if it
still matches the latest reviewed source snapshot:

```bash
threadpoint prune --plan
threadpoint prune --apply --yes
```

Changed sources remain in place and must be staged and committed again.
`restore list`, `restore --plan`, and `restore --apply --yes` operate on
backups for the same selected root.
