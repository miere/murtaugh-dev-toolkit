# Skill: GitHub Forms Workflow

Murtaugh can turn a Block Kit message into an interactive form: your agent posts a
message with buttons, and when a user clicks one, Murtaugh routes the interaction
to a `workflow-rules` entry that replies and/or runs a command. This is how you
build approve/reject cards, GitHub PR actions, and similar lightweight forms.

> **Buttons only.** Murtaugh currently handles `block_actions` interactions
> (buttons and other action elements that fire immediately). Modal dialogs
> (`views.open` / `view_submission`) are **not** supported yet, so design your
> forms around message buttons rather than pop-up modals.

## How it works

1. Your agent posts a message whose blocks include interactive elements — usually
   an `actions` block with one or more buttons.
2. Each button carries an `action_id` (and the enclosing block an optional
   `block_id`). These are your **routing keys**.
3. When a user clicks a button, Slack delivers an `interactive` event of type
   `block_actions` to Murtaugh over Socket Mode.
4. Murtaugh evaluates each `workflow-rules` entry in sorted-key order and runs the
   first whose `match` is a subset of the interaction payload.
5. The matched rule's triggers fire in order: `reply-to-slack` posts a templated
   response, and/or `run` invokes a command with the interaction JSON on stdin.

## Security — confirm with the user before you design

Slack delivers Block Kit messages to **every member of the channel** they're posted to. Murtaugh's `admin_user` / `allowed_users` allowlist gates *who can act* on the form (clicks from outsiders are silently dropped), but it does **not** control *who can see* it. Before drafting the form, walk the user through:

- **Visibility — who should see the buttons?** A public channel post is visible to every channel member. For one-recipient delivery, use `chat.postEphemeral` (in-channel, only that user, dismissible) or a DM. Modal dialogs (which would be inherently single-user) are not yet supported.
- **Actors — who is in `allowed_users`?** Anyone outside the allowlist will be silently ignored on click; confirm the intended actors are allowlisted, or the form is inert for them.
- **Payload — what's in `action_id`, `block_id`, and button `value`?** These travel inside the message JSON and are readable by anyone who can see the message. Never embed secrets, tokens, or PII; keep keys opaque and resolve them server-side.
- **Side effects — what does `run` do?** The command receives the full interaction payload on stdin and can have real-world effects (API calls, file writes). Make sure the user understands the blast radius of a click before wiring it up — and consider a confirmation step for destructive actions.

If any answer is fuzzy, refine the form first (narrow the channel, switch to ephemeral, scope `allowed_users`, gate destructive actions behind confirmation) rather than ship-and-see.

## Designing the blocks

Compose your form with the standard Block Kit primitives and give every actionable
element a stable `action_id` / `block_id` so rules can target it precisely:

```json
[
  {
    "type": "section",
    "text": { "type": "mrkdwn", "text": "*<https://github.com/acme/app/pull/42|PR #42>* — Add retries" }
  },
  {
    "type": "actions",
    "block_id": "github_pull_request",
    "elements": [
      { "type": "button", "action_id": "approve", "style": "primary",
        "text": { "type": "plain_text", "text": "Approve" } },
      { "type": "button", "action_id": "request_changes", "style": "danger",
        "text": { "type": "plain_text", "text": "Request changes" } }
    ]
  }
]
```

- **Block Kit reference:** https://docs.slack.dev/reference/block-kit
- **Available blocks:** https://docs.slack.dev/reference/block-kit/blocks

## Matching the interaction

A `workflow-rules` entry matches on the interaction `type` plus the `block_id` /
`action_id` of the clicked element. `match` is a partial (subset) match, so you only
specify the fields you care about:

```yaml
workflow-rules:
  pr-approve:
    request_event: interactive
    match:
      type: block_actions
      actions:
        - block_id: github_pull_request
          action_id: approve
    trigger:
      - reply-to-slack:
          template: github/pr-approved.json
      - run:
          cmd: /path/to/agent
          args: [github, approve-pr]
          timeout: 30s
```

You can also scope a rule to a channel by adding `channel: { name: nc-code-reviews }`
to `match`.

## Responding

- **`reply-to-slack`** renders a Block Kit JSON template (resolved against your
  config dir, then the embedded defaults) and POSTs it to the interaction's
  `response_url`. Templates use Go `text/template` with the payload under
  `.Payload`, e.g. `{{ .Payload.message.ts }}`.
- **`run`** spawns a command and writes the full Slack interaction callback to its
  stdin as JSON. Use it for side effects (e.g. calling the GitHub API). It does not
  post a reply on its own — pair it with `reply-to-slack` if the user needs feedback.

A rule may list both triggers; they execute in the order written.
