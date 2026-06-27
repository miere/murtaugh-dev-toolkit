# Seeding & config: bootstrap, slack (gateway.yaml), env, agents

These tools write into the workspace (`~/.config/murtaugh` by default) and back
up any file they replace.

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

- `gateway.yaml`, `agents.yaml`, `jobs.yaml` and `templates/` — **created once,
  then preserved**: your tokens and edits are never overwritten. (A legacy
  `slack.yaml` layout is auto-migrated to this shape on first run, or convert it
  ahead of time with `murtaugh config migrate`.)
- `.agents/skills/` (the home for your **bespoke** skills) plus a `.claude/skills`
  symlink to it — **created if absent**. The bundled `murtaugh-*` skills are
  served in-binary and are **not** written here; an agent's `export_skills_to_fs`
  is what mirrors chosen ones into a workdir (see the `murtaugh-agents` skill).
  Skills you add yourself are left alone.

Returns a report of which files were **created**, **updated** (refreshed), and
**preserved**. Run it first on a fresh install; safe to re-run any time.

## `setup_slack` — write gateway.yaml

*Write gateway.yaml with OAuth tokens, admin user, and chat settings.*

| Arg | Required | Meaning |
|---|---|---|
| `app_token` | yes | Slack app-level token; must start with `xapp-`. |
| `bot_token` | yes | Slack bot token; must start with `xoxb-`. |
| `admin_user` | yes | Admin handle (`@name`) or user ID (`U…`); written under `access.admin_user`. |
| `default_agent` | no | Agent name to wire into `chat.default_agent`. |

Validates the token prefixes, writes `gateway.yaml` at `0600`, and backs up any
existing file. The tool name is still `setup_slack` (CLI `setup slack`); it just
writes the renamed anchor file. Re-run to rotate tokens or change the admin.

```bash
murtaugh setup slack --app-token xapp-… --bot-token xoxb-… --admin-user @you
```

## `setup_env` — upsert .env secrets

*Upsert `KEY=VALUE` secrets into `~/.config/murtaugh/.env` (other entries
preserved).*

This is the credential writer the installer uses for **LLM provider keys** — a
native agent references its key by variable name (`api_key_env`), and the value
lives only here, never in YAML. Anything else that must stay out of YAML (an MCP
server token, etc.) goes here too.

| Arg | Required | Meaning |
|---|---|---|
| `set` | yes | A `KEY=VALUE` pair to upsert. **Repeatable** — pass `--set` once per pair. The value is written verbatim. |

Merges into the existing `.env`: keys you pass are added or updated, every other
entry is left untouched. Writes `0600` and backs up any existing file. The result
reports key **names** only — never the secret values.

```bash
# Native agents can't authenticate until their key is in .env:
murtaugh setup env --set GEMINI_API_KEY=AIza… --set ANTHROPIC_API_KEY=sk-ant-…
```

## `setup_agents` — write agents.yaml

*Write agents.yaml with the runtime block and a native (default) or ACP agent.*

The **kind is inferred** when you don't pass `--kind`: a `--provider` ⇒ **native**,
a `--command` ⇒ **acp**. Passing neither writes a disabled file (chat off);
passing agent flags that can't determine a kind is rejected rather than silently
skipped.

Shared:

| Arg | Required | Meaning |
|---|---|---|
| `agent_name` | no | Key to register the agent under. Defaults to `default`. |
| `kind` | no | `native` (default) or `acp`. Inferred from the flags when omitted. |

**Native** (`--kind native`, or just pass `--provider`):

| Arg | Required | Meaning |
|---|---|---|
| `provider` | yes | Provider family: `gemini`, `anthropic`, or `openai`. |
| `model` | yes | Provider model id (e.g. `gemini-2.5-pro`). |
| `api_key_env` | yes | Name of the `.env` variable holding the API key (write the value with `setup_env`). |
| `base_url` | no | Endpoint override for a compat provider (Z.ai/DeepSeek/Kimi). |
| `tools` | no | Tool allowlist. **Repeatable** — `--tools files --tools terminal …`. |
| `mcp_servers` | no | Names of `mcp_servers` entries to attach. **Repeatable**. |
| `system_prompt_file` | no | Path (relative to the config dir) to the system prompt file. |
| `context_limit` | no | Token budget for compaction (integer). `0` uses a per-family default. |
| `compaction` | no | `truncate` (default) or `summarize`. |
| `cache_retention` | no | Prompt-cache TTL: `5m` (default), `1h`, or `off`. |

**ACP** (`--kind acp`, or just pass `--command`):

| Arg | Required | Meaning |
|---|---|---|
| `command` | yes | Path to the ACP-speaking binary. |
| `args` | no | Arguments for the agent command. **Repeatable** — once per argument. |

Writes the `defaults:` runtime block with tuned defaults plus the agent profile
(`0600`, backs up existing). The profile is a tagged union — the backend is the
sub-block written (`native:` or `acp:`), there is no `kind:` key in the YAML. To
enable the chat surface you still need `chat.enabled: true` and
`chat.default_agent` in `gateway.yaml` (see the `murtaugh-agents` skill);
delegation (jobs, workflow rules, unfurls) runs whenever the agent is defined,
regardless of that gate.

> `setup_agents` does **not** write the `approval:` block, an inline
> `system_prompt`, a `workdir`, or `max_turns` — edit `agents.yaml` directly for
> those (see the `murtaugh-agents` skill).

```bash
# Native agent (default kind): the key value already went into .env via setup_env.
murtaugh setup agents --provider gemini --model gemini-2.5-pro \
  --api-key-env GEMINI_API_KEY \
  --tools files --tools terminal --tools skills --tools ask --tools present_plan

# Legacy ACP agent. --args is repeatable; values become argv in order:
murtaugh setup agents --agent-name claude --kind acp \
  --command /usr/local/bin/acp-agent --args --stdio --args --verbose
# → agents.yaml: command: /usr/local/bin/acp-agent, args: [--stdio, --verbose]
```
