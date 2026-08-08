# The ticket-card (github ticket_store)

In the **github** store a ticket IS a GitHub issue labeled `{labels.feedback}`,
and its machine-readable state lives in ONE pinned bot comment — the
**ticket-card**. Labels are the human-visible projection; the ticket-card is
authoritative. All lanes read and patch this single comment; never scatter
state across multiple comments.

## Format

The card is a bot-authored issue comment containing a single fenced JSON
block bracketed by sentinels so extraction is unambiguous:

```
<!-- autorepo:ticket-card:begin -->
​```json
{ ...the card... }
​```
<!-- autorepo:ticket-card:end -->
```

Find it with `gh issue view <n> --comments` (or `gh api`), locate the
comment authored by the bot identity whose body contains
`autorepo:ticket-card:begin`, and parse the JSON between the fences. Use the
`ticket_card.sh` helper (`init` / `read` / `set` (alias `patch`) /
`add-event` / `find-by-comms`) rather than hand-editing — it keeps the
parse/merge correct, trusts only the bot-authored card, and is the only Bash
surface the lanes are allowlisted for this. A `set` patch never replaces the
append-only `events` array; use `add-event` to extend it.

## Schema (v1)

```jsonc
{
  "schema": 1,
  "ticket": 123,                 // the issue number == the ticket id (github store)
  "kind": "bug",                 // bug | feature | other
  "status": "triaged",           // see state-machine.md
  "marker": "acme-feedback",     // == config.marker; redundant for robust extraction
  "comms_ref": "conv_abc123",    // FILER app conversation ID only; never an address
  "duplicate_of": null,          // issue number of the canonical ticket, or null
  "fix_gate": {
    "mode": "hitl",              // mirrors config at triage time
    "decision": "needs_approval",// needs_approval | auto | n/a   (set by triage)
    "surface": null              // the always_hitl surface that forced hitl, or null
  },
  "approval": {
    "status": "needed",          // none | needed | pending | approved | declined
    "conversation_id": null,     // APPROVER app conversation ID; set on first send
    "decided_by": null,          // approver address, on a verified decision
    "reason": null               // approver's note on decline
  },
  "pr": null,                    // fix PR number, or null
  "customer_note": null,         // the PR's customer-note block (fix lane captures it; comms slots it into the shipped email)
  "contact": true,               // filer opted into updates; comms flips false on a verified unsubscribe — gates proactive sends
  "notified": [],                // comms stages already sent (idempotency): ["triage-ack","shipped",...]
  "events": [                    // append-only timeline (audit trail)
    { "at": "2026-06-29T12:00:00Z", "actor": "triage-lane", "kind": "triaged", "detail": "classified bug; issue created" }
  ]
}
```

## Field discipline

- **`comms_ref` is an opaque id, never PII.** The filer's email address
  lives only in the e2a mailbox; resolving `comms_ref` → address needs the
  comms lane's e2a key. A public reader sees only the id.
- **`notified` is the notification ledger.** A stage appears at most once;
  the comms lane reads-before-send. The per-lane `concurrency:` group is the
  only serializer in the github store (weaker than a DB unique index — the
  documented price of zero-backend).
- **`events` is append-only.** Never rewrite history; append.
- **`status` is authoritative;** whenever it changes, relabel the issue to
  match (and open/close per `state-machine.md`) in the same transition.

## Idempotency / claim discipline (intake)

Each filer application conversation maps to at most one ticket via its
`conversation_id`. The
crash-safe dedup key is the **bot-authored issue-body footer**
`<!-- {marker} comms:<conversation_id> -->`, written ATOMICALLY with the
issue body — so it exists even if the run dies before the ticket-card is
written. Before creating an issue, run `ticket_card.sh find-by-comms
<conversation_id>`; if it returns an issue, the email is already triaged (a
crashed prior run) — match it (write the card if missing), do not create a
duplicate. Mark the intake message read only AFTER the issue exists. Worst
case of a mid-run crash is an unread email re-examined next tick and matched
to the issue by its footer — never a duplicate ticket. (The card's
`comms_ref` mirrors the footer for in-card reads; the footer is the recovery
authority because it cannot be missing when the issue exists.)
