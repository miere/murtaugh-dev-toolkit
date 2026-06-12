# Skill: Murtaugh Link Unfurling

Murtaugh can replace a bare URL posted in Slack with a **rich Block Kit
preview**. When a message containing a matched link is posted, Slack sends a
`link_shared` event; Murtaugh matches it against an `unfurl-rules` entry and
renders a preview from a **template**, a **command** you run, or an **agent** you
delegate to. Use this whenever a task involves turning links (PRs, tickets,
dashboards) into inline previews.

> **Prerequisite — register the domain.** Slack only delivers `link_shared` for
> domains listed in your Slack app's **App Unfurl Domains** (max 5). If a
> domain isn't registered there, no event arrives and nothing you configure
> here will fire. This is enforced by Slack, not Murtaugh.

## The flow (mental model)

1. A user posts a message containing a URL on a registered domain.
2. Slack delivers a `link_shared` event to Murtaugh over Socket Mode.
3. Murtaugh evaluates each `unfurl-rules` entry in **sorted-key order** and takes
   the **first** rule that matches the URL. → `reference/configuring.md`
4. The rule's action renders a Block Kit attachment — a **`template`** (Go
   text/template), a **`run`** command (JSON in → attachment JSON out), or a
   **`delegate-to-agent`** (agent returns the attachment JSON). →
   `reference/actions.md`
5. Murtaugh posts all previews for the message in one `chat.unfurl` call.

## The three action modes (at a glance)

| Mode | Field | Use when |
|---|---|---|
| **template** | `template:` | the preview is static-ish — fill a JSON template with URL parts |
| **run** | `run:` | the preview needs a lookup (call an API, read a DB) before rendering |
| **delegate-to-agent** | `delegate-to-agent:` | producing the preview needs an agent's judgement or tool use (output must be attachment JSON) |

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Writing the `match` (domain / url_pattern / channels) and rule ordering | `reference/configuring.md` |
| Writing a `template`, a `run` command, or a `delegate-to-agent` (contracts) | `reference/actions.md` |
| Wanting a working rule + template | `examples/` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **Register the domain in the Slack app first** (App Unfurl Domains), or debug
  no further — the event never arrives.
- **Prefer `template`** for previews that are pure functions of the URL; reach
  for `run` only when you must look something up.
- **Fail silent.** A `run` command that prints nothing (or exits non-zero)
  suppresses that link's preview without error — that's the right way to skip.
- **No secrets in the URL or captures.** Whatever you extract is rendered into a
  message visible to the channel.
- For the Block Kit itself, see the **`murtaugh-slack`** skill (`reference/blocks.md`).
