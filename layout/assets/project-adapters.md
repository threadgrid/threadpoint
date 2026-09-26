# Agent Adapters

The shared `AGENTS.md` and `.agents/` layout is the portable source of truth.
Keep agent-specific configuration outside this repository when possible. Do
not create or maintain shared memory in native agent locations.

When an external adapter is needed, have it read the canonical shared layout
instead of duplicating its contents.
