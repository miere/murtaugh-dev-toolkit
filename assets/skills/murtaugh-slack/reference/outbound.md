# Outbound: sending & updating messages

The *authoring* half of the dance (step 1): how an agent or automation pushes
messages into Slack through Murtaugh — posting, updating in place, threading,
ephemeral/DM, and resolving a user mention. For the blocks you put in these
messages see `blocks.md`; for what happens when someone clicks, see `inbound.md`.

## How to invoke these

Murtaugh ships concrete tools for posting, updating, and reading messages —
`slack_send-msg`, `slack_update-msg`, `slack_fetch-msgs`, `slack_fetch-reactions`
— on the CLI (`murtaugh slack <tool> …`) and over MCP. They use the gateway's bot
token, so you never reach around Murtaugh with a raw token. **The
`murtaugh-slack-tools` skill documents each tool's arguments**; this file covers
the underlying *patterns* (one-message-per-entity, threading, mentions). Each
operation below notes the Slack method it maps to.

## The operations you need

| Operation | Slack method | Key inputs | Returns |
|---|---|---|---|
| Post a new message | `chat.postMessage` | `channel`, `blocks`, `text` (fallback) | message `ts` — **store it** |
| Update in place | `chat.update` | `channel`, `ts`, new `blocks` | — |
| Reply in a thread | `chat.postMessage` | `channel`, `thread_ts` = parent `ts`, `blocks` | reply `ts` |
| Send to one person only | `chat.postEphemeral` | `channel`, `user` (ID), `blocks` | — |
| Direct message | `conversations.open` then `chat.postMessage` | `users` → `channel`, `blocks` | message `ts` |
| Resolve a mention | (see below) | email or handle | user ID `U…` |
| Read a thread | `conversations.replies` | `channel`, `ts` | replies |

## One message per entity (the core pattern)

The default for any status/lifecycle surface: **post once, then update in place.**
Never repost on every tick.

1. Compute a stable key for the entity (e.g. `repo#number`).
2. Look up the key in a small state store (JSON file — see `automations.md`).
   - **Not seen →** `chat.postMessage`, then save `{ key: { ts, ...flags } }`.
   - **Seen →** `chat.update` against the stored `ts` with freshly-rendered blocks.
3. Use a **thread reply** (`thread_ts` = the stored `ts`) for follow-ups that
   should notify or accrue over time — e.g. tagging a reviewer when a PR becomes
   ready. Gate "post once" follow-ups behind a flag in the state store so a
   per-minute job doesn't re-tag every run.

### Idempotent reconcile loop (clock-tick automations)

```
load state
for each current entity:
    state = derive_state(entity)          # pure function of the entity's data
    blocks = render(entity, state)
    if entity.key not in store:
        ts = post(channel, blocks); store[key] = {ts, last_state: state}
    elif store[key].last_state != state:
        update(channel, store[key].ts, blocks); store[key].last_state = state
    # else: nothing changed — do nothing (idempotent)
    handle_one_shot_followups(entity, state, store[key])   # e.g. tag once
save state
```

Running this twice in a row must change nothing the second time. See
`automations.md` for the state file and scheduling conventions.

## Mentions

A real Slack mention needs the **user ID**, not the handle: render `<@U0B20G0ET9T>`
(not `@miere`) in `text`/`mrkdwn`. Two ways to get the ID:

- **Resolve at runtime** — `users.lookupByEmail` (most reliable) or scan
  `users.list`. Cache the result.
- **Inject via config** — read it from an env var / config so the script stays
  declarative. (Known mapping in this workspace: `@Miere` = `U0B20G0ET9T`.)

Put the mention in the message `text` too, so the notification fires even if the
block rendering is collapsed.

## Resilience

- A stored `ts` can go stale (message deleted). If `chat.update` fails with
  `message_not_found`, **re-post** and refresh the stored `ts`.
- On a per-entity failure, log and continue with the others; don't let one bad PR
  abort the whole reconcile.
- Treat a missing or corrupt state file as "start fresh" — never crash on it.
