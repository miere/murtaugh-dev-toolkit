# agents.yaml: runtime tuning & agent profiles

`agents.yaml` has two parts: a `defaults:` block of runtime tuning that applies
to **both** backends, and an `agents` map of the profiles Murtaugh can talk to.
Each profile uses one of two **backends**:

- **native** (the **default**) — Murtaugh runs the LLM loop in-process: it talks
  to the provider directly and owns the conversation. This is what you almost
  always want.
- **ACP** (legacy) — Murtaugh drives an external agent process over ACP (the
  Agent Client Protocol). Kept for back-compat.

A profile is a **tagged union**: there is no `kind:` field. The backend is
chosen by which sub-block is present — a `native:` block makes the profile
native, an `acp:` block makes it a legacy ACP process. Shared knobs (`workdir`,
`tools`, `mcp_servers`, `approval`, `progress_display`, `export_skills_to_fs`)
sit at the top level of the profile.

```yaml
defaults:                  # runtime tuning, grouped by concern
  session:
    idle_timeout: 30m
    request_timeout: 10m
    max_concurrent: 100
  rendering:
    progress_display: simplified
    stream_min_chunk_chars: 96
    stream_append_interval: 750ms
  acp:                     # ACP child-process lifecycle (native ignores these)
    startup_timeout: 10s
    cancel_grace_period: 2s
  # approval:              # global default, overridable per agent below
  #   terminal: allowlist
  #   requests: ask

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
  default:                         # native (it carries a `native:` block)
    workdir: ${HOME}/work          # roots the files/terminal/attach tools
    tools: [files, terminal, skills, slack, jobs, ask, present_plan, attach]
    mcp_servers: [vaultre]
    approval:
      terminal: allowlist
      allow: [kubectl, "docker ps"]
    native:
      provider: gemini
      model: gemini-2.5-pro
      api_key_env: GEMINI_API_KEY
      system_prompt_file: prompts/default.md
      max_turns: 40
      context_limit: 1000000
      compaction: truncate
      cache_retention: 5m
```

## `defaults:` — runtime tuning (both backends)

`defaults:` is the runtime block, grouped by the concern each knob serves:
`session`, `rendering`, `acp`, and an optional `approval` global default. The
knobs apply to native and ACP agents alike. Each field is a Go duration / int;
the **effective default** below is what applies when the field is omitted (the
bootstrapped file ships tuned values).

| Field | Default if omitted | Controls |
|---|---|---|
| `session.idle_timeout` | `30m` | How long an idle session is kept before teardown. |
| `session.request_timeout` | `10m` | Idle timeout per chat turn: max time with **no agent activity** before the turn is treated as stalled. Resets on every chunk/task update, so a long but progressing response is never cut off. |
| `session.max_concurrent` | `100` | Concurrent session cap per agent. |
| `rendering.progress_display` | `simplified` | How tool/step progress renders while a turn streams: `simplified` (one small context-line message — "Reading file…" — that updates in place and resolves to "✓ Done thinking" when the turn ends) or `tasks` (the full multi-card plan woven into the reply). Per-agent profiles can override it. |
| `rendering.stream_min_chunk_chars` | `24` | Minimum characters before a chunk is flushed (avoids choppy edits). |
| `rendering.stream_append_interval` | `250ms` | How often buffered chunks are flushed to Slack. |
| `acp.startup_timeout` | `10s` | Budget for the agent warmup probe at daemon start (ACP agents). |
| `acp.cancel_grace_period` | `2s` | After asking the agent to cancel, how long to let trailing chunks flush before hard-cancelling. |
| `approval.terminal` | `allowlist` | Global default terminal-tool gate (see `approval:` below); per-agent profiles override it. |
| `approval.requests` | `ask` | Global default for how an ACP agent's own permission prompts are answered; per-agent profiles override it. |

> The chat on/off switch is **not** here — it's `chat.enabled` in `gateway.yaml`,
> which gates only the Slack chat surface (DMs + @mentions). Agent delegation
> (jobs, workflow rules, unfurls) runs whenever the target agent is defined.

## Native profiles (the default — a `native:` sub-block)

A native agent needs a **provider**, a **model**, and an **api_key_env**; the key
value itself never lives in YAML (it comes from `~/.config/murtaugh/.env` — see
the `murtaugh-setup` skill's `setup_env`). The provider/model knobs go under the
profile's `native:` sub-block; the shared knobs (`workdir`, `tools`,
`mcp_servers`, `approval`, `progress_display`, `export_skills_to_fs`) stay at the
top level of the profile.

| Field | Block | Required | Meaning |
|---|---|---|---|
| `provider` | `native:` | yes | Provider family: `gemini`, `anthropic` (Anthropic-compatible), or `openai` (OpenAI-compatible). GLM/Z.ai, DeepSeek and Kimi ride the `anthropic` or `openai` family via `base_url`. |
| `model` | `native:` | yes | Provider model id (e.g. `gemini-2.5-pro`, `glm-4.6`). |
| `api_key_env` | `native:` | yes | Name of the `.env` variable holding the API key (e.g. `GEMINI_API_KEY`). The credential never appears in YAML. |
| `base_url` | `native:` | no | Endpoint override for a compatible third party (Z.ai, DeepSeek, Kimi, self-hosted). Empty uses the provider default. |
| `system_prompt` | `native:` | no | Inline system prompt. Mutually exclusive with `system_prompt_file`; when both are empty a built-in default is used. |
| `system_prompt_file` | `native:` | no | Path (resolved against the config dir) to a file holding the system prompt. Mutually exclusive with `system_prompt`. |
| `max_turns` | `native:` | no | Tool-call iterations allowed in a single prompt. `0` uses a default. |
| `context_limit` | `native:` | no | Conversation token budget that drives compaction. `0` uses a per-provider-family default. |
| `compaction` | `native:` | no | How the conversation is kept within `context_limit`: `truncate` (default — drop oldest turn-groups) or `summarize` (LLM-compress the oldest groups, truncation as the fallback). |
| `cache_retention` | `native:` | no | Prompt-cache TTL: `5m` (default) or `1h`; `off`/`none` disables. Applied for Anthropic/OpenAI; Gemini caches a static prefix implicitly regardless. |
| `workdir` | top level | no | Working directory that roots the files/terminal/attach tools. Defaults to the workspace (`~/.config/murtaugh`) when unset. |
| `tools` | top level | no | Allowlist of tool groups the agent may use — native groups (`files`, `terminal`, `attach`) plus registry namespaces (`skills`, `slack`, `jobs`, `ask`, `present_plan`, …) and the `manage` skills-visibility grant. `attach` lets the agent return a workspace file (report, image, export) to the user as a real downloadable upload, confined to `workdir` like the files tools. Empty means only the always-on set. |
| `export_skills_to_fs` | top level | no | Bundled (`murtaugh-*`) skills to write into this agent's `workdir` so a filesystem-discovering backend (e.g. a Claude-based ACP agent) can load them. Empty (default) keeps the bundled skills in-binary only — readable solely through the gated `skills` tool, never by `files`/`terminal`. `[all]` exports every bundled skill. See below. |
| `mcp_servers` | top level | no | Names from the top-level `mcp_servers` block to attach. Each contributes its remote tools. |
| `approval` | top level | no | Human-approval gate for side-effecting tool calls (see below). Defaults to gating on (`allowlist`). |
| `progress_display` | top level | no | Override `defaults.rendering.progress_display` for this agent. |

A workspace `AGENTS.md` (in the agent's `workdir`) is auto-loaded into the system
prompt as project guidelines — no config needed. The agent's **name and voice**
are conventionally set there.

### `export_skills_to_fs` — making bundled skills filesystem-discoverable

The bundled `murtaugh-*` skills live **in the binary** and are served only
through the gated `skills` tool — they never touch disk, so the `files`/`terminal`
tools (and a shell) can't read them. That's the default and the secure posture.

Some backends discover skills from the **filesystem** instead (a Claude-based ACP
agent reads `.claude/skills/`). For those, list the skills to mirror into the
agent's `workdir`:

```yaml
agents:
  claude:
    workdir: ${HOME}/work/claude     # gets its own .agents/skills + .claude/skills
    export_skills_to_fs: [all]        # or e.g. [murtaugh-slack, murtaugh-jobs]
    acp:
      command: claude-code-acp
```

- The list is the **source of truth**, reconciled on every gateway start: listed
  skills are (re)written, and any `murtaugh-*` skill no longer listed is removed —
  so upgrades and edits self-heal. **Bespoke skills are never touched.**
- Names are validated **fail-closed**: an unknown skill name (anything other than a
  bundled `murtaugh-*` name or `all`) makes the gateway refuse to start.
- **Exporting a skill opts it out of the in-binary blind for that agent** — once on
  disk it's readable by that agent's file/terminal tools. Export only what a
  filesystem-discovering backend actually needs.
- Agents that **share a `workdir`** should agree on their export lists; they
  reconcile the same `.agents/skills`, so the last one wins. Give agents with
  different export needs distinct `workdir`s.

### `approval:` — the unified approval gate

`approval` is the unified, per-agent (top-level) human-approval gate, with a
matching global default under `defaults.approval`. It carries up to three keys:

```yaml
approval:
  terminal: allowlist          # allowlist (default) | prompt | off  — native terminal gate
  allow: [kubectl, "docker ps"] # extra read-only command keys for the terminal gate
  requests: ask                # ask (default) | auto-allow | auto-deny — ACP agent's own permission prompts
```

**Native terminal gate** (`terminal` + `allow`) gates a native agent's
side-effecting **terminal** tool calls behind a human **Approve / Deny** in Slack
(the terminal is the only tool that can act outside the rooted workspace — the
files tools are confined to `workdir`):

- `terminal: allowlist` (**default**) — auto-run a recognized **read-only**
  command (`ls`, `cat`, `grep`, `git status`, …); **ask** for anything else
  (fail-closed).
- `terminal: prompt` — ask before **every** terminal command.
- `terminal: off` — never ask (the pre-gate behaviour).
- `allow` extends the built-in read-only allowlist with extra command keys: an
  argv0 (`kubectl`) or a `binary subcommand` pair (`"docker ps"`).

**ACP permission** (`requests`) decides how an ACP agent's own permission prompts
are answered: `ask` (default — surface them to the user), `auto-allow`, or
`auto-deny`. (This replaces the former top-level `acp_permission` knob.)

The terminal gate is only active in a **live Slack chat** (where there's a human
to ask); headless runs — scheduled jobs and delegated agents — are never gated.

## ACP profiles (legacy — an `acp:` sub-block)

The legacy backend: Murtaugh drives an external agent process over ACP. A
profile becomes ACP by carrying an `acp:` sub-block (which holds `command`).
The backend-specific knobs (`command`, `args`, `interruptible`, `env`) live
under `acp:`; the shared knobs (`workdir`, `tools`, `approval`,
`progress_display`) stay at the top level of the profile.

| Field | Block | Required | Meaning |
|---|---|---|---|
| `command` | `acp:` | yes | Path to the ACP-speaking executable. |
| `args` | `acp:` | no | CLI args — commonly `[--stdio]`. |
| `interruptible` | `acp:` | no | Override for session/cancel support (see below). |
| `env` | `acp:` | no | Extra environment variables for the agent process; values are expanded against Murtaugh's own environment and layered on top of the inherited one. |
| `workdir` | top level | no | Working directory. Defaults to the workspace (`~/.config/murtaugh`) when unset. |
| `progress_display` | top level | no | Override `defaults.rendering.progress_display`: `simplified` or `tasks`. |

```yaml
agents:
  legacy:
    workdir: /path/to/workspace
    # interruptible lives under acp: below
    acp:
      command: /path/to/acp-agent
      args: [--stdio]
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
