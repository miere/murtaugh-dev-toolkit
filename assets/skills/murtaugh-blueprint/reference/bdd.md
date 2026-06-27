# BDD for Murtaugh customisations

How to use Behaviour-Driven Development (`behave` + Gherkin) as the way you
**agree** on a customisation with the user and then **enforce** that agreement as
tests. This is the verification discipline referenced by the blueprint's main
workflow.

## The loop

```
1. DRAFT & CONFIRM   write Gherkin scenarios from the request → show the user →
                     get an explicit yes. The confirmed scenarios are the spec.
2. ENCODE (lock)     turn the confirmed scenarios into runnable behave tests.
                     Once confirmed, the scenarios are the ORACLE — do not weaken
                     or edit them to make code pass; behaviour change ⇒ re-confirm.
3. RED → GREEN       implement/iterate code, running behave each cycle, until every
                     confirmed scenario passes. Then run one real-systems DRY_RUN
                     as acceptance (fakes ≠ reality).
```

### 1. Draft & confirm

- Write scenarios in plain business language (`Given/When/Then`), one per
  behaviour. Cover the happy path **and** the edges the request implies (failures,
  idempotency, "nothing to do", overflow, dedup, state transitions).
- Present a **concise scenario list** for confirmation (titles + a line each), not
  the whole feature file — Gherkin is verbose for Slack. Keep the full `.feature`
  available/attached.
- Confirmation is a gate: do not start coding until the user says yes.

### 2. Encode as tests (lock the oracle)

- Translate each confirmed scenario into a `.feature` and its step definitions.
- After confirmation, treat the scenarios as fixed. If you discover a scenario was
  wrong or missing, **stop and re-confirm** the change with the user — never
  silently relax a test to go green.

### 3. Red → green cycle, then acceptance

- Run `behave` after each change; iterate until all confirmed scenarios pass.
- **Cap the loop** (e.g. ~5 iterations). If still red, stop and report to the user
  with the failing scenarios — don't grind indefinitely.
- Fakes can be green while the real `gh`/Slack contract differs, so the final
  acceptance is: confirmed scenarios green **and** one read-only `DRY_RUN=1` run
  against the real systems looks right.

## Tier the ceremony to the change

Don't impose the full loop on trivial edits, and don't skip it on anything with
real logic. Decide with this rule:

**Full BDD loop** (confirm → behave → red/green → `DRY_RUN` acceptance) if the
change has **any** of these — i.e. it can be wrong in more than one way:

- reads or writes state (a `state/` file, dedup set, cursor);
- branches on external data (PR checks, reactions, API responses);
- retries, timing, or eventual-consistency logic;
- loops over a variable number of items (digests, queues, batches);
- derives/transitions a status, or composes a message whose *shape* changes.

**Light path** (a `DRY_RUN`/preview + a short **confirmed checklist**, no `behave`)
only if **none** of the above apply — the change is pure static rendering or a
single fixed mapping:

- a static `templates/*.json` payload (only values interpolated);
- a one-line `workflow-rules.yaml` rule with a fixed `template`/`reply` and no logic;
- a config/threshold/schedule tweak.

| Change | Tier |
|---|---|
| Python automation with state / branching / retries / loops / status logic | **Full loop** |
| `workflow-rules.yaml` `run` script (or rule) containing real branching logic | **Full loop** |
| Static `templates/*.json`, a fixed one-line rule, a config/schedule tweak | **Light path** |

When a change spans both (e.g. a new template *and* the automation that fills it),
the automation pulls it into the full loop. When genuinely unsure, default to the
full loop — a confirmed scenario is cheap insurance.

## Testability rules (so the loop can run with NO network)

The automations are thin orchestration over the `murtaugh` and `gh` CLIs — the
two tools every Murtaugh user already has. Reach **Slack through the `murtaugh`
CLI**, not a bespoke per-machine Python client, and recover structured data via
its JSON output:

> The harness covers *automation* code (CLI-driven Slack posting/reading). The agent's
> native interaction tools (`ask` / `present_plan` and the terminal approval gate) are
> **not** exercised by this BDD harness — they're a daemon-side path with no `murtaugh`
> CLI seam to fake. Test those at the Go level, not in `behave`.

```
murtaugh slack send-msg --to "#chan" --body "…" --json   ->   {"ok":true,"channel":"C…","ts":"170…"}
```

> Prerequisite: this assumes the `murtaugh` CLI supports `--json` structured
> output on `slack send-msg`/`update-msg`/`fetch-reactions`. That is a
> Murtaugh-core capability the model depends on.

Build every non-trivial routine with an **injection seam** so the CLI runners and
the clock can be swapped for fakes:

```python
import json, subprocess
from datetime import datetime

def _run_murtaugh(args, check=False):
    return subprocess.run(["murtaugh"] + list(args), capture_output=True, text=True, check=check)

def _run_gh(args, check=False):
    return subprocess.run(["gh"] + list(args), capture_output=True, text=True, check=check)

def main(argv=None, *, murtaugh=_run_murtaugh, gh=_run_gh, now=datetime.now) -> int:
    ...
    res = murtaugh(["slack", "send-msg", "--to", channel, "--body", body, "--json"])
    posted = json.loads(res.stdout)          # {"ok","channel","ts"}
    rows = json.loads(gh(["pr", "list", "--json", "…"]).stdout)
    when = now()                             # not datetime.now() directly
```

- **Slack** → route through the injected `murtaugh` runner (`slack send-msg`,
  `slack update-msg`, `slack fetch-reactions`, all with `--json`); tests pass a
  `FakeMurtaugh`. Never call `subprocess.run(["murtaugh", ...])` inline.
- **`gh`** → route through the injected `gh` runner; tests pass a `FakeGh`. Never
  call `subprocess.run(["gh", ...])` inline.
- **Time** → take `now` (and avoid bare `time.sleep` in tested paths, or make the
  sleep injectable/skippable); tests pass a `FakeClock`.
- Production defaults mean production code is unchanged at call sites; tests just
  override the keyword args.

### Mutable config goes in the seam too (B form — the standard)

Put the routine's **mutable config** — the things a test wants to vary per
scenario, typically `state_file` and `dry_run` — in the same keyword-only seam,
each **defaulting to its module global**. Then thread those *values* down through
the functions that use them as parameters:

```python
STATE_FILE = os.path.join(_HERE, "state", "routine.json")
DRY_RUN = os.environ.get("DRY_RUN", "") not in ("", "0", "false", "False")

def load_state(state_file=STATE_FILE): ...
def save_state(state, state_file=STATE_FILE): ...
def reconcile(murtaugh, item, store, dry_run=DRY_RUN): ...

def main(argv=None, *, murtaugh=_run_murtaugh, gh=_run_gh, now=datetime.now,
         state_file=STATE_FILE, dry_run=DRY_RUN) -> int:
    store = load_state(state_file)
    ...
    reconcile(murtaugh, item, store, dry_run=dry_run)
    if not dry_run:
        save_state(store, state_file)
```

The `main()` boundary is the **only** place the globals are read; production is
unchanged (the defaults *are* the globals). Tests pass a fresh temp path and an
explicit flag per call:

```python
review_queue.main([], murtaugh=context.murtaugh, gh=context.gh,
                  state_file=context.state_file, dry_run=False)
```

Because nothing is mutated, there is **no shared mutable state and no
scenario-order leakage** — two scenarios can't see each other's state file, and
flipping `dry_run` in one can't bleed into the next. Get a fresh
`context.state_file` per scenario from the `before_scenario` hook (the harness
`environment.py` provides one via `tempfile.mkdtemp()`). Do **not** monkeypatch
`routine.STATE_FILE` / `routine.DRY_RUN` — that reintroduces exactly the
cross-scenario coupling the seam exists to remove.

**A-form fallback (legacy only).** An older automation that reads import-time
globals directly and has no seam can be tested by snapshotting and restoring
those globals around each scenario, using the harness
`snapshot_module_config(module, names)` helper in `before_scenario` (restore in
`after_scenario`). This is a stopgap; prefer converting the routine to B form.

`DRY_RUN` is **not** a substitute for fakes: it suppresses side effects but you
can't assert on the structured payload that *would* have been sent. Use fakes for
assertions; use `DRY_RUN` for the final real-systems acceptance.

> **Caution — fixtures must script every CLI call a routine makes.** `FakeGh` and
> `FakeMurtaugh` return an empty-success result for any call no rule matches, so a
> missing fixture is *silently masked* rather than failing loudly. Script **all**
> of a routine's calls, including fallbacks: e.g. the review queue issues
> `gh search prs` **and** a per-repo `gh pr list` **and** a `gh pr view` fallback
> for list misses — stub all three, or an unintended fallback path can pass
> looking like the path you meant to test.

## Where tests and dependencies live

- **Runtime stays stdlib-only** (launchd-safe, Python 3.9). `behave` is a **dev
  dependency** — install it in a separate virtualenv, never on the deployed import
  path. See `../harness/requirements-dev.txt`.
- Per-routine tests live beside the routine:

  ```
  automations/<routine>/tests/features/
  ├── environment.py        # copied from ../harness/environment.py, adjusted
  ├── <behaviour>.feature   # the confirmed scenarios
  └── steps/
      ├── common.py         # re-exports the shared steps (see below)
      └── <routine>_steps.py  # routine-specific Given/When/Then
                              # (NOT <routine>.py — that name shadows
                              #  `import <routine>` inside the step module)
  ```

- Run with: `behave automations/<routine>/tests/features`

## Use the shared harness — don't reinvent fakes

The blueprint ships a harness at `../harness/` so each customisation composes
instead of rebuilding mocking:

- `fakes.py` — `FakeMurtaugh` (Slack-aware fake `murtaugh` runner: understands
  `slack send-msg`/`update-msg`/`fetch-reactions`, records them structurally,
  returns the `--json` payload, and can simulate failures / dropped timestamps),
  `FakeGh`/`FakeRunner` (scriptable command runner), `FakeClock`. No
  machine-specific imports.
- `murtaugh_bdd_steps.py` — reusable `Given/Then` steps (`a fake murtaugh CLI`,
  `a fake Slack workspace`, `no Slack message is posted`, `a Slack message is
  posted to "…"`, etc.).
- `environment.py` — a template that puts the routine, `automations/`, and the
  harness on `sys.path` and resets fakes between scenarios.

Routine `steps/common.py` is just `from murtaugh_bdd_steps import *`; everything
specific to that routine goes in its own steps module. **Name that module
`<routine>_steps.py`, not `<routine>.py`** — behave imports step files by their
bare module name, so a `review_queue.py` step file shadows `import review_queue`
and your `When` step can't load the routine. See `../harness/README.md`
for the wiring and a worked example.

## Worked sketch

Confirmed scenario:

```gherkin
Scenario: A green PR waiting on a human tags the reviewer once
  Given a fake Slack workspace
  And a fake gh CLI returning one open PR with all checks green
  When the review queue runs
  Then a Slack message is posted to "#nc-code-reviews"
  And a threaded reply is posted
```

Step (routine-specific) drives the injected seam with the fake CLI runners:

```python
@when('the review queue runs')
def step_run(context):
    import review_queue
    context.rc = review_queue.main([], murtaugh=context.murtaugh, gh=context.gh,
                                   state_file=context.state_file, dry_run=False)
```

The shared `Then a Slack message is posted to "…"` step asserts against
`context.murtaugh.sent`. No network, no subprocess, deterministic, fast.
