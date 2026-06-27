# Agent chat

Murtaugh can route Slack **DMs and `@mentions` to an AI agent**, stream the
reply back into the thread, and let a follow-up interrupt an in-flight response.
This page covers the two agent backends, the tools an agent may call, routing,
and how a turn behaves.

Two files turn chat on:

1. **`agents.yaml`** — define at least one agent.
2. **`gateway.yaml`** — set `chat.enabled: true` and point `chat.default_agent`
   at one of those agents.

With chat disabled (the default), DMs and mentions are ignored — but agents are
still available to [jobs](jobs.md), [workflow rules](slack.md#workflow-rules),
and [unfurls](slack.md#link-unfurling).

---

## Two backends: native and ACP

An agent's backend is chosen by **which sub-block it carries** — there is no
`kind:` field.

| | **native** (default) | **ACP** |
|---|---|---|
| Sub-block | `native:` | `acp:` |
| What runs | Murtaugh runs the LLM loop in-process | An external agent binary, driven over the Agent Client Protocol |
| Needs | `provider` + `model` + `api_key_env` | a `command` |
| Auth | API key from `.env` (named by `api_key_env`) | the child process's own env |

### A native agent

```yaml
agents:
  emily:
    workdir: ${HOME}/work/emily                 # roots the files/terminal tools
    tools: [files, terminal, skills, slack, jobs, ask, present_plan, attach]
    approval:
      terminal: allowlist                       # confirm side-effecting commands in Slack
      allow: [kubectl, "docker ps"]             # extend the auto-run read-only set
    native:
      provider: gemini                          # gemini | anthropic | openai
      model: gemini-2.5-pro
      api_key_env: GEMINI_API_KEY               # variable name in .env (never the key itself)
      # base_url:                               # set for Z.ai / DeepSeek / Kimi compat endpoints
      system_prompt_file: prompts/emily.md      # or inline `system_prompt: |`
      max_turns: 40
      context_limit: 1000000                    # token budget; omit for a provider default
      compaction: truncate                      # truncate (default) | summarize
      cache_retention: 5m                       # 5m (default) | 1h | off
```

The API key value **never** lives in YAML — `api_key_env` names a variable in
`~/.config/murtaugh/.env`. Write it with `murtaugh setup env`. GLM, DeepSeek, and
Kimi ride the `anthropic`/`openai`-compatible families via a `base_url`
override. A workspace `AGENTS.md` in the agent's `workdir` is auto-loaded into
the system prompt as project guidelines.

### An ACP agent

```yaml
agents:
  default:
    workdir: /path/to/workspace
    acp:
      command: /path/to/acp-agent
      args: [--stdio]
      # interruptible: false   # omit to auto-detect cancel support at startup
      # env:
      #   ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}
```

Leave `interruptible` unset and let Murtaugh probe the agent's cancel support at
startup; only pin it when the probe is wrong. Native agents don't probe.

---

## What an agent can do: tools

The `tools:` list (shared by both backends) controls which capabilities an agent
may call. Entries are native tool **groups** plus registry **namespaces**:

| Tool / group | Lets the agent… |
|---|---|
| `files` | Read and write files under its `workdir`. |
| `terminal` | Run shell commands (subject to the approval gate, below). |
| `skills` | Load Murtaugh's bundled skills for how-to knowledge. |
| `slack` | Post, update, and read Slack messages and reactions. |
| `jobs` | Define and run [jobs](jobs.md). |
| `ask` | Put a question with options to you as clickable buttons, and **wait** for the answer. |
| `present_plan` | Show a plan with Proceed / Revise / Cancel and **wait** for sign-off. |
| `attach` | Return a workspace file (report, image, export) as a real Slack upload; confined to `workdir`. |

`ask` and `present_plan` are recommended — they let the agent get a real answer
instead of guessing. See [Slack → Asking the user](slack.md#asking-the-user).

### The approval gate

`approval.terminal` governs whether a native agent's `terminal` commands need
your sign-off in Slack **during live chat**:

- `allowlist` (default) — auto-run recognised read-only commands (`ls`, `cat`,
  `grep`, `git status`, …); ask before anything else.
- `prompt` — ask before *every* command.
- `off` — never ask.

`approval.allow` extends the auto-run set with your own commands — an argv0 like
`kubectl`, or a `binary subcommand` like `docker ps`. The gate is **only** active
in live chat; scheduled and delegated runs are never gated.

### External MCP servers

A native agent can attach to external MCP servers, defined once at the top of
`agents.yaml` and referenced by name:

```yaml
mcp_servers:
  vaultre:
    command: vaultre-mcp
    args: [--stdio]
    env:
      VAULTRE_TOKEN: ${VAULTRE_TOKEN}
  data-api:
    url: https://data-api.internal/mcp

agents:
  emily:
    mcp_servers: [vaultre, data-api]   # additive on top of the global set
    native: { ... }
```

Each server uses exactly one transport: a stdio child process (`command`) or a
remote endpoint (`url`).

---

## Routing: which agent answers

```yaml
# gateway.yaml
chat:
  enabled: true
  default_agent: default        # DMs and any unrouted channel
  # dm_agent: support           # optional: a different agent for DMs
  # channel_agents:
  #   C0ENG1: coding            # by channel ID
  #   feature-*: coding         # or by channel-name glob
```

- `default_agent` handles DMs and any channel without a more specific route.
- `channel_agents` is keyed by **channel ID** (`C0ENG1`) or a **channel-name
  glob** (`feature-*`) — not a `#name`.
- Every routed agent name must exist in `agents.yaml`, or the gateway refuses to
  start (fail-closed).

| Entry point | Session scope |
|---|---|
| DM the bot | one session per DM channel |
| `@mention` in a channel | one session per Slack thread |
| `/murtaugh chat <prompt>` | one session per thread |

Sessions are bound to their conversation and never shared across threads.

---

## How a turn behaves

**Streaming.** The reply streams into the thread using Slack's native streaming
APIs, updated as chunks arrive — no polling. Tune the cadence with
`defaults.rendering.stream_append_interval` and `stream_min_chunk_chars`, and
choose how tool progress renders with `defaults.rendering.progress_display`
(`simplified`, the default one-line status, or `tasks`, the full plan cards).

**Pausing for you.** Mid-turn, a native agent may stop to **ask you** (`ask`),
get **sign-off on a plan** (`present_plan`), or seek **approval to run a command**
(the terminal gate). The turn waits for your click. A quiet turn may be *waiting*
on you, not hung.

**Interrupts and stop.** A new message in the same DM or thread automatically
interrupts the previous reply (if the agent supports it): Murtaugh cancels the
in-flight prompt, waits `defaults.acp.cancel_grace_period` (default `2s`) for
trailing chunks, then hard-cancels. The interrupted reply is sealed with an
`_interrupted_` marker so partial output stays visible. To stop without sending a
follow-up, run `/stop` (or `/murtaugh stop`) from the thread or DM.

---

## Runtime defaults

The `defaults:` block in `agents.yaml` tunes **both** backends:

```yaml
defaults:
  session:
    idle_timeout: 30m
    request_timeout: 10m       # idle-bounded: reset by each agent event, not total wall-clock
    max_concurrent: 100
  rendering:
    progress_display: simplified
    stream_min_chunk_chars: 96
    stream_append_interval: 750ms
  acp:                         # ACP child-process lifecycle (native ignores these)
    startup_timeout: 10s
    cancel_grace_period: 2s
  # approval:                  # global default, overridable per agent
  #   terminal: allowlist
  #   requests: ask            # how an ACP agent's own permission prompts are answered
```

> `request_timeout` is **idle-bounded** — it's reset by every agent event, so a
> long-but-active turn won't time out; only a genuinely stalled one will.

An agent's `workdir` defaults to the workspace (`~/.config/murtaugh`) when unset,
so it starts where the bundled skills and templates live.
