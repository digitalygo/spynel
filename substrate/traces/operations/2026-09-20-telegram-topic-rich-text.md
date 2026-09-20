---
status: completed
created_at: 2026-09-20
files_edited:
  - AGENTS.md
  - docs/AGENTS.md
  - docs/architecture.md
  - docs/cli.md
  - docs/integrations.md
  - internal/AGENTS.md
  - internal/agentdocs/agentdocs_test.go
  - internal/agentdocs/content.go
  - internal/app/AGENTS.md
  - internal/app/service.go
  - internal/app/service_test.go
  - internal/channel/telegram/AGENTS.md
  - internal/channel/telegram/route.go
  - internal/channel/telegram/route_test.go
  - internal/channel/telegram/telegram.go
  - internal/channel/telegram/telegram_test.go
  - internal/markdown/AGENTS.md
  - internal/markdown/telegram_chunks.go
  - internal/markdown/telegram_chunks_test.go
rationale: Record the completed Telegram topic-conversation routing and bounded HTML rich-text delivery work together with the synchronized documentation, DOX, and compiled agent documentation pass.
supporting_docs:
  - ../../../docs/architecture.md
  - ../../../docs/cli.md
  - ../../../docs/integrations.md
  - https://core.telegram.org/api/forum
  - https://core.telegram.org/bots/api
  - https://core.telegram.org/bots/api#formatting-options
  - https://core.telegram.org/bots/faq
---

# Telegram topic conversations and bounded rich-text replies

## Summary of changes

Telegram now routes private threaded chats and forum supergroup topics into canonical `-topic-<thread-id>` conversations. Every thread ID of at least 2 receives an independent durable history and harness session, including Pi, while absent threads and the General topic keep the existing base conversation. Replies, terminal errors, typing, welcomes, native attachments, proactive events, and recovery delivery stay in the originating topic, and the current allow-list and `group_mode` policy are reapplied at every inbound and outbound boundary.

Agent Markdown for Telegram renders to the supported HTML subset and is delivered as independently valid `sendMessage` HTML chunks: at most 4096 parsed-visible Unicode code points per message and 32768 parsed-visible code points across at most eight messages per reply. Formatting stays balanced across chunk boundaries, an over-budget reply ends with a visible truncation marker while durable history stays complete, one chunk may retry once as plain text after a provider HTML entity parse rejection, and one rate-limit rejection may retry once with the exact route authorization rechecked.

The documentation pass synchronized `docs/integrations.md`, `docs/architecture.md`, `docs/cli.md`, and the compiled `internal/agentdocs` catalog with the implemented behavior, updated the owning DOX contracts at the root, `internal`, `internal/app`, `internal/channel/telegram`, `internal/markdown`, and `docs`, and recorded the result here.

## Technical reasoning

### Canonical conversation grammar

Routes are built and parsed only through a strict grammar: `TG-<positive-user-id>`, `TG-<positive-user-id>-topic-<positive-thread-id>`, `TG-group-<negative-chat-id>`, and `TG-group-<negative-chat-id>-topic-<positive-thread-id>`. Absent threads and thread ID 1 (the General topic) normalize to the base conversation and outbound thread ID 0, so existing histories and harness sessions keep their identifiers without migration. Reusing the base identity for the General topic avoids splitting a forum's main channel into a second history. Parser and constructor failures fail closed: leading zeroes, signs, whitespace, Unicode digits, suffix junk, non-positive thread IDs, and overflow are rejected, and every outbound path re-parses the conversation string instead of trusting a cached chat ID.

### Authorization stays policy-based

Topic routing is automatic, default-on, and added no configuration setting, so `group_mode` and `allowed_users` remain the only transport policies. Thread IDs never grant authorization by themselves. Private topic conversations authorize through their base numeric user with the live allow-list and the verified username mapping; group topic conversations honor the current `group_mode`. The adapter runs the same policy immediately before every route-bound provider call and again before the single bounded retry, so a revocation or recipient change during a rate-limit wait cannot complete a stale send. The shared application origin validator uses the same parser for proactive deliveries, which keeps topic origins consistent with terminal replies.

### Route-preserving delivery

Inbound and outbound paths carry one route value instead of a bare chat ID. Replies, terminal errors, typing actions, welcome messages, native document and photo uploads, proactive events, and recovery delivery all address the route's `message_thread_id` when it names a topic, and base or General-topic delivery omits the field. Typing references are keyed by route, so stopping one topic cannot silence another concurrent topic. The reply reference anchors only the first delivered text chunk.

### Bounded rich-text chunking

Telegram documents a 4096-character message limit, so the implementation counts parsed-visible Unicode code points: HTML tags contribute nothing and an entity spelling counts as its decoded code point. The complete reply budget is 32768 code points, eight messages of at most 4096. Chunking renders ordinary agent Markdown through the trusted Telegram HTML conversion exactly once, parses only the renderer-owned allowlisted subset, closes open formatting tags at each boundary, and reopens them with attributes intact on the next chunk. An entity or Unicode code point is never split, grapheme boundaries are preferred whenever the remaining tail still fits the remaining chunks, and the hard per-message limit wins when both constraints cannot hold. Replies above the reply budget truncate deterministically, preferring a grapheme boundary when the hard limits allow and never splitting a Unicode code point, and end with `… (response truncated; full response available elsewhere)` inside the same budget; durable history and the complete agent turn are untouched. This stays on the ordinary Bot API `sendMessage` endpoint with `parse_mode: HTML`; the implementation does not use a rich-message endpoint.

### Fallback and rate limiting

Only a provider 400 whose description reports an entity parse failure downgrades that single chunk once to plain text; every other provider failure remains visible and is never silently downgraded. One rate-limit response may retry once with the provider's `retry_after` when it is positive and at most 60 seconds, and the retry rechecks route authorization first. Larger or repeated rate limits fail without waiting or looping.

## Impact assessment

- Base conversation identities are unchanged. Threaded chats and forum topics create new sanitized durable histories under `.spynel/history`, each with an independent session keyed by its conversation, so conversation context does not bleed across topics or harnesses.
- Explicit `spynel notify` origins accept the topic forms and are reauthorized against the current policies; recent-authorized routing continues to exclude every group conversation, including group topics.
- No configuration setting, schema, or migration was added. Existing `group_mode` and allow-list behavior still governs group and private access.
- Telegram delivery keeps its documented at-least-once behavior and crash-duplicate caveat. Truncation affects only the remote message; the complete response remains in durable history.
- Documentation, DOX contracts, and the compiled agent catalog now describe the same identity grammar, authorization rules, and bounded rich-text behavior.

## Validation steps

- Ran `go test -count=1 ./internal/channel/telegram/ ./internal/markdown/ ./internal/app/ ./internal/agentdocs/`; all packages passed. These tests cover canonical route parsing and rejection, inbound topic routing, per-topic delivery of replies, errors, typing, proactive events, and attachments, base and General-topic omission, activity isolation between topics, fail-closed outbound routes, HTML parse fallback and non-downgrade, bounded and reauthorized rate-limit retries, topic origin validation, and recent-authorized group-topic exclusion.
- Ran `go test ./...`; all 28 packages with tests passed.
- Ran `go vet ./...`; no findings.
- Built the host binary with `go build -o .tmp-bin/spynel ./cmd/spynel`.
- Ran `scripts/dev.sh dox`; reported `DOX coverage valid for 51 tracked directories`.
- Ran `git diff --check`; no whitespace errors.
- No repository markdownlint configuration or installed Markdown linter was available. A structural check of every changed Markdown file confirmed one H1 per file, uninterrupted heading progression, no introduced em dash, no trailing whitespace, and a single trailing newline.

## Release and publication status

The intended release target is `v1.2.0`. Publication is pending the standard release gates: a published `v1.2.0` GitHub Release, the release workflow's Go test/vet/build plus smoke and npm launcher checks, all four native build jobs on matching runners, checksums, and npm publication of `@digitalygo/spynel`. Host-target `scripts/package-native.sh` validation and execution of the extracted archive remain useful pre-release checks. This record does not claim that `v1.2.0` is published.

## References

- [Telegram Bot API](https://core.telegram.org/bots/api)
- [Telegram Bot API formatting options](https://core.telegram.org/bots/api#formatting-options)
- [Telegram forum topics](https://core.telegram.org/api/forum)
- [Telegram Bot FAQ](https://core.telegram.org/bots/faq)
- [Communication integrations](../../../docs/integrations.md)
- [Architecture](../../../docs/architecture.md)
- [Plain CLI and automation](../../../docs/cli.md)
