---
status: completed
created_at: 2026-09-23
updated_at: 2026-09-25
files_edited:
  - docs/configuration.md
  - docs/integrations.md
  - internal/agentdocs/agentdocs_test.go
  - internal/agentdocs/content.go
  - internal/app/AGENTS.md
  - internal/app/command_menu.go
  - internal/app/command_menu_test.go
  - internal/app/job_reconnect_test.go
  - internal/channel/telegram/AGENTS.md
  - internal/channel/telegram/commandmenu_test.go
  - internal/channel/telegram/commands.go
  - internal/channel/telegram/telegram.go
  - internal/cli/cli.go
  - internal/orchestrator/AGENTS.md
  - internal/orchestrator/manager_test.go
  - internal/orchestrator/routes.go
  - internal/orchestrator/routes_test.go
rationale:
  - Expose the framework-handled command set discoverably inside Telegram through the native Bot API command menu, keeping every listed command out of the harness agent.
  - Give long-running task and goal sessions more room before age-based stale recovery reclaims their leases: 4 hours for tasks, 12 hours for goals.
  - Keep the command policy application-owned and the transport validation-only, matching the existing channel and application dependency direction.
  - Publish the validated feature as v1.6.0 and update the local npm-managed service so Telegram can register the menu.
supporting_docs:
  - docs/integrations.md
  - docs/configuration.md
  - internal/agentdocs/content.go
  - substrate/traces/research/2026-09-19-spynel-architecture-security-quality-gates.md
  - substrate/traces/operations/2026-09-20-telegram-topic-rich-text.md
  - docs/releasing.md
  - https://github.com/digitalygo/spynel/releases/tag/v1.6.0
  - https://github.com/digitalygo/spynel/actions/runs/36078654371
  - https://github.com/digitalygo/spynel/actions/runs/36079830208
---

# Telegram command menu and longer stale thresholds

## Summary of changes

Spynel now best-effort registers a curated 25-entry command menu with the Telegram Bot API `all_private_chats` scope after `getMe`, before polling or webhook startup, and the fixed stale-recovery thresholds moved from 30 minutes to 4 hours for tasks and from 2 hours to 12 hours for goals.

## Technical reasoning

The user asked for the commands they can run from Telegram to be visible in Telegram itself, as messages that live and die in the framework and never reach the agent. Command routing already had that property (`internal/app/service.go` intercepts every `/` message before harness dispatch), so the real gap was discoverability: Telegram shows no command list until `setMyCommands` registers one.

- Menu policy lives in the application (`internal/app/command_menu.go`) as a tested ordered subset of the canonical slash catalog, because the channel packages must not depend on `internal/app` and the transport must not duplicate application command policy. Deriving the menu from `SlashCommands()` heuristically was rejected: it would risk advertising agent-dispatching commands (`/task`, `/goal`), the self-refusing `/telegram`, TUI-only commands (`/quit`, `/primary`, `/new`, `/resume`), and undocumented aliases.
- `internal/cli` wires the two sides with one line (`bot.SetCommands(app.TelegramCommands())`), and `internal/channel/telegram/commands.go` only validates and registers: Bot API name grammar `^[a-z0-9_]{1,32}$`, trimmed 1 to 256 code point valid-UTF-8 descriptions, first-wins dedup, 100 entry cap, exactly one `setMyCommands` request with scope `{"type":"all_private_chats"}`.
- Registration runs through the existing provider wrapper, so the live Telegram allow-list is re-applied and the single bounded 429 retry re-checks it. Ordinary provider failures log one content-free line and never stop the channel; only authorization loss and caller cancellation fail startup, and teardown `deleteWebhook` remains the sole post-revocation provider exception.
- The notification-basis question needed no code change. The behavior is already deterministic plus agent judgment (`internal/orchestrator/notification_decision.go`, `orchestrator.task_notifications` policy, task `notify` front matter) and is documented in `docs/configuration.md`; it was explained in chat instead of duplicated in docs.
- Stale thresholds are fixed route code (`internal/orchestrator/routes.go`), never configuration. The recovery prompt placeholder and the semantic heartbeat derive from the same value, so only the two route entries changed in production code.

A second agent session was editing `internal/channel/telegram/telegram.go` and `internal/cli/cli.go` in the same worktree during planning. Work was serialized behind an explicit coordination notice, its transcription work landed first as commit `7965b15`, and the delta was implemented afterwards on a clean tree.

## Impact assessment

- Any private Telegram user who opens the bot now sees the 25 command names and descriptions. Execution is unchanged: every inbound update still fails closed against the live allow-list before application handling. This visibility is documented as non-authorization in `docs/integrations.md` and the compiled channels topic.
- Groups get no command menu, so group behavior is unchanged, and the menu can never advertise `/pi` to a group surface.
- Task and goal leases now wait longer before age-based recovery reclaims them: resumption after a dead session is slower (up to 4 hours for tasks, 12 for goals), while active in-process harness sessions still fence recovery and foreign-owner leases still recover immediately. Duplicate dispatch risk decreases rather than increases.
- The threshold change made two test fixtures that encoded the old ages non-stale: `internal/orchestrator/manager_test.go` now derives the age from `workflowRoutes()[0].StaleAfter`, and `internal/app/job_reconnect_test.go` uses 24 hours with a comment naming the 4-hour threshold. Both preserve their original scenario intent; no test was weakened, skipped, or deleted.
- A provider-side menu set by a previous run persists until the next successful registration. Revoked or disabled Telegram cannot clear it, because post-revocation provider work is restricted to teardown `deleteWebhook`.

## Validation steps

- `go test ./...`: every package `ok`, including `internal/app`, `internal/channel/telegram`, `internal/orchestrator`, `internal/agentdocs`, `internal/cli`, `scripts`.
- `go vet ./...`: clean. `go build -o .tmp-bin/spynel ./cmd/spynel`: ok. `git diff --check`: clean. `gofmt -l` on touched packages: clean.
- `scripts/dev.sh dox`: `DOX coverage valid for 51 tracked directories`. `scripts/smoke.sh`: `Spynel smoke test passed`.
- New tests: `internal/app/command_menu_test.go` (4 tests: exact order and descriptions, bare-root format, root coverage against `SlashCommands()`, duplicates, exclusions and presences, copy isolation), `internal/channel/telegram/commandmenu_test.go` (12 tests: validation matrix, payload shape, polling and webhook registration order, non-fatal provider failure, revocation after `getMe` blocking the request, empty menu sending nothing), `internal/orchestrator/routes_test.go` (`TestWorkflowRouteStaleThresholds`).
- Coverage over the delta: `TelegramCommands`, `SetCommands`, `buildTelegramCommandMenu`, `registerCommands`, and `workflowRoutes` all 100 percent of statements.
- Quality gate: `PASS`. Residual advisories: the `internal/cli/cli.go` wiring line has no direct test (a dropped line would leave the feature silently absent while tests stay green), and the menu scope was read as surface eligibility rather than inspection-only, matching the user's phrase "the commands I can run from Telegram".
- Security review of the delta: `PASS`. Residual notes: menu visibility to unauthorized private users as described above, and the pre-existing `/extension install <git-url>` subcommand reachable behind the allow-list, unchanged by this work.
- Pre-existing parallel transcription work was left untouched and verified committed before implementation started; its earlier uncommitted state was snapshot in an ephemeral `substrate/traces/status/` record that was removed at closure.

## Update 2026-09-25: v1.6.0 publication and local deployment

### Summary of changes

The command menu and longer stale thresholds were published as [Spynel v1.6.0](https://github.com/digitalygo/spynel/releases/tag/v1.6.0) from commit `59972839b831385789658b034c587703ebf138e8`. The local npm-managed Spynel service was updated from 1.5.2 to 1.6.0 through `spynel update`; Telegram registered the intended private-chat menu.

### Technical reasoning

The previously installed v1.5.2 predates the menu commit, so pushing the feature to `main` did not change the running bot. A minor release was chosen for the new user-visible Telegram command menu and the task/goal recovery behavior change. The release kept the committed development-version placeholder and root README unchanged: the workflow prepared versioned package metadata and tag-pinned README links in its isolated release checkout.

Publication followed the documented two-stage process: [manual release validation](https://github.com/digitalygo/spynel/actions/runs/36078654371) on the exact source commit with publication skipped, then the annotated `v1.6.0` tag and public Release, which triggered the [publishing workflow](https://github.com/digitalygo/spynel/actions/runs/36079830208). A unique temporary validation branch pinned that commit and was deleted after both runs succeeded. Local installation used the official coordinated updater rather than a direct npm replacement; the same owner temporarily made a Linuxbrew npmrc owner-writable and restored its original mode through a shell trap.

### Impact assessment

- The public stable `latest` npm package and GitHub Release are now 1.6.0. The Release contains four native archives and `checksums.txt`; npm publication includes SLSA provenance v1.
- The live home-workspace server is 1.6.0 with the same service PID but a new registered process generation, `ready=true`, Telegram connected, Pi connected, and no active turns, jobs, or orchestration leases at verification time.
- The Telegram Bot API reports the exact 25 ordered command names and descriptions from `internal/app/command_menu.go` in the `all_private_chats` scope. This proves provider registration, not what an individual Telegram client has already rendered in its compose UI.
- The pre-existing untracked `.ai-telemetry/` directory was not staged, modified by this task, or packaged; its contents were not inspected. It made the DOX/smoke gate fail in the developer worktree because it lacks an `AGENTS.md`; the same gates passed in a clean detached Git worktree of the tagged source. The npmrc returned to mode `0444` with its prior digest unchanged.

### Validation steps

- Locally, `scripts/dev.sh test`, `node npm/test.js`, a Go build, and race tests for `internal/app`, `internal/channel/telegram`, and `internal/orchestrator` passed. In a clean detached worktree at the release commit, `scripts/dev.sh dox` and `scripts/smoke.sh` passed, and `npm/prepare-release.js v1.6.0 false` followed by `npm pack --dry-run --json` verified the version, README links, and absence of untracked telemetry from the npm package.
- Host `scripts/package-native.sh` produced a Linux amd64 1.6.0 archive, SHA-256 `8e6c063abde65717cfd3d1552ee472ea30e5e7d14e0f4f63eed771391cde67ee`; the extracted binary reported `spynel 1.6.0` with native libraries and licenses present. The independent public Linux amd64 archive matched `checksums.txt`, SHA-256 `5dc3cc22102e32150524aea327cf46f869f2903fa0685d130e7121830cc453db`, and launched as 1.6.0. Build output need not be byte-identical across local and CI runners.
- Manual validation run `36078654371` passed verify and all four native builds, with publish skipped; its four downloaded native evidence records were independently checked for the exact source commit, `observed-native` classification, 1.6.0 archive identity, and six passing checks each.
- Release run `36079830208` passed verify, four native builds, and publish on the same commit. The annotated tag peels to `59972839b831385789658b034c587703ebf138e8`; GitHub exposes five release assets, and npm `latest` points to 1.6.0 with SLSA provenance v1.
- Before update, the home service was idle and `spynel update --json check` reported 1.5.2 to 1.6.0 available. One official update completed successfully. Afterwards the installed CLI and registered live process both reported 1.6.0, with a new generation `dea13a0efacd0b7971298e3cab560702`, `ready=true`, current/latest equal, Telegram and Pi connected, no active work, and the Linuxbrew npmrc back at mode `0444` with its original SHA-256.
- A credential-safe read-only `getMyCommands` request for `all_private_chats` independently matched all 25 ordered command names and descriptions against the source catalog. No manual `setMyCommands` call was made.
