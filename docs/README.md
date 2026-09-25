# Documentation

Use this page to choose the shortest path to the information you need. The repository README stays focused on what Spynel is; operational and implementation detail lives here.

## Start and operate

- [Getting started and development](getting-started.md) - install, initialize a workspace, run from source, and verify a development checkout.
- [Configuration](configuration.md) - workspace, harness, interface, channel, speech, startup, and extension settings.
- [Configuration application matrix](configuration-live-matrix.md) - every exposed setting, its runtime owner, and its verification boundary.
- [Communication integrations](integrations.md) - TUI, Telegram, WhatsApp, voice, histories, and transport behavior.
- [TUI editing and terminal checks](tui-editing.md) - selection modes, clipboard fallback, word editing, and a local acceptance checklist.
- [Troubleshooting](troubleshooting.md) - common installation, harness, channel, startup, update, and speech problems.

## Automate

- [Plain CLI and automation](cli.md) - messages, follow-ups, conversations, status, framework commands, notifications, and output contracts.
- [Programmatic integration](programmatic-integration.md) - the supported v1 HTTP/NDJSON contract, committed subscriptions, and the private Unix socket.
- [Agent-readable documentation](agent-docs.md) - the offline `spynel docs` interface and its versioned JSON schema.
- [Extensions and hooks](extensions.md) - trusted executable extensions and delivery guarantees.

## Understand and maintain

- [Product vision](vision.md) - positioning, product boundaries, and the three pillars.
- [Architecture](architecture.md) - application, process, history, harness, channel, job, and notification design.
- [Coding harness compatibility](harness-compatibility.md) - evidence-backed lifecycle coverage and known gaps.
- [Provider-canary threat model](provider-canary-threat-model.md) - controls for authenticated provider testing.
- [Releasing](releasing.md) - native packaging, npm publication, credentials, and release verification.
