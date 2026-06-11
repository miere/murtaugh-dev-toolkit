# Inbound: handling interactions

How Murtaugh turns a button click into a response. This is the *interaction* half
of the dance (steps 2–4). For composing and sending the message that carries the
buttons, see `outbound.md`; for the blocks themselves, see `blocks.md`.

> **Buttons only.** Murtaugh handles `block_actions` interactions. Modal dialogs
> (`views.open` / `view_submission`) are not supported yet.

## How it works

1. Your agent posts a message whose blocks include interactive elements — usually
   an `actions` block with one or more buttons.
2. Each button carries an `action_id`, and the enclosing block an optional
   `block_id`. These are your **routing keys**.
3. When a user clicks, Slack delivers an `interactive` event of type
   `block_actions` to Murtaugh over Socket Mode.
4. Murtaugh evaluates each `workflow-rules` entry in sorted-key order and runs the
   **first** whose `match` is a subset of the interaction payload.
5. The matched rule's triggers fire in order: `reply-to-slack` posts a templated
   response, and/or `run` invokes a command with the interaction JSON on stdin.

## Stable routing keys

Give every actionable element a stable `action_id` (and its block a `block_id`) so
rules can target it precisely. **Check `slack.yaml` for keys that already exist and
reuse them** — e.g. the code-review flow already uses `block_id: github_pull_request`
with `action_id`s `approve_only` and `approve_merge`. Inventing parallel keys for
the same behaviour leaves your buttons unwired.

## Matching the interaction

`match` is a partial (subset) match — specify only the fields you care about:

```yaml
workflow-rules:
  pr-approve:
    request_event: interactive
    match:
      type: block_actions
      actions:
        - block_id: github_pull_request
          action_id: approve_only
    trigger:
      - reply-to-slack:
          template: code-review/02-approved.json
      - run:
          cmd: /path/to/agent
          args: [github, approve-pr]
          timeout: 30s
```

Scope a rule to a channel by adding `channel: { name: nc-code-reviews }` to
`match`. Rules are evaluated in **sorted key order**; the first match wins, so name
more-specific rules to sort ahead of catch-alls.

## Responding

- **`reply-to-slack`** renders a Block Kit JSON template (resolved against your
  config dir first, then the embedded defaults) and POSTs it to the interaction's
  `response_url`. Templates use Go `text/template` with the payload under
  `.Payload`, e.g. `{{ .Payload.message.ts }}`. Set `"replace_original": true` in
  the template to overwrite the clicked message (see
  `templates/code-review/02-approved.json`).
- **`run`** spawns a command and writes the full Slack interaction callback to its
  stdin as JSON. Use it for side effects (calling the GitHub API, etc.). It does
  **not** post a reply on its own — pair it with `reply-to-slack` if the user needs
  feedback.

A rule may list both triggers; they execute in the order written.

## Security — confirm with the user before you design

Slack delivers Block Kit messages to **every member of the channel** they're posted
to. The `admin_user` / `allowed_users` allowlist gates *who can act* (clicks from
outsiders are silently dropped), but does **not** control *who can see*. Before
drafting a form, walk the user through:

- **Visibility — who should see the buttons?** A public post is visible to every
  channel member. For one recipient, use an ephemeral message (in-channel, only
  that user, dismissible) or a DM. Modals (inherently single-user) are not
  supported.
- **Actors — who is in `allowed_users`?** Anyone outside the allowlist is silently
  ignored on click; confirm the intended actors are allowlisted, or the form is
  inert for them.
- **Payload — what's in `action_id`, `block_id`, button `value`?** These travel
  inside the message JSON and are readable by anyone who can see the message. Never
  embed secrets, tokens, or PII; keep keys opaque and resolve them server-side.
- **Side effects — what does `run` do?** The command receives the full payload on
  stdin and can have real-world effects. Make sure the user understands the blast
  radius of a click — consider a confirmation step for destructive actions.

If any answer is fuzzy, refine the form first (narrow the channel, switch to
ephemeral, scope `allowed_users`, gate destructive actions) rather than ship-and-see.

## Related

- A worked, wired example lives in `slack.yaml` (`workflow-rules.code-review-approval`)
  with its template at `templates/code-review/02-approved.json` — see `examples/`.
- Unfurling bare URLs into rich previews is a *different* mechanism (`unfurl-rules`
  + `link_shared`), covered by the separate `murtaugh-unfurl` skill.
