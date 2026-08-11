# Remove "via e2a" — custom MAIL FROM alignment

Status: **Track B shipped** (the MAIL FROM mechanic, behind the existing dormant
`sender_identity.ses_region` gate). **Track A** (live-SES validation + enabling
`ses_region` in prod) is the remaining ops gate.

## Problem
Outbound mail showed "via e2a" two ways: (a) a literal `"Name via e2a"` display
name e2a wrote for non-verified domains, and (b) Gmail's auto **"via / mailed-by
e2a.dev"** label, driven by the envelope **MAIL FROM (Return-Path)** which was
always e2a-owned — even for DKIM-aligned own-address sends. (a) was already gone
for verified domains; (b) needed an aligned Return-Path.

## What shipped (Track B)
- **`internal/mailfrom`** — the shared subdomain convention: `Domain("acme.com")
  = "bounce.acme.com"`, `EnvelopeSender = "bounces@bounce.acme.com"`. One source
  of truth for the SES MAIL FROM config, the published DNS records, and the
  envelope sender. Leaf package (zero internal deps) so `outbound` uses it without
  importing `senderidentity`.
- **SES provider** (`internal/senderidentity/ses.go`): `Provision` now also calls
  `PutEmailIdentityDkimSigningAttributes` on `AlreadyExists` so a deleted and
  re-registered domain replaces the prior incarnation's BYODKIM selector/key,
  then calls
  `PutEmailIdentityMailFromAttributes(MailFromDomain=bounce.<domain>,
  BehaviorOnMxFailure=USE_DEFAULT_VALUE)` after `CreateEmailIdentity` (incl. the
  idempotent `AlreadyExists` path), and returns the **MX + SPF** DNS records
  (region-targeted: `10 feedback-smtp.<region>.amazonses.com` + `v=spf1
  include:amazonses.com ~all`). Records flow unchanged via `SetSendingStatus` →
  `sending_dns_records` → the domain view.
- **`mapSESStatus`** now requires the MAIL FROM axis: `verified` ⇔ sending-verified
  **AND** DKIM `SUCCESS` **AND** MAIL FROM `SUCCESS` (**all-or-nothing**, design
  Q2). A hard failure on either DKIM or MAIL FROM is terminal. So reaching
  `verified` means there is genuinely no "via e2a".
- **Send path** (`internal/outbound/sender.go`): the sending-verified gate is
  resolved once; a verified domain's Return-Path becomes
  `bounces@bounce.<domain>` (aligned → SPF passes on the From org-domain → no
  "via"), else the e2a relay envelope + "via e2a" rewrite (**fail-closed**,
  unchanged). Bounces still reach SES's feedback handler via the subdomain MX, so
  e2a keeps capturing them.
- `FakeProvider` mirrors the records.

## Decisions (from design Q&A)
- **Q2 all-or-nothing** — no DKIM-only intermediate tier; `verified` requires both
  axes. The intermediate state passes DMARC (via DKIM) but still shows "via", so
  it doesn't meet the goal and isn't exposed.
- **Q3 `USE_DEFAULT_VALUE`** on MX failure — deliverability-safe (SES falls back to
  its own MAIL FROM rather than dropping mail; the send path only uses the aligned
  envelope when `verified`, which requires the MX).
- **Q1/Q6 deviation from the design:** the subdomain label ships as a **fixed
  convention const (`bounce`)**, derived (no schema change). A per-deployment
  config knob is a trivial future addition (thread one string to the SES provider
  + the Sender); deferred to keep v1 minimal.

## Edge cases / invariants
- Fail-closed: any non-`verified` state → e2a relay envelope + "via" From.
- `Provision` idempotent (`CreateEmailIdentity` `AlreadyExists` refreshes both
  BYODKIM and MAIL FROM with idempotent `PutEmailIdentity…` operations; the
  desired-state worker serializes provider mutations per domain).
- Migration 101 adds a durable managed-domain ledger. It is written before a
  provider create/update, records the applied registration incarnation only
  after success, and survives domain deletion until SES confirms teardown. The
  hourly reaper can therefore retry exhausted jobs without ever deleting an
  unrelated identity from the same SES account.
- Mutation and reconcile job kinds and their River queue are versioned for
  blue/green rollout. The old slot does not listen to the v2 queue and cannot
  claim new work; legacy jobs are drained by compatibility
  workers that converge current state and hand polling to an incarnation-aware
  v2 job with a fresh attempt budget.
- The MX/SPF records are **preserved across verify** — `Status()` re-emits them on
  every poll, so a verified domain's view keeps showing the records the customer
  must KEEP published (removing them later silently loses SPF alignment). (Adjusted
  from the original "clear on verify" after review.)

## Verification
- Unit: `mapSESStatus` across both axes; `Provision` configures MAIL FROM + emits
  MX/SPF (incl. `AlreadyExists`); `mailfrom` convention; `envelopeSender`
  (verified → aligned, else relay/fail-closed).
- Local-service e2e (real binary + Mailpit): seeded a `sending_status=verified`
  domain and a `none` domain; over real SMTP confirmed verified → `From:
  bot@acme.e2etest` (no "via") + `Return-Path: bounces@bounce.acme.e2etest`;
  fail-closed → `From: … via e2a <agent@relay>` + relay Return-Path. (Seeding
  `sending_status` stands in for the SES reconciler, which needs live AWS.)

## Deferred
- **Track A** — validate the real `sesv2` path (CreateEmailIdentity +
  PutEmailIdentityDkimSigningAttributes + PutEmailIdentityMailFromAttributes +
  GetEmailIdentity + the BYODKIM
  PKCS#1→PKCS#8 key) against a live SES account; then set `sender_identity.ses_region`
  in prod. Confirm in Gmail: no "via", aligned `mailed-by`, DMARC pass on SPF+DKIM.
- Per-deployment configurable subdomain label (Q1).
- **ARC sealing** + `_dmarc` policy fetch (separate inbound-auth deferrals) — out
  of scope.
