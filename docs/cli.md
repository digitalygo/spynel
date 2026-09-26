# Plain CLI and automation

Spynel is a classic, non-AI coordination program; coding harnesses provide its external intelligence. The plain CLI exposes Spynel's non-visual control plane without opening the full-screen TUI. It uses the same application service, durable histories, harness sessions, configuration save/reload boundary, jobs, logs, and trusted extension hooks as Telegram, WhatsApp, and the TUI.

## Offline documentation

```bash
spynel docs
spynel docs commands
spynel docs jobs page 1
spynel docs search "primary election"
spynel docs search "notify" page 1 --format json
```

`docs` is embedded and deterministic: it does not load `.spynel/config.yaml`, join a primary server, read workspace state, or invoke a harness. The index, topic sections, and search results use stable topic/section references. Ordinary documents render whole. Oversized output is split only between complete records using a conservative estimate of one token per three Unicode runes and a 10,000-token page budget; split pages report that budget and their estimated tokens. A separate 128-entry, 64 KiB, and 48 Ki-rune ceiling prevents pathological output. Plain output is Markdown without ANSI/control sequences, including when redirected. `--format json` emits the versioned `spynel.docs/v1` document with kind, IDs, title, content, related references, and page metadata. Unknown topics, invalid pages, and oversized input return actionable structured errors; close misspellings suggest a valid topic. Flags may appear before or after the topic/query.

There is no `spynel instructions` command. Legacy `.spynel/instructions/` role files left by early releases stay byte-for-byte untouched and inert: Spynel never creates, inspects, validates, renders, or loads them, and no harness prompt receives their contents.

Static topics describe user commands, workflow contracts, and implementation architecture. They label runtime-only subjects such as jobs and logs and never represent live values. Use `spynel status`, `jobs`, and `logs`, plus durable history files, for current state.

`/log`, its page ranges, and case-insensitive search read the same retained newest-4,096-entry view after restart. `/log page <start>-<end>` accepts any positive ascending range and clamps the requested end to the oldest available retained page before rendering. Private JSONL session files live under `.spynel/runtime/logs`, rotate at 2 MiB, and retain at most eight files. `/log clear` removes both the active view and those retained files. Stored entries are attributed and bounded, with terminal controls and common credential forms removed before persistence.

Runtime logs contain application diagnostics; per-job archives contain the ordered provider-neutral event stream for one numbered job generation. `/log clear` does not delete job archives. `/cleanup [days]` owns their age-based retention. A failed Telegram final text send appears in `/log` content-free, and its terminal reply or error record is retried from the private `.spynel/runtime/telegram-replies/` queue on a bounded schedule for up to one hour before expiry. The complete response stays in `/job output <number>` and history, a failed delivery never fails the job, and an expired reply is never retroactively resent.

## Proactive notifications

`spynel notify --workdir /absolute/workspace (--origin CHANNEL/CONVERSATION | --recent-authorized) --message "message"` queues a complete assistant message without invoking a coding harness. Supported explicit origins are `telegram/TG-<user-id>`, `telegram/TG-<user-id>-topic-<thread-id>`, `telegram/TG-group-<chat-id>`, `telegram/TG-group-<chat-id>-topic-<thread-id>`, `whatsapp/WA-<number>`, `whatsapp/WA-group-<group-id>`, `tui/<conversation>`, and `cli/<conversation>`. A private threaded chat or forum topic is addressed with the matching `-topic-<thread-id>` form; an absent thread and the General topic (thread ID 1) use the base origin, and thread IDs never grant authorization by themselves. `--workdir` selects that workspace's canonical `.spynel/config.yaml`; `--config` and positional message text remain available for operator compatibility. The shared enqueue boundary removes ANSI/ECMA-48 sequences, terminal control strings and replies, and other unsafe C0/C1 controls while preserving tabs, newlines, Unicode, and Markdown; it rejects input left empty by normalization before delivery or history append. The origin must already exist in durable history and remote origins must still satisfy the current allow-list/group policy. Success prints `queued notification <id>`; disconnected remote delivery remains in `.spynel/runtime/outbox/` for ordinary retry.

```bash
spynel notify --workdir /absolute/workspace --origin telegram/TG-42-topic-7 --message "Build finished"
spynel notify --workdir /absolute/workspace --origin telegram/TG-group--1001234567890-topic-9 --message "Review requested"
```

`spynel notify --recent-authorized --message TEXT` is the explicit alternative to `--origin`; the two modes are mutually exclusive. The application resolves the most recent user-active conversation, revalidates current authorization, queues through the same durable outbox, and returns no recipient/history data. Multiple authorized remote users, including separate Telegram and WhatsApp principals, fail closed because Spynel does not infer identity links; groups, tied recency, revocation, unknown conversations, and missing activity also fail closed. Pending outbox deliveries retry through the ordinary primary maintenance loop. This is a minimal resolver and delivery primitive with no reminder, scan, or agent policy, so consumers must persistently deduplicate visible effects by the durable event ID.

Pass command flags before positional arguments. For example, use `spynel conversations show --json telegram TG-42`, not flags after `TG-42`. Use `--` before message text that begins with a dash.

## Send messages

```bash
spynel send [--config PATH] [--conversation NAME] [--stream|--json] [--stdin] [--attach PATH]... [--] TEXT
```

The default conversation is `cli/local`; each other `--conversation` value owns an independent append-only history and harness session.

Accepted requests, commands and their text replies, and command/hook/harness errors are saved in that conversation's history. TUI `Err` messages for these failures survive restart and `/resume`; model and harness selection confirmations are saved too. Subsequent agent prompts seed these same records within the configured history limits, except that Pi receives only the raw current user message and a retained provider session receives the current entry alone; the current user message is always delivered, and zero limits disable only prior-history seeding. Secret command values remain redacted. `/clear` and normal retention cleanup still remove history explicitly; status indicators and form contents are not chat messages.

- Default output is only the last assistant-message item followed by a newline. Harness progress/preamble items remain in durable history but do not pollute final-only shell output.
- `--stream` writes every text delta as it arrives, including any provider progress items, and avoids repeating the complete final response.
- `--json` writes every `core.Event` as one NDJSON object. This already streams and cannot be combined with `--stream`.
- `--stdin` reads a multiline body of at most 512 KiB and cannot be combined with positional text. Use attachments for larger inputs.
- `--attach PATH` is repeatable. Each regular file is copied with the configured byte limit into `.spynel/attachments/cli/`, and an absolute `[Attachment name](<path>)` token is appended to the harness message. An attachment may be sent without text.

If an elected workspace server exists, `send` uses its private authenticated loopback service. This preserves one writer for the live harness, runtime jobs, logs, and sessions. Without an owner, the command builds a one-shot local service, waits for the response, and exits; it does not start Telegram, WhatsApp, or background services. Shared configuration and extension commands remain usable when the selected harness is missing. A `message.received` extension may also answer or reject an offline message before harness dispatch; otherwise a natural-language send reports the harness-availability error.

Examples:

```bash
spynel send --conversation deploy --stream "Check the release state"
printf '%s\n' 'Review these multiline notes.' 'Keep the answer brief.' | \
  spynel send --conversation review --stdin
spynel send --conversation review --attach ./trace.log "Diagnose this trace"
spynel send --conversation bot --json "Report current work" | jq -c .
```

## Follow up during an active turn

```bash
spynel followup [send flags] TEXT
```

`followup` targets the same `cli/<conversation>` key as `send`, but it is strict: the elected service rejects it before writing history unless that conversation currently has an active harness execution. Codex and Pi use native turn steering. Harnesses that declare queue semantics retain follow-ups in the same session; adjacent ordinary messages that accumulate before the next turn are combined in arrival order into one provider prompt. This makes a failed shell race visible instead of silently starting unrelated work.

When native steering transfers output ownership, the earlier waiting CLI process receives a terminal transport status and exits successfully; the follow-up process receives subsequent deltas and the final response. This is the same emitter handoff used to keep overlapping remote-channel typing state correct.

An ordinary second `send` still follows the shared channel behavior: it steers or queues when the conversation is active and starts a new turn when idle.

## Inspect and resume conversations

```bash
spynel conversations list [--config PATH] [--limit 1..1000] [--json]
spynel conversations show [--config PATH] [--tail 1..1000] [--chars 1..2097152] [--json] CHANNEL CONVERSATION
spynel conversations resume [--config PATH] [--json] CHANNEL CONVERSATION
```

`list` reads bounded previews from disk, newest first. Plain output is tab-separated; JSON output is an array with channel, conversation, update time, last role, preview, and path.

`show` reads only the newest bounded tail. Plain output includes timestamps and roles; JSON output contains metadata plus the history entries.

`resume` makes a point-in-time copy of any TUI, Telegram, WhatsApp, or CLI history into a new `cli/resume-<short-id>` conversation. It never rewinds or mutates the source. Plain output is only the new conversation name so it can be captured directly:

```bash
branch=$(spynel conversations resume telegram TG-42)
spynel send --conversation "$branch" "Continue this conversation from the saved context"
```

## Status and framework commands

```bash
spynel status [--config PATH] [--conversation NAME] [--json]
spynel command [--config PATH] [--conversation NAME] [--json] NAME [ARGUMENTS...]
```

`status` is a typed, non-secret snapshot rather than presentation Markdown. JSON keeps the title plus abbreviated caller/primary IDs, live-job and retained-log counts, Telegram/WhatsApp state, harness availability, model, reasoning effort, service mode, sandbox, the saved autostart preference, and active-turn state. Theme and conversation thread are not status fields.

`command` routes a non-visual slash command through the shared application handler. It joins the live owner when present; an offline framework command does not start a coding harness merely to read or mutate deterministic Spynel state. Common commands also have direct aliases:

`spynel config get <key>`, `spynel config set <key> <value>`, and `spynel config unset <key>` route the shared `/config` handler through that same boundary. Unset clears a clearable stored value through the validated save; clearing `speech.elevenlabs_api_key` restores environment resolution, while clearing an enabled Telegram token fails with the ordinary validation error. Secret values stay masked and are never echoed by get, set, or unset output.

`spynel jobs` lists live executions. `spynel jobs recent` lists up to 20 newest workspace-local archives by job number, while `spynel job info <number>` and `spynel job output <number> [tail <bytes>]` inspect bounded metadata or the newest captured event bytes. Numbers advance from 1 through 9999 and wrap to 1; after reuse, inspection selects the newest generation. The same slash commands work through authenticated channels and the terminal API because all use the shared application handler.

`/cleanup [days]` runs safe retention with a seven-day default; it uses strict whole-day validation and reports removed conversations, removed job archives and bytes, protected items, and failures. `/pi session`, `/pi name <name>`, `/pi compact [instructions]`, and `/pi import <full-session-id>` manage the Pi conversation session and are available only in the local TUI and canonical private Telegram conversations. The plain CLI, WhatsApp, groups, forum topics, malformed routes, and other channels refuse before any session capability call and never receive a session ID or path. `/pi session` reports the full session ID and a shell-safe `pi --fork <absolute-session-file>` command, `/pi name` sets a 1-to-128-code-point single-line name on an existing session and force-renames a private Telegram topic even when Spynel already renamed it, `/pi compact` compacts the existing idle session with optional instructions of at most 4096 Unicode code points, and `/pi import` forks one validated direct Pi session into `.spynel/runtime/pi-sessions` without modifying the source. A first ordinary message that creates a Pi session also receives a best-effort 32-code-point label derived from the accepted user content and triggers one automatic private Telegram topic rename that the adapter applies at most once per conversation and process, so a new thread is not left as "New chat"; existing provider names and later user title edits are preserved, and naming failures never affect the turn or reply. On the allowed surfaces, the first successful non-continuing final response after a prompt that creates or rotates a session begins with the full session ID; that display-only notice stays out of durable history.

```bash
spynel jobs
spynel jobs recent
spynel job info 3
spynel job output 3 tail 8192
spynel job kill 3
spynel log page 2
spynel log search webhook
spynel stop --conversation deploy
spynel clear --conversation deploy
spynel harness codex
spynel model gpt-5
spynel telegram on
spynel whatsapp off
spynel update
spynel update check
spynel killall
spynel config set workspace.history_max_messages 40
spynel config unset speech.elevenlabs_api_key
spynel command help commands
```

Options such as `--config`, `--conversation`, and `--json` must precede alias arguments (`spynel log --json search webhook`). Prefer typed `spynel status --json` over `spynel command --json status`, whose NDJSON response follows the generic event contract.

`spynel jobs` renders the same global workspace snapshot as authenticated Telegram, WhatsApp, TUI, and shared application/API callers. Every admitted execution receives one workspace-local number from 1 through 9999 that remains its user-facing reference while live, recovering, completed, and archived; the counter wraps from 9999 to 1, and numeric lookup selects the newest generation after reuse. Ordinary active executions use two logical rows: emphasized job number plus a generic live-conversation label, then compact lifetime, cumulative provider steps, canonical execution status, and provider channel. Conversation IDs and live prompt text are omitted. `spynel job info NUMBER` exposes the same bounded live or archived evidence without prompts, origins, or transcripts; `spynel job output NUMBER` reads the newest captured provider events while a job runs or after it ends; `spynel job kill NUMBER` requests provider-level interruption of the exact owning harness session and reports clearly when the job already finished or is not running. Every authenticated workspace interface sees and controls the same live jobs. Conversation identity, creation channel, and `notify.origin` do not restrict access, and archive paths cannot escape the active workspace.

The former `spynel task`, `spynel goal`, `spynel tasks`, `spynel goals`, and `spynel run` helpers are not part of the current CLI, and the corresponding slash commands (`/task`, `/goal`, `/tasks`, `/goals`, `/trigger`) are no longer framework commands. A retired or otherwise unrecognized slash input is not categorically rejected: when the active harness accepts native conversation input, the raw spelling and arguments reach Pi's own prompt pipeline, while a harness without that capability replies with the ordinary `Unknown command` message. Existing workspace task, goal, prompt, and instruction files stay untouched and inert.

TUI-only visual or ownership operations, including theme preview, title, welcome, resume screen, primary-window promotion, and quit, are intentionally rejected by `command`. Conversation resume has the disk-backed command above. `spynel whatsapp pair` remains the plain QR-pairing command.

`spynel update` checks, updates and restarts every instance of its own managed installation across workspaces. It is independent of the current workspace or primary. `/update` through a channel selects that primary's installation. `spynel update check` only reports versions; `spynel update --json check` emits structured version state, while `spynel update --json` emits a terminal update acknowledgment and reports failures through its exit status. `spynel killall` stops all verified Spynel processes the caller can control, including other installations. Matching autostart services are stopped while their future registrations and workspace data remain intact. See [installation and updates](getting-started.md#updates) for older-instance handling and per-user scope.

## Extensions

Installed extensions are trusted executable integrations. When enabled, their `message.received`, `harness.before`, and `harness.after` hooks run for CLI messages exactly as they do for interactive channels. Hooks receive and return bounded JSON over standard streams and execute with Spynel's operating-system authority. Review repositories before installing them. See [Extensions and hooks](extensions.md).

```bash
spynel extension list
spynel extension install https://github.com/example/spynel-hooks.git
spynel extension remove spynel-hooks
```

## Exit status and cancellation

Argument, configuration, inactive-follow-up, hook, transport, and harness failures return an error and a nonzero process exit through the executable entry point. Ctrl+C or SIGTERM cancels the caller context; a loopback request disconnect does not impose a shorter provider timeout. Complete histories remain disk-backed even though every list/show/input operation is bounded.
