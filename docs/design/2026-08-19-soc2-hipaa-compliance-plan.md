# SOC 2 / HIPAA compliance plan

**Status:** proposed
**Date:** 2026-08-19 (revised 2026-08-20 after maintainer review)

This document is the roadmap for making e2a usable by HIPAA-regulated
customers and auditable under SOC 2. It is grounded in a file-level review of
the current tree: every gap below carries a code anchor, and the plan is
phased so each phase is independently shippable and independently valuable.

## Governing principle

> **For a HIPAA-enabled account, treat every email and its associated
> metadata as potentially regulated data and route it through a tightly
> controlled, fail-closed HIPAA data path.**

We never depend on content classification to decide whether a particular
message contains PHI. The account-level capability defines the boundary: when
an account is HIPAA-enabled, every message, attachment, header, address, and
derived artifact belonging to that account is inside the boundary, whatever
it contains. Three consequences run through the whole plan:

- **The boundary is architectural, not inferential.** No scanner, heuristic,
  or LLM ever decides that some message is "not PHI" and may leave the
  controlled path.
- **Every copy inherits the controls.** Encryption, retention, deletion,
  access control, and logging rules apply to every place regulated data
  lands — secondary copies and operational surfaces included — not only the
  primary `messages` columns.
- **Misconfiguration fails closed.** When an invariant of the HIPAA path
  cannot be satisfied (a non-approved subprocessor, a TLS downgrade, an
  unapproved LLM endpoint), the operation is refused; it never silently
  degrades onto the ordinary path.

## Why

Two commercial forces point at overlapping engineering work, and the plan
orders them deliberately:

1. **HIPAA** applies the moment a covered entity (or another business
   associate) routes protected health information (PHI) through an agent
   inbox. e2a stores message bodies, attachments, and raw MIME persistently
   in Postgres (`migrations/001_init.sql`, `migrations/056_inbound_intake.sql`),
   so the "mere conduit" exception is unavailable — HHS OCR guidance is
   explicit that a service which *maintains* ePHI is a business associate
   even if it never looks at the content. Serving healthcare customers means
   signing Business Associate Agreements (BAAs) and meeting the Security
   Rule's administrative, physical, and technical safeguards.
2. **SOC 2 Type II** is the de-facto gate for selling any hosted service to
   enterprises. It is complementary work that shares most of its control
   surface with the HIPAA program (compliance platforms cross-map ~65% of
   controls), but it is **not a prerequisite for signing a BAA or launching a
   correctly scoped HIPAA service**. It proceeds in parallel or afterward on
   its own commercial timeline (Phase 4).

One regulatory deadline sharpens the HIPAA work: the HHS **Security Rule
NPRM (January 6, 2025)** proposes making encryption at rest/in transit, MFA,
network segmentation, and annual business-associate verification *mandatory*
(no more "addressable" outs). Final action is currently targeted for 2027
with a ~240-day compliance runway. Building to the NPRM bar now means the
final rule is a non-event.

## Scope and the open-core boundary

e2a is open-core: this repo is the product; the private `e2a-ops` repo owns
the hosted deployment (edge, IAM, backups, monitoring). That split defines
two compliance audiences and the plan must serve both:

- **The hosted service** (`agents.e2a.dev` / `api.e2a.dev`) is what actually
  signs BAAs and gets a SOC 2 report. Its audit scope spans this repo *plus*
  `e2a-ops` — infra controls (backups, disk encryption, network policy,
  production access) live there and are out of scope here except where this
  document assigns them.
- **Self-hosters** need e2a to be *compliance-capable*: the software must
  provide the technical controls (encryption, audit trail, retention, MFA)
  so an operator can pass their own audit. `docs/data-handling.md` already
  frames operator responsibilities honestly; this plan moves several items
  from "operator's problem" into the product.

Positioning: during build-out, **"compliance-capable software, plus a hosted
HIPAA mode under BAA."** We do not claim "HIPAA compliant" as a product
property while the controls are still landing — compliance is a property of
a deployment and its paperwork. Once the hosted controls are operating and
BAAs are available, the hosted product line can be positioned as **"e2a —
HIPAA-compliant email infrastructure for AI agents, available under BAA."**

## Where we stand

### Strengths worth presenting to an auditor as-is

- **Tenant isolation through a single choke point** — `resolveOwnedAgent`
  (`internal/httpapi/operations.go:193`) with 404-not-403 anti-enumeration,
  mirrored on WebSocket and trash paths, backed by ~55 authz test
  assertions.
- **Credential hygiene** — API keys stored as SHA-256 hashes only
  (`internal/identity/store.go:5582`), OAuth 2.1 with mandatory PKCE S256
  (`internal/oauth/provider.go:107`), DCR scope ceilings, fosite
  HMAC-signature token storage.
- **Log privacy by design** — `internal/logredact` (addresses reduced to
  domains, IPs truncated, subjects logged as length only) with a CI tripwire
  test, and a telemetry label-cardinality contract
  (`docs/observability.md`).
- **Field-level crypto already exists in one place** — DKIM private keys use
  AES-256-GCM envelope encryption with HKDF-derived KEKs
  (`internal/identity/dkimcrypt.go`). This is the pattern Phase 1
  generalizes.
- **Supply chain** — cosign keyless image signing with SLSA provenance,
  committed-credential CI gate, Dependabot.
- **Vulnerability disclosure** — `SECURITY.md` with response SLAs (directly
  citable for TSC CC7.4).
- **Data-subject rights** — `GET /v1/account/export` and
  `DELETE /v1/account?confirm=DELETE` exist and are tenant-scoped.

### Gap register

Severity is rated against the *hosted service offering a BAA-backed HIPAA
mode (and later SOC 2)*; several items are non-issues for a self-hoster who
supplies their own control.

| # | Sev | Gap | Anchor | Fixed in |
|---|-----|-----|--------|----------|
| G1 | Critical | Inbound content scan sends subject + sender + up to 4,000 chars of body text to the **Gemini AI Studio consumer endpoint** (`generativelanguage.googleapis.com`, bare API key). Not BAA-eligible, no DPA/residency path. Opt-in and off by default, but its mere availability is disqualifying for a HIPAA account. PR #920 makes the endpoint/auth configurable (`GEMINI_BASE_URL`, `GEMINI_AUTH=adc`) so deployments can route to Vertex AI under a Google Cloud BAA. | `internal/piguard/gemini.go:22,201-214` | P0 (vendor register), P3 (enforced) |
| G2 | Critical | No application-layer encryption at rest for bodies, attachments, headers, or raw MIME; only DKIM keys are field-encrypted. `docs/data-handling.md:47` delegates this to the operator. NPRM makes encryption at rest mandatory. | `migrations/001_init.sql:76-99` | P1 |
| G3 | Critical | No audit trail. Zero durable records of logins, key issuance/revocation, agent/domain lifecycle, protection-config changes, data reads/downloads/exports, or the acting principal on approve/reject. The only append-only table is `protection_events`. | absent; cf. `migrations/040_screening.sql:47` | P1 |
| G4 | Critical | No MFA, no roles, no teams — one user owns one account, identity fully delegated to Google/OIDC. TSC CC6.1–CC6.3 (logical access, provisioning, reviews) and the NPRM's MFA mandate are unimplementable as built. | `migrations/001_init.sql:6-12` | P1 |
| G5 | High | Indefinite retention of message bodies with no ceiling and no per-account knob (`migrations/072_indefinite_message_retention.sql` removed the old expiry). HIPAA §164.530(j) and TSC CC6.5 expect a defined disposal schedule. | `docs/data-handling.md:11` | P1 |
| G6 | High | No published privacy policy, ToS, subprocessor list, DPA, or BAA. The hosted product is marketed with no privacy notice. | `web/src/app/` (no `/privacy`, `/legal`) | P2 |
| G7 | High | No documented backup/DR/BCP, RPO/RTO, or restore testing (explicitly disclaimed). TSC A1.2–A1.3; HIPAA requires encrypted backups and tested recovery. | `docs/data-handling.md:45` | P1 (`e2a-ops`) |
| G8 | Med | Session tokens stored in plaintext — the token is the primary key of `user_sessions`. API keys are hashed; sessions are not. A read-only DB compromise is a session hijack. | `internal/identity/store.go:5975-5988` | P1 |
| G9 | Med | Webhook signing secrets (`whsec_`) stored plaintext (must be replayable, but should be envelope-encrypted like DKIM keys). | `internal/identity/webhooks.go:104` | P1 |
| G10 | Med | No SAST / dependency-vuln / container scanning in CI (Dependabot only). TSC CC7.1. | `.github/workflows/` | P1 |
| G11 | Med | Postgres TLS never enforced — every shipped example uses `sslmode=disable` and `env: production` does not require it, though it enforces TLS elsewhere. | `config.example.yaml:37`, `internal/config/config.go:603` | P1 |
| G12 | Med | `protection_events` retains literal content excerpts (`spans`) and LLM verdict text past message deletion (soft reference by design), with no retention bound. | `internal/piguard/piguard.go:163-169`, `migrations/046_rename_protection_events.sql:12` | P1 |
| G13 | Med | `GET /v1/account/export` is knowingly incomplete (contacts, engagements, templates, webhook config/history omitted). | `docs/data-handling.md:37` | P1 |
| G14 | Med | The 2026-08-08 committed-credential incident has a CI backstop (`scripts/check-no-committed-credentials.sh`) but no formal incident record, post-mortem, or IR policy behind it. TSC CC7.3–CC7.5 test the *process*, not the one-off fix. | `scripts/check-no-committed-credentials.sh:6-16` | P2 |
| G15 | Med | `E2A_HMAC_SECRET` rotation is impossible without silently orphaning every encrypted DKIM key (documented hazard, no re-key tooling). Key-rotation is a standard control ask. | `internal/identity/dkimcrypt.go:29-36` | P1 |
| G16 | Low | Unstructured `log.Printf` to stdout (~340 sites); no request/access log, no error tracker. Evidence collection for CC7.2 needs structured, queryable logs. | `internal/httpapi/httpapi.go:498` | P1 |
| G17 | Low | Attachment download URLs are unauthenticated 15-minute capability tokens not bound to a principal — fine generally, but a PHI-retrieval URL that lands in proxy logs. | `internal/httpapi/attachments.go:62-131` | P3 |
| G18 | Low | In-memory per-process rate limits multiply by replica count (documented). | `internal/ratelimit/ratelimit.go:8` | backlog |

## The plan

Architecture and PHI-boundary work comes first; data-plane hardening is
graded against it; compliance evidence then validates the architecture;
product exposure comes only after the controls exist; SOC 2 attestation runs
on its own track.

### Phase 0 — The PHI boundary: inventory, trust boundaries, isolation

No product code. The output is design documents that everything later is
built and graded against, and the direct input to Phase 2's Security Risk
Analysis. Target: 1–2 weeks.

1. **Canonical data-flow and storage inventory** (an ADR in `docs/design/`)
   covering the complete path:

   `SMTP ingress → intake/queue → workers → Postgres/backups →
   API/WebSocket/MCP/webhooks → customer systems`

   and every secondary copy and operational surface, including at minimum:
   - webhook outbox rows and delivery payloads (`internal/webhookpub`,
     `internal/webhookdelivery`, `internal/eventpayload`)
   - River job arguments and stored job rows (`internal/jobs`)
   - templates, contacts, and engagement records
   - message lifecycle evidence (`internal/messagelifecycle`)
   - idempotency stored responses (`internal/idempotency`)
   - `protection_events` content excerpts and LLM verdicts
   - Postgres WAL, replicas, and backups
   - process logs, metrics/telemetry, analytics (self-hosted Umami)
   - support tooling and operator access paths (`e2a-ops`)
   - upstream email providers (SES/SNS) and LLM APIs (piguard detectors)

   For each location, record: what data reaches it, why it is necessary,
   how long it persists, how it is encrypted, how it is deleted, who can
   access it, and which vendor/service/configuration receives it. This
   inventory is the checklist Phase 1's encryption/retention/deletion work
   is graded against — "every PHI-bearing copy" means every row of this
   inventory, not just the `messages` table.
2. **Trust-boundary map and an explicit isolation decision.** Decide how
   HIPAA accounts are isolated: a **dedicated HIPAA cell** (separate
   deployment, database, and key hierarchy) or **documented logical +
   cryptographic isolation** within the shared deployment (per-account
   encryption keys, row-level tenancy through the existing
   `resolveOwnedAgent` choke points, PHI-safe shared observability). A flag
   alone is not the isolation boundary — the account-level capability
   (Phase 3) selects *enforcement*, but isolation itself must be one of
   these two designs, recorded as an ADR with a blast-radius analysis.
3. **Vendor/service register.** For every external service the boundary
   touches, record the *exact service and the executed agreement* — never
   just that a vendor brand offers some HIPAA-eligible products. AWS: the
   specific services (SES, SNS, …) listed in the executed AWS BAA. Google:
   Vertex AI under the Google Cloud BAA — the AI Studio consumer endpoint is
   not coverable. PR #920 already makes the detector endpoint and auth
   configurable so the hosted deployment can route to Vertex; the register
   decides what is *approved*, and Phase 3 enforces it per account.

### Phase 1 — Data-plane hardening (this repo; the long pole)

Minimize the PHI boundary, then protect every copy that remains. Ordered so
each item is a separate PR train with its own migration + tests, per house
rules (idempotent, forward-only, non-destructive on prod-sized tables).
Items that don't depend on the Phase 0 inventory (4, 6, 7, 8) can start
immediately.

1. **Audit trail (G3).** New append-only `audit_events` table:
   `(id, occurred_at, actor_type [user|api_key|oauth_client|system],
   actor_id, acting_credential_id, event [enum], target_type, target_id,
   outcome, ip /24-truncated, metadata JSONB)`. Two event families:
   - **administrative** — `authenticatePrincipal` success/failure
     (`internal/agent/api.go:810`), API-key mint/revoke, session
     create/destroy, agent/domain create/delete, protection-config changes,
     webhook create/rotate/delete, approve/reject with acting principal,
     account deletion;
   - **data access** — message body reads, attachment downloads, and
     account exports, so PHI access is reconstructable, not just PHI
     administration.
   No message content, ever (`logredact` rules apply to metadata). Read
   surface: `GET /v1/audit-events` (account scope), included in account
   export. Retention: 6 years, configurable upward, never below the HIPAA
   documentation floor for hosted HIPAA accounts.
2. **PHI-safe observability (G16).** Migrate `log.Printf` → `log/slog`
   JSON, add a request/access-log middleware alongside the existing metrics
   middleware, keep `logredact` as the mandatory formatting layer, and
   extend the tripwire test beyond the subject rule. The minimization rule
   for the HIPAA path: logs and telemetry carry **opaque identifiers and
   operational state only** (`message_id`, `delivery_status`,
   `http_status`) — never subject, body, attachment data, raw addresses,
   customer-controlled metadata, URLs containing capability tokens, or
   unredacted provider errors. This is mechanical but wide; do it early so
   every later feature logs correctly.
3. **Envelope encryption for content at rest (G2).** Generalize
   `dkimcrypt`'s AES-256-GCM + HKDF pattern into `internal/fieldcrypt` with
   **key versioning, a re-key job** (which also retires G15's rotation
   hazard), and **per-account key separation**: per-account DEKs wrapped by
   the KEK, giving the hosted service cryptographic tenant separation and a
   **crypto-erasure path** — destroying an account's keys renders its rows
   in live tables, replicas, WAL, and backups unreadable, which is the only
   credible secure-deletion story for backup media. Apply to every
   PHI-bearing copy the Phase 0 inventory identifies — at minimum
   `messages.body_text/body_html/attachments_json/raw_message`,
   `inbound_intake.raw_message`, `protection_events.spans/raw`, and webhook
   secrets (G9). Migration strategy: new nullable ciphertext columns,
   dual-read window, background re-encryption via River job, then cut
   over — no `ALTER COLUMN TYPE` on `messages`. Ship with
   `E2A_FIELD_ENCRYPTION=on|off` (default on for new installs; hosted
   forces on) so self-hosters with full-disk encryption can opt out.
   Explicitly out of scope: this protects against DB-snapshot/backup
   exposure, not against a compromised app server holding the KEK.
4. **Hash session tokens (G8).** Store SHA-256 of the session token,
   mirroring API-key handling; sessions are short-lived so migration is a
   dual-read window plus natural expiry.
5. **Retention and secure deletion (G5, G12).** Per-account
   `retention: {message_max_age_days, protection_event_max_age_days}` with
   a janitor sweep. Defaults stay "indefinite" for existing accounts
   (current documented behavior), but the knob must exist and HIPAA mode
   (Phase 3) requires a finite value. Deletion must have **defined behavior
   for every copy** in the Phase 0 inventory: live rows, queue/outbox rows,
   webhook delivery payloads, idempotency responses, lifecycle evidence,
   replicas and backups (via crypto-erasure from item 3), logs (bounded
   retention windows), and exports. `protection_events` rows gain a bounded
   lifetime and deletion coupling to their message where one exists. Update
   `docs/data-handling.md` per-copy in the same PRs.
6. **Postgres TLS (G11).** `env: production` refuses
   `sslmode=disable|allow|prefer` unless
   `E2A_DATABASE_TLS_OVERRIDE=insecure` is set (mirrors the existing
   production strictures for HTTPS webhooks and HMAC strength in
   `internal/config`). Fix every example DSN.
7. **CI security scanning (G10).** Add `govulncheck` + `gosec` (Go),
   CodeQL (repo-wide), Trivy (image), and `dependency-review` to
   `.github/workflows/`. Wire findings to the same fail-closed posture as
   the existing gates.
8. **MFA (part of G4).** TOTP enrollment + recovery codes on the dashboard
   account, WebAuthn next; add `mfa_verified` to sessions and a
   revoke-all-sessions endpoint. For OIDC-delegated sign-in, allow
   requiring `amr`/`acr` MFA claims from the IdP instead of double-prompting.
9. **Organizations and RBAC (rest of G4).** Multi-user accounts:
   `organizations`, `memberships(role)` with owner/admin/member/auditor
   roles; agents and domains re-homed to the org (migration keeps
   single-user accounts as single-member orgs, no behavioral change for
   existing users). Role checks land in the same `resolveOwnedAgent`-style
   choke points; the auditor role is read-only + audit-event access, which
   is what an access reviewer and an external auditor both need. Pair with
   **least-privilege operator access** on the hosted side (`e2a-ops`).
   This is the largest product change in the plan and is also plain
   enterprise-feature work — schedule it on product merit with compliance
   as a forcing function.
10. **Backups/DR (G7).** Encrypted backups, defined RPO/RTO for the hosted
    Postgres, scheduled restore tests, documented BCP. Owner: `e2a-ops`;
    this repo's contribution is documenting what a deployment must provide
    (`docs/deployment.md` section).
11. **Complete the export (G13).** Extend `/v1/account/export` schema to v5
    with the omitted resources.

### Phase 2 — Compliance evidence: SRA, policies, agreements, training

Evidence follows and validates the architecture — this phase is performed
*against* the Phase 0 inventory, not from a generic template. All of it must
be complete **before the first BAA customer**:

1. **Formal Security Risk Analysis** (§164.308(a)(1)) over the Phase 0
   data-flow inventory and trust-boundary map, tracked privately (a
   compliance platform — Vanta, Drata, or Secureframe — is the natural
   evidence store; all three cross-map SOC 2 ↔ HIPAA) and reviewed on a
   defined cadence.
2. **Policy set adopted**: information security, access control, change
   management, incident response and breach notification, backup/recovery,
   workforce security, data retention, vendor management.
3. **Incident-response record for 2026-08-08 (G14)** — timeline, impact
   assessment (key was inert), remediation (history handling, CI gate),
   lessons. This converts an awkward grep-finding into evidence that the IR
   process works. Adopt the IR policy it exemplifies, including HIPAA
   breach-notification duties (business associates notify the covered
   entity without unreasonable delay, 60-day outer bound) as a runbook in
   `docs/runbooks/`.
4. **Executed agreements.** Work through the Phase 0 vendor register:
   execute the infrastructure/subprocessor BAAs, verifying the *exact
   service and executed agreement* covers the workload (e.g. SES and SNS
   listed under the hosted account's AWS BAA; Vertex AI under the Google
   Cloud BAA if LLM screening stays on for HIPAA accounts). Prepare the e2a
   **customer BAA** template offered with the hosted HIPAA plan.
5. **Legal surface (G6)**: privacy policy, terms of service, DPA, and the
   subprocessor list (which falls out of the register) published on the
   website. Nothing blocks shipping the privacy policy and ToS earlier —
   they are overdue independent of HIPAA.
6. **Training** for everyone with access to the HIPAA boundary, with
   completion records.

### Phase 3 — Enforced HIPAA product mode

Productize only after the controls exist. An account-level capability
(`account.hipaa_enabled = true`, set at plan purchase together with a signed
customer BAA — the Stripe-tier hook in
`docs/design/2026-06-01-stripe-tier-webhooks.md` already anticipates "HIPAA /
data-residency commitments" as a tier trigger) that **automatically
requires**, enforced at call sites rather than in the settings UI, failing
closed:

- **TLS with no plaintext downgrade, everywhere.** Outbound SMTP is
  TLS-required — a recipient MX that cannot negotiate TLS gets a failed
  delivery, never a downgrade (production already sets `require_tls` in
  `internal/outbound/smtp_relay.go:223`; HIPAA mode removes every pathway
  to weaken it). TLS likewise required for APIs, webhooks,
  database/internal connections, and administrative access.
- **Approved HIPAA infrastructure and subprocessors only**, per the Phase 0
  register; the feedback→GitHub channel is disabled for content originating
  from these accounts.
- **LLM screening disabled unless the exact provider, service,
  model/feature, account configuration, and contract are approved** for the
  workload (G1) — checked at the detector call site. With PR #920 the only
  approvable configuration today is Vertex AI under the Google Cloud BAA
  (`GEMINI_BASE_URL` + `GEMINI_AUTH=adc`); absent an approved entry in the
  register, HIPAA accounts run heuristics-only.
- **PHI-safe logging, monitoring, analytics, and error handling** — the
  Phase 1 minimization rules become mandatory invariants, not defaults.
- **MFA for every human principal**, least-privilege RBAC, and the audit
  trail (including data reads/downloads/exports) always on with ≥6-year
  retention.
- **Finite retention required**, secure deletion across all copies,
  crypto-erasure on account deletion, encrypted backups with tested
  recovery.
- **Attachment URLs** switch to principal-bound, single-use tokens (G17).

A **secure-message portal** — notification email followed by authenticated
portal retrieval, for customers who need stronger delivery guarantees to
human recipients — is a valuable later enhancement, explicitly *not*
required for the initial HIPAA offering.

### Phase 4 — SOC 2 on its own commercial timeline

Not a gate for the HIPAA offering; scheduled against enterprise demand. By
this point Phases 0–3 have built and evidenced most of the control surface.

1. Readiness assessment against SOC 2 **Security + Confidentiality +
   Availability** (Processing Integrity and Privacy deferred; Availability
   included because uptime is the product promise).
2. External penetration test (auditors expect one; scope: `/v1`, OAuth,
   SMTP intake, webhook egress).
3. **SOC 2 Type I** (point-in-time) as soon as Phase 1 lands — typically
   8–12 weeks after readiness; unblocks enterprise deals immediately.
4. Start the Type II observation window (3–6 months of evidence), then the
   **Type II report**, issued as a **SOC 2+ with HIPAA mapping** so one
   audit serves both frameworks.
5. Budget expectation: roughly $15k–40k for a first Type II at this size,
   plus the platform subscription and pen test.

## Control mapping (abbreviated)

| Framework requirement | Where it lands |
|---|---|
| §164.308(a)(1) security risk analysis | P2 SRA over the P0 inventory |
| TSC CC6.1–CC6.3 logical access, provisioning, reviews | P1 MFA, RBAC + auditor role, audit events |
| TSC CC6.5 / HIPAA §164.530(j) disposal | P1 retention + per-copy secure deletion, crypto-erasure |
| TSC CC7.1 vuln management | P1 CI scanning + existing Dependabot |
| TSC CC7.2 monitoring / §164.312(b) audit controls | P1 audit trail (admin + data access) + PHI-safe logs |
| TSC CC7.3–CC7.5 incident mgmt / §164.410 breach notification | P2 IR policy, record, runbook |
| TSC A1.2–A1.3 availability | P1 backups/DR (`e2a-ops`), existing probers |
| §164.312(a) access control / §164.312(d) authentication | Existing key/OAuth design + P1 MFA + P1 session hashing |
| §164.312(e) transmission security | P3 no-downgrade TLS + P1 Postgres TLS |
| §164.312(a)(2)(iv) & NPRM encryption at rest | P1 field encryption (per-account keys) |
| §164.308(b) / §164.314 BAAs with subprocessors | P0 vendor register + P2 executed BAAs |
| §164.316(b) six-year documentation retention | P1 audit-event retention + P2 policy archive |

## What we deliberately do not do

- **No content-based PHI detection.** We never scan a message to decide
  whether it is regulated; the account capability defines the boundary
  (see governing principle). Scanning-as-classifier is both unreliable and
  itself a disclosure risk.
- **No conduit-exception claims.** e2a persists content; we are a business
  associate when PHI flows. Marketing must never suggest otherwise.
- **No HITRUST / ISO 27001 in this cycle.** SOC 2 + HIPAA mapping covers the
  demand we see; add frameworks only against a named deal.
- **No end-to-end (S/MIME/PGP) message encryption.** Out of scope; at-rest
  envelope encryption + no-downgrade TLS in transit is the bar both
  frameworks set. The Phase 3 secure-message portal is the eventual answer
  for guaranteed-confidential delivery to human recipients, not message
  cryptography.
- **No "HIPAA compliant" badge on the OSS artifact.** Self-hosters get
  compliance-*capable* software and documentation; their deployment and
  paperwork are their own. The hosted service earns the "available under
  BAA" positioning only once the controls are operating and BAAs are
  executed.

## Sequencing summary

| Phase | Contents | Owner | Exit criterion |
|---|---|---|---|
| P0 | Data-flow/storage inventory, trust boundaries + isolation decision, vendor/service register | founders + this repo (ADRs) | ADRs merged; register adopted |
| P1 | Field encryption (per-account keys, crypto-erasure), audit trail incl. data access, PHI-safe observability, retention + secure deletion, session hashing, PG TLS, CI scanning, MFA, orgs/RBAC, backups/DR, export v5 | this repo + `e2a-ops` | All P1 G-items closed with tests |
| P2 | SRA, policy set, IR record + runbook, executed subprocessor BAAs, customer BAA template, legal pages, training | founders | SRA complete; BAAs executed; policies adopted |
| P3 | Enforced `hipaa_enabled` mode + BAA purchase flow; later: secure-message portal | this repo + `e2a-ops` | First customer BAA signable |
| P4 | SOC 2 readiness, pen test, Type I, Type II observation, SOC 2+ report | external | Report issued |

P0 is short and comes first — Phases 1 and 2 are graded against its
inventory. P1 items that don't depend on the inventory (session hashing,
PG TLS, CI scanning, MFA) can start immediately; P2 runs in parallel with P1
once the inventory exists. P3 requires P0–P2 complete. P4 runs whenever the
commercial timeline wants it, any time after P1 is emitting evidence.

## References

- HHS OCR, *Guidance on HIPAA & Cloud Computing* (business-associate status
  of storage services; conduit exception scope).
- HHS, *HIPAA Security Rule NPRM*, 90 FR 898 (Jan 6, 2025) — proposed
  mandatory encryption, MFA, and annual BA verification.
- AICPA *Trust Services Criteria* (2017, rev. 2022).
- 45 CFR §164.308(a)(1) (security risk analysis), §164.312 (technical
  safeguards), §164.316 (documentation, six-year retention), §164.410
  (BA breach notification).
- In-tree: `docs/data-handling.md`, `SECURITY.md`,
  `docs/design/2026-06-01-stripe-tier-webhooks.md` (HIPAA tier trigger),
  `internal/identity/dkimcrypt.go` (the crypto pattern P1 generalizes),
  PR #920 (BAA-coverable detector endpoint).
