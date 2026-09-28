# Account soft deletion

Status: shipped in v1.11.0 (trash, restore, purge, identity tombstones).

The normative behaviour of account deletion — trash by default, restore by
signing in, the janitor purge, identity tombstones and the deleted-account
summary — is documented in [data-handling.md](../data-handling.md) (the
`DELETE /v1/account` entries) and, for paused accounts, in
[account-read-only.md](account-read-only.md). Code comments cite this file
for the design rationale; this file currently carries the sections added
after the initial release.

## Deferred erase for recent senders

### Problem

`DELETE /v1/account?confirm=DELETE` moves an account to the trash for the
account trash window (30 days by default). While it is trashed, the account's
`account_sending_controls` row and its bounce/complaint aggregates
(`account_sending_outcomes_daily`) survive and keep updating from late
provider feedback. `permanent=true` (and "erase now" on the restore
interstitial) skips that window and purges at once.

Provider feedback is late: a complaint can arrive hours to days after the
send. An account that sends a burst and then erases itself immediately takes
its control row and aggregates with it before that feedback lands, so the
feedback counts against nothing — the abuse detector never sees the burst's
complaint rate, and an operator loses the evidence. Before this change the
only thing that refused an immediate erase was an active sending pause
(`409 erase_held`).

### Rule

A permanent erase of an account that **sent to an external recipient within
the last `trash.recent_sender_erase_defer_days` days** (default 14; `0`
disables) is deferred: the account is moved to the trash — the same trash the
default delete performs — and the janitor purges it at the normal end of the
account trash window.

- **External recipient** uses the external-sending-access notion: any
  recipient other than an agent of the same account, the account's verified
  owner mailbox (valid proof for its current email), or an address on one of
  the deployment's shared agent domains (verified `domains` rows with no
  owning account).
- **Source of truth** is `message_recipients`: a row is written exactly when
  the provider (or relay) accepted the message, one per normalized envelope
  recipient (to/cc/bcc). It therefore records real sends only — never drafts,
  review holds, refusals or queued mail — and is reached through the
  account's agents and `idx_messages_agent_created`, with an `EXISTS` that
  stops at the first hit. The send instant is the latest of the message's
  `created_at`, `provider_accepted_at`, `reviewed_at` and `scheduled_at`, so a
  scheduled or review-held message submitted recently counts even if it was
  created long ago. `usage_events` was rejected as the source because it
  carries no recipient addresses (it cannot tell an external send from an
  agent-to-agent one) and is not written for non-standard account classes;
  the deletion-resistant `sending_feedback_*` ledger was rejected because it
  stores recipients only as keyed digests.
- **Ordering.** The decision is taken *after* the trash commits, so no send
  can settle between the check and the purge — a trashed account cannot send.
  If the check itself fails, the erase is deferred (the account is already in
  the trash and will be purged at the end of the window): the failure mode
  keeps evidence rather than destroying it.
- **Precedence.** The paused-account refusal (`409 erase_held`) is checked
  first and is unchanged.
- **Scope.** The same rule, scoped to one agent or one message, applies to
  permanent agent and message deletes (see "Agent and message level"), so
  the evidence cannot be removed on demand inside the window.
- **No trash, no deferral.** With `trash.account_retention_days: 0` there is
  no window to hold the account in, so the rule does not apply.

### Contract

The deferred erase is a success, not an error — the account *is* deleted from
the owner's point of view (keys, sessions and grants revoked, agents trashed,
sending stopped). The receipt is additive over the existing one:

```json
{
  "deleted": true,
  "mode": "trash",
  "erase_deferred": true,
  "purge_after": "2026-10-28T12:00:00Z",
  "message": "This account emailed external recipients recently, so it is kept in the trash until purge_after ...",
  "user_deleted": false,
  "messages_deleted": 0,
  "agents_deleted": 2
}
```

The counts are the trash counts when this request trashed the account, and
zero when the account was already in the trash (the restore interstitial's
"erase now"). On the interstitial the restricted session is kept, so the
restore offer keeps working. Billing is notified with the account-state
`trash` notice, exactly as for a default delete; the purge notice follows
from the janitor.

### Agent and message level

The account check reads `message_recipients`, which cascades from
`messages`. If agents or messages could be purged on demand, an account could
remove its own evidence first — permanently delete every agent that sent
externally (or each sent message), then erase itself — and the account erase
would no longer be deferred. So the same rule applies to every on-demand path
that purges sent mail:

| Path | Deferred to | Receipt |
|---|---|---|
| `DELETE /v1/account?permanent=true`, restore interstitial "erase now" | account trash (`trash.account_retention_days`) | `mode: trash`, `erase_deferred`, `purge_after`, `message` |
| `DELETE /v1/agents/{email}?confirm=DELETE&permanent=true` (live or trashed agent) | agent trash (`trash.retention_days`) | `erase_deferred`, `purge_after`, `message`, `messages_deleted: 0` |
| `DELETE …/messages/{id}?permanent=true&confirm=DELETE` (trashed message) | message trash (`trash.retention_days`) | `erase_deferred`, `purge_after`, `message` |

- The agent check is the same predicate scoped to one agent, the message
  check scoped to one message. The agent decision runs under the agent row
  lock after the send-lease check, and the deferral trashes the agent in the
  same transaction; the message decision runs under the message row lock.
- The paused-account refusal (`409 erase_held`) still comes first on the
  agent path. A read-only (abuse-paused) account cannot reach any of these
  writes at all.
- `trash.recent_sender_erase_defer_days: 0` disables all three.
- Every other purge is time-based: the janitor purges trashed accounts,
  agents and messages only after their trash windows, which are longer than
  or equal to the look-back window by default. Domain deletion is refused
  while any live or trashed agent remains on the domain, so it cannot purge
  sent mail either. The MCP surface exposes no permanent message delete, and
  its `delete_agent` goes through the same `/v1` operation.

### What stays the same

- Restore and the restricted restore session work exactly as for any trashed
  account, agent or message.
- Operator force-purge (runbook: backdate `deleted_at` past the window and
  let the janitor run) still purges a deferred account, agent or message.

### Remaining limits

- The account check counts only sends recorded in `message_recipients`, i.e.
  mail the provider or relay accepted. A deployment whose trash windows are
  configured shorter than `recent_sender_erase_defer_days` lets the janitor
  purge a trashed agent or message before the look-back window ends; the
  janitor's time-based purge is intentionally not deferred. Keep
  `trash.retention_days` at least as long as the defer window (the defaults,
  30 and 14, satisfy this).
