# Shared Agent Memory

## Canonical locations

Maintain shared instructions in `AGENTS.md`. Maintain shared knowledge, rules,
skills, plugins, and adapter guidance beneath `.agents/`. These files are the
portable source of truth for every agent that works in this project.

## Native locations

Do not create, copy, or maintain shared memory in agent-native files or
directories. Treat native locations as external sources that may be reviewed
and imported into the canonical layout, never as canonical destinations.

## Maintenance

Keep instructions concise, current, and project-specific. Put durable facts in
`.agents/knowledge/`, behavioral constraints in `.agents/rules/`, and reusable
procedures in `.agents/skills/`. Review existing canonical material before
adding or changing it, and preserve user-authored content.

## Skill layout

Keep each portable skill definition under `.agents/skills/<skill-name>/`. Use
`scripts/` for executable helpers, not a skill-local `bin/` directory. Keep
mutable runtime data outside the definition at
`state/skills/<skill-name>/`; the complete `state/` directory is ignored by
Git.
