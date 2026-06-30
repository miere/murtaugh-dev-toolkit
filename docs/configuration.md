# Configuration

Murtaugh reads its configuration from `~/.config/murtaugh/` (override the path
with `--config /path/to/gateway.yaml`; the siblings are looked up alongside it).
Each file owns one concern.

| File | Purpose | Reference |
|---|---|---|
| `.env` | **All secrets** — Slack tokens, provider API keys. Mode `0600`. | [below](#env--secrets) |
| `gateway.yaml` | The gateway: OAuth, access control, the chat surface. | [below](#gatewayyaml) |
| `agents.yaml` | Chat agents (native / ACP) and runtime defaults. | [Agent chat](agents.md) |
| `jobs.yaml` | Shell-command and agent jobs, manual or scheduled. | [Jobs](jobs.md) |
| `journal.yaml` | The structured event journal (streams, retention). | [Gateway Debug Mode](journal.md) |
| `workflow-rules.yaml` | React to Slack button clicks / form submissions. | [Slack](slack.md#workflow-rules) |
| `unfurl-rules.yaml` | Turn shared links into rich previews. | [Slack](slack.md#link-unfurling) |

Every file is optional except the secrets the gateway needs to connect. An
absent `jobs.yaml` / `journal.yaml` / `workflow-rules.yaml` / `unfurl-rules.yaml`
just means that feature is off (journal streams default **on**).

> **Golden rule:** secrets live **only** in `.env`. The YAML files reference them
> as `${VAR}`. This is what lets `murtaugh troubleshoot` bundle your `.yaml`
> files for sharing without leaking credentials — the bundler never collects
> `.env`.

---

## `.env` — secrets

```sh
# ~/.config/murtaugh/.env   (mode 0600 — keep it secret)

# --- Slack (required to run the gateway) ---
SLACK_APP_TOKEN=xapp-replace-me
SLACK_BOT_TOKEN=xoxb-replace-me

# --- LLM providers (only the ones your native agents use) ---
# The variable NAME is what an agent profile's `api_key_env:` points at.
GEMINI_API_KEY=
ANTHROPIC_API_KEY=
OPENAI_API_KEY=

# --- External MCP servers (optional) ---
# VAULTRE_TOKEN=
```

A value exported in the real environment overrides the one here. Write keys with
`murtaugh setup env --provider gemini --key ...` or by editing the file directly.

---

## `gateway.yaml`

The gateway's own configuration: how it authenticates to Slack, who may use it,
and whether the chat surface is on.

```yaml
oauth:
  app_token: ${SLACK_APP_TOKEN}   # xapp-… Socket Mode token
  bot_token: ${SLACK_BOT_TOKEN}   # xoxb-… bot token

access:
  admin_user: murtaugh-admin      # @handle or Slack user ID
  allowed_users: []               # who may interact; empty = admin-only
  debug: false

chat:
  enabled: false                  # gate the DM + @mention chat surface
  defaults:
    agent: default                # required when enabled; must exist in agents.yaml
    # dm_agent: support           # optional: a different agent for DMs
    # reply_on_thread: true       # optional: global reply strategy (default true)
  # channels:                     # optional: per-channel agent / reply overrides
  #   feature-*:
  #     agent: coding
  #     reply_on_thread: false    # reply in-channel instead of a thread
  no_mention:                     # users who can talk without @mentioning the bot
    everywhere: []
    # by_channel:
    #   feature-*: [alice, U0123ABC]
```

### Access control is fail-closed

Only `admin_user` plus everyone in `allowed_users` may interact with the bot
(slash commands, mentions, DMs). The admin is always implicitly allowed; leave
`allowed_users` empty to keep the bot admin-only. Entries may be Slack user IDs
(`U0123ABC`) or handles (`alice`, `@alice`); handles are resolved to IDs at
startup and **the gateway refuses to start if any entry can't be resolved**.

> Access gates *who can act*, not *who can see*. A message posted to a channel is
> visible to every member — use a DM or an ephemeral message for private replies.

### The chat gate

`chat.enabled` controls **only** the Slack chat surface (DM + `@mention`
replies). Agent delegation from jobs, workflow rules, and unfurls runs whenever
the target agent is defined, regardless of this flag. When `chat.enabled: true`,
`chat.defaults.agent` is required and every routed agent name must exist in
`agents.yaml` or the gateway won't start. `chat.defaults.reply_on_thread`
(default `true`) and a per-channel `chat.channels.<k>.reply_on_thread` choose
whether the bot replies in a thread or directly in the channel.

`no_mention` waives the `@mention` requirement for listed users (a waived user
must still be in `allowed_users`). `everywhere` applies in every channel;
`by_channel` applies per channel-ID or channel-name glob.

### Slash commands

Slash commands (`/murtaugh`, `/stop`) are registered in the **Slack app
manifest**, not here. Murtaugh recognises the verbs `chat`, `stop`,
`troubleshoot`, `restart`, and `help` (e.g. `/murtaugh stop`, or a standalone
`/stop`).

---

## Applying changes

The gateway loads config **once at startup** — it never hot-reloads. After
editing any config file, restart the gateway. When a config file changes the
running daemon *suggests* a restart (via an admin-only button) but applies
nothing until you do. See [Operations](operations.md#applying-config-changes).

## Reference assets

The repository's `assets/` directory ships a fully-commented starter for every
file above (`gateway.yaml`, `agents.yaml`, `jobs.yaml`, `journal.yaml`,
`workflow-rules.yaml`, `unfurl-rules.yaml`, `env.example`), plus default Block
Kit templates. `setup_bootstrap` seeds copies into your config directory; you can
also read them in-tree as the canonical commented reference.
