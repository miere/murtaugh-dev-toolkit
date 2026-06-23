# Running Murtaugh as an MCP server

`murtaugh mcp` runs Murtaugh as a **stdio MCP server** (JSON-RPC over
stdin/stdout). It exposes every Murtaugh tool — `ping`, `version`, `jobs_*`,
`slack_*`, `setup_*` (including `setup_env`) — to any MCP client, each advertised
with its own input schema. The agent's mid-turn interaction tools `ask` and
`present_plan` are part of the surface too, but only act inside a live Slack
conversation (over plain MCP, with no gateway to route the click back, they
return an error).

## How it's used

You rarely run it by hand; an MCP client launches it. `setup_mcp-register` wires
the launch command (`<binary> mcp`) into the client config — see
`reference/daemon-and-clients.md`. Once registered, the client can:

- **list** the tools (names + schemas), and
- **call** a tool by name with arguments matching its schema; the result comes
  back as JSON text (errors are returned as an error result, not a crash).

## Notes

- It's the **same tools** the CLI exposes — `slack_send-msg`, `jobs_run`, etc. —
  so anything documented in the other skills works identically over MCP; pass the
  schema properties as the tool's arguments.
- **No config required to start.** Like the other setup-adjacent paths, the MCP
  server starts even before a full config exists; individual tools surface a
  clear error if they need configuration that isn't there yet (e.g. a missing bot
  token only fails when you actually call a `slack_*` tool).
- Run it directly only to inspect the surface:

```bash
murtaugh mcp        # speaks JSON-RPC on stdio; Ctrl-C to exit
```
