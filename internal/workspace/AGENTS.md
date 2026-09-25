# Workspace Initialization DOX

## Purpose

- Own initialized `.spynel` structure, embedded configuration defaults, repair of missing current assets, and user-overridable workspace copies.

## Local Contracts

- Create canonical private directories and configuration without following unsafe state or instruction symlinks; preserve user-edited themes and legacy user-owned prompt, instruction, and workflow-contract files untouched and inert during upgrades.
- Embed current configuration and workspace assets from `templates/` as source of truth for new workspaces; retired workflow prompts, role instruction files, and workflow contracts are not materialized. General compatibility migrations and retired asset fixtures do not belong in this package. Upgrade preserves configuration byte-for-byte; unused keys are ignored on load and disappear only on the next canonical save.
- Do not materialize workflow prompts, role instruction files, or task/goal workflow folders into new workspaces; legacy copies in existing workspaces remain untouched and inert.
- Delegate built-in palette assets to `internal/theme`; workspace initialization only asks that package to materialize editable copies.
- Automatically managed speech models live in the operating-system user cache, not the workspace.
- Create `.spynel/jobs` as private inspection-only job history.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [templates/AGENTS.md](templates/AGENTS.md) | Embedded workspace defaults and templates. |
