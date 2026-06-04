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
  debug: false

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
payloads for every command listed in the configuration.

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

## Run

~~~sh
go run ./cmd/murtaugh-slack
~~~

Use `--config /path/to/slack.yaml` to load a non-default config file.
