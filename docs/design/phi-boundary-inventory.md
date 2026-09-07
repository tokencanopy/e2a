# PHI boundary: data-flow inventory, trust boundaries, isolation decision

Status: **DRAFT for review** (2026-08-21). Phase 0 deliverable of the SOC 2 /
HIPAA compliance plan (PR #919), per the maintainer direction on that PR.
Companion to `docs/data-handling.md` (the operator-facing summary; this
document is the compliance-grade census behind it). All anchors are `file:line`
at `main` @ `3a606d6c` unless a branch is named.

Governing principle (from PR #919): **for a HIPAA-enabled account, every email
and its associated metadata is treated as potentially regulated data and routed
through a tightly controlled, fail-closed HIPAA data path.** This inventory is
the definition of "every" — the checklist Phase 1's encryption, retention,
deletion, and observability work is graded against, and the direct input to
Phase 2's Security Risk Analysis. A copy not listed here is a copy the controls
miss, so additions to this file are part of the definition of done for any
change that creates a new place message data can live.

Each location records: what data reaches it, why it is necessary, how long it
persists, how it is encrypted, how it is deleted, and who can access it
(external vendors additionally: which exact service and configuration receives
it — §6). "Content" below means message bodies / raw MIME / attachment bytes;
"metadata" means addresses, subjects, headers, verdicts, timestamps, and
identifiers. Under the governing principle both classes are regulated on the
HIPAA path — metadata copies are inventoried with the same rigor as content
copies.

---

## 1. Canonical data flow

```
sender MTA ──SMTP──▶ relay (:2525, direct MX)
                       │  sync mode: parse/auth/screen inline
                       │  async mode: raw MIME ▶ inbound_intake ▶ River ▶ worker
                       ▼
                    Postgres (messages + secondary tables)
                       │
      ┌────────────────┼──────────────────┬──────────────┐
      ▼                ▼                  ▼              ▼
  REST API /       webhook outbox     process logs,   LLM screening
  WebSocket /      ▶ per-subscriber   metrics         (Gemini)
  MCP / export     deliveries ▶ customer HTTPS endpoints
      │
      ▼
  customer systems (agents, SDKs, saved exports)

outbound: API ▶ messages row ▶ River ▶ SES SMTP relay ▶ recipient MTA
feedback: SES ▶ SNS ▶ POST /webhooks/ses ▶ message_recipients / suppressions
```

Two structural facts shape everything below:

- **Postgres is the only content store.** Raw MIME, bodies, and attachment
  bytes live in Postgres rows — no S3/GCS, no filesystem spool, no temp files
  in the mail path (`docs/data-handling.md:14`, greps confirmed). The at-rest
  story is therefore exactly the Postgres story: live cluster, WAL, replicas,
  backups.
- **River job args never carry content.** Every job payload is a bare
  identifier (`internal/inboundprocess/worker.go:49-51`,
  `internal/outboundsend/worker.go:81-83`,
  `internal/webhookdelivery/worker.go:113-115`); workers re-read the row per
  attempt. The queue's PHI exposure is via *error strings*, not args (F7).

---

## 2. Storage inventory — content copies

| # | Location | What | Persists | Encrypted | Deleted by | Read by |
|---|----------|------|----------|-----------|-----------|---------|
| C1 | SMTP session buffer | full raw MIME, in process memory only (`internal/relay/server.go:338`, cap 10 MB `:170`) | duration of the SMTP transaction | in-transit STARTTLS only if certs configured (`:186-192`) | GC | relay process |
| C2 | `inbound_intake.raw_message` (async mode) | full raw MIME + recipient, envelope-from, HELO, remote IP, content hash (`migrations/056_inbound_intake.sql:19-34`) | `processed`: 72 h then pruned (`internal/inboundprocess/retention.go:13-17`); **`failed` / stranded `accepted`: forever** | none | prune sweep only; **no FK to users/agents — account deletion and agent purge do not touch it** | DB role only; no API surface |
| C3 | `messages.raw_message` / `body_text` / `body_html` / `attachments_json` | everything: inbound raw MIME, outbound composed MIME, held-draft bodies + base64 attachment bytes (`internal/identity/store.go:434-458`, `:3556-3573`) | **indefinitely** since migration 072 (terminal-state scrub removed in the same commit, `6bd33923`); trash purge at `deleted_at` + 30 d is the only automatic clock (`internal/identity/store.go:5220-5245`) | none at the application layer (`docs/data-handling.md:47`) | trash purge, delete-forever, agent permanent purge, account deletion | owning user via REST (raw + parsed + draft body, `internal/httpapi/messages.go:110-124`), dashboard, MCP `get_message`, export; any DB-level operator |
| C4 | `idempotency_keys.response_body` | exact response bytes of the first send/reply call — includes subject, addresses, held-draft body (`migrations/015_idempotency_and_send_attempts.sql:57-71`) | 24 h (`internal/idempotency/store.go:42`), hourly sweep | none | sweep; user-delete cascade | replaying caller; DB role |
| C5 | `protection_events.raw` / `spans` | per-detector forensic JSON: Gemini's free-text rationale over the message (or first 200 runes of an unparseable model response, `internal/piguard/gemini.go:143-181`); `spans.Text` is a literal body excerpt when a span-producing provider is wired (schema-live, unpopulated today, `internal/piguard/piguard.go:161-169`); plus `subject_addr` — the counterparty address (`migrations/040_screening.sql:48-63`) | **no TTL; deliberately outlives the message** (soft ref, no FK — `migrations/040_screening.sql:11-13`); no janitor sweep | none | only agent-hard-delete cascade (`migrations/046_rename_protection_events.sql:5-9`) | review API (verdict projection); export omits `raw`/`spans`/`categories`; DB role sees all |
| C6 | account export response | complete PHI copy: all messages with raw MIME, bodies, attachment bytes (`internal/identity/user_data_rights.go:512-531`) | not persisted server-side — assembled in memory, streamed | TLS in transit | n/a | account-scoped bearer only; the copy lives wherever the caller saves it |
| C7 | Postgres WAL / replicas / backups | byte-level copies of every row above | operator-defined; **e2a ships no backup story** (`docs/data-handling.md:45`) and no encryption/retention defaults | operator-defined | operator-defined; crypto-erasure is the only credible per-account deletion story here (plan Phase 1 item 3) | hosting provider + operators (`e2a-ops`) |

Why each exists: C1/C2 are the durability boundary (async mode returns `250`
only after the raw bytes are durable); C3 is the product — an agent inbox with
history; C4 is exactly-once send semantics; C5 is the security forensic trail;
C6 is the GDPR right of access; C7 is physics.

## 3. Storage inventory — metadata copies

| # | Location | What | Persists | Deleted by | Notes |
|---|----------|------|----------|-----------|-------|
| M1 | `webhook_events.envelope` | full event JSON: subject, all addresses (incl. bcc on `email.sent`), auth evidence, attachment metadata — deliberately no body (`internal/relay/server.go:783-800`, `internal/eventpayload/payloads.go:67-151`) | 30 d (`migrations/026_webhook_events.sql:103`); **stuck-`pending` rows retained forever by design** (`internal/webhookpub/outbox.go:176-223`) | janitor sweep; user-delete cascade | `message_id` is `ON DELETE SET NULL` — envelope survives message purge |
| M2 | `webhook_subscriber_deliveries.event_payload` | second full envelope copy, one per matched subscriber (`internal/webhookpub/fanout_core.go:152-192`) | 90 d (`migrations/027_retry_envelope_extension.sql:25`); redelivery/test insert fresh copies with new clocks (`internal/agent/replay_api.go:48-59`) | two-phase sweep | survives message deletion by design (`migrations/025_webhook_subscriber_deliveries.sql:17-24`) |
| M3 | `message_lifecycle_transitions` | recipient address per row + bounded evidence JSONB (sanitized auth verdicts, `smtp_detail` provider diagnostics ≤2 KiB — truncation, **not** redaction, `internal/messagelifecycle/model.go:22-52`) | lifetime of the message (FK cascade) | with message | inbound writes 2–3 rows per message in the persist tx |
| M4 | `message_recipients` / `send_attempts` / `suppressions` | every recipient address; SES `diagnosticCode` free text (remote-MTA responses routinely quote the recipient back); to/cc/bcc arrays; suppression reasons (`migrations/031_delivery_feedback.sql:41-70`, `migrations/015…:73-84`) | no independent TTL; recipients/attempts cascade with message, suppressions live until account deletion | cascades | `message_recipients.detail` is written **without** `SafeDiagnostic` (`internal/identity/delivery_store.go:267-269`) |
| M5 | `contacts` / `contact_engagements` / `templates` | counterparty addresses, display names, free-form metadata JSONB; user-authored template subject/body (PHI-capable free text) (`migrations/079…`, `081…`, `050…`) | account lifetime | API delete; user-delete cascade | **not included in the account export** (`docs/data-handling.md:36`) |
| M6 | `river_job.errors` | provider error text appended per failed attempt — outbound send errors wrap the raw SES/SMTP response incl. quoted recipient addresses (`internal/outboundsend/worker.go:577-604`, `internal/outbound/smtp_relay.go:75-78`; the 200-rune log truncation does **not** apply to the error returned to River) | River defaults: completed 24 h, discarded 7 d (no override configured, `internal/jobs/jobs.go:117-135`) | River job cleaner | args are IDs only (§1) |
| M7 | threading columns on `messages` | `conversation_id`, `thread_id`, `rfc_message_id_key` — no separate thread table, no full-text index anywhere (verified: no tsvector/trgm in migrations) | with message | with message | only content-adjacent query surface is a bounded subject filter (`internal/identity/filter_registry.go:89`) |

## 4. Egress and operational surfaces

| # | Surface | What crosses | Control today |
|---|---------|-------------|---------------|
| E1 | REST `GET …/messages/{id}` | raw MIME + parsed text/html + draft body | bearer; `resolveOwnedAgent` tenancy pinning; TLS at operator's proxy |
| E2 | WebSocket live-tail | `email.received` envelope (metadata, no body) | bearer in header (query-param form removed precisely because it leaked into logs, `internal/ws/handler.go:82-105`) |
| E3 | MCP tools | bodies via `get_message` (no raw MIME/bytes); attachment base64 ≤256 KiB inline | stateless proxy; bearer forwarded; no persistence |
| E4 | Attachment download URL | decoded attachment bytes | HMAC capability token binding `message_id\|index\|expiry`, 15-min TTL, **multi-use**, not principal-bound (`internal/httpapi/attachments.go:22-73`; plan gap G17); no bearer on the download route; token never stored server-side and e2a has no HTTP access-log middleware — but it lands in operator proxy logs and agent transcripts |
| E5 | Customer webhook endpoints | full envelope (metadata) | HTTPS + SSRF guard in production (`internal/webhook/subscriber_deliverer.go:66-96, 158-160`); HMAC signing secret stored **plaintext** (`migrations/023_webhooks.sql:48-54`); one plaintext escape hatch `E2A_WEBHOOK_INTERNAL_SINK_URL` |
| E6 | Outbound SMTP to SES | full wire message | STARTTLS **opportunistic**; `require_tls` defaults on only in production and, when off, silently downgrades to cleartext (`internal/outbound/smtp_relay.go:225-234`); HIPAA mode requires no-downgrade unconditionally (plan Phase 3) |
| E7 | HITL approval email to the account owner | agent address, To/Cc/Bcc lists, subject, magic-link tokens in URLs — body deliberately excluded (`internal/hitlnotify/notifier.go:152-160, 358-366`) | same SES relay as E6; subject+recipients of a held message are themselves regulated on the HIPAA path |
| E8 | Process logs (stderr) | policy: subjects never (machine-checked, `internal/logredact/logguard_test.go:66`), external addresses → domain, IPs → /24//48; **but** resolved agent addresses logged in full by design, `logredact` is adopted in only 7 files, and raw provider/transport errors (which can quote recipient addresses) reach logs in `internal/outbound`, `internal/delivery`, `internal/webhook` | stdlib `log` to stderr — no structured logger, no levels; shipped to "centralized log storage with long retention" (`internal/logredact/logredact.go:1-29`) |
| E9 | Metrics | Prometheus, closed label vocabularies, series-capped; no addresses/subjects possible by construction (`internal/telemetry/prom.go:15-19, 85-93`); loopback-bound by default | low risk; keep the closed-vocabulary invariant |
| E10 | Umami analytics | public marketing pageviews only; allowlisted paths, URLs rebuilt origin+path, auto-track off (`web/src/app/components/UmamiTracker.tsx:36-152`) | outside the PHI boundary while the collector stays self-hosted and the allowlist excludes the dashboard |
| E11 | Feedback → GitHub + email | user free text (≤5000 runes) verbatim into a public issue, first 80 chars as the title, optional submitter email; session email auto-fill (`internal/agent/api.go:1698-1850`) | no BAA possible; **disabled for HIPAA accounts in Phase 3**. Same applies to the `agentify` loop, which copies filer message bodies verbatim into public issues (`plugins/e2a-labs/skills/agentify/templates/runtime-skill/triage.md:72-79`) |
| E12 | Operator access | no admin/impersonation endpoint exists in the app — operator reach is **direct Postgres** (`docs/data-handling.md:47`) plus the `e2a-ops` deploy VM | unlogged, unaudited today; Phase 1 items 1 and 9 |

Credential storage (who can *become* a reader): API keys SHA-256 hashed;
OAuth tokens stored as signatures; unsubscribe tokens hashed; DKIM private keys
AES-256-GCM under an HKDF-derived KEK with domain-bound AAD
(`internal/identity/dkimcrypt.go` — the pattern Phase 1 generalizes; note its
documented no-re-key caveat at `:30-38`); **session tokens plaintext**
(`migrations/001_init.sql:14-19`, plan gap G5); **webhook signing secrets
plaintext** (G6); SMTP/Gemini/GitHub credentials env-only, never in the DB.
Database transport: no TLS enforcement or `E2A_DATABASE_TLS*` knob exists; the
DSN is passed straight to pgxpool and every shipped example uses
`sslmode=disable` (`cmd/e2a/main.go:122`, `config.example.yaml:44`; G11).

## 5. Trust-boundary map and isolation decision

Trust zones, from least to most trusted:

1. **Internet** — sender MTAs, customer agents/SDKs, webhook receivers,
   browser. Crossings: SMTP :2525 (STARTTLS best-effort), HTTPS API/WS/MCP
   (bearer), SNS callback (signature + topic allow-list), attachment capability
   URLs (HMAC, unauthenticated route).
2. **Application processes** — relay, API, workers, MCP proxy. Hold plaintext
   content in memory; emit logs/metrics.
3. **Postgres** — the entire content plane (C2–C5, M1–M7) plus WAL. One
   database, one role; tenancy is row-level via `user_id`/`agent_id` enforced
   at the `resolveOwnedAgent` choke point in the application, not in the
   database.
4. **Substrate** — host VM, docker volumes, backups, replicas, centralized log
   storage. Operated from the private `e2a-ops` repo; invisible to this
   codebase.
5. **External vendors** — §6. Content crosses to exactly two: SES (full
   messages, necessarily) and the Gemini endpoint (subject + sender + up to
   4000 runes of body incl. extracted attachment text,
   `internal/piguard/gemini.go:200-214`). Everything else receives metadata or
   less.

**The isolation decision.** A flag alone is not the isolation boundary. Two
admissible designs:

- **Option A — dedicated HIPAA cell.** Separate deployment, database, key
  hierarchy, SES identities, and log storage; HIPAA accounts live only there.
  Blast radius: a compromise or operational error in the shared cell cannot
  reach HIPAA data *by construction*; observability separation is trivially
  fail-closed. Costs: a second production to patch, back up, monitor, and keep
  in config parity — duplicated toil that itself degrades the controls HIPAA
  cares about while the HIPAA customer count is small; account upgrade becomes
  a data migration.
- **Option B — documented logical + cryptographic isolation.** Shared
  deployment; isolation = (i) row tenancy through the existing single choke
  point, (ii) per-account DEKs wrapped by a KEK so every content column and
  PHI-bearing secondary copy of a HIPAA account is ciphertext to any bulk DB
  read, with crypto-erasure as the deletion story for WAL/replicas/backups,
  (iii) PHI-safe shared observability (opaque identifiers only), (iv)
  fail-closed enforcement at call sites (Phase 3). Blast radius: a stolen DSN
  or SQL injection yields ciphertext for encrypted columns — but only for
  columns the Phase 1 encryption actually covers, which is why §2–§3 is the
  grading checklist; the residual worst case is an application-layer authz bug
  (tenancy confusion) or KEK compromise, both shared-cell-wide.

**Proposal: Option B first, with named graduation triggers.** e2a's exposure
concentrates in Postgres; per-account envelope encryption buys most of what a
cell buys there, at a fraction of the operational surface — and a second cell
run by the same small team plausibly *weakens* patching, backup, and monitoring
discipline. Graduation to a dedicated cell (Option A) when any of: a signed
enterprise BAA customer requires physical separation; HIPAA-account volume
makes a cell economically sane; or the Phase 2 SRA rates the shared-cell
residual risk unacceptable. This is a proposal, not a decision — maintainer
sign-off on this section is the Phase 0 exit criterion, and the accepted
option (with this blast-radius analysis) becomes the ADR of record.

## 6. Vendor / service register

Approval is per **exact service + executed agreement**, never per brand. The
executed agreements themselves (counterparty, date, covered services) are
evidence, tracked privately in `e2a-ops`; this register records what each
service receives and what agreement is required before a HIPAA account exists.

| Vendor | Exact service / endpoint | Receives | Agreement required | HIPAA-path status |
|--------|--------------------------|----------|--------------------|--------------------|
| AWS | SES **SMTP submission** `email-smtp.us-east-2.amazonaws.com` (`internal/outbound/smtp_relay.go:176-302`) | full outbound messages | AWS BAA listing SES | required — not yet executed |
| AWS | **SESv2 API** (identity mgmt: `CreateEmailIdentity`, BYODKIM…, `internal/senderidentity/ses.go:27-33`) | customer domains + **customer DKIM private keys** | same AWS BAA | required — not yet executed |
| AWS | **SNS** delivery feedback → `POST /webhooks/ses` (`internal/delivery/`) | recipient addresses, bounce/complaint diagnostics; **original headers incl. subject if the config set enables it** | same AWS BAA; HIPAA config: keep "include original headers" off | required — not yet executed |
| Google | **Gemini API, AI Studio** `generativelanguage.googleapis.com` (`internal/piguard/gemini.go:22`) | subject + sender + ≤4000 runes body incl. extracted attachment text | none available — **not BAA-coverable** | never approvable; must be hard-off for HIPAA accounts |
| Google | **Vertex AI** `{region}-aiplatform.googleapis.com` (routable via PR #920's `GEMINI_BASE_URL` + `GEMINI_AUTH=adc`; unmerged) | same content | Google Cloud BAA covering Vertex AI | the only approvable LLM config; BAA not yet executed; hosted deployment not yet switched |
| Google | OAuth / OIDC userinfo (`internal/auth/auth.go:550-560`) | account email + name (sign-in identity, not message data) | n/a (workforce/customer auth) | acceptable; outside message path |
| GitHub | `api.github.com` issues (feedback, `internal/agent/api.go:1794`) | feedback free text + optional submitter email | none available | disabled for HIPAA accounts (Phase 3) |
| Stripe (via private billing sidecar) | sidecar endpoints; server egress is `{user_id}` only (`internal/agent/user_data_rights_api.go:108-136`) | no message data | standard DPA | outside PHI boundary; keep it that way |
| Umami (self-hosted) | operator's own collector | public marketing pageviews only | n/a (self-hosted) | outside boundary while self-hosted + allowlist holds |
| UptimeRobot / GCP Cloud Monitoring / Cloud Run (`e2a-ops`, `tests/sdk-monitor`) | uptime probes, synthetic mail | no customer content (verify in `e2a-ops`) | review during Phase 2 SRA | verify |
| Customer webhook endpoints | customer-controlled HTTPS URLs | event metadata (subject, addresses) | not a subprocessor — customer-directed disclosure; note in customer BAA | n/a |
| DNS resolvers | host resolver | queried names only (customer domains) | n/a | n/a |

Headline consequence, restated from the plan: the hosted deployment currently
sends inbound email content to the AI Studio consumer endpoint. Until the
Vertex switch is made and the Google Cloud BAA executed, LLM screening is
incompatible with a HIPAA account (heuristics-only is the fallback,
`internal/inboundscreen/inboundscreen.go:228`).

## 7. Findings register

New facts this census established, beyond the plan's G1–G18 gap register
(PR #919). These become Phase 1 work items; IDs continue the plan's numbering
convention as F-series (inventory findings).

- **F1 — failed intake rows are immortal and escape erasure.** `inbound_intake`
  rows in `failed` (or stranded `accepted`) state hold full raw MIME forever,
  have no FK to users/agents, and are untouched by account deletion and agent
  purge (`migrations/056_inbound_intake.sql`, `internal/inboundprocess/retention.go:16`).
  Fix: bound failed-row retention; include intake in erasure; encrypt the column.
- **F2 — the forensic trail stores LLM-derived content with no clock.**
  `protection_events.raw` carries the model's rationale about the message (and
  raw model output on parse failure) with no TTL and no per-row deletion; the
  `spans` column will carry literal excerpts if a span-producing provider is
  ever wired (`internal/piguard/gemini.go:143-181`, `migrations/040_screening.sql`).
  Fix: retention policy + encryption for `raw`/`spans`; on the HIPAA path,
  store verdict codes only.
- **F3 — stuck-pending outbox rows never expire.** `webhook_events` sweeps
  exclude `pending`, so a permanently-stuck event's subject + addresses are
  retained forever (`internal/webhookpub/outbox.go:176-223`). Fix: terminal
  cap on pending age.
- **F4 — envelope copies survive message deletion and escape the export.**
  `webhook_events` / `webhook_subscriber_deliveries` are `ON DELETE SET NULL`
  against messages and are absent from the account export
  (`docs/data-handling.md:36-37`). Fix: fold into deletion semantics and the
  export, or document precisely.
- **F5 — provider diagnostics are truncated, not redacted.** `SafeDiagnostic`
  is UTF-8 + length bounding only; remote-MTA text quoting recipient addresses
  lands in `message_recipients.detail` (that path skips even `SafeDiagnostic`),
  `messages.delivery_detail`, lifecycle evidence, and suppression reasons
  (`internal/messagelifecycle/model.go:22-37`, `internal/identity/delivery_store.go:267-269`).
- **F6 — logging is best-effort outside seven files.** The redaction policy is
  machine-checked for subjects only; `logredact` is unused in the delivery,
  webhook, screening, identity, and ws packages; raw transport/provider errors
  reach stderr, which ships to long-retention central storage
  (`internal/logredact/logredact.go:1-29`, adoption grep). Phase 1 item 2 is
  the fix; the HIPAA-path rule is opaque identifiers only.
- **F7 — River's error log is a PHI surface.** Failed-attempt error strings
  (quoting recipients) persist on `river_job` rows for the discarded-retention
  window; no retention override is configured (`internal/outboundsend/worker.go:604`,
  `internal/jobs/jobs.go:117-135`). Fix: sanitize errors returned to River on
  the HIPAA path.
- **F8 — Gemini sees extracted attachment text.** The screening payload
  includes `attachment_text` segments, not just the body
  (`internal/piguard/extract.go:35-66`) — relevant to the Vertex BAA scope and
  to customer-facing documentation, which currently describes the payload as
  subject/sender/body.
- **F9 — doc drift.** `docs/data-handling.md:28` claims webhook delivery logs
  the destination URL; the deliverer does not log URLs. Keep the doc honest in
  whichever direction is intended.

Confirmations worth recording (controls that held up under the census): no
content in River job args; no temp files or object storage in the mail path;
no full-text index over bodies; WS auth moved out of query params; attachment
tokens absent from server-side storage and e2a's own logs; metrics label
vocabulary closed by construction; Umami fenced to marketing paths; export
assembled in-memory rather than written to disk.

## 8. What Phase 1 is graded against

Encryption, retention, secure deletion, and PHI-safe observability are done
when every row of §2–§4 has an explicit answer, and "not applicable" is
recorded, not assumed. Concretely: C2–C5 and M1–M6 get encryption + a bounded
retention clock + an erasure path; C7 gets the per-account DEK / crypto-erasure
design; E6 gets no-downgrade TLS; E8 and F5–F7 get the opaque-identifier rule;
E4 gets principal-bound tokens (G17); E11 stays hard-off for HIPAA accounts.
Additions to this inventory are part of the definition of done for any change
that adds a table, column, log line, or vendor call that message data can
reach.
