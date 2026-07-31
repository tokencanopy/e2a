# Webhook fan-out → River — Migration Design

Status: **Shipped** (drafted 2026-07-09 as a design pass off the as-built worker).
As built, the worker lives in `internal/webhookpub` (`FanOutArgs.Kind() =
"webhook_fanout"`, no separate `internal/webhookfanout` package), behind the
`E2A_WEBHOOK_FANOUT_MODE` flag in `internal/config`.
Completes the webhook triad: **fan-out** (Layer 1 → Layer 2) is the last webhook stage
still on a hand-rolled loop. Companion to `webhook-delivery-river-migration.md`
(Layer 2 → Layer 3, shipped) and `inbound-message-pipeline-river.md` (the closest
structural mirror — fact row + in-tx job enqueue + River worker re-reads).

---

## 1. Problem statement

The three-layer webhook architecture is:

```
Layer 1 — FACTS           webhook_events                 immutable event log; GET /v1/events, redelivery, ~30d
            │  FAN-OUT  (1 event → N matching subscribers)   ← THIS STAGE
            ▼
Layer 2 — DELIVERY STATE  webhook_subscriber_deliveries  per (event × webhook); GET /v1/webhooks/{id}/deliveries
            │  DELIVERY (HTTP POST + retry)                   ← already on River (QueueWebhook)
            ▼
Layer 3 — EXECUTION       river_job (webhook lane)
```

Layer 2 → 3 (delivery) moved to River in the delivery migration. Layer 1 → 2
(**fan-out**) is still `webhookpub.OutboxWorker` — a hand-rolled background loop
(`internal/webhookpub/worker.go`, started at `cmd/e2a/main.go:683`). It:

- runs a **`LISTEN webhook_events_new` reader** on a dedicated connection with a
  1s/2s/5s/10s/30s reconnect backoff (`listenLoop`), plus a **1s fallback-poll ticker**
  (`tickLoop`);
- **leases** a batch of 32 pending events with `FOR UPDATE SKIP LOCKED` + a
  `next_poll_at` bump and a 5-minute lease, fanning out 8-at-a-time;
- for each event: reads enabled webhooks for the user, matches filters in Go, inserts
  one `webhook_subscriber_deliveries` row per match (`ON CONFLICT (event_id, webhook_id)
  DO NOTHING`), enqueues a River **delivery** job per row in the same tx, then marks the
  event `processed`/`no_match` with a `WHERE status='pending'` guard.

This is **not a durability gap** — `webhook_events` is durable and retry-forever, the
fallback poll covers missed NOTIFYs, and downstream delivery is on River. It is a
**consistency + code-health gap**: it hand-rolls exactly what River already provides
(leasing, lease-expiry reclaim, retry/backoff, LISTEN-driven low-latency pickup,
multi-replica safety), and it is the sole remaining bespoke background loop doing
durable work. The subtle invariants it maintains by hand — the lease-expiry race
(a slow worker B finishing after worker C took over its lease), the per-row
`ON CONFLICT` backstop, and the `WHERE status='pending'` snapshot guard — are exactly
the kind of hand-maintained concurrency reasoning that River's job leasing subsumes.

## 2. Goals and non-goals

**Goals**
- Move fan-out **execution** (lease / retry / concurrency / wakeup) onto River, using
  the **same three-layer, in-tx-enqueue pattern** as inbound (#391) and delivery: the
  trigger writes the `webhook_events` fact row **and** enqueues a `fan_out` River job
  carrying the `event_id`, in one transaction; a `FanOutWorker` re-reads the event,
  matches, inserts deliveries + enqueues delivery jobs, marks the event terminal.
- **Delete** the `LISTEN`/reconnect reader, the fallback-poll ticker, the manual lease
  (`FOR UPDATE SKIP LOCKED` + `next_poll_at`), and the `webhook_events_new` NOTIFY
  trigger — ~200 lines of hand-rolled concurrency.
- **Preserve exactly**: the fan-out matching logic, the `(event_id, webhook_id)` dedup,
  the `processed`/`no_match` event states, the delivery-history API, redelivery/replay,
  and — crucially — the **sub-second fan-out latency** (achievable because River's own
  job pickup is `LISTEN`/`NOTIFY`-driven, not poll-only).
- **Flag-gated cutover** (`E2A_WEBHOOK_FANOUT_MODE`, default `legacy`) with the legacy
  path byte-for-byte unchanged until flipped; a startup + periodic reconciler for
  stranded events (mirrors delivery's `ReconcileWorker`).

**Non-goals**
- Changing `webhook_events` (Layer 1) semantics, the event catalog, filters, signing,
  the SSRF guard, or the delivery-history API shape.
- Changing **redelivery/replay** — it inserts deliveries + enqueues delivery jobs
  directly (one of the "three enqueue sites" from the delivery migration) and does
  **not** flow through fan-out. Untouched.
- Exactly-once. Fan-out is at-least-once; the `(event_id, webhook_id)` unique index is
  the dedup point and stays.
- Touching the outbound / inbound / notify lanes (already migrated).

## 3. Relevant context and constraints

- **Every inbound email flows through this stage** (`email.received` → fan-out), plus
  every outbound `email.sent`/`review_*` event. It is a hot path; cutover is the
  load-bearing risk, exactly as delivery's was.
- **All webhook_events writers go through two methods** — `Outbox.PublishTx`
  (pre-side-effect: `email.received`/flagged/blocked/pending_review/review_rejected) and
  `Outbox.PublishBestEffortTx` (post-side-effect: `email.sent`/review_approved). Both
  already receive the caller's `pgx.Tx`. So the fan-out job can be enqueued **in those
  two methods**, in the same tx as the row — the ~10 trigger call sites
  (`relay/server.go`, `agent/api.go`, `agent/screening.go`, `agent/outbound_async.go`)
  **do not change**. This is the single wiring seam.
- `PublishBestEffortTx` must never fail the caller's tx (the irreversible SES send
  already happened). In-tx job enqueue keeps that contract: if the row commits but the
  enqueue fails, the reconciler re-drives it — same safety net as delivery's separate-tx
  `/test` path.
- River's client picks up newly-inserted jobs via its own `LISTEN`/`NOTIFY` notifier
  (plus a poll fallback) — structurally identical to today's `webhook_events_new`
  listener, so **latency parity is expected**, not a regression.
- Multi-replica: River leasing (`SKIP LOCKED` on `river_job`, attempt/lease columns)
  replaces the hand-rolled lease. The FanOutWorker must stay **idempotent** under
  at-least-once redelivery — the `(event_id, webhook_id)` `ON CONFLICT DO NOTHING` and
  the `WHERE status='pending'` guard already guarantee this and are retained.

## 4. Design

### 4.1 New job + worker (as built: `internal/webhookpub`)

Mirror `internal/webhookdelivery`:

```
FanOutArgs{ EventID string }   Kind() = "webhook_fanout"; routed to QueueWebhook (§4.3)
FanOutWorker.Work(ctx, job):
    ev := loadEvent(EventID)                       // pgx.ErrNoRows → return nil (event GC'd; done)
    if ev.Status != "pending" { return nil }       // already fanned out (idempotent re-run)
    webhooks := ListEnabledWebhooksForRouting(user, type)
    matched := matchFilters(webhooks, ev)          // ported verbatim from fanOutOne
    tx := begin
      for w in matched:
        id := insert wsd row ON CONFLICT (event_id, webhook_id) DO NOTHING
        if inserted: EnqueueDeliveryTx(tx, id)     // reuse the EXISTING delivery enqueuer
      UPDATE webhook_events SET status = matched?'processed':'no_match',
             matched_webhook_ids = … WHERE id = EventID AND status = 'pending'
    tx.commit
```

The body is a near-verbatim lift of `OutboxWorker.fanOutOne` + the status update; only
the leasing/batching wrapper is dropped (River owns it). `EnqueueDeliveryTx` is the
same interface the OutboxWorker already calls — delivery wiring is unchanged.

Retry policy: default River backoff, a bounded `MaxAttempts` (fan-out failures are
transient DB/identity-read errors; a matching bug would fail every attempt and should
surface, not retry-forever). `Timeout()` bounded (a few minutes) — fan-out is a handful
of queries; never the 60s-cut hazard the janitor had.

### 4.2 Enqueue seam (in-tx, in the two Outbox methods)

Add a `FanOutEnqueuer` (`EnqueueFanOutTx(ctx, tx, eventID)`) injected into the `outbox`
struct, and a `fanout_job_id` column on `webhook_events` (migration, §Slice 0). In
`PublishTx`/`PublishBestEffortTx`, **when `fanout_mode == river`**, after
`writeOutboxRow` enqueue the fan-out job in the same tx and stamp `fanout_job_id`. When
`mode == legacy`, behave exactly as today (no enqueue; the OutboxWorker drains). This is
the only conditional; the trigger sites are untouched.

### 4.3 Queue placement — DECISION NEEDED

Two options:
- **(A) Reuse `QueueWebhook`** (recommended default). Fan-out is cheap (a few queries +
  N inserts) and low-volume relative to delivery; sharing the lane is simplest and adds
  no new queue. Risk: a fan-out storm (inbound spike) competes with delivery workers.
- **(B) Dedicated `QueueWebhookFanout`** (isolation-purist, matches the "separate lanes"
  philosophy in `jobs/queues.go`). A fan-out backlog never delays deliveries and vice
  versa, at the cost of a 7th queue + pool tuning.

Recommend **(A)** to start (measure `SetPublisherLag` + delivery lag post-cutover;
promote to (B) only if fan-out is observed to starve delivery). Cheap to change — it's
one `InsertOpts.Queue`.

### 4.4 Reconciler (stranded-event backstop)

Mirror delivery's `ReconcileWorker`: a `QueueMaintenance` periodic (every 1 min) +
a startup pass that re-enqueues any `webhook_events` row with
`status='pending' AND fanout_job_id IS NULL`. Covers the `PublishBestEffortTx`
enqueue-failure window and any crash between row-commit and job-insert. Backed by a
partial index (Slice 0). Idempotent via the `fanout_job_id IS NULL` guard.

## 5. Slice plan (one PR each)

**Slice 0 — schema prep.** Migration: `ADD COLUMN fanout_job_id BIGINT` (nullable,
non-destructive) to `webhook_events`; partial index `(id) WHERE status='pending' AND
fanout_job_id IS NULL` `CONCURRENTLY` (`-- e2a:no-transaction`) for the reconciler scan.
No behavior change. *(Mirrors migrations 057/058 for notify.)*

**Slice 1 — worker behind flag (not wired).** New worker in `internal/webhookpub`
(the draft's separate `internal/webhookfanout` package was not created):
`FanOutArgs`/`FanOutWorker` (body lifted from `fanOutOne`), `Jobs` registrar
(`RegisterJobs` adds the worker + `ReconcileWorker` periodic), `EnqueueFanOutTx`,
`ReconcilePending`. Unit + integration tests against the real fan-out SQL. `E2A_WEBHOOK_FANOUT_MODE`
config (default `legacy`). Nothing enqueues fan-out jobs yet; legacy OutboxWorker still
runs. *Fully dormant.*

**Slice 2 — wire + reconcile (flag still default legacy).** Inject `FanOutEnqueuer`
into `outbox`; `PublishTx`/`PublishBestEffortTx` enqueue in-tx + stamp `fanout_job_id`
when `mode==river`. Register `FanOutWorker`/`ReconcileWorker` on the client; wire the
startup + periodic reconciler in `main.go`. When `mode==legacy`, start the OutboxWorker
as today; when `mode==river`, **don't** start it. Live e2e in `river` mode (Mailpit):
inbound email → `email.received` → fan-out job → deliveries + delivery jobs → POST.
Assert latency parity + dedup on re-drive.

**Slice 3 — cutover.** Flip `E2A_WEBHOOK_FANOUT_MODE=river` in e2a-ops. Verify
`SetPublisherLag`, delivery lag, no `webhook_events` stuck `pending`. (Reversible: flip
back to `legacy`; the OutboxWorker re-drains any pending — both paths honor the same
`status='pending'` guard + `ON CONFLICT`.)

**Slice 4 — delete legacy.** Remove `OutboxWorker` (`listenLoop`, `tickLoop`, lease
logic, `notifyCh`), the `webhook_events_new` NOTIFY trigger + its migration (or leave
the trigger inert), the `pg_notify` call in `writeOutboxRow`, and the `mode` flag (River
becomes unconditional, matching how delivery ended up). ~200 lines deleted.

## 6. Testing

- **Unit** (`webhookpub`): fake identity reader + fake delivery enqueuer — matched /
  no_match / multi-match; idempotent re-run (second Work is a no-op via status guard);
  `ErrNoRows` (GC'd event) → nil.
- **Integration** (real PG): enqueue a fan-out job → N `wsd` rows + N delivery jobs;
  re-run the job → no duplicate rows (`ON CONFLICT`), status stays `processed`;
  `no_match` path; reconciler re-drives a `pending`/`fanout_job_id IS NULL` row.
- **e2e** (Mailpit): inbound + outbound events fan out end-to-end in `river` mode;
  `legacy` mode byte-for-byte unchanged; A/B latency check vs the LISTEN worker.
- **Cutover safety**: an event written under `legacy` then flipped to `river` (and
  vice-versa) is fanned out exactly once (dedup index proves it).

## 7. Risks

- **Hot path.** Every received/sent event fans out here. Mitigated by the flag +
  reconciler + one-line rollback, the same discipline that made delivery/inbound safe.
- **Double fan-out across cutover.** Prevented by the `(event_id, webhook_id)` unique
  index + `WHERE status='pending'` guard — identical to today; a legacy drain and a
  River job racing the same event both no-op the loser.
- **Latency.** Expected parity (River notifier). Slice 2 measures it before cutover; if
  River pickup lags, keep `legacy` and revisit — no forced cutover.
- **Queue contention** (if §4.3(A)): watched via lag metrics; promote to a dedicated
  lane if needed.

## 8. Open questions

1. §4.3 queue placement — reuse `QueueWebhook` (A, recommended) vs dedicated lane (B)?
2. Keep `E2A_WEBHOOK_FANOUT_MODE` as a permanent seam, or delete it in Slice 4 like
   delivery did (River unconditional)? Recommend delete — one execution model.
3. Retire the `webhook_events_new` trigger in Slice 4, or leave it inert as a cheap
   belt-and-suspenders? Recommend retire — nothing reads it once the listener is gone.
