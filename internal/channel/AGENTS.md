# Channel Runtime DOX

## Purpose

- Own the common channel contract, activity signaling, and hot-reload supervisor for TUI, Telegram, and WhatsApp adapters.

## Local Contracts

- Adapters translate messages and screens through `core` and the application service; they never invoke harness implementations directly.
- The supervisor consumes refreshed shared configuration snapshots, replaces only changed adapters, revokes stale adapter authority before cancellation, isolates failures, publishes lifecycle state, and retries only eligible unchanged configurations.
- Activity references must be balanced and overlap-safe so an older turn cannot clear a newer turn's visible activity.
- `ConversationLabeler` and `ConversationLabelRouter` are optional best-effort capabilities: an adapter renames one provider-visible conversation label only after re-applying its own authorization and route rules, and the supervisor forwards each request to the active generation of the named channel. An automatic rename (`force=false`) is at most once per conversation and process, and a conversation the adapter already renamed is a silent no-op success, while `force=true` always renames. `ErrConversationLabelUnsupported` marks a missing, disconnected, or incapable generation so callers treat inapplicable channels as silent no-ops; a failed rename never affects the turn or reply.
- Route proactive canonical conversation events only through the currently connected channel generation and bind their activity references to that generation's cancellation context so disconnect, replacement, and shutdown clean up safely.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [telegram/AGENTS.md](telegram/AGENTS.md) | Telegram authorization, polling/webhook, media, and delivery. |
| [tui/AGENTS.md](tui/AGENTS.md) | Interactive terminal UI, rendering, forms, and input. |
| [whatsapp/AGENTS.md](whatsapp/AGENTS.md) | WhatsApp pairing, authorization, media, and delivery. |
