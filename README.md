# Murtaugh Dev Toolkit

A single Go binary that turns Slack into a first-class developer surface: slash
commands, Block Kit workflow rules, AI chat via any ACP-compatible agent, rich
link unfurling, and an MCP server — all configured in two YAML files.

---

## Table of contents

- [Features](#features)
- [Installation](#installation)
  - [macOS (recommended)](#macos-recommended)
  - [Build from source](#build-from-source)
- [Configuration](#configuration)
  - [Slack app setup](#slack-app-setup)
  - [slack.yaml](#slackyaml)
  - [agents.yaml](#agentsyaml)
- [First steps after setup](#first-steps-after-setup)
- [Running Murtaugh](#running-murtaugh)
- [Usage](#usage)
  - [AI chat](#ai-chat)
  - [Workflow rules](#workflow-rules)
  - [Custom link unfurling](#custom-link-unfurling)
  - [CLI tools](#cli-tools)
  - [MCP server](#mcp-server)
- [Reference assets](#reference-assets)
- [Architecture](#architecture)
- [Contributing](#contributing)

---

## Features

- **AI chat in Slack** — DM the bot, `@mention` it in a channel, or use a
  slash command; responses stream in real time via Slack's native streaming APIs.
- **Workflow rules** — react to Block Kit button/form submissions with
  template-rendered replies or arbitrary shell commands.
- **Custom link unfurling** — replace bare URLs with rich Block Kit previews,
  powered by templates or external scripts.
- **Gateway Debug Mode** — every gateway interaction, workflow rule, unfurl, and
  job run is recorded as a structured event you (or an agent) can query back with
  `journal query` to debug what happened.
- **MCP server** — expose every tool to AI clients over JSON-RPC stdio.
- **CLI** — call any registered tool directly from your terminal.

---

## Installation

### macOS (recommended)

The bundled installer handles downloading the right binary for your
architecture, writing starter config files, setting up a LaunchAgent, and
optionally wiring Murtaugh into your MCP client.

~~~sh
curl -fsSL https://github.com/miere/murtaugh-dev-toolkit/releases/latest/download/install_macos.sh | bash
~~~

Common flags:

| Flag | Effect |
|---|---|
| `--yes` | Non-interactive; accept all defaults |
| `--version v1.2.3` | Install a specific release |
| `--dry-run` | Preview every change without writing anything |
| `--force` | Reinstall even when the current version is already installed |
| `--skip-config` | Update the binary only; leave config files untouched |
| `--reconfigure` | Rewrite all config files from scratch |

The installer is **safe to re-run**:

- If the installed version already matches the latest release it exits cleanly.
- Config files are **preserved by default**; use `--reconfigure` to overwrite.
- When a running LaunchAgent is present, the installer restarts it automatically
  after updating the binary.

#### Supported chat agents

During installation you can point Murtaugh at any ACP-compatible agent binary
already on your machine:

- `opencode acp`
- `goose acp`
- `auggie --acp --allow-indexing`
- a custom command of your choice
- skip — set this up later in `agents.yaml`

> **Note:** the installer does not download or install third-party agents for
> you; it only records the command in `agents.yaml`.

#### MCP client setup

The installer can register Murtaugh as an MCP server in supported clients and
will create a backup of any existing client config before modifying it.

---

### Build from source

Requirements: **Go 1.22+**

~~~sh
git clone https://github.com/miere/murtaugh-dev-toolkit.git
cd murtaugh-dev-toolkit
go build -o murtaugh ./cmd/murtaugh
~~~

Move the binary anywhere on your `$PATH`, then continue with
[Configuration](#configuration) below.

---

## Configuration

Murtaugh reads two YAML files from `~/.config/murtaugh/` by default. You can
override the path with `--config /path/to/slack.yaml`; `agents.yaml` is always
looked up in the same directory.

### Slack app setup

Before filling in the config files you need a Slack app with the following
settings enabled:

1. **Socket Mode** — on; generates the `xapp-…` app-level token.
2. **Slash commands** — add one entry per command listed in `slack.yaml`
   `commands`.
3. **OAuth scopes** (Bot Token):
   - `commands` — slash commands
   - `app_mentions:read`, `im:history` — chat
   - `chat:write`, `chat:write.public` — sending messages
   - `links:read` — link unfurling (if used)
4. **Event subscriptions** — subscribe to:
   - `app_mention`, `message.im` — for AI chat
   - `link_shared` — for URL unfurling (if used)
5. **App Unfurl Domains** — register each domain you want to unfurl (max 5).

### slack.yaml

Create `~/.config/murtaugh/slack.yaml`:

~~~yaml
oauth:
  app_token: xapp-your-socket-mode-app-token
  bot_token: xoxb-your-bot-token

configuration:
  admin_user: your-slack-handle   # @handle or Slack user ID
  debug: false

chat:
  default_agent: default
  dm_agent: default
  channel_agents:
    C12345: coding   # route a specific channel to a different agent

commands:
  - name: /murtaugh
    description: Entrypoint for Murtaugh commands
~~~

`configuration.admin_user` may be a Slack handle (with or without `@`) or a
user ID. When Socket Mode connects, Murtaugh opens a DM with that user and
sends the startup ping message.

### agents.yaml

Create `~/.config/murtaugh/agents.yaml`:

~~~yaml
acp:
  enabled: true
  request_timeout: 10m
  session_idle_timeout: 30m
  max_sessions: 100
  stream_append_interval: 750ms
  stream_min_chunk_chars: 96

agents:
  default:
    command: /path/to/default-agent
    args: [--stdio]
  coding:
    command: /path/to/coding-agent
    args: [--stdio]
~~~

Set `acp.enabled: false` (or omit `agents.yaml` entirely) if you do not need
AI chat.

---

## First steps after setup

Once the Slack app is configured and Murtaugh is running, try these two things
to verify everything is wired up correctly.

### 1 — Test the connection

When Murtaugh starts it sends a **ping card** to your configured `admin_user`
DM. Open that DM in Slack and press the **Ping** button on the card. You should
see a pong reply appear in the same thread within a second or two. If the pong
arrives, Socket Mode, OAuth tokens, and workflow rules are all working.

### 2 — Ask the bot to customise your workflows

Start a DM with the bot (or mention it in a channel) and describe what you want
it to set up. For example:

> "Hey Murtaugh, create a workflow rule that replies with a thank-you message
> whenever someone clicks the Approve button on a code-review card in
> `#eng-reviews`."

The bot can draft workflow-rule YAML, suggest Block Kit templates, and guide you
through any additional Slack app configuration needed for the new rule.

---

## Running Murtaugh

### Slack gateway

The gateway is the long-running Socket Mode daemon. Start it explicitly:

~~~sh
murtaugh slack gateway
~~~

`murtaugh slack` on its own lists the slack subcommands; `murtaugh` on its own
prints usage. For the full command reference run **`murtaugh help`**, or
**`murtaugh help <command>`** / **`murtaugh <command> --help`** for a single
command (e.g. `murtaugh help slack send-msg`) — every flag, default, and
example is documented there.

### Slack tools (CLI)

The same Slack capabilities the MCP server exposes are available as one-shot
CLI tools under the `slack` namespace:

~~~sh
murtaugh slack send-msg --to '#general' --body 'hello'
murtaugh slack fetch-msgs --channel general
murtaugh slack fetch-reactions --channel general --from @ada --emoji thumbsup
murtaugh slack update-msg --channel C123 --ts 1234.5678 --body 'edited'
~~~

These reuse the gateway's `oauth.bot_token`, so no extra configuration is
needed.

### As a LaunchAgent (macOS)

The macOS installer can create `~/Library/LaunchAgents/dev.murtaugh.plist` so
the gateway starts automatically on login and restarts on crash.

---

## Usage

### AI chat

When `acp.enabled: true`, Murtaugh routes Slack conversations to whichever ACP
agent is configured for that context:

| Entry point | Session scope |
|---|---|
| DM the bot | One session per DM channel |
| `@mention` in a channel | One session per Slack thread |
| `/murtaugh chat <prompt>` | One session per thread |

Responses stream in real time using `chat.startStream` / `chat.appendStream` /
`chat.stopStream` — no polling or `chat.update` loops.

A new message in the same DM or thread automatically interrupts the previous
response: Murtaugh asks the ACP agent to cancel the in-flight prompt, waits
`acp.cancel_grace_period` (default `2s`) for trailing chunks to flush, then
hard-cancels the chat goroutine. The interrupted reply is sealed with an
`_interrupted_` marker so the partial output stays visible. To stop a response
without sending a follow-up, invoke `/stop` (or `/<command> stop`) from inside
the thread or DM — registration of the `/stop` slash command in the Slack app
config is the operator's job.

### Workflow rules

Add rules to `slack.yaml` under `workflow-rules` to respond to Block Kit
button and form submissions. Rules match against the raw Slack interaction
payload; the first match wins. Triggers run in order and may post a rendered
JSON reply, execute a shell command, or hand the work to an agent.

~~~yaml
workflow-rules:
  code-review-approval:
    request_event: interactive
    match:
      channel: { name: eng-reviews }
      actions:
        - block_id: github_pull_request
          action_id: approve_only
    trigger:
      - reply-to-slack:
          template: code-review/approved.json
      - run:
          cmd: /path/to/notify-script
          args: [--env, production]
      # Fire-and-forget: run an agent that acts through its own tools. Prompts
      # are rendered against the interaction payload under `.Payload`.
      - delegate-to-agent:
          agent: default
          prompt: "Post a review summary for {{ .Payload.user.id }} in the thread."
~~~

A `reply-to-slack` trigger can itself delegate to an agent instead of a
`template`/`run` — the agent must then return solely a valid Slack message
JSON, which is posted back. `template`, `run`, and `delegate-to-agent` are
mutually exclusive within `reply-to-slack`.

If `workflow-rules` is omitted, Murtaugh installs the built-in ping/pong rule
so the startup card works out of the box.

### Custom link unfurling

Add rules to `slack.yaml` under `unfurl-rules` to replace shared URLs with
Block Kit attachment previews:

~~~yaml
unfurl-rules:
  github-pr:
    match:
      domain: github.com
      url_pattern: '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/(?P<number>\d+)'
    unfurl:
      template: unfurl/github-pr.json
~~~

An `unfurl` action is exactly one of `template`, `run`, or `delegate-to-agent`.
A `delegate-to-agent` unfurl runs an agent whose final output must be a valid
Slack attachment JSON (the prompt can reference `{{ .URL }}` and
`{{ .Captures.<name> }}`); non-JSON output is logged and the link is left
un-unfurled. Template paths are resolved relative to the config directory, then
fall back to the embedded `assets/` files. Each `match.domain` must also be
registered in the Slack app's **App Unfurl Domains** list.

### CLI tools

Every registered tool is callable directly from your terminal:

~~~sh
murtaugh ping                                          # → pong

murtaugh jobs run --name nightly-deploy

murtaugh jobs define \
  --name nightly-deploy \
  --command /usr/local/bin/deploy \
  --args --env --args production \
  --workdir /srv/deploy \
  --timeout 15m
~~~

Schema-typed arguments are coerced automatically: `--count 5` → integer,
`--verbose true` → boolean, repeated `--args` flags → array. Note every flag
takes a value — booleans included (`--load true`, not a bare `--load`). Run
`murtaugh help <command>` for the full per-command flag reference.

### MCP server

~~~sh
murtaugh mcp    # speaks MCP JSON-RPC on stdin/stdout
~~~

Exposes every registered tool to AI clients. Stdout is reserved for the
protocol; diagnostics go to stderr.

---

## Gateway Debug Mode

Murtaugh records what it does — gateway interactions (slash commands, button
clicks), workflow rule matches and trigger outcomes, link unfurls, and job runs
— as **structured events** in a local SQLite journal, so you (or the gateway
chatbot) can ask *why did this misbehave?* and get an answer by querying rather
than grepping logs. This is separate from the daemon's stderr logs.

It is **on by default**; tune it in `journal.yaml` (per-stream `enabled` and
`retention`, the DB `path`, and the sweep cadence). Inspect it with:

```
murtaugh journal query --stream gateway --channel C123 --since 1h --level error
murtaugh journal query --corr-id gw_3f9c2b1a   # the whole story of one interaction
murtaugh journal stats                         # per-stream counts and time span
murtaugh journal prune                         # drop events past their retention
```

Every event from one interaction shares a correlation id, so the usual flow is:
filter to find a failure, then re-query by `--corr-id` to see that interaction
end to end. The bundled `murtaugh-journal` agent skill teaches the chatbot this
workflow. The gateway daemon prunes past-retention events automatically (at
startup and every `sweep.every`); `journal prune` is the manual equivalent.

---

## Reference assets

The `assets/` directory ships a starter `slack.yaml`, default ping/pong JSON
payloads, an example `unfurl/github-pr.json` template, and a `journal.yaml`
reference. Copy or adapt them to your config directory to override the built-in
defaults.

---

## Architecture

Internal design decisions, package layout, data-flow diagrams, and contribution
conventions are covered in [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Contributing

1. Fork the repository and create a feature branch.
2. Run `go build ./...`, `go vet ./...`, and `go test ./...` before opening a
   pull request.
3. Follow the conventions in [ARCHITECTURE.md](ARCHITECTURE.md).
