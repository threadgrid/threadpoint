# Provider Registry Extension Guide

The registry is the single source for supported provider-native artifacts.
Update it when adding a provider instead of adding path lists to commands or
diagnostics.

Each definition supplies:

| Field                        | Purpose                                                                                                                                  |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| provider ID and display name | Stable provider identity.                                                                                                                |
| project artifacts            | Relative paths, kind, read mode, recursion, and declared canonical scope.                                                                |
| entrypoints                  | Native files a provider uses to find canonical guidance.                                                                                 |
| workspace markers            | Specific provider-workspace detection. Workspace markers are also guarded implicit-root fallbacks, so adding one changes root discovery. |

Every artifact must declare one read mode. Use `import` only for bounded
regular-file content that can enter a review stage. `manual-review`,
`catalog-only`, and `denied` are reported for inspection but never enter a
review stage; use them for settings, hooks, generated state, or
behavior-changing surfaces that must not be copied into canonical guidance.

`catalog-only` is also a discovery result for otherwise importable directories
and symlinks: they are listed as metadata but never read into a stage. A safety
classification always takes precedence, so sensitive or runtime paths remain
`denied` even when they are directories or symlinks.

Paths must be relative to the selected root. Do not add home-directory,
parent-directory, or multi-root discovery.

For a new definition, add registry tests, corpus fixtures for each declared
artifact class, and discovery and stage coverage.

## Requesting Support

If you are requesting support rather than contributing an implementation, use
the repository's feature request form. New agents/providers and additional
project-native artifacts are standard feature requests. Include public vendor
documentation plus synthetic, project-relative artifact details only; do not
include absolute paths, private project content, or sensitive data. A request
does not promise support.
