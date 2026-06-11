# Reading messages & reactions

Both read tools return **oldest-first** messages and share the same time-window
semantics. Each result message carries `ts`, `user`, `text`, optional
`thread_ts`, and any `reactions`.

> **Time is Sydney-local.** `since` is parsed as `YYYY-MM-DD HH:mm:ss` in
> Australia/Sydney time and defaults to **24 hours ago**. Both tools fetch at
> most **100 messages** (no pagination) — narrow with `since`/`thread` rather
> than expecting deep history.

## `slack.fetch-msgs` — read a channel or thread

*Fetch messages from a Slack channel or thread, oldest first.*

| Arg | Required | Meaning |
|---|---|---|
| `channel` | yes | Channel name (with or without `#`) or channel ID. |
| `thread` | no | A parent `ts` — fetch that thread's replies instead of channel history. |
| `since` | no | Exclude messages before this Sydney datetime (`YYYY-MM-DD HH:mm:ss`). Default: 24h ago. |

- With `thread`, returns the thread's replies; otherwise channel history.
- Slack returns newest-first; the tool reverses to oldest-first for you.

```bash
murtaugh slack fetch-msgs --channel "#releases" --since "2026-06-10 09:00:00"
murtaugh slack fetch-msgs --channel C123 --thread 1700000000.000100
```

## `slack.fetch-reactions` — find what a user reacted to

*Fetch messages a specific user reacted to with a given emoji.*

| Arg | Required | Meaning |
|---|---|---|
| `from` | yes | User handle (with or without `@`). |
| `emoji` | yes | Emoji name, with or without colons (`thumbsup` or `:thumbsup:`). |
| `channel` | yes | Channel name (with or without `#`) or channel ID. |
| `since` | no | Same Sydney-time window as above. Default: 24h ago. |

- Fetches recent channel history (≤100) and keeps only messages where `from`
  reacted with `emoji`. Colons are stripped from the emoji, so `:thumbsup:` and
  `thumbsup` are equivalent.
- Use it for lightweight approvals — e.g. "which release notes did @lead 👍?".

```bash
murtaugh slack fetch-reactions --from @lead --emoji thumbsup \
  --channel "#releases" --since "2026-06-09 00:00:00"
```
