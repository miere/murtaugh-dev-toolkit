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
flickering token-by-token. Each turn is bounded by `request_timeout`
(default 10m) as an *idle* timeout — the budget resets on every chunk or task
update, so a long turn that keeps making progress is never cut off; only an
agent that goes silent for the whole window is treated as stalled. When that
happens Murtaugh asks the agent to stop and posts a notice rather than leaving a
silent, dead message behind.

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

## Mid-turn interaction: the agent asks *you*

A native agent doesn't just stream a reply — it can pause mid-turn and put a
**decision in front of you in the same thread**, then block until you respond.
Three surfaces:

- **`ask`** — the agent asks a question and **WAITS** for your answer instead of
  assuming one. A single quick choice renders as clickable Slack buttons
  (`question` + `options`); several questions at once, or multi-select / free-text
  answers, render as a form behind an *Answer* button (`questions`). The agent
  gets back exactly what you chose or typed — or a note that you didn't respond
  (which it must **not** treat as approval).
- **`present_plan`** — the agent lays out a concrete plan and **WAITS** for
  sign-off via **Proceed / Revise / Cancel** buttons before starting multi-step
  work. *Proceed* greenlights it as presented; *Revise* sends it back for changes
  (it doesn't start); *Cancel* stops it.
- **The terminal approval gate** — when a native agent wants to run a
  side-effecting shell command, Murtaugh posts it to the thread with
  **Approve / Deny** buttons and waits. Whether a given command is gated depends
  on the agent's `approval:` block (`allowlist` auto-runs recognized read-only
  commands and asks for the rest; `prompt` asks for every command; `off` never
  asks). See `reference/agents-yaml.md`. The gate is only active in **live chat**
  — scheduled jobs and delegated agents are never gated.

All three only work **inside a Slack conversation**: they target the same thread
the agent is talking in. Outside a chat turn (a CLI/MCP call, a headless job) the
interactive tools return an error rather than blocking.

### Why this exists — the consent posture

These tools encode Murtaugh's house rule that **consent is explicit or it isn't
consent**. Before anything that changes code, runs a side-effecting command, or
spans several steps, the agent says what it intends and waits for an explicit
go-ahead; read-only information-gathering is the only thing it does on its own.
Silence, a non-answer, or a changed subject are **not** approval — if the agent
asked a question (via `ask`/`present_plan`), it does not get to answer it by
acting. And **approval covers only what was agreed**: if the approved path fails
or needs a workaround — a different command, installing something — that's a new
decision and the agent stops to ask again.

### Non-interruptible agents

If an agent can't be interrupted (probe said so, or `interruptible: false`), a
follow-up that arrives mid-reply is **deferred, not dropped destructively**:
Murtaugh leaves the current reply running and posts a brief thread note —

> :hourglass_flowing_sand: Still working on your previous message — this agent
> can't be interrupted, so I'll finish that first before picking this up.

— so the user knows their follow-up will be handled after the current one finishes.

## Warmup (ACP agents)

At startup Murtaugh **warms each ACP agent** (bounded by `startup_timeout`): it
probes for session/cancel support and logs whether the agent is interruptible.
A failed warmup is logged but doesn't stop the daemon — the agent is simply
exercised lazily on first use. Check the gateway log to confirm an agent came up
and whether it's interruptible.

Native agents don't run this session/cancel probe — they own the loop in-process
— so the warmup verdict and `interruptible` only apply to ACP profiles.
