# Configuring unfurl-rules

Rules live under `unfurl-rules:` in `slack.yaml`, keyed by name:

```yaml
unfurl-rules:
  github-pr:
    match:
      domain: github.com
      url_pattern: '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/(?P<number>\d+)'
    unfurl:
      template: templates/unfurl/github-pr.json
```

## `match` — which links a rule claims

| Field | Meaning |
|---|---|
| `domain` | Matches the link's domain **exactly or as a suffix** (so `github.com` also matches `gist.github.com`), case-insensitive. |
| `url_prefix` | The full URL must start with this exact string. |
| `url_pattern` | A Go regexp the full URL must match. **Named capture groups** (`(?P<name>…)`) are extracted into `Captures` for the action. |
| `channels` | Optional list of Slack channel IDs; the rule only applies in those channels. Omit to match every channel. |

**At least one** of `domain`, `url_prefix`, or `url_pattern` is required
(validation rejects a rule with none). You can combine them — all present
conditions must hold.

## Matching order

- Rules are evaluated in **sorted key order** (alphabetical by rule name); the
  **first** match wins. Name a specific rule to sort ahead of a catch-all.
- Per rule, the checks run: channel → domain → url_prefix → url_pattern. Any
  failing check moves on to the next rule.
- Domain comes from Slack's event (or is parsed from the URL).

## `unfurl` — the action

Exactly **one** of `template` or `unfurl.run` must be set (not both, not
neither). See `reference/actions.md` for each contract.

```yaml
    unfurl:
      run:
        cmd: /path/to/agent
        args: [unfurl-jira]
        timeout: 8s        # Go duration; default 30s
        workdir: /some/dir # optional
```

## Behavior & limits

- **Up to 10 unique links** per `link_shared` event are processed; duplicates
  within the event are de-duplicated.
- **Composition previews are skipped** — Slack sends a non-numeric (UUID)
  timestamp while a user is still typing; Murtaugh ignores those and only acts
  on posted messages.
- Each `run` command is bounded by its `timeout` (default **30s**); the whole
  unfurl handling for a message is bounded at 2 minutes.
- A per-link failure is logged and that link is skipped — it never blocks the
  other links in the same message.

## Validation rules

- `match` requires at least one of `domain` / `url_prefix` / `url_pattern`.
- `url_pattern`, if set, must compile as a Go regexp.
- each `channels` entry must be non-blank.
- `unfurl` requires exactly one of `template` or `run`; a `run` requires `cmd`.
