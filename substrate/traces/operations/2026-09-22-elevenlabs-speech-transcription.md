---
status: completed
created_at: 2026-09-22
updated_at: 2026-09-22
files_edited:
  - AGENTS.md
  - docs/AGENTS.md
  - docs/architecture.md
  - docs/configuration.md
  - docs/configuration-live-matrix.md
  - docs/integrations.md
  - docs/troubleshooting.md
  - internal/AGENTS.md
  - internal/app/session_name.go
  - internal/app/session_name_test.go
  - internal/channel/telegram/telegram.go
  - internal/channel/telegram/telegram_test.go
  - internal/channel/whatsapp/whatsapp.go
  - internal/channel/whatsapp/whatsapp_test.go
  - internal/cli/cli.go
  - internal/cli/cli_test.go
  - internal/config/AGENTS.md
  - internal/config/config.go
  - internal/config/config_test.go
  - internal/config/settings.go
  - internal/config/settings_test.go
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
rationale: Record the ElevenLabs speech provider from its original opt-in introduction through its current default status with a sentinel-only missing-key fallback to local Parakeet, the provider-neutral transcription contract, and the shared transcript markers, together with the DOX and user-documentation passes that keep the contracts aligned with the implemented behavior.
supporting_docs:
  - ../../../docs/configuration.md
  - ../../../docs/configuration-live-matrix.md
  - ../../../docs/integrations.md
  - ../../../docs/troubleshooting.md
  - ../../../docs/architecture.md
  - ../../../internal/media/AGENTS.md
  - ../../../AGENTS.md
  - ../../../docs/AGENTS.md
  - ../../../internal/config/AGENTS.md
  - https://elevenlabs.io/pricing
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
