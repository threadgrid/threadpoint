# Adapter Bootstrap

Threadpoint uses root `AGENTS.md` and `.agents/` as the shared project guide.
Do not create provider-specific instruction files for repository guidance.

Agents should load:

1. `AGENTS.md`
2. `.agents/knowledge/feature-catalog.json`
3. `.agents/knowledge/requirements-manifest.json`
4. `.agents/rules/threadpoint.md`

Tool-specific runtime configuration belongs outside the repository in that
tool's project-scoped config area.
