# CLI & MCP server

Every Murtaugh capability is registered once as a **tool** and surfaced three
ways: as a Slack interaction, a CLI command, and an MCP tool. This page covers
the latter two — calling tools from your terminal, and exposing them to other AI
clients.

---

## The CLI

Every registered tool is callable directly from your terminal:

```sh
murtaugh ping                      # → pong

murtaugh slack send-msg --to '#general' --body 'hello'
murtaugh jobs run --name nightly-deploy
murtaugh journal query --stream gateway --since 1h --level error
```

### Discovering commands

The binary documents itself — this is the fastest reference:

```sh
murtaugh help                      # list every command
murtaugh help <command>            # full help for one command
murtaugh <command> --help          # same, e.g. `murtaugh jobs run --help`
murtaugh slack                     # a namespace on its own lists its subcommands
```

Every flag, default, and example is documented there.

### Flag conventions

- **Every flag takes a value — booleans included.** Write `--load true`, not a
  bare `--load`.
- **snake_case arg names map to kebab flags** — `binary_path` → `--binary-path`,
  `app_token` → `--app-token`.
- **Schema-typed args are coerced automatically** — `--count 5` → integer,
  `--verbose true` → boolean, repeated `--args` flags → an array.

```sh
murtaugh jobs define \
  --name nightly-deploy \
  --command /usr/local/bin/deploy \
  --args --env --args production \   # repeated --args build the array
  --workdir /srv/deploy \
  --timeout 15m
```

### The gateway is a command too

The long-lived daemon is just another command. It takes no tool flags — only the
global `--config PATH`:

```sh
murtaugh slack gateway
murtaugh --config /path/to/gateway.yaml slack gateway
```

See [Operations](operations.md) for running it as a daemon.

---

## The MCP server

```sh
murtaugh mcp        # speaks MCP JSON-RPC over stdin/stdout
```

This exposes **every registered tool** to MCP-capable AI clients (Claude
Desktop, IDE extensions, etc.) over JSON-RPC on stdio. Stdout is reserved for the
protocol; diagnostics go to stderr.

Over MCP, tool names are dotted (`jobs.run`, `journal.query`, `slack.send-msg`)
where the CLI uses spaces (`murtaugh jobs run`).

### Registering with a client

On macOS the installer can register Murtaugh as an MCP server in supported
clients (backing up any existing client config first). You can also do it
yourself:

```sh
murtaugh setup mcp-register ...    # see `murtaugh help setup mcp-register`
```

Point your MCP client at the `murtaugh mcp` command and it will discover the full
toolset on connect.

---

## Bundled agent skills

Murtaugh ships a set of **agent skills** in-binary — focused, progressive-
disclosure docs that teach a connected agent how to drive each surface
(`murtaugh-setup`, `murtaugh-operations`, `murtaugh-agents`, `murtaugh-slack`,
`murtaugh-jobs`, `murtaugh-journal`, `murtaugh-blueprint`). They are served from
the binary (not written to disk), so a native agent with the `skills` tool, or an
MCP client that reads skills, gets the same operational knowledge these docs
describe — straight from the running version.
