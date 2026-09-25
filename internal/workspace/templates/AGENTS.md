# Embedded Workspace Template DOX

## Purpose

- Own defaults embedded into newly initialized workspaces: configuration, extension guidance, and workspace DOX.

## Local Contracts

- Treat current configuration, extension guidance, and workspace DOX files here as source for new workspaces while preserving user-modified copies when missing assets are repaired.
- Do not store theme assets, migration fixtures, retired workflow prompts, role instruction files, or other package-owned data in this directory.
- Keep configuration defaults and workspace DOX synchronized with current runtime behavior and tests.
- Do not ship or materialize task/goal workflow contracts, workflow prompts, role instruction files, or retired orchestrator defaults; the retired workflow packages and their assets have been deleted from the repository.
- Default configuration must validate without secrets and must not introduce a configurable state directory.
- Default model reasoning effort and service mode remain empty so existing provider behavior is inherited until a user makes a supported selection.

## Child DOX Index

No child DOX files.
