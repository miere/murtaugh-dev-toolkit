# Architecture

This document is the orientation guide for anyone — human or AI agent — changing
the Murtaugh Dev Toolkit. It describes what each package does, how data flows
between them, and the conventions you must respect so changes stay consistent.

## What this service is

A Go toolkit that ships a single binary with **three frontends** backed by a
shared Tool registry:

- **Slack gateway** — Socket Mode daemon (`murtaugh slack gateway`) that
  responds to slash commands, runs YAML-defined **workflow rules** against
  interactive payloads (Block Kit buttons, etc.), bridges Slack conversations
  to an **ACP** (Agent Communication Protocol) agent with live response
  streaming, and renders **custom link unfurls** for shared URLs.
- **CLI** — human-facing direct invocation (`murtaugh <tool> [...]`), including
  the Slack tools under the `slack` namespace (`murtaugh slack send-msg`, …).
- **MCP** — JSON-RPC stdio server (`murtaugh mcp`) that exposes every
  registered tool to AI clients.

Module path: `github.com/miere/murtaugh-dev-toolkit`.

## High-level architecture

```
                          ┌──────────────┐
                          │ cmd/murtaugh │
                          └──────┬───────┘
                                 │
                          ┌──────▼───────┐
                          │ internal/app │   ← composition root
                          └──────┬───────┘
                  builds Registry + selects Mode
        ┌────────────────────────┼────────────────────────┐
        │                        │                        │
┌───────▼────────┐      ┌────────▼────────┐      ┌──────────▼─────────┐
│ frontends/cli  │      │ frontends/mcp   │      │ slack/gateway      │
└───────┬────────┘      └────────┬────────┘      └────────────────────┘
        │                        │
        └──────► Tool ◄──────────┘
                  (internal/tools/*)
```

- `internal/app` is the only place tools are wired into the registry.
- CLI and MCP frontends know nothing about each other and reach tools only
  through `tools.Tool`.
- The Slack gateway does not use the Tool registry today; it runs side-by-side
  as a third frontend selected by mode (`ModeGateway`). The `slack.*` tools,
  by contrast, are ordinary registry tools shared by the CLI and MCP frontends.

## Repository layout

```
cmd/murtaugh/         Entry point: flag parsing, mode selection, signal handling.
internal/app/         Composition root + Registry wiring.
internal/frontends/   CLI and MCP adapters over the Tool registry.
  cli/                Human frontend: kebab → snake flag mapping, render dispatch.
  mcp/                MCP stdio adapter wrapping the same Registry.
internal/tools/       Shared Tool interface + one package per tool.
  tool.go             Tool interface + Registry.
  ping/               Health-check tool (the canonical example).
  jobs/run/           Tool `jobs.run`: execute a job defined in jobs.yaml.
  jobs/define/        Tool `jobs.define`: register a job in jobs.yaml.
  slack/              Slack tools: send-msg, fetch-msgs, fetch-reactions, update-msg.
internal/config/      Config schema, loading, and validation.
internal/slack/       Slack subsystem:
  gateway/            Socket Mode gateway, event loop, all Slack event handlers.
  client/             Slack Web API client wrapper used by the slack.* tools.
internal/acp/         ACP process client, session manager, protocol types.
internal/workflow/    Workflow engine, command runner, template rendering.
internal/unfurl/      Link matcher and Block Kit attachment renderer.
assets/               Embedded reference config, JSON templates, agent skills.
```

`internal/*` is private to this module. Cross-package dependencies flow in one
direction: `slack/gateway` orchestrates `config`, `acp`, `workflow`, and
`unfurl`; those packages do not import `slack/gateway`. Tool packages depend
on `config`
where they need shared types (e.g. `JobProfile`), never the other way around.

## The Tool contract

```go
type Tool interface {
    Name() string
    Description() string
    InputSchema() *jsonschema.Schema
    Invoke(ctx context.Context, args map[string]any) (any, error)
}
```

- `Name` is the registry key. A `.`-separated name (e.g. `jobs.run`) declares
  the tool as belonging to a namespace; the CLI frontend resolves
  `murtaugh jobs run` to the registered name `jobs.run`.
- `Description` is a one-line human-readable hint used by MCP clients.
- `InputSchema` returns the JSON Schema that documents and validates the
  tool's parameters. Returning `nil` means the tool takes no parameters.
- `Invoke` receives args keyed by the JSON-Schema property names declared on
  `InputSchema`. The CLI frontend maps `--kebab-case` flags to `snake_case`
  keys before invocation; array-typed properties accumulate when the flag is
  repeated.

### Frontend conventions

- **CLI** writes tool output to stdout and diagnostics to stderr. Tool
  results dispatch by type via `cli.Render`: `string`, `[]string`,
  `fmt.Stringer`, and finally `%v` fallback. Tools that own a rich result
  struct provide a `String()` method for the CLI representation and let the
  MCP frontend JSON-marshal the same struct.
- **MCP** wraps every registered tool's result in a single `TextContent`
  block. Strings pass through; everything else is JSON-marshalled. Tool
  errors map to `CallToolResult{IsError: true, ...}`.

### Adding a new tool

1. Create `internal/tools/<flat>/` or `internal/tools/<ns>/<sub>/` and
   implement the `Tool` interface.
2. Register the new tool in `internal/app/application.go::buildRegistry`.
3. Add a `_test.go` covering happy-path invocation and schema declarations.
4. **Document the command in `assets/cli-help.md`** (see "CLI/MCP command
   reference" below) and add it to the command list in
   `internal/help/help_test.go::TestEveryCommandDocumented`.
5. Note the command in `README.md`.

The same rule applies when you **change or remove** a tool: every flag rename,
new flag, changed default, new enum value, or deleted command must be reflected
in `assets/cli-help.md` in the same change. `go test ./internal/help/...` fails
if a registered command has no help section, but it cannot catch a stale flag
description — keep the prose honest by hand.

### CLI/MCP command reference (`assets/cli-help.md`)

`assets/cli-help.md` is the **single source of truth** for what every command
does and which flags it takes. It is embedded into the binary (via the
`assets` `go:embed` directive) and surfaced by the `internal/help` package:

- `murtaugh help` prints the whole document; `murtaugh help <command>` (e.g.
  `murtaugh help slack send-msg`) and `murtaugh <command> --help` print a single
  command's section.
- CLI usage errors and the bare-invocation usage line point users at
  `murtaugh help`.

The document is plain Markdown with one parser contract: each command's section
begins with a level-2 header of the exact form `## murtaugh <invocation>`
(e.g. `## murtaugh jobs run`). `help.Section` returns everything from that header
up to the next `## ` or `# ` line, and accepts both the spaced CLI form
(`jobs run`) and the dotted registry form (`jobs.run`). Top-of-file overview
material uses level-1 (`# `) headers so it is never mistaken for a command
section. When you add a command, follow that header convention or `help.Section`
will not find it.

Because tool flag descriptions also live in each tool's `InputSchema`, keep the
two in agreement: the schema `Description` is the one-line MCP hint; the
`cli-help.md` section is the full human/agent-facing manual (required/optional,
defaults, mutual exclusions, the boolean-needs-a-value CLI quirk, examples).

## Lifecycle (entrypoint → shutdown)

`cmd/murtaugh/main.go`:

1. Extracts the global `--config` flag from `os.Args` (supports
   `--config PATH` and `--config=PATH`).
2. Resolves the config path (`config.DefaultPath()` →
   `~/.config/murtaugh/slack.yaml`, overridable with `--config`).
3. Selects the mode: `slack gateway` → `ModeGateway`, `mcp` → `ModeMCP`, or
   any other tokens (including `slack <tool>`) → `ModeCLI`. No subcommand, or
   a bare `slack`, prints usage rather than launching anything.
4. `config.Bootstrap(path)` seeds the config directory on first run, then
   `config.Load(path)` reads, parses, validates, and records `BaseDir`.
5. Builds an `slog.Logger` (text handler; debug level when
   `configuration.debug: true`; warn level for CLI mode so tool output
   dominates the terminal).
6. Creates a `signal.NotifyContext` for `SIGINT`/`SIGTERM`.
7. `app.New(...)` builds the Registry and the chosen frontend; `Run(ctx)`
   blocks until the context is cancelled or the frontend returns.

## Configuration (`internal/config`)

`Config` is the root struct, populated from YAML via `gopkg.in/yaml.v3`:

- `BaseDir` (`yaml:"-"`) — directory of the loaded file; used as the template
  search root.
- `OAuth` — Slack Socket Mode and bot tokens (`oauth.app_token`,
  `oauth.bot_token`) loaded from `slack.yaml`.
- `Configuration` — runtime Slack settings (`configuration.admin_user`,
  `configuration.debug`) loaded from `slack.yaml`.
- `Chat` — routing fields (`chat.default_agent`, `chat.dm_agent`,
  `chat.channel_agents`) loaded from `slack.yaml`.
- `ACP` (`yaml:"-"`) — timeout and streaming knobs loaded from `agents.yaml`.
- `Agents` (`yaml:"-"`) — map of agent profiles loaded from `agents.yaml`.
- `Commands` — registered slash commands (names must start with `/`).
- `WorkflowRules` — `map[string]WorkflowRuleConfig` keyed by rule name.
- `UnfurlRules` — `map[string]UnfurlRuleConfig` keyed by rule name.

### Multi-agent routing

ACP settings and agent profiles are defined in `agents.yaml`. Murtaugh routes
chat requests to agents based on the `chat` config in `slack.yaml`:

1.  **Direct Messages**: Use `chat.dm_agent` if set, otherwise `chat.default_agent`.
2.  **Channels**: Use `chat.channel_agents[channel_id]` if set, otherwise
    `chat.default_agent`.

`chat.default_agent` is required when `acp.enabled: true`.

`Load` → `Parse` → load `agents.yaml` / `jobs.yaml` → `Validate()`. Validation
covers required Slack tokens, slash-command name prefixes, ACP durations, agent
routing references, and per-rule checks.

### Triggers and actions

`TriggerConfig` has a **custom `UnmarshalYAML`** that requires a mapping with
exactly one key — either `reply-to-slack` (→ `ReplyToSlackTriggerConfig`) or
`run` (→ `RunTriggerConfig`). Any other key is rejected at parse time.

- `ReplyToSlackTriggerConfig` — `template` (path) or a nested `run`.
- `RunTriggerConfig` — `cmd`, `args`, `timeout`, `workdir`.

`UnfurlRuleConfig` = `Match` (`channels`, `domain`, `url_prefix`, `url_pattern`)
+ `Unfurl` (`template` **xor** `run`). `validateUnfurlRule` requires at least one
content condition, a compilable `url_pattern`, non-blank channel entries, and
exactly one action.

## Slack gateway (`internal/slack/gateway`)

`Gateway` owns the `*slack.Client`, the `*socketmode.Client`, and the four
subsystems (`handler`, `workflow`, `chat`, `unfurl`). `New()` wires them:

- `chat` is built **only if** `acp.enabled` is true.
- `unfurl` is built **only if** `unfurl-rules` is non-empty (a bad matcher logs
  and disables the feature rather than crashing).
- `workflow.NewEngine` always exists; with no rules it falls back to a built-in
  `ping-pong` default.

### Event loop

`Run` launches `socket.RunContext` in a goroutine, warms the ACP client, then
selects over `socket.Events`, dispatching each to `handleEvent`:

| Socket event            | Handler                | Behaviour                                            |
|-------------------------|------------------------|------------------------------------------------------|
| `Connected`             | `notifyStartup`        | Opens a DM with `configuration.admin_user`, sends the startup ping (once). |
| `SlashCommand`          | `handleSlashCommand`   | `/...  chat` → ACP chat; otherwise the default handler acks. |
| `Interactive`           | `handleInteractive`    | Acks, then runs `workflow.Execute` in a goroutine (5 min).   |
| `EventsAPI`             | `handleEventsAPI`      | Routes inner events (below).                          |

Inner Events API events:

- `LinkSharedEvent` → `handleLinkShared` (unfurl subsystem).
- `AppMentionEvent` → `startChat` (skipped if chat disabled or sender is a bot).
- `MessageEvent` → `startChat` **only** for direct messages
  (`ChannelType == "im"`, no bot/subtype).

Handlers **ack first, then work asynchronously** in a goroutine with a bounded
context. Long work must never block the event loop.

## ACP chat (`internal/acp` + `slack/gateway/chat_handler.go`)

`acp.Client` is the interface (`Initialize`, `NewSession`, `Prompt`, `Cancel`,
`Close`). `ProcessClient` implements it by speaking **JSON-RPC over the agent's
stdio** (NDJSON): requests carry an incrementing id, responses are matched via a
`pending` map, and `session/update` notifications fan out to per-session
subscriber channels as `Event`s (`text`, `status`, `complete`, `error`).

`SessionManager` caches sessions keyed by `ConversationKey`
(`TeamID`/`ChannelID`/`ThreadTS`/`DM`). It initializes the client lazily (or via
`Warm`), evicts on idle timeout / `max_sessions`, and reuses sessions so a Slack
thread maps to one persistent agent conversation.

`ChatHandler.Handle` builds the key + `SessionMetadata`, sets the assistant
status to `is thinking...`, then ranges over the prompt's event channel.

## Streaming (`slack/gateway/stream_api.go` + stream writer)

`StreamAPI` abstracts the Slack streaming surface so the chat handler is
testable: `StartStreamContext`, `AppendStreamContext`, `StopStreamContext`, and
`SetAssistantThreadsStatusContext`. `*slack.Client` satisfies it in production.

The stream writer is **lazy**: the live message is only started on the **first
non-empty text chunk**, appends are batched by `stream_append_interval` /
`stream_min_chunk_chars`, and the stream is stopped on `complete` (or `Fail`ed on
error). Streaming requires a source message timestamp; without one `Handle`
returns an error. A `ConversationKey` requires a thread timestamp for replies.

## Workflow engine (`internal/workflow`)

`Engine` holds rules sorted by name (deterministic, first-match-wins), a
`ResponsePoster`, a `CommandRunner`, and the template search roots. `Execute`
marshals the `slack.InteractionCallback` to both a `map[string]any` (for
matching) and JSON (for `run` stdin), finds the first `interactive` rule whose
`match` is a subset of the payload, and runs its triggers in order:

- `reply-to-slack` → render JSON (template or nested `run`) and POST it to the
  interaction's `response_url`.
- `run` → execute the command with the payload JSON on stdin.

**Template rendering** uses Go `text/template` with `Option("missingkey=error")`,
so every field referenced in a template must exist in the data
(`{"Payload": payload}`). Output must be valid JSON (`validJSON`).

`CommandRunner.Run(ctx, RunTriggerConfig, stdin []byte) ([]byte, error)` is the
external-process contract. `OSCommandRunner` enforces a timeout (default 30s),
pipes stdin in, and captures stdout. **Convention: handlers read a JSON object on
stdin and print a single JSON object on stdout.**

## Custom link unfurling (`internal/unfurl` + `slack/gateway/link_unfurl_handler.go`)

- `Matcher` compiles rules once (sorted-key order). `Match(url, domain, channel)`
  returns the first rule whose optional channel allowlist, domain (exact or
  subdomain suffix), `url_prefix`, and RE2 `url_pattern` all match. Named regex
  groups are returned as `Captures`.
- `Renderer` turns a Block Kit JSON template into a `slack.Attachment`, resolving
  the path against the config dir first, then the embedded `assets.FS`. The
  template/`run` data is the exported `Data` struct (`URL`, `Domain`, `Channel`,
  `User`, `MessageTS`, `ThreadTS`, `TeamID`, `Captures`). `ParseAttachment`
  rejects non-JSON output.
- `LinkUnfurlHandler.Handle` skips composer-mode events (non-numeric timestamp)
  and the bot's own links, dedupes URLs, caps at 10, builds each preview
  (`run` → JSON stdin/stdout, or `template` → render), isolates per-link
  failures, and posts one `chat.unfurl` (`UnfurlMessageContext`).

Each `match.domain` must be registered in the Slack app's **App Unfurl Domains**
list (max 5) or no `link_shared` event is delivered.

## Assets and embedding (`internal/../assets`)

`assets/assets.go` embeds reference files via
`//go:embed slack.yaml agents.yaml jobs.yaml cli-help.md templates skills`.
Block Kit templates live under `templates/` (`ping/`, `unfurl/`); bundled agent
skills live under `skills/`, each a `SKILL.md` + `reference/` + `examples/` tree.
`cli-help.md` is the canonical command reference (see "CLI/MCP command
reference" above). The `templates` and `skills` directories are embedded
recursively.

The embedded FS is the **fallback** template source: the workflow engine and unfurl renderer both
look in the config directory first, then `assets.FS` — so a config template path
like `templates/unfurl/github-pr.json` resolves to `<workspace>/templates/unfurl/github-pr.json`
on disk, falling back to the same path inside `assets.FS`. **If you add a new asset
directory, the recursive `templates`/`skills` embeds cover nested files, but a new
top-level asset must be added to the `go:embed` directive** or it will not ship in the binary.

`config.Bootstrap` runs on every start. It mirrors `templates/` into
`<workspace>/templates/` and `skills/` into `<workspace>/.agents/skills/`, then
symlinks `<workspace>/.claude/skills` to `.agents/skills` so both ACP and
Claude-based agents discover the bundled skills. The two trees use different
copy policies (`copyPolicy` in `bootstrap.go`): **config files and `templates/`
are `preserveExisting`** (seeded once, never overwritten, since they hold the
user's tokens/edits), while **`skills/` is `refreshFromAssets`** — shipped skill
files are rewritten in place when their content drifts from the embedded copy,
so a binary upgrade keeps the workspace skills current. Bootstrap only ever
writes files it embeds, so user-added skills are never deleted; an unchanged
file is a no-op (no mtime churn). The `BootstrapReport` buckets each path as
Created, Updated, or Preserved.

## Testing conventions

- Tests are standard `go test` table tests colocated with each package.
- Interfaces (`StreamAPI`, `ChatSessionManager`, `Unfurler`, `CommandRunner`,
  `ResponsePoster`, `acp.Client`) exist primarily so handlers can be driven by
  **fakes/stubs** (`fakeStreamAPI`, `fakeChatSessions`, fake `Unfurler`,
  `stubRunner`). Add a fake rather than reaching for real Slack/process I/O.
- Use `discardLogger()` (a `slog.Logger` over `io.Discard`) in tests.
- Config behaviour is verified by parsing YAML strings through `Parse` and
  asserting on both success and the specific validation error.

## Guidelines for making changes

1. **Work in a dedicated git worktree** under `ignore/worktrees/`, branched off
   the up-to-date local `HEAD`. Never push without explicit permission.
2. **Keep config changes complete.** A new config field means: struct field +
   `yaml` tag, `Validate()` coverage, an example in the relevant `assets/*.yaml`,
   a README note, and tests in `config_test.go`.
2b. **Keep command docs complete.** A new/changed/removed tool or flag means
   updating its section in `assets/cli-help.md` (and the command list in
   `internal/help/help_test.go`). See "Adding a new tool".
3. **Trace downstream effects.** New Slack events need a `handleEvent` /
   `handleEventsAPI` case; new ACP event types need handling in `ChatHandler`.
4. **Respect the JSON contracts.** Templates and `run` handlers must emit valid
   JSON; `missingkey=error` means every referenced template field must be present.
5. **Embed new assets** by updating the `go:embed` directive.
6. **Validate before finishing:** `go build ./...`, `go vet ./...`, and
   `go test ./...` must all pass. Add or update tests for the behaviour you change.
7. **Match the surrounding style** — small, focused types; constructor functions
   that default optional dependencies; bounded contexts for async work.
