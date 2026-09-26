# Lock Recovery

Threadpoint prevents concurrent project mutations with one cooperative lock per
canonical project root beneath `THREADPOINT_HOME/locks`. Mutating commands
normally reclaim locks whose owner process is gone, whose live owner has passed
the stale threshold, or whose malformed metadata has passed its grace period.

If a command remains blocked, select that command's project explicitly for
recovery, replacing `/path/to/project` with its root:

```bash
threadpoint lock clear --root /path/to/project
```

The command removes only a lock classified stale by the normal policy. It is
safe to repeat and succeeds when no lock is present.
The `--root` flag is optional when running inside the same project and using
the same [root discovery](environment-and-state-policy.md#selected-root).

For a fresh lock that you have verified is no longer needed, first stop the
recorded owner, then explicitly acknowledge the concurrency risk:

```bash
threadpoint lock clear --root /path/to/project --force --yes
```

Force cleanup detaches and removes only the exact regular lock entry it
reviewed. It cannot make a still-running writer safe; using it while the owner
is active can allow overlapping mutations and corrupt project state.
