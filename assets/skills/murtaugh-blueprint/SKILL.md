---
name: murtaugh-blueprint
description: Architecture and conventions for customising Murtaugh — automations/, templates/, Slack rules, and scheduled jobs, and how they fit together; the entry point before changing how Murtaugh behaves.
requires: [manage]
files:
  reference/architecture.md: { requires: [manage], summary: "layout, conventions, and wiring of a customisation" }
  reference/bdd.md:          { requires: [manage], summary: "confirm behaviour with the user and lock it as behave tests" }
---

# Skill: Murtaugh Customisation Blueprint

How to **organise and implement a customisation** of Murtaugh — the architecture
and conventions that keep automations, Slack workflow rules, Block Kit templates,
and scheduled jobs consistent, discoverable, and maintainable. Use this whenever
the user is **building or changing how Murtaugh behaves**: adding or editing an
automation routine, wiring a Slack workflow rule in `workflow-rules.yaml`, creating a
Block Kit template, scheduling a job in `jobs.yaml`, or reorganising any of these.
It is **not** needed for ordinary chat, reading data (reminders, mail, etc.), or
one-off tasks that don't touch the config.

> This is the *connective architecture* skill. For the mechanics of each surface,
> defer to the capability skills: `murtaugh-slack` (messaging, buttons/workflow
> rules, link previews — see its `messaging.md` / `workflow-rules.md` / `unfurl.md`)
> and `murtaugh-jobs` (scheduling). This skill is how you fit them together.

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Designing or restructuring a customisation — layout, conventions, wiring | `reference/architecture.md` |
| Confirming behaviour with the user and enforcing it as tests | `reference/bdd.md` (harness in `harness/`) |

`reference/architecture.md` is the binding structural reference: the
`automations/` layout (`shared/`, self-contained routine folders, the single
`main.py` entrypoint, `state/`, routine-local `lib/`, imports), how `templates/`
and `skills/` work, and how customisations are wired into `workflow-rules.yaml` / `jobs.yaml`.

## Workflow — follow this every time you customise

1. **Plan first.** State what the user wants, which surface(s) it touches
   (automation / `workflow-rules.yaml` rule / template / job), and a short
   verification checklist *before* writing anything.
2. **Load the rules.** Read `reference/architecture.md` and the relevant
   capability skill(s) for the surface you're touching.
3. **Check the registry.** Read `automations/AGENTS.md` to see what already
   exists — reuse a routine or `shared/` helper rather than duplicating.
4. **Implement to the conventions.** New automation → a self-contained folder
   with a single `main.py` entrypoint; shared-across-routines code → `shared/`;
   routine-local helpers → that routine's `lib/`; state → that routine's
   `state/`; static Block Kit → a `templates/` file.
5. **Wire it.** Register the entrypoint by its deployed
   `~/.config/murtaugh/...` path in `jobs.yaml` (schedule) and/or `workflow-rules.yaml`
   (Slack trigger).
6. **Update the registry.** Add or update the routine's entry in
   `automations/AGENTS.md` in the *same* change. A stale registry is a bug.
7. **Verify.** Run a safe preview first — most routines honour `DRY_RUN=1`
   (no Slack sends, no state writes) — then confirm the work against your
   checklist before calling it done.

## Conventions at a glance

- **One entrypoint per routine: `main.py`.** Multiple operations → `main.py`
  routes subcommands; operation modules expose `main(argv) -> int` and have no
  `__main__` block.
- **`shared/` = cross-routine only.** Routine-local logic goes in the routine's
  own `lib/`.
- **State is routine-owned**, under `<routine>/state/`, pathed relative to the
  script so it moves with the folder.
- **Templates are for *static* Block Kit**; build blocks in code when the
  message shape is dynamic.
- **`murtaugh-*` skills are overwritten on update** — keep bespoke skills
  un-prefixed.

See `reference/architecture.md` for the full, binding version of each rule.

## Verification discipline

Pair every customisation with a way to confirm it works, scaled to the change:

- **Non-trivial automations** → use the BDD loop in `reference/bdd.md`: draft
  Gherkin scenarios → **confirm them with the user** → encode as `behave` tests
  (the confirmed scenarios are the locked oracle) → iterate code↔tests until green
  → finish with a read-only `DRY_RUN` acceptance. Build routines with an injection
  seam and reuse the fakes/steps in `harness/` so tests run with no network.
- **Trivial template/rule/config edits** → a `DRY_RUN`/preview plus a short
  confirmed checklist is enough; skip `behave`.

Once the user confirms scenarios, do not weaken or edit them to make code pass —
a behaviour change means re-confirming first.
