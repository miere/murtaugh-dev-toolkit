#!/usr/bin/env bash
# Example first-run install sequence for Murtaugh.
# Assumes the `murtaugh` binary is already on PATH.
set -euo pipefail

BIN="$(command -v murtaugh)"

# 1. Seed the workspace (~/.config/murtaugh): config defaults, templates, skills.
murtaugh setup bootstrap

# 2. Slack credentials + admin user.
murtaugh setup slack \
  --app-token "xapp-REPLACE" \
  --bot-token "xoxb-REPLACE" \
  --admin-user "@you"

# 3. Provider API key → ~/.config/murtaugh/.env. A native agent references the
#    key by variable NAME (api_key_env); the value lives only here, never in YAML.
#    --set is repeatable; other .env entries are preserved.
murtaugh setup env --set GEMINI_API_KEY="AIza-REPLACE"

# 4. Native agent (the DEFAULT kind). Passing --provider infers kind=native.
#    --tools is repeatable. Wire chat.default_agent in slack.yaml separately
#    (setup_slack --default-agent, or the murtaugh-agents skill).
murtaugh setup agents \
  --provider gemini --model gemini-2.5-pro \
  --api-key-env GEMINI_API_KEY \
  --tools files --tools terminal --tools skills --tools ask --tools present_plan

#    Alternative: a legacy ACP agent (omit the native flags). Passing --command
#    infers kind=acp; --args is repeatable. Omit both paths to leave chat off.
# murtaugh setup agents --kind acp \
#   --command /usr/local/bin/acp-agent --args --stdio

# 5. (macOS) install + start the gateway daemon as a LaunchAgent.
murtaugh setup launchd --binary-path "$BIN" --load true

# 6. (optional) expose Murtaugh's tools to an MCP client.
murtaugh setup mcp-register --client opencode --binary-path "$BIN"

# Later: self-update the binary, then reload the daemon.
# A bare `setup update` refuses to overwrite a dev build — add --force true.
# murtaugh setup update --force true
# murtaugh setup launchd --binary-path "$BIN" --load true
