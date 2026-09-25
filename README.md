<p align="center">
  <img src=".github/resources/banner.webp" alt="Spynel - Simplicity. Leverage. Quality." width="100%">
</p>

<p align="center">
  <a href="https://agent-zero.ai"><img alt="Website" src="https://img.shields.io/badge/www-agent--zero.ai-0A192F?style=flat&amp;logo=googlechrome&amp;logoColor=white"></a>
  &nbsp;
  <a href="https://discord.gg/B8KZKNsPpj"><img alt="Discord" src="https://img.shields.io/badge/Discord-5865F2?style=flat&amp;logo=discord&amp;logoColor=white"></a>
  &nbsp;
  <a href="https://x.com/Agent0ai"><img alt="X" src="https://img.shields.io/badge/X-000000?style=flat&amp;logo=x&amp;logoColor=white"></a>
  &nbsp;
  <a href="https://www.youtube.com/@AgentZeroFW"><img alt="YouTube" src="https://img.shields.io/badge/YouTube-FF0000?style=flat&amp;logo=youtube&amp;logoColor=white"></a>
  &nbsp;
  <a href="https://github.com/sponsors/agent0ai"><img alt="GitHub Sponsors" src="https://img.shields.io/badge/Sponsors-FF69B4?style=flat&amp;logo=githubsponsors&amp;logoColor=white"></a>
</p>

  <em>Using more agents should not mean spending more time managing agents - that's not leverage.<br> I want to have one communication channel to all my work. One assistant to talk to. One that will do all the management and scaling instead of me.</em>
  <br><em>Jan Tomášek, founder of <a href="https://github.com/agent0ai/agent-zero">Agent Zero</a></em>

<table>
  <tr>
    <td width="50%" valign="middle">
      <h2>One human, one chat</h2>
      <p>Talk with one assistant for all of your AI work. No more switching between chats, projects, and spaces - that is work for your harnesses.</p>
    </td>
    <td width="50%" align="center">
      <img src=".github/resources/readme-one-human-one-chat.webp" alt="One person using a phone connected to one assistant" width="200" style="width: 100%; max-width: 200px; height: auto;">
    </td>
  </tr>
  <tr>
    <td width="50%" align="center">
      <img src=".github/resources/readme-infinite-leverage.webp" alt="One assistant coordinating a network of agents" width="200" style="width: 100%; max-width: 200px; height: auto;">
    </td>
    <td width="50%" valign="middle">
      <h2>Compounding leverage</h2>
      <p>One relationship reaches every agent your coding harnesses can run, so leverage scales without a matching increase in your coordination time.</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="middle">
      <h2>Nothing gets lost</h2>
      <p>Every conversation, execution, and diagnostic stays durably on disk and inspectable, so quality work stays reviewable long after the turn ends.</p>
    </td>
    <td width="50%" align="center">
      <img src=".github/resources/readme-quality.webp" alt="An assistant with durable conversation and execution records" width="200" style="width: 100%; max-width: 200px; height: auto;">
    </td>
  </tr>
</table>



<br><br>

---

# ◉◉ Spynel in a nutshell

- Spynel is a lightweight program with no AI inside.
- Spynel uses your existing Codex, Claude Code, Pi, and other coding harnesses to do the work.
- Spynel gives you one communication interface and one durable record over all of it.


The idea is **one human → one assistant → ALL of the work**

Spynel has three pillars:

1. **Communication interface** - work through a single terminal chat UI, Telegram, or WhatsApp
2. **Durable oversight** - complete conversation history, live job inspection, and bounded diagnostics you can audit at any time
3. **Agentic loops in your harnesses** - your coding harnesses plan, implement, debug, and improve quality while Spynel stays a classic coordinator

**Simplicity. Leverage. Quality.**

## Quick start

For macOS and Linux:

```sh
curl -LsSf https://raw.githubusercontent.com/digitalygo/spynel/main/install.sh | sh
spynel
```

Or use npm (Node.js 18+):

```bash
npm install -g @digitalygo/spynel
spynel
```

Use `spynel update` to update Spynel and restart its running instances. Use `spynel killall` to stop all running Spynel instances.

Run `spynel` from your project directory.

Use `/config` for setup.


## Documentation

- **Start here:** [Getting started and development](docs/getting-started.md)
- **Configure Spynel:** [Configuration](docs/configuration.md) and the [configuration application matrix](docs/configuration-live-matrix.md)
- **Use the TUI, Telegram, WhatsApp, and voice:** [Communication integrations](docs/integrations.md)
- **Choose a coding harness:** [Harness compatibility](docs/harness-compatibility.md)
- **Understand the system and its security boundaries:** [Architecture](docs/architecture.md) and [provider-canary threat model](docs/provider-canary-threat-model.md)
- **Automate from the terminal:** [CLI and automation](docs/cli.md) and [programmatic integration](docs/programmatic-integration.md)
- **Send proactive messages:** [Proactive notifications](docs/cli.md#proactive-notifications)
- **Agent-readable docs:** [Agent-readable documentation](docs/agent-docs.md)
- **Install trusted hooks:** [Extensions](docs/extensions.md)
- **Build and publish releases:** [Releasing and packaging](docs/releasing.md)
- **Diagnose common setup problems:** [Troubleshooting](docs/troubleshooting.md)
- **Read the product principles:** [Product vision](docs/vision.md)

See the [documentation index](docs/README.md) for the complete guide map.

## Uninstall

```sh
curl -LsSf https://raw.githubusercontent.com/digitalygo/spynel/main/uninstall.sh | sh
```

Your workspace data is kept.
