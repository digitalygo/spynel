---
status: completed
created_at: 2026-09-22
files_edited:
  - docs/architecture.md
  - docs/harness-compatibility.md
  - docs/troubleshooting.md
  - internal/harness/AGENTS.md
  - internal/harness/pi.go
  - internal/harness/pi_test.go
  - internal/harness/process_fixture_test.go
  - substrate/traces/operations/2026-09-22-pi-rate-limit-answer-recovery.md
rationale: Record the completed Pi turn-settlement fix that makes a successful in-run retry win over a transient provider error, plus the documentation and DOX pass describing the corrected delivery behavior.
supporting_docs:
  - ../../../docs/architecture.md
  - ../../../docs/harness-compatibility.md
  - ../../../docs/troubleshooting.md
  - 2026-09-19-local-pi-configuration.md
  - 2026-09-20-pi-session-controls.md
  - 2026-09-21-pi-retained-prompt-context.md
  - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md
  - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/settings.md
---

# Pi rate-limit answer recovery

## Summary of changes

A settled Pi turn now prefers its successful assistant messages over any transient provider error recorded during the same run. Before the fix, `piTurn.errorText` was sticky for the whole turn, so when Pi retried a rate-limited attempt inside one settled run and the retry produced the answer, Spynel emitted the stale `EventError` and silently dropped the successful text. The final terminal now selects success whenever at least one assistant message succeeded.

The implementation changes live in three Go files, still unstaged: `internal/harness/pi.go` (extracted `reconcileMessage`, new `failMessage`, success-tracking `finishMessage`, and success-first `finishTurn`), `internal/harness/pi_test.go`, and `internal/harness/process_fixture_test.go` (new fixture error variant and the `pi-retry-success`, `pi-all-failed`, `pi-success-then-failed`, and `pi-no-message` modes). This record also captures the documentation and DOX pass that makes the fixture-exercised behavior part of the binding contracts and the user-facing docs.

Two live private-Telegram turns motivated the fix. In both, the run produced meaningful assistant text and then immediately emitted the upstream OpenRouter rate-limit terminal, and the chat received only the error. The transcripts are paraphrased below, with identifiers removed.

## Technical reasoning

### Root cause: sticky per-turn error state

`piTurn` tracked assistant output with one `text` builder plus a single `errorText` field. `finishMessage` was called for every assistant `message_end`, including errored ones, and `finishTurn` treated any non-empty `errorText` as fatal regardless of what else the run had produced. Pi retries transient failures inside the same run, so the failed attempt's error stayed set while the retry's successful message arrived; `finishTurn` then emitted only the error. The streamed answer was visible in the TUI and in the app's streamed-history record, but the terminal delivered to Telegram was the error, and Telegram delivers only the last terminal response for a remote message. That mismatch is exactly what the two live transcripts show.

### Pi retries inside one settled run

Pi handles transient provider errors in `AgentSession._prepareRetry`: it checks the user's retry settings, increments the attempt counter, emits `auto_retry_start` with attempt, maximum attempts, and delay, removes the errored assistant message from agent state (keeping it only in the session file for history), waits `baseDelayMs * 2^(attempt-1)`, and continues the same run. The run stays unsettled throughout; `agent_settled` is emitted exactly once, after the loop in `_runAgentPrompt` finishes. Defaults come from Pi's own settings (`retry.enabled`, `retry.maxRetries` default 3, `retry.baseDelayMs` default 2000). Spynel passes only session, model, thinking, and sandbox arguments to `pi --mode rpc`, so the user's retry settings are inherited and never overridden.

### Success-first settlement

The fix splits per-message handling by outcome:

- `finishMessage` appends the reconciled message text to `successfulMessages`, sets `lastMessage` to the authoritative text (falling back to the reconciled text), and clears `errorText`.
- `failMessage` reconciles and streams the failed attempt's missing suffix for live display, but records no success, so the failed text never joins the settled result.
- `finishTurn` replaces the accumulated stream text with the newline-joined `successfulMessages` whenever at least one message succeeded and reports `FinalText` as the last successful message. It emits `EventError` only when the run settled with no successful assistant message. Process-failure behavior (`fail`), steering, statuses, session naming, compaction, and disclosure are untouched.

This matches Pi's own model: an errored assistant message is discarded from agent state and retried, so it is not part of the answer the run produced. A later failure after a success leaves `successfulMessages` intact, so it can never swallow the answer.

### Nonterminal retry status and delivery contracts

`auto_retry_start` and `summarization_retry_scheduled` remain nonterminal status events (text `Pi is retrying`, execution state `reconnecting`), so the TUI keeps showing the live retry. Remote transports are unchanged: Telegram and WhatsApp deliver only the last non-continuing final or terminal error, so a transient error surfaces in chat only when the run ultimately fails, and a successful retry makes the answer the single terminal.

### Why no later retrigger

When a run truly settles with an error, the message is covered by the app's terminal correlation. The orchestrator's unresponded-message recovery scans for admitted messages that have no terminal coverage; a terminal error is coverage, so the message is not auto-retried later. This is an intentional contract, not a gap: longer rate-limit windows are handled by the user's own Pi retry settings, and the recovery scanner is not a notification or redelivery mechanism.

## Impact assessment

- Successful retries now deliver the answer through every channel: TUI final, plain CLI default output, and the last-terminal delivery for Telegram and WhatsApp.
- Durable history keeps the settled final text only. The failed attempt's streamed text is not part of the final message; the app's existing streamed-record behavior on error is unchanged.
- Exactly one terminal event is still emitted per settled turn, so existing terminal correlation, emitter accounting, and job archiving are unaffected.
- A truly failed run still reports the provider error as its terminal, and that terminal still covers the message for unresponded-message recovery.
- No configuration key, schema, store layout, harness lifecycle, session naming, compaction, or disclosure behavior changed.
- The docs and DOX contracts now state the success-first settlement rule: `internal/harness/AGENTS.md`, `docs/architecture.md`, `docs/harness-compatibility.md`, and `docs/troubleshooting.md`.

## Validation steps

Implementation evidence recorded by the implementation work on this exact tree (baseline clean `HEAD` at `40f557c`):

- `scripts/dev.sh test` (full Go suite plus nested Bubble Tea module tests) passed, exit 0.
- `go test ./... -count=1`: run 1 failed transiently with the failing test unidentified because output was suppressed; runs 2 and 3 passed cleanly, the last with captured output. Two pre-existing flakes, `TestWebhookModeVerifiesSecretAndRoutesUpdate` and `TestSocketLifecycle`, are documented elsewhere in the repo and are untouched by this delta; both passed isolated `-count=3` re-runs, and the changed `internal/harness` package passed consistently.
- `go test -race -count=1 ./internal/harness` passed, exit 0 (81s).
- `go vet ./...` was clean, exit 0.
- `mkdir -p .tmp-bin && go build -o .tmp-bin/spynel ./cmd/spynel` succeeded and the artifact was removed.
- `scripts/smoke.sh` passed.
- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- `git diff --check` was clean.
- Coverage of the executable delta: `reconcileMessage` 100%, `finishMessage` 100%, `failMessage` 100%, `finishTurn` 85.7%, all at or above the 80 percent gate.
- Test-to-behavior mapping: `TestPiSettledTurnPrefersSuccessfulAssistantMessages` covers retry success and success-then-failure; `TestPiSettledTurnWithoutSuccessfulMessagesReportsTerminalError` covers the error-only path; `TestPiFailedMessageReconcilesLiveTextWithoutRecordingSuccess` covers live reconciliation of a failed attempt without a recorded success; `TestPiReconcileMessageMergesAuthoritativeText` replaced `TestPiAuthoritativeMessageAddsOnlyMissingStreamSuffix` with a strict superset table; the fixture modes cover `pi-retry-success`, `pi-all-failed`, `pi-success-then-failed`, and `pi-no-message`.
- Quality gate verdict: PASS, with the test replacement (`TestPiAuthoritativeMessageAddsOnlyMissingStreamSuffix` superseded by `TestPiReconcileMessageMergesAuthoritativeText`) reviewed as a justified subsumption that loses no coverage.
- Security review: deliberately not run. The delta changes turn and delivery semantics only; it adds no API, authentication, secret handling, or sensitive-data path.

Documentation pass checks run for this record:

- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- `go test ./internal/harness ./internal/agentdocs` passed.
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of the new trace and every added line across the changed files confirmed exactly one H1 per file, uninterrupted heading progression, blank lines around blocks, no trailing whitespace, a single trailing newline, and no em dash in added content.

## Motivating live transcripts

Both cases come from the operator's private Telegram history and are paraphrased, with identifiers removed. They share one signature: the run persisted meaningful assistant text and then emitted the rate-limit terminal less than a millisecond later.

- 2026-09-20: the operator confirmed a YouTrack issue update with completion state and effort. The run streamed a take-charge confirmation and then emitted `openai/gpt-5.6-sol is temporarily rate-limited upstream` as the terminal. The chat showed only the error; the operator's next message asked why the update had not happened, and the notification agent later confirmed the issue had not been modified.
- 2026-09-21: the operator asked whether an earlier promised local-repository catalog update had completed and which directory the assistant was in. The run streamed the completed-catalog answer including the working directory and then emitted the same rate-limit terminal. The chat showed only the error until the operator asked the question again in a later turn.

In both cases the assistant text was present at settle time, which is consistent with a successful in-run retry whose answer was discarded by the sticky `errorText`.

## Live canary and release status

No authenticated live-provider rate-limit or retry canary was run for this fix, and none is claimed; the evidence is deterministic fixtures plus the two historical transcripts above. No release or publication was requested or prepared by this work, and no commit or staged change exists for it: the three Go files and the documentation/DOX files are left in the working tree, unstaged.

## References

- [Architecture](../../../docs/architecture.md)
- [Coding harness compatibility](../../../docs/harness-compatibility.md)
- [Troubleshooting](../../../docs/troubleshooting.md)
- [Local Pi configuration](2026-09-19-local-pi-configuration.md)
- [Pi session controls](2026-09-20-pi-session-controls.md)
- [Pi retained prompt context](2026-09-21-pi-retained-prompt-context.md)
- [Pi RPC documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md)
- [Pi settings documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/settings.md)
