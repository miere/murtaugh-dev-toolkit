# Murtaugh documentation

Murtaugh is a single Go binary that turns Slack into a developer surface: AI
chat, reactive workflow rules, link previews, scheduled jobs, an event journal,
a CLI, and an MCP server. These pages explain how to install it, configure it,
and operate it.

> New here? Start with **[Getting started](getting-started.md)**.

## Guides

| Guide | What it covers |
|---|---|
| [Getting started](getting-started.md) | Install Murtaugh, create the Slack app, write the config, and run the gateway. |
| [Configuration](configuration.md) | The config directory and every file in it (`gateway.yaml`, `agents.yaml`, `.env`, `jobs.yaml`, `journal.yaml`, `workflow-rules.yaml`, `unfurl-rules.yaml`). |
| [Agent chat](agents.md) | Native and ACP agents, the tools they can call, routing, streaming, interrupts, and approval gates. |
| [Slack](slack.md) | Posting and reading messages, asking the user, Block Kit, workflow rules, and link unfurling. |
| [Jobs](jobs.md) | Defining, running, and scheduling shell-command and agent jobs. |
| [Gateway Debug Mode](journal.md) | Querying the structured event journal to debug interactions and audit chat sessions. |
| [CLI & MCP server](cli-and-mcp.md) | Running any tool from the terminal, and exposing the toolset to other AI clients over MCP. |
| [Operations](operations.md) | Running the daemon, restarting it, reading its logs, and troubleshooting. |

## Concepts in one minute

- **The gateway** (`murtaugh slack gateway`) is the long-lived daemon. It owns
  every Slack event, the chat agents, the workflow/unfurl handlers, and the job
  scheduler. Almost everything runs inside it.
- **Tools** are the unit of capability. Each tool is defined once and surfaced
  three ways — as a Slack interaction, a CLI command (`murtaugh <tool>`), and an
  MCP tool (`murtaugh mcp`).
- **Config lives in `~/.config/murtaugh/`** as a handful of YAML files plus a
  secret `.env`. Secrets are *only* in `.env`; the YAML references them as
  `${VAR}` so config can be shared safely.
- **Agents** answer chat and can be delegated work by jobs, workflow rules, and
  unfurls. A *native* agent runs the LLM loop in-process; an *ACP* agent is an
  external process Murtaugh drives.
- **The journal** records what happened as structured events, so you debug by
  querying rather than grepping logs.

## A note on the config schema

Murtaugh moved from a single `slack.yaml` to a multi-file layout
(`gateway.yaml` + siblings). A legacy config directory is **auto-migrated** on
the first run of a new binary — backed up, validated, and rolled back on
failure — or you can convert it ahead of time with `murtaugh config migrate`.
These docs describe the current schema.
