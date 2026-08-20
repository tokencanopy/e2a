# SOC 2 / HIPAA compliance plan

**Status:** proposed
**Date:** 2026-08-19

This document is the roadmap for making e2a auditable under SOC 2 and usable
by HIPAA-regulated customers. It is grounded in a file-level review of the
current tree: every gap below carries a code anchor, and the plan is phased so
each phase is independently shippable and independently valuable.

## Why

Two commercial forces point at the same engineering work:

1. **SOC 2 Type II** is the de-facto gate for selling any hosted service to
   enterprises. Auditors test controls against the AICPA Trust Services
   Criteria (TSC) over a 3–12 month observation window; the controls have to
   exist and emit evidence before the window can even start.
2. **HIPAA** applies the moment a covered entity (or another business
   associate) routes protected health information (PHI) through an agent
   inbox. e2a stores message bodies, attachments, and raw MIME persistently
   in Postgres (`migrations/001_init.sql`, `migrations/056_inbound_intake.sql`),
   so the "mere conduit" exception is unavailable — HHS OCR guidance is
   explicit that a service which *maintains* ePHI is a business associate
   even if it never looks at the content. Serving healthcare customers means
   signing Business Associate Agreements (BAAs) and meeting the Security
   Rule's administrative, physical, and technical safeguards.

One regulatory deadline sharpens this: the HHS **Security Rule NPRM
(January 6, 2025)** proposes making encryption at rest/in transit, MFA,
network segmentation, and annual business-associate verification *mandatory*
(no more "addressable" outs). Final action is currently targeted for 2027
with a ~240-day compliance runway. Building to the NPRM bar now means the
final rule is a non-event.

The two frameworks overlap heavily (compliance platforms cross-map ~65% of
controls), so this is one program with two attestation outputs, not two
programs.

## Scope and the open-core boundary

e2a is open-core: this repo is the product; the private `e2a-ops` repo owns
the hosted deployment (edge, IAM, backups, monitoring). That split defines
two compliance audiences and the plan must serve both:

- **The hosted service** (`agents.e2a.dev` / `api.e2a.dev`) is what actually
  gets a SOC 2 report and signs BAAs. Its audit scope spans this repo *plus*
  `e2a-ops` — infra controls (backups, disk encryption, network policy,
  production access) live there and are out of scope here except where this
  document assigns them.
- **Self-hosters** need e2a to be *compliance-capable*: the software must
  provide the technical controls (encryption, audit trail, retention, MFA)
  so an operator can pass their own audit. `docs/data-handling.md` already
  frames operator responsibilities honestly; this plan moves several items
  from "operator's problem" into the product.

Positioning target: **"compliance-capable software, plus a hosted HIPAA mode
under BAA."** We do not claim "HIPAA compliant" as a product property —
compliance is a property of a deployment and its paperwork.

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

Severity is rated against the *hosted service seeking SOC 2 + BAA*; several
items are non-issues for a self-hoster who supplies their own control.

| # | Sev | Gap | Anchor | Fixed in |
|---|-----|-----|--------|----------|
| G1 | Critical | Inbound content scan sends subject + sender + up to 4,000 chars of body text to the **Gemini AI Studio consumer endpoint** (`generativelanguage.googleapis.com`, bare API key). Not BAA-eligible, no DPA/residency path. Opt-in and off by default, but its mere availability is disqualifying for a HIPAA account. | `internal/piguard/gemini.go:22,201-214` | P3 (gated), P0 (vendor decision) |
| G2 | Critical | No application-layer encryption at rest for bodies, attachments, headers, or raw MIME; only DKIM keys are field-encrypted. `docs/data-handling.md:47` delegates this to the operator. NPRM makes encryption at rest mandatory. | `migrations/001_init.sql:76-99` | P1 |
| G3 | Critical | No audit trail. Zero durable records of logins, key issuance/revocation, agent/domain lifecycle, protection-config changes, or the acting principal on approve/reject. The only append-only table is `protection_events`. | absent; cf. `migrations/040_screening.sql:47` | P1 |
| G4 | Critical | No MFA, no roles, no teams — one user owns one account, identity fully delegated to Google/OIDC. TSC CC6.1–CC6.3 (logical access, provisioning, reviews) and the NPRM's MFA mandate are unimplementable as built. | `migrations/001_init.sql:6-12` | P1 (MFA), P2 (RBAC) |
| G5 | High | Indefinite retention of message bodies with no ceiling and no per-account knob (`migrations/072_indefinite_message_retention.sql` removed the old expiry). HIPAA §164.530(j) and TSC CC6.5 expect a defined disposal schedule. | `docs/data-handling.md:11` | P1 |
| G6 | High | No published privacy policy, ToS, subprocessor list, DPA, or BAA. The hosted product is marketed with no privacy notice. | `web/src/app/` (no `/privacy`, `/legal`) | P0 |
| G7 | High | No documented backup/DR/BCP, RPO/RTO, or restore testing (explicitly disclaimed). TSC A1.2–A1.3. | `docs/data-handling.md:45` | P0 (`e2a-ops`) |
| G8 | Med | Session tokens stored in plaintext — the token is the primary key of `user_sessions`. API keys are hashed; sessions are not. A read-only DB compromise is a session hijack. | `internal/identity/store.go:5975-5988` | P1 |
| G9 | Med | Webhook signing secrets (`whsec_`) stored plaintext (must be replayable, but should be envelope-encrypted like DKIM keys). | `internal/identity/webhooks.go:104` | P1 |
| G10 | Med | No SAST / dependency-vuln / container scanning in CI (Dependabot only). TSC CC7.1. | `.github/workflows/` | P1 |
| G11 | Med | Postgres TLS never enforced — every shipped example uses `sslmode=disable` and `env: production` does not require it, though it enforces TLS elsewhere. | `config.example.yaml:37`, `internal/config/config.go:603` | P1 |
| G12 | Med | `protection_events` retains literal content excerpts (`spans`) and LLM verdict text past message deletion (soft reference by design), with no retention bound. | `internal/piguard/piguard.go:163-169`, `migrations/046_rename_protection_events.sql:12` | P1 |
| G13 | Med | `GET /v1/account/export` is knowingly incomplete (contacts, engagements, templates, webhook config/history omitted). | `docs/data-handling.md:37` | P1 |
| G14 | Med | The 2026-08-08 committed-credential incident has a CI backstop (`scripts/check-no-committed-credentials.sh`) but no formal incident record, post-mortem, or IR policy behind it. TSC CC7.3–CC7.5 test the *process*, not the one-off fix. | `scripts/check-no-committed-credentials.sh:6-16` | P0 |
| G15 | Med | `E2A_HMAC_SECRET` rotation is impossible without silently orphaning every encrypted DKIM key (documented hazard, no re-key tooling). Key-rotation is a standard control ask. | `internal/identity/dkimcrypt.go:29-36` | P1 |
| G16 | Low | Unstructured `log.Printf` to stdout (~340 sites); no request/access log, no error tracker. Evidence collection for CC7.2 needs structured, queryable logs. | `internal/httpapi/httpapi.go:498` | P1 |
| G17 | Low | Attachment download URLs are unauthenticated 15-minute capability tokens not bound to a principal — fine generally, but a PHI-retrieval URL that lands in proxy logs. | `internal/httpapi/attachments.go:62-131` | P3 |
| G18 | Low | In-memory per-process rate limits multiply by replica count (documented). | `internal/ratelimit/ratelimit.go:8` | backlog |

## The plan

### Phase 0 — Program, paper, and vendors (no product code)

The cheapest phase and the one that unblocks sales conversations. Target:
2–4 weeks of focused effort.

1. **Stand up a compliance program** on an automation platform (Vanta, Drata,
   or Secureframe — pick in week 1; all three cross-map SOC 2 ↔ HIPAA).
   Write and adopt the core policy set: information security, access
   control, change management, incident response, vendor management,
   business continuity, data retention. Run the first formal risk
   assessment.
2. **Publish the legal surface** on the website: privacy policy, terms of
   service, subprocessor list, and a DPA. Draft the BAA template (offered
   under the hosted HIPAA plan, Phase 3). The subprocessor list today is
   short and knowable from code: AWS (SES/SNS), Google (OAuth sign-in;
   Gemini *only if scanning is enabled*), GitHub (feedback issues),
   self-hosted Umami (path-only analytics), UptimeRobot/GCP monitoring.
3. **Vendor decisions that gate later phases:**
   - **AWS**: sign the AWS BAA for the hosted account; SES and SNS are
     HIPAA-eligible services. Confine hosted PHI-bearing workloads to
     BAA-covered services. (Owner: `e2a-ops`.)
   - **Gemini (G1)**: decide the screening provider's future. Options, in
     order of preference: (a) move the LLM detector to a BAA-coverable
     endpoint (Vertex AI under Google Cloud BAA, or another provider that
     signs BAAs with zero-retention API terms); (b) keep AI Studio for
     non-HIPAA accounts and hard-disable the detector for HIPAA accounts
     (Phase 3 does this regardless, as defense in depth); (c) heuristics
     only. Until resolved, document the current data flow in the
     subprocessor list verbatim — subject, sender, 4,000 chars of body.
4. **Write the incident-response record for 2026-08-08 (G14)** — timeline,
   impact assessment (key was inert), remediation (history handling, CI
   gate), lessons. This converts an awkward grep-finding into evidence that
   the IR process works. Adopt the IR policy it exemplifies, including
   HIPAA breach-notification duties (business associates notify the covered
   entity without unreasonable delay, 60-day outer bound) as a runbook in
   `docs/runbooks/`.
5. **Backups/DR (G7)** — define RPO/RTO for the hosted Postgres, automate
   backups, schedule restore tests, and document the BCP. Owner:
   `e2a-ops`; this repo's contribution is documenting what a deployment
   must provide (`docs/deployment.md` section).
6. **Scope statement for auditors** — a short doc mapping the system
   boundary: this repo (application controls) vs `e2a-ops` (infrastructure
   controls) vs inherited controls (cloud provider attestations).

### Phase 1 — Platform hardening (this repo; the long pole)

Ordered so each item is a separate PR train with its own migration + tests,
per house rules (idempotent, forward-only, non-destructive on prod-sized
tables).

1. **Audit trail (G3).** New append-only `audit_events` table:
   `(id, occurred_at, actor_type [user|api_key|oauth_client|system],
   actor_id, acting_credential_id, event [enum], target_type, target_id,
   outcome, ip /24-truncated, metadata JSONB)`. Emit from the existing choke
   points — `authenticatePrincipal` success/failure
   (`internal/agent/api.go:810`), API-key mint/revoke, session
   create/destroy, agent/domain create/delete, protection-config changes,
   webhook create/rotate/delete, **approve/reject with acting principal**,
   account deletion, export invocation. No message content, ever
   (`logredact` rules apply to metadata). Read surface:
   `GET /v1/audit-events` (account scope), included in account export.
   Retention: 6 years, configurable upward, never below the HIPAA
   documentation floor for hosted HIPAA accounts.
2. **Structured logging (G16).** Migrate `log.Printf` → `log/slog` JSON,
   add a request/access-log middleware alongside the existing metrics
   middleware, keep `logredact` as the mandatory formatting layer, and
   extend the tripwire test beyond the subject rule. This is mechanical but
   wide; do it early so every later feature logs correctly.
3. **Envelope encryption for content at rest (G2).** Generalize
   `dkimcrypt`'s AES-256-GCM + HKDF pattern into `internal/fieldcrypt` with
   **key versioning and a re-key job** (which also retires G15's rotation
   hazard). Apply to: `messages.body_text/body_html/attachments_json/
   raw_message`, `inbound_intake.raw_message`, `protection_events.spans/raw`,
   and webhook secrets (G9). Migration strategy: new nullable ciphertext
   columns, dual-read window, background re-encryption via River job,
   then cut over — no `ALTER COLUMN TYPE` on `messages`. Ship with
   `E2A_FIELD_ENCRYPTION=on|off` (default on for new installs; hosted
   forces on) so self-hosters with full-disk encryption can opt out.
   Explicitly out of scope: this protects against DB-snapshot/backup
   exposure, not against a compromised app server holding the KEK.
4. **Hash session tokens (G8).** Store SHA-256 of the session token,
   mirroring API-key handling; sessions are short-lived so migration is a
   dual-read window plus natural expiry.
5. **Retention controls (G5, G12).** Per-account
   `retention: {message_max_age_days, protection_event_max_age_days}` with
   a janitor sweep. Defaults stay "indefinite" for existing accounts
   (current documented behavior), but the knob must exist, HIPAA mode
   (Phase 3) requires a finite value, and `protection_events` rows gain a
   bounded lifetime and deletion coupling to their message where one exists.
   Update `docs/data-handling.md` retention table in the same PRs.
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
9. **Complete the export (G13).** Extend `/v1/account/export` schema to v5
   with the omitted resources.

### Phase 2 — Organizations and RBAC (G4)

Multi-user accounts: `organizations`, `memberships(role)` with
owner/admin/member/auditor roles; agents and domains re-homed to the org
(migration keeps single-user accounts as single-member orgs, no behavioral
change for existing users). Role checks land in the same
`resolveOwnedAgent`-style choke points; the auditor role is read-only +
audit-event access, which is what an access reviewer and an external auditor
both need. Quarterly access-review evidence then falls out of
`GET /v1/audit-events` + membership listings. This is the largest product
change in the plan and is also plain enterprise-feature work — it should be
scheduled on product merit with compliance as a forcing function, not built
solely for the audit.

### Phase 3 — Hosted HIPAA mode

A per-account `compliance_profile: hipaa` (account-scope, set at plan
purchase together with a signed BAA) that *enforces invariants rather than
trusting configuration*:

- **LLM screening hard-off** (or restricted to the BAA-covered provider
  chosen in Phase 0) regardless of `E2A_CONTENT_SCAN_ENABLED` and per-agent
  settings — checked at the detector call site, not the settings UI (G1).
- **Field encryption mandatory**, finite retention policy required (G2, G5).
- **MFA required** for every human principal in the org (G4/NPRM).
- **Audit trail + 6-year retention** always on (G3).
- **Attachment URLs** switch to principal-bound, single-use tokens (G17).
- **Subprocessor routing pinned** to BAA-covered services; the feedback→
  GitHub channel is disabled for content originating from these accounts.
- Outbound TLS is already `require_tls` in production
  (`internal/outbound/smtp_relay.go:223`); document that plain-SMTP
  recipients are the covered entity's risk decision, as HHS permits with
  individuals' consent for their own mail, and keep the refusal to
  downgrade agent-to-agent traffic.

Plus the paperwork wiring: BAA signature flow attached to the plan tier,
breach-notification runbook rehearsed, and the `docs/design/`
Stripe-tier hook (`2026-06-01-stripe-tier-webhooks.md` already anticipates
"HIPAA / data-residency commitments" as a tier trigger).

### Phase 4 — Attestation

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
| TSC CC6.1–CC6.3 logical access, provisioning, reviews | P1 MFA, P2 RBAC + auditor role, audit events |
| TSC CC6.5 / HIPAA §164.530(j) disposal | P1 retention controls |
| TSC CC7.1 vuln management | P1 CI scanning + existing Dependabot |
| TSC CC7.2 monitoring / §164.312(b) audit controls | P1 audit trail + structured logs |
| TSC CC7.3–CC7.5 incident mgmt / §164.410 breach notification | P0 IR policy, record, runbook |
| TSC A1.2–A1.3 availability | P0 backups/DR (`e2a-ops`), existing probers |
| §164.312(a) access control / §164.312(d) authentication | Existing key/OAuth design + P1 MFA + P1 session hashing |
| §164.312(e) transmission security | Existing require_tls + P1 Postgres TLS |
| §164.312(a)(2)(iv) & NPRM encryption at rest | P1 field encryption |
| §164.308(b) / §164.314 BAAs with subprocessors | P0 vendor program (AWS BAA, Gemini decision) |
| §164.316(b) six-year documentation retention | P1 audit-event retention + policy archive |

## What we deliberately do not do

- **No conduit-exception claims.** e2a persists content; we are a business
  associate when PHI flows. Marketing must never suggest otherwise.
- **No HITRUST / ISO 27001 in this cycle.** SOC 2 + HIPAA mapping covers the
  demand we see; add frameworks only against a named deal.
- **No end-to-end (S/MIME/PGP) message encryption.** Out of scope; at-rest
  envelope encryption + TLS in transit is the bar both frameworks set.
- **No "HIPAA compliant" badge on the OSS artifact.** Self-hosters get
  compliance-*capable* software and documentation; their deployment and
  paperwork are their own.

## Sequencing summary

| Phase | Contents | Owner | Exit criterion |
|---|---|---|---|
| P0 | Policies, legal pages, vendor BAAs, IR record, backups/DR, scope doc | founders + `e2a-ops` | Platform onboarded, policies adopted, legal pages live |
| P1 | Audit trail, slog, field encryption + re-key, session hashing, retention knobs, PG TLS, CI scanning, MFA, export v5 | this repo | All G-items marked P1 closed with tests |
| P2 | Orgs/RBAC/access reviews | this repo | Auditor role usable for an access review |
| P3 | HIPAA compliance profile + BAA flow | this repo + `e2a-ops` | First BAA signable |
| P4 | Pen test, Type I, Type II observation, SOC 2+ report | external | Report issued |

P0 and P1 can run concurrently. Nothing in P1 blocks on P0 except the
Gemini vendor decision (item 3), which only gates the *final* wiring of the
screening path in P3.

## References

- HHS OCR, *Guidance on HIPAA & Cloud Computing* (business-associate status
  of storage services; conduit exception scope).
- HHS, *HIPAA Security Rule NPRM*, 90 FR 898 (Jan 6, 2025) — proposed
  mandatory encryption, MFA, and annual BA verification.
- AICPA *Trust Services Criteria* (2017, rev. 2022).
- 45 CFR §164.312 (technical safeguards), §164.316 (documentation,
  six-year retention), §164.410 (BA breach notification).
- In-tree: `docs/data-handling.md`, `SECURITY.md`,
  `docs/design/2026-06-01-stripe-tier-webhooks.md` (HIPAA tier trigger),
  `internal/identity/dkimcrypt.go` (the crypto pattern P1 generalizes).
