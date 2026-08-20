"""Denominator test for the dot-segment path-collapse guard (e2a#792).

The other tests in ``test_v1_client_side_validation.py`` pin the guard at
specific call sites, found by hand: first the issue's own DELETE-route
sweep, then two misses a maintainer review caught on 2026-08-19, then one
more found by re-running the same sweep against every HTTP method. A
hand-found list like that is bounded by what the reader thought to check,
so a fourth miss is exactly as plausible as the third one was.

This test instead derives the complete denominator mechanically, straight
from ``api/openapi.yaml``, every run:

  For every path parameter on every operation, compute what path the URL
  collapses to when that parameter is set to ".." (the value itself and its
  immediately preceding literal/param segment are removed, the same rule
  ``new URL()`` / ``httpx.URL`` apply). If the collapsed path is a
  *different* existing route that also defines a handler for the *same*
  HTTP method, a client sending that value sends a real, structurally valid
  request to the wrong resource under the wrong intent: that is the shape
  of the bug the issue and both review blockers describe.

Every (route, method, param) the sweep finds must appear in exactly one of
GUARDED (an ergonomic call that reproduces the bug pre-guard, expected to
raise ``unsafe_path_segment`` with no request sent) or ALLOWLIST (a reason
the collapse cannot be reached, or need not be guarded, at the ergonomic
layer today). ``test_sweep_fully_classified`` fails the moment
``api/openapi.yaml`` adds a route this file has not yet triaged, so it is a
gate against the next one of these, not just a record of the ones already
found.
"""

from __future__ import annotations

from pathlib import Path
from typing import Callable, NamedTuple

import pytest
import yaml

from e2a.v1 import AsyncE2AClient
from e2a.v1.errors import E2AValidationError

BASE = "http://test.local"

# tests/test_dot_segment_enumeration.py -> sdks/python/tests/ -> sdks/python/ -> sdks/ -> repo root
OPENAPI_PATH = Path(__file__).resolve().parents[3] / "api" / "openapi.yaml"

_METHODS = ("get", "post", "put", "patch", "delete")


class Collapse(NamedTuple):
    origin_path: str
    method: str
    param: str
    target_path: str


def _segments(path: str) -> list[str]:
    return [s for s in path.strip("/").split("/") if s]


def _collapse_segments(segments: list[str], idx: int) -> list[str]:
    # A ".." value at position idx removes itself and the segment directly
    # before it (or just itself, at position 0, where there is nothing before it).
    return segments[: idx - 1] + segments[idx + 1 :] if idx > 0 else segments[idx + 1 :]


def _route_table(paths: dict) -> dict[tuple[str, ...], str]:
    return {tuple(_segments(p)): p for p in paths}


def _matches(route_table: dict[tuple[str, ...], str], collapsed: list[str]) -> list[str]:
    out = []
    for route_segs, route_path in route_table.items():
        if len(route_segs) != len(collapsed):
            continue
        if all(rs.startswith("{") or rs == cs for rs, cs in zip(route_segs, collapsed)):
            out.append(route_path)
    return out


def compute_dangerous_collapses() -> list[Collapse]:
    with open(OPENAPI_PATH) as f:
        doc = yaml.safe_load(f)
    paths = doc["paths"]
    route_table = _route_table(paths)
    found: list[Collapse] = []
    for path, methods in paths.items():
        segs = _segments(path)
        param_positions = [i for i, s in enumerate(segs) if s.startswith("{") and s.endswith("}")]
        for method, mdef in methods.items():
            if method not in _METHODS:
                continue
            for i in param_positions:
                collapsed = _collapse_segments(segs, i)
                for target in _matches(route_table, collapsed):
                    if target == path:
                        continue
                    if method in paths[target]:
                        found.append(Collapse(path, method, segs[i], target))
    return found


DENOMINATOR = compute_dangerous_collapses()


# ── GUARDED: reproduces the pre-guard bug through the ergonomic client ──
# Each entry's key is (origin_path, method, param) exactly as the sweep
# above spells it; the value builds an AsyncE2AClient call that supplies
# ".." for that param and otherwise-ordinary values for everything else.

GUARDED: dict[tuple[str, str, str], Callable[[AsyncE2AClient], "object"]] = {
    ("/v1/account/api-keys/{id}", "delete", "{id}"):
        lambda c: c.account.api_keys.delete(".."),
    ("/v1/account/suppressions/{address}", "delete", "{address}"):
        lambda c: c.account.suppressions.delete(".."),
    ("/v1/agents/{email}/contacts/{address}", "delete", "{email}"):
        lambda c: c.contacts.delete_outreach("..", "recipient@example.net"),
    ("/v1/agents/{email}/contacts/{address}", "delete", "{address}"):
        lambda c: c.contacts.delete_outreach("bot@test.dev", ".."),
    ("/v1/agents/{email}/messages/{id}", "delete", "{id}"):
        lambda c: c.messages.delete("bot@test.dev", ".."),
    ("/v1/agents/{email}/messages/{id}", "patch", "{id}"):
        lambda c: c.messages.update_labels("bot@test.dev", "..", {"add_labels": ["x"]}),
    ("/v1/agents/{email}/messages/{id}/restore", "post", "{id}"):
        lambda c: c.messages.restore("bot@test.dev", ".."),
    ("/v1/agents/{email}/suppressions/{address}", "delete", "{address}"):
        lambda c: c.agents.delete_suppression("bot@test.dev", ".."),
}

# ── ALLOWLIST: sweep hits with no ergonomic-layer reproduction today ──
# Each reason must be independently checkable by grepping client.py: either
# the collapse's HTTP method is read-only (get), or no ergonomic method
# calls the generated operation that would carry the collapsing param.

ALLOWLIST: dict[tuple[str, str, str], str] = {
    ("/v1/agents/{email}/contacts", "get", "{email}"):
        "GET only (listEngagements -> listContacts); no state change to guard against",
    ("/v1/agents/{email}/contacts/{address}", "get", "{email}"):
        "GET only (getEngagement -> getContact); info-disclosure risk, not a destructive-action "
        "blocker, out of scope for this priority-1 pass, same as the WS builders",
    ("/v1/agents/{email}/contacts/{address}", "get", "{address}"):
        "GET only (getEngagement -> getAgent); same as above",
    ("/v1/agents/{email}/conversations/{id}", "get", "{id}"):
        "GET only (getConversation -> getAgent); same as above",
    ("/v1/agents/{email}/messages/{id}", "get", "{id}"):
        "GET only (getMessage -> getAgent); same as above",
    ("/v1/agents/{email}/messages/{id}/attachments/{index}", "get", "{index}"):
        "GET only (getAttachment -> getMessage); same as above",
    ("/v1/agents/{email}/metrics", "get", "{email}"):
        "GET only (getAgentMetrics -> getAccountMetrics); same as above",
}


@pytest.mark.parametrize("collapse", DENOMINATOR, ids=lambda c: f"{c.method.upper()} {c.origin_path} [{c.param}]")
def test_sweep_fully_classified(collapse: Collapse):
    key = (collapse.origin_path, collapse.method, collapse.param)
    assert key in GUARDED or key in ALLOWLIST, (
        f"{key} collapses onto {collapse.target_path} (same method, real route) and is "
        "classified in neither GUARDED nor ALLOWLIST: api/openapi.yaml grew a new "
        "dot-segment collapse onto a same-method route; triage it (guard the ergonomic "
        "call site, or allowlist it with a reason) before this can pass"
    )


@pytest.mark.anyio
@pytest.mark.parametrize("key", list(GUARDED.keys()), ids=lambda k: f"{k[1].upper()} {k[0]} [{k[2]}]")
async def test_guarded_collapse_rejected_before_any_request(httpx_mock, key):
    call = GUARDED[key]
    async with AsyncE2AClient(api_key="e2a_test", base_url=BASE) as c:
        with pytest.raises(E2AValidationError) as ei:
            await call(c)
    assert ei.value.code == "unsafe_path_segment"
    assert httpx_mock.get_requests() == []


def test_allowlist_entries_are_still_in_the_denominator():
    # An allowlist reason for a collapse the spec no longer produces (route
    # renamed/removed) is a stale entry hiding a shrunk sweep; catch it
    # explicitly rather than letting it silently stop mattering.
    live = {(c.origin_path, c.method, c.param) for c in DENOMINATOR}
    stale = set(ALLOWLIST) - live
    assert not stale, f"stale allowlist entries no longer produced by the sweep: {stale}"


def test_denominator_is_nonempty():
    # A parse failure or an empty openapi.yaml would make every test above
    # vacuously pass; assert the sweep actually found the routes we know
    # are there.
    assert len(DENOMINATOR) >= len(GUARDED)
