"""Shared helper: which deployment did the run that produced these shards
actually target?

coverage_gate.py (deleteSuppression) and event_coverage_gate.py (SES
delivery feedback + the suppression events it causes) each allowlist a
handful of operations/event-types with a STAGING-specific justification —
real SES delivery feedback and the real-bounce-only account suppression it
creates cannot be produced against staging (its e2a-staging-smtp IAM policy
denies ses:SendRawEmail to the mailbox-simulator addresses). Once a suite
runs those against PRODUCTION instead (tests/e2e-prod/suites/prod/), that
excuse no longer holds — the allowlist entry must become a REQUIRED
coverage item, or a genuine production gap could hide behind a staging-only
justification forever.

The signal for "did this run target production" has to be un-fudgeable: not
an operator flag (E2E_ALLOW_PROD is a safety opt-in, not a "grade me as
prod" switch — trusting it here would let a mis-set env var silently relax
the gate). Instead it's read from reports/target/*.json, shards written by
harness/target.ts's recordTarget() — called once per suite-file process,
from the SAME resolved apiUrl every request in that process actually used,
through the SAME hostname allowlist (env.ts's isProductionTarget) that gates
the destructive prod opt-in in the first place. `node --test` runs each
suite/*.test.ts in its own process, and every suite (staging and prod-only
alike) constructs an ApiClient, so a shard exists whenever any suite ran.

Fail-closed like the other two gates' "no coverage shards" case: an absent
run must never silently read as a staging pass (or a prod pass). Call
resolve_target() and let a ValueError propagate to an exit(2) at the
call site — never guess.
"""
import glob
import json
import os


def resolve_target(target_dir: str) -> tuple[bool, int, list[str]]:
    """Returns (targeted_prod, shard_count, sorted distinct apiUrls).

    targeted_prod is True iff ANY recorded shard's apiUrl resolved to a
    hosted production origin — a run that touches prod even once must be
    held to the stricter bar; there is no such thing as "partially staging".
    """
    if not os.path.isdir(target_dir):
        raise ValueError(
            f"no target shard directory at {target_dir}. Run a suite first — "
            "harness/target.ts's recordTarget() (wired into ApiClient's constructor) "
            "writes one shard per suite-file process; an absent run is not a pass."
        )
    shards = sorted(glob.glob(os.path.join(target_dir, "*.json")))
    if not shards:
        raise ValueError(f"no target shards in {target_dir} — did any suite run?")
    targeted_prod = False
    hosts: set[str] = set()
    for shard in shards:
        with open(shard) as f:
            data = json.load(f)
        hosts.add(str(data.get("apiUrl", "?")))
        if data.get("isProd"):
            targeted_prod = True
    return targeted_prod, len(shards), sorted(hosts)
