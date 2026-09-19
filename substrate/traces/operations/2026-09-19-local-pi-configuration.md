---
status: completed
created_at: 2026-09-19
updated_at: 2026-09-19
files_edited:
  - .spynel/config.yaml
  - AGENTS.md
  - docs/architecture.md
  - docs/configuration.md
  - docs/harness-compatibility.md
  - internal/harness/AGENTS.md
  - internal/harness/pi.go
  - internal/harness/pi_test.go
  - internal/harness/process_fixture_test.go
rationale: Install and configure the published Digitalygo Spynel package to use Pi without inference or task-review overrides, then make the Pi adapter load the user's complete trusted Pi resource environment.
supporting_docs:
  - ../../../docs/architecture.md
  - ../../../docs/configuration.md
  - ../../../docs/getting-started.md
  - ../../../docs/harness-compatibility.md
---

# Local Pi configuration

## Summary of changes

Installed `@digitalygo/spynel@1.0.0` globally and initialized this repository as a local Spynel workspace. The workspace uses Pi, inherits Pi's model and reasoning defaults, and disables Spynel task review.

## Technical reasoning

Spynel must omit model and thinking overrides for Pi to apply its own saved defaults. The workspace therefore keeps `harness.model` empty and sets reasoning effort and service mode to inherit. `harness.reviews: never` removes the separate Spynel task-review phase because review already belongs to the user's Pi workflow.

Spynel keeps mandatory goal outcome review independently of the task-review setting. This behavior cannot be disabled by `harness.reviews`.

## Impact assessment

- `spynel` resolves to the npm-installed Digitalygo launcher and reports version `1.0.0`.
- Pi resolves to the local installation and reports version `0.85.1`.
- The workspace configuration is private mode `0600` under the gitignored `.spynel/` directory.
- The effective harness is `pi` with model `harness default`, reasoning effort `inherit`, service mode `inherit`, and task review `never`.
- Sandbox mode remains the existing `danger-full-access` default.
- Telegram, WhatsApp, autostart, and persistent Spynel processes remain disabled or stopped.

## Validation steps

- Confirmed `spynel --version` returns `spynel 1.0.0`.
- Confirmed `pi --version` returns `0.85.1`.
- Confirmed targeted `spynel config get` results for harness, model, reasoning effort, service mode, review policy, and sandbox.
- Ran `spynel doctor`; all local checks passed and Pi was detected.
- Ran `spynel status`; the workspace is idle with no primary process, active task, goal, or lease.
- Confirmed `.spynel/` is gitignored and no persistent Spynel process remains running.

## Update 2026-09-19: full Pi resource loading

### Summary of new work

Changed the built-in Pi adapter to start ordinary Pi RPC without suppressing the user's extensions, skills, prompt templates, or themes. Pi now exposes the same global resource environment, including extension-provided tools and providers, that the user maintains for direct Pi sessions.

### Technical reasoning for the update

The previous adapter passed fixed `--no-extensions`, `--no-skills`, `--no-prompt-templates`, and `--no-themes` flags. Those flags isolated the RPC subprocess from trusted user resources and prevented the global YouTrack extension from registering its tools. Removing the suppression flags is the smallest implementation that preserves Pi as the source of resource discovery and avoids duplicating an extension allowlist in Spynel.

Spynel passes no project approval override. Project-local Pi resources therefore remain subject to Pi's own saved trust decisions and non-interactive `defaultProjectTrust` behavior. Because loaded extensions can request interactive UI through RPC, Spynel now cancels blocking dialogs explicitly and publishes a status event rather than allowing a headless turn to stall. Fire-and-forget UI requests receive no response.

### Impact assessment for the update

- Global Pi extensions, custom tools and providers, skills, prompt templates, themes, context files, and model/provider settings load normally in Spynel-managed Pi processes.
- The YouTrack extension under the user's global Pi resource directory can register its tools for Spynel conversations.
- Pi model and reasoning inheritance, Spynel-owned session paths, native steering, and full-access behavior remain unchanged.
- Read-only mode retains Pi's strict `read,grep,find,ls` allowlist across built-in and extension tools.
- Pi resources remain separate from Spynel executable hooks.
- Interactive extension dialogs are unavailable through Spynel channels and fail closed by cancellation instead of hanging.

### Validation steps for the update

- Ran `scripts/dev.sh dox` successfully.
- Ran Pi harness tests with the race detector successfully.
- Ran the complete Go test suite and `go vet ./...` successfully.
- Built `cmd/spynel` successfully and ran `scripts/smoke.sh` successfully.
- Verified synthetic ordinary and model-discovery Pi invocations contain no resource-suppression or project-approval override flags.
- Verified a blocking extension dialog is cancelled, fire-and-forget UI receives no response, and the turn reaches `agent_settled`.
- Completed independent quality and focused security reviews with passing verdicts.
