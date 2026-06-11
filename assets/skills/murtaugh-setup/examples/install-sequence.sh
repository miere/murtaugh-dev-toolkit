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

# 3. ACP agent (omit --command to leave ACP chat disabled).
murtaugh setup agents --command /usr/local/bin/acp-agent --args --stdio

# 4. (macOS) install + start the gateway daemon as a LaunchAgent.
murtaugh setup launchd --binary-path "$BIN" --load true

# 5. (optional) expose Murtaugh's tools to an MCP client.
murtaugh setup mcp-register --client opencode --binary-path "$BIN"

# Later: self-update the binary, then reload the daemon.
# murtaugh setup update
# murtaugh setup launchd --binary-path "$BIN" --load true
