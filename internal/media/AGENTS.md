# Media DOX

## Purpose

- Own bounded attachment storage and outbound directives plus speech decoding, provider selection, model acquisition, and transcription.

## Local Contracts

- The pinned native runtime initializes before Go. On Linux ARM64, a native ELF `fwrite` interposition filters only the exact three-chunk zero-vendor startup warning, passes changed/unrelated diagnostics through, and disables filtering in the executable constructor. Do not redirect stderr, alter CPU detection, or mutate shared module-cache libraries. Keep native constructor and real recognizer-failure subprocess checks; remove this workaround when the pinned runtime no longer emits the warning.
- Stream media under configured limits into private files, revalidate opened outbound files against symlink, type, readability, and concurrent-growth constraints, and keep ordinary links inert.
- Transcribe through the provider-neutral `Transcriber` contract and `TranscriptionRequest` carrying the attachment path plus the transport-declared duration. Keep the exact marker constructors centralized: `[Speech transcription is disabled; inspect the attached audio manually]`, `[Speech transcription failed; inspect the attached audio manually: ...]`, and `[Generated speech transcription; may contain errors]`. Transports render outcomes through these constructors, failure details are sanitized to one bounded marker-safe line, and session-label derivation skips these markers plus the legacy voice prefixes.
- The ElevenLabs client posts the stored attachment to the fixed `https://api.elevenlabs.io/v1/speech-to-text` endpoint with the `xi-api-key` header, refuses redirects, streams multipart from the bounded file descriptor, serializes one upload process-wide, and keeps one attempt plus its optional retry inside a 15-minute deadline that a shorter caller deadline still bounds. Enforce `speech.max_file_mb` from the descriptor and `speech.max_duration_seconds` from the declared duration before any request, and fail closed on a missing or malformed duration. Parse only the exact JSON `text` field, fail on empty or oversized transcripts, bound response bodies at 8 MB and error bodies at 64 KB, and reduce provider failures to one sanitized line that strips control and format characters and neutralizes square brackets. Retry exactly once only for HTTP 429 with provider code `rate_limit_exceeded` and a valid `Retry-After` of at most 60 seconds, and never retry other 4xx responses, 5xx responses, timeouts, or cancellations. Report `ErrSpeechAPIKeyMissing` only when the effective key is missing or blank, meaning the stored `speech.elevenlabs_api_key` and the configured environment variable both resolve empty; that sentinel is the sole substitution point: `NewFallback` runs the local Parakeet backend for that call and returns its transcript or error unchanged, while invalid keys, rate limits, timeouts, network errors, empty transcripts, and every other provider outcome pass through without consulting the local backend or model cache. Resolve the effective key from one configuration snapshot per transcription, trimmed stored key first and trimmed environment value second, and never log or expose either value.
- Parakeet decodes supported audio to mono 16 kHz float PCM and serializes transcription through one process-wide worker; preserve originals and make failures visible without invoking Python, FFmpeg, or external ASR tools.
- Coordinate Parakeet model cache installation across processes, enforce pinned size/hash/archive safety and compatibility markers, and atomically publish only complete private model directories.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [miniaudio/AGENTS.md](miniaudio/AGENTS.md) | Pinned miniaudio decoder bridge and license. |
