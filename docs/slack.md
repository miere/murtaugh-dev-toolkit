# Slack

Everything Slack flows through Murtaugh: posting and reading messages, asking the
user a question, composing Block Kit, and (for operators) wiring reactive rules
and link previews. This page is organised by task — read the section you need.

- [Messaging](#messaging) — post, update, and read messages and reactions.
- [Asking the user](#asking-the-user) — get an answer back and block on it.
- [Block Kit](#block-kit) — compose rich messages.
- [Workflow rules](#workflow-rules) — react to button clicks and form submissions.
- [Link unfurling](#link-unfurling) — turn shared links into rich previews.

---

## Messaging

The same Slack capabilities the agent and MCP server use are available as
one-shot CLI tools under the `slack` namespace. They reuse the gateway's
`oauth.bot_token`, so no extra configuration is needed.

```sh
murtaugh slack send-msg --to '#general' --body 'hello'
murtaugh slack fetch-msgs --channel general
murtaugh slack fetch-reactions --channel general --from @ada --emoji thumbsup
murtaugh slack update-msg --channel C123 --ts 1234.5678 --body 'edited'
```

**Conventions** (the agent follows these; they're good practice for your own
automations too):

- **One message per entity.** Post once, store the `ts`, then `update-msg` in
  place; use a thread reply for follow-ups. Don't repost on every tick.
- **No secrets in a message.** Never put tokens or PII in `action_id`,
  `block_id`, or a button `value` — they travel inside the message and are
  readable by anyone who can see it.
- **A channel post is visible to everyone in the channel.** Access control gates
  *who can act*, not *who can see*. For single-recipient delivery use a DM or an
  ephemeral message.

---

## Asking the user

`send-msg` is fire-and-forget. To get an **answer back**, an agent uses the
blocking tools:

- **`ask`** — pose a question with options as clickable Slack buttons and wait
  for the user's choice before continuing.
- **`present_plan`** — show a multi-step plan with **Proceed / Revise / Cancel**
  and wait for sign-off before starting the work.

Both pause the turn until the user clicks. Enable them per agent via the `tools:`
list (see [Agent chat → Tools](agents.md#what-an-agent-can-do-tools)).

---

## Block Kit

Murtaugh renders Block Kit messages from JSON templates or builds them in code.
Templates are for **static** message shapes (sections, action rows, plan cards);
build blocks dynamically in code (or have the agent emit JSON) when the shape
depends on data.

Template paths are resolved relative to the config directory first, then fall
back to the embedded `assets/` defaults. The repo ships starter templates under
`assets/templates/` (e.g. the ping/pong cards and `unfurl/github-pr.json`).

---

## Workflow rules

Add rules to `workflow-rules.yaml` to react to Slack interactive events (button
clicks, form submissions). Rules match the raw interaction payload; the first
match wins, and its triggers run in order.

A trigger has **three mutually exclusive action types**:

- **`reply-to-slack`** — produce a Slack message JSON and post it back. The JSON
  comes from a `template`, the stdout of a `run` command, **or** a
  `delegate-to-agent` block (whose final output must be valid Slack message JSON).
- **`run`** — execute a command with the interaction payload on stdin.
- **`delegate-to-agent`** — start a real chat turn in the button's thread (the
  same pipeline an @mention drives: streaming, journaling, approval gate,
  per-thread session). `agent` is an optional override; omit it to use the
  channel's normal routing.

```yaml
# ~/.config/murtaugh/workflow-rules.yaml
workflow-rules:
  code-review-approval:
    request_event: interactive
    match:
      channel: { name: eng-reviews }
      actions:
        - block_id: github_pull_request
          action_id: approve_only
    trigger:
      - reply-to-slack:
          template: code-review/approved.json
      - run:
          cmd: /path/to/notify-script
          args: [--env, production]
      - delegate-to-agent:
          agent: default
          prompt: "Post a review summary for {{ .Payload.user.id }} in the thread."
```

`delegate-to-agent` prompts are rendered as Go templates against the interaction
payload, exposed under `.Payload` (e.g. `{{ .Payload.user.id }}`,
`{{ index .Payload.channel "name" }}`). Each delegated agent runs in an isolated
one-shot session — no shared chat memory.

> The **ping → pong** self-test is built into the binary, not a workflow rule —
> it posts the card and answers the click directly, so the round-trip can't be
> broken by a config edit. `workflow-rules.yaml` may be omitted entirely.

---

## Link unfurling

Add rules to `unfurl-rules.yaml` to replace shared URLs with rich Block Kit
previews. An `unfurl` action is exactly one of `template`, `run`, or
`delegate-to-agent`.

```yaml
# ~/.config/murtaugh/unfurl-rules.yaml
unfurl-rules:
  # Template-based: build the preview from the URL's parts alone.
  github-pr:
    match:
      domain: github.com
      url_pattern: '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/(?P<number>\d+)'
    unfurl:
      template: templates/unfurl/github-pr.json

  # Agent-based: the agent's final output must be a valid Slack attachment JSON.
  github-issues:
    match:
      domain: github.com
      url_pattern: '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/issues/(?P<number>\d+)'
    unfurl:
      delegate-to-agent:
        agent: default
        prompt: |
          Summarise GitHub issue {{ .URL }} and return solely a valid Slack
          attachment JSON object containing a one-paragraph summary.
```

Prompts and templates can reference `{{ .URL }}`, `{{ .Domain }}`, and named
captures via `{{ .Captures.<name> }}`. Unlike a workflow trigger, an unfurl
**always** renders JSON — non-JSON agent output is logged and the link is left
un-unfurled.

> **Slack requirements:** enable the `link_shared` bot event, the `links:read`
> and `chat:write` scopes, and register each `match.domain` in the Slack app's
> **App Unfurl Domains** list (max 5) — otherwise no `link_shared` event is
> delivered.
