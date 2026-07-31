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
| Deployment-wide HMAC secret (operator key) | Operator's env (`E2A_HMAC_SECRET`); never written to DB | Lifetime of the deployment. Used for HITL approval / magic-link tokens and internal key derivation. SDKs verify webhook deliveries with the per-webhook `whsec_` secret, not this. |

## What's logged

- The SMTP relay logs envelope metadata on every inbound message: sender/recipient domains, byte count, the SPF/DKIM verdict. Addresses are redacted before they reach a log line (`internal/logredact`): an unresolved or external address is reduced to its domain (`AddressDomain` / `AddressDomains`), and client IPs are truncated to their network prefix (`IPNetwork`, `/24` for IPv4, `/48` for IPv6). Once a recipient resolves to one of the deployment's own agents, its full address is logged as the tracing key — that's e2a's own namespace, not third-party PII. Message subject lines are never logged in full, only `subject_len`. Operators in privacy-strict environments who also want resolved agent addresses redacted, or who need to bound retention of the domain-only remnants, should still plan for that in their log forwarder.
- HITL state transitions log message IDs and agent IDs but not bodies.
- Webhook delivery attempts log the destination URL and status code.

Application logs do **not** include message bodies, attachment contents, raw API keys, or HMAC secrets.

## User rights

The API exposes the two operations that GDPR Art. 15 / Art. 17 (and CCPA equivalents) require:

- **`GET /v1/account/export`** — returns a JSON dump of the account's core data: profile, agents, domains, API key metadata, all messages with bodies, usage events, protection events, OAuth connections, and account-wide or exact-agent suppressions (`internal/identity/user_data_rights.go`'s `UserExport` struct is the authoritative field list). Export schema v4 retains the v3 suppression-entry contract: optional `agent_email` distinguishes exact-agent blocks, while entries without it are account-wide. Internal identifiers (Google subject, key hashes, session tokens) are excluded. **Not yet included:** contacts, contact engagements, contact import batches, and templates — those tables are account-owned but not yet wired into the export.
- **`DELETE /v1/account?confirm=DELETE`** — wipes the account and every related row in a single Postgres transaction (cascade through `agent_identities → messages → webhook_deliveries`, the account-owned suppression/token tables, and the contacts/engagements/import-batches/templates tables, plus explicit deletion of `usage_events` which has `ON DELETE SET NULL` rather than CASCADE so it survives by default). Returns per-table row counts, including `agent_suppressions_deleted` and `agent_unsubscribe_tokens_deleted`, so the caller can audit most of what was removed — the receipt does not yet break out contacts/engagements/import-batches/templates counts, even though those rows are deleted.

Both are scoped to the authenticated user — there's no path to target someone else's data.

## Operator responsibilities

Things e2a doesn't (and can't) handle for you:

- **Database backups.** Take them, encrypt them, set retention policy. e2a doesn't ship a backup story; use whatever your Postgres provider gives you.
- **TLS termination** for the API and SMTP. Production mode enforces HTTPS for webhook delivery; the operator's reverse proxy / ingress terminates TLS for inbound API traffic and the SMTP relay's `tls_cert` / `tls_key` config covers `:2525`.
- **At-rest encryption.** Disk-level / volume-level encryption is the operator's responsibility (Postgres TDE, EBS encryption, GCP CMEK, …). e2a does not currently encrypt message bodies or attachments at the application layer; if your threat model includes a privileged DBA, you'll want to add column-level encryption.
- **Log redaction.** e2a already redacts unresolved/external addresses to domain-only and truncates client IPs before they reach `log.Printf` (`internal/logredact`); resolved agent addresses (your own users) are logged in full as the tracing key. If your environment can't tolerate that — or the domain-only remnants — redact further in your log forwarder; e2a doesn't expose a config toggle to suppress its own log lines.
- **Compliance attestations** (SOC 2, HIPAA, ISO 27001) — those are deployment-level, not code-level.
