# Extensions and hooks

Spynel uses portable executable hooks instead of Go's platform/toolchain-coupled plugin mechanism. Install repositories only after review:

```bash
spynel extension install GIT_URL [NAME]
spynel extension list
spynel extension remove NAME
```

Each repository declares `.spynel-extension.yaml`:

```yaml
name: redact-and-audit
hooks:
  message.received: ["./bin/hook", "message.received"]
  harness.before: ["./bin/hook", "harness.before"]
  harness.after: ["./bin/hook", "harness.after"]
```

The supported lifecycle names are exactly `message.received`, `harness.before`, and `harness.after`. Discovery rejects any other hook name so a typo or removed hook cannot remain silently inactive.

Spynel runs commands from the extension root with `SPYNEL_HOOK` and `SPYNEL_EXTENSION` environment variables. One JSON line arrives on stdin:

```json
{"hook":"message.received","payload":{"channel":"telegram","conversation":"TG-42","sender":"luca","text":"hello","reply_to":""}}
```

`harness.before` carries `session_key`, `prompt`, and `channel`; `harness.after` carries `session_key`, `text`, and `kind`.

Empty stdout preserves the payload. A JSON result can replace it, cancel the operation, or provide a local message:

```json
{"payload":{"text":"rewritten"},"cancel":false,"message":"optional explanation"}
```

Hooks run sequentially in sorted extension-name order and each receives the previous hook's payload. Nonzero exit, timeout, invalid JSON, or protocol stdout beyond 1 MiB fails the surrounding operation instead of silently ignoring enforcement. Stderr is diagnostic rather than protocol data: Spynel retains at most 64 KiB for a failed hook, sends it through the runtime log's redaction and entry-boundary controls, and never includes it in the user-facing hook error.

Hooks carry no automatically supplied `event_id`, Spynel keeps no per-extension success receipt, and the runner does not retry by itself. Invocation can still repeat when the surrounding operation is retried by its caller, including after a hook failure, timeout, or interrupted restart, so a successful hook can execute more than once even after it already produced externally visible effects. Consumers must make those effects idempotent with their own stable keys when needed. Spynel does not claim exactly-once execution of arbitrary extension side effects.

The current hook model changes messages and lifecycle behavior. Adding a future compiled-in channel or harness still requires implementing the corresponding Go interface; a future extension RPC protocol can expose those registries without weakening hook portability.
