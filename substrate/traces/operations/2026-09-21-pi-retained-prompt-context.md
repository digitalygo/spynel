---
status: completed
created_at: 2026-09-21
updated_at: 2026-09-22
files_edited:
  - AGENTS.md
  - docs/AGENTS.md
  - docs/architecture.md
  - docs/cli.md
  - docs/configuration-live-matrix.md
  - docs/configuration.md
  - docs/harness-compatibility.md
  - docs/integrations.md
  - internal/AGENTS.md
  - internal/agentdocs/content.go
  - internal/app/AGENTS.md
  - internal/app/prompt_context_test.go
  - internal/app/recovery.go
  - internal/app/recovery_test.go
  - internal/app/service.go
  - internal/channel/tui/visual_capture_test.go
  - internal/config/settings.go
  - internal/config/settings_test.go
  - internal/harness/AGENTS.md
  - internal/harness/context_test.go
  - internal/harness/harness.go
  - internal/harness/pi.go
  - internal/harness/supervisor.go
  - internal/history/AGENTS.md
  - internal/history/store.go
  - internal/history/store_test.go
rationale: Stop re-sending bounded Spynel history to Pi provider sessions that already retain the conversation, while guaranteeing that the current user message, the durable history path, and framework directives reach every chat prompt in every mode.
supporting_docs:
  - ../../../docs/architecture.md
  - ../../../docs/cli.md
  - ../../../docs/configuration.md
  - ../../../docs/harness-compatibility.md
  - ../../../docs/integrations.md
  - 2026-09-19-local-pi-configuration.md
  - 2026-09-20-pi-session-controls.md
  - 2026-09-20-telegram-topic-rich-text.md
  - https://github.com/digitalygo/spynel/releases/tag/v1.4.0
---

# Pi retained prompt context

## Summary of changes

Chat prompts no longer repeat bounded Spynel history to a Pi provider session that already retains the conversation. The application asks one optional provider-neutral capability, `ConversationContextProvider`, whether an ordinary send for a conversation key would reuse a retained session. When the answer is true the prompt carries the current user message plus the durable history path; when it is false, which covers every unsupported, fresh, mismatched, or uncertain state, the bounded seed is delivered exactly as before.

The current user message is now guaranteed in every chat prompt. `workspace.history_max_messages: 0` and `workspace.history_char_limit: 0` disable prior-history seeding only; the current entry is still rendered, and a positive character limit bounds it with the existing fail-closed reply rules. A new `history.Store.PromptContext` method owns this rendering, the Pi adapter implements the capability, the supervisor forwards it, and `/task`, `/goal`, and recovery turns keep their existing appended directives. Conversation recovery now renders the newest reserved user entry as current context instead of the synthetic recovery control text.

The packaged `internal/workspace/templates/chat.md` is intentionally unchanged, and existing or customized workspace prompts are untouched. The compiled `internal/agentdocs` session topic already described the seed-only behavior, and this pass synchronized the repository documentation, DOX contracts, configuration descriptions, and the configuration live matrix with the new semantics.

## Technical reasoning

### Pi already retains the conversation

Pi runs one long-lived RPC process per conversation and persists its session file, so the provider already holds the full conversation and manages its own context and compaction. Re-sending the newest bounded Spynel window on every turn duplicated context Pi already had, spent tokens on a second copy of the same messages, and risked two different views of one conversation. The prompt still needs the complete history path so the agent can inspect older material on demand.

### Optional capability instead of provider checks in the application

Application and orchestration code stays harness-neutral, so the decision cannot branch on Pi types or read Pi session state inside `internal/app`. `internal/harness` defines the optional `ConversationContextProvider` interface with one method, `ProvidesConversationContext(key string) bool`. Only Pi implements it. The supervisor snapshots the active target under its mutex, asserts the interface outside that lock, and reports false when the supervisor is closed, the target is unavailable, or the harness does not implement the capability. `ThreadID` and `SessionInspector` were rejected as substitutes because a thread identity or a durable session map does not prove that a provider process can reuse the retained conversation.

### What counts as retained

`Pi.ProvidesConversationContext` runs under the same per-key lock and computes the same session policy as `ensureProcess`, so the capability answer and the dispatch path cannot drift. It reports true in three cases: a live active turn whose process session has an ID and a path; a live idle process whose captured policy matches the current model, effort, and sandbox; or a persisted policy-matching session whose file exists as a regular file. An active turn counts as retained even when a configuration commit changed the policy, because the provider memory still holds the conversation for that turn; after the turn releases, the stale policy reports false and the next dispatch keeps the bounded seed. A closed or not-started adapter, a missing session, a missing or non-regular session file, and any policy mismatch report false. The persisted-session branch validates both the policy and the file because a stale or deleted session would start fresh.

### False is always safe

The interface returns a boolean and never an error. Every failure, uncertainty, or unsupported state reports false, which selects the bounded seed path that predates this change, so a false answer can only add context to the prompt, never remove it. That property is what keeps a provider failure from silently changing conversation semantics.

### Current user message guarantee and limit semantics

`history.PromptContext` renders one bounded window that always carries the current user entry and the full history path. Prior-history seeding is requested only when the capability reports false; when either limit is non-positive the window degrades to the current entry alone. A positive character limit still bounds the current entry: content is tail-truncated behind a leading ellipsis, and an entry whose complete labeled reply identity cannot fit is omitted entirely, matching the existing fail-closed reply rule. In seeded mode the current entry is pinned by source identity; if concurrent appends push it outside the bounded tail, the result degrades to the current-only form instead of scanning unbounded history or substituting another message. The application builds the current entry through the same `userHistoryEntry` helper that admission persists, so the delivered message content, timestamp, sender, reply reference, redaction, and source identity cannot drift from durable history.

### Directives and recovery

`/task` and `/goal` continue to append their saved creation directives after the ordinary communication prompt. Recovery calls the same prompt builder with the newest reserved user entry as the current entry, so the synthetic `recover stalled conversation` control message never appears as user context while the bounded stalled-message block and the relevance guidance remain appended. `newestReservedEntry` orders by local acceptance time and breaks ties by receive time.

### Accepted race

A narrow build-to-dispatch race remains accepted. The capability is queried while the prompt is constructed; if a session rotation or policy change lands between construction and provider dispatch, that turn may start a fresh session with only the current message and the framework directives. The full history path is still present, so the agent can recover older context by inspection, and a false answer is always safe. Closing the race would require holding the provider session lock across dispatch, which the design avoids.

## Impact assessment

- Pi conversations stop re-sending bounded history once a reusable session exists. Other harnesses, fresh sessions, and every uncertain state keep the existing seed, so no other adapter changes behavior.
- The current user message can no longer be dropped by zero history limits, which removes the earlier behavior where `0` produced an empty prompt window. A positive character limit still bounds it.
- Configuration descriptions, the live matrix, integration, architecture, CLI, and harness-compatibility documentation now state the seed-only scope and the current-message guarantee. `docs/AGENTS.md` names prompt-history consistency as a documentation contract, and the root and package DOX contracts carry the capability, false-is-safe, and state-condition rules.
- `internal/agentdocs/content.go` was updated during implementation to say that bounded history is seeded only for provider sessions that do not already retain the conversation. This documentation pass found that text consistent with the implementation and made no further catalog change.
- The packaged chat template keeps its existing wording, which remains accurate for a one-entry window. Existing and customized workspace `chat.md` files are not rewritten.
- No configuration key, schema, migration, session layout, or state representation changed.
- The work is uncommitted on top of `90eb2bc` and is not part of a release.

## Validation steps

- `go test ./internal/agentdocs ./internal/app ./internal/history ./internal/harness` passed at this revision.
- `go test ./...` passed across all 28 packages with tests and 3 packages without test files; `go test -race ./internal/history ./internal/harness ./internal/app` passed (`internal/harness` 65.4s, `internal/app` 197.6s).
- `go vet ./...` and `mkdir -p .tmp-bin && go build -o .tmp-bin/spynel ./cmd/spynel` produced no errors.
- `scripts/dev.sh test` passed the full suite, the replaced Bubble Tea module tests, and vet. `scripts/smoke.sh` passed the initialization, configuration, task/goal, and command smoke checks. `scripts/dev.sh dox` reported `DOX coverage valid for 51 tracked directories`.
- Package test coverage of the new functions: `internal/history` `PromptContext` 91.3% and `renderBoundedEntry` 95.5%; `internal/harness` Pi `ProvidesConversationContext` 96.7% and supervisor `ProvidesConversationContext` 100.0%; `internal/app` `chatPromptWithCurrent` 93.8%, `chatPrompt` 100.0%, `userHistoryEntry` 100.0%, and `newestReservedEntry` 100.0%.
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of every changed file and this record confirmed one H1, uninterrupted heading progression, no trailing whitespace, single trailing newlines, and no em dashes on added lines. Pre-existing em dashes remain on untouched lines in `docs/architecture.md`, `docs/cli.md`, `docs/configuration.md`, `docs/integrations.md`, and `internal/AGENTS.md`.
- The new behavior is covered by `internal/history/store_test.go` (current-entry guarantee, seeded-window pinning, zero limits, rune-safe tail bounds, reply-identity omission, concurrent-append fallback, render branches), `internal/harness/context_test.go` (Pi live, idle, persisted, policy, file, closed, and started state plus supervisor forwarding and unsupported harnesses), `internal/app/prompt_context_test.go` (retained and seeded chat prompts, creation directives, recovery current entry, zero limits, custom templates), `internal/app/recovery_test.go` (`newestReservedEntry` selection), and `internal/config/settings_test.go` (setting descriptions).

## Related operation traces

- [Pi session controls](2026-09-20-pi-session-controls.md) introduced the optional provider-neutral capability pattern and the `/pi` control surface that this change extends.
- [Telegram topic conversations and bounded rich-text replies](2026-09-20-telegram-topic-rich-text.md) owns the canonical topic conversations that carry independent Pi sessions.
- [Local Pi configuration](2026-09-19-local-pi-configuration.md) records the Pi resource and session behavior this change assumes.

## Release and publication status

No release was requested for this change. It is uncommitted work on top of `90eb2bc` and is not part of any published release; the current published release remains `v1.3.0`. Nothing in this record claims that the changed prompt behavior is available to installed users.

## References

- [Architecture](../../../docs/architecture.md)
- [Configuration](../../../docs/configuration.md)
- [Communication integrations](../../../docs/integrations.md)
- [Coding harness compatibility](../../../docs/harness-compatibility.md)
- [Plain CLI and automation](../../../docs/cli.md)
- [Pi RPC documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md)

## Update 2026-09-22: v1.4.0 publication

### Summary

The retained-context prompt dedup is now published. Release [v1.4.0](https://github.com/digitalygo/spynel/releases/tag/v1.4.0) was cut from commit `62be722371e82c2cbee09b8acfb7b9ae931a68df`, where this change landed as `13e8985` (`feat(harness): reuse retained Pi conversation context`). The earlier "Release and publication status" note describing the work as uncommitted on top of `90eb2bc` is superseded: v1.4.0 is the current published release and the retained-context behavior is available to installed users.

### Technical reasoning

v1.4.0 bundles four changes since v1.3.0: the retained-context prompt dedup recorded here, Pi session and Telegram topic naming with `/pi name`, the at-most-once topic rename, and Pi rate-limit answer recovery. Release packaging was validated first on the local host: `spynel_1.4.0_linux_amd64.tar.gz` with sha256 `cc14f0a3e5b0917ab41fcb36c34ee85e1b647152605ffe4220b20f9cc079230a`, whose extracted binary reports version 1.4.0. Manual validation run 35733023893 then exercised verify and all four native builds with publish skipped, and release run 35734040515 passed verify, all four native builds, and publish. The stable release carries four native archives plus `checksums.txt`. The npm package `@digitalygo/spynel@1.4.0` is `latest`.

### Impact

Installed users now receive the seed-only prompt semantics, the current-message guarantee, and the capability fence described above, and the work is no longer uncommitted. The live npm-managed home service was updated to 1.4.0 through the official `spynel update` path, so the retained-context path runs in ordinary operation there. The known Linuxbrew `npmrc` quirk occurred during that update: the 0444 file produced `EACCES` and was handled with a temporary owner-write plus trap restore; npm's reify normalized the file's whitespace to the same `prefix` value and the 0444 mode was restored afterwards.

### Validation

- Release evidence: tag `v1.4.0` at `62be722371e82c2cbee09b8acfb7b9ae931a68df`; the release carries `darwin_amd64`, `darwin_arm64`, `linux_amd64`, and `linux_arm64` archives plus `checksums.txt`.
- Run evidence: manual validation run 35733023893 passed verify and all four native builds with publish skipped; release run 35734040515 passed verify, all four native builds, and publish.
- Package evidence: the extracted `spynel_1.4.0_linux_amd64.tar.gz` binary reports version 1.4.0; sha256 `cc14f0a3e5b0917ab41fcb36c34ee85e1b647152605ffe4220b20f9cc079230a`.
- Registry evidence: `@digitalygo/spynel@1.4.0` is `latest` with SLSA provenance v1 via Trusted Publishing without tokens; a clean-prefix install reports 1.4.0 after brief registry edge convergence.
- Live service evidence after the official `spynel update`: Telegram and Pi connected and idle, model, reasoning, and service inherited, reviews `never`, updater current at 1.4.0, and the `/pi` catalog includes `/pi name`.
- Non-blocking warnings: Node 20 `actions/upload-artifact` deprecation annotations appeared on the workflow runs and did not affect the builds or published artifacts.
