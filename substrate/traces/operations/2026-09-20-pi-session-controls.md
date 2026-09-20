---
status: completed
created_at: 2026-09-20
updated_at: 2026-09-20
files_edited:
  - AGENTS.md
  - docs/AGENTS.md
  - docs/cli.md
  - docs/harness-compatibility.md
  - docs/integrations.md
  - internal/AGENTS.md
  - internal/app/AGENTS.md
  - internal/agentdocs/agentdocs.go
  - internal/agentdocs/content.go
  - internal/app/pi_control.go
  - internal/app/pi_control_test.go
  - internal/app/service.go
  - internal/harness/AGENTS.md
  - internal/harness/harness.go
  - internal/harness/pi.go
  - internal/harness/pi_sessions.go
  - internal/harness/pi_sessions_test.go
  - internal/harness/pi_test.go
  - internal/harness/process_fixture_test.go
  - internal/harness/supervisor.go
  - internal/harness/supervisor_test.go
rationale: Record the completed Pi session-control implementation and the synchronized documentation, DOX, and compiled agent documentation pass, with the v1.3.0 release still pending its quality, security, and release gates.
supporting_docs:
  - ../../../docs/cli.md
  - ../../../docs/harness-compatibility.md
  - ../../../docs/integrations.md
  - 2026-09-19-local-pi-configuration.md
  - 2026-09-20-telegram-topic-rich-text.md
  - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md
  - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/sessions.md
  - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/session-format.md
  - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/compaction.md
  - https://github.com/digitalygo/spynel/releases/tag/v1.3.0
---

# Pi session controls

## Summary of changes

Spynel gained a harness-specific Pi control surface. The canonical commands are `/pi session`, `/pi compact [instructions]`, and `/pi import <full-session-id>`, and all three are available only in the local TUI and canonical private Telegram conversations, including private topics. Groups, forum topics, the plain CLI, WhatsApp, malformed routes, and other channels refuse before any session capability call and receive no session ID or path.

The application now talks to optional provider-neutral capability interfaces instead of Pi types. `internal/harness` defines `SessionInspector`, `SessionCompactor`, and `SessionImporter`, plus `SessionInfo`, `CompactResult`, `SessionCompactMaxInstructions` (4096), `ControlErrorMaxRunes` (400), `SafeControlErrorText`, and `ErrSessionControlsUnsupported`. Only the Pi adapter implements the three interfaces. `Supervisor` forwards inspection, compaction, and import, and returns the explicit unsupported error for every other harness.

`/pi session` returns the full current session identity and path plus a shell-safe `pi --fork <absolute-session-file>` command. Fork is deliberate: Pi JSONL session files have no cross-process lock, so direct Pi and Spynel must never open the same session file concurrently. `/pi compact` calls Pi RPC `compact` on an existing idle, configuration-matching session, accepts up to 4096 Unicode code points of custom instructions, and reports bounded `tokensBefore` and `estimatedTokensAfter` counts. It never creates or replaces a session. `/pi import` accepts only a full canonical lowercase UUID, performs bounded read-only discovery over the effective Pi session stores, validates a supported regular JSONL header with the exact ID and a matching working directory, then forks the validated source into `.spynel/runtime/pi-sessions`. The source file stays unchanged and the fork receives a new ID.

The first successful non-continuing final response after an ordinary TUI or private-Telegram prompt that creates or rotates a session gets a prepended full-ID notice and `/pi session` hint. The notice is applied downstream of extension hooks, media processing, durable history, and job archival, so it never enters durable or model-visible history, and it never appears on errors, resumed or imported sessions, groups, or other channels. Telegram keeps its existing last-final-only delivery semantics.

The implementation landed in `510897d` (`feat(pi): add session controls and safe imports`) and hardened in `ac94014` (`fix(pi): harden session controls`). The documentation, DOX, and compiled agent documentation pass described here is uncommitted work on top of `ac94014` and is left unstaged.

## Technical reasoning

### Optional capabilities instead of Pi types in the application

The application layer must stay harness-neutral, so the command handlers assert the optional `SessionInspector`, `SessionCompactor`, and `SessionImporter` interfaces rather than importing Pi behavior. A harness without a capability receives one bounded unsupported reply. The supervisor exposes the same three methods and forwards them to the active adapter. `SessionInfo` inspection is process-free: it reads the durable session map and returns `ok=false` when a conversation has no session yet, which lets `/pi session` explain that the first ordinary prompt creates one. Inspection also works while the provider is unavailable, because constructing an adapter for the durable map does not start a process.

### Surface restriction and disclosure

Private session identities and control operations are restricted to surfaces where the operator already owns the local terminal or an allow-listed private chat. `piControlSurface` accepts `tui` and Telegram conversations whose canonical route parses and is not a group. That admits `TG-<user-id>` and `TG-<user-id>-topic-<thread-id>` while rejecting `TG-group-...`, group topics, malformed route spellings, the plain CLI, WhatsApp, and unknown channels before any capability call runs. Restricted replies are the same fixed guidance and never contain a session ID or filesystem path.

### Fork instead of attach

Pi's JSONL session format provides no cross-process lock, so opening one session file simultaneously from a direct Pi process and from Spynel is unsafe. The control surface therefore never hands out an attach command. It hands out `pi --fork <absolute-session-file>`, which starts a distinct direct session from the same history, and `/pi import` performs the mirror operation by forking a validated direct session into Spynel's own `pi-sessions` directory. The source remains the property of the direct installation.

### Read-only import discovery

`resolveExternalPiSession` accepts only the exact canonical lowercase UUID grammar. It derives bounded lookup roots from the effective Pi environment and settings: `PI_CODING_AGENT_SESSION_DIR`, a project `.pi/settings.json` `sessionDir` (or the global agent settings value when no project value exists), and finally `<agent-dir>/sessions`. The walk never follows symlinks and enforces explicit depth, directory, directory-entry, candidate-file, header-byte, and source-byte budgets. A candidate must be a regular JSONL file whose first line is a supported Pi session header with `type: session`, the exact requested ID, a supported version, and a non-empty `cwd`. The `cwd` must resolve to the Spynel harness working directory, so foreign-workspace sessions fail closed. Zero matches, multiple matches, unreadable roots, oversized candidates, and budget overruns fail closed.

### Import validation, rollback, and orphan policy

Import starts Pi with `--mode rpc --session-dir <workspace store> --fork <validated-source>` plus the ordinary model, thinking, and sandbox arguments. It validates through `get_state` that the fork has a distinct non-empty ID, a distinct session file inside the Spynel session directory, and a path the pre-import directory snapshot proves did not exist before. The source file's identity, size, and modification time must match across the fork window; a change fails the import closed with instructions to close direct Pi and retry. Persistence failure rolls back the session map and the reported fork file. Cleanup removes only a regular file inside the Spynel session directory that is neither the validated source nor a pre-existing sibling, which prevents a hostile or confused provider from steering deletion at another conversation's session. When Pi creates a fork file but exits before `get_state` reports its path, the adapter has no path to remove; that file can remain as an inert unreferenced orphan because scanning the shared directory to guess a path could delete another session. The session map is not updated in that case, so the orphan is never resumed or referenced.

### Manual compaction

`CompactSession` validates the instruction string as UTF-8 and against the shared 4096-rune bound, then takes the per-conversation lock. It requires an existing stored session, a regular session file, and a configuration-matching policy. It reuses an idle live process when one matches, or resumes the exact persisted file. An active turn, a missing session, a missing file, and a policy mismatch refuse without creating or replacing a session. The RPC response must carry an integer `tokensBefore`; `estimatedTokensAfter` is optional and reported only when known. During an active agent turn, Pi's `compaction_start` and `compaction_end` events surface as provider-neutral status events only, and never as terminal results.

### New-session notice

`markPiSessionDisclosure` records that a successful dispatch returned a session identity different from the pre-dispatch identity, and `takePiSessionNotice` claims that marker on the next non-continuing final for the conversation. Keeping the marker across a steered release means a session created by a turn whose response was superseded still receives exactly one disclosure. The final check in `wrapEmit` runs after extension hooks, outbound media parsing, durable history append, and job archival, and it prepends the notice to both `Text` and `FinalText`. Durable history, hook payloads, the job archive, and later model context therefore never see it. Errors skip the notice because only `EventFinal` is decorated, and resumed or imported sessions are already announced or pre-existing, so they are not re-announced.

## Impact assessment

- No configuration key, schema, migration, or store layout changed. Pi conversation sessions continue to live under `.spynel/runtime/pi-sessions`, and `/clear` remains the reset path before importing a different session.
- Pi global extensions, skills, prompt templates, themes, context files, model/provider settings, project trust, read-only tool policy, and existing session-policy rotation remain unchanged.
- Every other harness reports the explicit unsupported guidance; no adapter fabricates a session identity.
- Telegram delivery keeps its last-non-continuing-final behavior. The notice is prepended before Telegram HTML chunking, so it survives tail truncation on long replies while durable history stays complete.
- Control failures become bounded, sanitized replies that redact the workspace root, state path, and session path and strip control characters. Full session IDs appear only in explicit `/pi session` or import/notice replies on allowed surfaces.
- Documentation, DOX contracts, and the compiled `internal/agentdocs` catalog describe the same commands, surface restrictions, bounds, fork rationale, and notice placement as the implementation.
- The intended release target is `v1.3.0`. Publication is pending the standard quality, security, and release gates and is not claimed here.

## Validation steps

Implementation evidence recorded by the committed work:

- `internal/app/pi_control_test.go` covers catalog and usage replies, restricted-surface refusal without capability calls, shell-safe fork command quoting, no-session and unsupported replies, bounded compaction counts and instruction limits, import success and refusal, sanitized adapter errors, notice placement on TUI and private Telegram, notice exclusion on groups, CLI, WhatsApp, errors, resumed, and imported sessions, steer-release notice survival, and Telegram rich chunking with no history leakage.
- `internal/harness/pi_sessions_test.go` covers process-free inspection, compaction token mapping and rejections, lazy resume, policy mismatch, active-turn refusal, import discovery validation, unsafe source rejection, changing-source fail-closed behavior, rollback, pre-existing sibling preservation, orphan handling, directory and file budgets, effective environment resolution, and error redaction.
- `internal/harness/supervisor_test.go` covers capability forwarding, unsupported reporting for other harnesses, and persisted-map inspection while unavailable. `internal/harness/pi_test.go` and `internal/harness/process_fixture_test.go` cover Pi-only compaction status events and the new synthetic fixture modes.
- Synthetic fixtures only. No live real-provider import or compaction canary was run, and none is claimed.

Documentation pass checks run for this record:

- `scripts/dev.sh dox` reported `DOX coverage valid for 51 tracked directories`.
- `go test ./internal/agentdocs ./internal/app` passed, including the slash-catalog synchronization test that proves every `DocumentedSlashCommands` entry exists in the canonical catalog. `internal/agentdocs/content.go` and `internal/agentdocs/agentdocs.go` already named `/pi` and its surface restriction from the implementation commits, so no synchronization edit was needed.
- `go test ./internal/harness` passed, re-running the Pi session-control, Pi lifecycle, and supervisor capability tests at this revision.
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of the new trace and every added line across the changed files confirmed exactly one H1, uninterrupted heading progression, blank-line block spacing, no trailing whitespace, and single trailing newlines. The same scan over complete files found only pre-existing em dashes on untouched lines in `docs/cli.md`, `docs/integrations.md`, and `internal/AGENTS.md`; none was introduced or modified by this pass.

## Release and publication status

The intended release target is `v1.3.0`. Publication is pending the standard release gates: the repository quality and security reviews, a published `v1.3.0` GitHub Release, the release workflow's Go test/vet/build plus smoke and npm launcher checks, all four native build jobs on matching runners, checksums, and npm publication of `@digitalygo/spynel` with provenance. Host-target `scripts/package-native.sh` validation and execution of the extracted archive remain useful pre-release checks. This record does not claim that `v1.3.0` is published, and it does not claim that any live provider compaction or import canary was run.

## References

- [Pi RPC documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md)
- [Pi sessions documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/sessions.md)
- [Pi session format documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/session-format.md)
- [Pi compaction documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/compaction.md)
- [Communication integrations](../../../docs/integrations.md)
- [Plain CLI and automation](../../../docs/cli.md)
- [Coding harness compatibility](../../../docs/harness-compatibility.md)
- [Local Pi configuration](2026-09-19-local-pi-configuration.md)
- [Telegram topic conversations and bounded rich-text replies](2026-09-20-telegram-topic-rich-text.md)

## Update 2026-09-20: v1.3.0 publication

### Summary of changes

The pending state in "Release and publication status" above is resolved: `v1.3.0` is released at tag target `b8ce0b6a12906324e948501e5280caad6afb74f5`, the stable non-prerelease GitHub Release publishes four native archives plus `checksums.txt`, `@digitalygo/spynel@1.3.0` is the npm `latest` package with SLSA provenance v1 through Trusted Publishing, and the live npm-managed home service runs that release. No real-provider compaction or import canary and no Telegram session-notice canary were run; the Pi session-control behavior remains evidenced by the automated fixtures and race tests, the smoke checks, the native packaging evidence, and the release gates recorded below.

### Technical reasoning

The release reused the existing gates instead of adding publication steps. A manual `workflow_dispatch` validation run on the exact release commit (run 35515300385) passed the `verify` job and all four native build jobs for Linux amd64, Linux arm64, macOS amd64, and macOS arm64 while the `publish` job remained skipped, keeping validation separate from publication. Publishing the GitHub Release then triggered run 35515821891, which passed `verify`, the same four native jobs, and `publish`. The publish job attached four native archives plus `checksums.txt` and released npm through GitHub Actions Trusted Publishing, so no npm token or other long-lived publication credential was used. The local native package for `v1.3.0` passed, and this record's documentation, DOX, and compiled agent documentation pass, originally recorded as uncommitted, is committed as the `v1.3.0` tag target itself.

One environment condition repeated from the `v1.2.0` rollout without producing a release defect. The updater's first attempt hit the known read-only local Linuxbrew npmrc `0444` `EACCES`; the official updater succeeded after a temporary owner-write, the original `0444` mode and hash were restored, and no persistent permission change remains. The Node.js 20 deprecation annotation on the release workflow's artifact action was informational and non-blocking.

### Impact assessment

- [GitHub Release v1.3.0](https://github.com/digitalygo/spynel/releases/tag/v1.3.0) is public, stable, and non-prerelease at `b8ce0b6a12906324e948501e5280caad6afb74f5`, with `checksums.txt` and the four supported native archives for Linux amd64, Linux arm64, macOS amd64, and macOS arm64.
- npm `latest` resolves to `@digitalygo/spynel@1.3.0` with the `spynel` bin mapping, Digitalygo repository metadata, and a SLSA provenance v1 attestation produced through Trusted Publishing without an npm token.
- The live npm-managed home service runs `1.3.0` with Telegram and Pi connected and an inactive turn, inherits model, reasoning, and service settings, keeps task reviews at `never`, reports `Current` and `Latest` both `1.3.0` with no available update, and exposes `/pi` in its compiled documentation.
- The Pi session controls documented in this record are part of the published release, and the earlier note that the documentation pass was left unstaged no longer describes the repository state.
- No live real-provider compaction or import canary and no Telegram session-notice canary are claimed; the automated fixtures, race tests, smoke checks, native packaging evidence, and release gates remain the behavioral evidence.

### Validation steps

- `git rev-list -n1 v1.3.0` resolves the tag to `b8ce0b6a12906324e948501e5280caad6afb74f5`.
- Manual validation run 35515300385 completed successfully: `verify` passed, all four native build jobs passed, `publish` was skipped.
- Release run 35515821891 completed successfully: `verify` passed, all four native build jobs passed, `publish` passed.
- Release inspection confirmed a stable non-prerelease `v1.3.0` release with `checksums.txt` and the four supported native archives for Linux amd64, Linux arm64, macOS amd64, and macOS arm64.
- Registry inspection confirmed `latest` at `1.3.0`, the expected `spynel` bin mapping and Digitalygo repository metadata, and the SLSA provenance v1 predicate with no npm token used for publication.
- The local native package for `1.3.0` passed, and a clean-prefix installation of exactly `@digitalygo/spynel@1.3.0` reported `spynel 1.3.0`.
- The live npm-managed home service updated to `1.3.0`; `spynel status --json` reports Telegram and Pi connected with an inactive turn, inherited model, reasoning, and service settings, and task reviews `never`; `spynel update --json check` reports `Current` and `Latest` both `1.3.0`; `/pi` appears in the compiled docs.
- The updater's first Linuxbrew attempt hit the read-only npmrc `0444` `EACCES`; the official updater succeeded after a temporary owner-write; the original `0444` mode and hash were restored with no persistent permission change.
- `scripts/dev.sh dox` and `git diff --check` passed for this documentation-only update.
