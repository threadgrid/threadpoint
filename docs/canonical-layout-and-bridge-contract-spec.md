# Canonical Layout and Provider Entrypoint Contract

The selected root has two canonical lanes and one ignored runtime directory:

| Area    | Paths                               | Git state                   |
| ------- | ----------------------------------- | --------------------------- |
| shared  | `AGENTS.md`, `.agents/`             | ordinary repository content |
| local   | `AGENTS.local.md`, `.agents.local/` | ignored and untracked       |
| runtime | `state/skills/<skill-name>/`        | ignored and untracked       |

`init` creates missing shared paths, the `state/skills/` runtime skeleton, and
the anchored `/state/` ignore rule. Local-lane paths and their ignore rules are
created only when a local import needs them. Skill definitions use
`.agents/skills/<skill-name>/` and `scripts/` helpers; they do not contain
mutable runtime data or skill-local `bin/` directories. `init` does not manage
provider-native paths or the Git index.

Provider registry definitions describe known native entrypoints and artifacts.
Entrypoints are native provider files that point an agent to `AGENTS.md` and
`.agents/`; threadpoint may inspect them during discovery but never creates,
rewrites, or treats them as canonical content.

`stage` reads regular, importable artifacts under the selected root into private
review copies. A stage has a fixed source, scope, provider, kind, and target.
`commit` writes that one target after validation. `CLAUDE.local.md` carries a
fixed local-lane target regardless of Git classification.

No canonical or provider-native path is inferred from a parent directory, a
home directory, or a second project root.
