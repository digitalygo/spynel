# Configuration application matrix

Every setting below is exposed by the shared typed catalog used by the TUI, slash commands, and plain CLI. The structured route array uses JSON as its command/form value while remaining ordinary YAML on disk. All rows use the serialized `app.Service.ApplySettings` path: validate, atomically save private YAML, reload that canonical file into the shared process snapshot before returning, and notify runtime owners. Subsequent operations read the refreshed snapshot; minimal direct hooks refresh owners that cache derived state. Active channels and in-flight work are preserved while their supervisors consume the new snapshot. `/config unset <key>` and `spynel config unset <key>` clear a clearable value through the same path; clearing can reject with ordinary validation, for example when an enabled Telegram token would become empty. The three extension rows are the sole restart exception.

| Setting | Prior behavior | Runtime owner and final effect | Validation / application | Verification boundary |
| --- | --- | --- | --- | --- |
| `harness.name` | Live, idle-only | Harness supervisor replaces the adapter before commit | Old harness restored if persistence fails | harness supervisor and service tests |
| `harness.model` | Live, active-safe | Persistence commit and provider-dispatch snapshot share one supervisor fence; admitted turns keep their model and later dispatches use the new value | No runtime model publication if persistence fails | active-turn, continuation, and dispatch-race tests |
| `harness.reasoning_effort` | Live, active-safe | Shares the atomic inference-selection commit and dispatch snapshot with model/service mode | Unsupported model combinations reject; a model change clears stale effort to inherit | capability, adapter, continuation, and dispatch-race tests |
| `harness.service_mode` | Live, active-safe | Shares the atomic inference-selection commit; Codex maps the catalog value to app-server `serviceTier` | Unsupported harness/model combinations reject; a model change clears stale mode to inherit | capability, adapter, continuation, and dispatch-race tests |
| `harness.sandbox` | Live, idle-only | Harness supervisor applies provider policy before commit | Old harness restored if persistence fails | harness policy tests |
| `harness.acp_command` | Live, idle-only | Harness supervisor replaces custom ACP before commit | Old harness restored if persistence fails | ACP configuration tests |
| `harness.acp_args` | Live, idle-only | Harness supervisor replaces custom ACP arguments before commit | Old harness restored if persistence fails | ACP argument tests |
| `workspace.history_max_messages` | Live | Prompt construction seeds the newest bounded-history count only when the provider session does not retain the conversation; `0` disables prior-history seeding while the current message is always delivered | Validation/persistence is all-or-nothing | history prompt-limit and retained-context tests |
| `workspace.history_char_limit` | Live | Prompt construction reads the newest character limit and bounds the current entry when positive; `0` disables prior-history seeding | Validation/persistence is all-or-nothing | history prompt-limit and retained-context tests |
| `workspace.attachment_max_mb` | Live | Channel generation fingerprint and outbound parsing use the accepted limit | Invalid values reject before commit; stale adapters are revoked | channel/media limit tests |
| `workspace.cleanup_retention_days` | Live | The primary's eight-hour automatic cleanup reads the committed age in whole days; manual `/cleanup [days]` uses its own explicit value | Whole-day range validation rejects before commit | cleanup eligibility and selector tests |
| `startup.enabled` | Live | Immediate enable/disable actions and text commands register/remove and validate after snapshot reload, including unchanged retries | Invalid values reject before save; native errors identify unverified registration despite a saved preference | startup native-query/environment tests and application/TUI action tests |
| `channels.tui.title` | Live | Shared-state title publication updates attached TUIs | Persistence failure publishes nothing | service title tests |
| `channels.tui.theme` | Live | Palette validates before commit, then publishes to attached TUIs | Unknown/invalid palette rejects before commit | theme service and visual tests |
| `extensions.enabled` | Restart-bound | Process-start hook runner snapshot; takes effect after restart | Typed validation and atomic persistence only | catalog restart-bound test |
| `extensions.directory` | Restart-bound | Process-start trusted extension root; takes effect after restart | Typed validation and atomic persistence only | extension discovery tests |
| `extensions.hook_timeout` | Restart-bound | Process-start per-hook timeout; takes effect after restart | Duration validation and atomic persistence only | extension timeout tests |
| `channels.telegram.token` | Live | Channel supervisor replaces only Telegram; secrets remain masked | Invalid complete config rejects before commit; stale generation is revoked | channel supervisor/security tests |
| `channels.telegram.allowed_users` | Live | Adapter also resolves the newest list at every authorization boundary | Invalid/revoked access fails closed; stale generation is revoked | Telegram authorization tests |
| `channels.telegram.enabled` | Live | Channel supervisor starts or stops Telegram | Invalid enablement rejects before commit | supervisor lifecycle tests |
| `channels.telegram.name` | Live | Replacement Telegram generation uses the new display name | Stale generation cannot publish status | Telegram configuration tests |
| `channels.telegram.token_env` | Live | Replacement resolves the new environment reference | Missing token fails closed | Telegram preflight tests |
| `channels.telegram.mode` | Live | Replacement switches polling/webhook mode | Webhook prerequisites validate before commit | Telegram webhook tests |
| `channels.telegram.webhook_url` | Live | Replacement uses the new public webhook URL | Complete webhook config validates before commit | Telegram webhook tests |
| `channels.telegram.webhook_listen` | Live | Channel supervisor consumes the refreshed snapshot and replaces Telegram | Invalid values reject before save; adapter failures remain isolated | Telegram webhook and supervisor tests |
| `channels.telegram.webhook_secret` | Live | Replacement uses the new verification secret | Missing secret rejects webhook mode before commit | Telegram webhook security tests |
| `channels.telegram.poll_timeout_seconds` | Live | Replacement polling generation uses the new timeout | Range validation rejects before commit | Telegram polling tests |
| `channels.telegram.group_mode` | Live | Replacement and delivery authorization use the new group policy | Validation/persistence is all-or-nothing | Telegram group tests |
| `channels.telegram.welcome_enabled` | Live | Replacement reads the new welcome policy | Validation/persistence is all-or-nothing | Telegram welcome tests |
| `channels.telegram.welcome_message` | Live | Replacement reads the new template | Validation/persistence is all-or-nothing | Telegram welcome tests |
| `channels.telegram.notify_messages` | Live | Replacement emits notices under the new policy | Stale adapter status/notices are fenced | channel notice tests |
| `channels.telegram.attachment_max_age_hours` | Live | Replacement cleanup policy uses the new retention | Range validation rejects before commit | Telegram attachment tests |
| `channels.whatsapp.mode` | Live | Channel supervisor replaces only WhatsApp | Invalid complete config rejects before commit; stale generation is revoked | WhatsApp mode tests |
| `channels.whatsapp.allowed_numbers` | Live | Adapter resolves the newest canonical list at every authorization boundary | Invalid/revoked access fails closed | WhatsApp authorization tests |
| `channels.whatsapp.enabled` | Live | Channel supervisor starts or stops WhatsApp | Invalid enablement rejects before commit | supervisor lifecycle tests |
| `channels.whatsapp.database` | Live | Channel supervisor consumes the refreshed snapshot and opens the selected workspace-relative session database | Invalid values reject before save; adapter failures remain isolated | WhatsApp configuration and supervisor tests |
| `channels.whatsapp.allow_groups` | Live | Replacement and delivery authorization use the new group policy | Validation/persistence is all-or-nothing | WhatsApp group tests |
| `channels.whatsapp.poll_interval_seconds` | Live | Replacement health loop uses the new interval | Range validation rejects before commit | WhatsApp polling tests |
| `speech.enabled` | Live | Channel fingerprint replaces adapters; the next voice or audio event uses the new policy | Stale adapters are revoked | speech/channel tests |
| `speech.transcript_echo` | Live | The next successful Telegram or WhatsApp transcription echoes the shared generated-transcript block to the same chat or topic before dispatch | Boolean catalog validation rejects before commit; disabling suppresses only the echo | transcript echo, config, and channel tests |
| `speech.provider` | Live | Default `elevenlabs`; the channel fingerprint re-wires the replacement adapter to the selected backend, and the ElevenLabs selection is wrapped so a missing or blank API key alone falls back to local Parakeet for that transcription | Catalog validation rejects before commit | provider selection, fallback, and channel rewire tests |
| `speech.language` | Live | Replacement transcriber selection uses the new language for either provider | Catalog validation rejects before commit | speech language tests |
| `speech.elevenlabs_api_key` | Live | The next ElevenLabs transcription resolves the trimmed stored key first; a blank stored value falls through to the environment variable, and both empty fall back to local Parakeet for that call | Secret setting masked as `set`/`not set` and trimmed on save; `/config unset` clears it through the same validated transaction | secret masking, precedence, unset, status/doctor, and channel rewire tests |
| `speech.elevenlabs_api_key_env` | Live | The next ElevenLabs transcription resolves the new environment reference when no stored key is set; both empty fall back to local Parakeet for that call | Portable variable-name validation rejects before commit; the key value is never read during save | speech key-reference and fallback tests |
| `speech.elevenlabs_model_id` | Live | The next ElevenLabs upload sends the new model ID | Catalog validation rejects before commit | speech model tests |
| `speech.model_dir` | Live | Parakeet replacement uses the new explicit model root on the next transcription | Invalid runtime model use returns a visible event error | speech model tests |
| `speech.num_threads` | Live | Parakeet replacement transcriber snapshot uses the new worker count | Range validation rejects before commit | speech configuration tests |
| `speech.max_file_mb` | Live | Replacement rejects subsequent oversized voice and audio files at the new bound | Range validation rejects before commit | speech size-limit tests |
| `speech.max_duration_seconds` | Live | Replacement bounds the next transcription duration or rejects the upload | Range validation rejects before commit | speech duration tests |
| `speech.chunk_seconds` | Live | Parakeet replacement chunks the next transcription at the new duration | Range validation rejects before commit | speech chunk tests |

`channels.tui.enabled` is intentionally absent. Legacy YAML containing it is normalized during load and a canonical save removes it. Bare `spynel` launches the TUI; `spynel serve` remains headless unless invoked with `--tui`.

Retired task and goal workflow settings are ordinary unused keys: they are ignored on load and omitted by the next canonical save.
