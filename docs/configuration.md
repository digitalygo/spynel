# Configuration

`.spynel/config.yaml` is the user-editable project configuration inside the fixed private `.spynel` state directory. Spynel searches parent directories for that path, while relative configuration paths resolve from the workspace root one directory above `.spynel`, not from the process's current directory. On bare interactive startup only, an uninitialized launch directory with an initialized ancestor produces a pre-startup choice to use that parent, initialize locally, or exit. The parent choice is the default and changes the process working directory to the selected root before election; explicit config targets, server mode, and automation keep ordinary deterministic discovery without prompting. Settings that do not exist in the current schema are ignored on load without rewriting the file, and the next canonical save writes only current keys. Retired inputs such as `channels.tui.enabled`, `harness.reviews`, `harness.chat_agent_prefix`, `harness.developer_agent_prefix`, `harness.reviewer_agent_prefix`, `harness.heartbeat_agent_prefix`, and every `orchestrator.*` key are ordinary unused keys under that rule.

Spynel no longer runs Markdown task or goal workflows, review or recovery loops, a semantic heartbeat, a notification agent, or autonomous unresponded-message scans. Existing `.spynel/tasks`, `.spynel/goals`, `.spynel/prompts`, and `.spynel/instructions` content remains untouched and inert on disk; Spynel never creates, inspects, renders, or loads it, there is no `spynel instructions` command, and no legacy instruction file reaches a harness prompt. Harness dispatch adds no framework, dispatcher, or role prompt sections: Pi receives the raw accepted user text, and only Telegram Pi RPC sessions load the embedded final-replies note extension additively with `--extension`, whose `before_agent_start` writes one namespaced `spynel_telegram` system-prompt section, while every other harness receives the bounded provider-neutral history context and, on Telegram and WhatsApp, the outbound-attachment directive.

## One setting catalog, two interfaces

The [configuration application matrix](configuration-live-matrix.md) inventories every exposed setting, its runtime owner, save/reload behavior, and verification. Extensions are the only restart-bound entries.

The TUI renders the scalar settings catalog as form controls. The same keys are available as text commands, which is the common denominator for Telegram, WhatsApp, scripts, and terminals without form support:

```text
/config
/config get harness.name
/config set harness.name claude-code
/config set harness.sandbox danger-full-access
/config set workspace.cleanup_retention_days 30
/config unset speech.elevenlabs_api_key
/telegram get allowed_users
/telegram set allowed_users 123456789,trusted_username
/telegram on
/whatsapp set mode dedicated
/whatsapp off
/theme catppuccin-latte
```

Within text and secret fields, Space inserts text, Left/Right moves the cursor, Home/End jumps to either edge, and Backspace/Delete edits at the cursor. Those keys continue to cycle values when a toggle or select control is focused.

In `/telegram` and `/whatsapp`, short keys are scoped automatically: `allowed_users` becomes `channels.telegram.allowed_users`. `/config` expects the complete key. Lists use comma-separated values, and Boolean values accept `on` and `off`.

Top-level channel forms open at a live status section showing whether the transport is not configured, connecting, connected, or in error, followed by any available connection or error detail. Once Telegram connects, this section also shows its `@username` as a clickable `t.me` link that opens the bot conversation. Channel forms then provide a filled **Setup wizard** action. Wizard steps do not repeat the live connection-status section: they begin directly with their setup title, tabs, instructions, and controls. Telegram guides the user from the official [BotFather](https://t.me/BotFather) through bot creation, secret-token entry, access, and enabling. WhatsApp asks only for account mode and required access numbers; continuing from access atomically saves those essentials, enables the channel in the background, and opens pairing with [WhatsApp's official device-linking help](https://faq.whatsapp.com/1317564962315842). There is no separate WhatsApp enable-choice step. **Show QR** gives the code the complete terminal with no header, footer, help text, or form controls; any key returns to the wizard. **Retry pairing** discards an expired session and starts a fresh one. **Use pairing code** accepts the linked account's international phone number and generates WhatsApp's short-lived code for **Linked devices → Link a device → Link with phone number instead**, as described in [WhatsApp's phone-linking help](https://faq.whatsapp.com/1324084875126592). Named wizard tabs use bold labels above a continuous baseline, with the active tab highlighted only by the segment directly beneath its label. Back/Continue carries unsaved wizard values in process memory, Cancel returns without saving, and the completion transition saves all essential values as one validated transaction. WhatsApp must save and start the channel before pairing details can be produced.

WhatsApp pairing timeouts and terminal pairing errors automatically start a fresh session after a short delay. **Retry pairing** remains available only to refresh immediately; recovery never requires pressing it.

On first use, bare TUI `/telegram` and `/whatsapp` open their setup wizard directly instead of showing an empty status/config form. Telegram is considered configured after it has a resolved token and at least one allowed user. WhatsApp is considered configured only after at least one allowed phone number is saved; an enabled flag or existing session database does not bypass first-time setup by itself. After that, the ordinary form places **Enabled** inside the live status section, above all configuration. **Setup** contains the wizard and **Basic settings** contains the remaining connection essentials, with one blank row between ordinary settings and two blank rows before each section rule that follows content. Both channel forms combine their page title and status heading into a single **Telegram Status** or **WhatsApp Status** rule and omit the extra descriptive subtitle. Advanced controls start collapsed behind one combined heading and disclosure rule: select **Show Advanced Settings ↵** to reveal optional controls, and the same rule changes to **Hide Advanced Settings ↵** while expanded. Edited advanced values remain part of the same atomic Ctrl+S save even if the section is collapsed again. In `/config`, `/telegram`, and `/whatsapp`, Ctrl+S closes the form and restores chat after a successful atomic save; an unchanged form also closes as a no-op, while a persistence error keeps its edits visible. Escape closes an unchanged form directly. If any field changed, Escape instead opens a centered modal with **Save**, **Discard**, and **Keep editing** in that order and keeps **Keep editing** selected safely by default. Save follows the same validated persistence path as Ctrl+S, Discard exits without saving, and Escape returns to the edited form. Ctrl+C bypasses the modal, parent navigation, and dirty-change prompt and returns directly to main chat, discarding unsaved in-memory edits. Muted `←→ nav`, `␠/↵ choose`, and `␛ cancel` hints sit in the lower border separated by rule segments, with one blank interior row above them. Main `/config` omits redundant introductory copy and begins directly with its **Core settings** rule.

Configuration is parsed into typed values and validated before saving. Spynel atomically replaces the private `0600` YAML file, reloads that canonical file into the running process's shared configuration before the save returns, and publishes the refreshed snapshot to runtime owners. Subsequent operations use the reloaded values without restarting; active channels and in-flight work are preserved while cached owners reconcile the new snapshot. Telegram and WhatsApp cannot be enabled without a valid canonical sender allow-list, and attempts to clear or replace an enabled transport's list with whitespace-only, punctuation-only, malformed, or normalization-empty entries are rejected. Each adapter separately resolves and validates the live list at runtime, so direct construction or bypassed configuration validation cannot start or use a permissive transport. Secret values are masked in forms/replies and redacted from durable command history. Stored secrets such as `speech.elevenlabs_api_key` live in that private file as plaintext, not ciphertext, and `/config unset <key>` or `spynel config unset <key>` clears a clearable value through the same validated transaction. Clearing can reject with the ordinary validation error, for example when an enabled Telegram token would become empty. Prefer the local TUI or CLI for entering secrets: a CLI positional value also reaches shell history and the process argument list, and a value set from Telegram or WhatsApp transits that provider's servers.

Telegram cannot mutate Telegram from a Telegram message, and WhatsApp cannot mutate WhatsApp from a WhatsApp message. Use the TUI or the other remote channel. Non-own channel changes apply live through an isolated transport supervisor.

## Workspace

- `history_max_messages`: maximum newest entries seeded into a harness prompt when the provider session does not already retain the conversation. `0` disables prior-history seeding; the current user message is always delivered.
- `history_char_limit`: maximum total characters for the seeded prior history; a positive value also bounds the current user entry. `0` disables prior-history seeding and leaves the current entry complete.
- `attachment_max_mb`: maximum inbound or agent-sent Telegram/WhatsApp attachment size.
- `cleanup_retention_days`: age in whole days before the primary's automatic conversation and job cleanup. It defaults to `30` and applies live; manual `/cleanup [days]` uses its own explicit value.

The private runtime root is always `.spynel`; it is not a configuration setting. Configuration, histories, attachments, themes, installed extensions, credentials, leases, and other runtime state live together beneath that directory.

Prompt context is read backward from append-only JSONL and stops at both history limits. Pi ordinary conversations take the native path, so the prompt carries only the raw current user message because the Pi session retains the conversation and manages its own context. Every other harness receives the bounded provider-neutral seed unless the optional retained-conversation capability reports an existing provider session, in which case only the current entry is delivered; on that path, a fresh session, a missing or non-regular session file, a closed or unavailable harness, a different harness, or any uncertain provider state keeps the bounded seed. The current user message is always delivered, and zero limits disable only prior-history seeding. The TUI uses a separate fixed display tail, `/resume` discovers only metadata plus one preview entry per conversation, and branching copies on disk. An old conversation therefore does not have to live in RAM.

Telegram and WhatsApp replies store a compact same-message reference as `reply_to`: the native referenced-message ID and, when supplied by the inbound event, up to 100 normalized Unicode characters of referenced text or caption. Recent prompt history labels it explicitly as `[reply_to: ...]`. The complete label and native ID take priority when the character limit requires truncation; a limit too small for that identity omits the affected entry instead of exposing a partial ID. Ordinary non-replies omit the field.

`.spynel/attachments/` holds TUI attachments; remote media uses `.spynel/attachments/telegram/` and `.spynel/attachments/whatsapp/`. Treat all three as conversation data.

## Coding harness (`harness`)

- `name`: `codex`, `claude-code`, `agent-zero`, `pi`, an ACP alias (`opencode`, `qwen-code`, `kimi`, `goose`, `cursor`, `gemini-cli`, `github-copilot`, or `factory-droid`), or custom `acp`.
- `model`: optional model override; empty selects the harness default.
- `reasoning_effort`: optional model-specific effort/thinking level; explicit empty or `inherit` uses the model/harness default. For compatibility, a configuration created before this key existed and still omitting it retains Spynel's historical `medium` runtime effort.
- `service_mode`: optional provider service/speed tier; omitted or `inherit` uses the provider default.
- `sandbox`: `danger-full-access`, `workspace-write`, or `read-only`. The default is `danger-full-access`, which removes Codex workspace confinement.
- `acp_command`: executable name or absolute path used only by custom `acp`.
- `acp_args`: shell-free YAML string list used only by custom `acp`; settings display and accept familiar one-line command-line text.

Spynel detects installed built-ins from `PATH` and the conventional per-user `.local/bin`. The first choices are Codex, Claude Code, and Agent Zero CLI, followed by the remaining catalog order. Native adapters use Codex app-server, Claude Code print mode, or Pi JSONL RPC. Agent Zero CLI is the fixed `agent-zero` profile: Spynel reports it available only when the installed `a0` executable passes `a0 acp --check`, then launches `a0 acp` through the shared adapter. This rejects pre-ACP A0 CLI builds instead of treating any `a0` executable as usable. The CLI retains its normal Agent Zero host discovery and authentication configuration. Other ACP aliases supply their documented executable and arguments; custom ACP is the sole manual-command exception. Stable ACP v1 uses newline-delimited JSON-RPC over stdio, not an HTTP endpoint, and arguments are passed directly without a shell.

Pi is not resource-filtered by Spynel. The `pi` profile runs `pi --mode rpc` against the user's ordinary Pi installation, so global extensions and their custom tools and providers, skills, prompt templates, themes, context files, and model/provider settings all load normally. Its one added resource is the embedded Telegram final-replies note extension, materialized as a private `0600` regular file under the workspace `.spynel/runtime` directory and passed additively with one `--extension` argument only for `chat:telegram:` conversation processes, so global and trusted-project `SYSTEM.md` and `APPEND_SYSTEM.md` discovery and every ordinary resource stay intact; its `before_agent_start` writes one namespaced `spynel_telegram` system-prompt section, and the note is guidance, not enforcement, because a user extension that returns a forced prompt can override it while extension-triggered model runs may bypass `before_agent_start`. Spynel passes no resource-suppression flag and no approval override and keeps no extension allowlist; project-local Pi resources follow Pi's own non-interactive trust decision, where RPC shows no trust prompt and saved decisions plus Pi's `defaultProjectTrust` setting apply. Pi resources belong to the user's Pi configuration and remain distinct from Spynel executable hooks, which run only from repositories explicitly installed under `extensions.directory`. Because trusted Pi extensions can request interactive UI, a blocking `extension_ui_request` dialog (`confirm`, `select`, `input`, or `editor`) receives an immediate explicit cancellation and one visible status event, while fire-and-forget UI requests are ignored so a headless turn cannot stall.

All harness scalar settings live under the YAML `harness:` section and in the shared settings catalog's harness group. Model, reasoning-effort, and service-mode changes validate and persist even while a harness turn is active. The active turn keeps the complete inference selection captured when it was dispatched; the next provider dispatch ordered after the atomic configuration commit, including a queued continuation, uses the new selection. Harness executable, custom command/arguments, and sandbox changes retain their transactional idle-session rules.

The capability mapping is deliberately asymmetric and provider-backed:

| Harness profile | Reasoning effort | Speed/service mode | Applied mechanism |
| --- | --- | --- | --- |
| `codex` | Detected per-model values from app-server `model/list`, or a manual value | Exact per-model `serviceTiers`, including `fast` only when advertised | app-server `turn/start` `effort` and `serviceTier` |
| `claude-code` | Detected `low`, `medium`, `high`, `xhigh`, `max` where available, or a manual value | Unsupported | `claude --effort` |
| `pi` | Exact per-model values from RPC `get_available_thinking_levels`, queried in a separate `--no-session --model <id>` process so inspection never changes Pi's saved default; `get_state` identifies the current/default model and its current thinking level because the catalog has no default metadata; other models receive no guessed default label; the sole `off` response offers no detected choices; manual values remain available | Unsupported | `pi --thinking` |
| `agent-zero`, `opencode`, `qwen-code`, `kimi`, `goose`, `cursor`, `gemini-cli`, `github-copilot`, `factory-droid`, custom `acp` | Unsupported by Spynel: optional ACP `thought_level` choices are unavailable until after session creation, too late for the shared pre-dispatch selector | Unsupported: ACP defines no standard speed category | No reasoning or speed option is sent; advertised model selection remains supported. A legacy omitted key may decode as `medium` for compatibility, but ACP ignores that sentinel. |

This mapping is based on the [Codex app-server model and turn schemas](https://developers.openai.com/codex/app-server/), [OpenAI service-tier semantics](https://developers.openai.com/api/reference/resources/responses/methods/create), [Claude Code CLI reference](https://docs.anthropic.com/en/docs/claude-code/cli-usage), [Pi's model/thinking interface](https://github.com/earendil-works/pi), and [ACP session config options](https://agentclientprotocol.com/announcements/session-config-options-stabilized). Unknown or too-late-to-discover capabilities are not inferred from another harness or presented as selectable.

Custom ACP arguments are passed directly without a shell:

```text
/config set harness.acp_command my-acp-agent
/config set harness.acp_args --stdio --profile "work profile"
/config set harness.name acp
```

The argument field splits on whitespace and supports single or double quotes, empty quoted arguments, and backslash escapes for whitespace, quotes, and backslashes. Other backslashes remain literal, so paths such as `C:\path\agent` work without escaping every separator. The field is one line; malformed quotes, dangling escapes, NUL bytes, and invalid UTF-8 are rejected without changing the saved setting. `$`, backticks, wildcards, pipes, redirection, semicolons, and parentheses remain literal argument data: Spynel parses this text itself and passes the resulting vector directly to the process without invoking a shell.

`harness.sandbox` is deliberately user-controlled. Codex and Claude receive their native policy mappings. Pi passes the `read,grep,find,ls` read-only tool allow-list, which Pi applies across built-in and extension tools. ACP automatically accepts one-time requests outside read-only mode and rejects edit/delete/move/execute/other requests in read-only mode; fetch also requires network permission, which Spynel does not currently grant. Pi and ACP agents are still trusted local processes, and ACP agents may act without requesting permission, so neither mapping is an operating-system containment boundary. Harness names and sandbox values must use the exact current catalog choices after case and edge-whitespace normalization.

`/harness` opens a TUI choice list with detected/install status or lists choices remotely; `/harness <name>` selects directly. Configure the custom ACP command before selecting `acp`. Selecting a missing built-in records the choice and gives installation guidance when no harness is running. An installed but pre-ACP Agent Zero CLI is reported as unsupported with update guidance. Once launched, connection, authentication, or Agent Zero reachability failures come from the CLI's ACP negotiation as actionable harness errors; Spynel does not persist a second host or credential configuration. `/model` opens/lists a catalog when the selected adapter provides one, and `/model <exact-name>` sets a custom identifier. In the TUI, model and reasoning-effort lists end with **Custom**, which opens a text field and a Continue button. Custom entry remains available when discovery fails or omits a value. Reasoning entry is available for Codex, Claude Code, and Pi; ACP still has no supported effort control. Choosing a model opens the applicable effort and service screens, then commits the complete selection at the final step; Escape before that final step leaves configuration unchanged and restores any unsaved parent configuration edits. `/effort [level|inherit]` and `/speed [mode|inherit]` inspect, set, or reset the same properties from every channel. `/config get|set harness.reasoning_effort` and `harness.service_mode` are the noninteractive equivalents. Explicit model and effort identifiers, including direct YAML edits, bypass discovery membership checks; the harness reports an error if it cannot use them. Model names must be valid UTF-8, contain no control characters, and fit within 1024 bytes; efforts must be valid UTF-8 identifiers without whitespace or controls and fit within 128 bytes. Service modes still require detected support. Changing models clears a now-invalid stored property to `inherit` unless an effort is explicitly supplied in the same transaction. Changing harnesses likewise clears stored properties known to be unsupported by the destination before final transaction validation, while an explicitly requested unsupported combination is rejected. Configurations that omit the newly introduced effort key retain the historical `medium` runtime value; both legacy and explicitly entered Pi efforts now reach Pi for validation or clamping, including when detection advertises only `off`. An explicit empty value or `inherit` opts into the provider default. Codex and Pi query runtime catalogs; Claude Code supplies aliases; ACP applies an exact identifier through a model-category session config option and reports clearly when the agent exposes none. Session policy changes start a fresh provider session while bounded Spynel history remains the context fallback.

A working harness is never replaced by an unavailable choice, and switching is allowed only while the current harness is idle. Each harness has its own persisted session map.

## TUI

- `channels.tui.title`: initial window title.
- `channels.tui.theme`: active palette name, default `spynel`.

Startup is invocation-driven and deterministic: bare `spynel` launches the TUI, `spynel serve` is headless, and `spynel serve --tui` attaches a TUI. There is no launch-preference setting.

`/title <name>` writes a private `.spynel/tui-title` override, updates the running TUI, and survives restart. Remove the override file to return to the configured title.

Theme files live in `.spynel/themes/*.yaml`. Each contains a safe `name`, a `description`, optional `appearance` (`light` or `dark`) and `color_blind_friendly` metadata, and every semantic color: `background`, `surface`, `surface_elevated`, `surface_selected`, `text`, `text_muted`, `primary`, `secondary`, `border`, `user`, `success`, `warning`, `error`, `info`, and `code`. Colors use `#RRGGBB`; incomplete or duplicate themes are rejected. Metadata remains optional so existing user themes continue to load.

Fresh workspaces contain exactly twelve editable stock files. The picker groups all six dark themes first: `spynel`, `hack-the-box`, `github-colorblind-dark` (accessible), `gruvbox-dark`, `nord`, and `okabe-ito-dark` (accessible). The six light themes follow: `gruvbox-light`, `rose-pine-dawn`, `tol-muted-light` (accessible), `catppuccin-latte`, `okabe-ito-light` (accessible), and `solarized-light`. The collection therefore has exactly six choices in each appearance group and exactly two explicitly color-blind-friendly choices in each group.

Palette foundations come directly from the [Nord palette](https://www.nordtheme.com/), [Gruvbox source palette](https://github.com/morhetz/gruvbox/blob/master/colors/gruvbox.vim), [Catppuccin palette data](https://github.com/catppuccin/palette/blob/main/palette.json), [Solarized specification](https://ethanschoonover.com/solarized/), [GitHub Primer primitives](https://github.com/primer/primitives), [Rosé Pine Dawn palette](https://rosepinetheme.com/palette/ingredients/), [Paul Tol's color-blind-safe muted scheme](https://sronpersonalpages.nl/~pault/), and the established [Okabe-Ito categorical palette](https://jfly.uni-koeln.de/color/). Spynel preserves each source's characteristic canvas and accents but darkens some foreground accents, strengthens layer/border separation, and maps categorical colors to semantic UI roles to satisfy measured TUI contrast. In particular, Rosé Pine's rose, pine, and iris accents are darkened for text use; Tol Muted uses a cool paper canvas and contrast-adjusted wine/indigo/green/ochre roles; and Okabe-Ito Light assigns error to purple and warning to ochre so severe red/green-deficiency simulations retain both hue and luminance separation.

An upgraded workspace with no theme files receives the revised built-ins immediately. If theme YAML already exists, Spynel treats every file as user-owned: `spynel init --force` adds missing revised stock templates but neither overwrites nor deletes old stock-named files. To obtain exactly the revised set, run that command, then archive unwanted older palette files outside `.spynel/themes`; keep any locally modified files you still want in the picker.

Bare `/theme` reloads the theme files, then opens an inline TUI list or prints the available names and descriptions in Telegram/WhatsApp. In the TUI, Up/Down (or Tab/Shift+Tab) temporarily previews the highlighted palette across the complete interface, Enter persists it, and Escape restores the palette that was active before browsing. `/theme <name>` also reloads and applies directly from every channel, including when the active file was edited without changing its name.

## Telegram

One bot is configured per workspace:

Essential controls:

- `token`: token embedded in the private workspace configuration; prefer `token_env`. Stored tokens are masked in replies and lists and redacted from history; clear one with `/telegram unset token` while Telegram is disabled.
- `allowed_users`: required before Telegram can be enabled; accepts positive numeric IDs or case-insensitive ASCII usernames made from letters, digits, and underscores, optionally prefixed with `@`. Invalid entries do not satisfy the runtime guard. To find your numeric ID, message the third-party [@userinfobot](https://t.me/userinfobot) helper.
- `enabled`: run the bot.

Advanced controls:

- `name`: friendly bot name.
- `token_env`: environment variable containing the token, default `SPYNEL_TELEGRAM_TOKEN`.
- `mode`: `polling` or `webhook`.
- `webhook_url`: public HTTPS base URL; Spynel appends a per-token path that is never shown in status output.
- `webhook_listen`: local HTTP listener behind the reverse proxy.
- `webhook_secret`: Telegram secret-token verification value; required when webhook mode is enabled.
- `poll_timeout_seconds`: long-poll timeout.
- `group_mode`: `mention`, `all`, or `off`.
- `welcome_enabled` and `welcome_message`: new-member greeting; `{name}` is replaced.
- `notify_messages`: put a compact incoming-message notice into the TUI/runtime log without recording the message body.
- `attachment_max_age_hours`: periodic Telegram attachment retention; `0` keeps files.

Webhook mode requires `webhook_url`, `webhook_listen`, and `webhook_secret` when enabled. The local server accepts bounded POST bodies, verifies the secret in constant time, and uses a bounded update queue.

## WhatsApp

Essential controls:

- `mode`: `self-chat` or `dedicated`.
- `allowed_numbers`: required normalized phone-number allow-list; formatting whitespace and punctuation are ignored, `00` and `+` international prefixes compare equivalently, while letters, normalization-empty entries, and values beyond 15 digits are invalid. Missing or invalid live authorization rejects every sender and prevents startup or pairing.
- `enabled`: run the client.

Advanced controls:

- `database`: private persistent multi-device SQLite store.
- `allow_groups`: permit addressed group messages.
- `poll_interval_seconds`: connection health-check interval, minimum two seconds.

When groups are enabled, the message must mention or reply to the linked account. `/whatsapp` offers a full-terminal QR, retry for expired sessions, and an official phone-number linking code; `spynel whatsapp pair` remains the non-TUI QR fallback.

## Speech

Speech covers Telegram and WhatsApp voice notes and audio files, including push-to-talk and ordinary audio. Video, documents (even audio-named ones), images, and stickers are never transcribed. The default provider is the ElevenLabs cloud API; `speech.provider: parakeet` keeps transcription local. The effective API key is the trimmed stored `speech.elevenlabs_api_key` when set, otherwise the trimmed value of the variable named by `speech.elevenlabs_api_key_env` (default `ELEVENLABS_API_KEY`). When both are missing or blank at transcription time, that call falls back to local Parakeet. That missing-key condition is the only fallback: a nonblank stored key is authoritative, and invalid keys, rate limits, timeouts, network errors, and empty transcripts surface as the ordinary failure marker.

Upgrade note: a pre-existing workspace that never wrote `speech.provider` adopts the `elevenlabs` default after upgrade. If a non-empty `ELEVENLABS_API_KEY` is already visible to the running Spynel process, voice and audio messages start going to the ElevenLabs cloud API without further action. Set `speech.provider: parakeet` to keep transcription local.

- `enabled`: transcribe incoming voice notes and audio files; enabled by default in new workspaces.
- `transcript_echo`: echo each successful transcription back to the originating chat or topic before the agent turn starts; default `true`. The echo is a standalone literal message that contains the shared generated-transcript block and, on Telegram, replies to the originating voice or audio message. WhatsApp text sends cannot quote-reply, and WhatsApp clients may still render matched formatting pairs in the literal text. A failed or disabled transcription produces no echo, and echo delivery failure never affects the turn or the reply. Disable it to send transcripts only to the agent.
- `provider`: `parakeet` or `elevenlabs`; default `elevenlabs`. It applies live, and the replacement channel adapter wires the new backend on its next use.
- `language`: `auto` or one of the selectable language codes below, shared by both providers. `auto` selects Parakeet's multilingual automatic detection and omits the language field from an ElevenLabs request; `en` uses Parakeet Unified EN.
- `max_file_mb`: accepted source-audio limit, enforced from the opened file for both providers. The transport-declared duration is sender-controlled metadata, so this is the hard size bound.
- `max_duration_seconds`: maximum audio duration processed. Parakeet bounds the beginning portion it decodes; ElevenLabs rejects an upload before any request when the declared duration is missing or above this value.
- `elevenlabs_api_key`: ElevenLabs only. Optional API key stored in the private `.spynel/config.yaml`; a stored key overrides the environment variable. Lists, get output, and forms report only `set` or `not set`, and the value is redacted from durable command history and session labels. Clear it with `/config unset speech.elevenlabs_api_key` to restore environment resolution. The stored value is plaintext in the private `0600` file, not encrypted; prefer entering it from the TUI or local CLI. Trusted `message.received` extensions see raw commands, the authenticated loopback request body carries the value, and a value set from Telegram or WhatsApp transits that provider's servers.
- `elevenlabs_api_key_env`: ElevenLabs only. Name of the environment variable used when no stored key is set; default `ELEVENLABS_API_KEY`. The name must be a portable variable identifier (`[A-Za-z_][A-Za-z0-9_]*`, at most 128 bytes). Spynel reads the variable when a transcription runs and never stores, logs, or displays its value.
- `elevenlabs_model_id`: ElevenLabs only. `scribe_v2` (default) or `scribe_v1`.
- `model_dir`: Parakeet only. Optional local directory containing `encoder.int8.onnx`, `decoder.int8.onnx`, `joiner.int8.onnx`, and `tokens.txt`; when empty, Spynel securely downloads the selected model on first supported use.
- `num_threads`: Parakeet only. Positive CPU thread count used by sherpa-onnx; default `2`.
- `chunk_seconds`: Parakeet only. Maximum mono PCM segment passed to Parakeet at a time.

Selectable values cover all 25 languages supported by multilingual Parakeet: Bulgarian (`bg`), Croatian (`hr`), Czech (`cs`), Danish (`da`), Dutch (`nl`), English (`en`), Estonian (`et`), Finnish (`fi`), French (`fr`), German (`de`), Greek (`el`), Hungarian (`hu`), Italian (`it`), Latvian (`lv`), Lithuanian (`lt`), Maltese (`mt`), Polish (`pl`), Portuguese (`pt`), Romanian (`ro`), Slovak (`sk`), Slovenian (`sl`), Spanish (`es`), Swedish (`sv`), Russian (`ru`), and Ukrainian (`uk`). `auto` is also available for multilingual automatic detection.

The first supported voice note downloads about 480 MB of compressed model data and expands it to roughly 640 MB in `spynel/speech/v1/parakeet` below the platform's per-user cache directory. Linux and macOS use the path returned by their native user-cache convention. Compatible workspaces reuse one model version. A cross-process lock protects validation and installation; the downloader writes a private `.partial` file, enforces the pinned maximum size, verifies SHA-256, rejects absolute or parent-traversal archive paths, verifies every required file and compatibility marker, and atomically publishes a versioned directory. A corrupt managed version is replaced. If the user cache cannot be resolved or created, explicit `parakeet` reports the failure at startup and recommends fixing its permissions or setting `speech.model_dir` explicitly; a missing-key fallback meets the same problem at transcription time as the ordinary failure marker. This model lifecycle applies to the local Parakeet backend, whether selected explicitly or used as the fallback.

Initialization writes the current embedded contract for new workspaces but does not overwrite an existing customized `.spynel/AGENTS.md`.

Miniaudio accepts WAV, FLAC, and MP3. Telegram and WhatsApp Ogg/Opus voice notes are detected from their container signature and decoded by an in-process pure-Go Opus implementation. Both paths downmix to mono float32 at 16 kHz. M4A/AAC, WebM, and other formats return an explicit unsupported-format error without starting the model download. No Python, FFmpeg, external ASR executable, installer, or PATH change is needed. One process-wide worker serializes voice work, reuses the active recognizer, holds only one configured PCM chunk in memory, and bounds the final transcript.

With `elevenlabs` selected, Spynel streams the stored attachment to the fixed `https://api.elevenlabs.io/v1/speech-to-text` endpoint with the `xi-api-key` header, refuses to follow redirects, serializes one upload process-wide, and applies a 15-minute budget to the upload, provider processing, and any retry. Provider errors are reduced to one bounded line with the HTTP status and a sanitized provider code and message. One upload retries exactly once, and only when ElevenLabs answers HTTP 429 with the `rate_limit_exceeded` code and a `Retry-After` of at most 60 seconds. Selecting this provider sends accepted audio to ElevenLabs, including permitted group audio, and billing follows audio duration; see [ElevenLabs pricing](https://elevenlabs.io/pricing). Transcription is best-effort and at least once, so a transport redelivery can upload the same audio again. The ElevenLabs client never initializes or downloads a local model. Only a missing or blank effective API key, meaning both the stored key and the environment variable resolve empty, triggers the fallback to local Parakeet; a nonblank stored key is authoritative, and every other provider outcome surfaces as the failure marker. The speech-cache startup check applies only to explicit `parakeet`.

Known settings still require valid types and values; `/config set` rejects unknown names.

## Run at startup

Opening `/configure` checks the operating system’s registration and shows its state beside one button: **Enable autostart** when disabled, or **Disable autostart** when enabled. Each action validates the result and updates that same button, keeping keyboard focus and unsaved edits. Disable then enable to repair a registration. If inspection fails, the form shows the error and **Check autostart** to retry. Text channels and automation use `/config set startup.enabled on|off` through the same validated operation.

The service executes `spynel serve --automatic-startup --config <absolute-workspace>/.spynel/config.yaml` with the workspace root as its working directory. npm installations retain the Node launcher and script installations retain their stable entry point, preserving explicit `/update` support without proactive update checks during automatic startup. Disable the old workspace's registration before moving it, then enable the new location. After upgrading a broken registration or moving the executable, disable then enable autostart to regenerate and validate it.

On Linux, root registrations are `/etc/systemd/system/spynel-<workspace-hash>.service`, enabled at boot through `multi-user.target.wants`, with an explicit root user and home directory. Other users register under `~/.config/systemd/user/` with `default.target.wants`; these start with the user manager at login, or at boot when the OS has enabled lingering for that user. Spynel validates the unit with `systemd-analyze verify`, reloads the matching systemd manager, and checks its unit-file registration. No manual daemon reload is needed. Workspace paths may contain spaces, Unicode, quotes, backslashes, `%`, and `$`; generated units preserve them literally. Control characters in workspace paths are rejected.

On macOS, Spynel validates the launch agent/daemon plist with `plutil` and applies the matching launchctl enable/disable override. Registrations preserve `HOME` and any explicit `XDG_CONFIG_HOME`/`XDG_CACHE_HOME` on both platforms, including the per-user environment identity and speech cache. Other shell-only environment variables are not copied. Store the Telegram token through the private `/telegram` form/command (clear it with `/telegram unset token` while Telegram is disabled), or provide a configured `token_env` through an environment source visible to the service account. The same requirement applies to harness API keys that are not stored by the harness's normal login flow. For the ElevenLabs speech key, either store it in the workspace with `/config set speech.elevenlabs_api_key <key>` so the service reads it from private configuration, or provide an environment source visible to the service account for `speech.elevenlabs_api_key_env`.

The `startup.enabled` preference is saved before registration/removal and remains saved if the OS operation fails; retry the corresponding action after correcting the reported error. `/status` reports this saved preference, not live registration. Verification establishes future automatic startup registration, not current service health. Enabling does not launch another Spynel process, and disabling removes only this workspace's registration without stopping running sessions.

## Retention cleanup

`workspace.cleanup_retention_days` defaults to `30` for new and existing workspaces and applies live. The elected primary runs cleanup every eight hours. `/cleanup [days]` runs the identical operation manually and defaults to `7`; values must be whole days from 1 through 36500. Eligibility is strict: an item timestamped exactly at the cutoff is retained, and only items older than the cutoff are processed.

Cleanup uses a cross-process lock, so manual and automatic invocations cannot overlap. Conversation age comes from the last durable history entry, with file modification time used only for an empty history. Cleanup protects every conversation leased by an authenticated live TUI, the invoking conversation, conversations with live jobs, and every exact live job archive. Completed job archives use their end time and interrupted archives use their start time; files strictly older than the same cutoff are removed. Cleanup deletes only old histories and validated bounded job archives. Results report removed conversations, removed job files and bytes, protected items, and per-item failures without rolling back successful independent items, and never touch active work or leases.

After a new primary takes ownership, cleanup remains fenced for one full live-lease duration so attached clients can renew into the replacement process. An ownerless plain-CLI cleanup holds the election mutation lock through deletion and runs only when no primary lease has existed outside the durable one-live-lease grace period after a clean release. Startup admission and resume-branch creation through registration are serialized with history deletion so a new conversation can never be removed by a concurrent cleanup.

## Extensions

- `enabled`: bypass or enable every hook without deleting extensions.
- `directory`: installed Git repository root.
- `hook_timeout`: maximum duration per executable hook.

These extension controls are available through `/config` and take effect after restart. The supported hook names are exactly `message.received`, `harness.before`, and `harness.after`; discovery rejects any other name. Extension repositories are trusted local code and receive the same filesystem/process authority as Spynel.
