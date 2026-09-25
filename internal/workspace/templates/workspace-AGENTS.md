# Spynel workspace DOX

This directory is durable state owned jointly by the user, Spynel, and the coding harness sessions it hosts.

## Contracts

- Spynel is a classic program with no AI in its core. It coordinates external AI/coding harnesses through one assistant-facing relationship: you converse with Spynel, and each conversation runs its own harness session that does the actual work. Simplicity at scale. Never describe Spynel itself as an agent, and never invent product capabilities or guarantees.
- Every channel conversation has an independent harness session and an independent durable history. Sending a message continues that conversation's session; starting or resuming a conversation starts or resumes its session. There is no required document, task, or goal to create first: ordinary requests in chat are enough, and the harness asks for clarification when a request is ambiguous.
- `.spynel/config.yaml` is the private user configuration for the workspace root one directory above this state directory. The `.spynel` location is fixed; no `workspace.state_dir` setting exists.
- Exactly one elected primary process runs the remote channels and background services, and every local interface reaches it over an authenticated loopback connection. Do not edit `.spynel/runtime/`; Spynel owns election records, harness session mappings, logs, outbox state, and transient runtime files.
- Do not edit or publish `.spynel/history/`, `.spynel/jobs/`, `.spynel/attachments/`, `whatsapp.db`, or harness session data; they may contain secrets, captured execution output, remote media, and private conversations.
- Automatically managed speech models live outside this workspace. Original voice attachments remain workspace-owned.
- All Markdown files under `prompts/`, including `chat.md`, are legacy copies from retired workflows: they stay byte-for-byte untouched and inert. Spynel materializes and reads no prompt template from this directory. Keep project-specific guidance in the workspace root and nearer project `AGENTS.md` files.
- Legacy `instructions/`, `tasks/`, and `goals/` content is user data from retired workflows. Initialization and upgrade never create or inspect those directories, and Spynel never processes, recreates, or deletes existing copies; they stay byte-for-byte untouched and inert. The retired `runtime/leases/` directory is likewise never created or inspected, and any existing copy stays untouched. Do not read retired workflow documents as current product behavior.
- Themes in `.spynel/themes/` are user-overridable semantic TUI palettes. Keep every required color and a unique safe name so Spynel can validate and switch them atomically.
- Extensions in `.spynel/extensions/` are trusted executable code. Review repositories before installing them.

## Structure

- `config.yaml` owns validated user configuration for the workspace.
- `prompts/` appears only in older workspaces as inert retired-workflow data.
- `themes/` owns named semantic TUI and terminal-Markdown palettes.
- `extensions/` owns installed Git extensions and hook manifests.
- `history/` owns independent append-only histories per channel and conversation.
- `jobs/` owns private bounded job metadata, the atomic 1..9999 job-number sequence, and captured event output; private generation identities prevent reuse collisions.
- `attachments/` owns private TUI, Telegram, and WhatsApp media referenced from messages.
- `runtime/` owns election records, harness session mappings, logs, outbox state, and transient bounded work files.
- `instructions/`, `tasks/`, `goals/`, and `runtime/leases/` appear only in older workspaces as inert retired-workflow data and are never recreated or inspected.
