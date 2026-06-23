---
name: murtaugh-slack
description: End-to-end guide for building reactive Slack experiences on Murtaugh, where an agent posts a Block Kit message, a user interacts with it, and Murtaugh's server handles the block_actions click and renders a response via slack.yaml workflow-rules. Covers the inbound/reactive surface — matching workflow-rules by block_id/action_id (workflow-rules fire on block_actions only; they cannot match modal view_submissions), firing reply-to-slack, run, and delegate-to-agent triggers, designing message buttons, and scheduled clock-tick automations. Use when wiring what happens on a Slack button click, defining or reusing action_ids and match rules in slack.yaml, building approval forms, PR action cards, status mirrors, or unfurl/ping-style interactive handlers. To have the AGENT itself ask the user a question (incl. a multi-field modal) and block for the answer, use the `ask`/`present_plan` tools instead of hand-wiring a button. For actively posting, updating, or reading messages from a script, see murtaugh-slack-tools instead.
---

# Skill: Murtaugh Slack Workflows

End-to-end guide for building Slack experiences on Murtaugh: an agent **posts** a
Block Kit message, a user **interacts** with it, Murtaugh's server **handles** the
interaction and **renders** a response. Use this whenever a task involves sending,
updating, or reacting to Slack messages through Murtaugh — code-review cards, PR
actions, approval forms, status mirrors, and similar.

> **`workflow-rules` are buttons-only.** A `slack.yaml` `workflow-rules` entry
> matches `block_actions` interactions (buttons and other elements that fire
> immediately) — it **cannot** match a modal `view_submission`. So design *rule-driven*
> flows around message buttons, not pop-ups.
>
> **Want the agent to *ask* the user something?** Don't hand-wire a button plus a
> matching workflow-rule. Have the agent call the `ask` tool (for a question with
> options, or a multi-field modal) or `present_plan` (for plan sign-off): these
> post the buttons/modal and **block the turn** until the user answers, then hand
> the choice straight back to the agent. They go through Murtaugh's native
> interaction broker — a separate path from `slack.yaml` — and the daemon *does*
> open real modals there (`views.open` / `view_submission`). See `agents.yaml`
> (`tools: [… ask, present_plan]`) and the `murtaugh-agents` skill.

## The dance (mental model)

1. **Author & send** — your agent (often a script) composes blocks and posts a
   message, or updates one it posted earlier. → `reference/outbound.md`
2. **Interact** — Slack delivers the click to Murtaugh over Socket Mode as a
   `block_actions` event, keyed by `block_id` / `action_id`.
3. **Handle** — Murtaugh matches the first `workflow-rules` entry whose `match` is
   a subset of the payload. → `reference/inbound.md`
4. **Render** — the rule's triggers fire: `reply-to-slack` posts a reply (from a
   template, command, or agent), `run` invokes a command for side effects, and
   `delegate-to-agent` hands the work to an agent fire-and-forget. →
   `reference/inbound.md`

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Posting / updating / threading / DMing a message | `reference/outbound.md` |
| Choosing or composing blocks (incl. `plan`, `card`) | `reference/blocks.md` |
| Wiring what happens on a click | `reference/inbound.md` |
| Writing a scheduled / clock-tick automation | `reference/automations.md` |
| Wanting a working example | `examples/` (live templates) |

## Global guidelines (defaults — follow unless the user says otherwise)

- **Read `slack.yaml` first.** It is the source of truth for the channel,
  `admin_user` / `allowed_users`, and the **existing `action_id`s already wired in
  `workflow-rules` (e.g. `approve_only`, `approve_merge`). Reuse them — do not
  invent new keys for behaviour that already exists.**
- **No implementation specified → write a Python script in `./automations/`**
  (overwrite an existing one with the same purpose). Document how to run it in a
  top-of-file docstring.
- **Secrets come from environment variables.** Never hardcode tokens, and never
  put secrets/PII in `action_id`, `block_id`, or button `value` — those travel
  inside the message and are readable by anyone who can see it.
- **One message per entity.** Post once, then **update in place** via a stored
  `ts`; use a **thread reply** for follow-ups (tags, results). See
  `reference/outbound.md`.
- **Confirm visibility, actors, and side-effects before shipping a form** (see the
  security section in `reference/inbound.md`). If any answer is fuzzy, tighten the
  form first rather than ship-and-see.

## Security in one line

A channel post is visible to **every member of the channel**. The
`admin_user` / `allowed_users` allowlist gates **who can act**, not **who can
see** — for single-recipient delivery use an ephemeral message or a DM. Full
checklist in `reference/inbound.md`.
