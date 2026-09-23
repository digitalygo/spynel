---
status: completed
created_at: 2026-09-23
files_edited:
  - AGENTS.md
  - docs/AGENTS.md
  - docs/configuration-live-matrix.md
  - docs/configuration.md
  - docs/integrations.md
  - docs/troubleshooting.md
  - internal/AGENTS.md
  - internal/channel/telegram/AGENTS.md
  - internal/channel/telegram/telegram.go
  - internal/channel/telegram/telegram_test.go
  - internal/channel/whatsapp/AGENTS.md
  - internal/channel/whatsapp/whatsapp.go
  - internal/channel/whatsapp/whatsapp_test.go
  - internal/cli/cli.go
  - internal/config/AGENTS.md
  - internal/config/config.go
  - internal/config/config_test.go
  - internal/config/settings.go
  - internal/config/settings_test.go
  - internal/markdown/AGENTS.md
  - internal/markdown/telegram_chunks.go
  - internal/markdown/telegram_chunks_test.go
  - substrate/traces/operations/2026-09-23-transcript-echo.md
rationale: Record the default-on speech.transcript_echo setting, the pre-dispatch standalone transcript echo on Telegram and WhatsApp, the literal TelegramPlainChunks render path for user speech, and the DOX plus user-documentation pass that names the echo as a separate outbound family without weakening the final-only rule for agent responses.
supporting_docs:
  - ../../../AGENTS.md
  - ../../../docs/AGENTS.md
  - ../../../docs/configuration.md
  - ../../../docs/configuration-live-matrix.md
  - ../../../docs/integrations.md
  - ../../../docs/troubleshooting.md
  - ../../../internal/AGENTS.md
  - ../../../internal/channel/telegram/AGENTS.md
  - ../../../internal/channel/whatsapp/AGENTS.md
  - ../../../internal/config/AGENTS.md
  - ../../../internal/markdown/AGENTS.md
  - 2026-09-22-elevenlabs-speech-transcription.md
---

# Transcript echo for speech transcription

## Summary of changes

`speech.transcript_echo` is a new live, non-secret, advanced boolean that defaults to `true`. When enabled, a successful speech transcription of a Telegram voice note or audio file, or a WhatsApp push-to-talk or ordinary audio message, is echoed back to the same chat or topic as a standalone message before the message is dispatched to the application agent. The echo contains exactly the shared generated-transcript block, meaning the marker line `[Generated speech transcription; may contain errors]` plus the trimmed transcript. A failed or disabled transcription produces no echo. On Telegram the echo replies to the originating audio message; the WhatsApp text path has no quoted-reply support. Echo delivery is non-fatal: a failed send is discarded, never affects the turn, and transcript content is never written to logs.

The echo is user content, so Telegram renders it literally through the new `markdownfmt.TelegramPlainChunks`. That function HTML-escapes the input and then reuses the existing Telegram chunk, reply-budget, entity, grapheme, and truncation pipeline, so speech characters cannot become markup. WhatsApp sends the literal transcript text on its text path; clients may still render matched formatting pairs in received text, and the sender cannot disable that behavior.

This record also covers the DOX and user-documentation pass for the feature. The root and internal contracts, both channel DOX files, and the config and markdown DOX files now name the transcript echo as a separate outbound family with its setting, rendering, and delivery rules. The configuration, live-matrix, integration, and troubleshooting documents describe the same behavior. `internal/agentdocs/content.go` was inspected and left unchanged because it does not describe transcript handling; its only transcript reference is the persistent-instructions rule that one-off transcripts are never stored.

## Technical reasoning

### Echo before dispatch so the user can correct bad terms

The echo is sent after transcription and before the application agent receives the message, so a misheard name, number, or technical term is visible while the agent is still working and the user can correct it in the same conversation. Sending the echo after the response would arrive too late to influence that turn, and keeping transcripts only in the agent prompt would hide the one artifact the user is best placed to judge. Both transports build the echo list in their preparation step (`prepareMessage` on Telegram, `prepareMessageAndEchoes` on WhatsApp) and deliver it in the inbound worker immediately before the handler call.

### Literal rendering because transcripts are user content

A transcript is speech, not authored Markdown. Passing it through the ordinary `TelegramHTML` renderer would let a spoken "asterisk asterisk" or an underscore pair become formatting, and could restyle or mangle what the user actually said. `TelegramPlainChunks` escapes the input first, so provider HTML parsing cannot treat transcript characters as markup, and then delegates to the same chunker as agent replies with the identical limits: 4096 parsed-visible code points per message, 32768 per reply, at most eight messages, entity and grapheme boundaries, and the deterministic truncation marker. On WhatsApp, `sendPlain` bypasses the `markdownfmt.WhatsApp` conversion, but the text path cannot suppress client-side rendering of matched formatting pairs; the limitation is documented rather than claimed fixed.

### Non-fatal delivery and no transcript logging

The echo loop discards the send error on both transports. A provider failure to deliver the echo must not cancel or delay the agent turn, and the tests pin that behavior, including that the log writer does not receive the transcript text. Authorization is still reapplied at the send boundary, so a revocation during transcription fails the echo closed without a provider call.

## Impact assessment

- New outbound family. Telegram and WhatsApp now have an explicit outbound path beyond agent responses, welcome messages, localized error responses, and validated attachment directives. The root contract, the internal runtime contract, both channel DOX files, and the documentation rule in `docs/AGENTS.md` name it so the final-only rule stays precise for agent responses.
- Default on. A workspace that omits the key decodes to `true`, matching the loader default and the root statement, so transcribed audio appears in the chat unless the user disables `speech.transcript_echo`. No migration or schema change is involved, and the setting applies live through the ordinary replacement-adapter path.
- Privacy boundary unchanged. The echo carries the transcript the agent already receives. It adds no provider, no storage, and no logging of transcript content, and the existing speech provider and secret contracts were left intact.
- WhatsApp limits documented: no quoted reply on the echo, and possible client-side formatting of matched pairs in the literal transcript.

## Validation steps

Implementation and review evidence recorded for this tree:

- `scripts/dev.sh test` passed, covering the full Go suite plus the nested Bubble Tea module tests.
- `go test -race -count=1 ./internal/channel/telegram ./internal/channel/whatsapp ./internal/markdown` passed. One earlier Telegram run hit the repository's known flaky webhook test; subsequent runs, including this pass's race run, were clean.
- `go vet ./...` was clean.
- `mkdir -p .tmp-bin && go build -o .tmp-bin/spynel ./cmd/spynel` succeeded and the disposable binary was removed.
- `scripts/smoke.sh` passed.
- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- Coverage over the executable delta: Telegram `prepareMessage` 90.5%, `send` and `sendPlain` 100%; WhatsApp `prepareMessageAndEchoes` 96.8% and `sendPlain` 100%; markdown `TelegramPlainChunks` 100%.
- Quality gate verdict: PASS after one FAIL cycle. The first cycle found two WhatsApp test gaps; a tests-only correction closed both, and no production change was required.

Documentation pass checks run for this record on this tree:

- `scripts/dev.sh dox` reported DOX coverage valid for 51 tracked directories.
- `go test -count=1 ./internal/config ./internal/channel/telegram ./internal/channel/whatsapp ./internal/markdown ./internal/agentdocs` passed (config 0.022s, telegram 0.482s, whatsapp 0.147s, markdown 0.588s, agentdocs 0.006s).
- `go test -race -count=1 ./internal/channel/telegram ./internal/channel/whatsapp ./internal/markdown` passed (telegram 1.830s, whatsapp 1.338s, markdown 4.349s).
- A coverage re-check through `go test -coverprofile` and `go tool cover -func` reproduced the function coverage above.
- `git diff --check` reported no whitespace errors.
- A structural Markdown scan of the new record and every added line across the changed files confirmed one H1 per file, uninterrupted heading progression, blank lines around blocks, no em dash, no trailing whitespace, and a single trailing newline.

## Release status

The user requested a patch release for this feature. It is pending at trace-writing time: no tag, GitHub Release, npm publication, deployment, or release validation has been performed, and none is claimed here. Every changed and new file remains unstaged in the working tree.

## References

- [Root contract](../../../AGENTS.md)
- [Documentation DOX](../../../docs/AGENTS.md)
- [Configuration](../../../docs/configuration.md)
- [Configuration application matrix](../../../docs/configuration-live-matrix.md)
- [Communication integrations](../../../docs/integrations.md)
- [Troubleshooting](../../../docs/troubleshooting.md)
- [Internal runtime DOX](../../../internal/AGENTS.md)
- [Telegram channel DOX](../../../internal/channel/telegram/AGENTS.md)
- [WhatsApp channel DOX](../../../internal/channel/whatsapp/AGENTS.md)
- [Configuration DOX](../../../internal/config/AGENTS.md)
- [Markdown rendering DOX](../../../internal/markdown/AGENTS.md)
- [ElevenLabs speech transcription record](2026-09-22-elevenlabs-speech-transcription.md)
