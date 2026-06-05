# Architecture

This document is the orientation guide for anyone — human or AI agent — changing
the Murtaugh Dev Toolkit. It describes what each package does, how data flows
between them, and the conventions you must respect so changes stay consistent.

## What this service is

A Go service that connects Murtaugh to Slack over **Socket Mode**. It:

- responds to slash commands,
- runs YAML-defined **workflow rules** against interactive payloads (Block Kit
  buttons, etc.),
- bridges Slack conversations to an **ACP** (Agent Communication Protocol) agent
  with live response streaming, and
- renders **custom link unfurls** for shared URLs.

Module path: `github.com/miere/murtaugh-dev-toolkit`.

## Repository layout

```
cmd/murtaugh-slack/   Entrypoint (flag parsing, logger, signal handling).
internal/config/      Config schema, loading, and validation.
internal/slackapp/    Socket Mode app, event loop, all event handlers.
internal/acp/         ACP process client, session manager, protocol types.
internal/workflow/    Workflow engine, command runner, template rendering.
internal/unfurl/      Link matcher and Block Kit attachment renderer.
assets/               Embedded reference config, JSON templates, agent skills.
```

`internal/*` is private to this module. Cross-package dependencies flow in one
direction: `slackapp` orchestrates `config`, `acp`, `workflow`, and `unfurl`;
those packages do not import `slackapp`.

## Lifecycle (entrypoint → shutdown)

`cmd/murtaugh-slack/main.go`:

1. Resolves the config path (`config.DefaultPath()` → `~/.config/murtaugh/slack.yaml`,
   overridable with `--config`).
2. `config.Load(path)` reads, parses, validates, and records `BaseDir`.
3. Builds an `slog.Logger` (text handler; debug level when `slack.debug: true`).
4. Creates a `signal.NotifyContext` for `SIGINT`/`SIGTERM`.
5. `slackapp.New(cfg, logger)` then `app.Run(ctx)` blocks until the context is
   cancelled or Socket Mode returns a fatal error.

## Configuration (`internal/config`)

`Config` is the root struct, populated from YAML via `gopkg.in/yaml.v3`:

- `BaseDir` (`yaml:"-"`) — directory of the loaded file; used as the template
  search root.
- `Slack` — `app_token`, `bot_token`, `admin_user`, `debug`.
- `ACP` — agent command, working dir, and all timeout/streaming knobs. The
  `Effective*()` methods supply defaults when fields are blank/zero.
- `Commands` — registered slash commands (names must start with `/`).
- `WorkflowRules` — `map[string]WorkflowRuleConfig` keyed by rule name.
- `UnfurlRules` — `map[string]UnfurlRuleConfig` keyed by rule name.

`Load` → `Parse` → `Validate()`. **`Parse` fails fast if `Validate()` fails**, so
invalid config never reaches the running app. Validation covers required Slack
tokens, slash-command name prefixes, ACP durations, and per-rule checks.

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

## Slack app (`internal/slackapp`)

`App` owns the `*slack.Client`, the `*socketmode.Client`, and the four
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
| `Connected`             | `notifyStartup`        | Opens a DM with `admin_user`, sends the startup ping (once). |
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

## ACP chat (`internal/acp` + `slackapp/chat_handler.go`)

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

## Streaming (`slackapp/stream_api.go` + stream writer)

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

## Custom link unfurling (`internal/unfurl` + `slackapp/link_unfurl_handler.go`)

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
`//go:embed slack.yaml ping/*.json unfurl/*.json skills/*.md`. The embedded FS is
the **fallback** template source: the workflow engine and unfurl renderer both
look in the config directory first, then `assets.FS`. **If you add a new asset
directory or extension, you must extend the `go:embed` directive** or the files
will not ship in the binary.

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
   `yaml` tag, `Validate()` coverage, an example in `assets/slack.yaml`, a README
   note, and tests in `config_test.go`.
3. **Trace downstream effects.** New Slack events need a `handleEvent` /
   `handleEventsAPI` case; new ACP event types need handling in `ChatHandler`.
4. **Respect the JSON contracts.** Templates and `run` handlers must emit valid
   JSON; `missingkey=error` means every referenced template field must be present.
5. **Embed new assets** by updating the `go:embed` directive.
6. **Validate before finishing:** `go build ./...`, `go vet ./...`, and
   `go test ./...` must all pass. Add or update tests for the behaviour you change.
7. **Match the surrounding style** — small, focused types; constructor functions
   that default optional dependencies; bounded contexts for async work.
