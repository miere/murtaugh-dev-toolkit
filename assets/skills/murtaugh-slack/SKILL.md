---
name: murtaugh-slack
description: Everything Slack on Murtaugh — post/update/read messages and reactions, ask the user and block for the answer, compose Block Kit, and (operator) wire reactive buttons and link previews. Read only the row your task needs.
requires: [slack, ask, present_plan, manage]
templated: true
files:
  reference/messaging.md:      { requires: [slack],            summary: "post, update, and read messages & reactions" }
  reference/asking.md:         { requires: [ask, present_plan], summary: "ask the user a question / get plan sign-off and block for the answer" }
  reference/blocks.md:         { requires: [slack, manage],    summary: "compose Block Kit (sections, actions, plan, card)" }
  reference/automations.md:    { requires: [manage],           summary: "conventions for scheduled clock-tick scripts that post to Slack" }
  reference/workflow-rules.md: { requires: [manage],           summary: "wire what happens on a button click in workflow-rules.yaml" }
  reference/unfurl.md:         { requires: [manage],           summary: "turn posted links into rich previews in unfurl-rules.yaml" }
  examples/unfurl/:            { requires: [manage] }
---

# Skill: Murtaugh Slack

Everything Slack flows through Murtaugh — posting and reading messages, asking the
user, composing Block Kit, and (for operators) wiring reactive rules. Read only
the file your task needs:

{{FILES}}

> If a task needs something not listed above, it's outside what you can do here —
> often an operator config change in `gateway.yaml` (or a sibling like
> `workflow-rules.yaml` / `unfurl-rules.yaml`). Say so and stop; don't try to
> edit config files yourself.

## Guidelines (defaults — follow unless the user says otherwise)

- **One message per entity** — post once, then `update-msg` in place against the
  stored `ts`; use a thread reply for follow-ups. Don't repost on every tick.
- **No secrets in a message** — never put tokens or PII in `action_id`,
  `block_id`, or a button `value`; they travel inside the message and are
  readable by anyone who can see it.
- **A channel post is visible to every member of the channel.** The allowlist
  gates *who can act*, not *who can see* — for single-recipient delivery use an
  ephemeral message or a DM.
- **To *ask*, don't post.** A `send-msg` is fire-and-forget; to get an answer
  back use `ask` / `present_plan`, which block the turn.
