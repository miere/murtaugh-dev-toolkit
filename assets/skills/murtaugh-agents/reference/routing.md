# Chat routing: which agent answers

Routing is **kind-agnostic** — the same rules pick a native or an ACP agent. The
`chat` block in `slack.yaml` decides which agent handles each conversation:

```yaml
chat:
  default_agent: default        # required when acp.enabled
  dm_agent: support             # optional: override for DMs
  channel_agents:               # optional: per-channel overrides (by channel ID)
    C0ENG1: coding
    C0SUP2: support
```

## Resolution order

For an incoming prompt, Murtaugh picks the agent like this:

- **DM** → `chat.dm_agent` if set, otherwise `chat.default_agent`.
- **Channel @-mention** → `chat.channel_agents[<channel ID>]` if that channel has
  an entry, otherwise `chat.default_agent`.
- **`/murtaugh chat …`** → same rules, based on where the command was run.

> `channel_agents` keys are **Slack channel IDs** (`C0ENG1`), not `#names`. Grab
> the ID from the channel's "About" panel or a message link.

## Validation (fail-closed)

When `acp.enabled: true`, the gateway refuses to start unless:

- `chat.default_agent` is **set** and names an agent defined in `agents.yaml`.
- `chat.dm_agent`, if set, names a known agent.
- every value in `chat.channel_agents` names a known agent.
- at least one agent is defined in `agents.yaml`.

So a typo in an agent name is caught at startup, not at first message.

## Worked example

```yaml
# agents.yaml has: default, coding, support
chat:
  default_agent: default
  dm_agent: support            # DMs go to the support agent
  channel_agents:
    C0ENG1: coding             # #engineering mentions go to the coding agent
# → a mention in any other channel falls back to `default`
```
