# External sending access

Status: proposed; product direction accepted, implementation and activation pending.
This document changes no runtime behavior. Hosted policy values and account-level
rollout decisions belong in the private operations repository.

## Problem statement

A newly created account must be able to build and test an inbox without gaining
immediate permission to send arbitrary mail using the platform's shared identity.
Quota is capacity, not authorization: purchasing more capacity must never grant
external sending access.

## Goals and non-goals

Success means that, when enforcement applies, an unapproved account makes zero
provider submissions to unauthorized recipients from a shared sending identity.
It can receive mail, send to its verified owner mailbox, and send to live agents
owned by the same account. A verified custom sending identity permits external
mail only when the actual sender uses that identity. An operator can approve or
revoke shared-identity external access independently of billing and account pause.

The rules cover To/Cc/Bcc, send/reply/reply-all/forward, scheduled messages,
customer review approvals, retries, attachments, and every supported client.
Self-host defaults remain unchanged. No public API credential can grant approval.

Out of scope: arbitrary verified-recipient lists, a general recipient-verification
email service, new content classifiers, provider tenant provisioning, changes to
billing quotas, and automatic graduation by account age/payment. Existing content,
quota, suppression, and sending-budget checks remain independent requirements.

## Relevant context and constraints

At the inspected base, `internal/sendingpolicy` owns immutable provider operations,
transactional authorization, budgets, and one-use provider authorizations.
`PrepareExternalTx` handles acceptance; `ConsumeAttempt` authorizes worker delivery;
`RedeemProviderCall` rechecks immediately before the SMTP adapter opens a socket.
The gate already reads current account controls, and pause is independent of budget
mode. Extend this module rather than adding an alternative provider path.

`account_sending_controls.state` means active/paused. Do not overload it with
sandbox/approved states: an approved account can still be paused. The separate
`probation` budget classification describes shared-reputation exposure, not this
new permission, and must not be reused as an approval bit.

Google login checks `email_verified` before `CreateOrGetUser`, but `users.email`
alone is not durable verification evidence: bootstrap and internal provisioning
can also populate it. Ownership of a custom domain, inbound MX verification, and
provider-confirmed outbound identity readiness are different facts.

`RuntimePolicy` is canonicalized, hashed, strictly parsed, and activated with CAS.
An extension must preserve existing hashes for legacy payloads and fail closed
when a policy requires a capability unavailable in the running binary.

Reference behavior: Resend restricts its shared testing sender to the account's
own mailbox, and requires a verified custom domain for other recipients. It does
not require manual production approval. The shared-domain operator approval route
below is an intentional extension for agent inboxes without a customer domain.

- https://resend.com/docs/knowledge-base/403-error-resend-dev-domain
- https://resend.com/docs/knowledge-base/does-resend-require-production-approval

## Proposed design

### Authorization rule

Evaluate the whole provider envelope, not just the visible To field. Refuse the
entire operation when any recipient is unauthorized; never silently drop a Cc/Bcc
or send an allowed subset. A previous inbound message or an asserted Reply-To does
not authorize an external reply recipient.

Apply this ordered rule:

1. Existing account pause and source-deletion checks still deny first.
2. If this feature is disabled or the account is outside the rollout cohort,
   retain existing behavior. First-party system/internal exemptions must use the
   existing server-owned account classification, never names or email suffixes.
3. A customer message whose **actual** outbound identity is currently verified,
   customer-controlled, and owned by the sending account may target external
   recipients. This does not approve other shared-identity agents on the account.
4. A current operator approval permits external shared-identity sending.
5. Otherwise every recipient must be either the currently verified owner mailbox
   or a live, non-trashed agent belonging to the sending account.

All other controls still apply after an allow. A purchased add-on, paid plan,
customer-configurable protection setting, account API key, or message review
approval cannot satisfy steps 3 or 4.

Provider verification that disappears before delivery removes step 3. A stored
`sent_as=own_address` marker alone is insufficient. Never fall back from an
unverified custom identity to an unrestricted shared identity. Apply this to the
identity actually used on the wire, not any verified domain on the account.

### Account state and audit

Extend `account_sending_controls` with:

- `external_sending_approved BOOLEAN NOT NULL DEFAULT false`;
- `external_sending_access_revision BIGINT NOT NULL DEFAULT 0`, nonnegative;
- approval change timestamp (nullable until first operator change).

Use a dedicated append-only access event table because existing control-event
columns represent active/paused transitions. Record account reference, old/new
approval, old/new revision, actor, nonblank reason, and server timestamp; use the
existing control-audit retention policy and documented account-deletion handling.
Do not log message bodies, recipients, or addresses in operational metrics.

Provide local server operator commands to inspect, approve, and revoke. Mutations
require account ID, expected revision, and reason. Lock the account/control rows
in the established order and atomically update approval plus audit. Stale revision
fails without writes. A same-state request with the current revision is a no-op;
a response-loss retry inspects state before retrying. No customer endpoint or MCP
tool can mutate this state. Approval never clears pause; revocation leaves inbound
and account login available. Existing explicit pause wins for every sender class.

### Verified owner mailbox

Persist the exact mailbox verified by the trusted login flow, its verification
source and timestamp, bound to the user. Compare it with the current owner address
at decision time; an owner-email change invalidates the old exception immediately.
Use existing email normalization; do not add Gmail dot/plus alias equivalence.

A trusted Google login records proof only after validated OAuth state, verified
provider response, and subject/account binding. Bootstrap, profile edits, arbitrary
OIDC email claims, and internal user provisioning do not manufacture proof.
Existing users without proof remain without this exception until a verified login
or a separately authorized verification flow supplies it. Never backfill proof
from a nonempty email or a synthetic subject prefix.

For delegated/provisioned accounts in v1, same-account agent tests and verified
custom-domain sending work without owner-mailbox proof. Before hosted activation,
confirm whether their onboarding needs a trusted issuer proof path; if so, implement
and test issuer/subject/email binding before activating the feature for any new accounts. The cutoff does not distinguish
login providers; there is no implicit delegated-account exemption. A caller-supplied
`email_verified=true` on an ordinary profile/provisioning request is insufficient.

### Policy and compatibility

Add an optional `external_sending_access` policy object containing:

- `mode`: disabled/shadow/enforce;
- `accounts_created_at_or_after`: an immutable RFC3339 UTC rollout cutoff.

Absent object means disabled and must remain omitted when canonicalizing a legacy
payload, preserving stored policy hashes. A present object requires a known mode
and a valid cutoff. For all-account enforcement use a documented earliest cutoff;
do not dynamically recompute the cutoff at process startup or first send.

Wire the same typed object through config-source and database-source policies.
Bump the runtime capability contract and gate activation on all active/candidate
slots supporting the feature. Old binaries rejecting the new policy is expected;
rollback requires either a compatible binary or a reviewed downgrade to a policy
without this object. No silent removal of unknown fields or activation bypass.

Shadow mode evaluates the same decision and records bounded reason/count metrics,
but does not block. Enforce is independent of BudgetMode and RampEnabled.

### Acceptance, delivery, and races

Use one internal decision implementation for API acceptance and worker rechecks.

- Preflight the composed sender and full recipient envelope before persisting an
  ordinary or customer-review-held send. Disallowed synchronous requests return
  HTTP 403 with code `external_sending_not_enabled`, and do not enqueue work.
- Repeat the check in `PrepareExternalTx` inside acceptance, so changes between
  preflight and the transaction cannot bypass it. Preserve idempotency replay:
  replaying an already accepted request does not create another message.
- Recheck in `ConsumeAttempt` from durable sender/recipient/account state, covering
  messages accepted before activation, scheduled jobs, retries and held-message
  approval. No authorization on missing proof or dependency failure.
- Recheck at `RedeemProviderCall` before provider I/O, including owner address,
  recipient ownership, approval revocation, and custom identity validity.
- Queued messages newly disallowed become terminal failures with the same stable
  reason, normal lifecycle evidence and `email.failed`; do not create an indefinite
  backlog that silently sends when an account is later approved. Release any unused
  budget reservations. Only definitive permission denials are terminal: transient
  database/dependency errors prevent provider I/O and follow normal bounded retry
  handling, without being mislabeled `external_sending_not_enabled`. An operator
  can inspect failed mail; customers explicitly submit new mail after gaining access.

Follow the existing lock order and account/source deletion behavior; do not append
inverted row locks inside final redemption. Authority changes must be visible at
the final check. State the linearization point precisely: access is decided at the final
authoritative snapshot during redemption. A revocation committed before that
snapshot must refuse; an authorization already past that snapshot may still open
its socket after the revocation commits. This bounded in-flight window includes
reservation update, commit and dial; it is not a guarantee of instantaneous recall.
Test changes between consume and redeem and at that final snapshot. Never claim
transactional recall across a remote SMTP server.

A same-account exception must come from a current identity lookup, including
non-deleted status, not a shared-domain suffix. Inspect the exact self-send loopback
path separately: it may bypass the provider gate, but must remain strictly local
to the sender's own account and must not include external Cc/Bcc recipients.

Customer-triggerable platform notifications require a path inventory. Permit only
fixed platform templates to server-selected, verified owner destinations (plus
server-controlled operational recipients); never use notification purpose as a
way to send arbitrary customer-controlled content to an arbitrary address. Preserve
bounded operational pause/approval notices independently of customer mail access.

### API, SDK, and onboarding contract

Extend `GET /v1/account` with an optional read-only `sending_access` object:
`enforcement_applies`, `shared_external_approved`, and
`owner_recipient_verified`. `enforcement_applies` is true exactly when mode is
enforce and the account is in the cutoff cohort and not a system/internal
exemption; it stays true for approved accounts. It is false when disabled, shadow,
or outside the cohort. `shared_external_approved` reports the operator grant, not
whether the feature is enabled. `owner_recipient_verified` reports valid proof for
the current mailbox. These facts describe account eligibility, not a
promise that a given send passes pause/quota/content/domain checks. A verified
custom domain does not set `shared_external_approved=true`. The new object
contains booleans only and does not introduce an owner mailbox field. Existing
account response fields and their scope behavior remain unchanged; any broader
redaction change requires separate compatibility review.

Expose the additive object and structured 403 code in Go/OpenAPI, both generated
and ergonomic SDKs, CLI account/whoami, MCP whoami, and the web dashboard in one
coordinated implementation. Send/reply/forward tool descriptions must explain the
error and recovery without telling clients to repeatedly resend. Use the existing
error envelope. Do not return HTTP 402 or an upgrade link for a permission denial.

Dashboard copy: "You can receive email and test with your verified email or your
own agents. Verify a sending domain or request approval to email other recipients."
Show the owner option only when proof exists. Approved shared access and verified
custom identity have distinct status labels. Keep existing account access and mail
reading available when outbound is restricted.

For v1, a request-approval link opens an authenticated support flow; there is no
automatic approval endpoint or new customer-editable status. Supply account identity
server-side and keep private request details out of public issue trackers. Support
reviews use case and expected recipients, then uses the audited operator command.
Do not auto-send support requests or customer notices from implementation tooling.

### Alternatives considered

- Lower quotas only: bounds one account but leaves arbitrary-recipient access and
  account rotation intact; keep as defense in depth, not the permission model.
- Require a custom domain for every external send: simpler and close to Resend,
  but excludes legitimate users of shared agent inboxes. Keep audited approval.
- Customer-managed verified-recipient lists: useful later, but introduces challenge
  email abuse, token lifecycle and recipient-consent UX. The owner-only exception
  provides a smaller first release.

## Edge cases and failure handling

Test mixed envelopes, duplicate/case-normalized addresses, external Reply-To,
reply-all and forward, deleted/transferred agents, deleted/reverified domains,
email changes, no proof, unknown/absent approval rows, malformed policies,
concurrent approve/revoke, stale cache, process restart, retry and lost responses.
Source/account/proof database errors fail closed for affected new decisions. Do not
translate dependency failures into successful sends or claim approval is absent
when the database could not be read.

Re-signing or changing an envelope after authorization remains prohibited. Domain
verification on account A cannot authorize account B, nor a different shared agent
on A. Payment webhooks must leave all permission and verification fields untouched.

## Scalability and extensibility

Evaluate recipients in one bounded batch under the existing recipient-count limit,
not one query per recipient. Reuse the existing transaction and identity records.
Approval is account-scoped; grants do not multiply with agent count. Avoid global
locks for this permission check. Metrics use bounded outcomes/reasons, not account
IDs, domains or email labels. This design deliberately introduces no generic policy
engine; later verified-recipient support must supply explicit proof through this
same decision rather than a second allow path.

## Verification strategy

1. Migration tests: idempotence, old rows remain unapproved, proof stays absent,
   audit constraints, deletion behavior and all direct SQL writers remain valid.
2. Boundary tests with real Postgres: acceptance and actual provider adapter; all
   authorization branches plus zero provider calls for denied paths. Race tests
   cover revoke, domain loss and owner/recipient changes before redemption.
3. HTTP contract scenarios through Go/TS/Python live-server runners: denied mixed
   envelopes, allowed owner/same-account/verified-domain/approved sends, tenant
   isolation and idempotency. Verify 403 shape and additive account status.
4. CLI/MCP/dashboard tests: consistent recovery guidance, scope redaction, no
   upgrade/self-approve bypass, correct success and failure presentation.
5. Policy upgrade/rollback: old hashes unchanged when absent, strict new object,
   mixed-slot capability refusal, config/database parity and cohort cutoff.
6. Local over-the-wire e2e against Postgres + Mailpit, then staging with synthetic
   accounts and controlled recipients. Exercise immediate, scheduled, approval,
   reply/forward and retries. Delete test accounts and messages permanently.

Run repo-required spec generation, compatibility, client generation/parity,
contract, unit/integration, race and relevant web gates. Independent correctness
and adversarial reviews are required before merge. Activation evidence must prove
both allowed delivery and absence of provider submission for blocked traffic.

## Implementation slices and rollout

1. Persist owner verification and audited operator approval; add policy extension,
   capability checks and migration tests. Defaults remain disabled.
2. Integrate acceptance and final-delivery enforcement, lifecycle failures, and all
   bypass/race tests. Keep code in `sendingpolicy` and existing identity/auth paths.
3. Add account status and errors symmetrically across every client and dashboard;
   regenerate contracts and run live conformance. These slices ship together in
   one OSS implementation PR; no half-functional public API release.
4. In a separate private ops PR, configure staging, test shadow then enforce, pin
   the verified release, and propose the production cutoff. Merge remains the
   operator gate. Receiving and self-host defaults stay unchanged.
5. Proposed initial cohort: accounts created at/after activation. Review existing
   accounts privately before expanding; do not equate preexisting activity with
   trust or mark every old account operator-approved. Existing explicit pauses
   stay effective regardless of cohort. Freeze cutoff across restarts/rollback.

No production database reads, approval grants, customer communication, new release,
or policy activation is authorized merely by merging this design document.

## Open questions / decisions before activation

- Confirm initial cohort: new accounts first is proposed; enforcement for existing
  unapproved accounts requires an explicit rollout choice and impact review.
- Confirm delegated-account onboarding: is same-account testing sufficient until
  approval/domain setup, or must trusted owner-email proof ship in this release?
- Choose the private support intake and operational response target. Approval must
  be a usable path when enabled; a dead link is not an implementation.
