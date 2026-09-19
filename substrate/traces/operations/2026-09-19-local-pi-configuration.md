---
status: completed
created_at: 2026-09-19
files_edited:
  - .spynel/config.yaml
rationale: Install the published Digitalygo Spynel package locally and configure the repository workspace to use Pi without model or reasoning overrides and without Spynel task review.
supporting_docs:
  - ../../../docs/configuration.md
  - ../../../docs/getting-started.md
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
