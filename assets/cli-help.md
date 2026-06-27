# murtaugh — command-line reference

Murtaugh ships as a single binary with three frontends over one shared tool
registry:

- **CLI** — direct invocation: `murtaugh <command> [flags...]`.
- **MCP** — JSON-RPC stdio server (`murtaugh mcp`) exposing every tool below to
  AI clients. The MCP tool name is the registry name with every dot replaced by
  an underscore (e.g. `jobs_run`, `slack_send-msg`) — some providers (e.g.
  Gemini) reject a `.` in a function name, so it is normalised at the MCP
  boundary. The dotted form (`jobs.run`) remains the registry key, and the CLI
  spells the same tool with a space (`jobs run`, `slack send-msg`).
- **Slack gateway** — the long-running Socket Mode daemon
  (`murtaugh slack gateway`).

```
Usage: murtaugh [--config PATH] <command> [flags...]
```

Run `murtaugh help` for this full document, or `murtaugh help <command>`
(e.g. `murtaugh help slack send-msg`) for a single command. `murtaugh <command>
--help` works too.

# Global conventions

These rules apply to **every** CLI command. Read them once; the per-command
sections below assume them.

- **Global flag `--config PATH`** — path to `gateway.yaml`. Default
  `~/.config/murtaugh/gateway.yaml`. Accepts `--config PATH` or `--config=PATH`.
  Its sibling files `agents.yaml`, `jobs.yaml`, `journal.yaml`,
  `workflow-rules.yaml`, and `unfurl-rules.yaml` are resolved from the same
  directory.
- **Flags take values; there are no positional arguments.** Every flag is
  `--flag value`. There is no `--flag=value` form for tool flags (only the
  global `--config` accepts `=`).
- **Booleans require an explicit value.** Write `--load true`, `--force false`.
  A bare `--load` is rejected with `flag --load requires a value`. (Over MCP,
  pass a real JSON boolean instead.)
- **Flags are kebab-case.** A schema field `attachment_type` is the flag
  `--attachment-type`; `binary_path` → `--binary-path`; `app_token` →
  `--app-token`. The MCP argument names are the snake_case originals.
- **Repeatable (array) flags** accumulate when repeated:
  `--args one --args two` yields `["one", "two"]`.
- **Output goes to stdout; diagnostics/warnings go to stderr.** CLI results are
  rendered as human-readable lines; the MCP frontend returns the same data as
  JSON.
- **Unknown flags are errors.** An unrecognised `--flag` fails fast rather than
  being ignored.

## Slack target & identifier resolution

Several Slack tools accept channels, users, threads, and timestamps. The
accepted forms are consistent:

- **`--to` (send-msg only)** accepts `#channel-name`, `@user-handle`, or a raw
  `C…`/`G…`/`D…` ID. `@user` opens (or reuses) a DM. A bare `U…` user ID is
  **rejected** — use `@handle` or open the DM yourself. Anything not starting
  with `#`, `@`, `C`, `G`, or `D` is an error.
- **`--channel` (fetch-msgs, fetch-reactions)** accepts a channel name with or
  without a leading `#`, or a channel ID. Names are matched against
  `conversations.list`.
- **`--channel` (update-msg)** is resolved by name **only** when it starts with
  `#`; any other value is passed through unchanged as a channel ID.
- **`--from` / user handles** accept the handle with or without a leading `@`.
  Matching is case-insensitive and tries legacy username, then display name,
  then real name.
- **`@mentions` inside `--body`** are auto-expanded to `<@USERID>` Slack mention
  syntax. An unresolvable `@handle` is left as plain text and a warning is
  printed to stderr.
- **Thread / message timestamps (`--thread`, `--ts`)** are Slack `ts` strings
  like `1716950455.123456`.
- **`--since`** is a wall-clock datetime `YYYY-MM-DD HH:mm:ss` interpreted in
  **Sydney (Australia/Sydney) time**. Default when omitted: 24 hours ago.

## Block Kit blocks (`--blocks`)

`--blocks` accepts **either** an inline JSON string (the value's first
non-whitespace character is `[` or `{`) **or** a filesystem path to a `.json`
file. Either way the content must be valid JSON or the command fails with
`Error parsing blocks JSON: …`. `--blocks` is mutually exclusive with
`--attachment` on `send-msg`.

# Commands

## murtaugh ping

Health check. Returns `pong`. Takes no flags.

```
murtaugh ping
```

## murtaugh jobs run

Run a job previously defined in `jobs.yaml`, by name. The job's command runs
with its configured args/workdir/timeout; child stdout/stderr stream to your
terminal (and are captured into the JSON result over MCP).

| Flag     | Required | Type   | Notes                                   |
|----------|----------|--------|-----------------------------------------|
| `--name` | yes      | string | Job key as it appears in `jobs.yaml`.   |

- Default timeout is **10 minutes** when the job has no `timeout` set.
- Exit code is reported in the result; a non-zero exit is **not** a CLI error
  (the job ran), but the scheduler treats it as a failed run.
- Fails if the job name is not found or the job has no `command`.

```
murtaugh jobs run --name nightly-backup
```

## murtaugh jobs define

Register a new job, or update an existing one, in `jobs.yaml`. Does **not** run
the job — use `jobs run` for that. Unrelated jobs in the file are preserved.

| Flag         | Required | Type            | Notes                                                                 |
|--------------|----------|-----------------|-----------------------------------------------------------------------|
| `--name`     | yes      | string          | Job key. Must be non-empty.                                           |
| `--command`  | yes      | string          | Absolute path or PATH-resolved binary to execute.                     |
| `--args`     | no       | string (repeat) | Positional arguments for the command. Repeat the flag per arg.        |
| `--workdir`  | no       | string          | Working directory for the command.                                    |
| `--timeout`  | no       | duration        | Go duration (e.g. `30s`, `5m`). Defaults to `10m` at run time.        |
| `--schedule` | no       | cron            | 5-field cron (e.g. `0 2 * * *`) for automatic runs by the gateway.    |
| `--every`    | no       | duration        | Go duration (e.g. `1h`) for fixed-interval runs by the gateway.       |

- `--schedule` and `--every` are **mutually exclusive**; set at most one.
- A scheduled job only fires while `murtaugh slack gateway` is running.
- `--timeout` and `--every` must be valid Go durations; `--every` must be > 0.

```
murtaugh jobs define --name nightly-backup \
  --command /usr/local/bin/backup --args --full --args /data \
  --workdir /srv --timeout 30m --schedule "0 2 * * *"
```

## murtaugh journal query

Read structured events back out of the **event journal** — the queryable record
of what Murtaugh did (gateway interactions, workflow rules, link unfurls, job
runs). This is how Gateway Debug Mode answers "why did this interaction
misbehave?". Distinct from the daemon's stderr logs: the journal is for filtered,
correlated inspection. Configure it in `journal.yaml`.

All filters are optional and ANDed; results are most-recent-first.

| Flag        | Required | Type     | Notes                                                                 |
|-------------|----------|----------|-----------------------------------------------------------------------|
| `--stream`  | no       | string   | `gateway`, `job`, or `acp_session`.                                   |
| `--kind`    | no       | string   | Exact event kind, e.g. `workflow.trigger`, `unfurl.render`, `job.run`.|
| `--level`   | no       | string   | Minimum severity (at least): `debug`, `info`, `warn`, `error`.        |
| `--channel` | no       | string   | Slack channel ID.                                                     |
| `--user`    | no       | string   | Slack user ID.                                                        |
| `--session` | no       | string   | ACP session ID.                                                       |
| `--corr-id` | no       | string   | Correlation id — every event from one interaction shares it.          |
| `--rule`    | no       | string   | Workflow or unfurl rule name.                                         |
| `--since`   | no       | string   | Lower time bound: a Go duration ago (`2h`) or an RFC3339 timestamp.   |
| `--until`   | no       | string   | Upper time bound: a Go duration ago (`5m`) or an RFC3339 timestamp.   |
| `--limit`   | no       | integer  | Max events (default `50`, capped at `500`).                           |

- The typical flow: filter by `--channel`/`--since`/`--level error` to find a
  failure, then re-query with `--corr-id` to see that whole interaction.
- Each event's `payload` carries the detail (template path, render error,
  non-JSON agent output, command error, …).

```
murtaugh journal query --stream gateway --channel C0REVIEWS --since 1h --level warn
murtaugh journal query --corr-id gw_3f9c2b1a
```

## murtaugh journal stats

Summarise the journal: row count and oldest/newest timestamp per stream. Every
known stream is listed, including empty ones — a `0` count on `gateway` means
Gateway Debug Mode is not recording (check `journal.yaml` and restart the
daemon). Takes no flags.

```
murtaugh journal stats
```

## murtaugh journal prune

Delete events older than each stream's configured retention (from `journal.yaml`)
— a manual run of the sweep the gateway daemon performs automatically (on startup
and every `sweep.every`). Takes no flags; uses the configured retention.

```
murtaugh journal prune
```

## murtaugh slack send-msg

Post a message (or upload a file) to a Slack channel or user. Uses the bot
token from `oauth.bot_token` in `gateway.yaml`.

| Flag                | Required | Type   | Notes                                                                       |
|---------------------|----------|--------|-----------------------------------------------------------------------------|
| `--body`            | yes      | string | Message text. Also the notification fallback when `--blocks` is set. `@mentions` are expanded. |
| `--to`              | yes      | string | Destination: `#channel`, `@user`, or `C…/G…/D…` ID. See target resolution.  |
| `--thread`          | no       | string | Parent message `ts` to reply in-thread.                                     |
| `--attachment`      | no       | string | Path to a file to upload. Mutually exclusive with `--blocks`.               |
| `--attachment-type` | no       | enum   | Snippet type for the attachment. Only value: `markdown`.                    |
| `--blocks`          | no       | string | Block Kit JSON (inline string or file path). Mutually exclusive with `--attachment`. |

```
murtaugh slack send-msg --to "#deploys" --body "Build green :white_check_mark:"
murtaugh slack send-msg --to "@miere" --body "ping" --thread 1716950455.123456
murtaugh slack send-msg --to "#status" --body "Status" --blocks ./status-blocks.json
```

## murtaugh slack create-channel

Create a public or private Slack channel, optionally inviting users and setting
a topic/purpose. Uses the bot token from `oauth.bot_token` in `gateway.yaml`. The
bot needs the `channels:manage` scope for public channels and `groups:write`
for private ones (those scopes also cover the invites).

| Flag        | Required | Type    | Notes                                                              |
|-------------|----------|---------|--------------------------------------------------------------------|
| `--name`    | yes      | string  | Channel name (a leading `#` is stripped). Slack lowercases it and replaces spaces with hyphens. |
| `--private` | no       | boolean | Create a private channel instead of a public one.                  |
| `--invite`  | no       | array   | Users to invite: `@handle` mentions or raw `U…/W…` user IDs. Unresolvable handles are skipped with a warning; per-user failures don't abort. |
| `--topic`   | no       | string  | Channel topic to set after creation.                               |
| `--purpose` | no       | string  | Channel purpose/description to set after creation.                 |

```
murtaugh slack create-channel --name launch-2026 --topic "Launch coordination"
murtaugh slack create-channel --name incident-42 --private true --invite @miere --invite U07ABCDE
```

## murtaugh slack fetch-msgs

Fetch messages from a channel or thread, oldest-first. Capped at 100 messages.

| Flag        | Required | Type   | Notes                                                              |
|-------------|----------|--------|--------------------------------------------------------------------|
| `--channel` | yes      | string | Channel name (with or without `#`) or channel ID.                  |
| `--thread`  | no       | string | A thread `ts`; fetches that thread's replies instead of history.   |
| `--since`   | no       | string | `YYYY-MM-DD HH:mm:ss` Sydney time. Excludes older messages. Default 24h ago. |

```
murtaugh slack fetch-msgs --channel deploys
murtaugh slack fetch-msgs --channel deploys --since "2026-06-12 09:00:00"
murtaugh slack fetch-msgs --channel C0123456789 --thread 1716950455.123456
```

## murtaugh slack fetch-reactions

Fetch the messages in a channel that a specific user reacted to with a specific
emoji. Scans up to 100 recent messages and filters them. Output is oldest-first.

| Flag        | Required | Type   | Notes                                                              |
|-------------|----------|--------|--------------------------------------------------------------------|
| `--from`    | yes      | string | User handle, with or without `@`.                                  |
| `--emoji`   | yes      | string | Emoji name, with or without colons (`thumbsup` or `:thumbsup:`).   |
| `--channel` | yes      | string | Channel name (with or without `#`) or channel ID.                  |
| `--since`   | no       | string | `YYYY-MM-DD HH:mm:ss` Sydney time. Default 24h ago.                |

```
murtaugh slack fetch-reactions --from @miere --emoji eyes --channel triage
```

## murtaugh slack update-msg

Update an existing message in a channel, optionally rewriting its Block Kit
blocks.

| Flag        | Required | Type   | Notes                                                                          |
|-------------|----------|--------|--------------------------------------------------------------------------------|
| `--channel` | yes      | string | Channel ID, **or** a channel name with a leading `#`. A name without `#` is treated as a raw ID. |
| `--ts`      | yes      | string | Timestamp of the message to update.                                            |
| `--body`    | no       | string | Fallback text for the update. Defaults to `Message updated`.                   |
| `--blocks`  | no       | string | Block Kit JSON (inline string or file path).                                   |

```
murtaugh slack update-msg --channel "#deploys" --ts 1716950455.123456 \
  --body "Build finished" --blocks ./done-blocks.json
```

## murtaugh slack gateway

Start the Slack gateway: the long-running Socket Mode daemon. It responds to
slash commands, runs YAML workflow rules against interactive payloads, bridges
Slack conversations to an ACP agent with live streaming, renders custom link
unfurls, and fires scheduled jobs. Configuration comes entirely from the config
files (`gateway.yaml`, `agents.yaml`, `jobs.yaml`, `workflow-rules.yaml`,
`unfurl-rules.yaml`); there are no tool flags. Stop it with SIGINT/SIGTERM.
Normally run under launchd (see `setup launchd`).

```
murtaugh slack gateway
murtaugh --config /etc/murtaugh/gateway.yaml slack gateway
```

## murtaugh mcp

Start the MCP stdio server. Serves every registered tool to an MCP client over
JSON-RPC on stdin/stdout. stdout is reserved for protocol traffic — do not run
this interactively expecting human output. Register it with a client via
`setup mcp-register`.

```
murtaugh mcp
```

## murtaugh setup bootstrap

Seed the Murtaugh config directory with embedded defaults (`gateway.yaml`,
`agents.yaml`, `jobs.yaml`, `workflow-rules.yaml`, `unfurl-rules.yaml`,
`system-prompt.md`, Block Kit templates, bundled skills). Runs on every Murtaugh
start, not just the first.

| Flag      | Required | Type    | Notes                                                              |
|-----------|----------|---------|-------------------------------------------------------------------|
| `--force` | no       | boolean | Refresh the bundled default `system-prompt.md` to the shipped version. |

- **Config files and templates** (`gateway.yaml`, `agents.yaml`, `jobs.yaml`,
  `workflow-rules.yaml`, `unfurl-rules.yaml`, `templates/`) and **`AGENTS.md`**
  (the agent's identity) are created once and
  then **preserved** — your tokens, edits, and chosen persona are never
  overwritten, even with `--force`.
- **`system-prompt.md`** (the default base prompt) is created once and preserved,
  but `--force` refreshes it to the version shipped with the binary.
- **Bundled skills** (`.agents/skills/`) are **refreshed** to the shipped version
  on every run, so the workspace tracks upgrades. A skill directory you add
  yourself is left untouched; an edit to a skill Murtaugh ships is overwritten —
  add a new skill instead of editing a shipped one.

The report lists which files were created, updated (refreshed), and preserved.

```
murtaugh setup bootstrap
murtaugh setup bootstrap --force true   # refresh the default system prompt
```

## murtaugh setup slack

Write `gateway.yaml` (admin user and the `/murtaugh` slash command) and store the
Slack tokens in `~/.config/murtaugh/.env`. The YAML references them as
`${SLACK_APP_TOKEN}` / `${SLACK_BOT_TOKEN}`, so the tokens never live in a file
the troubleshoot bundler collects. Both `gateway.yaml` and `.env` are backed up
before being replaced/merged.

| Flag              | Required | Type   | Notes                                                       |
|-------------------|----------|--------|-------------------------------------------------------------|
| `--app-token`     | yes      | string | Slack app-level token; must start with `xapp-`. Stored in `.env`. |
| `--bot-token`     | yes      | string | Slack bot OAuth token; must start with `xoxb-`. Stored in `.env`. |
| `--admin-user`    | yes      | string | Admin handle (`@name`) or user ID (`U…`).                   |
| `--default-agent` | no       | string | `agents.yaml` key to wire into `chat.default_agent`.        |

```
murtaugh setup slack --app-token xapp-… --bot-token xoxb-… --admin-user @miere
```

## murtaugh setup env

Upsert `KEY=VALUE` secrets into `~/.config/murtaugh/.env`, preserving existing
entries and comments. This is where LLM provider API keys live; `agents.yaml`
references them by name via `api_key_env`. The file is backed up before being
merged. Output reports key **names** only — never the secret values.

| Flag    | Required | Type            | Notes                                   |
|---------|----------|-----------------|-----------------------------------------|
| `--set` | yes      | string (repeat) | A `KEY=VALUE` pair. Repeat for several. |

```
murtaugh setup env --set GEMINI_API_KEY=AIza… --set VAULTRE_TOKEN=…
```

## murtaugh setup agents

Write `agents.yaml` with the runtime tuning block and a single named agent.
Supports both backends: a **native** LLM agent (`kind: native`, the default —
Murtaugh talks to the model directly) and an external **ACP** agent
(`kind: acp`). The kind is inferred from the flags when `--kind` is omitted:
`--provider` ⇒ native, `--command` ⇒ acp. With no agent flags the file is
written with chat disabled. Secrets are never written here — a native profile
records `--api-key-env` (the `.env` variable name); set the value with
`setup env`.

| Flag                   | Required | Type            | Notes                                                             |
|------------------------|----------|-----------------|------------------------------------------------------------------|
| `--agent-name`         | no       | string          | Key the agent is registered under. Defaults to `default`.         |
| `--kind`               | no       | enum            | `native` or `acp`. Inferred from the other flags when omitted.    |
| `--command`            | acp      | string          | ACP: absolute path to the ACP-speaking binary.                    |
| `--args`               | no       | string (repeat) | ACP: arguments passed to the command.                             |
| `--provider`           | native   | enum            | `gemini`, `anthropic`, or `openai` (compat via `--base-url`).     |
| `--model`              | native   | string          | Provider model id (e.g. `gemini-2.5-pro`).                        |
| `--api-key-env`        | native   | string          | Name of the `.env` variable holding the API key.                  |
| `--base-url`           | no       | string          | Native: endpoint override for compat providers.                   |
| `--tools`              | no       | string (repeat) | Native: tool allowlist (`files`, `terminal`, `skills`, namespaces).|
| `--mcp-servers`        | no       | string (repeat) | Native: `mcp_servers` entries to attach.                          |
| `--system-prompt-file` | no       | string          | Native: path (relative to config dir) to the system prompt.       |
| `--context-limit`      | no       | integer         | Native: token budget for compaction. 0 = per-family default.      |
| `--compaction`         | no       | enum            | Native: `truncate` (default) or `summarize`.                      |
| `--cache-retention`    | no       | enum            | Native: prompt-cache TTL — `5m` (default), `1h`, or `off`.        |

- For ACP, supplying `--args` without `--command` is an error.

```
murtaugh setup agents --provider gemini --model gemini-2.5-pro \
  --api-key-env GEMINI_API_KEY --tools files --tools terminal --tools skills
murtaugh setup agents --kind acp --agent-name goose --command /usr/local/bin/goose --args acp
```

## murtaugh setup mcp-register

Register Murtaugh as an MCP server in a downstream AI client's config, merging
into the existing file (other keys preserved) and backing it up first.

| Flag            | Required | Type   | Notes                                                          |
|-----------------|----------|--------|----------------------------------------------------------------|
| `--client`      | yes      | enum   | One of `opencode`, `auggie`, `goose`.                          |
| `--binary-path` | yes      | string | Absolute path to the `murtaugh` binary used as the MCP command.|

Target files: `opencode` → `~/.config/opencode/opencode.json`; `auggie` →
`~/.augment/settings.json`; `goose` → `~/.config/goose/config.yaml`.

When the client is also a provider Murtaugh can collect diagnostics for (today
`goose`), it is recorded in `troubleshoot.yaml` so `troubleshoot bundle` and
`/murtaugh troubleshoot` include that provider's sessions/logs **by default**
(no `--include` needed). Recording is best-effort — a failure there only adds a
warning, it does not fail the client registration.

```
murtaugh setup mcp-register --client opencode --binary-path /usr/local/bin/murtaugh
murtaugh setup mcp-register --client goose --binary-path /usr/local/bin/murtaugh
```

## murtaugh setup launchd

Write the `dev.murtaugh` LaunchAgent plist (macOS only) and optionally load it
via launchctl. On non-macOS hosts it returns a clean "unsupported on <os>"
error. An existing plist is backed up first.

| Flag            | Required | Type    | Notes                                                              |
|-----------------|----------|---------|--------------------------------------------------------------------|
| `--binary-path` | yes      | string  | Absolute path to the `murtaugh` binary.                            |
| `--load`        | no       | boolean | `true` runs `launchctl bootout`+`bootstrap`+`kickstart` after writing. Remember booleans need a value: `--load true`. |

```
murtaugh setup launchd --binary-path /usr/local/bin/murtaugh --load true
```

## murtaugh setup update

Replace the running Murtaugh binary with the matching asset from a GitHub
release. The fetched asset is verified before the swap; the previous binary is
backed up.

| Flag                 | Required | Type    | Notes                                                            |
|----------------------|----------|---------|------------------------------------------------------------------|
| `--version`          | no       | string  | Release tag to install. Default: latest release.                 |
| `--force`            | no       | boolean | Update even when the current build is `dev` or already current. `--force true`. |
| `--release-json-url` | no       | string  | Override the release-metadata URL. Mainly for tests/fixtures.    |

- A `dev` build is refused unless `--force true` (it is likely a local checkout).
- An already-current install short-circuits with a "nothing to do" result.

```
murtaugh setup update
murtaugh setup update --version v0.5.0 --force true
```

## murtaugh troubleshoot bundle

Assemble a self-contained **diagnostics bundle** (a zip) for investigating a
problem: a consistent snapshot of the event journal, the ACP transcripts, the
daemon logs (tail-truncated), the **redacted** config files, optional
downstream-provider artifacts (e.g. Goose sessions + logs), a `manifest.json`,
and an `INSTRUCTIONS.md` telling an AI agent how to read it. Deterministic — it
never asks an agent to gather the files.

| Flag             | Required | Type            | Notes                                                                          |
|------------------|----------|-----------------|--------------------------------------------------------------------------------|
| `--note`         | no       | string          | Symptom description; recorded in the manifest.                                 |
| `--include`      | no       | string (repeat) | Provider whose on-disk diagnostics to add (known: `goose`). Repeat per provider. Defaults to the providers in `troubleshoot.yaml` (written by `setup mcp-register`), else all known providers.|
| `--out`          | no       | string          | Output path for the zip. Defaults to a timestamped file in the temp dir.       |
| `--max-log-bytes`| no       | integer         | Tail cap per log file in bytes. Defaults to 5 MiB.                             |
| `--redact`       | no       | boolean         | Redact known secrets. Defaults to `true`; only set `false` for local-only use. |

- Redaction removes Slack tokens (`xoxb-`/`xapp-`/`xoxp-`) and the values of
  obviously-secret config keys. It **cannot** scrub secrets inside conversation
  transcripts or binary `*.db` files — treat the bundle as sensitive.
- The same capability is exposed over MCP as `troubleshoot_bundle`, and in Slack
  as `/murtaugh troubleshoot <symptoms>` (which DMs the bundle to the admin).
- Missing sources (e.g. logs on a non-macOS host, or a provider that isn't
  installed) are skipped and noted in `manifest.json`, never fatal.

```
murtaugh troubleshoot bundle --note "bot goes silent on action requests" --include goose
murtaugh troubleshoot bundle --out /tmp/murtaugh-diag.zip --max-log-bytes 1048576
```

## murtaugh version

Print the binary's version string (e.g. `v0.4.1` or `dev`). Takes no flags.

```
murtaugh version
```

## murtaugh help

Print this reference. With no argument, prints the full document; with a command
argument, prints just that command's section.

```
murtaugh help
murtaugh help slack send-msg
murtaugh help jobs define
```
