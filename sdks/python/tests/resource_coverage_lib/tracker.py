"""Records which ergonomic methods a live test run actually exercised.

The numerator half of the gate (see ``discovery.py`` for the denominator).
Mirrors ``tests/e2e-prod/harness/mcp-coverage.ts``'s shard-per-process design:
each pytest process writes its own ``reports/resource-coverage/<pid>.json``,
and the gate script (``tests/resource_coverage_gate.py``) unions every shard in
the directory. That indirection (write-to-file, read-back-in-a-separate-step)
is deliberate, not incidental complexity — it's what lets the gate be run as
an independent, re-runnable check after the test process has already exited
(`` `pytest ...` then `python tests/resource_coverage_gate.py` ``), exactly
like the MCP/API gates, instead of only being trustworthy from inside a
special pytest plugin hook.

"Covered" is asserted by the CALLER: ``mark_covered(name)`` must only be
called after the method call succeeded AND the test asserted something about
the real result — never merely "it didn't raise". This mirrors
``mcp-coverage.ts``'s ``recordToolCall``, which records a tool only when
``isError !== true``: a rejected/erroring call proves the surface exists, not
that it works. There's no way for this module to enforce the "asserted on the
result" half automatically (same limitation the MCP/API gates accept) — that
discipline is enforced by code review and by the `` `if not x: return` `` /
bare-skip ban called out in the task, not by the tracker itself.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Set

REPORTS_DIR = Path(__file__).resolve().parent.parent / "reports" / "resource-coverage"

_covered: Set[str] = set()


def mark_covered(name: str) -> None:
    """Record that ergonomic method ``name`` (a dotted path matching
    ``discovery.discover_client_methods``'s keys, e.g. ``"messages.send"``)
    was called and its result asserted on.

    Flushes synchronously (not on process exit) so a gate run in the SAME
    process right after the test session — or a crash/SIGKILL mid-run — never
    loses a recorded call: the shard on disk is always at least as complete as
    what has actually been asserted so far.
    """
    if name in _covered:
        return
    _covered.add(name)
    REPORTS_DIR.mkdir(parents=True, exist_ok=True)
    path = REPORTS_DIR / f"{os.getpid()}.json"
    path.write_text(json.dumps({"covered": sorted(_covered)}))


def clear_reports() -> None:
    """Wipe every shard in the reports directory.

    Called once at the start of a live test session (see ``tests/conftest.py``)
    so a stale shard from a previous partial/aborted run can never mask a
    method this run failed to cover — the same "pretest clears the directory"
    discipline ``mcp-coverage.ts`` documents for the same reason.
    """
    if not REPORTS_DIR.exists():
        return
    for shard in REPORTS_DIR.glob("*.json"):
        shard.unlink()
