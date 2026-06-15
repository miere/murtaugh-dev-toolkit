# Sending & updating messages

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

> On the CLI these are kebab flags carrying a value: `--body`, `--to`,
> `--blocks`, `--attachment`, `--attachment-type`, `--thread`. Run
> `murtaugh help slack send-msg` for the canonical reference.

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
  For reliability, render `<@U…>` yourself.

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
arg** (update takes `--body` and/or `--blocks` only).

Behavior:
- A `channel` starting with `#` is resolved via `conversations.list`; anything
  else (including raw IDs like `C123ABC`) is used as-is — pass the stored channel
  ID to skip the lookup.

```bash
murtaugh slack update-msg --channel C123ABC --ts 1700000000.000100 \
  --blocks /path/to/card.json --body "Deploy complete"
```

> A stored `ts` can go stale (message deleted). The `murtaugh-slack` skill's
> `outbound.md` covers the re-post-on-failure pattern for resilient reconcile loops.
