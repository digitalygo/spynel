---
status: completed
created_at: 2026-09-20
updated_at: 2026-09-26
files_edited:
  - AGENTS.md
  - docs/AGENTS.md
  - docs/architecture.md
  - docs/cli.md
  - docs/integrations.md
  - docs/troubleshooting.md
  - internal/AGENTS.md
  - internal/agentdocs/agentdocs_test.go
  - internal/agentdocs/content.go
  - internal/app/AGENTS.md
  - internal/app/service.go
  - internal/app/service_test.go
  - internal/channel/telegram/AGENTS.md
  - internal/channel/telegram/route.go
  - internal/channel/telegram/route_test.go
  - internal/channel/telegram/replyqueue.go
  - internal/channel/telegram/replyqueue_test.go
  - internal/channel/telegram/retry_test.go
  - internal/channel/telegram/telegram.go
  - internal/channel/telegram/telegram_test.go
  - internal/cli/AGENTS.md
  - internal/cli/cli.go
  - internal/cli/server_runtime.go
  - internal/markdown/AGENTS.md
  - internal/markdown/telegram_chunks.go
  - internal/markdown/telegram_chunks_test.go
  - internal/workspace/templates/workspace-AGENTS.md
rationale: Record Telegram topic routing, bounded HTML delivery, short transport retry, the durable final-reply queue with five exponential retry rounds, its v2.0.2 release, and the user-confirmed local deployment.
supporting_docs:
  - ../../../docs/architecture.md
  - ../../../docs/cli.md
  - ../../../docs/integrations.md
  - ../../../docs/releasing.md
  - ../../../docs/troubleshooting.md
  - https://core.telegram.org/api/forum
  - https://core.telegram.org/bots/api
  - https://core.telegram.org/bots/api#formatting-options
  - https://core.telegram.org/bots/faq
  - https://github.com/digitalygo/spynel/releases/tag/v1.2.0
  - https://github.com/digitalygo/spynel/releases/tag/v2.0.2
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

## Update 2026-09-26: bounded retry for Telegram text delivery

### Summary of changes

The first response after a successful private-topic rename was archived in job 388 at 10:16:32 local time, but no Telegram reply arrived. The turn ended 40.003 seconds later, matching the configured HTTP request timeout; a polling connection error had appeared before the final response. The exact outbound failure cannot be proven from the old logs because final sends ignored their errors. The adapter now retries transient failures of `sendMessage` text chunks at most twice and records content-free failure diagnostics.

### Technical reasoning

A timeout, connection reset, closed connection, or premature EOF triggers waits of one and then two seconds. The same chunk retains its topic route, payload, and reply reference; successful prior chunks are never replayed. One complete text delivery has a three-minute budget that includes all chunks, the existing single bounded 429 retry, the entity-parse plain-text fallback, and every retry wait. The fallback cannot reset either retry allowance. The current route authorization and caller context are checked before each request; shutdown, revocation, and cancellation stop further attempts. Definitive provider failures and malformed non-200 responses do not trigger transport retries. Typing, topic naming, native attachments, and polling retain their previous transport behavior.

Final reply and proactive text failures produce one content-free diagnostic with a category, optional provider code, and actual executed request count. Recovery after a transient retry records its retry count. A delivery failure does not turn successful harness execution into a failure: full output remains in history and the job archive, while `turn_completed` still describes harness completion rather than Telegram delivery. The implementation does not create a durable reply outbox or retroactively resend job 388. Telegram `sendMessage` offers no idempotency key; if it accepted a request whose response was lost, retrying that chunk can create a duplicate. This is bounded best effort, not exactly-once delivery.

### Impact assessment

- Remote text delivery can recover within the running process after a transient network fault. The pre-existing proactive outbox remains separate and can retry across process restarts; ordinary final replies cannot.
- All text-send attempts use the live channel context and recheck recipient authorization, including private topics and group policy changes. Transcript-echo failures remain non-fatal.
- The root, internal, Telegram, and docs DOX contracts, user guides, and compiled documentation now state the retry limits, duplicate edge, log behavior, and lack of durable reply recovery. No schema, configuration setting, or production installation changed.

### Validation steps

- The implementation race started from `273c244e8fa5c435b3e85a6acd503dfc2adff570`; one backend branch produced passing code and tests, the other produced no implementation. After independent adjudication and focused corrections, the winner was merged as `8e75cd5`.
- Orchestrator checks passed: `scripts/dev.sh dox` (49 tracked directories), `go test ./...`, `go vet ./...`, host `go build -o .tmp-bin/spynel ./cmd/spynel`, `scripts/smoke.sh`, `go test -race -count=1 ./internal/channel/telegram`, and `git diff --check`. The pre-existing untracked `.ai-telemetry/` was temporarily excluded only from local Git metadata during the DOX check and was not modified.
- Focused quality judgment returned `PASS`; package coverage was 88.4%, and the one newly added helper below 80% was the unused-in-production nil-waiter fallback at 75%. Focused security review returned `PASS` for authorization, cancellation, redaction, retry bounds, and topic routing. These were mock-backed checks; no live Telegram canary, release, or deployment was performed.

## Update 2026-09-26: durable queue for final Telegram text

### Summary of changes

The short in-process retry described above cannot cover the roughly fifteen-minute gap between job 388's final response and the observed polling reconnection, nor survive a process restart. This update supersedes the earlier no-durable-reply policy for **new** terminal Telegram text and direct handler errors. A primary-term worker now enqueues and delivers their rendered chunks from `.spynel/runtime/telegram-replies/`. Job 388 was not backfilled; its outbound failure was never proven from the old logs, and a polling error does not establish whether `sendMessage` was reachable.

### Technical reasoning

One reply is rendered after the application has applied hooks, media processing, and the downstream Pi session notice. The Telegram adapter persists the resulting HTML chunks, their plain-text fallbacks, route, verified bot account, original group sender, and first-chunk reply reference in one private atomic plaintext record. Records retain a contiguous acknowledged-chunk cursor and a fenced claim with an absolute deadline, nonce, and revision. The worker persists a reserved delivery round before calling Telegram, advances the cursor after each confirmed chunk, and persists full acknowledgment before unlinking the file. Crashes or lost provider acknowledgments can duplicate only the unconfirmed current chunk; an acknowledged earlier chunk is not replayed. A failed unlink leaves an acknowledged record for cleanup, not redelivery.

The schedule is one initial round plus **five** retry rounds, waiting 30 seconds, one minute, two minutes, four minutes, and eight minutes after each failed round finishes. Each round has a three-minute deadline below its four-minute claim. A usable 429 delay of at most 60 seconds can postpone the next scheduled round but never adds another round; a provider HTML entity rejection can downgrade its current chunk once to the persisted plain representation. The queued path does not use the short one-second/two-second retries. Exhaustion, permanent provider refusal, and recipient revocation stop network sends; the record is retained until its one-hour expiry, then deleted while a primary is running. The owner-term worker operates independently of the Telegram polling generation and continues expiry maintenance while the adapter is disabled. Only the exact owner can mutate persisted state, and the bot account plus live recipient, route, group policy, and original group sender are checked again before each request.

The queue is bounded to 128 records, 16 per conversation, 512 KiB per record, 16 MiB total, four concurrent routes, and FIFO order per route. Enqueue or storage failure logs content-free evidence and never falls back to an untracked direct send. Private 0700 directory and 0600 regular files, symlink rejection, bounded scanning, and strict current-schema validation protect the private payload. Direct transcript echoes, welcomes, proactive messages, and native attachments retain their existing behavior; an attachment may arrive before delayed final text. A completed harness job still does not assert successful remote delivery, and Telegram provides no exactly-once guarantee.

### Impact assessment

- New terminal replies and errors may survive network outages lasting through the fifth retry and process restarts. Five attempts do not guarantee delivery; each record expires one hour after creation. A stopped server cannot clean up until a primary runs again.
- Pending queue files contain the rendered reply, including a downstream Pi session notice when present, but no queue record is inserted into conversation history or the job archive. The earlier Pi contract that the notice is absent from **history, archive, and model-visible input** remains true; it is now present temporarily in private queue storage.
- Native attachments do not enter the queue and are not replayed after restart. Pre-queue failures, including job 388, are not reconstructed or resent. The proactive notification outbox remains separate.
- Root, channel, CLI, workspace-template and documentation DOX, public docs, and compiled agent documentation were updated to distinguish durable final text from the pre-existing direct short-retry families.

### Validation steps

- Baseline for this update: `8e75cd536bc3826e5d9d79d76e8f80946b09346d` plus previously uncommitted short-retry documentation, recorded in `substrate/traces/status/2026-09-26-telegram-durable-replies-workspace-state.md`. The winning implementation was preserved as `4e41933` during an interrupted review and corrected as `afcf0ef`; the other race member produced no code. Only intended source and test files were merged into `main`, with pre-existing documentation changes preserved.
- The orchestrator ran `scripts/dev.sh dox` (49 tracked directories), `go test ./...`, `go vet ./...`, a host `go build -o .tmp-bin/spynel ./cmd/spynel`, `scripts/smoke.sh`, `go test -race -count=1 ./internal/channel/telegram ./internal/cli`, `go test -cover ./internal/channel/telegram` (84.6% statement coverage), and `git diff --check`; all passed. The pre-existing `.ai-telemetry/` was temporarily ignored only through local Git metadata for DOX/smoke without changing its contents.
- The judgment quality gate returned `PASS` for behavioral tests, durable storage, scheduling, and synchronized documentation. The focused security gate returned `PASS` for term fencing, authorization, private bounded state, and content-free logs. All tests were local and mock-backed; this update did not contact live Telegram, release a package, deploy or restart the running installation.

## Update 2026-09-26: v2.0.2 publication and pending local update

### Summary of changes

The durable queue and short-retry patch were committed and pushed to `main`, then published in the stable [v2.0.2 release](https://github.com/digitalygo/spynel/releases/tag/v2.0.2). The installed npm-managed bot remains at v2.0.1; no local update or live Telegram canary has been attempted in this publication step.

### Technical reasoning

Commit `32640a9d87afd7e754a857c5222d86554a00f02d` added the synchronized DOX, documentation, and compiled catalog to the three preceding implementation commits. The clean pre-release gate found an existing webhook-test race: a two-second test timeout was shorter than the server's five-second shutdown budget, and an unused HTTP keep-alive connection could hold teardown until its deadline. A test-only correction kept a bounded assertion for both `deleteWebhook` and listener shutdown while disabling test-client keep-alives; candidate commit `431e097e89b4c26568d7801f4609a8c76cd185e5` passed repeated targeted tests and the complete local gates. No production behavior changed in that correction.

The [nonpublishing validation run](https://github.com/digitalygo/spynel/actions/runs/36243673959) on that exact commit passed verification and all four native Linux/macOS builds; `publish` was skipped. The annotated tag object `b700fa8554072b006092d9587713ad16195bc39e` peels to the same commit. Publishing the GitHub Release triggered [release run 36244086074](https://github.com/digitalygo/spynel/actions/runs/36244086074), which passed verification, four native jobs, and mandatory `publish`. The five public assets are four supported native archives plus `checksums.txt`. Published Linux amd64 archive SHA-256 `dd700ac9eb8be0c1f70ef56b14399cc7b3aa458d5bdb654e6b156183d747424b` matched the published checksum and the extracted binary reported `spynel 2.0.2`. npm `@digitalygo/spynel@2.0.2` is on `latest` with SLSA provenance v1 after a bounded registry-convergence delay.

### Impact assessment

- The stable release is public, but publication and local deployment remain separate actions. The local npm-managed service is still v2.0.1, with Telegram and Pi connected, no active turn, zero live jobs, and four delivered, zero pending proactive outbox entries at read-only preflight. `spynel update --json check` reports v2.0.2 available and the coordinated restartability check passes.
- Telegram currently has two authorized senders. Spynel has no admission pause during an update: a message admitted between an idle check and process shutdown can lose its active turn, while the new queue protects only finals persisted after the update. As in the v2.0.1 deployment, local update is deferred until the operator explicitly confirms both authorized accounts will not send during the restart window. This request to update did not establish a quiet window by itself.
- The known Linuxbrew npmrc file under the Node 24 Cellar is a regular file owned by the user at mode 0444 with SHA-256 `a0e43e04265c9fc6231f0288288e0088ea033732278f9953d65551dbb8930ccd`. If the official updater requires temporary owner-write access, its original mode, inode, and digest must be restored and verified. No permission change was made during the read-only preflight.

### Validation steps

- Local exact-candidate checks from a clean detached worktree passed `scripts/dev.sh dox`, `scripts/dev.sh test` (including the nested Bubble Tea module), `scripts/smoke.sh`, `npm run test:npm`, Telegram/CLI race tests, host native packaging with companion libraries and extracted `spynel 2.0.2`, `npm/prepare-release.js v2.0.2 false`, and `npm pack --dry-run` (nine files). The main tree kept the `0.0.0-development` placeholder and the pre-existing `.ai-telemetry/` remained untouched.
- Workflow dispatch run 36243673959: `headSha=431e097e89b4c26568d7801f4609a8c76cd185e5`, `verify` and Linux/macOS amd64/arm64 builds all passed, `publish` skipped. Each retained `observed-native` evidence record reports that source commit and six passing synthetic/provider-free checks.
- Published release run 36244086074: same source commit, `verify`, four native jobs, and `publish` all passed. Exactly five public assets were listed. Published host archive passed the published checksum; registry `latest` resolved to 2.0.2 with SLSA provenance v1.
- Passive local preflight: installed CLI and npm package 2.0.1; managed server PID 2090417 and Node supervisor PID 2090396; Telegram connected, Pi connected, inactive turn, zero live jobs, proactive outbox four delivered/zero pending, `spynel check-restartable` successful, `spynel update --json check` shows `Current=2.0.1`, `Latest=2.0.2`, `Available=true`. The workspace and runtime directories remain at 0700, config file 0600. No package replacement or restart was attempted.

## Update 2026-09-26: local v2.0.2 deployment

### Summary of changes

After the operator confirmed a quiet window covering both authorized Telegram senders, the local npm-managed installation was updated through one official `spynel update` execution. The running server and launcher kept PIDs 2090417 and 2090396 while the primary generation changed to `bceeda55053920cd7d8bd9a8014b2602`. The CLI, npm package, and vendor metadata now report 2.0.2. No repository code or production configuration changed during deployment.

### Technical reasoning

The user confirmation was a separate prerequisite from the earlier authorization to publish and update: the updater cannot fence a newly admitted user message during shutdown. Immediately before running the updater, the installation-scoped restartability check passed and the live status still showed Telegram and Pi connected, zero live jobs, and an inactive turn. The update ran from the independent managed launcher under the same `HOME` as the supervisor, not from the repository binary or a process hosted by the target server. The updater stopped the old generation, replaced `@digitalygo/spynel`, and coordinated the restart of one registered server. The content-free runtime log shows `session_end` at 17:42:06 UTC, then `session_start`, `primary_started`, and Telegram `connected` by 17:42:07 UTC; no `turn_started` event appeared in that interval.

The Linuxbrew npmrc was a regular file owned by the current user, mode 0444, inode 281723, with SHA-256 `a0e43e04265c9fc6231f0288288e0088ea033732278f9953d65551dbb8930ccd`. An EXIT/INT/TERM/HUP restoration trap was installed before temporarily granting owner-write access at 0644, and restored mode 0444 with the original inode and digest after the official updater returned successfully. npm printed an `allowScripts` advisory for the package's postinstall script, but the installed vendor binary and metadata independently report 2.0.2 and the service is ready; no direct npm install, killall, or manual restart was used.

### Impact assessment

- The durable final-reply queue is now part of the running Spynel 2.0.2 process. `.spynel/runtime/telegram-replies/` is created lazily on first enqueue, so its absence immediately after an idle restart means no records exist, not a failure. No live model message or Telegram send canary was sent; delivery behavior still rests on synthetic and mock-backed tests plus passive readiness checks.
- Existing state was preserved: workspace and runtime modes remained 0700, config mode 0600, config SHA-256 `fb99602c2be66570acaa538aa2e13d43ade695061fbb6f9504f2091d4336e1c4`, Pi session-map SHA-256 `72d5c9f6bee40093e4a22295bbc42e5b982c43bf4bab6c8f7063a3ec8262d623`, and the proactive outbox kept four delivered and zero pending entries. No prior undelivered job, including job 388, was backfilled.
- The quiet-window restriction ended only after the new primary reported Telegram and Pi connected with zero live jobs and an inactive turn.

### Validation steps

- Update request started at 17:42:01 UTC and the official launcher returned at 17:42:07 UTC reporting `spynel 2.0.2` and one coordinated server restart. The npmrc trap restored its original mode, inode, and SHA-256 after the updater returned.
- An independent read-only recheck confirmed CLI version 2.0.2, global npm package 2.0.2, vendored binary metadata 2.0.2, same server and launcher PIDs with the new primary generation, Telegram and Pi connected, `jobs=0`, `live_jobs=0`, `turn_active=false`, and `spynel update --json check` with `Current=Latest=2.0.2` and `Available=false`.
- Config/session-map digests and permission modes matched the preflight snapshot, the proactive outbox remained four delivered and zero pending, and no queued reply files or new message turn were observed during the update window. Release workflow run 36244086074 remained successful at the published tag commit.
