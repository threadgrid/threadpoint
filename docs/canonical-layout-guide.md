# Canonical Layout Guide

Run `threadpoint init --yes` inside your project to create the shared agent
layout under its selected root. `--root` is optional and overrides
[project discovery](environment-and-state-policy.md#selected-root).
An unmarked subdirectory can select an ancestor. Init names the selected root
in its prompt and reports it on success; use `threadpoint init --root . --yes`
to initialize the current directory as a separate project.

```text
<root>/
  AGENTS.md
  .agents/
    adapters/README.md
    knowledge/
    rules/
    skills/
    plugins/
  state/
    skills/
```

`AGENTS.md` and `.agents/` are the shared lane. `init` also creates the
ignored `state/skills/` runtime skeleton and adds the anchored `/state/` rule
to `.gitignore`, preserving all other rules and never changing the Git index.
Each portable skill definition belongs at `.agents/skills/<skill-name>/`; use
`scripts/` for executable helpers and keep mutable runtime data at
`state/skills/<skill-name>/`. Do not use skill-local `bin/` directories or
store runtime data inside the skill definition.

`AGENTS.local.md` and `.agents.local/` are the local lane. They are created
only when a local import needs a canonical destination, which then adds their
anchored ignore rules.

`CLAUDE.local.md` is a provider-native project artifact whose declared target
is `AGENTS.local.md`. Provider entrypoint files remain provider-owned inputs;
threadpoint does not generate or rewrite them.

New canonical files and directories honor the process umask. Replacing a
regular canonical file retains its existing mode. Review copies live under the
threadpoint state home outside the selected project and are addressed by stage ID.
