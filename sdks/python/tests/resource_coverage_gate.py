#!/usr/bin/env python3
"""Python SDK ergonomic-facade coverage gate.

``sdks/python/src/e2a/v1/client.py`` (+ ``inbound.py``) is a hand-written
facade over a generated layer that is itself mechanically derived from
``api/openapi.yaml`` and already covered transitively by the repo-root
``/v1`` coverage gate (``tests/e2e-prod/coverage_gate.py``). The FACADE is
where real bugs live — argument marshalling, response shaping, kwarg
handling, request-body coercion (``_coerce``), pagination wiring — and until
this gate existed it was almost untested: 6 methods exercised out of dozens.

This is the Python-SDK analogue of ``tests/e2e-prod/mcp_coverage_gate.py``,
with the same fail-closed discipline, and one deliberate difference in where
the denominator comes from:

  mcp_coverage_gate.py takes its denominator from shards the harness recorded
  from the DEPLOYED SERVER's own `tools/list` advertisement, because a
  server's tool catalog can vary by deployment/tier and there is no
  drift-gated catalog file for it.

  This gate takes its denominator from LIVE RUNTIME INTROSPECTION of
  ``AsyncE2AClient`` (``tests/coverage/discovery.py``), computed fresh every
  time the gate runs — not from a source grep, and not from a recorded shard.
  A Python class's public surface doesn't vary by deployment the way a
  server's advertised tool list can, so there's no need for the shard
  indirection on the denominator side; asking the live object graph directly
  is both simpler and more honest than grepping ``client.py``, which
  undercounts the moment a resource is nested (see ``discovery.py``'s
  docstring for the concrete miss this would otherwise cause: 57 vs the true
  62 methods).

SCOPE — async only. ``sync_client.py``'s ``E2AClient`` is a pure runtime
mirror of ``AsyncE2AClient`` (``__getattr__`` + ``_wrap_attr``/``_wrap_value``)
— there is exactly one implementation of resources / retries / error mapping
/ pagination, the async one, by design (see that module's docstring). A
marshalling bug lives in the async resource method; the sync facade can only
fail to *relay* it, and that relay mechanism (bridging, error transparency,
AutoPager -> SyncAutoPager, InboundResource wrapping, the async-context guard)
already has dedicated offline coverage in ``tests/test_v1_sync_client.py``,
including a structural parity test
(``test_parity_every_async_method_reachable_sync``) that fails if a new async
resource/method is ever unreachable from the sync facade. Re-running the same
live argument-marshalling assertions twice (once per facade) against staging
would double API/quota usage for zero incremental bug-catching power. This is
a scope call, not an oversight — see the PR description for the full
reasoning.

Inputs:
  - reports/resource-coverage/*.json : shards written by
    tests/coverage/tracker.py's mark_covered(), one per pytest process.

"Covered" means a test called the method, it returned without raising, AND
the test asserted something about the real result (see tracker.py — the
"only after a real assertion" half is a review-enforced convention, same
limit the MCP/API gates accept for their own "isError!==true" / "2xx" proxy).

Usage:
    python3 tests/resource_coverage_gate.py [--reports DIR]

Exit 0 = every discovered method covered (or allowlisted); 1 = coverage gap;
2 = usage/IO error (including "no shards", which must never read as a pass).
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)  # so `from resource_coverage_lib.discovery import ...` resolves when run as a script

# NB: this package is deliberately NOT named `coverage` — `coverage` is the
# pytest-cov dependency's own top-level module name, and sys.path.insert(0, HERE)
# would shadow it for the rest of the process (breaking --cov for anything
# imported after this gate module).
from resource_coverage_lib.discovery import discover_client_methods  # noqa: E402

# Methods the live suite intentionally does NOT exercise, with the reason.
# Keep this SHORT and justified — every entry is coverage we knowingly forgo.
ALLOWLIST: dict[str, str] = {
    "account.delete": (
        "destructive — the suite must never delete its own staging account "
        "(same reasoning as the /v1 gate's deleteAccount entry)"
    ),
    "account.suppressions.delete": (
        "no happy-path black-box: account-level suppressions "
        "(/v1/account/suppressions) have no create operation — a real entry is "
        "created only by a real SES bounce/complaint, so there is nothing to "
        "delete (identical to the /v1 gate's deleteSuppression entry). NOTE this "
        "is distinct from agent-scoped suppressions (agents.create_suppression /"
        " agents.delete_suppression), which DO have a direct create path and are "
        "fully covered — do not conflate the two when re-evaluating this entry."
    ),
    "listen": (
        "constructs a WSStream synchronously with zero network I/O and zero "
        "request-body/response-shape surface to marshal (the bug class this gate "
        "targets) — it just closes over api_key/agent_email/base_url. Real "
        "connection/reconnect/parse behavior is covered offline by "
        "tests/test_v1_websocket.py and the sync-bridge tests in "
        "tests/test_v1_sync_client.py. Wiring a live WS round-trip through the "
        "e2e-prod-style harness is a materially different piece of infra than "
        "the HTTP-response-shaping bugs this gate exists to catch."
    ),
}


def load_shards(reports_dir: str) -> tuple[set[str], int]:
    covered: set[str] = set()
    paths = sorted(glob.glob(os.path.join(reports_dir, "*.json")))
    for path in paths:
        with open(path, encoding="utf-8") as fh:
            shard = json.load(fh)
        if isinstance(shard, dict):
            covered.update(shard.get("covered") or [])
    return covered, len(paths)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--reports", default=os.path.join(HERE, "reports", "resource-coverage"))
    args = ap.parse_args()

    # The denominator: live introspection, computed now, not read from a shard.
    try:
        advertised = set(discover_client_methods().keys())
    except Exception as e:  # pragma: no cover - defensive
        print(f"resource_coverage_gate: failed to introspect AsyncE2AClient: {e!r}", file=sys.stderr)
        return 2

    if not advertised:
        print(
            "resource_coverage_gate: runtime introspection found zero methods on "
            "AsyncE2AClient — the discovery heuristic is broken, not the SDK. "
            "Refusing to report a vacuous pass.",
            file=sys.stderr,
        )
        return 2

    if not os.path.isdir(args.reports):
        print(
            f"resource_coverage_gate: no shard directory at {args.reports}. "
            "Run the live suite first (`pytest tests/test_e2e.py tests/test_e2e_resources.py`, "
            "with E2A_TEST_BASE_URL/E2A_TEST_API_KEY/E2A_TEST_AGENT_EMAIL set); "
            "an absent run is not a pass.",
            file=sys.stderr,
        )
        return 2

    covered, shard_count = load_shards(args.reports)

    if shard_count == 0:
        print(
            f"resource_coverage_gate: no shards in {args.reports} — did the live suite run?",
            file=sys.stderr,
        )
        return 2

    # An allowlist entry that no longer names a real method is a silent hole
    # (e.g. the method was renamed/removed) — fail loudly so it can't drift stale.
    stale = set(ALLOWLIST) - advertised
    if stale:
        print(
            f"resource_coverage_gate: allowlist entries not found by introspection "
            f"(renamed/removed?): {sorted(stale)}",
            file=sys.stderr,
        )
        return 2

    # A covered name introspection never found means the test and the discovery
    # heuristic disagree — surface it, but it can't mask a real gap.
    unadvertised = sorted(covered - advertised)

    missing = advertised - covered
    allowlisted = missing & set(ALLOWLIST)
    uncovered = sorted(missing - set(ALLOWLIST))

    print(f"Shards               : {shard_count}")
    print(f"Discovered methods   : {len(advertised)}  (AsyncE2AClient, recursive runtime introspection)")
    print(f"Covered              : {len(covered & advertised)}")
    print(f"Allowlisted          : {len(allowlisted)} " + (str(sorted(allowlisted)) if allowlisted else ""))
    if unadvertised:
        print(f"Recorded but not discovered (stale tracker call?): {unadvertised}")

    if uncovered:
        print(f"\nUNCOVERED ({len(uncovered)}):", file=sys.stderr)
        for name in uncovered:
            print(f"  {name}", file=sys.stderr)
        print(
            "\nEvery method AsyncE2AClient exposes needs a live test that calls it "
            "successfully and asserts on the result, or an ALLOWLIST entry with a "
            "justification.",
            file=sys.stderr,
        )
        return 1

    print("\nPASS: every ergonomic method AsyncE2AClient exposes is exercised (or explicitly allowlisted).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
