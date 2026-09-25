# Spynel Extensions

Each installed Git repository may expose `.spynel-extension.yaml`:

```yaml
name: example
hooks:
  message.received: ["./bin/example-hook", "message.received"]
  harness.before: ["./bin/example-hook", "harness.before"]
```

Hook executables receive one JSON object on stdin and may return:

```json
{"payload": {"text": "rewritten"}, "cancel": false, "message": "optional note"}
```

Available hooks are `message.received`, `harness.before`, and `harness.after`. Hook commands execute with the extension repository as their working directory. Installing an extension grants it local code execution privileges; review it first.

Hook events carry no automatically supplied `event_id`, Spynel keeps no per-extension success receipt, and the hook runner does not retry an event. Invocation can still repeat when the surrounding operation is retried by its caller, including after a hook failure, timeout, or interrupted restart, so a successful hook can execute more than once even after it already produced externally visible effects. Extensions must make every externally visible effect idempotent with their own stable keys when needed. Spynel does not promise exactly-once execution of arbitrary hook side effects.
