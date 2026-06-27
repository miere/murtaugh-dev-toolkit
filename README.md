<div align="center">

# 🛠️ Murtaugh

_Your Slack-native AI agent and developer toolkit — chat, automations, jobs, and link previews, in a single Go binary._

[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Linux-555)](#quick-start)
[![Slack](https://img.shields.io/badge/Slack-Socket%20Mode-4A154B?logo=slack&logoColor=white)](https://api.slack.com/apis/socket-mode)
[![MCP](https://img.shields.io/badge/MCP-server-blue)](docs/cli-and-mcp.md)

</div>

Murtaugh turns Slack into a first-class developer surface. It is a single,
self-contained binary that connects to your workspace over Socket Mode and adds:

- 💬 **AI chat** — DM the bot or `@mention` it; a built-in native LLM agent (or
  any ACP-compatible agent) streams its reply back into the thread.
- 🔘 **Workflow rules** — react to Block Kit button clicks and form submissions
  with templated replies, shell commands, or agent delegation.
- 🔗 **Link unfurling** — replace bare URLs with rich Block Kit previews.
- ⏰ **Jobs** — run shell commands or agents on demand or on a cron/interval.
- 🔍 **Gateway Debug Mode** — every interaction is recorded as a structured,
  queryable event so you (or an agent) can ask *"why did that misbehave?"*.
- 🧰 **CLI + MCP server** — every capability is a terminal command and an MCP
  tool exposed to other AI clients.

---

## Quick start

**macOS** — the installer downloads the right binary, seeds your config, and
optionally installs the background daemon:

```sh
curl -fsSL https://github.com/miere/murtaugh-dev-toolkit/releases/latest/download/install_macos.sh | bash
```

**From source** (requires [Go 1.26+](https://go.dev/dl/)):

```sh
git clone https://github.com/miere/murtaugh-dev-toolkit.git
cd murtaugh-dev-toolkit
go build -o murtaugh ./cmd/murtaugh
```

Then create a Slack app, fill in your config, and start the gateway:

```sh
murtaugh slack gateway
```

👉 Full walkthrough: **[Getting started](docs/getting-started.md)**.

---

## Documentation

| Guide | What it covers |
|---|---|
| 🚀 [Getting started](docs/getting-started.md) | Install, create the Slack app, write the config, first run. |
| ⚙️ [Configuration](docs/configuration.md) | Every config file (`gateway.yaml`, `agents.yaml`, `.env`, …) and what each does. |
| 🤖 [Agent chat](docs/agents.md) | Native vs ACP agents, tools, routing, streaming, interrupts, approval. |
| 💬 [Slack](docs/slack.md) | Messaging, asking the user, Block Kit, workflow rules, and link unfurling. |
| ⏰ [Jobs](docs/jobs.md) | Define, run, and schedule shell-command and agent jobs. |
| 🔍 [Gateway Debug Mode](docs/journal.md) | Query the structured event journal to debug and audit Murtaugh. |
| 🧰 [CLI & MCP server](docs/cli-and-mcp.md) | Call any tool from the terminal or expose them over MCP. |
| 🛟 [Operations](docs/operations.md) | Run the daemon, restart it, read its logs, and troubleshoot. |
| 🏗️ [Architecture](ARCHITECTURE.md) | Internal design, package layout, and data flow. |

---

## How it fits together

```
                       ┌──────────────────────────────┐
   Slack workspace ◄──►│   murtaugh slack gateway      │
   (Socket Mode)       │   the long-lived daemon       │
                       │                               │
   • slash commands    │   • chat   → agent (native /  │──► LLM provider
   • @mentions / DMs    │             ACP), streamed    │    or ACP process
   • button clicks     │   • workflow rules            │
   • shared links      │   • link unfurls              │──► journal (SQLite)
                       │   • job scheduler             │
                       └──────────────────────────────┘
                          ▲                        ▲
                          │                        │
                 murtaugh <tool>            murtaugh mcp
                 (CLI, one-shot)         (MCP server, stdio)
```

Every capability is registered as a **tool** with one definition, surfaced three
ways: as a Slack interaction, a CLI command, and an MCP tool. See
[ARCHITECTURE.md](ARCHITECTURE.md) for the details.

---

## Contributing

1. Fork the repository and create a feature branch.
2. Run `go build ./...`, `go vet ./...`, and `go test ./...` before opening a PR.
3. Follow the conventions in [ARCHITECTURE.md](ARCHITECTURE.md).

History is kept linear — rebase your branch, don't merge `main` into it.
</content>
</invoke>
