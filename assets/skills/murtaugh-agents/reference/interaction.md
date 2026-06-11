# Chatting, streaming & interrupting

## What triggers a chat

| Trigger | How |
|---|---|
| **DM** | Any direct message to the bot from an allowed user. |
| **@-mention** | Mentioning the bot in a channel (the mention is stripped from the prompt). |
| **`/murtaugh chat <prompt>`** | Slash command; replies in the channel it's run in. |

All three are gated by the `allowed_users` allowlist; messages from others are
silently ignored. Duplicate Slack deliveries are de-duplicated so a redelivered
message doesn't spawn a second reply.

## Streaming

The reply streams into the thread: Murtaugh edits the message as chunks arrive,
batched by `stream_append_interval` and `stream_min_chunk_chars` (see
`reference/agents-yaml.md`) so the edit cadence stays readable rather than
flickering token-by-token. Each response is bounded by `request_timeout`
(default 10m).

## Interrupting & stopping

Two ways a reply ends early:

- **A follow-up on the same conversation** interrupts the one in flight.
- **`/stop`** (or `/murtaugh stop`) cancels the in-flight reply on demand. Inside
  a thread it targets that thread's reply; at channel root or in a DM it targets
  that conversation.

The cancel is **graceful, then hard**: Murtaugh asks the agent to cancel its
current prompt (keeping the session alive for the follow-up), waits
`cancel_grace_period` (default 2s) so trailing chunks already on the wire can
flush, then hard-cancels. The partial reply is marked `_interrupted_`.

### Non-interruptible agents

If an agent can't be interrupted (probe said so, or `interruptible: false`), a
follow-up that arrives mid-reply is **deferred, not dropped destructively**:
Murtaugh leaves the current reply running and posts a brief thread note —

> :hourglass_flowing_sand: Still working on your previous message — this agent
> can't be interrupted, so I'll finish that first before picking this up.

— so the user knows their follow-up will be handled after the current one finishes.

## Warmup

At startup Murtaugh **warms each agent** (bounded by `startup_timeout`): it
probes for session/cancel support and logs whether the agent is interruptible.
A failed warmup is logged but doesn't stop the daemon — the agent is simply
exercised lazily on first use. Check the gateway log to confirm an agent came up
and whether it's interruptible.
