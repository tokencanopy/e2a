# HITL approve → QueueOutbound (async send)

> **GA resolution (2026-07-15):** queue-first external outbound delivery is
> mandatory. `E2A_OUTBOUND_MODE` and the submit-inline fallback were removed.
> Historical sync-mode branches below are retained only as design history;
> self-send loopback remains a distinct local delivery path. The agent-path
> approve/reject endpoints were also removed in the pre-GA vocabulary freeze:
> the human-approve path described below now lives at
> `POST /v1/reviews/{id}/approve` (the `/v1/approve` magic link survives).

Status: approved · Owner: backend · Related: `internal/hitlworker`, `internal/agent`, `internal/outboundsend`, `internal/identity`

## Context / goal

When a HITL outbound hold is approved, e2a sends it. Both approve paths do the
send **inline, synchronously** today (blocking `Sender.Send`, retrying SMTP,
`send_attempts` gate):

- **TTL auto-approve** — the `QueueMaintenance` sweep, when `hitl_expiration_action=approve`.
- **Human approve** — `POST /v1/agents/{email}/messages/{id}/approve` (+ the magic-link
  `/v1/approve`), via `ApprovePendingCore` → `store.ApproveAndSend`.

This change routes **both** approve sends onto the async outbound River queue
(`QueueOutbound`) — the same pipeline a normal async send uses — when
`E2A_OUTBOUND_MODE=async`. The sweep becomes DB-only; the approve endpoint returns
**`accepted`** (send durably queued) instead of blocking for a synchronous `sent`.
This is the industry-standard semantic (SendGrid `202 Accepted`, Mailgun "Queued",
SES accepted-for-sending) and matches every normal e2a async send; the delivery
outcome follows via `email.sent`/`email.failed`. Sync mode is unchanged.

## Key enabling fact

The async `SendWorker` needs **no changes**. `LoadOutboundForSend` selects by id
filtered only on `direction='outbound'` (no `status` filter) and `alreadyDone()`
keys off `delivery_status`. So a hold row transitioned to `delivery_status='accepted'`
(+ `raw_message`/`envelope_from`/`sent_as` + a stamped `send_job_id`) loads and
sends like any accepted message, regardless of its hold `status`
(`review_approved` or `review_expired_approved`). `MarkSent`/`MarkFailed` touch
only `delivery_status`/`provider_message_id`/`delivery_detail`, leaving the hold
status intact. The existing outbound reconciler (`delivery_status='accepted' AND
send_job_id IS NULL`) covers a stranded transition for free.

## Design

Both approve cores (`hitlworker.autoApprove`, `agent.ApprovePendingCore`) gain the
same three-way branch. The self-send + async decision is made **before** the
transition, from the reviewed/edited recipients:

```
approve(msg, [edits], [reviewer]):
  guards (agent match / domain verified)                 (unchanged)
  req = build SendRequest from the (edited) draft + attachReferencesChain

  if loopback.IsSelfSend(req, agent):
      → SYNC loopback (unchanged): ApproveAndSend / ExpireApproveAndSend with the
        loopback callback. Self-sends never go on QueueOutbound.

  else if async enqueuer wired (E2A_OUTBOUND_MODE=async):
      → NEW async path:
          comp = sender.ComposeForAccept(agent, req)        # compose, no submit
          # targetStatus: 'sent' for a HUMAN approve (outbound's approved terminal,
          # same as sync ApproveAndSend — the human resolution is recorded via
          # reviewed_by_user_id + the review_approved event); 'review_expired_approved'
          # for the TTL sweep (same as sync ExpireApproveAndSend).
          msg  = store.ApproveAndAccept(msgID, reviewer, targetStatus, edited,
                       AcceptedSend{To,CC,BCC,Subject,Method,EnvelopeFrom,SentAs,Raw},
                       enqueue = outboundEnq.EnqueueSendTx)   # one tx (below)
          publish review_approved (auto_resolved for TTL / reviewer for human;
                                   provider_message_id empty — send is queued)
          return msg   # status=review_approved(_expired), delivery_status=accepted
          # NO blocking send, NO metering, NO send_attempts here — the SendWorker
          # does the submit + email.sent/failed + metering.

  else (sync mode):
      → SYNC (unchanged): ApproveAndSend / ExpireApproveAndSend with Sender.Send.
```

### New store primitive — `ApproveAndAccept`

A pure transactional persist+enqueue (all composition/edit logic stays in the
agent/hitlworker layer, where the `sender` + edits live). It does NOT re-send or
use `send_attempts`.

```go
func (s *Store) ApproveAndAccept(
    ctx context.Context, messageID string,
    reviewedByUserID string,   // "" (NULL) for the TTL sweep
    targetStatus string,       // sent (human approve) | review_expired_approved (TTL)
    edited bool,
    acc AcceptedSend,          // To,CC,BCC []string; Subject,Method,EnvelopeFrom,SentAs string; Raw []byte
    enqueue func(ctx context.Context, tx pgx.Tx, messageID string) (int64, error),
) (*Message, error)
```

One `WithTx`: a compare-and-set UPDATE (the `WHERE status='pending_review'` is the
atomic guard; `RowsAffected()==0` → `ErrNotPendingApproval`, a no-op) →
`enqueue(tx)` → `StampSendJobIDTx(tx, jobID)`:

```sql
UPDATE messages
   SET status              = $targetStatus,
       delivery_status     = 'accepted',
       to_recipients=$, cc=$, bcc=$, subject=$, recipient=$firstTo, method=$,
       envelope_from=$, sent_as=$, raw_message=$,   -- nullIfEmptyBytes
       provider_message_id = '',                    -- filled by the SendWorker
       reviewed_at         = now(),
       reviewed_by_user_id = $reviewer,             -- NULL for TTL
       edited              = $edited,
       body_text=NULL, body_html=NULL, attachments_json=NULL   -- scrub draft
 WHERE id=$1 AND direction='outbound' AND status='pending_review'
```

### Events & metering

- **`review_approved`** still fires from the approve core after the transition
  commits — with **empty `provider_message_id`** (send queued). Keeps `auto_resolved`
  (TTL) / `reviewed_by_user_id` + `edited` (human). It means "hold resolved to
  approved", distinct from the delivery outcome.
- **`email.sent`/`email.failed`** move to the `SendWorker` (real provider id).
- **Metering** for the async path moves to the `SendWorker`; the approve cores meter
  only their sync + self-send/loopback paths (unchanged).

### Error handling (async path)

Any tx/enqueue failure is transient (DB/River). The **sweep** logs and leaves the
row `pending_review` for the next cycle (never `autoReject` — no send happened, so
no `[hitl-stuck]`). The **endpoint** returns a 500 (same as a normal async accept-tx
failure); the reviewer retries (idempotency-key-safe).

### Response contract (human approve)

`ApprovePendingCore` returns the transitioned message; the httpapi `SendResultView`
now reports `status: "accepted"` for the async path (was the synchronous send
result). The confirmation page / dashboard show the send as `accepted → sent/failed`
like any async send. `edited` is still surfaced.

### `Timeout()` becomes mode-aware

`MaintenanceJobs`/`MaintenanceWorker` gain `asyncSends bool`. `Timeout()` returns a
**bounded** value (5 min) when async — the sweep is now DB-only, restoring River's
slot-occupancy protection — and stays `-1` in sync mode (blocking `Sender.Send`
remains). Delivers the "return the timeout to bounded" goal for the async prod path.

## Behavior changes (blessed)

In async mode:

- **Approve returns `accepted`, not `sent`** — the send is durably queued; the
  outcome follows via `email.sent`/`email.failed`. Matches every normal async send
  and the industry (SendGrid `202`, SES accepted-for-sending).
- **A send that ultimately fails stays approved.** A TTL-auto-approved
  (`review_expired_approved`) or human-approved (`sent`) hold whose SMTP submit
  fails ends `delivery_status='failed'` + an `email.failed` event — it does **not**
  fall back to `review_(expired_)rejected`. Review decision ≠ delivery outcome.
- **`email.sent` now fires on a HITL-approved send's success.** The sync approve
  path emitted only `review_approved` (and magic-link nothing); async approve adds
  `email.sent`/`email.failed` from the SendWorker, consistent with normal async
  sends. Subscribers keying off `email.sent` will observe it for approved sends once
  the flag flips.

Known limitation (F2): the SendWorker's 72h outage-tolerance clock (`AcceptedAt`)
is anchored to the message's `created_at` (hold-creation time), not the approve
time — so a hold approved >72h after creation, during a provider outage at send
time, would terminate immediately instead of snoozing. Narrow edge; shared with the
normal-send anchor, out of scope here.

## Scope / non-goals

- Both approve paths (TTL sweep + human endpoint). Self-sends stay on the sync
  loopback path. Sync mode (`E2A_OUTBOUND_MODE` unset/`sync`) byte-for-byte unchanged.
- Inbound review approve/reject and the outbound reject-on-expiry path are untouched.

## Files

- `internal/identity/store.go` — new `ApproveAndAccept` + `AcceptedSend`; reuse `StampSendJobIDTx`.
- `internal/hitlworker/{worker.go,maintenance.go}` — async branch in `autoApprove`; `outboundEnq` seam + `SetOutboundEnqueuer`; mode-aware `Timeout()`.
- `internal/agent/hitl_api.go` (+ `hitl_magic_api.go` response mapping) — async branch in `ApprovePendingCore`; reuse the `OutboundEnqueuer` already on `*API`.
- `internal/httpapi/hitl.go` / `outbound.go` — surface `status: accepted` on the approve view.
- `cmd/e2a/main.go` — pass `outboundJobs` to the hitl worker + `asyncSends` to the maintenance registrar (already sets `api.SetOutboundEnqueuer`).
- Tests + live e2e.

## Verification

- Unit: `ApproveAndAccept` CAS (transitions pending_review→accepted, no-op on resolved);
  sweep + endpoint async branches (enqueue, stamp, review_approved emitted, no meter,
  no autoReject/500-only on tx error); self-send still loopback; sync unchanged;
  mode-aware `Timeout()`; approve view reports `accepted`.
- Live e2e (async instance): human approve → `accepted` returned, `outbound_send`
  enqueued, `review_approved` fired → SendWorker submits → `delivery_status='sent'` +
  `email.sent`. Same for TTL auto-approve. Self-send → loopback (no outbound_send job).
  Failed-submit A/B → `email.failed`, hold stays approved. Sweep does no blocking send.
