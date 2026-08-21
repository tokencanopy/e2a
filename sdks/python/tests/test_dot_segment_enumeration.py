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
  collapses to under BOTH dot-segment values: ".." removes the value and
  its immediately preceding segment, "." removes just the value (with any
  resulting trailing slash dropped, as httpx and the TS URL builder both
  do). A collapsed segment that is still a path parameter carries an
  arbitrary caller-supplied value, so it matches ANY segment of a candidate
  route, literal or not, exactly as the router would. If the collapsed path
  is a *different* existing route that also defines a handler for the
  *same* HTTP method, a client sending that value sends a real,
  structurally valid request to the wrong resource under the wrong intent:
  that is the shape of the bug the issue and both review blockers describe.

Every MUTATING (non-GET) collapse the sweep finds must appear in exactly
one of GUARDED (an ergonomic call that reproduces the bug pre-guard,
expected to raise ``unsafe_path_segment`` with no request sent) or
ALLOWLIST (a reason the collapse cannot be reached, or need not be
guarded, at the ergonomic layer today). ``test_sweep_fully_classified``
fails the moment ``api/openapi.yaml`` adds a mutating collapse this file
has not yet triaged, so it is a gate against the next one of these, not
just a record of the ones already found.

GET-method collapses are triaged as one class, below the parametrized
test: misdirecting a read is an information-disclosure concern, and some
reads have side effects (fetching an unread message marks it read), but
neither is the destructive-action blocker this priority-1 pass guards, so
they are deliberately out of scope here, same as the WS builders.
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
_DOT_VALUES = ("..", ".")


class Collapse(NamedTuple):
    origin_path: str
    method: str
    param: str
    value: str
    target_path: str


def _segments(path: str) -> list[str]:
    return [s for s in path.strip("/").split("/") if s]


def _collapse_segments(segments: list[str], idx: int, value: str) -> list[str]:
    if value == "..":
        # ".." removes itself and the segment directly before it (or just
        # itself, at position 0, where there is nothing before it).
        return segments[: idx - 1] + segments[idx + 1 :] if idx > 0 else segments[idx + 1 :]
    # "." removes just itself; the trailing slash it can leave behind is
    # dropped by httpx (and by the TS builder when no query string follows).
    return segments[:idx] + segments[idx + 1 :]


def _is_param(seg: str) -> bool:
    return seg.startswith("{") and seg.endswith("}")


def _route_table(paths: dict) -> dict[tuple[str, ...], str]:
    return {tuple(_segments(p)): p for p in paths}


def _matches(route_table: dict[tuple[str, ...], str], collapsed: list[str]) -> list[str]:
    # A route-side "{param}" matches any collapsed segment, and a SURVIVING
    # collapsed-side "{param}" matches any route segment: it holds an
    # arbitrary caller-supplied value, which can equal a route literal
    # (e.g. set_outreach(".", "protection", ...) collapses onto
    # PUT /v1/agents/{email}/protection because address == "protection").
    out = []
    for route_segs, route_path in route_table.items():
        if len(route_segs) != len(collapsed):
            continue
        if all(_is_param(rs) or _is_param(cs) or rs == cs for rs, cs in zip(route_segs, collapsed)):
            out.append(route_path)
    return out


def compute_dangerous_collapses() -> list[Collapse]:
    with open(OPENAPI_PATH, encoding="utf-8") as f:
        doc = yaml.safe_load(f)
    paths = doc["paths"]
    route_table = _route_table(paths)
    found: list[Collapse] = []
    for path, methods in paths.items():
        segs = _segments(path)
        param_positions = [i for i, s in enumerate(segs) if _is_param(s)]
        for method, mdef in methods.items():
            if method not in _METHODS:
                continue
            for i in param_positions:
                for value in _DOT_VALUES:
                    collapsed = _collapse_segments(segs, i, value)
                    for target in _matches(route_table, collapsed):
                        if target == path:
                            continue
                        if method in paths[target]:
                            found.append(Collapse(path, method, segs[i], value, target))
    return found


DENOMINATOR = compute_dangerous_collapses()
MUTATING = [c for c in DENOMINATOR if c.method != "get"]


# ── GUARDED: reproduces the pre-guard bug through the ergonomic client ──
# Each entry's key is (origin_path, method, param, value) exactly as the
# sweep above spells it; the value builds an AsyncE2AClient call that
# supplies that dot-segment value for that param and otherwise-ordinary
# values for everything else.

GUARDED: dict[tuple[str, str, str, str], Callable[[AsyncE2AClient], "object"]] = {
    ("/v1/account/api-keys/{id}", "delete", "{id}", ".."):
        lambda c: c.account.api_keys.delete(".."),
    ("/v1/account/suppressions/{address}", "delete", "{address}", ".."):
        lambda c: c.account.suppressions.delete(".."),
    ("/v1/agents/{email}/contacts/{address}", "delete", "{email}", ".."):
        lambda c: c.contacts.delete_outreach("..", "recipient@example.net"),
    ("/v1/agents/{email}/contacts/{address}", "delete", "{address}", ".."):
        lambda c: c.contacts.delete_outreach("bot@test.dev", ".."),
    # "." in the email slot with address "protection" lands on
    # PUT /v1/agents/{email}/protection (putAgentProtection), a same-method
    # retarget only visible once surviving params match route literals.
    ("/v1/agents/{email}/contacts/{address}", "put", "{email}", "."):
        lambda c: c.contacts.set_outreach(".", "protection", {}),
    ("/v1/agents/{email}/messages/{id}", "delete", "{id}", ".."):
        lambda c: c.messages.delete("bot@test.dev", ".."),
    ("/v1/agents/{email}/messages/{id}", "patch", "{id}", ".."):
        lambda c: c.messages.update_labels("bot@test.dev", "..", {"add_labels": ["x"]}),
    ("/v1/agents/{email}/messages/{id}/restore", "post", "{id}", ".."):
        lambda c: c.messages.restore("bot@test.dev", ".."),
    ("/v1/agents/{email}/suppressions/{address}", "delete", "{address}", ".."):
        lambda c: c.agents.delete_suppression("bot@test.dev", ".."),
    # "." in the batch_id slot drops to /v1/contacts/imports, which chi
    # backtracks onto DELETE /v1/contacts/{address} (deleteContact) with
    # address "imports"; the confirm=DELETE this method already sends
    # satisfies deleteContact's own guard.
    ("/v1/contacts/imports/{batch_id}", "delete", "{batch_id}", "."):
        lambda c: c.contacts.delete_import("."),
}

# ── ALLOWLIST: mutating sweep hits with no ergonomic-layer reproduction ──
# Each reason must be independently checkable by grepping client.py: no
# ergonomic method calls the generated operation that would carry the
# collapsing param. Currently empty: every mutating collapse the sweep
# finds is reachable through an ergonomic method, so each one is guarded.

ALLOWLIST: dict[tuple[str, str, str, str], str] = {}


def _key(c: Collapse) -> tuple[str, str, str, str]:
    return (c.origin_path, c.method, c.param, c.value)


def _id(k: tuple[str, str, str, str]) -> str:
    return f"{k[1].upper()} {k[0]} [{k[2]}={k[3]}]"


@pytest.mark.parametrize("collapse", MUTATING, ids=lambda c: _id(_key(c)))
def test_sweep_fully_classified(collapse: Collapse):
    key = _key(collapse)
    assert key in GUARDED or key in ALLOWLIST, (
        f"{key} collapses onto {collapse.target_path} (same method, real route) and is "
        "classified in neither GUARDED nor ALLOWLIST: api/openapi.yaml grew a new "
        "dot-segment collapse onto a same-method mutating route; triage it (guard the "
        "ergonomic call site, or allowlist it with a reason) before this can pass"
    )


@pytest.mark.anyio
@pytest.mark.parametrize("key", list(GUARDED.keys()), ids=_id)
async def test_guarded_collapse_rejected_before_any_request(httpx_mock, key):
    call = GUARDED[key]
    async with AsyncE2AClient(api_key="e2a_test", base_url=BASE) as c:
        with pytest.raises(E2AValidationError) as ei:
            await call(c)
    assert ei.value.code == "unsafe_path_segment"
    assert httpx_mock.get_requests() == []


def test_classification_entries_are_still_in_the_denominator():
    # A GUARDED or ALLOWLIST key for a collapse the spec no longer produces
    # (route renamed/removed, or a typo in the key itself) is a stale entry
    # hiding a shrunk sweep; catch it explicitly rather than letting it
    # silently stop mattering.
    live = {_key(c) for c in MUTATING}
    stale = (set(GUARDED) | set(ALLOWLIST)) - live
    assert not stale, f"stale classification entries no longer produced by the sweep: {stale}"


def test_guarded_and_allowlist_are_disjoint():
    # One key in both dicts would mean two contradictory triages passing at
    # once; force every collapse to have exactly one.
    overlap = set(GUARDED) & set(ALLOWLIST)
    assert not overlap, f"keys triaged as both GUARDED and ALLOWLIST: {overlap}"


def test_denominator_is_nonempty():
    # A parse failure or an empty openapi.yaml would make every test above
    # vacuously pass; assert the sweep actually found the routes we know
    # are there.
    assert len(MUTATING) >= len(GUARDED)
