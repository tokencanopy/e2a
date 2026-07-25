#!/usr/bin/env python3
"""Conformance coverage gate.

Every OpenAPI operation must be exercised by the e2e-prod suite, or the gate
fails — so if a new operationId is added to api/openapi.yaml (the drift-gated
SSOT) and no suite calls it, CI catches the coverage regression.

Inputs:
  - reports/coverage-*.json : shards written by the harness (harness/coverage.ts),
    each a JSON array of "METHOD /concrete/path" strings the ApiClient issued.
  - api/openapi.yaml        : the operation catalog (method + path template +
    operationId).

Maps each exercised concrete path to the most-specific matching operationId, then
compares the covered set to the full catalog (minus an explicit allowlist of
operations the black-box suite legitimately must not call).

SCOPE: this gate covers the typed /v1 OpenAPI operations (the operationId
catalog, counted dynamically from api/openapi.yaml). Non-/v1 surface (billing, MCP, OAuth machine endpoints, /api/health,
/webhooks/*) and the webhook EVENT types are NOT operationIds and are out of scope
here — they have their own dedicated suites (e.g. 21-webhook-events for event
emission). Exercised paths that don't map to a /v1 operationId are reported (as
"unmapped") but do not affect the pass/fail verdict.

"Covered" means the suite issued a request to the operation that returned a 2xx
(the harness only records successes — see harness/coverage.ts), i.e. the operation
was actually exercised, not merely probed with a rejected request.

Usage: python3 coverage_gate.py [--openapi PATH] [--reports DIR] [--target-dir DIR]
Exit 0 = all covered (or allowlisted); 1 = coverage gap; 2 = usage/IO error.
"""
import argparse
import glob
import json
import os
import sys

from target_env import resolve_target

HERE = os.path.dirname(os.path.abspath(__file__))

# Operations the black-box conformance suite intentionally does NOT exercise, with
# the reason. Keep this list SHORT and justified — every entry is coverage we're
# knowingly forgoing. Allowlisted no matter what the run targeted.
ALWAYS_ALLOWLIST = {
    "deleteAccount": "destructive — the suite must never delete its own account",
}

# Allowlisted ONLY on a non-production run (see target_env.py for the
# mechanism) — REQUIRED once the run's target shards show production.
STAGING_ONLY_ALLOWLIST = {
    "deleteSuppression": "no happy path black-box on staging: a suppression is created "
    "only by a real SES bounce/complaint (no createSuppression API), and staging's "
    "e2a-staging-smtp IAM policy denies ses:SendRawEmail to the bounce/complaint "
    "mailbox-simulator addresses, so there's nothing to delete there; the "
    "404-unknown-address path is exercised by 19-account. Production has no such "
    "block — suites/prod/31-ses-feedback.test.ts creates a real account suppression "
    "via a real bounce/complaint and exercises this happy path.",
}


def load_ops(openapi_path):
    try:
        import yaml
    except ImportError:
        print("coverage_gate: PyYAML required (pip install pyyaml)", file=sys.stderr)
        sys.exit(2)
    with open(openapi_path) as f:
        spec = yaml.safe_load(f)
    ops = []  # (METHOD, [template segments], operationId)
    for path, item in (spec.get("paths") or {}).items():
        for method, op in (item or {}).items():
            if isinstance(op, dict) and "operationId" in op:
                ops.append((method.upper(), path.strip("/").split("/"), op["operationId"]))
    return ops


def match_op(method, segments, ops):
    """Most-specific (most literal segments) operationId matching a concrete path.

    Raises on an ambiguous match (two different operations tie at the same literal
    count) rather than silently picking one by dict order — that would let a path
    over-attribute coverage to the wrong op. Doesn't happen with today's spec, but
    a future literal-sibling of a {param} route would trigger it."""
    best, best_literals, tied = None, -1, set()
    for m, tsegs, opid in ops:
        if m != method or len(tsegs) != len(segments):
            continue
        literals, ok = 0, True
        for t, s in zip(tsegs, segments):
            if t.startswith("{") and t.endswith("}"):
                if not s:
                    ok = False
                    break
            elif t != s:
                ok = False
                break
            else:
                literals += 1
        if not ok:
            continue
        if literals > best_literals:
            best, best_literals, tied = opid, literals, {opid}
        elif literals == best_literals:
            tied.add(opid)
    if len(tied) > 1:
        raise ValueError(
            f"ambiguous match for {method} /{'/'.join(segments)}: {sorted(tied)} tie at {best_literals} literal segments"
        )
    return best


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--openapi", default=os.path.join(HERE, "../../api/openapi.yaml"))
    ap.add_argument("--reports", default=os.path.join(HERE, "reports", "coverage"))
    ap.add_argument("--target-dir", default=os.path.join(HERE, "reports", "target"))
    args = ap.parse_args()

    if not os.path.exists(args.openapi):
        print(f"coverage_gate: openapi spec not found: {args.openapi}", file=sys.stderr)
        return 2

    ops = load_ops(args.openapi)
    all_ids = {opid for _, _, opid in ops}

    # An allowlist entry that no longer matches a real operationId is a silent hole
    # (e.g. the op was renamed) — fail loudly so either tier can't drift stale.
    stale = (set(ALWAYS_ALLOWLIST) | set(STAGING_ONLY_ALLOWLIST)) - all_ids
    if stale:
        print(f"coverage_gate: allowlist entries not in the spec (renamed/removed?): {sorted(stale)}", file=sys.stderr)
        return 2

    try:
        targeted_prod, target_shard_count, hosts = resolve_target(args.target_dir)
    except ValueError as e:
        print(f"coverage_gate: {e}", file=sys.stderr)
        return 2

    allowlist = dict(ALWAYS_ALLOWLIST)
    if targeted_prod:
        env_label = "PRODUCTION"
    else:
        env_label = "non-production (staging/self-hosted)"
        allowlist.update(STAGING_ONLY_ALLOWLIST)

    shards = glob.glob(os.path.join(args.reports, "*.json"))
    if not shards:
        print(f"coverage_gate: no coverage shards in {args.reports} — did the suite run?", file=sys.stderr)
        return 2
    exercised = set()
    for shard in shards:
        with open(shard) as f:
            exercised.update(json.load(f))

    covered_ids, non_v1 = set(), set()
    for pair in exercised:
        method, _, path = pair.partition(" ")
        opid = match_op(method, path.strip("/").split("/"), ops)
        if opid:
            covered_ids.add(opid)
        else:
            non_v1.add(pair)

    missing = all_ids - covered_ids
    allowlisted = missing & set(allowlist)
    uncovered = missing - set(allowlist)

    print(f"Target environment : {env_label}  ({target_shard_count} target shard(s): {hosts})")
    print(f"OpenAPI operations : {len(all_ids)}  (/v1 operationId scope)")
    print(f"Covered (2xx)      : {len(covered_ids)}")
    print(f"Allowlisted        : {len(allowlisted)} " + (str(sorted(allowlisted)) if allowlisted else ""))
    print(f"Coverage shards    : {len(shards)}  (exercised 2xx pairs: {len(exercised)}, unmapped non-/v1: {len(non_v1)})")
    if non_v1:
        # Printed for visibility (a mistyped /v1 path would land here), but out of
        # scope for the gate — non-/v1 surface has its own suites.
        for pair in sorted(non_v1)[:20]:
            print(f"    non-/v1: {pair}")

    if uncovered:
        print(f"\nUNCOVERED ({len(uncovered)}):")
        for opid in sorted(uncovered):
            m, t, _ = next(o for o in ops if o[2] == opid)
            tier = " (staging-only allowlist, REQUIRED on this prod run)" if opid in STAGING_ONLY_ALLOWLIST else ""
            print(f"  - {opid:28s} {m} /{'/'.join(t)}{tier}")
        print("\nGATE: FAIL — the above operation(s) are in the spec but no suite exercises them for this target environment.")
        return 1

    print("\nGATE: PASS — every OpenAPI operation is exercised (or explicitly allowlisted).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
