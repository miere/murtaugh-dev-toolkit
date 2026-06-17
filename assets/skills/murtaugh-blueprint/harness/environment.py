"""behave environment for a Murtaugh routine's tests — TEMPLATE.

Copy this to ``automations/<routine>/tests/features/environment.py``. It puts the
routine, the ``automations/`` package root, and this murtaugh-blueprint harness on
``sys.path`` so tests can ``import <operation_module>``, ``from shared... import`` (your own helpers),
and ``from fakes import ...`` / ``from murtaugh_bdd_steps import *``.

Adjust ``_HARNESS`` if your deployment puts the skill somewhere other than the
default ``<config>/skills/murtaugh-blueprint/harness``.
"""

import os
import shutil
import sys
import tempfile

# .../automations/<routine>/tests/features/environment.py
_HERE = os.path.dirname(os.path.abspath(__file__))
_ROUTINE = os.path.abspath(os.path.join(_HERE, "..", ".."))          # automations/<routine>
_AUTOMATIONS = os.path.dirname(_ROUTINE)                              # automations/
_CONFIG_ROOT = os.path.dirname(_AUTOMATIONS)                         # ~/.config/murtaugh
_HARNESS = os.path.join(_CONFIG_ROOT, "skills", "murtaugh-blueprint", "harness")

for _p in (_HARNESS, _AUTOMATIONS, _ROUTINE):
    if _p not in sys.path:
        sys.path.insert(0, _p)


def snapshot_module_config(module, names):
    """Save ``module``'s attrs in ``names`` and return a ``restore()`` callable.

    A reset hook for **legacy** A-form automations that still read import-time
    globals (``STATE_FILE``, ``DRY_RUN``, …) and have no ``state_file``/``dry_run``
    seam. Snapshot before a scenario mutates them, restore after, so per-scenario
    overrides don't leak into the next scenario (scenario-order coupling). New
    automations should instead use the **B form** seam — pass ``state_file=`` /
    ``dry_run=`` to ``main()`` — and need none of this. See ``reference/bdd.md``.

        # in before_scenario:
        #     context._restore = snapshot_module_config(
        #         my_routine, ["STATE_FILE", "DRY_RUN"])
        # in after_scenario:
        #     if getattr(context, "_restore", None):
        #         context._restore()
    """
    saved = {n: getattr(module, n) for n in names}

    def restore():
        for n, v in saved.items():
            setattr(module, n, v)

    return restore


def before_scenario(context, scenario):
    # Cleared each scenario; the `Given a fake ...` steps populate them.
    context.murtaugh = None   # FakeMurtaugh — the Slack-via-CLI surface
    context.gh = None         # FakeGh / FakeRunner
    context.clock = None      # FakeClock

    # Fresh, isolated state location per scenario. B-form automations take this
    # via the seam: `main(..., state_file=context.state_file)`. No shared mutable
    # state, so scenarios can't leak state into each other regardless of order.
    context.state_dir = tempfile.mkdtemp(prefix="murtaugh-bdd-")
    context.state_file = os.path.join(context.state_dir, "state.json")

    # Legacy A-form only (delete if your routine has the seam): snapshot the
    # module globals a scenario will override, and restore them in after_scenario.
    #     context._restore = snapshot_module_config(my_routine, ["STATE_FILE", "DRY_RUN"])


def after_scenario(context, scenario):
    # Restore any legacy module globals snapshotted in before_scenario.
    restore = getattr(context, "_restore", None)
    if restore:
        restore()
    # Clean up the per-scenario temp state dir.
    state_dir = getattr(context, "state_dir", None)
    if state_dir and os.path.isdir(state_dir):
        shutil.rmtree(state_dir, ignore_errors=True)
