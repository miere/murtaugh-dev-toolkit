---
name: murtaugh-agents
description: Configure Murtaugh's agent chat in agents.yaml/gateway.yaml — native vs ACP profiles, which tools an agent may call, the defaults block, and which agent answers DMs vs each channel.
requires: [manage]
files:
  reference/agents-yaml.md: { requires: [manage], summary: "define agents (provider/model/tools/approval/defaults block) or a legacy ACP command" }
  reference/routing.md:      { requires: [manage], summary: "wire which agent answers DMs vs each channel" }
  reference/interaction.md:  { requires: [manage], summary: "chat triggers, streaming, interrupts/stop, warmup (how chat behaves)" }
---

# Skill: Murtaugh Agent Chat

Murtaugh can route Slack **DMs and @-mentions to an AI agent**, stream the reply
back into the thread, and let a follow-up interrupt an in-flight response. Use
this whenever a task involves configuring which agent answers, tuning
streaming/timeouts, or understanding the `/chat` and `/stop` behavior.

## Two agent backends

- **native** (the **default**) — Murtaugh runs the LLM loop in-process and
  talks to a provider (`gemini` / `anthropic` / `openai`, plus compatible third
  parties via `base_url`) directly. A native agent authenticates with an API key
  read from `~/.config/murtaugh/.env` — its profile names the variable via
  `api_key_env`, and the key value lives only in that `.env`, never in YAML.
  Configured with a `native:` sub-block.
- **ACP** (legacy) — Murtaugh drives an external agent process over ACP (the
  Agent Client Protocol). Configured with an `acp:` sub-block holding a `command`.

## Turning it on (two files)

1. **`agents.yaml`** — define at least one agent. For a **native** agent that
   means a `native:` sub-block with `provider` + `model` + `api_key_env`; for a
   legacy **ACP** agent it means an `acp:` sub-block with a `command`. →
   `reference/agents-yaml.md`
2. **`gateway.yaml`** — set `chat.enabled: true` and `chat.default_agent` to one
   of those agent names. → `reference/routing.md`

With chat disabled (the default), DMs and mentions are ignored. Note that
`chat.enabled` gates **only** the Slack chat surface (DMs + @mentions); agent
delegation (jobs, workflow rules, unfurls) runs whenever the target agent is
defined, regardless of this flag.

> The runtime tuning block is `defaults:` in `agents.yaml`; its knobs (under
> `session`, `rendering`, `acp`, `approval`) govern **both** native and ACP
> agents.

## The flow (mental model)

1. A user **DMs** the bot or **@-mentions** it in a channel (or runs
   `/murtaugh chat …`).
2. Murtaugh **resolves which agent** handles it (DM vs channel routing). →
   `reference/routing.md`
3. The agent's reply **streams** into the thread, updated as chunks arrive. A
   native agent may pause mid-turn to **ask you** (`ask`), get **sign-off on a
   plan** (`present_plan`), or seek **approval to run a command** (the terminal
   gate), waiting for your click before continuing. → `reference/interaction.md`
4. A new message on the same conversation **interrupts** the previous reply (if
   the agent supports it); `/stop` cancels on demand. → `reference/interaction.md`

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Defining agents (native `provider`/`model`/`tools`/`approval` or legacy ACP `command`) and tuning the `defaults` block (timeouts, streaming, sessions) | `reference/agents-yaml.md` |
| Wiring which agent answers DMs vs each channel | `reference/routing.md` |
| Understanding `/chat`, `/stop`, interrupts, streaming, and warmup (how chat behaves) | `reference/interaction.md` |
| How an agent *uses* `ask` / `present_plan` / the approval gate (vs *enabling* them here) | the `murtaugh-slack` skill (`reference/asking.md`) |
| Wanting a working `agents.yaml` + `chat` block | `examples/` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **Native is the default backend.** The backend is chosen by which sub-block a
  profile carries: a `native:` block makes it native, an `acp:` block makes it a
  legacy ACP process. There is no `kind:` field. Native needs `provider` +
  `model` + `api_key_env` (under `native:`), not a `command`.
- **Native agents authenticate via `~/.config/murtaugh/.env`.** The profile names
  the variable with `api_key_env`; write the value there with `setup_env` (see
  the `murtaugh-setup` skill). The key never goes in YAML.
- **`chat.default_agent` is required when `chat.enabled`** (in `gateway.yaml`) and
  every routed agent name must exist in `agents.yaml`, or the gateway refuses to
  start (fail-closed).
- **`channel_agents` is keyed by channel ID** (e.g. `C0ENG1`) or a channel-name
  glob (`feature-*`), not a `#name`.
- **Leave `interruptible` unset** (ACP only) and let Murtaugh probe the agent at
  startup; only pin it (`true`/`false`) when the probe is wrong or you want to
  skip it. Native agents don't probe.
- An agent's **`workdir` defaults to the workspace** (`~/.config/murtaugh`) when
  unset, so it starts where the bundled skills/templates live.
