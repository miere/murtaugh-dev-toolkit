# Seeding & config: bootstrap, slack, agents

All three write into the workspace (`~/.config/murtaugh` by default) and back up
any file they replace.

> **CLI flag spelling.** The `Arg` columns below name the schema fields
> (snake_case), which is what you pass over MCP. On the **CLI** each is a
> kebab-case flag carrying a value: `app_token` → `--app-token`, `default_agent`
> → `--default-agent`, `agent_name` → `--agent-name`. Every flag needs a value
> (there are no bare switches). Run `murtaugh help setup <tool>` (e.g.
> `murtaugh help setup slack`) or `murtaugh setup <tool> --help` for the full
> per-command reference.

## `setup_bootstrap` — seed the workspace

*Seed the Murtaugh config directory with embedded defaults.*

Takes **no arguments**. It runs automatically on every Murtaugh start (and you
can run it by hand). What it touches:

- `slack.yaml`, `agents.yaml`, `jobs.yaml` and `templates/` — **created once,
  then preserved**: your tokens and edits are never overwritten.
- `.agents/skills/<skill>/…` (the bundled agent skills) plus a `.claude/skills`
  symlink — **refreshed to the shipped version on every run**, so the workspace
  skills stay in sync with the binary. Skills you add yourself are left alone;
  edits to a skill Murtaugh ships are overwritten (add a new skill instead).

Returns a report of which files were **created**, **updated** (refreshed), and
**preserved**. Run it first on a fresh install; safe to re-run any time.

## `setup_slack` — write slack.yaml

*Write slack.yaml with OAuth tokens, admin user, and the /murtaugh slash command.*

| Arg | Required | Meaning |
|---|---|---|
| `app_token` | yes | Slack app-level token; must start with `xapp-`. |
| `bot_token` | yes | Slack bot token; must start with `xoxb-`. |
| `admin_user` | yes | Admin handle (`@name`) or user ID (`U…`). |
| `default_agent` | no | Agent name to wire into `chat.default_agent`. |

Validates the token prefixes, writes `slack.yaml` at `0600`, and backs up any
existing file. Re-run to rotate tokens or change the admin.

```bash
murtaugh setup slack --app-token xapp-… --bot-token xoxb-… --admin-user @you
```

## `setup_agents` — write agents.yaml

*Write agents.yaml with the ACP block and an optional default agent.*

| Arg | Required | Meaning |
|---|---|---|
| `agent_name` | no | Key to register the agent under. Defaults to `default`. |
| `command` | no | Path to the ACP-speaking binary. **Blank disables ACP** (`acp.enabled: false`). |
| `args` | no | Arguments for the agent command. Repeatable: pass `--args` once per argument. |

Writes the ACP block with tuned defaults plus the agent profile (`0600`, backs
up existing). To enable chat you still need `chat.default_agent` in `slack.yaml`
(see the `murtaugh-agents` skill).

`--args` is a repeatable flag — give it once per argument; the values become the
agent command's argv in order:

```bash
murtaugh setup agents --agent-name claude \
  --command /usr/local/bin/acp-agent --args --stdio --args --verbose
# → agents.yaml: command: /usr/local/bin/acp-agent, args: [--stdio, --verbose]
```
