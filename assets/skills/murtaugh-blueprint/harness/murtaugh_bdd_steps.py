"""Reusable ``behave`` step definitions for Murtaugh automation tests.

Re-export these from a routine's steps package so they register with behave:

    # automations/<routine>/tests/features/steps/common.py
    from murtaugh_bdd_steps import *   # noqa: F401,F403

(``environment.py`` puts this harness on ``sys.path`` — see ``reference/bdd.md``
and ``README.md``.)

Slack is reached by shelling out to the ``murtaugh`` CLI, so the fake Slack
surface is ``context.murtaugh`` (a ``FakeMurtaugh``). These steps cover the
generic Slack assertions. Routine-specific Given/When/Then — especially the
``When the <routine> runs`` step that wires the fake runners into the routine's
injection seam — live alongside in the routine's own steps module.
"""

from __future__ import annotations

from behave import given, then  # type: ignore

from fakes import FakeGh, FakeMurtaugh


# --- Given: stand up the fake CLI runners on the scenario context -----------

@given('a fake murtaugh CLI')
def step_fake_murtaugh(context):
    context.murtaugh = FakeMurtaugh()


@given('a fake Slack workspace')  # Slack is reached via the murtaugh CLI
def step_fake_slack(context):
    context.murtaugh = FakeMurtaugh()


@given('a fake Slack workspace that drops message timestamps')
def step_fake_slack_no_ts(context):
    context.murtaugh = FakeMurtaugh(return_no_ts=True)


@given('a fake Slack workspace that fails to post')
def step_fake_slack_fail(context):
    context.murtaugh = FakeMurtaugh(fail_send=True)


@given('a fake gh CLI')
def step_fake_gh(context):
    context.gh = FakeGh()


# --- Then: assert on what the routine did via the fake murtaugh runner ------

@then('no Slack message is posted')
def step_no_post(context):
    assert not context.murtaugh.sent, \
        "expected no posts, got %d: %r" % (len(context.murtaugh.sent), context.murtaugh.sent)


@then('a Slack message is posted')
def step_one_post(context):
    assert context.murtaugh.sent, "expected at least one Slack post, got none"


@then('a Slack message is posted to "{dest}"')
def step_post_to(context, dest):
    assert context.murtaugh.sent_to(dest), \
        "no message posted to %s (posts: %r)" % (dest, [m["to"] for m in context.murtaugh.sent])


@then('{count:d} Slack messages are posted')
def step_n_posts(context, count):
    assert len(context.murtaugh.sent) == count, \
        "expected %d posts, got %d" % (count, len(context.murtaugh.sent))


@then('a message is updated in place')
def step_updated(context):
    assert context.murtaugh.updated, "expected an update-msg call, got none"


@then('no message is updated in place')
def step_not_updated(context):
    assert not context.murtaugh.updated, \
        "expected no updates, got %d" % len(context.murtaugh.updated)


@then('the last posted message body contains "{text}"')
def step_body_contains(context, text):
    last = context.murtaugh.last_sent
    assert last is not None, "no message posted"
    assert text in (last["body"] or ""), \
        "last body %r does not contain %r" % (last["body"], text)


@then('a threaded reply is posted')
def step_threaded(context):
    assert context.murtaugh.threaded(), "expected a threaded reply, got none"
