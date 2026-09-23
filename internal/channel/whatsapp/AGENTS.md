# WhatsApp Channel DOX

## Purpose

- Own WhatsApp device pairing, live authorization, inbound/outbound messages, attachments, activity, and proactive delivery.

## Local Contracts

- Canonicalize phone-number authorization to international digits, resolve self-chat LIDs before authorization and conversation keying, and re-check the live allow-list at every pairing, event, provider, and delivery boundary.
- Distinguish socket pairing readiness from an authenticated connected device; expose only validated phone-number identities and retain QR data solely in the fullscreen pairing flow.
- Stream bounded encrypted media and deliver only the final response/error plus validated directives. When the default-on live `speech.transcript_echo` is enabled, each successful transcription also echoes once to the same chat as a standalone pre-dispatch message that contains only the shared generated-transcript block; the echo is not an agent response, and its delivery failure never affects the turn. The text send path has no quoted-reply support, so the echo does not reference the originating audio message, and WhatsApp clients may still render matched formatting pairs in the literal transcript because that client-side behavior cannot be disabled by the sender. Translate the canonical communication-agent activity lifecycle into composing for ordinary and proactively routed recovery turns, keep framework-only and eventless intake silent, and pause before terminal delivery.
- Carry the authenticated chat/stanza tuple as private stable source correlation through application acceptance so duplicate transport events cannot duplicate work.

## Child DOX Index

No child DOX files.
