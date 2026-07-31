# Async Message Pipeline — Architecture Design (Outbound + Inbound)

**Status:** Approved, hardened after adversarial launch review (2026-07-04); **reconciled onto River (2026-07-06)** now that the webhook delivery migration (#387) shipped and River is the shared job substrate on main. Companion to `async-send-contract.md` (the outbound contract spec; the inbound changes have no API-contract surface). **Decided 2026-07-03: at-least-once is a pre-GA blocker in BOTH directions.** Outbound slices land before the /v1 GA freeze and GA ships with async as the default outbound path; the inbound minimal fix (slice I1) is pre-GA, the inbound queue (slice I2) post-GA.

> **GA resolution (2026-07-15):** the outbound cutover is complete. River-backed,
> persist-first delivery is unconditional; the outbound mode flag and synchronous
> submit-inline path no longer exist.

> **⟳ RECONCILED ONTO RIVER (2026-07-06) — authoritative where it conflicts with the pre-River draft below.**
>
> The pre-River draft hand-rolled an `outbound_message_queue` table with `FOR UPDATE SKIP LOCKED` claim, a `lease_token` fencing token, ~60s heartbeat lease extension, and a sweeper for crash takeover — and the 2026-07-04 launch-review hardening (§§4–8) was almost entirely lease/fence/heartbeat machinery. **River provides all of that natively** (the draft even cited River's `attempted_by` as the fencing pattern it imitated). The webhook→River migration (#387) proved out the exact pieces this needs: the shared `internal/jobs` client, `jobs.Registrar`/`Enqueuer`, named queues (`QueueOutbound` already exists), the transactional-outbox enqueue (`InsertTx` in the business tx), a custom `NextRetry` retry envelope, `JobSnooze` for defer-without-burning-an-attempt, and a periodic reconciler backstop.
>
> **A note on "lease":** River does **not** have a renewable/heartbeat lease (the graphile-worker model the draft imitated). It has **job ownership** — on fetch a job goes `running` with `attempted_by`, and no other client touches it — enforced not by a short renewable TTL but by a coarse periodic **`JobRescuer`** that re-drives jobs stuck in `running` longer than **`RescueStuckJobsAfter`**. This doc says "claim + rescue," not "lease," deliberately: it is precisely the *absence* of a short renewable lease that removes the fencing/heartbeat problem (there is no lease to expire out from under a live worker).
>
> **What changes vs the draft:**
> - **No queue table, no `lease_token`, no heartbeat, no sweeper.** The job is `river_job` on `QueueOutbound`; the message row carries the payload, the job args carry just `{message_id}`. River owns claim / retry / rescue.
> - **One SMTP attempt per job attempt — River owns the retry envelope.** Do NOT port `sender.Send`'s internal ~4-attempt / ~6.5-min loop into one `Work()`. Each `Work()` does a **single** SMTP submit (~2-min worst case) and returns an error on failure; River drives the multi-attempt envelope via `NextRetry`. This is the idiomatic shape and it dissolves the rescue-window tension: because `Work()` is short, `RescueStuckJobsAfter` can be **small (~5 min)**, so crash recovery is fast *and* a live send is still never near the rescue threshold. The retry envelope also becomes observable in `river_job` (attempt, `scheduled_at`) instead of hidden in the relay loop.
> - **The scariest section dissolves.** The draft's crash-matrix **row 4** (a slow-but-alive worker over its lease double-submitting — the entire reason for the 12-min lease + heartbeat) cannot happen: with one short attempt per `Work()` and `RescueStuckJobsAfter` above that single-attempt time, a live send is never reclaimed. **No fencing token is needed:** River claims each job for one worker, and the only app write (`MarkMessageSent`) is idempotent, so even a pathological rescue of a genuinely-stuck job degrades to a harmless double-mark, not state corruption.
> - **`JobSnooze` replaces the two "don't burn an attempt" cases** — provider-connection errors (SES outage) and `sending_ramp_limited` deferrals both snooze without incrementing `Attempt`.
> - **The operator pause switch is `river.QueuePause`** (native), not a hand-rolled `outbound.paused` flag.
> - The terminal-failure guard, error classification, `X-E2A-Message-ID` reconciliation header, and durable outbox event emission stay as **application logic**.
>
> **What is unchanged:** the normative guarantee, the sync/async split (§2), the message state machine (§3), the contract linkage, the crash-matrix *reasoning* (§7, minus row 4), the unification wins (§6), and the entire inbound §9 (I1 is engine-agnostic; I2's `inbound_message_queue` becomes River `QueueInbound` — a mechanical swap of the same kind).

**Guarantee (normative — the GA blocker):**
- **Outbound:** once e2a returns 200 `accepted` on a send, it has durably persisted the message and MUST attempt SMTP delivery until the provider accepts or a terminal failure is declared (retries exhausted / permanent rejection → `delivery_status='failed'`). The outcome — `email.sent` or `email.failed` — is written to the durable event log (`webhook_events`, same transaction as the status write) and delivered to subscribers with at-least-once semantics.
- **Inbound:** e2a MUST NOT reply SMTP 250 until the message is durably persisted; on persist failure it replies 451 so the upstream MTA retries (SMTP's native durable-retry, the mirror of the API caller's idempotent retry). MTA retries dedupe on a content-aware key (§9). Once 250 is issued, the message reaches the agent's durable push path (event log → webhook) at-least-once.

No accepted message — in either direction — is ever silently dropped; every stage is either transactional or (under River) claimed-and-re-driven.

## 0. System overview — two lanes, shared tail

```
Inbound:   MX/SMTP receiver → river_job(QueueInbound) → parse worker ─┐
                                                                      │
Outbound:  API → river_job(QueueOutbound) → send worker → SES ────────┤
                                                                      ▼
        producers PublishTx only ──▶  webhook_events  ──[OutboxWorker drain]──▶  webhook_subscriber_deliveries  ──▶  river_job(QueueWebhook)  ──▶  customer webhook
              (L1 event log,                                              (L2 delivery state)                        (L3 execution)
               backs GET /v1/events)

Post-send feedback:   SES → SNS → delivery-feedback consumer → webhook_events (email.delivered / deferred / bounced / complained)
```

**The shared tail is the #387 three-layer webhook pipeline — a producer's only job is to `PublishTx` an event into `webhook_events` in its business transaction.** It does NOT create `webhook_subscriber_deliveries` or enqueue webhook jobs; the **OutboxWorker drain** does that (fan-out to matching subscribers + `InsertTx` the `webhook_deliver` job). Every lane is a River job on the shared `internal/jobs` client — one substrate, one set of named queues (`QueueOutbound`, `QueueInbound`, `QueueWebhook`, `QueueMaintenance`), one retry/rescue model. `webhook_events` is deliberately shared, not per-direction (events carry `direction`). §§1–8 design the outbound lane; §9 the inbound lane; §§10–12 rollout, testing, and open questions.

## 1. Outbound lane: current vs target

**Current (direct `/v1` send, `internal/agent/api.go` DeliverOutbound):**
```
request → screen (incl. LLM scan) → SES submit → meter → persist row → 200 sent
```
Synchronous SES inside the request (up to a ~6.5-min worst case — 4 attempts × up to a 2-min session deadline + 30s dial + 1s/5s/15s backoff sleeps, `internal/outbound/smtp_relay.go`), message row and `msg_` id created only AFTER SES accepts, insert failure swallowed (returns 200 anyway). Crash after SES-accept = sent + billed email with no DB trace, unrecoverable. The HITL **approve** path already has the correct shape (durable pending row + `send_attempts` WAL + `hitlworker` re-drive) — this design generalizes that shape to all outbound, on River.

**Target:**
```
request:  auth → validate → recipient-policy gate → template render → quota + N tokens
          → ONE TX: mint msg_id, insert messages row (delivery_status='accepted')
                    + InsertTx a River job on QueueOutbound {message_id}
                    + idempotency Complete (§6)
          → 200 {message_id, status: accepted}                     [low tens of ms]

worker:   River claims the job (one worker, attempt N) → load message row
          → [already terminal? no-op — idempotent re-drive]
          → content scan → [hold → pending_review, JobCancel/no-op]
          → ramp Reserve → [ramp-limited → river.JobSnooze(until rollover), attempt NOT burned]
          → compose MIME (X-E2A-Message-ID header) → SMTP submit to SES
          → ONE TX: MarkMessageSent + provider_message_id + meter + email.sent via outbox PublishTx
          → return nil ⇒ River completes the job
          (provider-connection error → river.JobSnooze(backoff), attempt NOT burned;
           app/permanent error → return err ⇒ River retries per NextRetry;
           final attempt failed → terminal-failure guard → MarkFailed + email.failed)

rescue:   a crashed worker's job is re-driven by River after RescueStuckJobsAfter
          (tuned below the SMTP envelope's headroom); re-drive is idempotent.

feedback: SES → SNS → /webhooks/ses → per-recipient rollup (already built, unchanged)
```

## 2. Sync/async split — the line is "no network I/O before the 200"

**Synchronous (all local):** auth; schema validation; recipient-policy gate (cheap DB read — disallowed recipient stays an immediate 403); template render (in-process mustache; render-before-hold ordering preserved); suppression check; monthly-quota N-check; token-bucket debit; the accept transaction (persist + `InsertTx` job + idempotency `Complete`).

**Asynchronous (River worker):** LLM/PI content scan and the hold it may produce; sending-ramp reservation; MIME compose; SMTP submit; provider-id capture; usage metering; `email.sent` emission.

**Product consequence (decided):** holds return **200** from GA (`body.status: pending_review`, not 202 — 200 iff persisted, contract §1.5). Through GA the scan runs synchronously before the accept-tx, so the hold is known at request time and the 200 carries `pending_review`. When slice 4 (post-GA) moves scanning into the worker, the request-time response for a to-be-held send becomes 200 `accepted` with `email.pending_review` following as an event — the HTTP code stays 200 either way. The recipient-policy 403 stays sync. A scan **block** needs a durable representation (today block is rowless 403): add `blocked` to `messages.status` (or map onto the review-rejected family — decide in slice 2), emit a terminal `email.failed{reason:blocked}` (contract §1.3).

## 3. Message state machine

`messages.delivery_status` (contract doc §3):
```
accepted → sending → sent → delivered
                   ↘ failed            ↘ deferred → delivered/bounced
        ↘ (scan hold: messages.status=pending_review; River job cancelled;
           approval re-enqueues a new job → accepted)   ↘ bounced / complained
```
Transitions: the accept-tx writes `accepted`; the worker writes `sending` when it starts real work (a plain UPDATE — no lease token, River owns exclusivity); worker success writes `sent` (existing `MarkMessageSent`); the terminal-failure branch writes `failed`; the SNS consumer owns everything after `sent` (existing monotonic `delivery.Merge`, `internal/delivery/status.go` — extend precedence to include `accepted`/`sending` below `sent`). While held for review, `delivery_status` stays `accepted`; the hold lives on `messages.status='pending_review'` (contract §3.1) — the two lifecycles keep their own columns.

## 4. Queue: River on `QueueOutbound`

No new table, no broker, no hand-rolled lease. The outbound job is a `river_job` on the existing `QueueOutbound` lane, registered via a `jobs.Registrar` exactly like `webhookdelivery.Jobs` (#387).

```go
type OutboundSendArgs struct { MessageID string `json:"message_id"` }
func (OutboundSendArgs) Kind() string { return "outbound_send" }
```

The job args carry only the `message_id`; the payload (raw/rendered body, recipients, envelope) is already on the `messages` row — the worker loads it (mirrors `WebhookDeliverArgs{DeliveryID}` loading the L2 row).

- **Transactional outbox enqueue (the persist↔enqueue atomicity):** the accept-tx mints `msg_id`, inserts the `messages` row (`delivery_status='accepted'`), and `enq.InsertTx`es the `OutboundSendArgs` job **in the same transaction** — persist and enqueue cannot diverge. This is the identical pattern the outbox drain uses to enqueue webhook deliveries (#387 slice 2). The idempotency `Complete` commits in this same tx (§6).
- **Claim + rescue → River, not a lease.** River claims each job for exactly one worker (`attempted_by`, `attempt`) and will not hand it to another until it completes, errors-and-reschedules, or is **rescued** as stuck. We implement no `lease_token`, no claim query, no heartbeat, no sweeper. River has no short *renewable* lease at all — the draft's fencing token existed to stop a slow-but-alive worker *whose short lease expired* from corrupting state after a takeover; with no such lease to expire, that takeover can't occur (see rescue window), and the one app write it guarded (`MarkMessageSent`) is idempotent — there is nothing to fence.
- **One SMTP attempt per `Work()`; River owns the retry envelope.** Each job attempt does a **single** SMTP submit (~2-min worst case: session deadline + dial), **not** `sender.Send`'s internal ~4-attempt / ~6.5-min loop. On a retryable failure `Work()` returns an error and River reschedules the *next* attempt per a custom `NextRetry` (e.g. 30s / 2m / 10m / 1h / 4h …, `MaxAttempts` default 6 for app/permanent errors) — the same override `webhookdelivery` uses. This needs a **single-attempt send path** in `internal/outbound/smtp_relay.go` (a small refactor to expose one attempt without the internal loop). Two wins: `Work()` stays short (so the rescue window can be tight, below), and the retry envelope becomes observable in `river_job` (`attempt`, `scheduled_at`) instead of hidden inside the relay loop.
- **Rescue window (small, because `Work()` is short).** River's `JobRescuer` re-drives a job stuck in `running` past `RescueStuckJobsAfter`. Because one `Work()` is a single ~2-min attempt rather than a 6.5-min loop, set `RescueStuckJobsAfter` to **~5 min** — comfortably above one attempt (so a live submit is never reclaimed), well below the draft's 12-min lease → **fast crash recovery**. **Constraint (pin + test in slice 3): `RescueStuckJobsAfter` MUST exceed the worst-case single-attempt time**, or River could rescue a live submit and cause a double-send. This is the only lease-adjacent knob that survives; the heartbeat is gone entirely.
- **Defer without burning an attempt → `river.JobSnooze`.** Two cases the draft handled with "does not increment attempts" map exactly onto `JobSnooze`, which reschedules the job **without** counting the attempt:
  - **`sending_ramp_limited`:** `river.JobSnooze(untilRampRollover)` — a ramp deferral must not burn the retry budget or a domain mid-ramp gets falsely `failed`.
  - **Provider-connection errors** (dial timeout, connection refused, SMTP 4xx) during an SES/SNS outage: `river.JobSnooze(outageBackoff)` — the outage defers rather than fails, giving the 8–72h industry retry horizon without exhausting `MaxAttempts`. This is the §8 circuit breaker's core mechanism, now free.
- **Terminal-failure guard (app logic, stays).** On the **final** attempt (`job.Attempt >= MaxAttempts`) with a failed send, before writing terminal `failed` + `email.failed{retryable:false}`, the worker MUST first check for provider-accept evidence (an SES `X-E2A-Message-ID`-tagged Send/Delivery notification) — declaring a delivered message `failed` is the uncorrectable error (§7). This is the same shape as `webhookdelivery`'s last-attempt branch, plus the provider-evidence check. `delivery.Merge` gets an explicit **exception: header-matched provider `sent`/`delivered` feedback overrides a local `failed`** (else a falsely-declared terminal `failed` is uncorrectable).
  **Implemented (2026-07-16).** Mechanics: (1) `Sender.SubmitOnce` stamps `X-E2A-Message-ID: <msg_id>` on the wire at submit time (post-DKIM, like the config-set header — submit-time stamping means pre-existing accepted rows get it on re-drive); (2) the SNS consumer, after signature verification + correlation (provider id, falling back to the header echo in `mail.headers`), records `messages.provider_accepted_at` for every post-acceptance notification kind and repairs a missing `provider_message_id`; (3) the worker's claim reports that evidence, so a re-driven job **settles the row as sent instead of re-submitting** (closing the duplicate residual when SNS beats the re-drive); (4) the store's guarded terminal write (`MarkFailed` in the worker AND the terminal reconciler) settles an evidence-bearing row as sent (+ `email.sent`, metered) and refuses to fail it (`provider_accepted_at IS NULL` in the failure CAS); (5) a final attempt that fails **ambiguously** defers its terminal write (`DeferTerminalFailure`: stores the diagnostic, releases the claim) and the one-minute terminal reconciler declares the outcome only after a **15-minute provider-evidence grace** on `river_job.finalized_at` — evidence → sent, none → `failed{source:local}` + exactly one `email.failed` (deterministic event id). Failure provenance (`delivery_failure_source`) makes only locally inferred failures correctable — see contract §3.1.
  **Ops requirement:** the header echo exists only when the SES configuration set / notification settings enable **"include original headers"** (`mail.headers` in the SNS payload). Without it the system degrades safely — provider-id correlation still works for everything after mark-sent, but crash-window rows can't be header-correlated, so enable it in e2a-ops as part of this rollout.
- **Terminal success/complete.** On success the worker returns `nil` and River marks the job `completed`; River prunes completed jobs on its own schedule (no manual DELETE). The durable record is `messages.delivery_status`, not the job.
- **`ON DELETE CASCADE` caveat is gone** for the queue (there is no FK'd queue row). Every irreversible message-owning deletion path (`PurgeMessage`, `DeleteAgent`, `PurgeDeletedAgents`, `DeleteExpiredMessages`, and user-data deletion) must still **cancel each linked live River send job in the same transaction**. If cancellation fails, the deletion rolls back; if River already pruned the job, treat that as success. Reversible trash deliberately retains the job so restore before `scheduled_at` can re-arm it; restore at/after that instant cancels the past-due job and restores only the message/inbox.

## 5. Worker

A River `Worker[OutboundSendArgs]` registered on `QueueOutbound` (concurrency via the queue's `MaxWorkers`, config `outbound.workers`, default ~8). No goroutine pool of our own, no poll loop — River's client owns fetching and `LISTEN/NOTIFY` wake-up.

`Work(ctx, job)`:
1. **Load** the `messages` row by `job.Args.MessageID`. Gone (cascade/TTL) → return `nil` (idempotent no-op). Already `sent`/terminal → return `nil` (idempotent re-drive after a crash post-mark-sent).
2. **Content scan** → hold/block/allow (reuses `screenOutbound` minus the recipient gate already done sync). Hold → set `messages.status='pending_review'` and `river.JobCancel` (approval later enqueues a fresh job — §6). Block → terminal `email.failed{reason:blocked}` (contract §1.3). *(At GA the scan is sync pre-accept, so this step is a no-op until slice 4 moves it here.)*
3. **`rampGate.Reserve`** → ramp-limited → `river.JobSnooze(untilRampRollover)` (attempt not burned).
4. **Compose** MIME with the `X-E2A-Message-ID: <msg_id>` header (`internal/outbound/compose.go` currently omits any e2a id — add it; it is what makes the SES-accept↔mark-sent residual reconcilable).
5. **SMTP submit** — a **single** attempt (the single-attempt path on `sender.Send`, §4), not its internal retry loop. River owns re-attempts.
6. **On success — one transaction:** `MarkMessageSent` + `provider_message_id` + meter (`recordOutboundUsage` moves here, post-success, fixing the bill-before-persist bug) + `email.sent` via the webhook outbox `PublishTx`. Return `nil` ⇒ River completes the job. Bundling mark-sent + meter + event into one tx makes crash-matrix row 6 a clean idempotent no-op on re-drive. **No fencing `WHERE lease_token` guard is needed** — River gives single-worker ownership (there is no renewable lease to expire under a live worker), and `MarkMessageSent` is idempotent.
7. **On retryable app / permanent-provider failure:** return an error ⇒ River retries per `NextRetry` (counts the attempt). **On provider-connection/outage error:** `river.JobSnooze(backoff)` (does not count). **On the final attempt failed:** run the terminal-failure guard (§4), then `MarkFailed` + `email.failed` via outbox `PublishTx`, and return the error so River discards.
- **Durable outcome events — the worker's *only* event action is one `PublishTx`.** On success the worker writes `email.sent` to `webhook_events` in the same tx as the mark-sent write; the terminal-failure branch writes `email.failed` in the mark-failed tx. That is all — the worker does **not** create `webhook_subscriber_deliveries` and does **not** enqueue webhook jobs. The **OutboxWorker drain** (the #387 tail) fans the event out to matching subscribers and `InsertTx`es the `webhook_deliver` jobs, at-least-once. Never the fire-and-forget legacy publisher (which #387 deleted). Mandatory whenever `outbound.mode=async` (§10).
- **The send worker emits ONLY `email.sent` / `email.failed`.** It knows just whether the provider *accepted the submission* (`sent`) or the submission *terminally failed* (`failed`). Everything after acceptance — `email.delivered` / `email.deferred` / `email.bounced` / `email.complained` — is **not** a send-time outcome: it arrives later from **SES → SNS → the delivery-feedback consumer** (`deliveryEventFirer`, already built and, post-#387, routed through the same outbox). Do not emit deferred/bounced/delivered from the send path. (`email.accepted` is not emitted at all today — see §12 open question.)
- **SES-accept vs mark-sent window (the one true residual):** if a crash lands between the SMTP accept and the mark-sent tx, the River re-drive MAY duplicate — at-least-once's irreducible residual (an accepted SMTP command cannot be recalled). The `X-E2A-Message-ID` header makes it detectable via SNS. **This is now the *only* duplicate window** — one short attempt per `Work()` plus `RescueStuckJobsAfter` set above that attempt means a live submit is never reclaimed, so the draft's slow-worker-takeover duplicate (row 4) is gone entirely.
- **Graceful shutdown:** `jobsClient.Stop(shutdownCtx)` drains in-flight jobs up to the shutdown budget (the pattern #387 already uses). Anything still sending when the budget expires dies into the SES-accept↔mark-sent residual and is re-driven on restart — size the shutdown budget against the SMTP envelope.
- **Ordering:** no FIFO guarantee, including within a conversation — document it (River is not ordered; add per-conversation sequencing later only if it ever matters).

## 6. Unification wins

- **Approve path collapses onto River.** Today `ApproveAndSend` has its own WAL (`send_attempts`, migration 015) and re-drive (`hitlworker`). Post-migration: approve = transition the message back to `accepted` + `InsertTx` an `OutboundSendArgs` job. `send_attempts` machinery is retired after the transition (keep the table; stop writing). This is the same "one queue, one worker" collapse the webhook migration did to `SubscriberRetryWorker`.
- **`wait=sent`** = after the accept-tx, poll the `messages.delivery_status` (or subscribe via `LISTEN/NOTIFY` on the row) up to the ~10s/≤20s ceiling, then return current state. Reads the message row, not River internals (River job state is an implementation detail; `delivery_status` is the contract surface). CLI `send` uses it by default (frozen exit-code contract).
- **Batch send** becomes: sync-validate all items → in ONE tx, persist N message rows + `InsertTx` N jobs + one batch idempotency `Complete` → 200 with N `accepted` items + `batch_id`. Crashed batch = jobs River drives to completion; no intent journal.
- **Idempotency simplifies:** with persist-first, "same key + same body → same `message_id`, never a second send" becomes strictly true — the key's `Complete` commits **inside the same transaction as whichever durable acceptance point fires** (the accept-tx for allowed sends AND `HoldForApprovalCore`'s hold insert for review-held sends, `internal/agent/api.go:1230-1243`; a held send never reaches the accept-tx, so slice 2 must commit `Complete` in the hold tx too, caching the 200 `pending_review`). The dead `markSideEffectCommitted` code gets deleted. (Contract §5.1 carries the caller-facing scope/TTL caveats.)

## 7. Crash matrix (target, on River)

Exactly-once holds for every row **except** the SMTP-accept↔mark-sent window, which is at-least-once by nature. **The draft's slow-worker-takeover row is removed** because one `Work()` is a single short SMTP attempt and `RescueStuckJobsAfter` sits above it — a live submit is never reclaimed — so there is no fencing token to reason about, only River's single-worker claim + idempotent re-drive.

| Crash / fault point | Outcome |
|---|---|
| Before accept-tx commit | Nothing durable; caller got no 200; retry safe (idempotency key replays nothing) |
| After accept-tx commit, before 200 reaches caller | `messages` row + River job exist; the worker sends; caller's same-key retry replays `accepted` + same `msg_id` — exactly-once |
| Worker crash before SMTP submit | River rescues the stuck job after `RescueStuckJobsAfter` (~5 min) → re-run; message not sent, so re-run is clean — exactly-once (latency = the rescue window, now small because `Work()` is one short attempt) |
| ~~Worker slow (not crashed) over-runs its claim, mid-`Send`~~ | **Eliminated by River.** `RescueStuckJobsAfter` is set above one SMTP attempt, so a live submit is never reclaimed; no second worker, no double-submit. (This was the draft's load-bearing residual + the entire reason for the `lease_token` + heartbeat.) |
| Crash after SMTP accept, before mark-sent tx | River re-drives → MAY duplicate (the irreducible residual); `X-E2A-Message-ID` makes it reconcilable via SNS. Common trigger: a deploy whose shutdown budget expires mid-send. |
| Crash after SES-accept on the FINAL attempt | Without the guard: the terminal branch declares `failed` + `email.failed{retryable:false}` for a message SES accepted, and `delivery.Merge` ranks `failed` above `delivered` → uncorrectable. **With the terminal-failure guard: check header-tagged provider-accept evidence before declaring `failed`, and the `delivery.Merge` exception lets header-matched `sent`/`delivered` override a local `failed`.** |
| Crash after mark-sent tx (before River marks the job complete) | River re-drives → worker loads the message → sees `delivery_status='sent'`/terminal → no-op; `email.sent` already committed (deterministic id, `ON CONFLICT DO NOTHING` on re-emit) |

## 8. Backpressure & limits

Quota N-check and token-bucket debit happen at accept time, so the queue is bounded per agent by the same budgets as today (accepting ≠ bypassing limits). Ramp throttling no longer fails requests — it `JobSnooze`s to the ramp-day rollover. A terminally-`failed` message has already consumed quota + a token at accept: decided, no automatic refund in v1 (document it).

**SES-outage circuit breaker — mostly free under River.** A multi-hour regional SES/SNS incident must not exhaust every job's `MaxAttempts` and mass-fire false `email.failed{retryable:false}`. Three mitigations:
1. **Error classification → `JobSnooze`** (§4): provider-connection errors snooze without counting attempts, so an outage defers rather than fails. This replaces the draft's separate "outage-tolerant tail."
2. **Operator pause → `river.QueuePause(ctx, jobs.QueueOutbound)`** (native): stops the outbound workers claiming without stopping the binary or the other lanes; `QueueResume` to lift it. This replaces the draft's hand-rolled `outbound.paused` config/SIGHUP switch — River gives it for free and it's observable in River's queue state.
3. **Admin re-queue** for terminally-`failed` messages = `InsertTx` a fresh `OutboundSendArgs` job (a runbook path / small admin endpoint).

**Per-tenant fairness — the one thing River does not give for free (OPEN, decide in slice 3).** Accept-time budgets bound queue *growth* per agent but not *contention*: one tenant's 100-job batch fills the head of `QueueOutbound` and starves others behind a fixed ~8-worker drain. River fetches roughly FIFO-with-priority per queue, not per-tenant-fair. Options, in preference order: (a) **accept-time per-agent in-flight cap** — bound how many `accepted`/`sending` jobs an agent may have outstanding, rejecting/deferring at accept (bounds contention at the source, fully River-compatible, simplest); (b) River **priority** or **multiple sub-queues** by tenant class (coarse); (c) a custom fetch/claim shim (defeats the "let River own claiming" win — avoid). **Requirement fixed: no pure-FIFO global drain.** Recommendation: (a).

**Observability.** River exposes queue depth, running/available/retryable/discarded counts, and rescue counts per queue — most of the draft's metrics list comes from River's own tables. App-level, still emit: **`email.failed` rate** (SES-outage early-warning), **`JobSnooze` rate** on `QueueOutbound` (outage/ramp-defer signal — the River analog of the draft's lease-takeover-rate precursor), and outbox **drain lag**. Alert thresholds live in the e2a-ops runbook. Optional accept-time global max-depth guard returning 503 `queue_full` (config, default off).

## 9. Inbound lane

**Current (`internal/relay/server.go`):**
```
MX/SMTP → RCPT gates (550 unknown recipient / 552 quota) → DATA:
  SPF/DKIM/DMARC → screen (incl. LLM scan — network I/O inside the open SMTP session)
  → persist messages row [+ events same-tx via outbox] → fan-out → ALWAYS reply 250
```
Three gaps, mirroring outbound's send-then-persist disease:

1. **250-before-durable.** `Data()` replies 250 regardless of persist failure — a `CreateInboundMessage` error is logged and dropped. A DB hiccup = acknowledged-and-lost mail. SMTP is the one protocol where durable retry is free (senders retry a 451 for days); the lever exists and is unused.
2. **No dedupe under MTA retry.** The `msg_` id is minted randomly per delivery attempt and there is no unique index on the stored RFC `Message-ID` — so the moment gap 1 is fixed and MTAs start retrying, each retry creates a duplicate row + duplicate events. Gaps 1 and 2 must ship together.
3. **Fire-and-forget event enqueue in legacy mode** — already closed on main: the outbox is unconditional after #387, and the relay writes the message + all events + River webhook-delivery enqueue in one tx. *(This gap is retired; keep the note for history.)*

**Pre-GA minimal fix (slice I1 — the guarantee, not the architecture; engine-agnostic, no River):**
- Propagate persist failure up through `deliverMessages` to `Data()` → reply **451** (transient; sender owns the retry). RCPT-time gates unchanged.
- **Content-aware, retry-horizon-bounded dedupe — NOT Message-ID alone.** Message-ID equality does not imply message identity (buggy senders reuse ids; forwards preserve them) and Message-ID is attacker-controlled (a guessed id becomes a mail-suppression primitive). Dedupe key = `(agent_id, email_message_id, body_hash)` where `body_hash` digests the raw message; on conflict reuse the row (a true MTA retry is byte-identical), otherwise **insert a new row**. Scope the key to the MTA retry horizon (~7 days). No-Message-ID messages dedupe on `(agent_id, body_hash)`.

**Target (slice I2, post-GA): River `QueueInbound` + parse worker.** The same swap as outbound — replace the draft's hand-rolled `inbound_message_queue` table with a River job whose args carry the raw payload (it is not on a `messages` row yet at DATA time):

```go
type InboundParseArgs struct {
  RawMessage []byte   `json:"raw_message"`
  BodyHash   []byte   `json:"body_hash"`   // retry-horizon dedupe (I1)
  MailFrom   string   `json:"mail_from"`
  RcptTo     []string `json:"rcpt_to"`
  ClientIP   string   `json:"client_ip"`   // captured at DATA time; SPF cannot be recomputed later
  Helo       string   `json:"helo"`
}
func (InboundParseArgs) Kind() string { return "inbound_parse" }
```

> **As built (supersedes the sketch above):** slice I2 shipped per
> `inbound-message-pipeline-river.md`'s intake-row design — the raw MIME lands in
> an `inbound_intake` row (migrations/056_inbound_intake.sql) at DATA time and the
> River job carries only the id: `InboundProcessArgs{ IntakeID }`,
> `Kind() = "inbound_process"` (`internal/inboundprocess/worker.go`). The
> payload-in-job-args sketch above was not built.
- **DATA handler shrinks to:** read body → `InsertTx` the parse job (dedupe on `(agent_id, body_hash)` via `InsertOpts.UniqueOpts` or an app pre-check) → 250. The LLM scan leaves the SMTP session entirely.
- **Parse worker:** claim (River) → SPF/DKIM/DMARC from the stored connection metadata → parse/thread → screening incl. LLM scan → per-recipient-agent `messages` rows + events (`email.received`/`pending_review`/`blocked`/`flagged`) in one outbox tx → best-effort WS live-tail → return `nil`.
- **Terminal dispositions.** River's states cover most of the draft's `parked`/`dropped` split: a **poison message** (parser-breaking MIME) → return an error until `MaxAttempts`, then River **discards** it — plus an alert (River's discarded-job count is the "parked, needs a code fix" signal). A **recipient deleted between the 250 and parse** → `river.JobCancel` (truly terminal, do-not-retry) + a terminal audit row. Discarded (poison, retry-after-fix) vs cancelled (gone, never retry) preserves the draft's distinction on River primitives.
- **Data-deletion sweep.** A River job for an offboarded user holds their raw mail in `river_job.args`. The user-data-rights deletion path MUST cancel/delete that account's pending `inbound_parse` jobs too.
- **Alerting** on discarded/old inbound jobs is first-class (a count+age threshold on River's inbound-queue state).

## 10. Rollout slices (each a reviewable PR; outbound 1–3 + inbound I1 pre-GA, rest post-GA)

1. **Contract + honesty fixes (pre-GA)** — everything in `async-send-contract.md` (enum/param/events/spec regen; §7 unswallow the insert error; meter-after-persist). Engine-agnostic. **This is already open as PR #385** (`contract-async-send`) — a synchronous server fully satisfies it, so it merges independently of (and before) the River queue slices below.
2. **Persist-first on River (pre-GA)** — register `outbound.Jobs` as a `jobs.Registrar` (`OutboundSendArgs` worker on `QueueOutbound`); reorder `DeliverOutbound` to the accept-tx (persist `messages` row + `InsertTx` job + idempotency `Complete` in the same tx — **both** the accept-tx and `HoldForApprovalCore`'s hold insert, §6). The worker code path is the send logic. Because River makes async natural, this slice can **be async** with `wait=sent` as the sync-compat bridge (the pre-River draft split this into "inline slice 2" + "async slice 3" only because a hand-rolled queue made inline the smaller step; on River there is no such asymmetry). Includes the custom `NextRetry`, `JobSnooze` for ramp/provider deferrals, and the terminal-failure guard. **Behavior change (correct):** once the accept-tx commits, a transient SES failure returns **200 `accepted`** (the send is River's job now), not today's 500 — returning 500 would make the caller retry into a duplicate. Only pre-accept-tx failures stay synchronous errors.
3. **Async default + operational hardening (pre-GA — GA freezes on this as the default path)** — `RescueStuckJobsAfter` tuned + tested above the single-attempt SMTP time (~5 min); the single-attempt send path in `smtp_relay.go`; error-classified `JobSnooze` backoff; `river.QueuePause` pause switch + admin re-queue; **per-tenant-fair accept-time in-flight cap (§8, the one real design decision)**; `email.failed`; `LISTEN/NOTIFY`-backed `wait=sent`; config `outbound.mode: sync|async` (sync = pre-slice-2 behavior, kept one release as an escape hatch). **The durable outbox is not a separate flag in async mode:** it is unconditional on main after #387, so `outbound.mode=async` simply relies on it; validate at startup that the outbox path is present.
4. **Async scan + hold/block states (post-GA)** — move screening into the worker; the `blocked` status decision (reserve it in the vocabularies now, contract §3.1); the block/reject/expiry **terminal signal** (`email.failed`-with-reason, contract §1.3).
5. **Approve-path unification (post-GA)** — approve = re-enqueue an `OutboundSendArgs` job; retire `send_attempts` writes; delete `markSideEffectCommitted`. Until this lands, scope contract §4.5's durability to direct sends and note approved-hold events as best-effort (or pull them onto the outbox earlier).
6. **Batch endpoint (post-GA)** — atomic insert-N + `InsertTx` N jobs; honor the §8 per-tenant cap so a 100-job batch can't starve the pool.

**Migration safety (per CLAUDE.md).** No `outbound_message_queue`/`inbound_message_queue` tables to add (River's `river_job` already exists on main via #386). The I1 inbound dedupe index on the prod-sized `messages` table uses `CREATE INDEX CONCURRENTLY` with the `e2a:no-transaction` directive (the pattern #387's migration 052 established); preclear duplicates first and give `email_message_id`/`body_hash` non-null defaults. Every package writing new `messages` columns gets DB-backed tests.

Inbound slices (independent track, small):
- **I1. Inbound guarantee (pre-GA)** — 451 on persist failure + the content-hash dedupe index + conflict-reuse (§9). Engine-agnostic; ~a day; closes the silent-loss bug without building any queue.
- **I2. River `QueueInbound` + parse worker (post-GA)** — the DATA-handler shrink and async parse/scan per §9. Internal refactor, no contract surface, mechanical given #387's patterns.

## 11. Testing strategy

- **Idempotent re-drive:** same-key retry at each stage returns the same `msg_id`, zero extra submits; **held-send replay** (crash between hold-commit and `Complete`) does not double-hold (fake sender with accept/latency/error controls).
- **Rescue / crash matrix:** kill/panic at every row of §7. Assert exactly-one SMTP submit per `msg_id` except the documented residual window; assert a crashed job re-drives after `RescueStuckJobsAfter` and a completed/terminal message re-drives as a no-op. **Assert the rescue-window invariant directly: a send that runs up to the full SMTP envelope is NOT rescued** (the property that removes draft row 4) — this is the single most important guard to pin, since a mis-tuned `RescueStuckJobsAfter` reintroduces double-submit.
- **`JobSnooze` semantics:** provider-connection errors for hours → assert the job snoozes (does NOT count toward `MaxAttempts`, no false `email.failed` storm) and `QueuePause` stops claiming; ramp-limited → snoozes to rollover without burning an attempt.
- **Terminal-failure guard:** crash after SES-accept on the final attempt → assert no `failed` when header-tagged provider evidence exists, and header-matched `delivered` overrides a local `failed` (Merge exception).
- **Per-tenant fairness:** one tenant's 100-job batch does not starve others (the accept-time in-flight cap holds).
- **Contract tests:** `accepted` shape, `wait=sent` per-outcome table (sent/failed/held/timeout), replay-during-wait 409.
- **Load:** p99 accept latency < 50ms; drain rate vs the 60/min token budget.
- **Inbound (I1):** injected persist failure at DATA → 451 on the wire; same RFC `Message-ID` twice → exactly one `messages` row + one `email.received`; a *different* message reusing a `Message-ID` → a new row (not dropped); DB-backed tests for the dedupe index.
- **Inbound (I2):** kill/panic across the DATA tx and the parse worker; River rescue takeover; poison → discarded-with-alert, recipient-gone → cancelled.

## 12. Open questions

Resolved by the River reconciliation (were open in the pre-River draft, now answered by River primitives): lease fencing mechanism (River claim/`attempted_by` — no token needed); lease size + heartbeat (River `RescueStuckJobsAfter` above the envelope — no heartbeat); crash takeover (River rescue); SES-outage defer + pause (`JobSnooze` + `QueuePause`); outbox mandatory (unconditional on main after #387).

Still open:
1. **`RescueStuckJobsAfter` value for `QueueOutbound`** — must be above the worst-case single-attempt SMTP time (safety) yet low enough for prompt crash recovery (~5 min). Pin with headroom + a test in slice 3. (With one-attempt-per-`Work`, this is the *only* claim/rescue knob that survives the River move — no lease size, no heartbeat.)
2. **Per-tenant-fair accept-time in-flight cap** — the mechanism (per-agent outstanding-job cap vs priority sub-queues); decide in slice 3. Requirement fixed: no pure-FIFO global drain.
3. **`email.deferred`** — add as an event vs poll-only. Contract-surface, **decide before freeze** (both docs + peer practice lean add).
4. **`blocked` representation** — new `messages.status` value vs review-rejected family. Reserve the name now (contract §3.1); activates in slice 4.
5. **`wait=sent` transport** — `LISTEN/NOTIFY` on the `messages` row vs a short poll; either satisfies the ≤20s ceiling.
6. **Residual-window reconciler** (header-tagged SNS feedback vs a `sending` row): ~~alert-only v1, auto-heal later~~ **shipped as auto-heal (2026-07-16)**: header-tagged evidence is recorded on the row, the re-driven worker/terminal reconciler settles evidence-bearing `accepted`/`sending` rows as sent, and the §3.1 correction rule heals an already-written local `failed` when correlated delivery feedback arrives.
7. **Inbound (I2): raw-blob retention** — `river_job.args` holds full raw messages for pending inbound jobs; cap size / age-out policy for a backlog.
8. **`email.accepted` event — emit or not?** Currently **not** emitted: the caller learns `accepted` synchronously (the 200 body + `delivery_status='accepted'` on the row), and contract §4's *push* vocabulary is deliberately terminal-only (`sent`/`failed`/`deferred`). Optional addition: a one-line `PublishTx` of `email.accepted` in the accept-tx would populate the `webhook_events` log (visible in `GET /v1/events`) and deliver only to anyone who *explicitly* subscribes — harmless, but it widens the event vocabulary. Decide: accept-time event-log entry for observability vs. keep the push vocabulary terminal-only. (Leaning: skip at GA — the sync 200 already carries `accepted`; revisit if subscribers ask for an accept-time signal.)
