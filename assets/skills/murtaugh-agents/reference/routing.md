# Chat routing: which agent answers

Routing is **backend-agnostic** — the same rules pick a native or an ACP agent.
The `chat` block in `gateway.yaml` decides which agent handles each conversation
and whether the reply is threaded:

```yaml
chat:
  defaults:
    agent: default              # required when chat.enabled
    dm_agent: support           # optional: override for DMs
    reply_on_thread: true       # optional: global reply strategy (default true)
  channels:                     # optional: per-channel overrides
    C0ENG1:                     #   by exact channel ID
      agent: coding
    support:                    #   by exact channel name
      agent: support
    "feature-*":                #   by channel-name glob
      agent: coding
      reply_on_thread: false    #   reply directly in the channel, not a thread
```

## Resolution order

For an incoming prompt, Murtaugh picks the agent like this:

- **DM** → `chat.defaults.dm_agent` if set, otherwise `chat.defaults.agent`.
- **Channel @-mention** → the matching `chat.channels` entry for that channel
  (by ID, name, or name glob — see below); its `agent`, or `chat.defaults.agent`
  when the entry omits one.
- **`/murtaugh chat …`** → same rules, based on where the command was run.

## Reply strategy: thread vs channel

`reply_on_thread` controls where the bot posts a reply to a **top-level** channel
message:

- **`true`** (the default) — the bot roots a thread on the triggering message,
  as it always has. Each thread is its own conversation/session.
- **`false`** — the bot replies **directly in the channel**. The channel is then
  treated as one long rolling conversation (a single shared session), not a fresh
  session per message. Reset it with `/clear`.

Effective value = the matched channel's `reply_on_thread` → `chat.defaults.reply_on_thread`
→ `true`. A message that is **already in a thread** always gets a threaded reply,
regardless of the flag. DMs are always threaded.

## Channel keys: ID, name, or glob

A `chat.channels` key — and a `chat.no_mention.by_channel` key, which uses the
same syntax — can be any of three forms. `matchChannel` tries them in this order:

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

- `chat.defaults.agent` is **set** and names an agent defined in `agents.yaml`.
- `chat.defaults.dm_agent`, if set, names a known agent.
- every `chat.channels.<k>.agent` that is set names a known agent.
- at least one agent is defined in `agents.yaml`.

So a typo in an agent name is caught at startup, not at first message.

## Worked example

```yaml
# agents.yaml has: default, coding, support
chat:
  defaults:
    agent: default
    dm_agent: support            # DMs go to the support agent
  channels:
    C0ENG1:
      agent: coding              # by ID: this channel's mentions go to coding
    "support-*":
      agent: support             # by glob: any support-* channel goes to support
      reply_on_thread: false     # …replying in-channel as one rolling thread
# → a mention in any other channel falls back to `default` (threaded)
```
