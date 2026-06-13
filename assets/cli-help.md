# murtaugh — command-line reference

Murtaugh ships as a single binary with three frontends over one shared tool
registry:

- **CLI** — direct invocation: `murtaugh <command> [flags...]`.
- **MCP** — JSON-RPC stdio server (`murtaugh mcp`) exposing every tool below to
  AI clients. The MCP tool name is the dotted registry name (e.g. `jobs.run`,
  `slack.send-msg`); the CLI spells the same tool with a space (`jobs run`,
  `slack send-msg`).
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

- **Global flag `--config PATH`** — path to `slack.yaml`. Default
  `~/.config/murtaugh/slack.yaml`. Accepts `--config PATH` or `--config=PATH`.
  Its sibling files `agents.yaml` and `jobs.yaml` are resolved from the same
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
token from `oauth.bot_token` in `slack.yaml`.

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
files (`slack.yaml`, `agents.yaml`, `jobs.yaml`); there are no tool flags. Stop
it with SIGINT/SIGTERM. Normally run under launchd (see `setup launchd`).

```
murtaugh slack gateway
murtaugh --config /etc/murtaugh/slack.yaml slack gateway
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

Seed the Murtaugh config directory with embedded defaults (`slack.yaml`,
`agents.yaml`, `jobs.yaml`, Block Kit templates, bundled skills). Takes no
flags. Runs on every Murtaugh start, not just the first.

- **Config files and templates** (`slack.yaml`, `agents.yaml`, `jobs.yaml`,
  `templates/`) are created once and then **preserved** — your tokens and edits
  are never overwritten.
- **Bundled skills** (`.agents/skills/`) are **refreshed** to the version shipped
  with the running binary on every run, so the workspace tracks upgrades. A
  skill directory you add yourself is left untouched; an edit to a skill
  Murtaugh ships is overwritten — add a new skill instead of editing a shipped
  one.

The report lists which files were created, updated (refreshed), and preserved.

```
murtaugh setup bootstrap
```

## murtaugh setup slack

Write `slack.yaml` (OAuth tokens, admin user, and the `/murtaugh` slash
command). An existing file is backed up before being replaced.

| Flag              | Required | Type   | Notes                                                       |
|-------------------|----------|--------|-------------------------------------------------------------|
| `--app-token`     | yes      | string | Slack app-level token; must start with `xapp-`.             |
| `--bot-token`     | yes      | string | Slack bot OAuth token; must start with `xoxb-`.             |
| `--admin-user`    | yes      | string | Admin handle (`@name`) or user ID (`U…`).                   |
| `--default-agent` | no       | string | `agents.yaml` key to wire into `chat.default_agent`.        |

```
murtaugh setup slack --app-token xapp-… --bot-token xoxb-… --admin-user @miere
```

## murtaugh setup agents

Write `agents.yaml` with the ACP tuning block and an optional named agent. ACP
is enabled only when `--command` is supplied; with no command the file is
written with `acp.enabled: false`.

| Flag           | Required | Type            | Notes                                                         |
|----------------|----------|-----------------|---------------------------------------------------------------|
| `--agent-name` | no       | string          | Key the agent is registered under. Defaults to `default`.     |
| `--command`    | no       | string          | Absolute path to the ACP-speaking binary. Blank disables ACP. |
| `--args`       | no       | string (repeat) | Arguments passed to the agent command. Requires `--command`.  |

- Supplying `--args` without `--command` is an error.

```
murtaugh setup agents --agent-name claude --command /usr/local/bin/claude-acp --args --acp
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

```
murtaugh setup mcp-register --client opencode --binary-path /usr/local/bin/murtaugh
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
