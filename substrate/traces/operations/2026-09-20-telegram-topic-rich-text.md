---
status: completed
created_at: 2026-09-20
updated_at: 2026-09-20
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
  - https://github.com/digitalygo/spynel/releases/tag/v1.2.0
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

## Update 2026-09-20: v1.2.0 publication

### Summary of changes

The pending state in "Release and publication status" above is resolved: `v1.2.0` is released from feature commit and tag target `ad11164b5e3e75901bfefe71a0860957d032c2e0`, the stable non-prerelease GitHub Release publishes four native archives plus `checksums.txt`, `@digitalygo/spynel@1.2.0` is the npm `latest` package with SLSA provenance v1, and the live npm-managed installation runs that release. No live Telegram forum canary was performed; the topic-routing and bounded rich-text behavior is evidenced by the automated mock-backed test coverage described in the original validation steps and by the release gates recorded below.

### Technical reasoning

The release reused the existing gates instead of adding publication steps. A manual `workflow_dispatch` validation run on the exact release commit (run 35508832299) passed the `verify` job and all four native build jobs for Linux amd64, Linux arm64, macOS amd64, and macOS arm64 while the `publish` job remained skipped, keeping validation separate from publication. Publishing the GitHub Release then triggered run 35509194685, which passed `verify`, the same four native jobs, and `publish`. The publish job attached four native archives plus `checksums.txt` and released npm through GitHub Actions Trusted Publishing with the workflow's `id-token: write` and `npm publish --provenance`, so no long-lived npm credential was used.

The host-target native package passed and the extracted archive reported `spynel 1.2.0`. Two environment and timing conditions are worth recording without treating either as a release defect. The clean-prefix exact-version installation needed a brief registry edge convergence window before it resolved `@digitalygo/spynel@1.2.0` and ran `spynel 1.2.0`. The updater's first attempt also hit a read-only local Linuxbrew npmrc `EACCES` after npm package extraction; a temporary permission change allowed the update to finish, the original `0444` mode was restored, and no persistent toolchain permission change remains. The `actions/download-artifact@v4` Node.js 20 deprecation annotation on the release run was informational and non-blocking.

### Impact assessment

- GitHub Release `v1.2.0` is public, stable, and non-prerelease at `ad11164b5e3e75901bfefe71a0860957d032c2e0`, with `checksums.txt` and the four supported native archives.
- npm `latest` resolves to `@digitalygo/spynel@1.2.0` with the `spynel` bin mapping, Digitalygo repository metadata, and a SLSA provenance v1 attestation.
- The live npm-managed home installation runs the `1.2.0` vendored binary, reports Telegram and Pi connected with an inactive turn, inherits model, reasoning, and service settings, and keeps task reviews at `never`; `spynel update --json check` reports current and latest `1.2.0` with no available update.
- Topic routing and rich-text delivery continue to carry the automated mock-backed coverage from the original work. No live Telegram forum canary is claimed, and none was run.
- The Linuxbrew npmrc permission incident was local to the updating environment. It produced no repository, workflow, package, or release change.

### Validation steps

- `git rev-list -n1 v1.2.0` resolves the tag to `ad11164b5e3e75901bfefe71a0860957d032c2e0`.
- Manual validation run 35508832299 completed successfully: `verify` passed, all four native build jobs passed, `publish` was skipped.
- Release run 35509194685 completed successfully: `verify` passed, all four native build jobs passed, `publish` passed.
- `gh release view v1.2.0` reports a stable non-prerelease release with `checksums.txt`, `spynel_1.2.0_darwin_amd64.tar.gz`, `spynel_1.2.0_darwin_arm64.tar.gz`, `spynel_1.2.0_linux_amd64.tar.gz`, and `spynel_1.2.0_linux_arm64.tar.gz`.
- Registry inspection confirmed `latest` at `1.2.0`, the `npm/bin/spynel.js` bin entry, the `git+https://github.com/digitalygo/spynel.git` repository, and the SLSA provenance v1 predicate.
- Host-target native packaging and execution of the extracted archive reported `spynel 1.2.0`.
- A clean-prefix installation of exactly `@digitalygo/spynel@1.2.0` returned `spynel 1.2.0` after the brief registry edge convergence window.
- The global npm installation reports `@digitalygo/spynel@1.2.0`, and the running primary executable resolves to the package's vendored binary; `spynel status --json` reports Telegram and Pi connected with `turn_active` false, and `spynel update --json check` reports `Current` and `Latest` both `1.2.0`.
- `scripts/dev.sh dox` and `git diff --check` passed for this documentation-only update.
