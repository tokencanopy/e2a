#!/usr/bin/env python3
"""Response-schema conformance gate.

Every response body the e2e-prod suite received must validate against the
OpenAPI response schema for its (operation, status) — or the gate fails. This
closes the gap the other gates leave open: coverage_gate.py proves an operation
RAN, the suites assert hand-picked fields, but nothing proved the full body
SHAPE matches api/openapi.yaml (the drift-gated SSOT). A field the server stops
returning, a type that changes under a wrong status, or an error path that
returns a bare string instead of the envelope — none of that was caught.

The suite itself stays zero-dependency (node: builtins only — the ops pipeline
runs it with no `npm install`), so validation cannot happen in-process. The
split is: the SUITE RECORDS, this GATE VALIDATES.

Inputs:
  - reports/response-samples/*.json : shards written by harness/responses.ts,
    each a JSON array of {method, path, status, contentType, kind, body}
    samples — ALL statuses, not just 2xx (the per-op `default` response
    documents the error envelope, so every 401/404/422 the suites provoke is a
    free check on the error contract).
  - api/openapi.yaml : the operation catalog + response schemas.

Maps each sample to the most-specific matching operationId (same matcher as
coverage_gate.py), picks the response schema for its exact status (falling back
to the op's `default`), resolves $refs against components, and validates with
`jsonschema` (Draft 2020-12 — the spec declares openapi 3.1.0).

What counts as a violation:
  - a JSON body that fails its schema (missing required field, wrong type, …);
  - an empty or non-JSON body where the spec documents application/json
    content (in today's spec: every documented response);
  - a status the spec neither documents nor covers with a `default`.
Responses are deliberately open (`additionalProperties: true` on the response
schemas) — EXTRA fields are not violations, and `format` stays annotation-only
(per the 2020-12 default), so no format false-positives.

What does not affect the verdict:
  - samples on non-/v1 paths (billing, OAuth, /api/health — out of the
    operationId scope, reported for visibility like coverage_gate.py);
  - `oversized` samples the recorder bounded (counted as skipped, never as
    passes).

Usage: python3 response_schema_gate.py [--openapi PATH] [--reports DIR]
Exit 0 = every sample valid (or allowlisted); 1 = violation(s); 2 = usage/IO
error (including "no shards" — an absent run must never read as a pass).
"""
import argparse
import glob
import json
import os
import sys
from collections import defaultdict

from coverage_gate import load_ops, match_op

HERE = os.path.dirname(os.path.abspath(__file__))

# (operationId, status) pairs whose violations we knowingly tolerate, keyed
# "operationId <status>", with the reason. Keep this SHORT and justified —
# every entry is a live spec/implementation mismatch we are choosing to ship.
ALLOWLIST: dict[str, str] = {}


def load_spec(openapi_path):
    try:
        import yaml
    except ImportError:
        print("response_schema_gate: PyYAML required (pip install pyyaml)", file=sys.stderr)
        sys.exit(2)
    with open(openapi_path) as f:
        return yaml.safe_load(f)


def response_entry(op, status):
    """The response object documented for `status`, or the op's `default`.

    Returns (entry, label) where label is the spec key that matched ("200",
    "default", …), or (None, None) when the spec has no answer for the status —
    itself a violation category, since a caller has nothing to code against."""
    responses = op.get("responses") or {}
    if str(status) in responses:
        return responses[str(status)], str(status)
    if "default" in responses:
        return responses["default"], "default"
    return None, None


def make_validator(schema, components, cache, cache_key):
    """A Draft 2020-12 validator for `schema`, with $refs resolvable.

    The spec's $refs are all document-internal ("#/components/schemas/…"), so
    wrapping the schema as {"allOf": [schema], "components": …} makes every ref
    resolve against the wrapped document itself — no external registry, and the
    stray "components" key is an unknown keyword the validator ignores."""
    import jsonschema

    if cache_key not in cache:
        wrapped = {"allOf": [schema], "components": components}
        cache[cache_key] = jsonschema.Draft202012Validator(wrapped)
    return cache[cache_key]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--openapi", default=os.path.join(HERE, "../../api/openapi.yaml"))
    ap.add_argument("--reports", default=os.path.join(HERE, "reports", "response-samples"))
    args = ap.parse_args()

    try:
        import jsonschema  # noqa: F401
    except ImportError:
        print("response_schema_gate: jsonschema required (pip install jsonschema)", file=sys.stderr)
        return 2

    if not os.path.exists(args.openapi):
        print(f"response_schema_gate: openapi spec not found: {args.openapi}", file=sys.stderr)
        return 2

    spec = load_spec(args.openapi)
    ops = load_ops(args.openapi)  # (METHOD, [template segments], operationId)
    all_ids = {opid for _, _, opid in ops}
    ops_by_id = {}
    for path, item in (spec.get("paths") or {}).items():
        for method, op in (item or {}).items():
            if isinstance(op, dict) and "operationId" in op:
                ops_by_id[op["operationId"]] = op
    components = spec.get("components") or {}

    # An allowlist entry naming an operationId the spec no longer has is a
    # silent hole (renamed/removed op) — fail loudly, same as the other gates.
    stale = {key for key in ALLOWLIST if key.split(" ")[0] not in all_ids}
    if stale:
        print(
            f"response_schema_gate: allowlist entries not in the spec (renamed/removed?): {sorted(stale)}",
            file=sys.stderr,
        )
        return 2

    shards = glob.glob(os.path.join(args.reports, "*.json"))
    if not shards:
        print(
            f"response_schema_gate: no response-sample shards in {args.reports} — did the suite run?",
            file=sys.stderr,
        )
        return 2
    samples = []
    for shard in shards:
        with open(shard) as f:
            samples.extend(json.load(f))

    validators = {}
    validated = 0
    skipped_oversized = 0
    non_v1 = set()
    ops_sampled = set()
    # (opid, status, message) → [count, example path]. Grouped so a violation
    # repeated across 40 list calls reads as one line, not 40.
    violations = defaultdict(lambda: [0, ""])

    def record_violation(opid, status, message, path):
        group = violations[(opid, status, message)]
        group[0] += 1
        group[1] = group[1] or path

    for s in samples:
        method, path, status = s["method"], s["path"], s["status"]
        try:
            opid = match_op(method, path.strip("/").split("/"), ops)
        except ValueError as e:
            print(f"response_schema_gate: {e}", file=sys.stderr)
            return 2
        if not opid:
            non_v1.add(f"{method} {path}")
            continue
        ops_sampled.add(opid)
        if s["kind"] == "oversized":
            skipped_oversized += 1
            continue
        entry, label = response_entry(ops_by_id[opid], status)
        if entry is None:
            record_violation(opid, status, "status not documented for this operation (no `default` either)", path)
            continue
        content = (entry.get("content") or {}).get("application/json")
        if content is None or "schema" not in content:
            # A response documented WITHOUT a JSON body: only an empty body conforms.
            if s["kind"] != "empty":
                record_violation(opid, status, f"spec documents no application/json content for {label}, got a body", path)
            else:
                validated += 1
            continue
        if s["kind"] == "empty":
            record_violation(opid, status, f"empty body where spec documents application/json ({label})", path)
            continue
        if s["kind"] == "nonjson":
            preview = (s.get("rawPrefix") or "").replace("\n", "\\n")[:80]
            record_violation(opid, status, f"non-JSON body where spec documents application/json ({label}): {preview!r}", path)
            continue
        validator = make_validator(content["schema"], components, validators, (opid, label))
        errors = sorted(validator.iter_errors(s.get("body")), key=lambda e: str(e.json_path))
        if errors:
            for e in errors:
                record_violation(opid, status, f"[{label} schema] {e.json_path}: {e.message[:200]}", path)
        else:
            validated += 1

    allowlisted_groups = {k: v for k, v in violations.items() if f"{k[0]} {k[1]}" in ALLOWLIST}
    failing_groups = {k: v for k, v in violations.items() if f"{k[0]} {k[1]}" not in ALLOWLIST}
    unused_allowlist = set(ALLOWLIST) - {f"{k[0]} {k[1]}" for k in allowlisted_groups}

    print(f"OpenAPI operations : {len(all_ids)}  (/v1 operationId scope)")
    print(f"Operations sampled : {len(ops_sampled)}")
    print(f"Samples            : {len(samples)}  ({len(shards)} shard(s); valid: {validated}, "
          f"oversized-skipped: {skipped_oversized}, unmapped non-/v1 paths: {len(non_v1)})")
    print(f"Allowlisted groups : {len(allowlisted_groups)} "
          + (str(sorted(f'{k[0]} {k[1]}' for k in allowlisted_groups)) if allowlisted_groups else ""))
    if unused_allowlist:
        # Not fatal (a partial run may simply not have hit the op), but visible —
        # an entry that never fires across full runs should be removed.
        print(f"Allowlist entries with no observed violation this run: {sorted(unused_allowlist)}")
    if non_v1:
        for pair in sorted(non_v1)[:10]:
            print(f"    non-/v1: {pair}")

    if failing_groups:
        print(f"\nVIOLATIONS ({len(failing_groups)} group(s), "
              f"{sum(v[0] for v in failing_groups.values())} sample(s)):")
        for (opid, status, message), (count, example) in sorted(failing_groups.items()):
            print(f"  - {opid} {status} ({count}x, e.g. {example})")
            print(f"      {message}")
        print("\nGATE: FAIL — the above response(s) do not conform to api/openapi.yaml. "
              "Fix the server or the spec, or add an ALLOWLIST entry with a justification.")
        return 1

    print("\nGATE: PASS — every sampled response body conforms to its OpenAPI response schema.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
