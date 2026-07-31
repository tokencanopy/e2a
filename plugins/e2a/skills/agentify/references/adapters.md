# Adapter contracts

The lane runtime depends only on three adapter contracts + the config file,
never on a product's schema/tools/auth. Adapters are **prompt-level
contracts** (a procedure + the tools/secrets it is allowed + config keys),
not a code plugin system — the runtime is a Claude Code agent.

## TicketStore — where ticket state lives

Operations the runtime skill needs: `list_pending(stage)`, `get(ref)`,
`transition(ref, to, …)`, `append_event(ref, kind, detail)`,
`find_candidates(query)`, `record_notified(ref, stage, comms_ref)`.

| impl | status | how |
|---|---|---|
| **github** | ✅ v0 | issue = ticket, labels = status, a pinned ticket-card comment = machine-readable state. Ops via `gh` + `scripts/ticket_card.sh`. OSS-only (public issue = no PII boundary; filer identity lives only in the comms channel). |
| **backend** | ⏳ anchor | the same ops over a durable §5.4 internal API (per-lane bearer secrets, server-validated transitions). Shaped by agentdrive's built implementation; not wired in v0. Use when you have private filer identity or need DB-grade notification idempotency. |

## CommsChannel — filer notifications + maintainer approvals

Operations: `notify(ref, stage, slots)`, `poll_replies()`,
`resolve_contact(ref)`.

| impl | status | how |
|---|---|---|
| **e2a** | ✅ v0 (slice 2) | `reply_to_message`/`send_message` out; `list_messages`+`get_message` in. **Verified reply** = caller-owned `conversation_id` matches a ticket's `comms_ref`/`approval.conversation_id`, `authentication.dmarc.status == "pass"`, AND `header_from` equals the address on file. `resolve_contact` is a no-op — the application-conversation view carries the contact correlation, and the address never leaves e2a. |
| **smtp** | ⏳ | plain SMTP send + IMAP poll; the adopter implements verified-sender matching. |
| **none** | ⏳ | no email — the public GitHub issue thread is the comms channel (filers are GitHub users). |

## Intake — where raw feedback originates

Produces raw items the triage lane normalizes into tickets.

| impl | status | how |
|---|---|---|
| **email** | ✅ v0 | triage polls the comms mailbox for inbound that is NOT a reply to a known ticket; it CREATES the issue. |
| **github_issue** | ⏳ | the community files issues directly; triage labels/structures existing issues. Cheapest second proof of the intake seam. |

✅ implemented · ⏳ designed-for, not built in v0
