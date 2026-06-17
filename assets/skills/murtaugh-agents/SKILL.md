---
name: murtaugh-agents
description: Configures Murtaugh's ACP (Agent Client Protocol) chat feature, which routes Slack DMs and @-mentions to an AI agent and streams replies into the thread. Use when enabling or tuning agent chat via `agents.yaml` (e.g. `acp.enabled`, agent `command`, `workdir`, `interruptible`, timeouts, streaming, sessions) or `slack.yaml` (`chat.default_agent`, `channel_agents`). Use when wiring which agent answers DMs versus specific channels, or when explaining the `/murtaugh chat` and `/stop` commands, reply streaming, interrupts, or agent warmup.
---

# Skill: Murtaugh ACP Chat & Agents

Murtaugh can route Slack **DMs and @-mentions to an AI agent** over ACP (the
Agent Client Protocol), stream the reply back into the thread, and let a
follow-up interrupt an in-flight response. Use this whenever a task involves
configuring which agent answers, tuning streaming/timeouts, or understanding the
`/chat` and `/stop` behavior.

## Turning it on (two files)

1. **`agents.yaml`** — set `acp.enabled: true` and define at least one agent
   (its `command`). → `reference/agents-yaml.md`
2. **`slack.yaml`** — set `chat.default_agent` to one of those agent names. →
   `reference/routing.md`

With ACP disabled (the default), DMs and mentions are ignored.

## The flow (mental model)

1. A user **DMs** the bot or **@-mentions** it in a channel (or runs
   `/murtaugh chat …`).
2. Murtaugh **resolves which agent** handles it (DM vs channel routing). →
   `reference/routing.md`
3. The agent's reply **streams** into the thread, updated as chunks arrive.
4. A new message on the same conversation **interrupts** the previous reply (if
   the agent supports it); `/stop` cancels on demand. → `reference/interaction.md`

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Defining agents and tuning ACP (timeouts, streaming, sessions, `interruptible`) | `reference/agents-yaml.md` |
| Wiring which agent answers DMs vs each channel | `reference/routing.md` |
| Understanding `/chat`, `/stop`, interrupts, and warmup | `reference/interaction.md` |
| Wanting a working `agents.yaml` + `chat` block | `examples/` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **`chat.default_agent` is required when ACP is enabled** and every routed agent
  name must exist in `agents.yaml`, or the gateway refuses to start (fail-closed).
- **`channel_agents` is keyed by channel ID** (e.g. `C0ENG1`), not channel name.
- **Leave `interruptible` unset** and let Murtaugh probe the agent at startup;
  only pin it (`true`/`false`) when the probe is wrong or you want to skip it.
- An agent's **`workdir` defaults to the workspace** (`~/.config/murtaugh`) when
  unset, so it starts where the bundled skills/templates live.
