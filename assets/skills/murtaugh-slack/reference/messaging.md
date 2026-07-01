# Messaging: post, update, and read

The **active** Slack surface: how an agent or automation posts, updates, and
reads messages through Murtaugh. Four tools — `slack_send-msg`,
`slack_update-msg`, `slack_fetch-msgs`, `slack_fetch-reactions` — on the CLI
(`murtaugh slack <tool> …`) and over MCP (`slack_<tool>`), backed by the
gateway's bot token, so a script never needs a raw Slack token of its own. For
the Block Kit you put in these messages see `blocks.md`.

> **To *ask*, don't post.** These tools are fire-and-forget — they post or read
> and return immediately, with no answer to wait on. To **ask the user a question
> and get the answer back**, use the `ask` / `present_plan` tools instead (see
> `asking.md`): they block the turn until the user responds.

## The four tools (at a glance)

| Tool | Does | Slack method | Key args |
|---|---|---|---|
| `slack_send-msg` | post a message (blocks or text, optional file) | `chat.postMessage` | `to`, `body` |
| `slack_update-msg` | replace an existing message's content | `chat.update` | `channel`, `ts` |
| `slack_fetch-msgs` | read a channel or thread, oldest-first | `conversations.history`/`replies` | `channel` |
| `slack_fetch-reactions` | find messages a user reacted to with an emoji | `conversations.history` | `from`, `emoji`, `channel` |

> On the CLI these are kebab flags carrying a value (`--to`, `--body`,
> `--blocks`, `--attachment-type`, …) — there are no bare switches. Run
> `murtaugh help slack <tool>` for the canonical reference (required vs optional
> flags, the `#channel`/`@user`/ID `--to` forms, mutual exclusions, examples).

## `slack_send-msg` — post a message

*Send a message (optionally with an attachment) to a Slack channel or user.*

| Arg | Required | Meaning |
|---|---|---|
| `body` | yes | Message text. Also the notification fallback when `blocks` are set. |
| `to` | yes | Destination: `#channel`, `@user`, or a `C`/`G`/`D` Slack ID. |
| `blocks` | no | Block Kit: a JSON string (starts with `[` or `{`) **or** a path to a JSON file. Mutually exclusive with `attachment`. |
| `attachment` | no | Path to a file to upload. Mutually exclusive with `blocks`. |
| `attachment_type` | no | Snippet type for the uploaded attachment. Closed enum — the only accepted value is `markdown`. |
| `thread` | no | Parent message `ts` to reply in-thread. |
| `as` | no | Sender identity. `bot` (default) posts as the app; `admin` posts as the human admin via their Slack user token. Closed enum: `bot` \| `admin`. |

Returns `{ ok, channel, ts, to }` — **store `ts`** to update or thread later.

Behavior:
- **Destination resolution:** `#name` → channel ID via `conversations.list`;
  `@handle` → user (matched case-insensitively against username, then display
  name, then real name), and a DM is opened automatically; a raw `C`/`G`/`D` ID
  is used directly.
- **Blocks vs attachment** are mutually exclusive; `blocks` JSON is validated
  before posting; `attachment` must be a file that exists on disk.
- **Mention expansion:** `@handle` tokens in `body` are resolved to `<@U…>`
  best-effort; unresolved handles are left as plain text (with a stderr warning).
  For reliability, render `<@U…>` yourself (see Mentions below).
- **Posting as the admin (`as: "admin"`):** posts through the admin's own Slack
  user token (`oauth.user_token`), so the message shows the admin's **real
  identity** — indistinguishable from one they typed themselves, and not marked
  app-generated. Default to `bot`; reach for `admin` only when speaking as the
  human is explicitly intended. If `oauth.user_token` is unset the call errors —
  it never silently falls back to the bot.

```bash
murtaugh slack send-msg --to "#dev" --body "Deploy started" \
  --blocks /path/to/card.json --thread 1700000000.000100
```

## `slack_update-msg` — replace a message's content

*Update an existing message in a Slack channel.*

| Arg | Required | Meaning |
|---|---|---|
| `channel` | yes | Channel ID, or a channel name with a leading `#`. |
| `ts` | yes | Timestamp of the message to update. |
| `body` | no | Fallback text. Defaults to `"Message updated"`. |
| `blocks` | no | Block Kit JSON string or file path (same as `send-msg`). |

Returns `{ ok, channel, ts }`. Updates the original message in place — there is
**no thread arg** (you can't move a message into a thread) and **no attachment
arg** (update takes `--body` and/or `--blocks` only). A `channel` starting with
`#` is resolved via `conversations.list`; anything else (including raw IDs like
`C123ABC`) is used as-is — pass the stored channel ID to skip the lookup.

```bash
murtaugh slack update-msg --channel C123ABC --ts 1700000000.000100 \
  --blocks /path/to/card.json --body "Deploy complete"
```

## One message per entity (the core pattern)

The default for any status/lifecycle surface: **post once, then update in place.**
Never repost on every tick.

1. Compute a stable key for the entity (e.g. `repo#number`).
2. Look up the key in a small state store (JSON file — see `automations.md`).
   - **Not seen →** `send-msg`, then save `{ key: { ts, ...flags } }`.
   - **Seen →** `update-msg` against the stored `ts` with freshly-rendered blocks.
3. Use a **thread reply** (`thread` = the stored `ts`) for follow-ups that should
   notify or accrue over time — e.g. tagging a reviewer when a PR becomes ready.
   Gate "post once" follow-ups behind a flag in the state store so a per-minute
   job doesn't re-tag every run.

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
(not `@miere`) in `body`. Two ways to get the ID:

- **Resolve at runtime** — `users.lookupByEmail` (most reliable) or scan
  `users.list`. Cache the result.
- **Inject via config** — read it from an env var / config so the script stays
  declarative. (Known mapping in this workspace: `@Miere` = `U0B20G0ET9T`.)

Put the mention in the message `body` so the notification fires even if the
block rendering is collapsed.

## Reading: `slack_fetch-msgs` and `slack_fetch-reactions`

Both read tools return **oldest-first** messages and share the same time-window
semantics. Each result message carries `ts`, `user`, `text`, optional
`thread_ts`, and any `reactions`.

> **Time is Sydney-local.** `since` is parsed as `YYYY-MM-DD HH:mm:ss` in
> Australia/Sydney time and defaults to **24 hours ago**. Both tools fetch at
> most **100 messages** (no pagination) — narrow with `since`/`thread` rather
> than expecting deep history.

### `slack_fetch-msgs` — read a channel or thread

*Fetch messages from a Slack channel or thread, oldest first.*

| Arg | Required | Meaning |
|---|---|---|
| `channel` | yes | Channel name (with or without `#`) or channel ID. |
| `thread` | no | A parent `ts` — fetch that thread's replies instead of channel history. |
| `since` | no | Exclude messages before this Sydney datetime (`YYYY-MM-DD HH:mm:ss`). Default: 24h ago. |

With `thread`, returns the thread's replies; otherwise channel history. Slack
returns newest-first; the tool reverses to oldest-first for you.

```bash
murtaugh slack fetch-msgs --channel "#releases" --since "2026-06-10 09:00:00"
murtaugh slack fetch-msgs --channel C123 --thread 1700000000.000100
```

### `slack_fetch-reactions` — find what a user reacted to

*Fetch messages a specific user reacted to with a given emoji.*

| Arg | Required | Meaning |
|---|---|---|
| `from` | yes | User handle (with or without `@`). |
| `emoji` | yes | Emoji name, with or without colons (`thumbsup` or `:thumbsup:`). |
| `channel` | yes | Channel name (with or without `#`) or channel ID. |
| `since` | no | Same Sydney-time window as above. Default: 24h ago. |

Fetches recent channel history (≤100) and keeps only messages where `from`
reacted with `emoji`. Colons are stripped, so `:thumbsup:` and `thumbsup` are
equivalent. Use it for lightweight approvals — e.g. "which release notes did
@lead 👍?".

```bash
murtaugh slack fetch-reactions --from @lead --emoji thumbsup \
  --channel "#releases" --since "2026-06-09 00:00:00"
```

## Resilience

- A stored `ts` can go stale (message deleted). If `update-msg` fails with
  `message_not_found`, **re-post** and refresh the stored `ts`.
- On a per-entity failure, log and continue with the others; don't let one bad
  entity abort the whole reconcile.
- Treat a missing or corrupt state file as "start fresh" — never crash on it.
