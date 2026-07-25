#!/usr/bin/env python3
"""Webhook event-type coverage gate.

Every webhook EVENT TYPE e2a can emit must be exercised by the e2e-prod suite
— with REAL emission, not just a subscription — or the gate fails. This is the
event-type analogue of coverage_gate.py (which covers /v1 operationIds) and
mcp_coverage_gate.py (which covers MCP tools); read both before changing this
file, and keep the same fail-closed discipline: no data / a stale allowlist /
an unknown shard entry must never read as a pass.

SCOPE: this gate is entirely about webhook EVENT TYPES (email.sent,
domain.sending_verified, ...) — the `type` field on an EventView / the
`events[]` a webhook subscribes to. It does NOT cover /v1 operationIds
(coverage_gate.py's job) or MCP tools (mcp_coverage_gate.py's job); a webhook
CRUD operation being covered there says nothing about whether the EVENT TYPES
it can be subscribed to are ever actually emitted and observed, which is what
this gate proves.

Inputs:
  - api/openapi.yaml (the drift-gated SSOT): the denominator is
    components.schemas.CreateWebhookRequest.properties.events.items.enum —
    the exact same enum a client sees when subscribing a webhook, generated
    from the server's own OpenAPI output. Do NOT grep internal/webhookpub
    (the Go AllEventTypes source) instead: that measures the repo, not the
    deployed/drift-gated contract, and (per coverage_gate.py's own precedent)
    would go quietly stale exactly when a 16th event type is added without a
    matching gate update.
  - reports/event-coverage/*.json: shards written by
    suites/21-webhook-events.test.ts, each a JSON array of event-type strings
    the suite VERIFIED this run. "Verified" is a deliberately high bar (see
    that suite's module doc): the suite records a type here ONLY after BOTH
    halves of its dual assertion passed — (1) the event's own
    delivery_status.matched_webhooks >= 1 (event-scoped fanout) AND (2) a
    listWebhookDeliveries entry for OUR webhook with attempts >= 1
    (webhook-scoped delivery attempt). Neither alone proves emission (see
    21-webhook-events.test.ts's module doc for why); merely subscribing a
    webhook to a type, or a type appearing in listEvents, is NOT enough to be
    recorded. There is no dedicated harness recorder for this (unlike
    coverage.ts / mcp-coverage.ts) — the shard-writing lives directly in the
    suite file so this gate stays a thin consumer of it, like the other two.

Usage: python3 event_coverage_gate.py [--openapi PATH] [--reports DIR]
Exit 0 = every event type verified (or explicitly allowlisted); 1 = coverage
gap; 2 = usage/IO error (including "no shards", which must never read as a
pass).
"""
import argparse
import glob
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# Event types the black-box conformance suite intentionally does NOT verify
# real emission for, with the SPECIFIC reason staging cannot produce each.
# Keep this list SHORT and justified — every entry is coverage we're
# knowingly forgoing, deferred to a prod-only differential suite (already
# planned as the next phase) where the real infrastructure exists.
ALLOWLIST = {
    "email.delivered": "requires real SES delivery feedback (SNS); staging's "
    "e2a-staging-smtp IAM policy denies ses:SendRawEmail to the bounce/complaint "
    "mailbox-simulator addresses, so delivery feedback cannot be produced there "
    "— deferred to the prod-only differential suite.",
    "email.bounced": "same SES delivery-feedback blocker as email.delivered — "
    "staging's e2a-staging-smtp IAM policy denies ses:SendRawEmail to the bounce "
    "simulator address — deferred to the prod-only differential suite.",
    "email.complained": "same SES delivery-feedback blocker as email.delivered — "
    "staging's e2a-staging-smtp IAM policy denies ses:SendRawEmail to the complaint "
    "simulator address — deferred to the prod-only differential suite.",
    "domain.suppression_added": "a suppression is created only by a real SES "
    "bounce/complaint (no createSuppression API — see coverage_gate.py's "
    "deleteSuppression entry), which staging cannot produce for the same reason "
    "as email.bounced/complained — deferred to the prod-only differential suite.",
    "agent.suppression_added": "same real-bounce dependency as "
    "domain.suppression_added — staging cannot produce the real bounce needed to "
    "seed an agent-scoped suppression — deferred to the prod-only differential suite.",
    "domain.sending_verified": "requires real SES sending-identity (DKIM) "
    "provisioning against a custom domain, verified asynchronously by AWS over "
    "minutes-to-hours; a throwaway shared-domain e2e agent has no sending identity "
    "to provision — deferred to the prod-only differential suite.",
    "domain.sending_failed": "same real SES sending-identity provisioning "
    "dependency as domain.sending_verified — deferred to the prod-only "
    "differential suite.",
}


def load_event_types(openapi_path):
    try:
        import yaml
    except ImportError:
        print("event_coverage_gate: PyYAML required (pip install pyyaml)", file=sys.stderr)
        sys.exit(2)
    with open(openapi_path) as f:
        spec = yaml.safe_load(f)
    try:
        enum = (
            spec["components"]["schemas"]["CreateWebhookRequest"]["properties"]["events"]["items"]["enum"]
        )
    except (KeyError, TypeError) as e:
        print(
            f"event_coverage_gate: CreateWebhookRequest.events.items.enum not found in the spec "
            f"(schema shape changed?): {e}",
            file=sys.stderr,
        )
        sys.exit(2)
    if not isinstance(enum, list) or not enum:
        print("event_coverage_gate: CreateWebhookRequest.events enum is empty — refusing a vacuous pass", file=sys.stderr)
        sys.exit(2)
    return list(enum)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--openapi", default=os.path.join(HERE, "../../api/openapi.yaml"))
    ap.add_argument("--reports", default=os.path.join(HERE, "reports", "event-coverage"))
    args = ap.parse_args()

    if not os.path.exists(args.openapi):
        print(f"event_coverage_gate: openapi spec not found: {args.openapi}", file=sys.stderr)
        return 2

    all_types = set(load_event_types(args.openapi))

    # An allowlist entry that no longer names a real event type is a silent hole
    # (e.g. the type was renamed) — fail loudly so the allowlist can't drift stale.
    stale = set(ALLOWLIST) - all_types
    if stale:
        print(
            f"event_coverage_gate: allowlist entries not in the spec (renamed/removed?): {sorted(stale)}",
            file=sys.stderr,
        )
        return 2

    if not os.path.isdir(args.reports):
        print(
            f"event_coverage_gate: no shard directory at {args.reports}. "
            "Run suites/21-webhook-events.test.ts first; an absent run is not a pass.",
            file=sys.stderr,
        )
        return 2

    shards = sorted(glob.glob(os.path.join(args.reports, "*.json")))
    if not shards:
        print(f"event_coverage_gate: no coverage shards in {args.reports} — did the suite run?", file=sys.stderr)
        return 2

    verified = set()
    for shard in shards:
        with open(shard) as f:
            verified.update(json.load(f))

    # A shard claiming a type the spec doesn't know is a recorder bug, not
    # coverage — report it for visibility but never let it inflate the count.
    unknown = sorted(verified - all_types)
    if unknown:
        print(f"event_coverage_gate: shard(s) verified unknown event type(s) not in the spec: {unknown}", file=sys.stderr)

    missing = all_types - verified
    allowlisted = missing & set(ALLOWLIST)
    uncovered = missing - set(ALLOWLIST)

    print(f"Webhook event types : {len(all_types)}  (CreateWebhookRequest.events enum, api/openapi.yaml)")
    print(f"Verified (dual-assertion emission) : {len(verified & all_types)}")
    print(f"Allowlisted          : {len(allowlisted)} " + (str(sorted(allowlisted)) if allowlisted else ""))
    print(f"Coverage shards      : {len(shards)}")

    if uncovered:
        print(f"\nUNVERIFIED ({len(uncovered)}):")
        for t in sorted(uncovered):
            print(f"  - {t}")
        print(
            "\nGATE: FAIL — the above event type(s) are in the spec but no suite verifies real emission "
            "(add a test to suites/21-webhook-events.test.ts, or an ALLOWLIST entry with a specific reason)."
        )
        return 1

    print("\nGATE: PASS — every webhook event type is verified (or explicitly allowlisted).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
