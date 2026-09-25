---
status: completed
created_at: 2026-09-21
updated_at: 2026-09-26
files_edited:
  - AGENTS.md
  - docs/AGENTS.md
  - docs/architecture.md
  - docs/cli.md
  - docs/harness-compatibility.md
  - docs/integrations.md
  - internal/AGENTS.md
  - internal/agentdocs/content.go
  - internal/app/AGENTS.md
  - internal/app/pi_control.go
  - internal/app/pi_control_test.go
  - internal/app/service.go
  - internal/app/session_name.go
  - internal/app/session_name_test.go
  - internal/channel/AGENTS.md
  - internal/channel/channel.go
  - internal/channel/supervisor.go
  - internal/channel/supervisor_test.go
  - internal/channel/telegram/AGENTS.md
  - internal/channel/telegram/telegram.go
  - internal/channel/telegram/topic_test.go
  - internal/cli/cli.go
  - internal/harness/AGENTS.md
  - internal/harness/harness.go
  - internal/harness/pi.go
  - internal/harness/pi_sessions.go
  - internal/harness/pi_sessions_test.go
  - internal/harness/process_fixture_test.go
  - internal/harness/supervisor.go
  - internal/harness/supervisor_test.go
rationale: Record automatic Pi session naming, private Telegram topic renaming, and explicit `/pi name`, including the at-most-once title rule and the correction for Pi's delayed first-session file creation, with synchronized DOX, compatibility evidence, and verification, plus the v2.0.1 publication and the user-confirmed local deployment closure.
supporting_docs:
  - ../../../docs/architecture.md
  - ../../../docs/cli.md
  - ../../../docs/harness-compatibility.md
  - ../../../docs/integrations.md
  - ../../../docs/releasing.md
  - 2026-09-20-pi-session-controls.md
  - 2026-09-20-telegram-topic-rich-text.md
  - 2026-09-21-pi-retained-prompt-context.md
  - https://core.telegram.org/bots/api#editforumtopic
  - https://core.telegram.org/bots/api#forumtopiccreated
  - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md
  - https://github.com/digitalygo/spynel/releases/tag/v1.4.0
  - https://github.com/digitalygo/spynel/releases/tag/v2.0.1
  - https://github.com/digitalygo/spynel/actions/runs/36193270388
  - https://github.com/digitalygo/spynel/actions/runs/36194463563
---

# Pi session and Telegram topic naming

## Summary of changes

Spynel names a Pi session from the first user message that creates it and renames a still-implicit private Telegram topic to the same effective name. `/pi name <name>` adds the explicit manual path for an existing session. Naming is best-effort metadata: it never changes a session identity, never affects the turn or reply, and is not retried.

- Automatic naming derives a label from the accepted user message, sends it through the optional provider-neutral `SessionNamer` capability with an existing-name-preserving request, and routes the provider's normalized effective name to the optional `ConversationLabelRouter`. Session rotation, import, resume, and steering do not trigger naming; `/clear` lets the next new session be named again.
- `/pi name <name>` sets an explicit 1-to-128-code-point single-line name on an existing session, is allowed during an active turn, and forces a private Telegram topic rename even when the user already titled the topic. A failed topic rename is reported as partial success and never rolls the provider name back.
- The Telegram adapter tracks a private topic title as a bounded in-memory tri-state learned from `forum_topic_created` and `forum_topic_edited` service messages. Automatic renames happen only for a known-implicit title; explicit titles are preserved and unknown state fails closed as a silent no-op, so a restart cannot overwrite a user title.
- The documentation, DOX, and compiled agent documentation pass synchronized the root and package contracts, `docs/integrations.md`, `docs/architecture.md`, `docs/cli.md`, `docs/harness-compatibility.md`, and `docs/AGENTS.md` with the implementation. `internal/agentdocs/content.go` already described `/pi name` and automatic first-message naming from the implementation work, and this pass found it accurate.

## Technical reasoning

### One trigger for automatic naming

`dispatchHarnessPrompt` captures the conversation's pre-dispatch session identity on every surface, then calls `nameNewPiSession` after a successful dispatch. Naming runs only when the send was not steered, returned a non-empty session identity, and the captured prior identity was empty, so it fires exactly once for the dispatch that creates a conversation's first Pi session. A rotation, import, or resume starts from an existing identity and is skipped. `/clear` resets the session map, so the next ordinary message creates a session and is named again; the topic adapter still refuses to overwrite an explicit title at that point.

Capturing the prior identity on every surface was required because automatic naming is channel-independent: the TUI, plain CLI, Telegram, and WhatsApp all use it, and only the transport decides whether it has a renameable conversation label. The one-time session-ID notice remains gated by `piControlSurface`, so a blocked surface still never receives a session identity even though the internal lookup now runs there.

### Label derivation

`conversationSessionLabel` builds the label from the accepted user content rather than from the rendered harness prompt. It redacts sensitive configuration commands, drops transport-generated lines (`[Attachment `, disabled, failed, and generated voice-transcription lines), joins the remaining lines, and trims the result. A leading slash command contributes only its argument text. Unicode whitespace runs collapse to one ASCII space, non-whitespace control characters are dropped, and emoji, zero-width joiners, variation selectors, combining marks, punctuation, and casing pass through unchanged. A label of at most 32 Unicode code points is used as is; a longer label keeps the largest whole-grapheme prefix that fits 31 code points and appends one ellipsis, so the result never exceeds 32 code points and never splits a grapheme.

### SessionNamer capability

`internal/harness` defines the optional `SessionNamer` capability and `SessionNameResult{Name, Changed}`. Only the Pi adapter implements it, and `Supervisor.SetSessionName` forwards the request to the active target with the same unsupported boundary as the other session controls. `Pi.SetSessionName` runs under the per-conversation lock, requires an existing stored session whose identity matches `expectedSessionID`, reuses a matching live process even during an active turn or otherwise resumes the exact stored session file, verifies that file is a regular file, and re-reads `get_state` before and after the RPC. With `onlyIfEmpty`, an existing provider name is returned unchanged and no `set_session_name` request is sent. The returned name is the normalized value the provider reports, and `Changed` distinguishes a real rename from a preserved name. Naming never creates or rotates a session, and adapter errors stay bounded and redact session and workspace paths.

### Telegram title tri-state and the rename fence

The Telegram adapter records title state before the ordinary content filter from allow-list-authenticated `forum_topic_created` (`is_name_implicit` marks a still-implicit title) and `forum_topic_edited` messages that carry a name. The state lives in a mutex-guarded, 1024-entry in-memory map keyed by chat and thread with simple eviction, so a long-lived process cannot grow it without bound and a restart resets every topic to unknown.

`RenameConversation` parses the canonical conversation, then rejects base chats, groups, malformed routes, and labels that are empty after trimming, invalid UTF-8, longer than 128 code points, or carry control characters. An implicit-only request is a silent no-op success unless the tracked state is exactly implicit, which preserves explicit user titles and fails closed for unknown state. A permitted request calls `editForumTopic` through the route-aware provider path, which reapplies the current allow-list, verified identity mapping, and group policy before the call and again before its single bounded rate-limit retry. A `TOPIC_NOT_MODIFIED` 400 is treated as success because it proves the requested title is already live; every other provider error propagates. A successful rename marks the topic explicit, so the automatic path does not repeat it.

### Explicit `/pi name` and partial success

`/pi name <name>` validates the name as trimmed, non-empty, valid UTF-8 of at most 128 code points without control characters, requires an existing session, and sends `SetSessionName` with the current session identity and `onlyIfEmpty=false`. The reply and any private-topic rename use the provider's normalized effective name when it reports one. On a private Telegram topic the command forces `RenameConversation` with `onlyIfImplicit=false`, so it can replace a user title. A topic failure is reported in the reply as partial success, and the provider name is never rolled back. On the TUI and on a private base chat the command changes only the session name.

### Documentation synchronization

The DOX pass added the naming contract to the root `AGENTS.md`, `internal/AGENTS.md`, `internal/harness/AGENTS.md`, `internal/app/AGENTS.md`, `internal/channel/AGENTS.md`, and `internal/channel/telegram/AGENTS.md`. User documentation received the same facts in `docs/integrations.md`, `docs/architecture.md`, `docs/cli.md`, and `docs/harness-compatibility.md`, and `docs/AGENTS.md` gained the documentation-consistency contract. The compiled `internal/agentdocs` catalog needed no further edit: the implementation work already updated `content.go` to name `/pi name <name>` and the automatic first-message behavior, and the catalog synchronization test passes.

## Impact assessment

- No configuration key, schema, migration, or store layout changed. Session naming adds an optional interface and metadata-only RPC use; existing session files remain authoritative.
- Harnesses other than Pi keep their current session-control behavior: the supervisor returns `ErrSessionControlsUnsupported` for controls and performs no naming.
- Telegram gains `editForumTopic` calls only for private topics with a known-implicit title, plus forced renames from `/pi name`. Groups, base chats, and explicit titles are never renamed automatically, and a restart loses title tracking instead of guessing.
- The pre-dispatch session lookup now runs on every surface, but it is process-free and internal; disclosure and naming remain gated, and the one-time new-session notice behavior is unchanged.
- Documentation, DOX contracts, and the compiled agent catalog describe the same trigger, label derivation, capability fences, topic tri-state, and `/pi name` behavior.

## Validation steps

- `go test -count=1 ./...` passed on the completed tree: all 28 packages with tests are green plus the three package directories without test files. A first full run under parallel load hit two unrelated flaky failures in unmodified tests, `TestWebhookModeVerifiesSecretAndRoutesUpdate` (`internal/channel/telegram`, webhook stop timeout) and `TestSocketLifecycle` (`internal/localapi`, `TempDir` cleanup race); both passed in isolated `-count=3` re-runs and the complete suite passed on re-run.
- `go test -race -count=1 ./internal/app ./internal/channel/telegram ./internal/harness` passed: `internal/app` 202.570s, `internal/channel/telegram` 1.794s, `internal/harness` 76.271s.
- `go vet ./...` reported no findings.
- `mkdir -p .tmp-bin && go build -o .tmp-bin/spynel ./cmd/spynel` built the binary and `./.tmp-bin/spynel version` printed `spynel dev`.
- `scripts/smoke.sh` passed initialization, configuration validation, and task/goal creation, and printed `DOX coverage valid for 51 tracked directories`.
- `scripts/dev.sh dox` printed `DOX coverage valid for 51 tracked directories`.
- `go test -count=1 ./internal/agentdocs ./internal/app ./internal/channel/telegram ./internal/harness` passed for all four packages.
- Package coverage totals from fresh profiles: `internal/app` 82.8%, `internal/channel` 82.1%, `internal/channel/telegram` 83.7%, `internal/harness` 76.5%. New-function coverage: `nameNewPiSession`, `conversationSessionLabel`, `sessionLabelGeneratedLine`, `collapseSessionLabelWhitespace`, `truncateSessionLabel`, the supervisor and Telegram `RenameConversation`, `trackTopicTitle`, `topicTitleState`, `isTopicNotModified`, `piProcessServesSession`, and the supervisor `SetSessionName` at 100.0%; `piNameCommand` 75.0%, `SetSessionName` in the Pi adapter 80.0%, `readPiState` 71.4%, `setTopicTitle` 88.9%.
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of every changed and added file confirmed one H1 per file, uninterrupted heading progression, no em dash on added lines, no trailing whitespace, and single trailing newlines.
- No live Telegram forum canary and no live Pi naming canary were run. The behavior evidence is the deterministic fixture and unit coverage above plus the local gates.

## Quality gate

Quality gate: PASS. The gate covered the implemented trigger, label derivation, capability fences, Telegram title tracking, `/pi name` behavior, the test evidence, and the documentation and DOX synchronization, and it recorded no blocking findings.

## Security gate

Security gate: PASS. Five non-blocking advisories were reviewed and accepted as hardening notes:

1. Provider-echo name revalidation. The effective name returned by `get_state` is trusted for the local reply and the label route. The Telegram adapter re-validates label bounds before any provider call, but a provider that echoes an out-of-bounds name can still reach the local reply.
2. Residual same-conversation TOCTOU. The expected-session-ID fence catches ordinary rotation between the application lookup and the adapter call, but it cannot prove the provider never interleaves a same-conversation change inside the RPC window.
3. Group-state bookkeeping. Authenticated group topic service messages populate the bounded title map even though the rename path refuses groups. The state is inert and capped, and it is never used for group delivery.
4. Internal session read on blocked surfaces. The surface-independent pre-dispatch lookup calls `SessionInfo` on channels that must never disclose session identity. The lookup is process-free and internal and disclosure stays gated, but the capability call itself is no longer skipped there.
5. Unbounded explicit-control RPC wait. The automatic path wraps naming in an eight-second timeout, while `/pi name` passes the caller context directly. The command is user-initiated, and provider or process failure still surfaces through the ordinary command error path.

## Related operation traces

- [Pi session controls](2026-09-20-pi-session-controls.md) introduced the provider-neutral capability pattern, the `/pi` surface, and the session-identity fences this change extends.
- [Pi retained prompt context](2026-09-21-pi-retained-prompt-context.md) covers the current-prompt guarantee and the pre-dispatch session handling in the same dispatch path.
- [Telegram topic conversations and bounded rich-text replies](2026-09-20-telegram-topic-rich-text.md) owns the canonical topic routes and the delivery boundary that topic renaming follows.

## Release and publication status

No release was requested for this change. It is uncommitted work on top of `13e8985` (`feat(harness): reuse retained Pi conversation context`), and the current published release remains `v1.3.0`. No live Telegram forum canary and no live Pi naming canary were run, and nothing in this record claims installed-user availability.

## References

- [Communication integrations](../../../docs/integrations.md)
- [Architecture](../../../docs/architecture.md)
- [Plain CLI and automation](../../../docs/cli.md)
- [Coding harness compatibility](../../../docs/harness-compatibility.md)
- [Telegram Bot API editForumTopic](https://core.telegram.org/bots/api#editforumtopic)
- [Telegram Bot API ForumTopicCreated](https://core.telegram.org/bots/api#forumtopiccreated)
- [Pi RPC documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md)
- [Pi session controls](2026-09-20-pi-session-controls.md)
- [Pi retained prompt context](2026-09-21-pi-retained-prompt-context.md)
- [Telegram topic conversations and bounded rich-text replies](2026-09-20-telegram-topic-rich-text.md)

## Update 2026-09-21: at-most-once topic rename

### Summary

The tri-state title fence is gone. A first user message that creates a Pi session still derives the same bounded label and still routes the effective name to the Telegram adapter, but the automatic rename is now unconditional: the adapter suppresses only a repeat for a conversation it already renamed in the same process. `/pi name <name>` force-renames even a topic Spynel already renamed, and the stale tri-state DOX bullets were replaced by the at-most-once rule.

### Technical reasoning

The user asked for one simple rule: rename the topic before the user would ordinarily edit it, then let the user own the title afterwards. The tri-state tracking existed to protect an explicitly titled topic, but it also delayed the rename until a `forum_topic_created` service message arrived and turned unknown state into a permanent skip after a restart. Dropping the state removes `forum_topic_created`/`forum_topic_edited` parsing, `is_name_implicit`, the unknown/implicit/explicit enum, and the silent skip of unknown topics, and it makes the first trigger deterministic: a new thread is renamed at its first session creation instead of waiting for transport-reported state, so it is not left as "New chat".

`RenameConversation` still parses the canonical route, rejects base chats, groups, malformed routes, and invalid labels before any provider call, and still routes `editForumTopic` through the authorization-checked provider path with its single bounded rate-limit retry. The only skip in the automatic path is the new bounded in-memory set: a successful rename, including the idempotent `TOPIC_NOT_MODIFIED` 400, marks the conversation, and a failed provider call leaves it unmarked.

### Impact

- The previous property "explicit titles are never overwritten automatically" is deliberately replaced by the user-approved at-most-once rule. The rename now happens before the user would typically edit the title, and a later user edit is not touched automatically again within the same process.
- The application keeps the same two call sites and best-effort semantics: automatic naming routes `force=false` and `/pi name` routes `force=true`. The transport owns the at-most-once fence, and the application never sees the renamed set.
- The `ConversationLabeler` parameter is renamed from `onlyIfImplicit` to `force` across `internal/channel` and its supervisor.
- A restart clears the set, so the first automatic trigger after a restart can rename the topic again even after the user edited it; eviction beyond 1024 conversations can do the same for one conversation. Both are accepted edges, not defects.
- `TOPIC_NOT_MODIFIED` still counts as success and now also marks the conversation, so an idempotent provider response does not cause a repeat within the process.

### Validation

- `go test -count=1 ./...` passed on the completed tree, including the locally replaced Bubble Tea module's nested tests.
- A targeted race run on the changed packages, `./internal/channel ./internal/channel/telegram ./internal/app`, passed.
- `go vet ./...` reported no findings.
- `mkdir -p .tmp-bin && go build -o .tmp-bin/spynel ./cmd/spynel` built the binary.
- `scripts/smoke.sh` passed.
- `scripts/dev.sh dox` printed the DOX coverage result.
- Fresh coverage: `RenameConversation` 100%, `topicRenamed` 100%, `markTopicRenamed` 88.9%, and `internal/app/session_name.go` 100%.
- `git diff --check` reported no whitespace errors.
- Quality gate: PASS.
- Security gate: PASS with two minor non-blocking advisories: the bounded-set comment overstated the automatic no-repeat guarantee, and `isTopicNotModified` matches `TOPIC_NOT_MODIFIED` as a description substring rather than an exact code. The comment wording was corrected in this pass; the substring match is accepted as bounded provider tolerance.

## Update 2026-09-22: v1.4.0 publication

### Summary

The session and topic naming work is now published. Release [v1.4.0](https://github.com/digitalygo/spynel/releases/tag/v1.4.0) was cut from commit `62be722371e82c2cbee09b8acfb7b9ae931a68df`; the original naming change landed as `dc8baeb` (`feat(pi): name new sessions and Telegram topics`) and the at-most-once rename follow-up as `40f557c` (`feat(telegram): rename each new topic at most once`). The earlier "Release and publication status" note is superseded: the work is no longer uncommitted and v1.4.0 is the current published release.

### Technical reasoning

v1.4.0 bundles four changes since v1.3.0: retained-context prompt dedup, the session and topic naming recorded here with `/pi name`, the at-most-once topic rename recorded in the update above, and Pi rate-limit answer recovery. Release packaging was validated first on the local host: `spynel_1.4.0_linux_amd64.tar.gz` with sha256 `cc14f0a3e5b0917ab41fcb36c34ee85e1b647152605ffe4220b20f9cc079230a`, whose extracted binary reports version 1.4.0. Manual validation run 35733023893 then exercised verify and all four native builds with publish skipped, and release run 35734040515 passed verify, all four native builds, and publish. The stable release carries four native archives plus `checksums.txt`. The npm package `@digitalygo/spynel@1.4.0` is `latest`.

### Impact

Installed users now receive automatic first-message naming, the unconditional at-most-once topic rename, and the explicit `/pi name` control, and the work is no longer uncommitted. The live npm-managed home service was updated to 1.4.0 through the official `spynel update` path, and its `/pi` catalog includes `/pi name`, so the published control surface is live in ordinary operation. The known Linuxbrew `npmrc` quirk occurred during that update: the 0444 file produced `EACCES` and was handled with a temporary owner-write plus trap restore; npm's reify normalized the file's whitespace to the same `prefix` value and the 0444 mode was restored afterwards.

### Validation

- Release evidence: tag `v1.4.0` at `62be722371e82c2cbee09b8acfb7b9ae931a68df`; the release carries `darwin_amd64`, `darwin_arm64`, `linux_amd64`, and `linux_arm64` archives plus `checksums.txt`.
- Run evidence: manual validation run 35733023893 passed verify and all four native builds with publish skipped; release run 35734040515 passed verify, all four native builds, and publish.
- Package evidence: the extracted `spynel_1.4.0_linux_amd64.tar.gz` binary reports version 1.4.0; sha256 `cc14f0a3e5b0917ab41fcb36c34ee85e1b647152605ffe4220b20f9cc079230a`.
- Registry evidence: `@digitalygo/spynel@1.4.0` is `latest` with SLSA provenance v1 via Trusted Publishing without tokens; a clean-prefix install reports 1.4.0 after brief registry edge convergence.
- Live service evidence after the official `spynel update`: Telegram and Pi connected and idle, model, reasoning, and service inherited, reviews `never`, updater current at 1.4.0, and the `/pi` catalog includes `/pi name`.
- Non-blocking warnings: Node 20 `actions/upload-artifact` deprecation annotations appeared on the workflow runs and did not affect the builds or published artifacts.

## Update 2026-09-25: name a live Pi session before its first file flush

### Summary of changes

A new Pi session could stay unnamed, leaving its private Telegram topic at "New chat". `SetSessionName` now accepts an expected-ID-matching, live Pi process even when its JSONL session file has not yet appeared; without that live process it still requires a regular stored file before resuming. A portable fixture now reproduces Pi's deferred file creation, and the harness documentation records the distinction.

### Technical reasoning

Spynel triggers first-message naming immediately after Pi accepts the prompt. The installed Pi 0.87.1 reports a session ID and file path first, then creates the JSONL only when it persists the first assistant message. The former unconditional `os.Stat` in `SetSessionName` rejected this valid pre-flush state, logged `session_name_failed`, and prevented the downstream Telegram topic rename. A content-free local log event confirmed this failure class. No title-state tracking, extra prompt, placeholder file, deferred user-facing reply, or retry was introduced. The existing per-conversation lock and expected-session-ID checks still fence the matching live process, and a failed live RPC never falls back to opening another session. The earlier description in this record that naming verifies the stored file even for a live process is superseded for that live pre-flush case only.

### Impact assessment

New first-turn Pi sessions can be named through the existing best-effort metadata path, allowing the unchanged private-topic rename path to run. Persisted resumes still reject missing or non-regular session files. Previously missed session and topic names are not automatically retried; users can explicitly rename an existing eligible Pi session with `/pi name <name>`. No session store, configuration, chat prompt, other harness, or authorization contract changed. The current installed bot remains at v2.0.0 until a separately authorized publication and local update.

### Validation steps

- A synthetic delayed-flush Pi fixture confirmed naming succeeds while the JSONL does not exist, persists its name on the first assistant-message flush, and reuses the same session and process. Separate tests cover missing and non-regular files without a live process, a closed live process, and provider death during naming.
- `go test ./...`, `scripts/dev.sh test` including the nested Bubble Tea module, `go vet ./...`, a Go build, targeted race tests for the harness, application, and Telegram channel, and `git diff --check` passed. `SetSessionName` coverage was 82.5%; its live-process predicate reached 100%.
- `scripts/dev.sh dox` and `scripts/smoke.sh` passed in a detached worktree containing the exact five-file implementation diff. They cannot pass directly in the main worktree because a pre-existing unrelated untracked `.ai-telemetry/` directory is outside DOX coverage; it was not inspected or modified.
- Quality judgment: PASS. Focused security review: PASS. No authenticated provider request or live Telegram rename was attempted, and the installed Pi source inspection is reported as observed behavior rather than a canary.

## Update 2026-09-26: v2.0.1 publication and pending local update

### Summary of changes

The six-file Pi naming fix was committed as `2725b6659d1d8c7d55d587f3f724253617177af0` (`fix(pi): name new sessions before first file flush`), pushed to `main`, and published as the stable [v2.0.1 release](https://github.com/digitalygo/spynel/releases/tag/v2.0.1). The annotated tag object is `45c66e5851fee3ccd4f1d6793261382955db7419` and peels to that exact source commit. The installed local bot remains at v2.0.0; no local update has been attempted.

### Technical reasoning

The [nonpublishing validation run](https://github.com/digitalygo/spynel/actions/runs/36193270388) on that commit succeeded in verification and all four native build jobs, with `publish` skipped. Only then was the tag and GitHub Release created. The [release run](https://github.com/digitalygo/spynel/actions/runs/36194463563) succeeded in verification, four native builds, and the mandatory `publish` job. Its five public assets are the Linux and macOS amd64/arm64 archives plus `checksums.txt`; the published Linux amd64 archive checksum is `cdd5b90fc2b1bb31903d84d971d9f06b5f8addaedca8e23013de47a3541f0ad8`. The downloaded host archive passed its published checksum and executed as `spynel 2.0.1`. npm `@digitalygo/spynel@2.0.1` is available under `latest` with SLSA provenance v1.

### Impact assessment

A read-only local preflight found one ready npm-managed v2.0.0 server, Pi and Telegram connected, zero active turns, zero jobs, no pending outbox, and no live TUI clients. However, Telegram has two authorized numeric senders and Spynel has no message-admission pause during an update. A new message admitted after an idle snapshot but before shutdown could lose its live turn, even though messages still queued at Telegram survive the restart. For that reason, the local update is deferred until the operator confirms both authorized accounts will not send messages through the brief maintenance window. Do not silently interrupt a newly admitted turn. Previously unnamed sessions and topics will not be renamed automatically after deployment; `/pi name <name>` is the explicit recovery path.

### Validation steps

- Clean detached release-candidate checks passed: DOX, smoke, npm tests, release-prepared `npm pack --dry-run`, and host native packaging with extracted archive execution. The main tree retained the development version placeholder and the unrelated private untracked `.ai-telemetry/` directory was not touched or packaged.
- The manual and publishing workflow runs succeeded at the same source commit. The public release has exactly five expected assets, npm `latest` is 2.0.1, and the published host archive checksum and version were verified independently.
- `spynel update --json check` reports installed v2.0.0 and available v2.0.1. No npm replacement, restart, authenticated Pi model request, or live Telegram rename canary has been performed as part of this release task yet.

## Update 2026-09-26: local v2.0.1 deployment

### Summary of changes

The installed npm-managed home service was updated from v2.0.0 to v2.0.1 through one official `spynel update` execution after the operator confirmed that both Telegram authorized accounts would not send messages until readiness was re-confirmed. The same process, PID 2090417, continued under a new generation record, and the CLI, package, and vendor metadata now report 2.0.1. No repository source file changed in this deployment step.

### Technical reasoning

The published release still needed local deployment evidence. A read-only preflight immediately before the update, at 2026-09-26T01:08:53+02:00, found one npm-managed service process, PID 2090417, generation `7646cfd03f85d9b810c8c4ccbecfde0a`, version 2.0.0, ready, with Telegram and Pi connected, no active turn, zero jobs, zero pending outbox entries, and npm `latest` at 2.0.1. The coordinated restart check passed. Spynel has no message-admission pause during an update and the Telegram transport has two authorized numeric senders, so the operator explicitly confirmed that both accounts would refrain from messaging through the brief maintenance window; that confirmation is what made the restart safe.

The update ran as `/home/linuxbrew/.linuxbrew/bin/spynel update` under the same `HOME` as the supervisor, between 01:08:53 and 01:09:00. The known Linuxbrew `npmrc` quirk applied: the file is a regular file owned by `luca` with mode 0444 and sha256 `a0e43e04265c9fc6231f0288288e0088ea033732278f9953d65551dbb8930ccd`. The npmrc was temporarily made owner-writable at 0644, then an `EXIT`/`INT`/`TERM`/`HUP` trap restored its original 0444 mode; its inode and hash stayed unchanged. No direct `npm install`, no `spynel killall`, and no manual restart was used; the official updater owned the whole transaction.

After the update, the same PID 2090417 ran under the new generation `9f7845e5ae7c6c603e733fcb9bb4cf59`, reported ready, and was re-elected primary. Telegram and Pi were connected, jobs were zero, the turn was idle, and `spynel update --json check` reported `Current=Latest 2.0.1` with `Available=false`. The configuration hash and mode and the secret-presence flags were unchanged, the Pi session-map digest was unchanged, the legacy docs metadata digest was unchanged, and the outbox showed four delivered and zero pending entries. The `.spynel` and `.spynel/runtime` directories remained private at 0700. An independent read-only orchestrator recheck at about 01:10 confirmed the externally visible version, health, mode, and registry states and that release run 36194463563 had succeeded.

### Impact assessment

The naming fix is now live on the installed bot. The service kept its PID through an in-place restart, then Telegram and Pi reconnected; the session-map digest remained unchanged while package, CLI, and vendor metadata moved to 2.0.1. Previously unnamed sessions and topics are not renamed retroactively; any private topic still titled "New chat" needs the explicit `/pi name` path. Automatic first-message naming and its Telegram topic rename remain covered by the synthetic fixture and unit evidence recorded above, not by a live canary. No model message and no live Telegram rename canary was sent during this deployment. The repository working tree is unchanged by the deployment, and the unrelated private untracked `.ai-telemetry/` directory was neither inspected nor modified.

### Validation steps

- Pre-update snapshot at 2026-09-26T01:08:53+02:00: one npm-managed service, PID 2090417, generation `7646cfd03f85d9b810c8c4ccbecfde0a`, version 2.0.0, `ready=true`, Telegram and Pi connected, `turn_active=false`, jobs 0, pending outbox 0, coordinated restart check passed, npm `latest` 2.0.1.
- Pre-update filesystem evidence: root and runtime directories private at 0700, configuration file at 0600, npmrc owned by `luca` as a regular file at 0444 with sha256 `a0e43e04265c9fc6231f0288288e0088ea033732278f9953d65551dbb8930ccd`.
- Update execution evidence: one official `/home/linuxbrew/.linuxbrew/bin/spynel update` invocation under the supervisor's `HOME` between 01:08:53 and 01:09:00; temporary 0644 npmrc write fenced by an `EXIT`/`INT`/`TERM`/`HUP` trap; original mode 0444, inode, and hash restored; no direct npm install, killall, or manual restart.
- Post-update evidence: CLI, npm package, and vendor metadata report 2.0.1; PID 2090417 remains under the new generation `9f7845e5ae7c6c603e733fcb9bb4cf59`, `ready=true`, primary re-elected, Telegram and Pi connected, jobs 0, turn idle; `spynel update --json check` reports `Current=Latest 2.0.1` and `Available=false`.
- State-preservation evidence: configuration hash and mode unchanged, secret-presence flags unchanged, Pi session-map digest unchanged, legacy docs metadata digest unchanged, outbox 4 delivered and 0 pending, `.spynel` and `.spynel/runtime` mode 0700.
- Independent read-only recheck at about 01:10 confirmed the externally visible version, health, mode, and registry states and the successful release run 36194463563.
- No model message and no live Telegram rename canary was sent. Existing "New chat" topics still require the explicit `/pi name` path, and the new-session behavior remains covered by the fixture, not by live observation.
