# Blocks: catalog & cheat-sheet

Murtaugh renders standard **Slack Block Kit** plus a couple of richer custom
blocks (`plan`, `card`). This file is the local catalog so you don't have to hit
the internet for the common cases. Live, copy-paste examples sit in `examples/`
(the `templates/` dir).

- Official spec (only when you need a block not listed here):
  https://docs.slack.dev/reference/block-kit/blocks
- `plan` block: https://docs.slack.dev/reference/block-kit/blocks/plan-block/

A message is `{ "blocks": [ … ] }` (optionally with a top-level `"text"` fallback
for notifications, and `"thread_ts"` to reply in a thread — see `messaging.md`).

## Useful blocks (the shortlist)

| Block | Use it for | Interactive? |
|---|---|---|
| `section` | A line/paragraph of `mrkdwn` or `plain_text`; optional `accessory` | accessory only |
| `actions` | A row of buttons (the form's controls) | **yes** |
| `context` | Small muted footnote (status, timestamps) | no |
| `divider` | Horizontal rule | no |
| `header` | Bold `plain_text` banner | no |
| `rich_text` | Structured text (sections, lists, code) — used inside `plan` task `details`/`output` | no |
| `card` *(custom)* | Titled card with icon, title, subtitle — good for a compact PR/entity header | no |
| `plan` *(custom)* | A checklist of tasks with per-task status — good for lifecycle/progress | no |

### `actions` — the interactive one

Every button needs a stable `action_id`; the block usually carries a `block_id`.
These are your routing keys (see `workflow-rules.md`) — **reuse the keys already in
`workflow-rules.yaml`**, and never put secrets in `value`.

```json
{
  "type": "actions",
  "block_id": "github_pull_request",
  "elements": [
    { "type": "button", "action_id": "approve_only",   "style": "primary", "value": "<repo>#<number>", "text": { "type": "plain_text", "text": "Approve" } },
    { "type": "button", "action_id": "approve_merge",                       "value": "<repo>#<number>", "text": { "type": "plain_text", "text": "Approve and Merge" } },
    { "type": "button", "action_id": "request_changes", "style": "danger",  "value": "<repo>#<number>", "text": { "type": "plain_text", "text": "Request changes" } }
  ]
}
```

Button `style` is `primary`, `danger`, or omitted (default). Interactive elements
live inside an `actions` block (or as a `section` `accessory`) — they do **not**
nest inside `plan`/`card`. To attach controls to a `plan` message, add a sibling
`actions` block, or post them as a thread reply (see the code-review example).

## Custom block: `card`

A compact header. Example from `templates/code-review/01-open-pull-request.json`:

```json
{
  "type": "card",
  "slack_icon": { "type": "icon", "name": "check" },
  "title":    { "type": "mrkdwn", "text": "HX-439: add ghost-user Cloud Tasks queues", "verbatim": false },
  "subtitle": { "type": "mrkdwn", "text": "Author: _chris-sciuto_", "verbatim": false }
}
```

## Custom block: `plan`

A task checklist with per-task status — ideal for showing where something sits in
a lifecycle. (Not to be confused with the agent's `present_plan` *tool*, which posts
a plan with Proceed / Revise / Cancel buttons and blocks the turn for sign-off — a
different surface entirely; see `asking.md`.)

**Fields:** `type` (`"plan"`), `title` (**object**: `{ "type": "plain_text",
"text": … }` — not a bare string), optional `block_id`, `tasks[]`.

**Task fields:** `task_id`, `title` (string), `status`, optional `sources[]`
(list of `{ "type": "url", "url", "text" }`), optional `details` and `output`
(both `rich_text` blocks).

> **Status enum is limited:** only `complete`, `in_progress`, and `pending` are
> supported. There is **no `failed`** status — convey a failure in the task's
> `details`/`output` text (or escalate it in the `title`), not via `status`.

```json
{
  "type": "plan",
  "title": { "type": "plain_text", "text": "<PR title>" },
  "block_id": "plan_<repo>_<number>",
  "tasks": [
    {
      "task_id": "task_1",
      "title": "Collect data about the PR",
      "status": "complete",
      "sources": [
        { "type": "url", "url": "<PR url>",     "text": "<repo>#<number>" },
        { "type": "url", "url": "<author url>", "text": "@<author>" }
      ]
    },
    {
      "task_id": "task_2",
      "title": "Continuous Integration feedback",
      "status": "in_progress",
      "details": { "type": "rich_text", "elements": [
        { "type": "rich_text_section", "elements": [
          { "type": "text", "text": "Mandatory automated assessments that ensure the software is shippable" } ] } ] }
    },
    {
      "task_id": "task_3",
      "title": "Human approval",
      "status": "pending",
      "details": { "type": "rich_text", "elements": [
        { "type": "rich_text_section", "elements": [
          { "type": "text", "text": "One final check to ensure everything is ready to be merged." } ] } ] }
    }
  ]
}
```

## Validate before you ship

Block Kit JSON is fiddly. Sanity-check structure with Slack's Block Kit Builder
(https://app.slack.com/block-kit-builder) — note it won't know the custom `plan`
/ `card` blocks, so validate those against the templates in `examples/`.
