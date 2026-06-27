# Chat routing: which agent answers

Routing is **backend-agnostic** — the same rules pick a native or an ACP agent.
The `chat` block in `gateway.yaml` decides which agent handles each conversation:

```yaml
chat:
  default_agent: default        # required when chat.enabled
  dm_agent: support             # optional: override for DMs
  channel_agents:               # optional: per-channel overrides
    C0ENG1: coding              #   by exact channel ID
    support: support            #   by exact channel name
    "feature-*": coding         #   by channel-name glob
```

## Resolution order

For an incoming prompt, Murtaugh picks the agent like this:

- **DM** → `chat.dm_agent` if set, otherwise `chat.default_agent`.
- **Channel @-mention** → the matching `chat.channel_agents` entry for that
  channel (by ID, name, or name glob — see below), otherwise `chat.default_agent`.
- **`/murtaugh chat …`** → same rules, based on where the command was run.

## Channel keys: ID, name, or glob

A `channel_agents` key — and a `chat.no_mention.by_channel` key, which uses
the same syntax — can be any of three forms. `matchChannelAgent` tries them in
this order:

1. **Exact channel ID** (`C…`/`G…`) — always works. Grab it from the channel's
   "About" panel or a message link.
2. **Exact channel name** — e.g. `support` (no leading `#`).
3. **Channel-name glob** — e.g. `feature-*` (`path.Match` syntax). When several
   globs match the same channel, the one with the **longest literal prefix**
   wins (so `feature-api-*` beats `feature-*`).

> Name and glob keys require Murtaugh to resolve the channel's **name**, which it
> reads from an in-memory cache built via the Slack `conversations.list` API. If
> the bot can't list channels (missing scope, or the cache failed to build),
> routing falls back to **exact-ID-only** and a name/glob key won't match.
> Channel **IDs** always work regardless — use them if name resolution is
> unavailable.

## Validation (fail-closed)

When `chat.enabled: true`, the gateway refuses to start unless:

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
    C0ENG1: coding             # by ID: this channel's mentions go to coding
    "support-*": support       # by glob: any support-* channel goes to support
# → a mention in any other channel falls back to `default`
```
