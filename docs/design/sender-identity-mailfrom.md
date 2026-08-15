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
- Every created SES identity carries `e2a-managed=sender-identity-v1`.
  Existing identities without that exact tag are treated as foreign: e2a will
  neither update nor delete them. The runtime IAM role should independently
  require `aws:RequestTag/e2a-managed=sender-identity-v1` for create/tag calls
  and `aws:ResourceTag/e2a-managed=sender-identity-v1` for DKIM, MAIL FROM, and
  delete mutations. This provider-side condition closes the check-then-mutate
  race that a client-side ownership check cannot close.
  `ses:TagResource` is also callable directly because AWS does not expose a
  create-only variant of its dependent authorization. e2a itself never makes
  that standalone call, but the runtime credential must remain trusted: this
  tag is not a boundary against its deliberate compromise. Do not operate two
  e2a installations with this shared tag value in the same AWS account and
  region; use an isolated account/region or separately scoped principal.
- Mutation and reconcile job kinds and their River queue are versioned for
  blue/green rollout. The old slot does not listen to the v2 queue and cannot
  claim new work; legacy jobs are drained by compatibility
  workers that converge current state and hand polling to an incarnation-aware
  v2 job with a fresh attempt budget.
- **Rollback contract — two-phase job-lane rollout plus an operator mutation freeze.**
  The old binary registers neither the v2 kinds nor the queue, so a v2 job
  committed while a new binary bakes strands unconsumed on rollback (and the
  rolled-back binary's blind legacy deprovision semantics return, including
  the pre-existing delete/re-register ABA hazard the versioning closes).
  The River lanes are made rollback-compatible by splitting the producer flip
  from the binary deploy: **phase 1** deploys this release with
  `legacy_job_compat: true` — it PRODUCES the legacy kinds (consumable by the
  previous release) while CONSUMING both lanes. This flag is not a provider
  mutation boundary: the previous worker creates identities without the new
  ownership tag and deletes without checking it. Therefore operators MUST
  freeze domain verification, domain deletion, and account deletion before a
  pre-ownership binary overlaps, and keep that freeze through old-slot drain
  and the post-drain convergence window. **Phase 2**, once phase 1 is the stable
  rollback target, flips the flag off in a config-only deploy; a rollback of
  phase 2 lands on the phase-1 binary, which consumes v2 — nothing strands.
  Residual: post-drain audit jobs are v2-only in both phases; a phase-1
  rollback strands only those repair *accelerators* (bounded — ledger + the
  next v2 binary's RunOnStart sweep converge them; the old binary's List
  sweep still ALERTs on orphans meanwhile). Apply the tag-conditioned IAM
  policy before any mixed-version overlap. Under that policy a rolled-back
  pre-tagging binary cannot CREATE identities (existing identities keep
  sending), but it can still delete a tagged identity; rollback to that binary
  therefore requires the same mutation freeze and leaves provisioning paused
  until roll-forward. Do not lift the freeze until the new binary is the sole
  worker, its post-drain audit/reaper is green, and it is the accepted rollback
  target. This operational gate is what closes the old-worker race; the compat
  flag alone never does.
- **`DELETE /v1/domains/{domain}` semantics:** the transaction commits the
  guarded row delete plus the durable teardown job — that is the API success
  boundary, independent of SES availability (an untagged/foreign identity or
  an SES outage can never fail the delete). A best-effort post-commit
  deprovision (bounded ~10s, errors logged only) then converges immediately.
  The response's open `sending_teardown` value is the DNS-release contract:
  only `confirmed` proves provider absence; `pending`, `manual_review`, and
  unknown future values require retaining DNS. `manual_review` means a
  provider identity exists but its ownership marker is absent, so e2a refuses
  to mutate it. The delete transaction also persists an owner-scoped teardown
  receipt. Each logical deletion should carry a unique `Idempotency-Key`; a
  retry with the same key replays the original receipt without deleting a
  same-name replacement registration. While the domain remains absent, an
  unkeyed repeat reads the current receipt so teardown can be polled until
  `confirmed`; never use an unkeyed retry across re-registration. Re-registering
  the domain clears the stale mutable receipt before the new incarnation is
  created, while the standard idempotency cache retains the original keyed
  response for its published window. If the sender provider
  is disabled while the durable ledger still records a managed identity, the
  receipt remains `pending`; only a provider-absence check, or a disabled
  provider plus proof that no managed ledger row ever existed, can produce
  `confirmed`. When a best-effort attempt fails transiently, teardown converges
  via the committed job and the hourly reaper instead. (An earlier
  revision ran Deprovision
  synchronously inside the delete transaction; review showed that coupling
  only added failure modes — permanent 500s on foreign identities, deletes
  blocked by SES outages, and a resurrection window after an irreversible
  provider delete — that the ledger already heals.)
- **Healthy-recheck no-op:** a forced mutation signal (POST /verify on an
  already-verified domain) short-circuits to a single provider GET when the
  ledger confirms the current incarnation applied AND the provider reports
  verified; a transient GET error retries the job rather than converging
  blind. Any definitive non-healthy outcome falls through to full
  convergence. This restores the pre-ledger guard against flapping a verified
  sender back to pending on every re-check. Caveat: the GET cross-checks the
  provider's verification *status*, not the installed key material —
  `applied_incarnation == incarnation` is what ties "verified" to this
  registration's selector/key, and that holds only while selector/key are
  immutable per incarnation. Any future in-place DKIM rotation or MAIL FROM
  convention change must invalidate `applied_incarnation` or revisit the
  gate.
- The MX/SPF records are **preserved across verify** — `Status()` re-emits them on
  every poll, so a verified domain's view keeps showing the records the customer
  must KEEP published (removing them later silently loses SPF alignment). (Adjusted
  from the original "clear on verify" after review.)

## Upgrading an existing SES-enabled installation

The ownership tag is a deliberate migration gate. Do not bulk-adopt everything
returned by `ListEmailIdentities`: an AWS account/region may contain identities
owned by another application.

1. Before deploying this release, export the customer-owned domains for which
   the existing e2a database records sender-identity work (`sending_status !=
   'none'`). Separately inventory SES identities in the configured region.
2. Review the intersection and confirm each candidate was created for this e2a
   installation. Exclude every SES-only identity and every ambiguous domain.
3. Tag only the confirmed candidates:

   ```bash
   aws sesv2 tag-resource \
     --region "$AWS_REGION" \
     --resource-arn "arn:aws:ses:$AWS_REGION:$AWS_ACCOUNT_ID:identity/$DOMAIN" \
     --tags Key=e2a-managed,Value=sender-identity-v1
   ```

4. Grant the runtime principal `ses:TagResource` as dependent authorization for
   tagged `CreateEmailIdentity` calls, in addition to the Create/Get/List/Put/
   Delete actions above. Apply request-tag conditions to create/tag and the
   resource-tag condition to both Put operations and delete.
5. Re-run both inventories and apply the strict tag-conditioned IAM policy.
   An untagged legacy e2a identity fails
   closed with `identity not owned`; audit and tag it explicitly rather than
   weakening the ownership check.
6. Freeze domain verification, domain deletion, and account deletion, then
   deploy with `sender_identity.legacy_job_compat: true`. After cutover, wait
   for the previous slot to stop and for the post-drain convergence window;
   require the candidate reaper/audit and legacy-job drain checks to be green.
   If rollback to the pre-tagging binary is required, re-establish/retain the
   freeze first and keep provisioning paused until roll-forward.
7. Once this release is the sole worker and accepted rollback target, lift the
   mutation freeze. Flip `legacy_job_compat` to false in a later config-only
   deploy; rollback of that flip lands on the v2-capable phase-1 binary.

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
