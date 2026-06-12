# Unfurl actions: template, run, or delegate-to-agent

All three actions produce the same thing — a **Slack Block Kit attachment** as
JSON, which Murtaugh posts via `chat.unfurl`. They differ only in how that JSON
is produced. Exactly one of `template`, `run`, or `delegate-to-agent` is set
per rule.

## Shared data

Both actions receive the same fields about the shared link:

| Field | Meaning |
|---|---|
| `URL` | the full shared link |
| `Domain` | the link's domain |
| `Channel` | Slack channel ID where it was posted |
| `User` | Slack user ID who posted it |
| `MessageTS` | the message timestamp |
| `ThreadTS` | thread timestamp (empty if not in a thread) |
| `TeamID` | Slack workspace/team ID |
| `Captures` | map of **named** regex captures from `match.url_pattern` (empty if none) |

## `template` — Go text/template → JSON

The template path resolves against the **workspace** first
(`<workspace>/templates/...`), then falls back to the embedded defaults at the
same path. Variables are the fields above, dot-prefixed; captures are
`.Captures.<name>`:

```json
{
  "blocks": [
    {
      "type": "section",
      "text": {
        "type": "mrkdwn",
        "text": "*<{{ .URL }}|Pull Request #{{ .Captures.number }}>*\n`{{ .Captures.owner }}/{{ .Captures.repo }}`"
      }
    }
  ]
}
```

- Rendering uses `missingkey=error`: referencing a capture that didn't match is
  an error (the preview is skipped), so guard optional captures in your regex.
- The rendered output must be valid JSON that decodes to a Slack attachment.

## `run` — command: JSON in → attachment JSON out

Murtaugh runs `cmd` (with `args`, in `workdir`, bounded by `timeout`) and:

**Writes to stdin** — the shared-data object as JSON:

```json
{
  "URL":       "https://example.com/browse/PROJ-42",
  "Domain":    "example.com",
  "Channel":   "C0ENG1",
  "User":      "U0ABCDEF",
  "MessageTS": "1700000000.000100",
  "ThreadTS":  "",
  "TeamID":    "T0ABCDEF",
  "Captures":  { "key": "PROJ-42" }
}
```

**Reads from stdout** — one valid Block Kit attachment JSON object:

```json
{
  "blocks": [
    { "type": "section",
      "text": { "type": "mrkdwn", "text": "*PROJ-42* — Fix the thing" } }
  ]
}
```

- **Print nothing, or exit non-zero → the link is silently skipped** (no
  preview, no error to the channel). This is the intended way to say "no
  preview for this one".
- stdout is trimmed and must be valid JSON decoding to a `slack.Attachment`;
  malformed output is treated as a skip-with-log.

## `delegate-to-agent` — agent renders the attachment JSON

Hands the preview to an agent (keyed in `agents.yaml`) running in an isolated
one-shot session. The prompt is rendered with the same shared-data fields as a
template (`{{ .URL }}`, `{{ .Domain }}`, `{{ .Captures.<name> }}`, …), using
`missingkey=error`.

```yaml
    unfurl:
      delegate-to-agent:
        agent: default
        prompt: |
          Summarise GitHub issue {{ .URL }} and return me solely a valid Slack
          attachment JSON object containing the summary in one paragraph.
```

- The agent's **final output must be a single valid JSON** Slack attachment.
- **Non-JSON output → the link is skipped** and a warning (with the raw output)
  is logged. Same skip-with-log behaviour as `run`.
- The agent may use its own tools/MCP to gather context (call an API, read a
  repo) before producing the JSON — richer than a `run` command, but slower and
  non-deterministic.

Use `run` for previews that need a fixed lookup (call the Jira/GitHub API, read
a DB); use `template` when the URL itself carries everything you need; use
`delegate-to-agent` when producing the preview needs an agent's judgement or
tool use.
