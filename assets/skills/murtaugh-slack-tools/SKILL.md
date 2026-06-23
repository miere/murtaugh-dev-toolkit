---
name: murtaugh-slack-tools
description: The active Slack surface for Murtaugh — four tools an agent or automation calls to programmatically post, update, and read Slack messages and reactions, exposed both on the CLI (`murtaugh slack <tool>`) and over MCP (`slack_<tool>`), backed by the gateway bot token so no raw Slack token is needed. Use when a task must post or update a status message/card (slack_send-msg, slack_update-msg), read recent channel or thread messages (slack_fetch-msgs), or find messages a user reacted to with an emoji (slack_fetch-reactions). Distinct from murtaugh-slack, which covers the reactive/workflow-rules side (buttons, link previews); load this skill for outbound posting and inbound reading driven by the agent itself.
---

# Skill: Murtaugh Slack Tools

The programmatic way an agent or automation **acts on Slack** through Murtaugh:
four tools to post, update, and read messages. They're exposed both on the CLI
(`murtaugh slack <tool> …`) and over MCP (`slack_<tool>`), backed by the
gateway's bot token — so a script never needs a raw Slack token of its own. Use
these whenever a task posts a status card, edits a message in place, or reads
recent messages/reactions.

> These are the **active** Slack surface (you call them). For **reactive** Slack
> — buttons and link previews — see the `murtaugh-slack` (workflow-rules) and
> `murtaugh-unfurl` skills.

> **To *ask*, don't post.** This skill's `slack.*` inventory is scoped to that
> namespace — posting and reading messages. To **ask the user a question and get the
> answer back**, use the separate `ask` / `present_plan` tools, not a plain
> `slack_send-msg`: they post the buttons/modal and **block the turn** until the user
> responds, then return the choice to the agent. A `send-msg` is fire-and-forget — it
> posts and returns immediately, with no answer to wait on. (`ask` / `present_plan`
> live outside the `slack.*` namespace; see the `murtaugh-agents` skill and
> `agents.yaml`.)

## The four tools (at a glance)

| Tool | Does | Key args |
|---|---|---|
| `slack_send-msg` | post a message (blocks or text, optional file) | `to`, `body` |
| `slack_update-msg` | replace an existing message's content | `channel`, `ts` |
| `slack_fetch-msgs` | read a channel or thread, oldest-first | `channel` |
| `slack_fetch-reactions` | find messages a user reacted to with an emoji | `from`, `emoji`, `channel` |

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Posting or updating a message (blocks, files, threads, mentions) | `reference/sending.md` |
| Reading messages or reactions (channels, threads, time windows) | `reference/fetching.md` |

## Invocation

- **CLI:** `murtaugh slack send-msg --to "#dev" --body "hi"`. Flags are the
  schema property names in kebab-case (`attachment_type` → `--attachment-type`)
  and every flag carries a value (there are no bare switches).
- **MCP:** the same tools appear as `slack_send-msg`, `slack_fetch-msgs`, etc.;
  pass the schema properties as the tool arguments.
- **Full flag reference:** `murtaugh help slack <tool>` (e.g.
  `murtaugh help slack send-msg`) or `murtaugh slack <tool> --help` — required
  vs optional flags, the `#channel`/`@user`/ID `--to` forms, `--blocks`
  (inline JSON or file path), mutual exclusions, and examples.

## Global guidelines (defaults — follow unless the user says otherwise)

- **One message per entity: post once, then `update-msg` in place** against the
  stored `ts`. Don't repost on every tick. The pattern and state-file conventions
  live in the `murtaugh-slack` skill (`reference/outbound.md`, `automations.md`).
- **Store the `ts`** that `send-msg` returns — it's how you update or thread later.
- **Mentions need the user ID**, rendered `<@U…>`; `send-msg` also expands
  `@handle` in `body` best-effort (see `reference/sending.md`).
- **Time windows are Sydney-local.** `fetch-*` `since` is parsed as Sydney time
  and defaults to the last 24h; both fetch tools cap at 100 messages.
