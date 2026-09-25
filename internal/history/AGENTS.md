# Conversation History DOX

## Purpose

- Own append-oriented per-channel conversation history, bounded context rendering, discovery, clearing, and point-in-time branching.

## Local Contracts

- Keep each channel/conversation independent, store complete durable JSONL with private permissions, and tolerate only explicitly handled tail corruption.
- `PromptContext` renders one bounded prompt window that always carries the current user entry; the full history path is returned as a separate value in every mode. Prior-history seeding is included only when the caller requests it; the application requests it unless the optional provider capability reports a retained conversation. Either non-positive limit disables seeding; seeding is bounded backward from disk under both message and character limits, and an unbounded history is never loaded to compute bounded context. A non-positive character limit leaves the current entry complete, while a positive limit bounds it by rune tail with a leading ellipsis or omits an entry whose complete reply identity cannot fit. In seeded mode the current entry is pinned by source identity and degrades to the current-only form when concurrent appends push it outside the bounded tail.
- Branch into a new conversation without mutating its source, retain bounded reply references, and exclude private attachment contents from records.
- Retention uses the last durable entry timestamp, falling back to file modification time only for empty histories; delete only strict pre-cutoff regular files and preserve explicitly protected live conversations while reporting per-item failures.
- Recent-authorized proactive routing reads a content-free durable latest-`user` activity sidecar per conversation, ignores assistant, notification, and delivery-ledger entries so delivery cannot select itself, and ignores sidecars without a corresponding history. It is only the minimal channel-resolution primitive and carries no policy or state.
- Keep source-message identity, local acceptance time, provider-neutral admission, logical execution, and exact terminal/cancellation coverage append-only and private. Duplicate-identity checks strictly stream the complete retained history with bounded per-entry memory and fail closed on corruption or unreadable entries.

- Own committed integration event replay and atomic bounded resynchronization snapshots. Cursors bind workspace/conversation, first history record and exact preceding record; invalid, cleared/replaced, corrupt, missing or excessive-lag cursors fail explicitly. Replay at most 256 raw records/4 MiB per read; snapshots pair the newest 100 visible entries under the existing 2 Mi-rune bound with the same append-lock cursor. A first subscription to an empty conversation appends only a private correlation baseline. Deltas/statuses are not durable subscription events, private ledgers stay hidden, notifications retain their existing outbox identity, and ordinary history retention/clear remains authoritative.

## Child DOX Index

No child DOX files.
