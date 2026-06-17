"""In-memory fakes for BDD/behave tests of Murtaugh automations.

Murtaugh customisations reach Slack (and GitHub) by shelling out to the
``murtaugh`` and ``gh`` CLIs, injected into the routine as *runner callables*
(see ``reference/bdd.md`` → Testability rules). These fakes stand in for those
runners so ``behave`` can exercise a routine's real logic with **no network and
no subprocesses** — and with nothing machine-specific to import.

Stdlib only. Dev/test module — never import it from runtime automation code.
"""

from __future__ import annotations

import itertools
import json
from datetime import timedelta
from typing import Any, Dict, List


class _Result:
    """Minimal ``subprocess.CompletedProcess`` look-alike."""

    def __init__(self, returncode=0, stdout="", stderr=""):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


def _flags(args) -> Dict[str, Any]:
    """Parse ``--flag value`` pairs from an argv list into a dict (best-effort).

    argv is already tokenised (routines build a list, not a shell string), so a
    value with spaces is a single element.
    """
    out: Dict[str, Any] = {}
    i = 0
    while i < len(args):
        a = args[i]
        if isinstance(a, str) and a.startswith("--"):
            key = a[2:]
            if i + 1 < len(args) and not str(args[i + 1]).startswith("--"):
                out[key] = args[i + 1]
                i += 2
            else:
                out[key] = True
                i += 1
        else:
            i += 1
    return out


def _resolve_channel(to: str) -> str:
    """Mimic the gateway: ``#name``/``@user`` resolve to a stable fake ID; a raw
    ``C``/``G``/``D`` ID passes through."""
    if to and to[0] in "#@":
        slug = "".join(c for c in to[1:] if c.isalnum()).upper()[:8]
        return "C" + (slug or "FAKE")
    return to


class FakeRunner:
    """Scriptable stand-in for an external command runner (e.g. ``gh``).

    Register responses, then call it like the production runner:
    ``runner(args, check=False) -> result`` with ``.returncode/.stdout/.stderr``.
    JSON endpoints are consumed by the routine as ``json.loads(result.stdout)``.

    Matching: a ``str`` matches as a substring of the joined argv; a
    ``list``/``tuple`` matches as an argv prefix. First matching rule wins; an
    unmatched call returns empty success.
    """

    def __init__(self):
        self._rules = []  # (matcher, _Result)
        self.calls: List[List[str]] = []

    def add(self, match, *, stdout="", returncode=0, stderr=""):
        self._rules.append((match, _Result(returncode, stdout, stderr)))
        return self

    def add_json(self, match, payload):
        return self.add(match, stdout=json.dumps(payload))

    def __call__(self, args, check=False):
        args = list(args)
        self.calls.append(args)
        joined = " ".join(str(a) for a in args)
        for match, result in self._rules:
            if self._matches(match, args, joined):
                if check and result.returncode != 0:
                    raise RuntimeError("fake command failed (rc=%d): %s" % (result.returncode, joined))
                return result
        return _Result(0, "", "")

    @staticmethod
    def _matches(match, args, joined):
        if isinstance(match, (list, tuple)):
            return args[:len(match)] == list(match)
        return match in joined


# `gh` is just a generic runner.
FakeGh = FakeRunner


class FakeMurtaugh(FakeRunner):
    """Slack-aware stand-in for the ``murtaugh`` CLI runner.

    Recognises the Slack subcommands automations use and (a) records them in a
    structured form for easy assertions, (b) returns the ``--json`` payload the
    real CLI would (``{ok, channel, ts}`` etc.). Any other ``murtaugh …`` call
    falls back to ``FakeRunner`` scripting (use ``.add`` / ``.add_json``).

    Failure simulation:
      * ``fail_send`` / ``fail_update`` → the call returns rc=1 (so ``check=True``
        raises and ``check=False`` lets the routine observe a non-zero result).
      * ``return_no_ts`` → ``send-msg`` JSON omits ``ts`` (to exercise fail-loud
        paths).
      * ``reactions`` → ``{channel: [message, ...]}`` (or a callable) returned by
        ``slack fetch-reactions``.
    """

    def __init__(self, *, reactions=None, fail_send=False, fail_update=False,
                 return_no_ts=False, ts_start=1_700_000_000):
        super().__init__()
        self.sent: List[Dict[str, Any]] = []
        self.updated: List[Dict[str, Any]] = []
        self.reaction_calls: List[Dict[str, Any]] = []
        self._reactions = reactions or {}
        self.fail_send = fail_send
        self.fail_update = fail_update
        self.return_no_ts = return_no_ts
        self._ts = itertools.count(ts_start)

    def _next_ts(self) -> str:
        return "%d.000000" % next(self._ts)

    @staticmethod
    def _slack_subcmd(args):
        """Find the slack subcommand regardless of leading global flags
        (e.g. ``--config <path> slack fetch-reactions …``)."""
        for i, a in enumerate(args):
            if a == "slack" and i + 1 < len(args):
                return args[i + 1]
        return None

    def __call__(self, args, check=False):
        args = list(args)
        sub = self._slack_subcmd(args)
        if sub == "send-msg":
            return self._send(args, check)
        if sub == "update-msg":
            return self._update(args, check)
        if sub == "fetch-reactions":
            return self._fetch_reactions(args)
        return super().__call__(args, check)

    def _send(self, args, check):
        self.calls.append(args)
        if self.fail_send:
            if check:
                raise RuntimeError("fake murtaugh slack send-msg failed")
            return _Result(1, "", "fake send failure")
        f = _flags(args)
        to = f.get("to", "")
        channel = _resolve_channel(to)
        ts = None if self.return_no_ts else self._next_ts()
        self.sent.append({
            "to": to, "channel": channel, "body": f.get("body"),
            "blocks": f.get("blocks"), "thread": f.get("thread"), "ts": ts,
        })
        payload = {"ok": True, "channel": channel, "to": to}
        if ts is not None:
            payload["ts"] = ts
        return _Result(0, json.dumps(payload), "")

    def _update(self, args, check):
        self.calls.append(args)
        if self.fail_update:
            if check:
                raise RuntimeError("fake murtaugh slack update-msg failed")
            return _Result(1, "", "fake update failure")
        f = _flags(args)
        self.updated.append({
            "channel": f.get("channel"), "ts": f.get("ts"),
            "body": f.get("body"), "blocks": f.get("blocks"),
        })
        return _Result(0, json.dumps({"ok": True, "channel": f.get("channel"), "ts": f.get("ts")}), "")

    def _fetch_reactions(self, args):
        self.calls.append(args)
        f = _flags(args)
        channel = f.get("channel", "")
        self.reaction_calls.append({
            "from": f.get("from"), "emoji": f.get("emoji"),
            "channel": channel, "since": f.get("since"),
        })
        msgs = self._reactions.get(channel, [])
        if callable(msgs):
            msgs = msgs(channel)
        return _Result(0, json.dumps({"channel": _resolve_channel(channel), "messages": list(msgs)}), "")

    # -- test conveniences ---------------------------------------------------
    @property
    def last_sent(self):
        return self.sent[-1] if self.sent else None

    def sent_to(self, dest):
        """All posts whose ``--to`` or resolved channel equals ``dest``."""
        return [m for m in self.sent if dest in (m["to"], m["channel"])]

    def threaded(self):
        """Posts that were threaded replies (carry a ``--thread`` ts)."""
        return [m for m in self.sent if m["thread"]]


class FakeClock:
    """Frozen, advanceable clock for time-dependent logic.

    Construct with a ``datetime``; ``now()`` returns it. ``advance(**timedelta)``
    moves it forward.
    """

    def __init__(self, now):
        self._now = now

    def now(self, tz=None):
        return self._now

    def advance(self, **delta):
        self._now = self._now + timedelta(**delta)
        return self._now
