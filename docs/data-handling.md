# Data Handling

What e2a stores, how long it lives, and what users + operators can do with it.

For vulnerability reporting and the security model, see [SECURITY.md](../SECURITY.md).

## What's stored

| Data | Where | Retention |
|---|---|---|
| Inbound + outbound message envelopes (sender, recipient, subject, caller-owned `conversation_id`, optional server-owned `thread_id`, RFC topology identifiers, timestamps) | Postgres `messages` | Indefinite while live. Soft-deleted rows are purged after 30 days by default. |
| Inbound message bodies (raw RFC822 in `raw_message`) | Postgres `messages` | Indefinite while live; same trash policy as the parent message |
| Outbound message bodies | Postgres `messages.raw_message`, `body_text`, `body_html`, `attachments_json` | Indefinite while live, including after approve, reject, expiry, or delivery transitions |
| Attachments | Postgres rows (`raw_message` / `attachments_json`) | Indefinite while live; same trash policy as the parent message — no S3/GCS |
| Agent + domain ownership records | Postgres `agent_identities`, `domains` | Until the user deletes the agent/domain or the account |
| Agent-scoped recipient suppressions | Postgres `agent_suppressions` | Until the account is deleted. They intentionally survive agent trash, permanent deletion, and recreation so recipient consent remains effective for the same sending address. |
| Managed-unsubscribe capability mappings (agent + recipient addresses and token hash; never the bearer token) | Postgres `agent_unsubscribe_tokens` | Until the account is deleted. Links in previously delivered mail remain valid, including after an agent is deleted. |
| API keys | Postgres `api_keys`, **hash only** (hex-encoded SHA-256 of the plaintext) | Until revoked or the user is deleted; plaintext exists only in the create response and is never persisted |
| OAuth sessions | Postgres `user_sessions` | 7 days; cleanup worker removes expired rows hourly |
| Usage events / summaries (only when `E2A_USAGE_TRACKING=true`) | Postgres `usage_events`, `usage_summaries` | Indefinite by default — operator can purge or override |
| Per-webhook signing secret (`whsec_…`) | Postgres, **plaintext** | Until the webhook is deleted. Returned once at creation; rotate via `POST /v1/webhooks/{id}/rotate-secret` (the previous secret stays valid for a 24h grace window). |
| Owner-mailbox proof (the sign-in address a verified Google login confirmed, its source and timestamp) | Postgres `users.owner_email_verified_*` | Until the account is deleted; stops applying as soon as the account email changes. |
| External sending access requests (use case, intended recipients, expected volume, review state) | Postgres `external_sending_access_requests` | Until the account is deleted (cascade). Private support data — never filed to a public tracker. |
| External sending access audit (operator grant/revoke: account id, old/new state, actor, reason; no addresses) | Postgres `external_sending_access_events` | Append-only; no account foreign key, so it outlives account deletion. Each row carries an `expires_at` (the `sending_control_audit_retention_days` policy value, 90 days by default); like `account_sending_control_events`, the purge of expired rows is not yet automated and is an operator task. |
| Trashed accounts (the whole account row and everything it owns, inert) | Postgres `users.deleted_at` and the owned rows | Until restored, or purged after `trash.account_retention_days` (default = `trash.retention_days`, 30 days). `0` disables account trash. |
| Identity tombstones (keyed HMAC digests of a purged account's login subjects, email and — after an abuse pause — verified domains; no plaintext) | Postgres `identity_tombstones` | Only when `trash.identity_tombstones` is on. At least 30 days (longest of the trash window and the rest of the quota month); 2 years for an abuse-paused account. Swept by the janitor at `expires_at`. |
| Deleted-account summaries (counts, recipient-domain histogram, per-day send counts, keyed subject/domain digests, pause state and optional operator evidence reference; no bodies, recipient addresses or owner email) | Postgres `deleted_account_summaries` | Only when `trash.identity_tombstones` is on. Same hold as the account's tombstones; swept by the janitor. Operator-readable only (`-inspect-deleted-account`). |
| Deployment-wide HMAC secret (operator key) | Operator's env (`E2A_HMAC_SECRET`); never written to DB | Lifetime of the deployment. Used for HITL approval / magic-link tokens and internal key derivation. SDKs verify webhook deliveries with the per-webhook `whsec_` secret, not this. |

## What's logged

- The SMTP relay logs envelope metadata on every inbound message: sender/recipient domains, byte count, the SPF/DKIM verdict. Addresses are redacted before they reach a log line (`internal/logredact`): an unresolved or external address is reduced to its domain (`AddressDomain` / `AddressDomains`), and client IPs are truncated to their network prefix (`IPNetwork`, `/24` for IPv4, `/48` for IPv6). Once a recipient resolves to one of the deployment's own agents, its full address is logged as the tracing key — that's e2a's own namespace, not third-party PII. Message subject lines are never logged in full, only `subject_len`. Operators in privacy-strict environments who also want resolved agent addresses redacted, or who need to bound retention of the domain-only remnants, should still plan for that in their log forwarder.
- HITL state transitions log message IDs and agent IDs but not bodies.
- Webhook delivery attempts log the destination URL and status code.

Application logs do **not** include message bodies, attachment contents, raw API keys, or HMAC secrets.

## User rights

The API exposes self-service export and deletion operations that support GDPR Art. 15 / Art. 17 (and CCPA-equivalent) requests. Operators remain responsible for accounting for data that these responses do not yet represent.

- **`GET /v1/account/export`** — returns the fields defined by `internal/identity/user_data_rights.go`'s `UserExport` struct: profile, agents, domains, API key metadata, all messages with bodies, usage events, protection events, OAuth connections, and account-wide or exact-agent suppressions. Export schema v4 retains the v3 suppression-entry contract: optional `agent_email` distinguishes exact-agent blocks, while entries without it are account-wide. Internal identifiers (Google subject, key hashes, session tokens) are excluded. **This is not yet an exhaustive export of every account-owned resource.** Examples currently omitted include contacts, contact engagements, contact import batches, templates, webhook subscriptions, and webhook event/delivery history.
- **`DELETE /v1/account?confirm=DELETE`** — moves the account to the **trash** (the default). The account becomes unusable at once — every API key, OAuth grant and dashboard session is revoked, every agent is trashed (inbound refused), sending stops (the sending gate refuses a trashed account; an existing abuse pause and its class are left untouched), and every custom domain loses its verification and its SES identity is torn down — while its data stays in Postgres, restorable by signing in to the dashboard, until the janitor purges it after `trash.account_retention_days` (default: `trash.retention_days`, 30 days). A restore brings the account and its agents back; API keys stay revoked and domains must be re-verified. The receipt is `mode: "trash"` with `purge_after`; `messages_deleted` is 0 and the other counts describe rows trashed, revoked or unverified.
- **`DELETE /v1/account?confirm=DELETE&permanent=true`** (and "erase now" in the restore interstitial) — erases immediately: the account's agents are drained in bounded chunks and then the account row and every related row are deleted (cascade through the account-owned suppression/token tables and the contacts/engagements/import-batches/templates tables, plus explicit deletion of `usage_events`, whose FK is `ON DELETE SET NULL`). The janitor's purge after the trash window runs exactly the same path. The receipt is `mode: "permanent"` with `user_deleted: true` and selected per-table counts (including `agent_suppressions_deleted` and `agent_unsubscribe_tokens_deleted`); it does not separately count contacts, engagements, import batches, templates, webhooks, or webhook event/delivery rows, even though those rows are deleted. A deployment that sets `trash.account_retention_days: 0` makes every account deletion permanent.
- **What outlives a purge.** The sending ledger (provider operations, budget counters, feedback provenance, control audit) keeps its own retention, as before. When the deployment enables `trash.identity_tombstones` (hosted policy; off by default) every purge also writes, before any row is deleted: **identity tombstones** — HMAC-SHA256 digests under a dedicated key (`E2A_TOMBSTONE_KEY`) of the account's login subject(s) and normalized email, held for the longer of the trash window and the rest of the current quota month (at least 30 days), plus the verified domains and a 2-year hold when the account was paused for abuse — so that identity cannot immediately register again (`registration_refused`; domains answer `domain_taken`); and a **deleted-account summary** (`deleted_account_summaries`) — counts, first/last send, a top-20 recipient-*domain* histogram, per-day send counts for the last 30 days, keyed digests of the top-20 subjects and of verified domains, and the pause state/class/reason with an optional operator evidence reference — kept for the same hold. Neither holds message bodies, recipient addresses, or the owner email in the clear; the summary is readable only through the operator command `-inspect-deleted-account`, never over HTTP or MCP. The janitor deletes both at `expires_at`.

Both are scoped to the authenticated user — there's no path to target someone else's data.

## Operator responsibilities

Things e2a doesn't (and can't) handle for you:

- **Database backups.** Take them, encrypt them, set retention policy. e2a doesn't ship a backup story; use whatever your Postgres provider gives you.
- **TLS termination** for the API and SMTP. Production mode enforces HTTPS for webhook delivery; the operator's reverse proxy / ingress terminates TLS for inbound API traffic and the SMTP relay's `tls_cert` / `tls_key` config covers `:2525`.
- **At-rest encryption.** Disk-level / volume-level encryption is the operator's responsibility (Postgres TDE, EBS encryption, GCP CMEK, …). e2a does not currently encrypt message bodies or attachments at the application layer; if your threat model includes a privileged DBA, you'll want to add column-level encryption.
- **Log redaction.** e2a already redacts unresolved/external addresses to domain-only and truncates client IPs before they reach `log.Printf` (`internal/logredact`); resolved agent addresses (your own users) are logged in full as the tracing key. If your environment can't tolerate that — or the domain-only remnants — redact further in your log forwarder; e2a doesn't expose a config toggle to suppress its own log lines.
- **Compliance attestations** (SOC 2, HIPAA, ISO 27001) — those are deployment-level, not code-level.
