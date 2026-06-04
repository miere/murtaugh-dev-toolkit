# Murtaugh Dev Toolkit

Go service for connecting Murtaugh to Slack via Socket Mode.

## Goals

- Make it easy to build refined Slack experiences with BlockKit.
- Handle custom Slack slash commands.
- Provide integration points for ACP agents, locally or remotely.

## Configuration

Create `~/.config/murtaugh/slack.yaml`:

~~~yaml
slack:
  app_token: xapp-your-socket-mode-app-token
  bot_token: xoxb-your-bot-token
  admin_user: your-slack-handle
  debug: false

acp:
  enabled: true
  command: /path/to/acp-agent
  args: [--stdio]
  workdir: /path/to/workspace
  request_timeout: 10m
  session_idle_timeout: 30m
  max_sessions: 100
  stream_append_interval: 750ms
  stream_min_chunk_chars: 96

commands:
  - name: /murtaugh
    description: Entrypoint for Murtaugh commands

workflow-rules:
  code-review-approval:
    request_event: interactive
    match:
      channel: { name: nc-code-reviews }
      actions:
        - block_id: github_pull_request
          action_id: approve_only
    trigger:
      - reply-to-slack:
          template: code-review/approved.json
      - run:
          cmd: /path/to/background-command
          args: [param1, param2]
~~~

The Slack app must have Socket Mode enabled and must subscribe to slash command
payloads for every command listed in the configuration. For ACP chat, subscribe
to the Events API event types `app_mention` and `message.im`, and grant scopes
for slash commands, app mentions, IM history, and chat writes.

`slack.admin_user` may be a Slack handle with or without `@` or a Slack user ID.
When Socket Mode reports that it is connected, Murtaugh opens a DM with that user
and sends the startup ping message from `assets/ping/01-ping.json`.

## ACP chat

When `acp.enabled` is true, Murtaugh can chat through a local ACP-compatible
agent process. It supports three entrypoints:

- DM the bot.
- Mention the bot in a channel.
- Run `/murtaugh chat <prompt>`.

Murtaugh keeps one ACP session per Slack conversation:

- DMs use one session per DM channel.
- Channel mentions use one session per Slack thread. If the mention is not in a
  thread, the mention message timestamp becomes the root thread key.

Responses use Slack's native streaming-message APIs, not simulated `chat.update`
loops:

- `chat.startStream` starts the streamed response.
- `chat.appendStream` appends ACP text deltas.
- `chat.stopStream` finalizes the response.

Murtaugh flushes the first ACP text chunk immediately, then uses
`stream_append_interval` and `stream_min_chunk_chars` to coalesce later small
chunks so it does not call `chat.appendStream` for every tiny token.

## Workflow rules

Workflow rules let Murtaugh respond to Slack interactive form/button submissions.
Interactive events are acknowledged immediately through Socket Mode, then the
matching workflow runs asynchronously. `reply-to-slack` triggers render JSON and
POST it to the Slack `response_url` from the interaction payload.

- `match` is a partial match against Slack's interaction JSON payload.
- Array match entries match when any payload array item contains the configured
  partial object.
- Triggers run in the order they are configured.
- Template paths are resolved relative to the configuration file directory.
- Commands are executed directly with explicit args, not through a shell. The
  Slack interaction JSON is passed to commands on stdin.
- `response_url` is treated as sensitive webhook data and is never logged.

If `workflow-rules` is omitted or empty, Murtaugh installs a built-in ping/pong
rule. If you already have workflow rules configured, add an explicit
`startup-ping-pong` rule that points at `ping/02-pong.json`. Murtaugh first
looks for that template relative to your config directory and then falls back to
the embedded reference asset. Pressing the startup message's `ping` button posts
the rendered pong response through Slack's `response_url`, using the original
startup message timestamp as `thread_ts` so the response appears in the message
thread.

## Reference assets

The `assets/` directory contains a fake `slack.yaml` plus the default ping and
pong JSON payloads. They are safe reference files; copy or adapt them to your
runtime config/template location if you want to override the built-in defaults.

## Run

~~~sh
go run ./cmd/murtaugh-slack
~~~

Use `--config /path/to/slack.yaml` to load a non-default config file.
