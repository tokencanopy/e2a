# Account sending trust ladder

This opt-in control replaces the custom-domain ramp with one account-level
external-recipient allowance. It does not change monthly recipient-delivery
quotas or grant permission to email external recipients. Operator approval,
pauses, suppression, verified sending identity, and provider authorization
continue to apply independently.

## Allowance and accounting

The account starts at 20 external recipients per UTC day. Each completed clean
active day advances a linear schedule: `20 + floor(1980 * min(days, 29) / 29)`.
The thirtieth active day's allowance is 2,000. A day qualifies after at least
one provider-accepted external recipient; idle days and failed attempts earn
nothing. A detector breach disqualifies the day. The detector that records
breaches and reduces trust is a later implementation phase.

Shared-identity recipients count toward the same account allowance and also
have a ceiling of 50 per day. The shared ceiling is a subset, not a separate
allowance. Adding or changing a domain never resets or multiplies trust.
The effective account allowance is the lower of trust and `max_messages_day`:
a null plan cap means unlimited, zero means no external sending, and accounts
without a provisioned limits row conservatively use 20. Thus bare Free remains
at 20; a paid plan or add-on removes only the plan cap, never the trust gate.

External recipients are the admission gate's recipient classes: exclude the
account's live agents and its currently verified owner mailbox. Normalize and
deduplicate the complete To/Cc/Bcc envelope. Internal recipients still count
against monthly quota. This classification is repeated at authorization and
immediately before a provider call, so an old grant cannot acquire new external
recipients after an ownership or verification change. Any count change, including becoming internal, requires a fresh authorization. Legacy/all-recipient attempts never earn trust credit, including across runtime-policy toggles.

Reservations serialize on an account row after the existing account-control,
operation, and budget locks. All identities compete for that same allowance.
Retries reuse the reservation; authoritative rejection/cancellation releases it;
uncertain outcomes keep it. Authoritative late acceptance restores a released
reservation. A refused midnight rollover retains the old reservation so delayed
provider evidence is not lost. Clean-day credit uses the settled attempt’s day and external units, separately from conservative reserved capacity; internal-only acceptance earns no credit. Late activity for a pruned historical bucket is not recreated, preventing a second award for archived days. Final authorization remains the enforcement
boundary; the immediate-send preflight is guidance, not a reservation.

## Configuration and compatibility

Both new settings default to false:

```yaml
sending_ramp:
  account_trust_enabled: true
  disable_legacy_daily_budgets: true
```

`account_trust_enabled` selects the fixed account schedule instead of the legacy
per-domain ramp, even when the budget mode is disabled. Existing domain history
and legacy self-host behavior remain intact when it is false.
`disable_legacy_daily_budgets` removes the probation, account daily, and account
shared daily caps from budget admission while retaining their counter keys for
safe settlement across policy changes. Platform and operational notice pools
remain independently governed by their existing policy. External-only platform counting starts with `account_trust_enabled`; disabling legacy caps alone retains the existing all-recipient platform accounting. A preceding shadow phase must not interpret those counters as external-only evidence.

The corresponding optional runtime-policy keys are omitted when false, preserving
canonical hashes of policies written before these keys existed. Database-sourced
policy remains authoritative at runtime, including daily quota delegation.

## Visibility

`GET /v1/account` has an optional `daily_limit` object with `limit`, `used`,
`shared_limit`, `shared_used`, `clean_active_days`, and `resets_at`. Usage includes
pending or uncertain reservations. The endpoint returns `limits_unavailable`
when the enabled control cannot be read instead of implying that it is absent.
Internal/system accounts are exempt and omit the object.

An immediate request exceeding the allowance returns 402 `limit_exceeded`,
resource `messages_day`, and the same snapshot in `error.details.daily_limit`.
Scheduled sends are checked when they fire. Queued sends encountering the cap
are held until UTC midnight, subject to the existing finite retry horizon;
queued-message and terminal hold diagnostics include usage, allowance, and reset time.
The generated TypeScript and Python models, CLI `whoami`, MCP `whoami`, and
usage dashboard carry the same contract. No platform-wide capacity is exposed.

## Persistence and rollout

Migrations 130–131 add account trust, daily buckets, message reservations, and immutable per-attempt external-unit provenance without
activating the control or exempting existing accounts. Foreign keys erase these
rows with their owning account. Daily maintenance prunes settled history older
than 90 days, folding clean-day credit into the account row first. It retains
unresolved reservations and their buckets. Each run bounds both accounts and
rows processed and skips active account locks.

This PR supplies implementation, not hosted activation. The activation change
must seed already-approved accounts from reviewed observed external volume before
enabling the ladder; `grandfather_daily` supports a bounded floor (0–2,000) without
bypassing the shared ceiling or plan cap. No send-path code sets that floor.
The hosted policy-source switch, reviewed grandfathering command/payload,
seven-day observation gate, detector automation, and production enablement remain
separate rollout work. User-facing rollout guidance belongs with enablement.
