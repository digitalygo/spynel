---
status: completed
created_at: 2026-09-22
updated_at: 2026-09-23
files_edited:
  - AGENTS.md
  - docs/AGENTS.md
  - docs/architecture.md
  - docs/cli.md
  - docs/configuration.md
  - docs/configuration-live-matrix.md
  - docs/integrations.md
  - docs/troubleshooting.md
  - internal/AGENTS.md
  - internal/agentdocs/content.go
  - internal/app/configuration.go
  - internal/app/service.go
  - internal/app/service_test.go
  - internal/app/session_name.go
  - internal/app/session_name_test.go
  - internal/channel/telegram/telegram.go
  - internal/channel/telegram/telegram_test.go
  - internal/channel/tui/tui_test.go
  - internal/channel/whatsapp/whatsapp.go
  - internal/channel/whatsapp/whatsapp_test.go
  - internal/cli/cli.go
  - internal/cli/cli_test.go
  - internal/config/AGENTS.md
  - internal/config/config.go
  - internal/config/config_test.go
  - internal/config/settings.go
  - internal/config/settings_test.go
  - internal/localapi/localapi_test.go
  - internal/media/AGENTS.md
  - internal/media/elevenlabs.go
  - internal/media/elevenlabs_test.go
  - internal/media/fallback.go
  - internal/media/fallback_test.go
  - internal/media/parakeet.go
  - internal/media/parakeet_test.go
  - internal/media/store.go
  - internal/media/transcription.go
  - internal/media/transcription_test.go
  - internal/workspace/templates/config.yaml
  - internal/workspace/workspace_test.go
  - substrate/traces/operations/2026-09-22-elevenlabs-speech-transcription.md
rationale: Record the ElevenLabs speech provider from its original opt-in introduction through its current default status with a sentinel-only missing-key fallback to local Parakeet, the provider-neutral transcription contract, and the shared transcript markers, together with the DOX and user-documentation passes that keep the contracts aligned with the implemented behavior. The stored-key update adds the Telegram-style persisted key, stored-first resolution, and the generic `/config unset` clear operation with its accepted trust boundaries.
supporting_docs:
  - ../../../docs/configuration.md
  - ../../../docs/configuration-live-matrix.md
  - ../../../docs/integrations.md
  - ../../../docs/troubleshooting.md
  - ../../../docs/architecture.md
  - ../../../docs/cli.md
  - ../../../internal/media/AGENTS.md
  - ../../../AGENTS.md
  - ../../../docs/AGENTS.md
  - ../../../internal/config/AGENTS.md
  - https://elevenlabs.io/pricing
  - https://github.com/digitalygo/spynel/releases/tag/v1.5.0
  - https://github.com/digitalygo/spynel/releases/tag/v1.5.1
---

# ElevenLabs speech transcription

## Summary of changes

Speech transcription gains a live provider switch. `speech.provider` selects the default local `parakeet` backend or the opt-in cloud `elevenlabs` backend, with the new keys defaulting to the existing local behavior. Transcription now covers Telegram and WhatsApp voice notes and audio files through a provider-neutral `media.Transcriber` request that carries the attachment path and the transport-declared duration. The three user-visible markers moved into `internal/media`, and session-label derivation skips them plus the legacy voice-era prefixes.

The ElevenLabs client streams the stored attachment to the fixed Speech-to-Text endpoint with the standard library only, enforces the size and duration limits before upload, serializes one upload process-wide under a 15-minute budget, parses only the exact `text` field, sanitizes provider errors, and retries exactly once for a bounded rate limit. There is no fallback path between providers, and selecting ElevenLabs never initializes or downloads a local model. The default path (enabled, `parakeet`, English) is unchanged.

This record also captures the documentation and DOX pass that added the provider switch to the root and package contracts and rewrote the configuration, live-matrix, integrations, architecture, and troubleshooting documentation around the implemented behavior.

## Technical reasoning

### Why the API key is an environment reference

`speech.elevenlabs_api_key_env` stores a portable variable name, not a key. The client resolves the value with `os.Getenv` at the start of every transcription, so the key never enters `.spynel/config.yaml`, durable history, runtime logs, or status output. This matches the existing `token_env` convention for the Telegram token and keeps a cloud credential out of a file that users share, copy, or paste into issues. The name is validated as `[A-Za-z_][A-Za-z0-9_]*` with at most 128 bytes, so a typo fails at save time instead of surfacing later as a confusing missing-key error. The setting itself is deliberately not a secret setting: it names a variable, and the referenced value is never stored or shown.

### Why redirects are refused

The request carries the `xi-api-key` header and the complete audio body. A default `http.Client` would forward both to whatever `Location` a redirect names, which would leak the credential and the audio to an unrelated host. `CheckRedirect` returns `http.ErrUseLastResponse`, so a redirect becomes a visible HTTP 3xx failure instead of a silent forward. The endpoint is a package constant rather than configuration, so no setting can point the billed traffic at a different server.

### Why limits are enforced before upload

`speech.max_file_mb` is checked from the stat of the opened descriptor, and ElevenLabs requires a positive declared duration, compares it with `speech.max_duration_seconds`, and fails closed on a missing or malformed value before any network call. Rejecting oversized or unbounded uploads locally avoids paying for a request that cannot succeed and avoids streaming an unbounded body. The duration arrives from the transport message and is controlled by the sender, so it is not a hard bound by itself; `max_file_mb` remains the enforceable limit, and the security review recorded that trust assumption as a non-blocking advisory. Parakeet deliberately ignores the declared duration and keeps its decoder-based duration bound.

### Why the retry is narrow

Exactly one retry happens, and only for HTTP 429 with provider code `rate_limit_exceeded` and a valid `Retry-After` of at most 60 seconds. Other 4xx responses are request or account defects that will not improve, 5xx responses and timeouts are ambiguous, and cancellations must stop immediately. The attempt and the retry wait share one 15-minute context that a shorter caller deadline still bounds, and the retry rebuilds the multipart body from the same validated descriptor and size, so nothing is buffered between attempts. A second rate limit is a visible failure, not another wait.

### Why the markers are centralized

Both transports rendered the same three strings inline, which invited drift once the wording changed. `internal/media/transcription.go` now owns `TranscriptionDisabledMarker`, `TranscriptionFailedMarker`, and `TranscriptionGeneratedMarker`. The failed marker sanitizes untrusted provider or decoder text to one bounded line, strips control and format characters, neutralizes square brackets, and caps the length, so an error can neither close the marker early nor smuggle an attachment directive into the message. Session-label derivation recognizes the new prefixes and the legacy voice prefixes, so existing histories keep producing sane labels.

### Provider-neutral versus Parakeet-only settings

`language`, `max_file_mb`, and `max_duration_seconds` now apply to both providers. `model_dir`, `num_threads`, and `chunk_seconds` remain Parakeet-only. `speechCacheStartupFailure` gates the per-user model-cache requirement on the Parakeet provider with an empty `model_dir`, so an ElevenLabs-configured workspace can start without a local model cache.

## Impact assessment

- Privacy: selecting `elevenlabs` sends accepted user audio, including group audio where the group policy permits processing, to the ElevenLabs API. The default keeps audio local, and the provider choice is explicit per workspace.
- Cost: ElevenLabs bills by audio duration, so longer or more frequent audio costs more than a per-request model would suggest. The documentation links to the provider pricing page and deliberately quotes no prices, because they change.
- At-least-once: transcription is best-effort and at least once. A redelivered transport message can download, transcribe, and upload the same audio again; there is no transcription deduplication cache, and a failed upload is not retried beyond the one narrow rate-limit retry.
- Operational: the key environment variable must be visible to the running Spynel process. Generated autostart services preserve home and cache paths but not arbitrary shell exports, so an interactive shell key can still be missing from a service.
- Compatibility: existing workspaces load unchanged and keep local Parakeet behavior, because omitted new keys default to `parakeet`, `ELEVENLABS_API_KEY`, and `scribe_v2`, and every new setting applies live.
- No fallback: a failing ElevenLabs call never silently switches to Parakeet. The transcript fails with the visible failed marker and the attachment link stays in the message.

## Validation steps

Implementation evidence recorded by the implementation and review work on this tree:

- `scripts/dev.sh test` passed, covering the full Go suite plus the nested Bubble Tea module tests.
- `go test -race -count=1 ./internal/media ./internal/channel/whatsapp ./internal/channel/telegram ./internal/app` passed. The Telegram package includes `TestWebhookModeVerifiesSecretAndRoutesUpdate`, the repository's known flaky webhook test; it passed isolated re-runs, and this delta does not touch webhook routing.
- `go vet ./...` was clean.
- `mkdir -p .tmp-bin && go build -o .tmp-bin/spynel ./cmd/spynel` succeeded, and the disposable binary was removed.
- `scripts/smoke.sh` passed.
- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- Coverage over the executable delta: `internal/media/elevenlabs.go` 93.3%, `internal/media/transcription.go` 100%, Telegram `messageText` 88.2%, WhatsApp `prepareMessage` 96.3%, WhatsApp `downloadableMedia` 100%, and the CLI selection helpers `speechTranscriber` and `speechCacheStartupFailure` 100%.
- Quality gate verdict: PASS with two non-blocking advisories. The disabled-speech guard inside `ElevenLabs.Transcribe` is unreachable through the production selector because a disabled setting yields no transcriber, so it is defense in depth only. Two retry-matrix sub-cases exercise the same shared retry predicate rather than independent branches.
- Security review verdict: PASS with two non-blocking advisories. The transport-declared duration is sender-controlled data, so `max_file_mb` is the enforceable bound and the trust assumption is now documented for users. The key check is a hint: use a dedicated environment variable for the ElevenLabs key instead of a broadly exported one.

Documentation pass checks run for this record on the completed tree:

- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- `go test -count=1 ./internal/media ./internal/config ./internal/app ./internal/agentdocs` passed (`media` 1.141s, `config` 0.014s, `app` 14.222s, `agentdocs` 0.005s).
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of the new record and every added line across the changed files confirmed one H1 per file, uninterrupted heading progression, blank lines around blocks, no em dash, no trailing whitespace, and a single trailing newline.

## Live canary and release status

No authenticated live ElevenLabs canary was run, and none is claimed. The evidence is deterministic `httptest` responses plus unit, channel, and CLI tests; no live provider request, billing event, or transcript is represented here. No release, tag, package publication, or commit was requested or prepared by this work. Every changed and new file is left unstaged in the working tree.

## References

- [Configuration](../../../docs/configuration.md)
- [Configuration application matrix](../../../docs/configuration-live-matrix.md)
- [Communication integrations](../../../docs/integrations.md)
- [Troubleshooting](../../../docs/troubleshooting.md)
- [Architecture](../../../docs/architecture.md)
- [Media DOX](../../../internal/media/AGENTS.md)
- [ElevenLabs pricing](https://elevenlabs.io/pricing)

## Update 2026-09-22: default provider and key-missing fallback

### Summary

Speech transcription now defaults to the cloud `elevenlabs` provider, and a missing or blank API key environment variable at transcription time falls back to local Parakeet for that call. The fallback is sentinel-only: `media.ErrSpeechAPIKeyMissing` is the sole substitution condition, and invalid keys, rate limits, timeouts, network errors, empty transcripts, and every other provider outcome still surface as the ordinary failure marker. The startup model-cache check now applies only to explicit `speech.provider: parakeet`; a fallback call that finds an unusable local model reports the transcription-time failure marker instead. This update supersedes the no-fallback and local-default statements in the record above and captures the matching documentation and DOX reconciliation.

### Technical reasoning

The product decision makes the cloud backend the default while keeping local transcription reachable without configuration: a missing or blank `speech.elevenlabs_api_key_env` value selects the local Parakeet backend for that call, so a workspace without a key still transcribes when the local model is available.

The fallback is deliberately narrow. The ElevenLabs client reports `ErrSpeechAPIKeyMissing` only for the missing-or-blank variable, and the `NewFallback` wrapper substitutes exactly on `errors.Is(err, ErrSpeechAPIKeyMissing)`. Invalid keys (HTTP 401), rate limits, timeouts, network errors, and empty transcripts are provider evidence that must not be hidden behind a local transcript, so they pass through unchanged. The ElevenLabs boundaries from the original work are untouched: fixed endpoint, redirect refusal, pre-upload size and duration gates, streamed multipart, bounded sanitized errors, and the single narrow rate-limit retry.

The local-model startup cache gate keeps its original scope: it aborts startup only for explicit `parakeet` without an explicit `model_dir`. A missing-key fallback reuses the same local backend without re-gating startup, so a model problem in that path surfaces at transcription time as the ordinary failure marker. The fallback direction is one-way: local Parakeet never sends audio to the cloud.

### Impact

- Upgrade note (security-relevant): a pre-existing workspace that never wrote `speech.provider` decodes to the new `elevenlabs` default. If a non-empty `ELEVENLABS_API_KEY` is already visible to the running Spynel process, voice and audio messages start going to the ElevenLabs cloud API after upgrade without further action. Setting `speech.provider: parakeet` keeps transcription local. The security gate recorded that release notes must carry this behavior change; the user documentation and this record now state it.
- Privacy and cost: with a key present, accepted audio goes to ElevenLabs by default; without a key, every accepted audio stays local through the fallback, which may download the local model at transcription time.
- Startup gating: fail-closed. Only explicit `parakeet` gates startup on the local model cache, and no code path silently sends audio to the cloud when the key is absent. An unusable local model during a fallback fails at transcription time with the ordinary marker instead of disabling transcription or reaching the provider.
- DOX drift: root `AGENTS.md`, `internal/AGENTS.md`, `internal/media/AGENTS.md`, `internal/config/AGENTS.md`, `docs/AGENTS.md`, `docs/architecture.md`, `docs/configuration.md`, `docs/configuration-live-matrix.md`, `docs/integrations.md`, and `docs/troubleshooting.md` now describe the default and the sentinel-only fallback. The package comment in `internal/media/elevenlabs.go` still calls the backend opt-in; this pass is documentation-only and left all Go code and tests untouched.

### Validation

Implementation and review evidence for this delta:

- `scripts/dev.sh test` passed, covering the full Go suite plus the nested Bubble Tea module tests.
- `go test -race -count=1 ./internal/media ./internal/cli` passed.
- `go vet ./...` was clean.
- `mkdir -p .tmp-bin && go build -o .tmp-bin/spynel ./cmd/spynel` succeeded.
- `scripts/smoke.sh` passed.
- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- Coverage over the new fallback wrapper: `internal/media/fallback.go` 100%.
- Quality gate verdict: PASS with two non-blocking advisories.
- Security gate verdict: PASS with three non-blocking advisories, including the release-notes requirement for the default-provider switch.

Documentation pass checks run for this update on the completed tree:

- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- `go test -count=1 ./internal/media ./internal/config ./internal/app ./internal/agentdocs` passed.
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of every added line across the changed files confirmed one H1 per file, uninterrupted heading progression, no em dash, no trailing whitespace, and a single trailing newline.

## Update 2026-09-22: v1.5.0 publication

### Summary

v1.5.0 is published and carries this feature from the provider commits through the default fallback. The stable [GitHub release v1.5.0](https://github.com/digitalygo/spynel/releases/tag/v1.5.0) (annotated tag object `6189d5e83b9ba98c973927bd297955e57a305f85`, target commit `3345548e8fecc315387df5573be34772c05a1082`) ships four native archives plus `checksums.txt`, and `@digitalygo/spynel@1.5.0` is the npm `latest` with SLSA provenance v1 from Trusted Publishing. The release bundles `015542e` (provider, audio-file scope, shared markers), `af4d985` (default `elevenlabs` with the missing-key local fallback), and `3345548` (the shutdown and quiesce fix). The live npm-managed home service was updated to 1.5.0 through the official `spynel update` path. Its restarted process does not inherit the interactive shell's `ELEVENLABS_API_KEY`, so the service currently transcribes through the missing-key local Parakeet fallback.

### Technical reasoning

The pre-release validation attempt at `af4d985` was blocked by intermittent `internal/localapi` `TempDir` cleanup races in CI. The job-archive shutdown and quiesce fix, committed as `3345548`, removed that failure mode (see [Job archive shutdown quiesce](2026-09-22-job-archive-shutdown-quiesce.md)), and the retried validation passed. The pipeline then ran a manual validation (35782622472) with publish skipped, followed by the release run (35783688043) that repeated the checks and performed the actual publish. The release notes carry the upgrade note the security gate required for the default-provider switch.

The live update exercised the release end to end. The Linuxbrew npmrc quirk was handled with a temporary owner-write plus a `trap` restore, and the npmrc hash was unchanged this time. The environment mismatch is the operational risk the earlier update described: a key exported only in an interactive shell is absent from a detached service, so the missing-key path selects local Parakeet. One restart from a key-bearing shell is enough for ElevenLabs to engage, and later in-place re-exec updates preserve that process environment. The transcription markers still do not name the producing provider, so the fallback is not visible from the transcript itself; that remains a known observability gap.

### Impact

- The cloud default with the sentinel-only Parakeet fallback is now live for consumers of the native archives and npm `latest`, not only for the development tree.
- The live home service is healthy on the fallback path: Telegram and Pi are connected and idle, settings are inherited, `reviews` stays `never`, and the updater reports the current version 1.5.0.
- Until the service is restarted from a key-bearing shell, accepted audio is transcribed locally rather than by ElevenLabs. No message flow is lost, and every other provider outcome stays visible, because the fallback remains limited to the missing-key sentinel.
- The shutdown and quiesce fix that unblocked validation ships in the same release, so the CI `TempDir` flake mode described in the sibling record is gone from v1.5.0 onward.
- Observability gap: nothing in a delivered transcript distinguishes ElevenLabs from Parakeet output.

### Validation

Publication and deployment evidence gathered on 2026-09-22:

- Manual validation run 35782622472 passed verify and all four native builds with publish skipped.
- Release run 35783688043 passed verify, all four native builds, and publish.
- Local host package validation passed: `spynel_1.5.0_linux_amd64.tar.gz` with sha256 `4437fa3dab7507de8b341f806b6873f1e2e024270566756886afb2b4710d1f6e`, and the extracted binary reports version 1.5.0.
- `gh release view v1.5.0` reports a non-draft, non-prerelease release with `checksums.txt`, `spynel_1.5.0_darwin_amd64.tar.gz`, `spynel_1.5.0_darwin_arm64.tar.gz`, `spynel_1.5.0_linux_amd64.tar.gz`, and `spynel_1.5.0_linux_arm64.tar.gz`.
- `npm view` reports `@digitalygo/spynel@1.5.0` as `latest` with SLSA provenance v1 (`https://slsa.dev/provenance/v1`) published by GitHub Actions OIDC, and a clean-prefix install reported 1.5.0 after the usual brief registry edge convergence.
- The live npm-managed home service runs 1.5.0 after the official `spynel update`; Telegram and Pi are connected and idle, settings are inherited, `reviews` is `never`, and the updater is current at 1.5.0.
- The restarted service process lacks `ELEVENLABS_API_KEY`, matching the missing-key fallback state described above.

Checks for this record's update on the completed tree:

- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of every added line confirmed one H1 in this file, uninterrupted heading progression, no em dash, no trailing whitespace, and a single trailing newline.

## Update 2026-09-22: stored ElevenLabs key with CLI configuration

### Summary

The ElevenLabs speech API key can now be stored in the private workspace configuration as `speech.elevenlabs_api_key`, mirroring the Telegram bot token pattern, and set or cleared directly from the CLI with `spynel config set|unset speech.elevenlabs_api_key ...` or the shared `/config` commands. The stored key is a masked secret: lists, get output, forms, and screens report only `set` or `not set`, command history and session labels redact the value, and status, doctor, runtime logs, templates, and canonical empty saves never contain it. Resolution is stored-first: the trimmed stored key wins over the named environment variable, a blank stored value falls through to the environment, and only two empty sources trigger the existing local Parakeet fallback. A new generic `/config unset <key>` and `spynel config unset <key>` operation clears any clearable setting through the same validated save boundary. This update also reconciles the documentation and DOX prose that still described the key as environment-only and never stored.

### Technical reasoning

The product request was explicit: make the ElevenLabs key behave like the Telegram token, so an operator can persist it once in private workspace state instead of depending on a shell export that a detached autostart service does not inherit. `Config.ElevenLabsAPIKey()` therefore resolves the trimmed stored value first and the trimmed environment value second; a blank stored value falls through, preserving existing environment-only workspaces, and the missing-key sentinel still fires only when both are empty. Stored-first is deliberate: an explicit workspace change is the operator's most recent intent and must win over an inherited variable, and a nonblank but invalid stored key is authoritative provider evidence, so it surfaces the provider error instead of hiding behind a local transcript.

Clearing needed a non-empty counterpart because `SetSetting` assigns a trimmed value and the ordinary `/config set` path cannot express an empty value for validated settings. `/config unset <key>` routes to the same `SetSetting` call with an empty value: clearable settings reset, and settings that cannot be empty reject with their ordinary validation error (for example, clearing `channels.telegram.token` while Telegram is enabled). This keeps one validated save boundary and one rollback behavior rather than a second persistence path.

The trust boundaries are accepted and documented rather than hidden. Redaction protects Spynel's own durable history, session labels, replies, status, doctor, and runtime logs; it cannot protect a CLI positional value that already entered shell history and the process argument list, a value typed into Telegram or WhatsApp that transits those provider servers, a raw command delivered to a trusted `message.received` extension, or the authenticated loopback request body that carries the value to the elected owner. Configuration storage is plaintext in the private `0600` workspace file, not encrypted. The documentation recommends entering stored secrets from local surfaces (TUI or CLI) with that tradeoff stated plainly.

### Impact

- Rotation and recovery: rotate by setting a new value; clear with `/config unset speech.elevenlabs_api_key` to return to the environment variable and the existing missing-key fallback.
- Clearing from the TUI is command-only: the form renders a configured secret as a masked `(configured)` control, an untouched secret is never resubmitted, and there is no clear control, so `/config unset` remains the supported clear action.
- Autostart services no longer need shell-only exports when the key is stored in the workspace; the environment reference remains available for operators who prefer it.
- All previously verified ElevenLabs boundaries and fallback semantics are unchanged: fixed endpoint, redirect refusal, pre-upload limits, streamed multipart, bounded sanitized errors, the single narrow rate-limit retry, and sentinel-only fallback.
- DOX and user documentation now match the implemented behavior across root `AGENTS.md`, `internal/AGENTS.md`, `internal/config/AGENTS.md`, `internal/media/AGENTS.md`, `docs/configuration.md`, `docs/configuration-live-matrix.md`, `docs/integrations.md`, `docs/troubleshooting.md`, `docs/cli.md`, and `docs/architecture.md`; the compiled `internal/agentdocs` secret topic already described stored secrets and `/config unset` and was verified rather than edited.

### Validation

Implementation, quality, and security evidence for this delta:

- `scripts/dev.sh test` passed, covering the full Go suite plus the nested Bubble Tea module tests.
- Race runs passed with an explicit timeout for `./internal/config ./internal/media ./internal/app`.
- `go vet ./...` was clean.
- `mkdir -p .tmp-bin && go build -o .tmp-bin/spynel ./cmd/spynel` succeeded.
- `scripts/smoke.sh` passed.
- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- Coverage over the changed functions: `Config.ElevenLabsAPIKey` 80%, `SetSetting` 100%, `SetSettings` 95.2%, `IsSecretSetting` 100%, `secretState` 100%, `redactSensitiveCommand` 100%.
- Quality gate verdict: PASS with two non-blocking advisories, including the wording of the unset rejection when Telegram is enabled.
- Security gate verdict: PASS with two non-blocking advisories, including stale comments fixed in parallel and the local-surface entry recommendation now documented.

Documentation pass checks run for this update on the completed tree:

- `scripts/dev.sh dox` passed.
- `go test -count=1 ./internal/config ./internal/media ./internal/app ./internal/agentdocs` passed.
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of every added line across the changed files confirmed one H1 per file, uninterrupted heading progression, no em dash, no trailing whitespace, and a single trailing newline.

## Update 2026-09-23: v1.5.1 publication

### Summary

v1.5.1 is published as a patch release carrying the stored ElevenLabs key with CLI configuration. The stable [GitHub release v1.5.1](https://github.com/digitalygo/spynel/releases/tag/v1.5.1) (annotated tag, target commit `84e1f817cab58ea023f0f87b944a1308a7101af4`) ships four native archives plus `checksums.txt`, and `@digitalygo/spynel@1.5.1` is the npm `latest` with SLSA provenance v1 from Trusted Publishing. The user requested the stored-key work as a patch because the provider feature shipped in v1.5.0, and this release adds `spynel config set`/`spynel config unset`, the masked secret, and stored-first resolution to that line. The live npm-managed home service was updated from 1.5.0 to 1.5.1 through the official `spynel update` path, and the in-place re-exec preserved the key-bearing environment, so `ELEVENLABS_API_KEY` remains PRESENT as the sole key source while the stored setting stays unset.

### Technical reasoning

The patch scope is deliberate. The stored key completes the operational story for detached services, whose generated autostart units do not inherit interactive shell exports, and it was requested as a patch so the v1.5.0 provider line receives it without a new minor release. Nothing else changed in the release, so every v1.5.0 boundary and the sentinel-only Parakeet fallback carry over unchanged.

Publication followed the same two-step validation as v1.5.0. A manual validation run passed verify and all four native builds with publish skipped, then the release run repeated the checks and performed the publish, so the published artifacts come from the commit that already passed the dry run. Local host package validation added an independent third check: the extracted binary reports 1.5.1 and the archive digest matches the published checksum. SLSA provenance v1 through Trusted Publishing ties the npm artifact to the GitHub Actions OIDC identity.

The upgrade preserved the process environment because the in-place re-exec keeps the environment of the replaced process. `ELEVENLABS_API_KEY` remains PRESENT in the restarted service and the stored `speech.elevenlabs_api_key` stays unset, so the environment is still the sole key source and the stored-first precedence has not been exercised live yet. The live speech settings are `provider=elevenlabs`, `language=auto`, and `model_id=scribe_v2`.

One verification nuance is worth recording. An intermediate check reported `speech.language=en` and the stored key as not set because it read the repository workspace configuration instead of `/home/luca/.spynel/config.yaml`. The home configuration is correct (`language: auto`, stored key unset, environment-provided key in use) and its file was last written well before the release, so the odd reading was a target mix-up rather than configuration drift.

### Impact

- The native archives and npm `latest` now deliver v1.5.1 with the stored-key support, and v1.5.0 is superseded.
- The live home service is healthy on 1.5.1: Telegram and Pi are connected and idle, settings are inherited, `reviews` stays `never`, and the updater reports the current version.
- The stored-first precedence path remains unexercised in the live service because the stored value stays unset; the environment variable still provides the key, and a stored key would win if one is set later.
- Non-blocking observations only: CI carries Node 20 `actions/upload-artifact` deprecation annotations, and the npm install path prints an informational `allowScripts` warning on postinstall. Neither affects the published artifacts or the live service.

### Validation

Publication and deployment evidence gathered on 2026-09-23:

- Manual validation run 35876976480 passed verify and all four native builds with publish skipped.
- Release run 35878168313 passed verify, all four native builds, and publish.
- Local host package validation passed: `spynel_1.5.1_linux_amd64.tar.gz` with sha256 `63a803c471c7d812c5840c120562157f76ebbb804c2a6e29f8ce656c99f64f8d`, and the extracted binary reports version 1.5.1.
- The stable [v1.5.1 release](https://github.com/digitalygo/spynel/releases/tag/v1.5.1) carries the four native archives plus `checksums.txt`; `@digitalygo/spynel@1.5.1` is the npm `latest` with SLSA provenance v1 from Trusted Publishing, visible after a few minutes of registry propagation.
- The live npm-managed home service runs 1.5.1 after the official `spynel update`; the Linuxbrew npmrc quirk was handled with a temporary owner-write and a `trap` restore, and the npmrc hash was unchanged. Telegram and Pi are connected and idle, settings are inherited, `reviews` is `never`, and the updater is current at 1.5.1.
- The restarted service process still carries `ELEVENLABS_API_KEY` as PRESENT, the stored `speech.elevenlabs_api_key` is unset, and `speech.provider=elevenlabs`, `speech.language=auto`, and `speech.elevenlabs_model_id=scribe_v2`.
- The intermediate `speech.language=en`/not-set reading came from querying the repository workspace configuration instead of `/home/luca/.spynel/config.yaml`; the home configuration is correct (`language: auto`, stored key unset, environment-provided key in use) and its file predates the release.

Checks for this record's update on the completed tree:

- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of every added line confirmed one H1 in this file, uninterrupted heading progression, no em dash, no trailing whitespace, and a single trailing newline.
