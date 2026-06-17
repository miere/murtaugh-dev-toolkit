# murtaugh-blueprint test harness

Reusable BDD test scaffolding for Murtaugh customisations, so each automation
*composes* tests instead of rebuilding mocking. Read `../reference/bdd.md` for the
confirm → enforce → loop method and the tiering policy; this README is the
mechanical wiring.

Automations reach Slack and GitHub by shelling out to the **`murtaugh`** and
**`gh`** CLIs (the tools every Murtaugh user has) — there is no per-machine Python
Slack client. Tests inject *fake runners* in their place.

> Prerequisite: the model assumes `murtaugh slack <tool> --json` returns
> structured output (`{ok, channel, ts}`). That is a Murtaugh-core capability.

## Contents

| File | What it is |
|---|---|
| `fakes.py` | `FakeMurtaugh` (Slack-aware fake `murtaugh` runner), `FakeGh`/`FakeRunner` (scriptable command runner), `FakeClock`. No network, no subprocess, no machine-specific imports. |
| `murtaugh_bdd_steps.py` | Reusable `behave` Given/Then steps (generic Slack assertions). |
| `environment.py` | Template `behave` environment — copy into a routine's `tests/features/`. |
| `requirements-dev.txt` | The dev-only `behave` dependency. |

## Prerequisite: a testability seam in the routine

Tests run the routine's *real* logic with fakes, so the routine must accept its
CLI runners (and clock) via keyword args; production defaults keep call sites
unchanged:

```python
import json, subprocess
from datetime import datetime

def _run_murtaugh(args, check=False):
    return subprocess.run(["murtaugh"] + list(args), capture_output=True, text=True, check=check)

def _run_gh(args, check=False):
    return subprocess.run(["gh"] + list(args), capture_output=True, text=True, check=check)

def main(argv=None, *, murtaugh=_run_murtaugh, gh=_run_gh, now=datetime.now,
         state_file=STATE_FILE, dry_run=DRY_RUN) -> int:
    store = load_state(state_file)
    res = murtaugh(["slack", "send-msg", "--to", chan, "--body", body, "--json"])
    posted = json.loads(res.stdout)   # {"ok","channel","ts"}
    if not dry_run:
        save_state(store, state_file)
    ...
```

Mutable config (`state_file`, `dry_run`) belongs in the seam too, each defaulting
to its module global, so tests vary it per call with no shared mutable state — see
`../reference/bdd.md` → *Testability rules* → *Mutable config goes in the seam too*.

## Install (separate venv — keep it off the runtime path)

```bash
python3 -m venv .venv-test
. .venv-test/bin/activate
pip install -r skills/murtaugh-blueprint/harness/requirements-dev.txt
```

## Wire a routine's tests

```
automations/<routine>/tests/features/
├── environment.py        # copy of harness/environment.py (adjust _HARNESS if needed)
├── <behaviour>.feature   # the confirmed scenarios
└── steps/
    ├── common.py         # from murtaugh_bdd_steps import *   # noqa: F401,F403
    └── <routine>_steps.py  # routine-specific Given/When/Then
```

> Name the routine step file `<routine>_steps.py`, **not** `<routine>.py`: behave
> imports step modules by their bare name, so `review_queue.py` would shadow
> `import review_queue` and your `When` step couldn't load the routine.

`environment.py` puts this harness, `automations/`, and the routine on `sys.path`,
so `steps/common.py` re-exports the shared steps and routine steps can
`from fakes import FakeMurtaugh, FakeGh`.

## Worked example

`automations/pull_request/tests/features/review_queue.feature`:

```gherkin
Feature: Mirror the GitHub review queue into Slack
  Scenario: A green PR waiting on a human is posted and the reviewer is tagged
    Given a fake Slack workspace
    And a fake gh CLI returning one open green PR
    When the review queue runs
    Then a Slack message is posted to "#nc-code-reviews"
    And a threaded reply is posted
```

`automations/pull_request/tests/features/steps/common.py`:

```python
from murtaugh_bdd_steps import *  # noqa: F401,F403  (shared Slack steps)
```

`automations/pull_request/tests/features/steps/review_queue_steps.py`:

```python
from behave import given, when


@given('a fake gh CLI returning one open green PR')
def step_one_green_pr(context):
    context.gh.add_json(["search", "prs"], [
        {"number": 42, "repository": {"nameWithOwner": "acme/web"}},
    ])
    context.gh.add_json(["pr", "list"], [{
        "number": 42, "title": "Add widget", "url": "https://github.com/acme/web/pull/42",
        "author": {"login": "alice", "url": "https://github.com/alice"},
        "isDraft": False,
        "reviewRequests": [{"login": "miere"}],
        "statusCheckRollup": [{"__typename": "CheckRun", "status": "COMPLETED", "conclusion": "SUCCESS"}],
        "reviewDecision": "", "state": "OPEN",
    }])


@when('the review queue runs')
def step_run(context):
    import review_queue
    # B-form seam: per-scenario temp state path + explicit dry_run, no globals.
    context.rc = review_queue.main([], murtaugh=context.murtaugh, gh=context.gh,
                                   state_file=context.state_file, dry_run=False)
```

`context.state_file` is a fresh temp path the harness `environment.py` provides
per scenario, so state never leaks between scenarios.
`context.murtaugh` (a `FakeMurtaugh`) records the routine's `slack send-msg`/
`update-msg` calls and answers them with the `--json` payload; the shared
`Then a Slack message is posted to "…"` step asserts against `context.murtaugh.sent`.

Run it:

```bash
behave automations/pull_request/tests/features
```

> Write new automations with the injection seam (`murtaugh`, `gh`, `now`) from
> the start — see `../reference/bdd.md`. Slack/GitHub I/O goes through those CLI
> runners; there is no Python Slack client to import. For a legacy automation
> that predates the seam, `unittest.mock.patch` its module-level runner helpers
> as a stopgap until you add the keyword args.
