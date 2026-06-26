# Murtaugh customisation architecture

The structural rules for everything you build on top of Murtaugh. Your Murtaugh
config lives at `~/.config/murtaugh` (this is what the daemon reads in
production), and a customisation is anything you add or change there: automation
routines, Slack workflow rules, Block Kit templates, and scheduled jobs.

> These conventions are binding. When you add or change anything below, follow
> the structure here and keep the per-area registries (e.g.
> `automations/AGENTS.md`) up to date — see [Keeping docs in sync](#keeping-docs-in-sync).

## The customisation surface

Murtaugh is customised across a few surfaces. This skill owns the *architecture*
that ties them together; the **mechanics** of each surface live in a dedicated
capability skill — defer to them for flag/tool details:

| Surface | What it is | Deep-dive skill |
|---|---|---|
| `automations/` | Python (or shell) routines that do the work | — (rules below) |
| `slack.yaml` | Slack workflow rules: reply / run / unfurl / interactive (buttons) | `murtaugh-slack` (`workflow-rules.md`) |
| active Slack actions | post / update / read messages from code | `murtaugh-slack` (`messaging.md`) |
| `agents.yaml` | agent definitions: provider/model, tools, `approval:` gate, context/cache | `murtaugh-agents` |
| `templates/` | static Block Kit payloads | `murtaugh-slack` (`blocks.md`) |
| link previews | URL unfurling | `murtaugh-slack` (`unfurl.md`) |
| `jobs.yaml` | scheduled / on-demand job execution | `murtaugh-jobs` |

> **`agents.yaml` carries an `approval:` block** (per agent) that gates side-effecting
> terminal commands behind a Slack confirmation: `terminal: allowlist` (the default —
> auto-run recognized read-only commands, ask for anything else), `prompt` (ask for
> every command), or `off` (never ask), plus `allow: [...]` to extend the read-only set
> with your own commands. This gate, and the agent's `ask` / `present_plan` tools, are
> Murtaugh's **native interactivity** — a path **separate from `slack.yaml`**. They open
> their prompts (buttons and real modals) through the gateway's interaction broker and
> block the agent's turn for the answer; they do **not** go through `workflow-rules`. See
> `murtaugh-agents`.

## Top-level layout

```
~/.config/murtaugh/
├── AGENTS.md                 # agent persona + working guidelines (not architecture)
├── agents.yaml               # agent definitions
├── jobs.yaml                 # scheduled jobs (cron/interval) -> commands
├── journal.yaml              # journal configuration
├── slack.yaml                # Slack workflow rules: reply / run / unfurl / interactive
├── slack-*.yaml              # additional Slack identities (e.g. a second workspace bot)
├── automations/              # the automation routines (see below)
├── templates/                # static Block Kit templates (see below)
├── .agents/skills/           # your bespoke skills (bundled murtaugh-* are in-binary; see below)
└── temp/                     # scratch space; throwaway scripts live in temp/scripts
```

## Automations (`automations/`)

The routines Murtaugh runs (wired from `jobs.yaml` and `slack.yaml`). The
catalogue of *which* automations exist lives in `automations/AGENTS.md`; the
*rules* for how they are structured are here.

### `shared/` is for cross-automation logic only

`automations/shared/` holds *your own* code genuinely used by **more than one**
routine — e.g. common Block Kit formatting helpers. If a piece of logic is used by
only a single routine, it does **not** belong here. Import it with the `shared`
prefix:

```python
from shared.formatting import render_card
```

> Slack and GitHub are **not** a `shared/` module: routines reach them by shelling
> out to the `murtaugh` and `gh` CLIs through injectable runners (so tests can
> fake them) — see `bdd.md` → *Testability rules*. Don't write a per-machine
> Slack client; use `murtaugh slack … --json`.

### Each routine is a self-contained folder

A routine is a folder directly under `automations/` (e.g. `pull_request/`).
Everything the routine needs lives inside it, so it can be reasoned about — and
moved — as one unit. Within that folder:

- **Exactly one entrypoint: `main.py`.** Nothing else in the folder is invoked
  directly by `jobs.yaml` / `slack.yaml`. If a routine performs several
  independent operations, `main.py` is a thin **router** that dispatches to them
  by subcommand and holds no business logic itself. Each operation lives in its
  own module exposing `main(argv) -> int`, wired as `main.py <subcommand> [args]`
  (e.g. `main.py review-queue`, `main.py approve --pr ...`). This keeps a
  routine's full set of capabilities discoverable from one file. Operation
  modules carry no `if __name__ == "__main__"` block — only `main.py` does.
- **State files go in `state/`.** Persisted JSON and other runtime state belong
  in `<routine>/state/`. Derive the path relative to the script
  (`os.path.dirname(__file__) + "/state"`) so moving the folder moves its state.
- **Routine-local shared logic goes in `lib/`.** When multiple files in the
  routine share logic, place it in `<routine>/lib/` (e.g.
  `pull_request/lib/prstate.py`). This is scoped to the *one* routine — distinct
  from the top-level `shared/`, which is cross-routine.

Because a routine's own `lib/` and the top-level `shared/` have different package
names, both can be on `sys.path` at once with no collision. A typical setup:

```python
_HERE = os.path.dirname(os.path.abspath(__file__))   # automations/<routine>
sys.path.insert(0, os.path.dirname(_HERE))           # automations/  -> `shared`
sys.path.insert(0, _HERE)                            # the routine   -> `lib`
from shared.formatting import render_card             # your cross-routine helper
from lib import prstate                               # this routine's helper
```

Even a single-operation routine gets its own folder with a `main.py` entrypoint.
There is no shared top-level `state/` — each routine owns its state.

### Wiring

Routines are registered where Murtaugh invokes them, by their **deployed**
absolute path under `~/.config/murtaugh/automations/...`:

- Scheduled / on-demand jobs → `jobs.yaml` (see `murtaugh-jobs`).
- Slack interaction / workflow triggers → `slack.yaml` (see `murtaugh-slack`,
  `workflow-rules.md`).

When you move or rename a routine, update those references too. Changes take
effect once the daemon reloads the config (or after a redeploy to
`~/.config/murtaugh`).

## Templates (`templates/`)

`templates/` holds **static Block Kit templates** — JSON files with Go-template
placeholders (`{{ .Payload... }}`, `{{ .URL }}`, `{{ .Captures... }}`) that
Murtaugh renders and sends. Reach for a template **any time you want to render a
static Block Kit payload** rather than build blocks dynamically in code:

- Reply / run acks and fixed responses → referenced by `template:` in a
  `slack.yaml` rule (e.g. `templates/github/approved.json`).
- Link unfurls → `templates/unfurl/...` (e.g. `templates/unfurl/github-pr.json`;
  see the `murtaugh-slack` skill's `unfurl.md`).

Use a template when the structure is fixed and only values are interpolated. When
a message's *shape* depends on runtime logic (conditional blocks, loops over a
variable number of items), build the blocks in the automation code instead — see
how `pull_request` renders its self-updating plan cards in Python.

## Skills

A skill is a folder with a `SKILL.md`. Two kinds, with two different homes:

- **Murtaugh-managed skills** (the `murtaugh-*` ones, e.g. `murtaugh-slack`,
  `murtaugh-jobs`, and this one). These ship **inside the binary** and are served
  only through the gated `skills` tool — they are **not** on disk by default, so
  the file/terminal tools can't read them (and an agent can't accidentally edit
  them; they always reflect the shipped binary). An agent's
  `export_skills_to_fs` can mirror chosen ones into its workdir for a
  filesystem-discovering backend — see the `murtaugh-agents` skill.
- **Bespoke / ad-hoc skills.** You author these on disk under
  `<workspace>/.agents/skills/`; they're layered in alongside the managed ones and
  may use the same `requires:` / `templated:` frontmatter for capability gating.
  **Do not use the `murtaugh-` prefix** for bespoke skills — that namespace belongs
  to Murtaugh (the export reconcile manages it and will remove stray `murtaugh-*`
  dirs it doesn't recognise).

## Keeping docs in sync

`automations/AGENTS.md` is the **registry of implemented automations** — what
each routine does, how it is triggered, and where its state lives. It is *not* a
place for architecture rules (those live here).

Whenever you add, remove, or materially change an automation, you **must** update
`automations/AGENTS.md` in the same change so the registry always reflects what is
actually implemented. Treat a stale registry as a bug.
