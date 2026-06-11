# Skill: Murtaugh Slack Tools

The programmatic way an agent or automation **acts on Slack** through Murtaugh:
four tools to post, update, and read messages. They're exposed both on the CLI
(`murtaugh slack <tool> …`) and over MCP (`slack.<tool>`), backed by the
gateway's bot token — so a script never needs a raw Slack token of its own. Use
these whenever a task posts a status card, edits a message in place, or reads
recent messages/reactions.

> These are the **active** Slack surface (you call them). For **reactive** Slack
> — buttons and link previews — see the `murtaugh-slack` (workflow-rules) and
> `murtaugh-unfurl` skills.

## The four tools (at a glance)

| Tool | Does | Key args |
|---|---|---|
| `slack.send-msg` | post a message (blocks or text, optional file) | `to`, `body` |
| `slack.update-msg` | replace an existing message's content | `channel`, `ts` |
| `slack.fetch-msgs` | read a channel or thread, oldest-first | `channel` |
| `slack.fetch-reactions` | find messages a user reacted to with an emoji | `from`, `emoji`, `channel` |

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Posting or updating a message (blocks, files, threads, mentions) | `reference/sending.md` |
| Reading messages or reactions (channels, threads, time windows) | `reference/fetching.md` |

## Invocation

- **CLI:** `murtaugh slack send-msg --to "#dev" --body "hi"`. Flags are the
  schema property names in kebab-case (`attachment_type` → `--attachment-type`).
- **MCP:** the same tools appear as `slack.send-msg`, `slack.fetch-msgs`, etc.;
  pass the schema properties as the tool arguments.

## Global guidelines (defaults — follow unless the user says otherwise)

- **One message per entity: post once, then `update-msg` in place** against the
  stored `ts`. Don't repost on every tick. The pattern and state-file conventions
  live in the `murtaugh-slack` skill (`reference/outbound.md`, `automations.md`).
- **Store the `ts`** that `send-msg` returns — it's how you update or thread later.
- **Mentions need the user ID**, rendered `<@U…>`; `send-msg` also expands
  `@handle` in `body` best-effort (see `reference/sending.md`).
- **Time windows are Sydney-local.** `fetch-*` `since` is parsed as Sydney time
  and defaults to the last 24h; both fetch tools cap at 100 messages.
