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
    suites/21-webhook-events.test.ts AND suites/prod/*.test.ts, each a JSON
    array of event-type strings the suite VERIFIED this run. "Verified" is a
    deliberately high bar (see 21-webhook-events.test.ts's module doc): a
    type is recorded ONLY after BOTH halves of the dual assertion passed —
    (1) the event's own delivery_status.matched_webhooks >= 1 (event-scoped
    fanout) AND (2) a listWebhookDeliveries entry for OUR webhook with
    attempts >= 1 (webhook-scoped delivery attempt). Neither alone proves
    emission; merely subscribing a webhook to a type, or a type appearing in
    listEvents, is NOT enough to be recorded. There is no dedicated harness
    recorder for this (unlike coverage.ts / mcp-coverage.ts) — the
    shard-writing lives directly in each suite file so this gate stays a
    thin consumer of it, like the other two.
  - reports/target/*.json: shards written by harness/target.ts, used via
    target_env.py to decide whether THIS run targeted production — see that
    module's doc for the full mechanism. This is what makes the allowlist
    below environment-aware instead of a permanent staging-shaped excuse.

ENVIRONMENT-AWARE ALLOWLIST: two tiers.
  - ALWAYS_ALLOWLIST: allowlisted regardless of target (the domain.sending_*
    pair — needs real DKIM sending-identity provisioning against a *custom*
    domain, which is a different, still-unbuilt fixture; not something a
    prod-vs-staging distinction unlocks by itself).
  - STAGING_ONLY_ALLOWLIST: allowlisted ONLY when this run did not target
    production. These all trace back to the same real-SES-feedback blocker
    that staging's e2a-staging-smtp IAM policy imposes; production has no
    such block, so a production run REQUIRES them — an allowlisted-forever
    entry would let a genuine prod gap hide behind a staging-only excuse.

Usage: python3 event_coverage_gate.py [--openapi PATH] [--reports DIR] [--target-dir DIR]
Exit 0 = every event type verified (or explicitly allowlisted); 1 = coverage
gap; 2 = usage/IO error (including "no shards" for either coverage or target
data, which must never read as a pass).
"""
import argparse
import glob
import json
import os
import sys

from target_env import resolve_target

HERE = os.path.dirname(os.path.abspath(__file__))

# Allowlisted no matter what the run targeted — see the module doc above.
ALWAYS_ALLOWLIST = {
    "domain.sending_verified": "requires real SES sending-identity (DKIM) "
    "provisioning against a custom domain, verified asynchronously by AWS over "
    "minutes-to-hours; a throwaway shared-domain e2e agent has no sending identity "
    "to provision — needs a dedicated custom-domain + sender-identity fixture "
    "(domain-lifecycle territory), independent of prod-vs-staging.",
    "domain.sending_failed": "same real SES sending-identity provisioning "
    "dependency as domain.sending_verified.",
}

# Allowlisted ONLY on a non-production run — REQUIRED once the run's target
# shards (reports/target/*.json) show production. See the module doc above.
STAGING_ONLY_ALLOWLIST = {
    "email.delivered": "requires real SES delivery feedback (SNS); staging's "
    "e2a-staging-smtp IAM policy denies ses:SendRawEmail to the mailbox-simulator "
    "addresses, so delivery feedback cannot be produced there. Verified in "
    "production by suites/prod/31-ses-feedback.test.ts.",
    "email.bounced": "same SES delivery-feedback blocker as email.delivered. "
    "Verified in production by suites/prod/31-ses-feedback.test.ts.",
    "email.complained": "same SES delivery-feedback blocker as email.delivered. "
    "Verified in production by suites/prod/31-ses-feedback.test.ts.",
    "domain.suppression_added": "a suppression is created only by a real SES "
    "bounce/complaint (no createSuppression API — see coverage_gate.py's "
    "STAGING_ONLY_ALLOWLIST deleteSuppression entry), which staging cannot "
    "produce for the same reason as email.bounced/complained. Verified in "
    "production by suites/prod/31-ses-feedback.test.ts.",
    "agent.suppression_added": "NOTE: unlike domain.suppression_added, this one "
    "does not actually require a real bounce — internal/delivery/consumer.go's "
    "SES-feedback path only ever fires domain.suppression_added (account-scoped); "
    "an agent-scoped suppression is created solely by the manual "
    "createAgentSuppression API or the unsubscribe-token flow, both already "
    "black-box testable anywhere (see suites/24-agent-suppressions.test.ts). It "
    "was allowlisted here under the same blanket real-bounce justification as its "
    "siblings, which doesn't actually apply to it. Verified in production by "
    "suites/prod/31-ses-feedback.test.ts (via the manual-create path); kept "
    "staging-only-allowlisted rather than un-allowlisted everywhere because no "
    "staging suite wires a webhook to it yet — a reasonable follow-up, not done "
    "here to keep this change scoped to the prod-only suite.",
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
    ap.add_argument("--target-dir", default=os.path.join(HERE, "reports", "target"))
    args = ap.parse_args()

    if not os.path.exists(args.openapi):
        print(f"event_coverage_gate: openapi spec not found: {args.openapi}", file=sys.stderr)
        return 2

    all_types = set(load_event_types(args.openapi))

    # An allowlist entry that no longer names a real event type is a silent hole
    # (e.g. the type was renamed) — fail loudly so either tier can't drift stale.
    stale = (set(ALWAYS_ALLOWLIST) | set(STAGING_ONLY_ALLOWLIST)) - all_types
    if stale:
        print(
            f"event_coverage_gate: allowlist entries not in the spec (renamed/removed?): {sorted(stale)}",
            file=sys.stderr,
        )
        return 2

    try:
        targeted_prod, target_shard_count, hosts = resolve_target(args.target_dir)
    except ValueError as e:
        print(f"event_coverage_gate: {e}", file=sys.stderr)
        return 2

    allowlist = dict(ALWAYS_ALLOWLIST)
    if targeted_prod:
        env_label = "PRODUCTION"
    else:
        env_label = "non-production (staging/self-hosted)"
        allowlist.update(STAGING_ONLY_ALLOWLIST)

    if not os.path.isdir(args.reports):
        print(
            f"event_coverage_gate: no shard directory at {args.reports}. "
            "Run suites/21-webhook-events.test.ts (and, on a prod run, suites/prod/*.test.ts) first; "
            "an absent run is not a pass.",
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
    allowlisted = missing & set(allowlist)
    uncovered = missing - set(allowlist)

    print(f"Target environment   : {env_label}  ({target_shard_count} target shard(s): {hosts})")
    print(f"Webhook event types : {len(all_types)}  (CreateWebhookRequest.events enum, api/openapi.yaml)")
    print(f"Verified (dual-assertion emission) : {len(verified & all_types)}")
    print(f"Allowlisted          : {len(allowlisted)} " + (str(sorted(allowlisted)) if allowlisted else ""))
    print(f"Coverage shards      : {len(shards)}")

    if uncovered:
        print(f"\nUNVERIFIED ({len(uncovered)}):")
        for t in sorted(uncovered):
            tier = " (staging-only allowlist, REQUIRED on this prod run)" if t in STAGING_ONLY_ALLOWLIST else ""
            print(f"  - {t}{tier}")
        print(
            "\nGATE: FAIL — the above event type(s) are in the spec but no suite verifies real emission "
            "for this target environment (add a test to suites/21-webhook-events.test.ts or "
            "suites/prod/*.test.ts, or an ALLOWLIST entry with a specific reason)."
        )
        return 1

    print("\nGATE: PASS — every webhook event type is verified (or explicitly allowlisted).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
