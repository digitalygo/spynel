# Telegram Channel DOX

## Purpose

- Own Telegram polling/webhook intake, live authorization, media handling, activity, identity mapping, and outbound delivery.

## Local Contracts

- Re-resolve and validate the current allow-list at startup and every inbound/outbound/provider boundary; stale identity mappings or credentials never grant access, and authorization loss terminates useful traffic.
- Persist only minimal identity learned from an authenticated private update, keep webhook secrets and URLs out of status, and permit only teardown `deleteWebhook` after revocation.
- Stream bounded media privately, deliver only the final response/error plus validated directives, and keep typing references per route, best-effort, serialized, and bounded. Translate proactively routed recovery activity through the same refresher and stop it before recovered terminal delivery.
- Build and parse routes only through the strict canonical conversation grammar: `TG-<positive-user-id>`, `TG-<positive-user-id>-topic-<positive-thread-id>`, `TG-group-<negative-chat-id>`, and `TG-group-<negative-chat-id>-topic-<positive-thread-id>`. Non-canonical origins fail closed. Absent threads and the General topic (ID 1) canonicalize to the base conversation and outbound thread ID 0, preserving existing identities; thread IDs of at least 2 select an independent durable conversation and harness session.
- Reapply the current allow-list, verified username mapping, and `group_mode` policy immediately before every route-bound provider call and again before the single bounded rate-limit retry. Private topics authorize through their base numeric user, group topics through the current group policy, and thread IDs never grant authorization on their own.
- Keep every outbound family in its originating route: replies, terminal errors, typing actions, welcome messages, native photo/document attachments, proactive events, and recovery delivery carry the route's `message_thread_id` when it addresses a topic, and base or General-topic delivery omits it.
- Deliver replies as independently valid `sendMessage` HTML chunks produced by the markdown chunker: at most 4096 parsed-visible Unicode code points per message and 32768 parsed-visible code points across at most eight messages per reply. Formatting stays balanced, an entity spelling or Unicode code point is never split, grapheme boundaries are preferred while the remaining reply budget allows, and an over-budget reply ends with the deterministic visible truncation marker while complete durable history stays intact. Retry one rejected HTML chunk once as plain text only for a provider entity/HTML parse rejection; honor one provider 429 with a usable `retry_after` up to 60 seconds and otherwise fail without waiting or looping.
- Carry the authenticated chat/message tuple as private stable source correlation through application acceptance so polling/webhook redelivery cannot duplicate work.
- Track private-topic title state as a bounded in-memory tri-state (unknown, implicit, explicit) learned only from allow-list-authenticated `forum_topic_created` and `forum_topic_edited` service messages, where `is_name_implicit` marks a still-implicit created title and a named edit marks the title user-owned. A restart starts every topic unknown, so the automatic rename path fails closed until the transport reports the topic again; explicit or unknown titles are silent no-ops for implicit-only renames, and explicit user titles are never overwritten automatically.
- `RenameConversation` renames one private topic through `editForumTopic` after parsing the canonical route and rejecting base chats, groups, malformed routes, and invalid labels, and every provider call plus its single bounded retry reapplies live authorization. A `TOPIC_NOT_MODIFIED` 400 counts as success because it proves the requested title is live, any other provider error propagates, and a successful rename marks the topic explicit.

## Child DOX Index

No child DOX files.
