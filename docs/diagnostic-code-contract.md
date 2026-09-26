# Diagnostic Code Contract

Threadpoint machine-readable outcomes use `code` and `message`. The code is a
finite unprefixed `snake_case` value for automation, such as
`discovery_sensitive_path` or `restore_target_changed`. The message is stable,
human-readable, and safe to show in command output.

Commands may include `remediation` and bounded, redacted `detail` when their
specific contract needs them. Human explanations attached to a curation choice
are `rationale`, not an operation outcome code.

Public JSON does not emit `reason`, `errorCode`, or `category`. This is the
initial contract: readers require the exact current fields and do not accept,
migrate, or infer a retired output shape. Add a new code only with a stable
message and a test that covers its public output.
