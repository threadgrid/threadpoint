# Redaction And Diff Safety

Threadpoint can read local agent guidance, provider-native instruction files,
private review copies, backups, and managed project guidance. Some commands display
plans or diffs so users can review local changes before applying them.

Those outputs are useful, but they can contain private memory or obvious
secrets if the source files already contain them. The initial threadpoint release applies
best-effort redaction to product-owned diagnostics, recovery plans, and review
candidates. `stage diff` is a deliberate raw local review surface: it prints
the unredacted local stage diff and warns on standard error instead of
redacting its artifact content. It also replaces configured private roots with stable placeholders such as `$ROOT`,
`$THREADPOINT_HOME`, and `$HOME`. Users should still treat local paths,
reports, and backups as potentially sensitive.

## Where Raw Local Content Can Appear

The highest-risk output surfaces are:

- `threadpoint stage diff <id>`, which prints the unredacted source and review
  content in a local unified diff;
- `threadpoint stage edit <id>`, which opens the private review copy;
- `threadpoint stage difftool <id>` and `threadpoint stage mergetool <id>`,
  which pass raw private bytes to the explicitly configured `$DIFF` or `$MERGE`
  program;
- `threadpoint restore --plan`, which reports restore candidates and conflict
  diffs;
- interactive `threadpoint restore`, which prints conflict diffs before
  prompting for restore, edit, or skip.

`stage`, `commit`, `prune`, `doctor`, and `status` primarily report paths,
metadata, hashes, findings, and actions rather than raw provider-native file bodies.
Path names can still reveal local project details.

## Best-Effort Redaction

Threadpoint redacts obvious sensitive values in restore plans and review
candidates intended for review or sharing. Redaction covers
representative patterns such as:

- API key, token, secret, credential, and password assignments;
- authorization headers;
- common access-token formats;
- private-key blocks.

It also replaces the selected root, threadpoint state root, and the user home
root with stable placeholders on these product-owned surfaces. `status` and
`doctor` remain machine-readable operational reports; their path output should
still be treated as sensitive and reviewed before sharing.

When redaction changes a JSON candidate, the candidate includes:

```json
{
  "redacted": true
}
```

Interactive previews print a note when values or private roots were redacted.

## What Redaction Does Not Change

Redaction is not local data sanitization. It does not remove secrets from:

- source provider-native files;
- private review copies;
- backups;
- raw `stage diff` output or the external programs selected through `$DIFF` or
  `$MERGE`;
- editor buffers opened by interactive workflows;
- final committed or restored local files.

Threadpoint keeps internal apply behavior based on the original local content so
users can still review, edit, commit, restore, or clean up the source data.

## Safe Sharing Guidance

Before sharing command output publicly:

- prefer JSON or text reports that do not include raw content or diffs;
- do not share `stage diff` output or the data shown by a configured diff or
  merge tool; review `restore` plans before pasting them;
- remove local paths, project names, backup payload paths, and raw
  memory content that are not needed for the issue;
- rotate real credentials if they were present in local memory or command
  output.

Future diagnostics bundle work should use the same redaction rules and should
avoid raw memory content by default.
