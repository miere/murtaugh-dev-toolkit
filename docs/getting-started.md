# Getting started

This guide takes you from nothing to a running Murtaugh that answers in Slack.
Three steps: **install the binary**, **create the Slack app**, **write the
config and run the gateway**.

---

## 1. Install

### macOS (recommended)

The bundled installer downloads the right binary for your architecture, seeds
starter config files, sets up a background daemon (LaunchAgent), and can wire
Murtaugh into your MCP client.

```sh
curl -fsSL https://github.com/miere/murtaugh/releases/latest/download/install_macos.sh | bash
```

Useful flags:

| Flag | Effect |
|---|---|
| `--yes` | Non-interactive; accept all defaults. |
| `--version v1.2.3` | Install a specific release. |
| `--dry-run` | Preview every change without writing anything. |
| `--force` | Reinstall even if the latest version is already present. |
| `--skip-config` | Update the binary only; leave config files untouched. |
| `--reconfigure` | Rewrite all config files from scratch. |

The installer is **safe to re-run**: it exits cleanly when already up to date,
preserves your config by default, and restarts a running daemon after updating.

During install you choose how chat is backed:

- **native** — Murtaugh talks to an LLM directly. You pick a provider
  (`gemini` / `anthropic` / `openai`), a model, and an API key. The key is
  stored in `~/.config/murtaugh/.env`.
- an **ACP agent** already on your machine (`opencode acp`, `goose acp`,
  `auggie --acp`, or a custom command). The installer records the command but
  does **not** download third-party agents for you.
- **skip** — set chat up later in `agents.yaml`.

For unattended installs (`--yes`), set `MURTAUGH_CHAT_AGENT=native` with
`MURTAUGH_NATIVE_PROVIDER`, `MURTAUGH_NATIVE_MODEL`, and
`MURTAUGH_NATIVE_API_KEY`.

### Build from source

Requirements: **Go 1.26+**.

```sh
git clone https://github.com/miere/murtaugh.git
cd murtaugh
go build -o murtaugh ./cmd/murtaugh
```

Move the binary anywhere on your `$PATH`, then continue below.

### Guided setup with the tools

Once the binary is on `PATH`, the `setup_*` tools can create and edit your whole
config for you (each is idempotent and backs up what it replaces):

```sh
murtaugh setup bootstrap          # seed ~/.config/murtaugh with defaults (run first)
murtaugh setup slack ...          # write gateway.yaml (OAuth tokens, admin user, chat)
murtaugh setup env ...            # upsert provider keys into .env
murtaugh setup agents ...         # write agents.yaml (a native or ACP agent)
murtaugh setup launchd ...        # (macOS) install the daemon
murtaugh setup mcp-register ...   # (optional) register Murtaugh in an MCP client
```

Run `murtaugh help setup <tool>` for the exact flags of each. The rest of this
guide shows the resulting files so you can also write them by hand.

---

## 2. Create the Slack app

Murtaugh connects over **Socket Mode**. In your Slack app settings, enable:

1. **Socket Mode** — generates the `xapp-…` app-level token.
2. **Slash commands** — register `/murtaugh` (and optionally a standalone
   `/stop`). Murtaugh recognises the verbs `chat`, `stop`, `troubleshoot`,
   `restart`, and `help`.
3. **OAuth scopes** (Bot Token):
   - `commands` — slash commands
   - `app_mentions:read`, `im:history` — chat
   - `chat:write`, `chat:write.public` — sending messages
   - `files:write` — uploading the `/murtaugh troubleshoot` bundle and agent attachments
   - `links:read` — link unfurling (only if you use it)
4. **Event subscriptions**:
   - `app_mention`, `message.im` — AI chat
   - `link_shared` — URL unfurling (only if you use it)
   - `app_home_opened` — the App Home control panel
5. **App Unfurl Domains** — register each domain you want to unfurl (max 5).
6. **App Home** — enable the **Home Tab**. This surfaces a control panel showing
   Murtaugh's version. For the `admin_user` it also offers a **Restart Murtaugh**
   button (a graceful, confirmed restart on demand) and a one-click **Update**
   button when a newer release is available. No extra scope required.

Copy the **app-level token** (`xapp-…`) and the **bot token** (`xoxb-…`); you
need both next.

---

## 3. Configure and run

Murtaugh reads its config from `~/.config/murtaugh/`. Secrets live in `.env`;
the YAML files reference them as `${VAR}` and never hold values.

### `.env` — secrets

```sh
# ~/.config/murtaugh/.env   (mode 0600 — keep it secret)
SLACK_APP_TOKEN=xapp-your-socket-mode-app-token
SLACK_BOT_TOKEN=xoxb-your-bot-token

# Only the provider keys your native agents use:
GEMINI_API_KEY=your-key-here
```

### `gateway.yaml` — the gateway

```yaml
# ~/.config/murtaugh/gateway.yaml
oauth:
  app_token: ${SLACK_APP_TOKEN}
  bot_token: ${SLACK_BOT_TOKEN}

access:
  admin_user: your-slack-handle   # @handle or Slack user ID
  allowed_users: []               # empty = admin-only (fail-closed)
  debug: false

chat:
  enabled: true                   # gate the DM + @mention chat surface
  defaults:
    agent: default                # must name an agent in agents.yaml
```

`admin_user` may be a handle (with or without `@`) or a user ID. On startup
Murtaugh opens a DM with that user and sends a **ping card**.

### `agents.yaml` — the chat agent

A minimal native agent (Murtaugh runs the LLM loop itself, no external process):

```yaml
# ~/.config/murtaugh/agents.yaml
agents:
  default:
    tools: [files, terminal, skills, slack, ask, present_plan]
    native:
      provider: gemini            # gemini | anthropic | openai
      model: gemini-2.5-pro
      api_key_env: GEMINI_API_KEY # names the variable in .env
```

If you don't need chat, set `chat.enabled: false` in `gateway.yaml` and omit the
agent. See **[Agent chat](agents.md)** for ACP agents, tools, and tuning.

### Start the gateway

```sh
murtaugh slack gateway
```

On macOS the installer can register a LaunchAgent so the gateway starts on login
and restarts on crash — see **[Operations](operations.md)**.

---

## 4. Verify it works

**Test the connection.** Open your DM with the bot and press the **Ping** button
on the startup card. A pong reply within a second or two confirms Socket Mode,
OAuth, and the workflow engine are all wired up.

**Ask the bot to help you.** DM it (or `@mention` it in a channel) and describe
what you want:

> "Hey Murtaugh, create a workflow rule that replies with a thank-you whenever
> someone clicks Approve on a code-review card in `#eng-reviews`."

The bot can draft workflow-rule YAML, suggest Block Kit templates, and guide you
through any extra Slack configuration the new rule needs.

---

## Next steps

- [Configuration](configuration.md) — the full map of config files.
- [Agent chat](agents.md) — tune which agent answers, its tools, and approvals.
- [Slack](slack.md) — workflow rules and link unfurling in depth.
- [Jobs](jobs.md) — schedule recurring work.
