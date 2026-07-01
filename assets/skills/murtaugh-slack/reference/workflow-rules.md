# Workflow-rules: handling button clicks

How Murtaugh turns a button click into a response — the *reactive* half of Slack.
For composing and sending the message that carries the buttons, see
`messaging.md`; for the blocks themselves, see `blocks.md`. This is operator
config (it edits `workflow-rules.yaml`).

> **`workflow-rules` are buttons-only.** A `workflow-rules` entry can only match
> `block_actions` interactions; it **cannot** trigger on a modal `view_submission`.
> This is a limitation of the *rules* surface, **not** the daemon: Murtaugh's native
> interaction broker (backing the `ask` / `present_plan` tools and the terminal
> approval gate — see `asking.md`) *does* open real modals and parse their
> `view_submission`s — the gateway routes those to the blocked agent turn before
> workflow-rules ever see them. So if you need a multi-field prompt, reach for the
> `ask` tool (it opens a modal and returns the answers) rather than trying to wire
> one through a workflow-rule.

## How it works

1. Your agent posts a message whose blocks include interactive elements — usually
   an `actions` block with one or more buttons.
2. Each button carries an `action_id`, and the enclosing block an optional
   `block_id`. These are your **routing keys**.
3. When a user clicks, Slack delivers an `interactive` event of type
   `block_actions` to Murtaugh over Socket Mode.
4. Murtaugh evaluates each `workflow-rules` entry in sorted-key order and runs the
   **first** whose `match` is a subset of the interaction payload.
5. The matched rule's triggers fire in order: `reply-to-slack` posts a reply
   (from a template, a command, or an agent), `run` invokes a command with the
   interaction JSON on stdin, and `delegate-to-agent` starts a real chat turn in
   the button's thread.

## Stable routing keys

Give every actionable element a stable `action_id` (and its block a `block_id`) so
rules can target it precisely. **Check `workflow-rules.yaml` for keys that already
exist and reuse them** — e.g. the code-review flow already uses `block_id: github_pull_request`
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

- **`reply-to-slack`** produces a Block Kit JSON reply and POSTs it to the
  interaction's `response_url`. The JSON comes from exactly one of: a `template`
  (Go `text/template`, resolved against your config dir first then the embedded
  defaults, payload under `.Payload`, e.g. `{{ .Payload.message.ts }}`); the
  stdout of a `run:` command; or a nested `delegate-to-agent:` (the agent must
  return solely valid Slack message JSON — non-JSON is logged and skipped). Set
  `"replace_original": true` to overwrite the clicked message (see
  `templates/code-review/02-approved.json`).
- **`run`** spawns a command for side effects (calling the GitHub API, etc.). It
  does **not** post a reply on its own — pair it with `reply-to-slack` if the
  user needs feedback. Two ways to get the interaction data into the command:
  - **stdin** — the raw Slack interaction callback (exactly what Slack sent) is
    written to the command's stdin as JSON. Parse it with e.g.
    `jq -r '.actions[0].value'`.
  - **templated `cmd`/`args`** — both are rendered with Go `text/template`, the
    payload under `.Payload` (same as templates/prompts). So
    `args: ["-c", "gh pr review {{ (index .Payload.actions 0).value }}"]` works.
    An unresolved placeholder fails the rule loudly rather than running a
    half-rendered command. (`timeout` is never templated.)
- **`delegate-to-agent`** (top-level) starts a **real chat turn in the thread the
  button lives in** — the same pipeline a Slack @mention drives: the reply streams
  into the thread, it's journaled, it can use the approval gate / `ask`, and it's
  bound to that thread's session. This is what you want for "review this PR" style
  buttons: it's visible and steerable, not a silent background run. The `prompt` is
  rendered with the payload under `.Payload`. `agent` is an **optional override**:
  omit it to use the channel's normal routing (the agent a real mention in that
  channel would reach), or name one (must be defined in `agents.yaml`) to pin it.
  Requires chat/ACP to be enabled.

  > Do **not** try to trigger the agent by having a `run:` command post a message
  > that @mentions the bot (even `--as admin`): Slack does not deliver an
  > `app_mention` back to the app for a message that same app authored, so the
  > agent never wakes. `delegate-to-agent` starts the turn in-process and sidesteps
  > that entirely.

A rule may list several triggers; they execute in the order written.

```yaml
    trigger:
      - reply-to-slack:
          delegate-to-agent:           # agent returns the JSON reply
            agent: default
            prompt: "Acknowledge {{ .Payload.user.id }}; return solely Slack JSON."
      - delegate-to-agent:             # starts a chat turn in the button's thread
          # agent omitted → use the channel's normal routing
          prompt: "Review the Pull Request {{ (index .Payload.actions 0).value }} (owner/repo#N — resolve with gh) and post your findings here."
```

## Security — confirm with the user before you design

Slack delivers Block Kit messages to **every member of the channel** they're posted
to. The `admin_user` / `allowed_users` allowlist gates *who can act* (clicks from
outsiders are silently dropped), but does **not** control *who can see*.

> Before you hand-build an "approval form" out of buttons + a workflow-rule, check
> whether you actually want the agent to *ask*. For agent-driven approval and
> decisions, prefer the `present_plan` tool (plan sign-off), the `ask` tool (a
> question with options, or a multi-field modal), or the terminal **approval gate**
> — all in `asking.md`. Those block the turn and return the user's choice directly —
> no rule wiring, and no secrets travelling in `value`. Hand-wired forms are for
> *standalone* reactive flows that aren't part of an agent turn (PR action cards,
> status mirrors).

Before drafting a hand-wired form, walk the user through:

- **Visibility — who should see the buttons?** A public post is visible to every
  channel member. For one recipient, use an ephemeral message (in-channel, only
  that user, dismissible) or a DM. (A *workflow-rule* can't drive a modal; if you
  need a single-user modal prompt, that's the `ask` tool's job, not a hand-wired form.)
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

- A worked, wired example lives in `workflow-rules.yaml` (the `code-review-approval`
  rule) with its template at `templates/code-review/02-approved.json` — see `examples/`.
- Unfurling bare URLs into rich previews is a *different* mechanism (`unfurl-rules`
  + `link_shared`), covered by `unfurl.md` in this skill.
