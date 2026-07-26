#!/usr/bin/env python3
"""MCP tool coverage gate.

Every tool the deployed MCP server advertises must be exercised by the e2e-prod
suite, or the gate fails — so a tool that ships without a test is caught the same
way an unexercised /v1 operation is caught by coverage_gate.py.

This is the MCP analogue of coverage_gate.py, with one deliberate difference:

  coverage_gate.py takes its denominator from a REPO FILE (api/openapi.yaml,
  which is itself drift-gated against the server).

  This gate takes its denominator from the DEPLOYED SERVER — whatever it
  advertised on `tools/list` during the run, captured by harness/mcp-coverage.ts.

There is no drift-gated catalog file for MCP tools, so grepping
mcp/src/tools/*.ts for registerTool() would measure the repo rather than the
thing under test, and would go quietly wrong the moment a tool is registered
conditionally or gated behind a tier. The server's own advertisement is the only
honest catalog: what a real MCP client can see is exactly what we are
accountable for testing.

Inputs:
  - reports/mcp-coverage/*.json : shards written by harness/mcp-coverage.ts, each
    {"advertised": [...tool names...], "covered": [...tool names...]}.

"Covered" means a `tools/call` returned a result with isError !== true, i.e. the
tool actually ran — not merely that it was probed with a rejected request.

Usage: python3 mcp_coverage_gate.py [--reports DIR]
Exit 0 = every advertised tool covered (or allowlisted); 1 = coverage gap;
2 = usage/IO error (including "no shards", which must never read as a pass).
"""
import argparse
import glob
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# Tools the black-box suite intentionally does NOT exercise, with the reason.
# Keep this SHORT and justified — every entry is coverage we knowingly forgo.
#
# Each of these three is, per mcp/src/tools/legacy.ts, literally titled
# "Deprecated alias: <canonical>" and routes straight to that canonical tool,
# which IS covered elsewhere in this suite. Allowlisting them loses no real
# signal — the underlying behavior is exercised via the canonical tool — and
# we deliberately do not want to commit to happy-path testing of deprecated
# surface. Their continued *advertisement* is still asserted by
# suites/08-mcp.test.ts, so removal of the alias itself would still be caught.
ALLOWLIST: dict[str, str] = {
    "send_email": "deprecated alias for send_message (covered)",
    "approve_pending_message": "deprecated alias for approve_review (covered)",
    "reject_pending_message": "deprecated alias for reject_review (covered)",
}


def load_shards(reports_dir: str) -> tuple[set[str], set[str], int]:
    advertised: set[str] = set()
    covered: set[str] = set()
    paths = sorted(glob.glob(os.path.join(reports_dir, "*.json")))
    for path in paths:
        with open(path, encoding="utf-8") as fh:
            shard = json.load(fh)
        # Tolerate a bare-list shard shape defensively, but the recorder always
        # writes the object form.
        if isinstance(shard, dict):
            advertised.update(shard.get("advertised") or [])
            covered.update(shard.get("covered") or [])
    return advertised, covered, len(paths)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--reports", default=os.path.join(HERE, "reports", "mcp-coverage"))
    args = ap.parse_args()

    if not os.path.isdir(args.reports):
        print(
            f"mcp_coverage_gate: no shard directory at {args.reports}. "
            "Run the suite first (`npm test`); an absent run is not a pass.",
            file=sys.stderr,
        )
        return 2

    advertised, covered, shard_count = load_shards(args.reports)

    if shard_count == 0 or not advertised:
        # Without a denominator there is nothing to verify. Failing loudly here is
        # the whole point: a silent 0-of-0 "pass" would hide a broken harness.
        print(
            "mcp_coverage_gate: no tools/list denominator was recorded. "
            "At least one suite must call tools/list against the deployed server.",
            file=sys.stderr,
        )
        return 2

    # An allowlist entry that no longer names an advertised tool is a silent hole
    # (e.g. the tool was renamed) — fail loudly so the allowlist can't drift stale.
    stale = set(ALLOWLIST) - advertised
    if stale:
        print(
            f"mcp_coverage_gate: allowlist entries the server does not advertise "
            f"(renamed/removed?): {sorted(stale)}",
            file=sys.stderr,
        )
        return 2

    # A covered tool the server never advertised means the recorder and the
    # catalog disagree — report it, but it cannot mask a real gap.
    unadvertised = sorted(covered - advertised)

    missing = advertised - covered
    allowlisted = missing & set(ALLOWLIST)
    uncovered = sorted(missing - set(ALLOWLIST))

    print(f"Shards             : {shard_count}")
    print(f"Advertised tools   : {len(advertised)}")
    print(f"Covered            : {len(covered & advertised)}")
    print(f"Allowlisted        : {len(allowlisted)} " + (str(sorted(allowlisted)) if allowlisted else ""))
    if unadvertised:
        print(f"Called but not advertised: {unadvertised}")

    if uncovered:
        print(f"\nUNCOVERED ({len(uncovered)}):", file=sys.stderr)
        for name in uncovered:
            print(f"  {name}", file=sys.stderr)
        print(
            "\nEvery advertised MCP tool needs a suite that calls it successfully. "
            "Add a test, or add an ALLOWLIST entry with a justification.",
            file=sys.stderr,
        )
        return 1

    print("\nPASS: every advertised MCP tool is exercised.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
