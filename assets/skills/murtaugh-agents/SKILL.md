---
name: murtaugh-agents
description: Configures Murtaugh's agent chat feature, which routes Slack DMs and @-mentions to an AI agent and streams replies into the thread. Agents come in two kinds — `native` (the default, an in-process LLM loop) and `acp` (a legacy external agent process over the Agent Client Protocol). Use when enabling or tuning agent chat via `agents.yaml` (a native profile's `provider`/`model`/`api_key_env`/`tools`/`mcp_servers`/`approval`/`context_limit`, or an ACP profile's `command`/`workdir`/`interruptible`, plus the shared `acp:`/`agent:` runtime block — timeouts, streaming, sessions) or `slack.yaml` (`chat.default_agent`, `channel_agents`). Use when wiring which agent answers DMs versus specific channels, when explaining the agent's mid-turn `ask`/`present_plan` tools or the terminal approval gate, or when explaining the `/murtaugh chat` and `/stop` commands, reply streaming, interrupts, or agent warmup.
---

# Skill: Murtaugh Agent Chat

Murtaugh can route Slack **DMs and @-mentions to an AI agent**, stream the reply
back into the thread, and let a follow-up interrupt an in-flight response. Use
this whenever a task involves configuring which agent answers, tuning
streaming/timeouts, or understanding the `/chat` and `/stop` behavior.

## Two agent kinds

- **`native`** (the **default**) — Murtaugh runs the LLM loop in-process and
  talks to a provider (`gemini` / `anthropic` / `openai`, plus compatible third
  parties via `base_url`) directly. A native agent authenticates with an API key
  read from `~/.config/murtaugh/.env` — its profile names the variable via
  `api_key_env`, and the key value lives only in that `.env`, never in YAML.
- **`acp`** (legacy) — Murtaugh drives an external agent process over ACP (the
  Agent Client Protocol). Configured with a `command`.

## Turning it on (two files)

1. **`agents.yaml`** — set `enabled: true` in the runtime block and define at
   least one agent. For a **native** agent that means a `provider` + `model` +
   `api_key_env`; for a legacy **ACP** agent it means a `command`. →
   `reference/agents-yaml.md`
2. **`slack.yaml`** — set `chat.default_agent` to one of those agent names. →
   `reference/routing.md`

With chat disabled (the default), DMs and mentions are ignored.

> The runtime block is `acp:`, also accepted as the alias `agent:`; its knobs
> (timeouts, streaming, sessions) govern **both** native and ACP agents.

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
| Defining agents (native `provider`/`model`/`tools`/`approval` or legacy ACP `command`) and tuning the runtime block (timeouts, streaming, sessions) | `reference/agents-yaml.md` |
| Wiring which agent answers DMs vs each channel | `reference/routing.md` |
| Understanding `/chat`, `/stop`, interrupts, the `ask`/`present_plan` tools, the approval gate, and warmup | `reference/interaction.md` |
| Wanting a working `agents.yaml` + `chat` block | `examples/` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **Native is the default kind.** A profile with no `command` (and no explicit
  `kind`) is native; one with a `command` infers `kind: acp`. Native needs
  `provider` + `model` + `api_key_env`, not a `command`.
- **Native agents authenticate via `~/.config/murtaugh/.env`.** The profile names
  the variable with `api_key_env`; write the value there with `setup_env` (see
  the `murtaugh-setup` skill). The key never goes in YAML.
- **`chat.default_agent` is required when chat is enabled** and every routed agent
  name must exist in `agents.yaml`, or the gateway refuses to start (fail-closed).
- **`channel_agents` is keyed by channel ID** (e.g. `C0ENG1`) or a channel-name
  glob (`feature-*`), not a `#name`.
- **Leave `interruptible` unset** (ACP only) and let Murtaugh probe the agent at
  startup; only pin it (`true`/`false`) when the probe is wrong or you want to
  skip it. Native agents don't probe.
- An agent's **`workdir` defaults to the workspace** (`~/.config/murtaugh`) when
  unset, so it starts where the bundled skills/templates live.
