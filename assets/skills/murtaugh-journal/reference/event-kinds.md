# Journal event kinds

Every event has an envelope — `time`, `stream`, `kind`, `level`
(debug/info/warn/error), `corr_id`, correlation `keys`
(team/channel/thread/user/session/job/rule), a one-line `summary`, and a
`kind`-specific `payload`. Filter on the envelope; read the `payload` for detail.

## `gateway` stream

Recorded while handling a Slack interaction. All events from one interaction
share a `corr_id` (minted at ingress as `gw_…`).

| kind | level | meaning / payload |
|------|-------|-------------------|
| `slash.command` | info | A slash command was accepted. payload: `command`, `text`. |
| `interactive.received` | info | A block-action / view-submission arrived. payload: `interaction_type`, `callback_id`. |
| `workflow.matched` | info | A workflow rule matched the interaction. keys: `rule_id`. payload: `interaction_type`. |
| `workflow.no_match` | debug | No rule matched. payload: `interaction_type`, `callback_id`, `action_ids`. The usual cause of "nothing happened". |
| `workflow.trigger` | info / warn / error | One trigger ran. payload: `trigger` (`reply-to-slack`\|`run`\|`delegate-to-agent`). **error** carries `error`; **warn** (`json_valid:false`) means a delegate-to-agent reply produced non-JSON and was skipped. |
| `unfurl.no_match` | debug | A shared link matched no unfurl rule. payload: `url`, `domain`. |
| `unfurl.render` | info / error | Built (or failed to build) a link preview. keys: `rule_id`. payload: `url`, `error` on failure. |
| `unfurl.post` | info / error | The `chat.unfurl` call. payload: `count`, `error` on failure. |

### Reading a failed interaction

A typical failing approve-button story, pulled with `--corr-id`:

1. `interactive.received` — the click arrived.
2. `workflow.matched` (`rule_id: code-review-approval`) — a rule claimed it.
3. `workflow.trigger` **error** — payload `error` says why (e.g. a template
   referenced a field the payload didn't have, since templates use
   `missingkey=error`).

If you see `interactive.received` but **no** `workflow.matched`, look for
`workflow.no_match` — the rule's `match` didn't fit the payload (wrong
`action_id`, `block_id`, or channel).

## `job` stream

| kind | level | meaning / payload |
|------|-------|-------------------|
| `job.run` | info / error | A `jobs.run` invocation. keys: `job_name`. payload: `command` or `agent`, `duration_ms`, and `exit_code` (command jobs). A non-zero exit is recorded at **error** level; a process that failed to start carries `error`. |

## `acp_session` stream

Reserved for persistent chat session logs. No event kinds are emitted yet.
