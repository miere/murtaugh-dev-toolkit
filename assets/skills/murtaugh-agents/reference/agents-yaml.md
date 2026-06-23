# agents.yaml: runtime tuning & agent profiles

`agents.yaml` has two parts: a runtime tuning block (`acp:`, also accepted as
`agent:`) that applies to **both** backends, and an `agents` map of the profiles
Murtaugh can talk to. Each profile is one of two **kinds**:

- **`native`** (the **default**) — Murtaugh runs the LLM loop in-process: it talks
  to the provider directly and owns the conversation. This is what you almost
  always want.
- **`acp`** (legacy) — Murtaugh drives an external agent process over ACP (the
  Agent Client Protocol). Kept for back-compat.

A profile's kind is resolved by `kind:`. If `kind` is omitted, a profile with a
`command:` is treated as **acp** (back-compat) and everything else defaults to
**native**.

```yaml
acp:                       # runtime tuning (also spelled `agent:`)
  enabled: true
  startup_timeout: 10s
  request_timeout: 10m
  session_idle_timeout: 30m
  max_sessions: 100
  stream_append_interval: 750ms
  stream_min_chunk_chars: 96
  cancel_grace_period: 2s
  progress_display: simplified

# External MCP servers a native agent can attach to (referenced by name from a
# profile's `mcp_servers:` list). Each uses exactly one transport: a stdio child
# process (command/args/env) or a remote endpoint (url). Secrets in env come
# from ~/.config/murtaugh/.env via ${VAR}.
mcp_servers:
  vaultre:
    command: vaultre-mcp
    args: [--stdio]
    env:
      VAULTRE_TOKEN: ${VAULTRE_TOKEN}

agents:
  default:                 # kind defaults to native
    provider: gemini
    model: gemini-2.5-pro
    api_key_env: GEMINI_API_KEY
    workdir: ${HOME}/work          # roots the files/terminal tools
    system_prompt_file: prompts/default.md
    tools: [files, terminal, skills, slack, jobs, ask, present_plan]
    mcp_servers: [vaultre]
    max_turns: 40
    context_limit: 1000000
    compaction: truncate
    cache_retention: 5m
    approval:
      terminal: allowlist
      allow: [kubectl, "docker ps"]
```

## `acp:` / `agent:` — runtime tuning (both backends)

`acp:` is the runtime block. `agent:` is its new, kind-agnostic spelling and is
accepted as an alias; when both keys are present `agent:` wins. The knobs apply
to native and ACP agents alike. Each field is a Go duration / int; the
**effective default** below is what applies when the field is omitted (the
bootstrapped file ships tuned values).

| Field | Default if omitted | Controls |
|---|---|---|
| `enabled` | `false` | Master switch for DM/mention chat. Off → DMs and mentions are ignored. |
| `startup_timeout` | `10s` | Budget for the agent warmup probe at daemon start (ACP agents). |
| `request_timeout` | `10m` | Idle timeout per chat turn: max time with **no agent activity** before the turn is treated as stalled. Resets on every chunk/task update, so a long but progressing response is never cut off. |
| `session_idle_timeout` | `30m` | How long an idle session is kept before teardown. |
| `max_sessions` | `100` | Concurrent session cap per agent. |
| `stream_append_interval` | `250ms` | How often buffered chunks are flushed to Slack. |
| `stream_min_chunk_chars` | `24` | Minimum characters before a chunk is flushed (avoids choppy edits). |
| `cancel_grace_period` | `2s` | After asking the agent to cancel, how long to let trailing chunks flush before hard-cancelling. |
| `progress_display` | `simplified` | How tool/step progress renders while a turn streams: `simplified` (one small context-line message — "Reading file…" — that updates in place and resolves to "✓ Done thinking" when the turn ends) or `tasks` (the full multi-card plan woven into the reply). Per-agent profiles can override it. |

## Native profiles (`kind: native` — the default)

A native agent needs a **provider**, a **model**, and an **api_key_env**; the key
value itself never lives in YAML (it comes from `~/.config/murtaugh/.env` — see
the `murtaugh-setup` skill's `setup_env`).

| Field | Required | Meaning |
|---|---|---|
| `kind` | no | `native` (the default). Omit it unless you want to be explicit. |
| `provider` | yes | Provider family: `gemini`, `anthropic` (Anthropic-compatible), or `openai` (OpenAI-compatible). GLM/Z.ai, DeepSeek and Kimi ride the `anthropic` or `openai` family via `base_url`. |
| `model` | yes | Provider model id (e.g. `gemini-2.5-pro`, `glm-4.6`). |
| `api_key_env` | yes | Name of the `.env` variable holding the API key (e.g. `GEMINI_API_KEY`). The credential never appears in YAML. |
| `base_url` | no | Endpoint override for a compatible third party (Z.ai, DeepSeek, Kimi, self-hosted). Empty uses the provider default. |
| `workdir` | no | Working directory that roots the files/terminal tools. Defaults to the workspace (`~/.config/murtaugh`) when unset. |
| `system_prompt` | no | Inline system prompt. Mutually exclusive with `system_prompt_file`; when both are empty a built-in default is used. |
| `system_prompt_file` | no | Path (resolved against the config dir) to a file holding the system prompt. Mutually exclusive with `system_prompt`. |
| `tools` | no | Allowlist of tool groups the agent may use — native groups (`files`, `terminal`) plus registry namespaces (`skills`, `slack`, `jobs`, `ask`, `present_plan`, …). Empty means only the always-on set. |
| `mcp_servers` | no | Names from the top-level `mcp_servers` block to attach. Each contributes its remote tools. |
| `max_turns` | no | Tool-call iterations allowed in a single prompt. `0` uses a default. |
| `context_limit` | no | Conversation token budget that drives compaction. `0` uses a per-provider-family default. |
| `compaction` | no | How the conversation is kept within `context_limit`: `truncate` (default — drop oldest turn-groups) or `summarize` (LLM-compress the oldest groups, truncation as the fallback). |
| `cache_retention` | no | Prompt-cache TTL: `5m` (default) or `1h`; `off`/`none` disables. Applied for Anthropic/OpenAI; Gemini caches a static prefix implicitly regardless. |
| `approval` | no | Human-approval gate for side-effecting tool calls (see below). Defaults to gating on (`allowlist`). |
| `progress_display` | no | Override `acp.progress_display` for this agent. |

A workspace `AGENTS.md` (in the agent's `workdir`) is auto-loaded into the system
prompt as project guidelines — no config needed. The agent's **name and voice**
are conventionally set there.

### `approval:` — the terminal approval gate

`approval` gates a native agent's side-effecting tool calls behind a human
**Approve / Deny** in Slack. v1 covers the **terminal** tool (the only tool that
can act outside the rooted workspace — the files tools are confined to `workdir`).

```yaml
approval:
  terminal: allowlist          # allowlist (default) | prompt | off
  allow: [kubectl, "docker ps"]
```

- `terminal: allowlist` (**default**) — auto-run a recognized **read-only**
  command (`ls`, `cat`, `grep`, `git status`, …); **ask** for anything else
  (fail-closed).
- `terminal: prompt` — ask before **every** terminal command.
- `terminal: off` — never ask (the pre-gate behaviour).
- `allow` extends the built-in read-only allowlist with extra command keys: an
  argv0 (`kubectl`) or a `binary subcommand` pair (`"docker ps"`).

The gate is only active in a **live Slack chat** (where there's a human to ask);
headless runs — scheduled jobs and delegated agents — are never gated.

## ACP profiles (`kind: acp` — legacy)

The legacy backend: Murtaugh drives an external agent process over ACP. Set
`kind: acp` (or just supply `command`, which infers acp).

| Field | Required | Meaning |
|---|---|---|
| `kind` | no | `acp`. Inferred when `command` is set and `kind` is omitted. |
| `command` | yes | Path to the ACP-speaking executable. |
| `args` | no | CLI args — commonly `[--stdio]`. |
| `workdir` | no | Working directory. Defaults to the workspace (`~/.config/murtaugh`) when unset. |
| `interruptible` | no | Override for session/cancel support (see below). |
| `env` | no | Extra environment variables for the agent process; values are expanded against Murtaugh's own environment and layered on top of the inherited one. |
| `progress_display` | no | Override `acp.progress_display`: `simplified` or `tasks`. |

```yaml
agents:
  legacy:
    kind: acp
    command: /path/to/acp-agent
    args: [--stdio]
    workdir: /path/to/workspace
    # interruptible: false
```

### `interruptible` — the cancel capability (ACP only)

Controls whether a follow-up can interrupt an in-flight reply:

- **omitted (default)** — Murtaugh **probes** the agent at warmup for
  session/cancel support and logs the verdict. Use this unless you have a reason
  not to.
- **`false`** — the agent can't be interrupted; a follow-up that arrives mid-reply
  is **deferred** (it waits for the current reply to finish) rather than cutting
  it off with a misleading `_interrupted_`.
- **`true`** — force-enable and skip the probe.

Native agents don't probe the same way, so `interruptible` only applies to ACP
profiles.
