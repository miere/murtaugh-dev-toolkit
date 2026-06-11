# Seeding & config: bootstrap, slack, agents

All three write into the workspace (`~/.config/murtaugh` by default) and back up
any file they replace.

## `setup.bootstrap` — seed the workspace

*Seed the Murtaugh config directory with embedded defaults (idempotent).*

Takes **no arguments**. Creates (without overwriting anything you've edited):

- `slack.yaml`, `agents.yaml`, `jobs.yaml` (defaults)
- `templates/` (the bundled Block Kit templates)
- `.agents/skills/<skill>/…` (the bundled agent skills) and a `.claude/skills`
  symlink to them

Returns a report of which files were **created** vs **preserved**. Run it first
on a fresh install; safe to re-run any time (it only fills in what's missing).

## `setup.slack` — write slack.yaml

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

## `setup.agents` — write agents.yaml

*Write agents.yaml with the ACP block and an optional default agent.*

| Arg | Required | Meaning |
|---|---|---|
| `agent_name` | no | Key to register the agent under. Defaults to `default`. |
| `command` | no | Path to the ACP-speaking binary. **Blank disables ACP** (`acp.enabled: false`). |
| `args` | no | Arguments for the agent command. |

Writes the ACP block with tuned defaults plus the agent profile (`0600`, backs
up existing). To enable chat you still need `chat.default_agent` in `slack.yaml`
(see the `murtaugh-agents` skill).

```bash
murtaugh setup agents --command /usr/local/bin/acp-agent --args --stdio
```
