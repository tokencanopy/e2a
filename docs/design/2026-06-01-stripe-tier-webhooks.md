# Stripe-tier webhook system for e2a

**Status:** Partially superseded by the `/v1` redesign (2026-06-01 design).
**Date:** 2026-06-01
**Builds on:** PR #180 (`feat/webhooks-resource`), now merged with the four blocker fixes.

> **⚠️ Predates the `/v1` redesign.** The outbox *architecture* (§4.1–4.5) is the
> shipped mechanism, but the API-surface details here are stale: endpoints are
> `/v1/events`·`/v1/webhooks` (not `/api/v1/...`); pagination is `cursor`/`limit`/
> `next_cursor` (not `page_size`/`token`); errors use the JSON envelope
> `{error:{code,message,details,request_id}}` (not plain-text bodies); the bulk
> `redeliver-since` endpoint (§4.6) was **dropped** (only per-event
> `POST /v1/events/{id}/redeliver` ships); and `WEBHOOKS_OUTBOX_ENABLED` is now
> permanently on. The `email.rejected` event name shipped as written.
> The original ~72-hour retry proposal is also superseded: the GA contract is
> eight attempts spanning 29h21m. See `docs/api.md` and
> `docs/design/webhook-delivery-river-migration.md` for the frozen schedule.

---

## 1. Problem statement

e2a's current webhook system gives **at-least-once delivery from the moment an event is queued**. It does not guarantee that an event will *reach* the queue.

Today's flow: triggers commit their business state (an inbound email row, a HITL approval, an outbound send) and then spawn a fire-and-forget goroutine to fan the event out into the delivery queue:

```go
// internal/relay/server.go:355
go s.relay.publisher.Publish(context.Background(), event)

// internal/agent/api.go:224
go a.publisher.Publish(context.Background(), e)
```

A crash anywhere in the goroutine — between trigger commit and the first `InsertPending` call — silently drops the event. The trigger row in `messages` is durable; the customer never sees the webhook. There is no record anywhere of "we intended to publish this and didn't."

Mature webhook systems close this gap with a transactional outbox: the trigger's transaction writes both business state and an event record, and a separate poller drains the events into deliveries. Stripe's `/v1/events` is the customer-facing form of the same pattern — a durable event log queryable by ID, used both for delivery and for customer reconciliation/replay.

This design upgrades e2a to that pattern.

---

## 2. Goals and non-goals

### Goals

1. **At-least-once end-to-end delivery.** Every event committed by a trigger eventually reaches every matching subscriber, even across process crashes, deploys, or DB hiccups.
2. **72h retry envelope** with exponential backoff. Current schedule (5 attempts over ~4h) extends to ~10 attempts over ~72h.
3. **`GET /api/v1/events` and `GET /api/v1/events/{id}`** — durable, queryable record of every event we generated.
4. **Replay.** `POST /api/v1/events/{id}/redeliver` (per-webhook) and `POST /api/v1/webhooks/{id}/redeliver-since?ts=…` (bulk).
5. **No new infrastructure dependencies.** Postgres only.

### Non-goals (this design)

- Open/click event tracking.
- Outbound deliverability events (`bounced`, `complained`, `delivered`) — these land on the existing publisher path with no design changes beyond adding new event-type constants.
- Per-webhook rate limiting.
- Svix wire format compatibility (separate proposal; this design *scaffolds* the abstraction point without using it).
- Per-event schema versioning *semantics* — we add the field to the envelope now so future evolution doesn't need a flag day, but no migration logic.

---

## 3. Decisions made (open questions answered with defaults)

Each of the six decisions below shapes the rest of the design. They are stated up front so the user can override before implementation begins.

### D1. Event log retention: **30 days**

Match Stripe. Defensible customer expectation, predictable storage cost.

**Storage estimate.** Envelope sizes:
- `email.sent`, `email.approved`, `email.rejected`, `email.pending_approval`: ~1–3 KB (metadata + reference fields).
- `email.received`: median ~5 KB, p95 ~30 KB (includes `raw_message` bytes which Go's JSON encoder base64-encodes; base64 strings compress poorly through pglz, so TOAST gains are smaller than typical JSONB).
- Median across all event types, weighted by mix: ~5 KB.
- Index overhead: ~20% on top of heap with the indices in §4.3.

| Daily event volume | Heap (30d) | + indices | Total |
|---|---|---|---|
| 1K/day | ~150 MB | ~30 MB | ~180 MB |
| 10K/day | ~1.5 GB | ~300 MB | ~1.8 GB |
| 100K/day | ~15 GB | ~3 GB | **~25–30 GB** |
| 1M/day | ~150 GB | ~30 GB | **~180 GB** |

At 100K/day we are well within Postgres-on-a-single-node territory; at 1M/day partitioning becomes the natural next step (covered in §6.4). The schema is partition-ready by construction (composite PK includes `created_at` — see §4.3).

### D2. Replay semantics: **per-webhook, never re-routed**

`POST /api/v1/events/{id}/redeliver` requires a `webhook_id` in the body. Replay creates one new `webhook_subscriber_deliveries` row directed at the specified webhook. The publisher's filter logic is **not re-run** at replay time.

**Why.** Fan-out replay (re-running the matcher against the current subscriber set) seems convenient but creates two surprises customers will hate:

1. A webhook added *after* the event fired would receive a historical event the customer hasn't reasoned about.
2. A webhook whose filter set has narrowed since the event fired would *not* receive the replay, even though the customer explicitly asked for it.

Per-webhook replay is unambiguous: "I want event X delivered to webhook Y." It also matches Stripe's model, which sets developer expectation.

The bulk variant (`POST /api/v1/webhooks/{id}/redeliver-since?ts=…`) is also per-webhook by construction — it replays every event for that webhook's user *that originally matched it* between `ts` and now. The match decision is preserved (not re-computed) by snapshotting which webhooks each event matched at outbox-drain time (see §4.3 column `matched_webhook_ids`).

### D3. Outbox poll cadence: **`LISTEN/NOTIFY` + 1-second fallback poll**

Two-tier dispatcher:

1. The trigger commit also issues `NOTIFY webhook_events_new`. The outbox publisher subscribes via `LISTEN`. Average wake-to-fan-out latency: ~5 ms.
2. As a backstop, the publisher polls `webhook_events WHERE status='pending' AND next_poll_at <= now()` every 1 second. This catches notifications missed during reconnect/deploy windows, and absorbs the "process started before the LISTEN was active" race.

**Why not naive polling.** At 100ms poll intervals, the publisher wakes 10×/sec/replica even when nothing is happening. Modest cost, but unnecessary.

**Why not pure LISTEN/NOTIFY.** Postgres notifications are best-effort across reconnect; a single missed notification with no fallback poll leaves rows stuck pending. Hybrid gets us sub-50ms latency in the common case and bounded recovery time (≤1s) in the failure case.

**Why not Redis.** Covered in design discussion; not worth the operational complexity for our scale and access pattern.

### D4. Event log scope: **webhook-emitting events only, schema accommodates broader**

V1 stores only events that fan out to webhooks: `email.received`, `email.sent`, `email.pending_approval`, `email.approved`, `email.rejected`, plus the future deliverability events (`bounced`, `complained`, `delivered`, `delivery_delayed`).

The schema includes an `aud` (audience) column (default `'webhook'`) so a future v2 can write internal-only events (auth, billing, agent CRUD) without a schema migration. V1 returns only `aud='webhook'` from `GET /events`.

**Why scope down for v1.** Stripe's `/v1/events` includes everything because the Events API replaces direct polling of resource state. We don't promise that today — customers poll `/messages`, `/agents`, etc. directly. Expanding scope can be additive later.

### D5. Privacy: **envelope stored inline, body-by-reference deferred to v2**

V1 stores the full envelope (metadata + `data` including `raw_message` bytes for `email.received`) inline in `webhook_events.envelope`. Retention is the only deletion mechanism; the 30-day TTL is identical to the `messages` table TTL, so there is no new privacy boundary.

Body-by-reference (store metadata + a pointer to `messages.id`; resolve at read time) is a v2 improvement worth considering when:
- Customers ask for the ability to delete a specific message and have all associated webhook history disappear.
- Storage cost becomes meaningful.
- We add HIPAA / data-residency commitments that need per-record deletion.

For v1, the inline envelope keeps the implementation simple and the API self-contained.

### D6. Replay reuses the event ID

A replay creates a *new* `whd_…` delivery row but the JSON envelope it carries has the *same* `id` field as the original event (e.g. `evt_abc123`). A customer whose receiver deduplicates on event ID — which we recommend in the docs — will treat the replay as a no-op, *which is the intent*.

**Why this is the right behavior.** Replay is a recovery tool, not a "send me this again" mechanism for customers who actually need a second delivery. If a customer wants their handler to run twice, they should call the underlying API to re-trigger the operation. The replay endpoint exists to recover from *our* delivery failures (e.g. their endpoint was down for 4 days, our retry window only covered 72h, they want the missed events). In that case their receiver hasn't seen the event yet, dedup is a no-op, and the replay produces the side effect they wanted.

We document this explicitly in customer docs.

---

## 4. Proposed design

### 4.1 Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│                          TRIGGER TRANSACTION                          │
│                                                                       │
│   ┌────────────────┐      ┌─────────────────────┐                    │
│   │ business write │      │ outbox write        │                    │
│   │ (messages,     │ ─AND─│ (webhook_events,    │                    │
│   │  pending_msg,  │  in  │  status='pending')  │                    │
│   │  hitl rows)    │  one │                     │                    │
│   └────────────────┘  tx  └──────────┬──────────┘                    │
│                                       │                               │
│                                  NOTIFY webhook_events_new            │
└───────────────────────────────────────┼───────────────────────────────┘
                                        │
                                        ▼
                          ┌───────────────────────────┐
                          │  OutboxPublisherWorker    │
                          │  (LISTEN + 1s fallback)   │
                          │                           │
                          │  1. lease pending rows    │
                          │     (FOR UPDATE SKIP      │
                          │      LOCKED)              │
                          │  2. read enabled webhooks │
                          │  3. filter-match in Go    │
                          │  4. INSERT one delivery   │
                          │     row per match         │
                          │  5. UPDATE outbox row     │
                          │     status='processed'   │
                          │     matched_webhook_ids   │
                          └────────────┬──────────────┘
                                       │
                                       ▼
                          ┌───────────────────────────┐
                          │ webhook_subscriber_       │
                          │ deliveries                │
                          │ (unchanged from #180)     │
                          └────────────┬──────────────┘
                                       │
                                       ▼
                          ┌───────────────────────────┐
                          │ SubscriberRetryWorker     │
                          │ (unchanged from #180,     │
                          │ except retry schedule)    │
                          └────────────┬──────────────┘
                                       │
                                       │ HMAC-signed POST
                                       ▼
                          ┌───────────────────────────┐
                          │  customer endpoint        │
                          └───────────────────────────┘
```

**Package layout:**

| Component | New location | Notes |
|---|---|---|
| `webhook_events` table | New migration `migrations/026_webhook_events.sql` | |
| Outbox writer (used by triggers) | `internal/webhookpub/outbox.go` (new) | Implements `Publisher` interface |
| Outbox publisher worker | `internal/webhookpub/publisher_worker.go` (new) | LISTEN + poll loop |
| Existing `webhookpub.Publisher` | Becomes the outbox writer | Same interface; in-process fan-out is *removed* from this path |
| `webhook_subscriber_deliveries` table | Unchanged | |
| `SubscriberStore` | Unchanged | |
| `SubscriberRetryWorker` | Unchanged except retry schedule constants | |
| Events API handlers | New `internal/agent/events_api.go` | Mirrors `webhooks_api.go` patterns |
| `WebhookSigner` interface (scaffold) | `internal/webhook/signer.go` (new) | Default impl is current Stripe-style |
| SDK Events resource | `sdks/typescript/src/v1/events.ts`, `sdks/python/src/e2a/v1/events.py` | Generated types regenerated |
| CLI events commands | `cli/src/commands/events.ts` | |
| MCP server tools | `mcp/src/tools/events.ts` | |

**Compatibility with #180.** `webhook_subscriber_deliveries` and `SubscriberRetryWorker` are unchanged; the outbox feeds them via the same `InsertPending` interface. The current `webhookpub.publisher` struct loses its filter-matching and `InsertPending` calls — those move to the worker. The `publisher.Publish` method becomes a thin wrapper around an outbox INSERT.

### 4.2 Trigger sites: how `Publish` evolves

**Today** (post-#180):
```go
// internal/relay/server.go:345-355
inboundMsg, err := s.relay.store.CreateInboundMessage(ctx, /* ... */)  // tx 1
// ...
go s.relay.publisher.Publish(context.Background(), event)              // fire-and-forget
```

**After this design:**
```go
// One transaction: business write + outbox write commit together.
err := s.relay.store.WithTx(ctx, func(tx pgx.Tx) error {
    inboundMsg, err := s.relay.store.CreateInboundMessageInTx(ctx, tx, /* ... */)
    if err != nil {
        return err
    }
    return s.relay.publisher.PublishTx(ctx, tx, event)
})
// PublishTx INSERTs into webhook_events, issues NOTIFY webhook_events_new.
// Returns immediately. The worker handles fan-out asynchronously.
```

The trigger commit and the outbox commit are now one atomic write. Crash anywhere after `tx.Commit()` returns — even SIGKILL — leaves a durable outbox row that the worker picks up on restart.

**Pre-side-effect vs. post-side-effect triggers (critical distinction).** Not every trigger can safely roll back on outbox failure. The handler flow shape matters:

| Event | Trigger order | If outbox INSERT fails… | At-least-once guarantee |
|---|---|---|---|
| `email.received` | message row → outbox row (one tx) | Roll back tx, return 4xx to MTA, MTA retries | ✅ Strong |
| `email.pending_approval` | pending_msg row → outbox row (one tx) | Roll back tx, API call fails, customer retries | ✅ Strong |
| `email.sent` | SES `Send` succeeds → message row → outbox row | **Cannot roll back** (email already left our system) | ⚠️ Best-effort |
| `email.approved` | SES `Send` of approved draft succeeds → row → outbox | **Cannot roll back** | ⚠️ Best-effort |
| `email.rejected` | reviewer decision row written → outbox | Roll back tx, API call fails, customer retries | ✅ Strong |
| Future `email.bounced` (SNS) | SNS handler tx | Roll back tx, return non-2xx, SNS retries | ✅ Strong |

**Two distinct publisher entry points** to make the asymmetry explicit:

- `PublishTx(ctx, tx, e)` — pre-side-effect path. Returns error; caller rolls back its tx on failure. The strong at-least-once guarantee applies here.
- `PublishBestEffortTx(ctx, tx, e)` — post-side-effect path. Records the publish failure to a `webhook_publish_failures` log (for ops visibility + future reconciliation) but never returns an error to the caller. Customer-facing claim for these events is best-effort, not at-least-once.

**Long-term resolution for post-side-effect events:** drive `email.sent` from the SES SNS delivery confirmation (alongside `email.bounced`, `email.complained`, etc.) rather than from the synchronous `/send` handler. That collapses post-side-effect into pre-side-effect because the SNS handler owns its own transaction. Out of scope for v1; the deliverability-events follow-up makes it natural.

**Three trigger call sites need migration:**

1. [internal/relay/server.go:323-355](../../internal/relay/server.go#L323-L355) — `email.received`. Currently `CreateInboundMessage` opens its own implicit tx (pool.Exec). **Use `PublishTx`** (pre-side-effect). Failure-mode contract: tx COMMIT fails → return 4xx to MTA. MTA retries; deterministic event ID + `ON CONFLICT (id) DO NOTHING` makes the outbox write idempotent regardless of whether a prior attempt's `messages` row survived. See §5.1 for the full SMTP-retry truth table. **Scope warning:** today the relay has *no* explicit `pgx.Tx` plumbing (`grep "Begin\|WithTx" internal/relay/server.go` is empty). The slice-3 refactor needs to thread a `tx pgx.Tx` parameter through `CreateInboundMessage` and probably through `LookupConversationID` (which writes to `conversations` and should be inside the same tx). Conservative estimate: ~150 LOC of plumbing + tests, separate from the actual `PublishTx` call.
2. [internal/agent/api.go:220-225](../../internal/agent/api.go#L220-L225) — split by event:
   - Outbound `email.sent`: **Use `PublishBestEffortTx`** (post-side-effect). SES has already accepted the message before the outbox write attempts.
   - HITL `email.pending_approval`: **Use `PublishTx`** (pre-side-effect — the pending row hasn't shipped to SES yet).
   - HITL `email.approved`: **Use `PublishBestEffortTx`** (post-side-effect — the approved draft has already gone to SES).
   - HITL `email.rejected`: **Use `PublishTx`** (pre-side-effect — rejection is a no-op on SES; just a row write).
3. [internal/agent/webhooks_api.go:540](../../internal/agent/webhooks_api.go#L540) (`/test` endpoint) — bypasses the outbox entirely. Goes directly to `InsertPendingForTest` because there's no event to durably record (test events aren't customer-meaningful, and we want them invisible to `/events`). The publisher interface must doc-comment this explicitly: test deliveries do not flow through `Publish*`.

**Backwards compat with the existing flag.** [internal/webhookpub/publisher.go:53-79](../../internal/webhookpub/publisher.go#L53-L79) has a `FeatureFlag`. When `WEBHOOKS_OUTBOX_ENABLED=false`, `Publish(ctx, e)` keeps the **legacy in-process fan-out path** (current `Publish` behavior — read enabled webhooks, filter, `InsertPending`). When the flag is true, `Publish` becomes a thin wrapper that opens a tx and calls `PublishTx` itself; `PublishTx` writes the outbox row and `NOTIFY`s. **Each migrated trigger site owns the branch:**

```go
// At each migrated trigger site (slice 3-4):
if a.outboxFlag.Enabled() {
    return publisher.PublishTx(ctx, tx, event)     // new path
}
go a.publisher.Publish(ctx, event)                  // legacy path
return nil
```

This is the slice-3/slice-4 invariant: **legacy `Publish` keeps the in-process fan-out for the flag-off rollout window.** When we flip the flag to true permanently (post-slice-10), the legacy in-process branch is unreachable and can be deleted in slice 11. Without this, slice 3 + flag-off = silently dropped events.

### 4.3 Data model: `webhook_events`

```sql
-- migrations/026_webhook_events.sql

CREATE TABLE IF NOT EXISTS webhook_events (
    -- Stable per-event id (evt_<32hex>). Reused across all deliveries
    -- and replays of this event for consumer dedup. Determinism: id =
    -- "evt_" + first-32-hex of sha256(message_id || event_type || ...);
    -- the message_id is itself unique so collisions are negligible at
    -- 30-day retention × projected event volume.
    --
    -- PRIMARY KEY (id) — single-column. Global uniqueness is required
    -- for the outbox writer's `ON CONFLICT (id) DO NOTHING` idempotency
    -- on retried trigger transactions; if a retry happens days later
    -- (process restart, replay of a queued message), the same `evt_…`
    -- must conflict regardless of when the retry's `now()` lands.
    --
    -- Partitioning trade-off: native Postgres range partitioning by
    -- `created_at` requires the partition key in every unique constraint,
    -- which would force this PK to become `(created_at, id)`. At our
    -- projected scale (≤ 1M events/day × 30 days = 30M rows ≈ 150 GB)
    -- partitioning is not required; a partitioning migration when needed
    -- will be expensive but well-understood (see §6.4). We accept that
    -- cost in exchange for honest global `id` uniqueness today.
    id                   TEXT PRIMARY KEY,

    user_id              TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type                 TEXT NOT NULL,    -- e.g. "email.received"

    -- Audience: v1 only writes 'webhook'. Column reserved without a
    -- CHECK constraint so a future v2 expansion (adding e.g. 'system'
    -- or 'internal') is non-breaking and does not require dropping a
    -- CHECK on a populated table. Enforcement is at the app layer.
    aud                  TEXT NOT NULL DEFAULT 'webhook',

    -- Envelope is the full {event, id, created_at, schema_version, data}
    -- JSON ready for delivery. Persisted at trigger time so the payload
    -- is the snapshot at the moment of the event.
    envelope             JSONB NOT NULL,

    -- schema_version is also held as a column for cheap filtering /
    -- migration audit. SMALLINT is plenty — we will never see 32k
    -- envelope versions in this product's lifetime, and the saving is
    -- meaningful at 30M rows. ALTER COLUMN TYPE on a populated table is
    -- expensive (CLAUDE.md migration rules), so right-size now.
    schema_version       SMALLINT NOT NULL DEFAULT 1,

    -- Optional indexed dimensions for filtering and the
    -- /events?agent_id=... query parameter. Sourced from envelope.data
    -- at write time. Nullable because not every event type carries
    -- these (e.g., domain.verified has no agent_id). No FK on agent_id /
    -- conversation_id — those can be deleted by the user and we want
    -- the historical event log to survive.
    agent_id             TEXT,
    conversation_id      TEXT,
    message_id           TEXT REFERENCES messages(id) ON DELETE SET NULL,

    -- Outbox state machine for the publisher worker.
    --   pending   – created by trigger; awaits fan-out
    --   processed – worker fanned out and wrote matched_webhook_ids
    --   no_match  – worker ran filter logic; no subscriber matched.
    --               Distinct from processed=0-matches so we can audit
    --               quickly which events would have triggered nothing.
    status               TEXT NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending', 'processed', 'no_match')),

    -- Worker bookkeeping. last_error length-capped to prevent a
    -- pathological 10KB stack trace × 30M rows blowing up disk.
    attempts             INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error           TEXT NOT NULL DEFAULT ''
                         CHECK (length(last_error) <= 4096),
    last_status_code     INTEGER,
    next_poll_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at         TIMESTAMPTZ,

    -- Snapshot of which webhooks the publisher matched at fan-out time.
    -- Used by the /webhooks/{id}/redeliver-since endpoint so bulk
    -- replay preserves the original match decision (instead of
    -- re-running the filter against the current subscriber set, which
    -- would surprise customers — see Decision D2). TEXT[] of webhook
    -- ids; bounded by the per-user webhook cap (50).
    matched_webhook_ids  TEXT[] NOT NULL DEFAULT '{}'
                         CHECK (cardinality(matched_webhook_ids) <= 50),

    -- created_at is the Postgres server's transaction start time.
    -- All e2a application replicas connect to the same primary, so
    -- there is no cross-replica clock skew on this column — `now()`
    -- is monotonic per (Postgres session, transaction). If we ever
    -- move to multiple writer instances (logical replication,
    -- multi-region primaries) we will need to revisit cursor
    -- pagination semantics; see §6.5.
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at           TIMESTAMPTZ NOT NULL DEFAULT now() + interval '30 days'
);

-- Hot path: outbox worker lease (FOR UPDATE SKIP LOCKED + next_poll_at
-- bump). Partial index restricts to pending rows; processed / no_match
-- never appear in the worker's poll. NOTE: every lease bumps
-- next_poll_at, so this index sees one write per delivery — autovacuum
-- cost is non-trivial at scale. Document scaling implications in §6.1.
CREATE INDEX IF NOT EXISTS idx_webhook_events_pending
    ON webhook_events (next_poll_at)
    WHERE status = 'pending';

-- Hot path: GET /events with cursor pagination by (created_at, id).
-- The id column is left out of the index — Postgres satisfies the
-- ORDER BY (created_at DESC, id DESC) tiebreak from the heap when
-- ranging on this index. Two-column index trades 1 index page for a
-- predictable post-fetch sort on the tiebreak (~ns at LIMIT 100).
CREATE INDEX IF NOT EXISTS idx_webhook_events_user_created
    ON webhook_events (user_id, created_at DESC);

-- Filter indexes for /events?type=... and /events?agent_id=...
CREATE INDEX IF NOT EXISTS idx_webhook_events_user_type_created
    ON webhook_events (user_id, type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_events_user_agent_created
    ON webhook_events (user_id, agent_id, created_at DESC)
    WHERE agent_id IS NOT NULL;

-- Hot path: janitor's DELETE WHERE expires_at <= now(). Without this,
-- the hourly cleanup full-scans a 30M-row table. Mirrors the
-- idx_messages_expires precedent in migration 001.
CREATE INDEX IF NOT EXISTS idx_webhook_events_expires
    ON webhook_events (expires_at);

-- DELIBERATELY OMITTED: a GIN index on matched_webhook_ids. The
-- redeliver-since query is bounded by a 7-day window (§4.6) AND a
-- user_id predicate, so the bitmap scan over idx_webhook_events_user_created
-- with an in-memory post-filter on matched_webhook_ids beats the GIN
-- bitmap-intersect cost. If telemetry later shows the post-filter is a
-- bottleneck, add the GIN index then with the partial predicate
-- WHERE status = 'processed' AND cardinality(matched_webhook_ids) > 0.
```

**Janitor ordering invariant.** When cleanup runs, `webhook_events` must be janitored *after* `webhook_subscriber_deliveries` checks references for the day — never the reverse. Because the delivery row's `event_id` is `ON DELETE SET NULL` (see §4.4), the dropped event invalidates the reference but the delivery row keeps its self-contained `event_payload`, so retries continue. If the order is reversed (delete deliveries first, then events), there's no race; both directions are safe in this schema, but document the conservative order in the cleanup loop comment so a future operator doesn't reason about it from scratch.

**Field choices, justified:**

- **`id` as `evt_<32hex>` reused across replays.** Replay creates new delivery rows (`whd_…`) but the envelope carries the original `evt_…`. Consumer dedup discards the replay if they've already processed it (Decision D6).
- **`envelope JSONB`** stores the full payload at trigger time. Body-by-reference deferred per Decision D5.
- **`aud` column** reserves space for non-webhook events without migration (Decision D4).
- **`schema_version`** is a defensive forward-compat hatch. V1 always emits 1; v2 might emit 2 for envelope shape changes.
- **`matched_webhook_ids TEXT[]`** snapshots the publisher's match decision. Cheap (typically 0-3 entries), enables the bulk-replay-since query without re-running filter logic at replay time.
- **`status='no_match'`** distinguishes "publisher ran, no subscriber wanted it" from "publisher hasn't run yet." Important for ops debugging ("why didn't my webhook fire?").
- **`expires_at`** mirrors `messages` 30-day TTL. Both tables drain on the same janitor; we extend the cleanup loop in [cmd/e2a/main.go:355-378](../../cmd/e2a/main.go#L355-L378).

**FK behavior choices:**

- `user_id ON DELETE CASCADE` — events vanish when the user account is deleted.
- `message_id ON DELETE SET NULL` — message TTL prunes before event TTL is not possible (same 30-day window), but defensive nullability survives operator-initiated message deletion. The event remains intact (envelope is self-contained); only the FK reference goes away.

**Idempotent INSERTs.** The outbox writer uses `INSERT INTO webhook_events ... ON CONFLICT (id) DO NOTHING` so retried trigger transactions (e.g. an idempotency-key reuse on the API caller's side) don't create duplicate events. The event ID is generated *before* the transaction begins; if the same id appears twice, the second insert is a no-op.

### 4.4 Outbox publisher worker

```go
// internal/webhookpub/publisher_worker.go

type OutboxWorker struct {
    pool          *pgxpool.Pool
    identityStore store
    inserter      deliveryInserter // existing dbInserter from publisher.go
    notifyChan    chan struct{}    // signaled by LISTEN goroutine

    pollInterval  time.Duration    // 1 * time.Second
    batchSize     int              // 32
    leaseDuration time.Duration    // 5 * time.Minute (mirrors SubscriberRetry)
}

func (w *OutboxWorker) Start(ctx context.Context) {
    // Two goroutines:
    //   1. LISTEN loop: SELECT pg_listen('webhook_events_new'),
    //      pushes a token onto notifyChan on each NOTIFY.
    //   2. Tick loop: wakes on notifyChan OR pollInterval timer,
    //      calls processBatch.

    go w.listenLoop(ctx)
    w.tickLoop(ctx)
}

func (w *OutboxWorker) processBatch(ctx context.Context) {
    // Lease up to batchSize pending events with FOR UPDATE SKIP LOCKED.
    // Identical pattern to SubscriberStore.GetPending — see #180 blocker
    // fix in subscriber_store.go:48-89.

    tx, _ := w.pool.Begin(ctx)
    defer tx.Rollback(ctx)

    rows, _ := tx.Query(ctx,
        `WITH candidates AS (
            SELECT id FROM webhook_events
            WHERE status = 'pending' AND next_poll_at <= now()
            ORDER BY created_at ASC
            LIMIT $1
            FOR UPDATE SKIP LOCKED
         )
         UPDATE webhook_events e
         SET next_poll_at = now() + ($2 * interval '1 second')
         FROM candidates c
         WHERE e.id = c.id
         RETURNING e.id, e.user_id, e.type, e.envelope, e.agent_id,
                   e.conversation_id, e.message_id, e.attempts`,
        w.batchSize, int(w.leaseDuration.Seconds()),
    )

    var events []leasedEvent
    for rows.Next() { events = append(events, scan(rows)) }
    rows.Close()
    tx.Commit(ctx)

    // Fan out each event in its own goroutine, bounded by w.concurrency.
    for _, ev := range events {
        go w.fanOutOne(ctx, ev)
    }
}

func (w *OutboxWorker) fanOutOne(ctx context.Context, ev leasedEvent) {
    // 1. List enabled webhooks for the user + event type.
    webhooks, err := w.identityStore.ListEnabledWebhooksForRouting(ctx, ev.userID, ev.type)
    if err != nil {
        w.recordFailure(ctx, ev.id, err.Error())
        return
    }

    // 2. Apply filter matching in Go (mirrors current publisher.go:138-186).
    var matched []string
    for _, wh := range webhooks {
        if matchesFilters(ev, wh) {
            matched = append(matched, wh.ID)
        }
    }

    // 3. Insert delivery rows in one transaction. Each row inserts with
    //    ON CONFLICT (event_id, webhook_id) DO NOTHING so a lease-expiry
    //    race where worker A and B both fan-out the same event produces
    //    duplicate inserts that are silently swallowed, NOT a transaction
    //    abort. Without per-row ON CONFLICT, any one row's constraint
    //    violation would roll back the entire batch of matched inserts —
    //    making forward progress impossible under contention.
    //
    //    Both inserts and the outbox-row status flip commit together;
    //    a crash mid-tx leaves status='pending' for the next attempt.
    err = w.pool.BeginFunc(ctx, func(tx pgx.Tx) error {
        if len(matched) > 0 {
            // Single multi-row INSERT covers the whole match set.
            // Postgres ON CONFLICT applies per-row, so a duplicate from
            // a lease-expiry race no-ops only the affected row; siblings
            // commit normally.
            //
            // The WHERE predicate matches the partial unique index in
            // migration 026 verbatim — Postgres requires exact predicate
            // matching for inference to bind to a partial index.
            err := w.inserter.InsertPendingBatchTx(ctx, tx,
                ev.id, matched, ev.type, ev.messageID, ev.envelope)
            if err != nil {
                return err
            }
        }
        // Mark outbox row processed atomically with the inserts.
        _, err := tx.Exec(ctx,
            `UPDATE webhook_events
             SET status = $1, processed_at = now(), matched_webhook_ids = $3
             WHERE id = $2`,
            ternary(len(matched) > 0, "processed", "no_match"),
            ev.id, matched,
        )
        return err
    })
    if err != nil {
        w.recordFailure(ctx, ev.id, err.Error())
    }
}
```

**Concurrency model: multi-replica safe by construction.**

- The outbox lease (`FOR UPDATE SKIP LOCKED` + `next_poll_at` bump by `leaseDuration`) lets every replica run an `OutboxWorker` independently. Two replicas competing for the same row → only one wins the SELECT-FOR-UPDATE, the other skips. The lease is the *primary* defense against double fan-out.
- **Lease-vs-fanout race protection.** If a worker's `fanOutOne` blocks (e.g. pgxpool exhausted) past the 5-min lease window, another worker picks up the same event row. Two safeguards prevent state corruption:
  1. Each row insert uses per-row `INSERT … ON CONFLICT (event_id, webhook_id) WHERE event_id IS NOT NULL AND replay_id IS NULL DO NOTHING` — duplicates from the racing fan-out are silently swallowed (per-row, not tx-aborting).
  2. The final `UPDATE webhook_events SET status='processed' …` is **conditional**: `WHERE id = $2 AND status = 'pending'`. The first finisher writes the `matched_webhook_ids` snapshot and flips status to `processed`; the second finisher's UPDATE matches zero rows and no-ops, leaving the first finisher's snapshot intact. Without this guard a slow-and-stale worker B could overwrite worker A's match set with B's (possibly outdated) view of subscribers.
- **The partial unique index is the backstop**, not the primary defense. The per-row `ON CONFLICT DO NOTHING` is the correctness mechanism; the index is what makes the conflict detectable.
- The destination table (`webhook_subscriber_deliveries`) also has its own per-attempt lease that prevents two retry workers from POSTing the same delivery row twice.

**`recordFailure` semantics.** When `fanOutOne`'s tx fails (e.g. DB connection lost mid-batch, constraint violation we didn't anticipate), the function calls `recordFailure(ctx, ev.id, err.Error())`:

```sql
UPDATE webhook_events
SET attempts = attempts + 1,
    last_error = $2,
    next_poll_at = now() + LEAST($3, interval '60 seconds')
WHERE id = $1
```

Outbox-level retry is *aggressive* (cap at 60s between attempts) because a stuck pending event is blocking customer-visible webhook delivery. If `attempts > 100` (sanity), the worker escalates: pages ops, leaves the row in `pending` for human triage. There is **no `failed` terminal state on the outbox** — at-least-once requires that we retry indefinitely. If the failure mode is truly stuck (e.g. an unparseable envelope), an operator clears the row manually after debugging.

**LISTEN connection management.** The `listenLoop` goroutine owns a **dedicated, non-pool connection** acquired via `pool.Acquire(ctx)` and held for its lifetime. pgx's pool connections cannot be used for LISTEN because subscription state is per-connection and gets lost when the connection returns to the pool. The goroutine:
1. Acquires its connection, issues `LISTEN webhook_events_new`, enters `WaitForNotification` loop.
2. On connection drop (Postgres restart, network partition): reconnects with backoff (1s, 2s, 5s, 10s, 30s cap), re-issues LISTEN.
3. While reconnecting, the 1s fallback poll in `tickLoop` keeps up with arrivals — no events are lost.

`notifyChan` is buffered at size 1 with drop-on-full semantics: a burst of N notifications fires the worker once per tick. Worker processes the batch (up to `batchSize=32`) per tick; if there's more, the next tick drains the next batch. Wasted notifications are fine — the SELECT in `processBatch` doesn't care how it got woken up.

**Idempotency key on delivery rows (added in migration 026):**

```sql
ALTER TABLE webhook_subscriber_deliveries
    ADD COLUMN IF NOT EXISTS event_id TEXT;
ALTER TABLE webhook_subscriber_deliveries
    ADD COLUMN IF NOT EXISTS replay_id TEXT;

-- Partial unique index enforces "one first-delivery row per (event,
-- webhook) pair." Replays bypass this constraint by setting replay_id
-- (the WHERE clause excludes rows where replay_id IS NOT NULL).
--
-- MIGRATION SAFETY:
--   `CREATE INDEX` (non-CONCURRENTLY) takes an ACCESS EXCLUSIVE lock
--   on the table for the duration of the build. The migration runner
--   wraps each .sql file in a transaction, and `CREATE INDEX
--   CONCURRENTLY` cannot run inside a transaction — so to use it we
--   must split this DDL into its own migration file (026b) that the
--   runner executes outside the txn boundary.
--
--   For the *initial* deploy on a table with no event_id values yet
--   (column is added immediately above with no default → all NULL),
--   the partial predicate matches zero rows and the build is fast
--   (single heap scan to confirm zero-matching). Even the brief lock
--   under that scenario is acceptable in practice.
--
--   But: a re-run on a production-sized table that has been
--   collecting delivery rows since rollout could hold the lock for
--   minutes. The migration runner must be configured to detect this
--   case (e.g., abort if `webhook_subscriber_deliveries` row count >
--   1M and a CONCURRENTLY path is not available). Defensive option:
--   move this CREATE UNIQUE INDEX to its own migration file 026b
--   that uses CONCURRENTLY from the start. Cost: one extra file. Pay
--   it now.
CREATE UNIQUE INDEX IF NOT EXISTS idx_wsd_event_webhook_uniq
    ON webhook_subscriber_deliveries (event_id, webhook_id)
    WHERE event_id IS NOT NULL AND replay_id IS NULL;

-- Supporting index for the redeliver-since handler, which needs to
-- find existing deliveries for a (webhook_id, event_id) pair to skip
-- replays of events that already have a pending delivery. The partial
-- unique index above only covers the WHERE-replay_id-IS-NULL case, so
-- this broader index is needed for replay history.
CREATE INDEX IF NOT EXISTS idx_wsd_event_id
    ON webhook_subscriber_deliveries (event_id)
    WHERE event_id IS NOT NULL;
```

`event_id` references `webhook_events.id` (logical link only; no FK because the parent has a composite PK that would require us to also store `created_at`. Behavior on event expiry: handled at the application layer — the replay handler returns 410 when the event row is gone).

`replay_id` is null for first deliveries and `whd_…` (the originating delivery's id) on replays. The partial unique index enforces "one delivery per (event, webhook) on the original fan-out path." A replay bypasses the constraint because `replay_id IS NOT NULL`.

**Multi-replica race walk-through:** Workers A and B both fan out event X to webhook Y. Worker A's tx inserts the (X, Y) row first. Worker B's tx attempts to insert (X, Y); the per-row `ON CONFLICT (event_id, webhook_id) WHERE event_id IS NOT NULL AND replay_id IS NULL DO NOTHING` matches the partial index predicate verbatim, treats the conflict as a no-op, and B's transaction commits with the outbox row marked `processed`. No churn, no rollback, exactly one delivery row.

**Backpressure.** If the worker can't keep up with the trigger rate, `next_poll_at` is never bumped past `now()` for pending rows; the worker just keeps draining FIFO. Lag becomes observable as the gap between `webhook_events.created_at` and `processed_at`. The system degrades gracefully: customers see delivery latency rise, no events are dropped.

**Wiring into `cmd/e2a/main.go`:**

```go
// Insert near the existing SubscriberRetryWorker wiring at line 312.
outboxWorker := webhookpub.NewOutboxWorker(pool, store, dbInserter)
workerWG.Add(1)
go func() {
    defer workerWG.Done()
    outboxWorker.Start(workerCtx)
}()
```

### 4.5 Event lifecycle, end-to-end

#### Inbound mail (`email.received`)

```
SMTP DATA finalize  (internal/relay/server.go:323)
        │
        ▼
BEGIN tx
   INSERT INTO messages ...                          ◄── business write
   INSERT INTO webhook_events                        ◄── outbox write
       (id, user_id, type, envelope, ...)
       ON CONFLICT (id) DO NOTHING
   pg_notify('webhook_events_new', event_id::text)
COMMIT
        │
        ▼ (SMTP 250 OK returns to sender)

(asynchronously)
OutboxWorker wakes (LISTEN signal or 1s poll)
   ├─ Lease pending rows (FOR UPDATE SKIP LOCKED + bump next_poll_at)
   ├─ For each event:
   │     ├─ ListEnabledWebhooksForRouting(user_id, event.type)
   │     ├─ Filter-match each webhook
   │     ├─ BEGIN tx
   │     │    INSERT one webhook_subscriber_deliveries row per match
   │     │    UPDATE webhook_events SET status='processed',
   │     │       matched_webhook_ids = ARRAY[...]
   │     │  COMMIT
        │
        ▼

SubscriberRetryWorker (unchanged): drains webhook_subscriber_deliveries
   ├─ HMAC-sign envelope
   ├─ POST to customer endpoint
   └─ MarkDelivered / RecordAttemptFailure
```

#### Outbound (`email.sent`)

Same shape. The trigger is the `/send` handler in [internal/agent/api.go:220-225](../../internal/agent/api.go#L220-L225). Today's `publishAsync` becomes `PublishTx` invoked inside the existing transaction that records the outbound message.

#### HITL (`email.pending_approval` / `email.approved` / `email.rejected`)

Identical pattern. Triggers live in [internal/agent/webhooks_api.go](../../internal/agent/webhooks_api.go) (for the HITL approval/rejection handlers in slice 3 of #180). Wrap each trigger in a transaction that includes the outbox write.

#### Future deliverability (`email.bounced`, `email.complained`, `email.delivered`, `email.delivery_delayed`)

These come from SES via SNS. The SNS handler becomes the trigger: it reads the SNS message, identifies the e2a message/agent, opens a transaction, updates the message's deliverability state, and writes the outbox row. No changes to the outbox / worker / delivery infrastructure beyond the new event type constants and payload builders.

#### Outbox row state machine

```
                INSERT
                  │
                  ▼
               pending ────────────────────────────────┐
                  │                                    │
       outbox worker leases + fans out                 │
                  │                                    │
                  ├──── matched ≥ 1 ──▶ processed      │
                  │                                    │
                  ├──── matched = 0 ──▶ no_match       │
                  │                                    │
                  └──── fan-out failed (DB error etc.) ┘
                       (next_poll_at already bumped by lease;
                        attempts++; row retried)
```

Terminal states are `processed` and `no_match`. There is no `failed` — the outbox retries indefinitely until success, because failure here means we cannot guarantee at-least-once. After ~10 attempts with logged errors, we page operators; we do not drop.

### 4.6 API surface

#### `GET /api/v1/events`

```
GET /api/v1/events?type=email.received&agent_id=ag_foo&page_size=50&token=...
Authorization: Bearer e2a_...

200 OK
{
  "events": [
    {
      "id": "evt_abc123",
      "type": "email.received",
      "schema_version": 1,
      "created_at": "2026-06-01T12:34:56.789Z",
      "agent_id": "ag_foo",
      "conversation_id": "conv_xyz",
      "message_id": "msg_def456",
      "status": "processed",
      "data": { ... full envelope.data ... },
      "delivery_status": {
        "matched_webhooks": 2,
        "delivered": 2,
        "pending": 0,
        "failed": 0
      }
    },
    ...
  ],
  "next_token": "opaque_cursor"
}
```

**Query params** (matches the conventions of `/api/v1/agents/{email}/messages` in `web/public/openapi.yaml:1799-1809`, as it existed at the time of this proposal):
- `type` — exact event type filter. Optional.
- `agent_id`, `conversation_id`, `message_id` — exact filters. Optional.
- `since`, `until` — RFC3339 timestamps. Optional. (Same naming as the existing messages endpoint.)
- `page_size` — 1–100, default 50.
- `token` — opaque cursor returned by previous response. (Same parameter name as the existing messages endpoint.)

**Cursor format:** opaque JSON blob containing `(created_at_ns, id, filter_snapshot)`. Same shape as the existing `messages` cursor at [internal/agent/api.go:2620-2643](../../internal/agent/api.go#L2620-L2643). Continuation requests with mismatched filter params return 400 — prevents silent paging into wrong result sets when a customer changes params between pages.

**`status` field** on each event is the outbox state machine value (`pending`, `processed`, `no_match`). Surfacing it directly distinguishes "matched but pending" (deliveries are in flight) from "no_match" (no subscriber wanted this event) — without forcing the customer to derive it from `delivery_status.matched_webhooks`.

**`delivery_status` block** is computed at read time by joining against `webhook_subscriber_deliveries`. Because deliveries live 90 days and events 30 days, a delivery referencing an event always outlives its parent — but a request to `/events?since=…` only returns events within the 30-day retention, so the join is always populated. The `delivery_status.purged: true` flag is **unreachable** in v1 (no scenario where deliveries are gone but the event still exists); remove the field from the response shape.

**Error response shape.** Plain text body, matching the existing convention used by `http.Error(w, msg, code)` throughout `internal/agent/` — see e.g. `internal/agent/api.go:2700-2710` and the schema declarations at `web/public/openapi.yaml:1497-1498` (as it existed at the time of this proposal). 4xx responses return a string body with the error message; OpenAPI declares `schema: type: string`. (Introducing a structured `Error` schema is a cross-cutting API refactor; if we want it, that's a separate proposal.)

#### `GET /api/v1/events/{id}`

```
GET /api/v1/events/evt_abc123
Authorization: Bearer e2a_...

200 OK
{
  "id": "evt_abc123",
  "type": "email.received",
  "schema_version": 1,
  "created_at": "2026-06-01T12:34:56.789Z",
  "agent_id": "ag_foo",
  "data": { ... },
  "deliveries": [
    {
      "delivery_id": "whd_111",
      "webhook_id": "wh_aaa",
      "status": "delivered",
      "attempts": 1,
      "last_status_code": 200,
      "last_attempt_at": "2026-06-01T12:35:01.234Z"
    },
    ...
  ]
}
```

Includes inline deliveries (full join). Bounded by the per-event match count (≤ 50 webhooks/user × match rate).

#### `POST /api/v1/events/{id}/redeliver`

```
POST /api/v1/events/evt_abc123/redeliver
Authorization: Bearer e2a_...
Content-Type: application/json

{
  "webhook_id": "wh_aaa"   // optional; see below
}

200 OK
{
  "delivery_id": "whd_replay_222",
  "event_id": "evt_abc123",
  "webhook_id": "wh_aaa",
  "status": "pending"
}
```

**Semantics:**
- Creates a *new* `webhook_subscriber_deliveries` row with `event_id = "evt_abc123"`, `replay_id = "whd_replay_222"`. Bypasses the `idx_wsd_event_webhook_uniq` constraint because `replay_id IS NOT NULL`.
- Envelope is fetched from `webhook_events.envelope` — exact bytes the original delivery would have used.
- Signing secret: **current** secret on the webhook (Decision: replay uses current crypto state, never historical). The 24h prev-secret grace window does not apply to replay.
- Webhook must be `enabled`. 409 if disabled.
- Webhook must exist and be owned by the caller. 404 if not.
- The original event must exist and be owned by the caller. 404 if not. 410 Gone if `expires_at < now()` (event has been janitored).

**Empty body — replay to all originally-matched webhooks.** If the request body omits `webhook_id`, we replay to every webhook in `webhook_events.matched_webhook_ids` (skipping any that are currently deleted or disabled, with a per-webhook 409 noted in the response). Response shape becomes:

```
{
  "event_id": "evt_abc123",
  "deliveries": [
    {"webhook_id": "wh_aaa", "delivery_id": "whd_replay_222", "status": "pending"},
    {"webhook_id": "wh_bbb", "delivery_id": null, "status": "skipped", "reason": "disabled"}
  ]
}
```

Common ask: "my entire subscriber pool missed this event during an outage." Without this, customers have to make N round-trips. With it, one call.

**Idempotency on the API call:** Same body posted twice within 5 minutes returns the same `delivery_id` and is a no-op for the second call. Implemented via the existing `idempotencyStore` ([cmd/e2a/main.go:387-394](../../cmd/e2a/main.go#L387-L394)) keyed on `(user_id, event_id, webhook_id_or_empty, "replay")`.

#### `POST /api/v1/webhooks/{id}/redeliver-since`

```
POST /api/v1/webhooks/wh_aaa/redeliver-since
Authorization: Bearer e2a_...
Content-Type: application/json

{
  "since": "2026-05-29T00:00:00Z"
}

200 OK
{
  "webhook_id": "wh_aaa",
  "since": "2026-05-29T00:00:00Z",
  "scheduled": 47,
  "skipped_already_pending": 2
}
```

**Semantics:**
- Identifies every `webhook_events` row where `user_id = $caller AND created_at >= since AND 'wh_aaa' = ANY(matched_webhook_ids)`. Planner uses `idx_webhook_events_user_created` for the range scan and post-filters the `ANY` against `matched_webhook_ids` in memory. At our scale this is acceptable (per the 7-day cap below); we explicitly omitted a GIN index because the bitmap-intersect cost outweighs the benefit at the projected match cardinality. See §4.3 DDL comments and the planner walk-through in §6.2.
- For each, creates a replay delivery row (same as per-event endpoint).
- Skips events that already have a pending or in-flight delivery for this webhook (idempotency).
- `since` must be ≤ 7 days ago. Cap exists to prevent accidental full-history replays; can be raised later. 400 if older.
- Rate limit: 1 call per webhook per minute. 429 with `Retry-After`.

**Rate-limit implementation caveat.** The existing rate limiter at [internal/ratelimit/ratelimit.go](../../internal/ratelimit/ratelimit.go) is **in-memory per process** — under N replicas the effective limit is N/min/webhook. Acceptable for v1 because the cost of an extra replay call is bounded (5 minutes of `idx_wsd_event_id` work even under abuse). Document this in customer docs. If we later need a sharp cross-replica cap, promote to a Postgres-backed counter on `webhooks.last_replay_at` with a CAS-on-timestamp check.

#### Error responses

Match existing API conventions: JSON body `{"error": "..."}`, status codes:
- 400 — invalid input (bad cursor, bad timestamp, invalid filter combo)
- 401 — missing/bad bearer
- 404 — event/webhook not found or not owned by caller
- 409 — webhook disabled (replay endpoints), state conflict
- 410 — event past retention
- 429 — rate limited (replay endpoints only)

#### OpenAPI spec changes

Add three paths to `web/public/openapi.yaml` (as it existed at the time of this proposal):
- `/api/v1/events`
- `/api/v1/events/{id}`
- `/api/v1/events/{id}/redeliver`
- `/api/v1/webhooks/{id}/redeliver-since`

Plus a new schema `WebhookEvent` mirroring the JSON shape above. Regenerate SDK types via `make generate-sdk`.

### 4.7 Retry extension

**New backoff schedule** (replaces `internal/webhook/retry.go:11-17`, as it existed at the time of this proposal):

```go
var retryBackoffs = []time.Duration{
    1 * time.Minute,
    5 * time.Minute,
    15 * time.Minute,
    1 * time.Hour,
    4 * time.Hour,
    8 * time.Hour,
    16 * time.Hour,
    24 * time.Hour,
}
```

Total envelope: ~72h spread over 8 attempts. Max attempts on new delivery rows: 8 (was 5).

**TTL change.** `expires_at` on `webhook_subscriber_deliveries` extends from 30 days to **90 days** to accommodate the longer retry envelope plus a generous tail for customer-side debugging via `/deliveries`. The event log's 30-day retention is unaffected.

**Migration of in-flight rows.** Existing rows with `max_attempts = 5` keep their cap (the schedule lookup gracefully returns "exhausted" past index 4 — see `retry.go:19-24`, as it existed at the time of this proposal). New rows get `max_attempts = 8`. No flag-day; the worker handles both transparently.

```sql
-- migrations/027_retry_envelope_extension.sql
ALTER TABLE webhook_subscriber_deliveries
    ALTER COLUMN max_attempts SET DEFAULT 8,
    ALTER COLUMN expires_at  SET DEFAULT now() + interval '90 days';
```

Both `ALTER COLUMN ... SET DEFAULT` are metadata-only (no row rewrite) on Postgres 11+, so safe on prod-sized tables. Per [CLAUDE.md](../../CLAUDE.md) migration safety rules.

### 4.8 Signing & `WebhookSigner` interface (scaffold only)

Current signing is hardcoded into [subscriber_deliverer.go:115-130](../../internal/webhook/subscriber_deliverer.go#L115-L130). To prepare for a future Svix-format swap without prejudicing the v1 design, extract:

```go
// internal/webhook/signer.go (new)

// SignContext carries every input a signer might need. Designed to be
// additive — future signers (Svix needs message_id in the signed
// string, AWS SNS uses X.509 chain) can read new fields without
// changing the interface.
type SignContext struct {
    Timestamp  int64
    Body       []byte
    EventID    string    // evt_<hex>, used by Svix in the signed string
    DeliveryID string    // whd_<hex>, available if a signer wants it
    Secret     string    // current per-webhook signing secret
    SecretPrev string    // previous secret during 24h rotation grace (may be "")
}

// WebhookSigner produces outbound HTTP headers for a delivery attempt.
// Implementations are stateless; SignContext carries everything.
type WebhookSigner interface {
    // Sign returns headers to add to the outbound POST. Implementations
    // typically set one or more signature-family headers; they MUST NOT
    // mutate any other request state. Body is provided read-only.
    Sign(ctx SignContext) http.Header

    // Name identifies the scheme for logging/telemetry ("stripe-style",
    // "svix", "ecdsa", etc.). Used in operational dashboards.
    Name() string
}

// StripeStyleSigner is the current implementation. Single header,
//   X-E2A-Signature: t=<ts>,v1=<hex>[,v1=<prev_hex>]
// EventID and DeliveryID from SignContext are unused.
type StripeStyleSigner struct{}

func (StripeStyleSigner) Sign(ctx SignContext) http.Header {
    h := http.Header{}
    h.Set("X-E2A-Signature", buildSignatureHeader(ctx.Timestamp, ctx.Body, ctx.Secret, ctx.SecretPrev))
    return h
}

func (StripeStyleSigner) Name() string { return "stripe-style" }
```

`SubscriberDeliverer` takes a `WebhookSigner` at construction. The existing [Deliver](../../internal/webhook/subscriber_deliverer.go#L75) signature stays as-is — the deliverer just calls `d.signer.Sign(SignContext{...})` instead of inlining `buildSignatureHeader`. No ripple to retry/store/auto-disable code.

```go
type SubscriberDeliverer struct {
    client       *http.Client
    requireHTTPS bool
    signer       WebhookSigner  // new
}
```

Default wiring in `main.go` passes `StripeStyleSigner{}`. A future Svix swap adds `SvixSigner` (which reads `ctx.EventID` to build the `{msg-id}.{timestamp}.{body}` signed string) and either changes the wiring (cutover) or composes via a `MultiSigner` that emits both header families during a dual-emit grace window.

The interface deliberately doesn't take `*http.Request` (would couple signers to net/http internals) or return error (signers operate on opaque bytes; failure modes are programming errors). Algorithm-agnostic by construction — an asymmetric signer (e.g., ECDSA like SendGrid) just stuffs the SignContext.Secret/SecretPrev with PEM-encoded key material.

### 4.9 SDKs and CLI

#### TypeScript SDK

New resource at `sdks/typescript/src/v1/events.ts`:

```typescript
export class EventsResource {
  list(params?: { type?: string; agent_id?: string; created_after?: string;
                  page_size?: number; page_token?: string }):
       Promise<{ events: WebhookEvent[]; next_token?: string }>;
  get(id: string): Promise<WebhookEvent>;
  redeliver(id: string, webhook_id: string): Promise<DeliveryRef>;
}
```

Exposed on `E2AClient` as `client.events`. Types regenerated from OpenAPI.

#### Python SDK

Mirrors TS shape:
```python
class EventsResource:
    def list(self, *, type=None, agent_id=None, created_after=None,
             page_size=None, page_token=None) -> EventListResponse: ...
    def get(self, event_id: str) -> WebhookEvent: ...
    def redeliver(self, event_id: str, webhook_id: str) -> DeliveryRef: ...
```

Available as `client.events`.

#### MCP server

Three new tools in [mcp/src/tools/](../../mcp/src/tools/):

| Tool | Input | Output |
|---|---|---|
| `list_events` | `{type?, agent_id?, created_after?, page_size?, page_token?}` | `{events, next_token?}` |
| `get_event` | `{event_id}` | full event |
| `redeliver_event` | `{event_id, webhook_id}` | delivery ref |

Bumps the tool count from 18 to 21. Update sites that hard-code the count:
- `mcp/examples/README.md:14-15` ("18 e2a tools")
- `mcp/examples/codex/README.md:5`
- `mcp/examples/crewai/README.md`, `mcp/examples/langchain/README.md`, `mcp/examples/adk/README.md` — the "Available tools" tables (3 + new category "Events")
- `skills/using-e2a/SKILL.md:260` ("18 MCP tools: agents (5), messages (5), HITL (4), domains (4)…") — extend with "Events (3)"
- Upstream docs PRs at [langchain-ai/docs#4150](https://github.com/langchain-ai/docs/pull/4150) (open) and [google/adk-docs#1793](https://github.com/google/adk-docs/pull/1793) (merged): the merged ADK doc will need a follow-up edit; the open LangChain doc PR can be updated before merge.

No production code asserts the count — [mcp/tests/http.test.ts:357](../../mcp/tests/http.test.ts#L357) uses `toBeGreaterThan(0)` — so the count surface is purely documentation.

#### CLI

```
e2a events list [--type <t>] [--agent <a>] [--since <ts>] [--limit N]
e2a events get <event-id>
e2a events redeliver <event-id> --webhook <wh-id>
```

Lives in `cli/src/commands/events.ts`.

#### Webhook signature verifiers

**No changes.** Wire format unchanged; existing `verifySignature` helpers in the SDKs continue to work for both first-delivery and replay payloads (the envelope `id` is the same so customer dedup is unaffected — Decision D6).

---

## 5. Edge cases and failure handling

### 5.1 Trigger transaction commits but the process crashes before API returns success

**Example:** SMTP receive completes, the transaction commits (`messages` + `webhook_events` rows are durable), but the process dies before the SMTP server sends back the 250 OK. The MTA retries; we receive the same message again.

**Defense:** the deterministic event ID + `ON CONFLICT (id) DO NOTHING` on the outbox write makes the entire trigger idempotent across SMTP retries, regardless of what state survived from a prior attempt.

- If the prior attempt's tx COMMITTED both rows → MTA retry writes a duplicate `messages` row (existing pre-design behavior, separate concern), but the deterministic `evt_<hex>` collides and the outbox INSERT no-ops. No duplicate event.
- If the prior attempt's tx aborted *before* COMMIT → both rows are absent. MTA retry writes both fresh in one tx. Event is delivered. Recovery from a previous-attempt event-loss is automatic.
- If the prior attempt CRASHED after COMMIT but before the SMTP 250 OK → both rows are durable. MTA retry writes a duplicate `messages` row but no duplicate event. Worker delivers exactly once.

**Note on existing inbound dedup:** today the relay does *not* dedupe by `Message-ID` upstream of `CreateInboundMessage` (no `GetInboundByEmailMessageID` call before insert). MTA retries create duplicate message rows. That's a pre-existing concern orthogonal to webhooks — the new design's deterministic event ID closes the *event* dedup gap regardless.

**Same-tx invariant.** This whole flow only works because `messages` write + outbox INSERT commit together in one `BEGIN ... COMMIT`. If they're in two separate txs (current pre-design code), the failure mode "first committed, second crashed" really does happen. Slice 3's migration (§7.7) is what closes this: the relay opens one tx, calls `CreateInboundMessageInTx`, then `PublishTx`, then commits both. Crash anywhere before COMMIT → MTA retry on the next attempt rewrites both rows from scratch via deterministic ID idempotency. Crash after COMMIT → both rows are durable, worker drains.

**Event ID derivation per event type:**

| Event type | Input formula |
|---|---|
| `email.received` | `sha256(message_id + "\|" + event_type)`, first 32 hex chars |
| `email.sent` | `sha256(message_id + "\|" + event_type)` |
| `email.pending_approval` | `sha256(pending_msg_id + "\|" + event_type)` |
| `email.approved` | `sha256(pending_msg_id + "\|" + event_type)` — same `pending_msg_id` as the matching pending_approval; event_type distinguishes |
| `email.rejected` | `sha256(pending_msg_id + "\|" + event_type)` |
| Future `email.bounced` / `email.complained` / `email.delivered` | `sha256(message_id + "\|" + event_type + "\|" + ses_event_id)` — SES SNS events carry their own ID; including it dedupes if SES re-delivers the SNS notification |
| Future `domain.verified` | `sha256(domain_id + "\|" + event_type + "\|" + verification_timestamp_unix)` — timestamp distinguishes re-verifications after a delete/reclaim cycle |

The `|` literal delimiter prevents accidental collisions where concatenated fields could be ambiguous (e.g. `("abc", "def")` vs. `("abcdef", "")`).

**Collision probability.** SHA-256 truncated to 128 bits gives birthday-collision probability `n² / 2^129` for `n` distinct events. At 1M events/day × 30-day retention × 5 event types ≈ 1.5 × 10^8 events, collision probability ≈ 3 × 10^-23. Negligible.

### 5.2 Webhook deleted between event commit and fan-out

The webhook FK is on `webhook_subscriber_deliveries`, not on `webhook_events`. When the worker runs `ListEnabledWebhooksForRouting`, the deleted webhook is absent; no delivery row is created for it.

The event row remains intact with `matched_webhook_ids` reflecting whichever webhooks *were* present at fan-out time. The event is queryable via `/events/{id}` indefinitely (within 30-day retention).

### 5.3 All webhooks for a user disabled between event commit and fan-out

Worker runs filter logic, finds 0 matches, sets `status='no_match'`. No delivery rows created.

**Subsequent re-enable:** does *not* backfill. The customer must use `/webhooks/{id}/redeliver-since?ts=…` if they want historical events delivered. This is explicit and predictable; auto-backfill on re-enable would surprise customers who disabled a webhook intentionally to silence a class of events.

### 5.4 Concurrent calls to `/events/{id}/redeliver` for the same (event, webhook) pair

Idempotency key on the API call: `(user_id, event_id, webhook_id, "replay")` stored in the existing `idempotencyStore`. Window is 5 minutes (matches other API idempotency).

Within the window: same response returned, no second delivery row created.
Outside the window: a second replay creates a second delivery row. This is acceptable — outside 5 minutes, the customer's intent is "I want this event redelivered again, on purpose."

**Runaway replay protection.** A buggy customer script could call `/events/{id}/redeliver` once per 5-minute window for 30 days = ~8.6K replay rows for a single event. We rely on the API rate limit (TBD — track in Q4 of §8) as the primary guard. Schema-level defense for v2: add `webhook_subscriber_deliveries.replay_count INTEGER DEFAULT 0` incremented per replay, with the replay handler returning 429 when count crosses a sanity cap (~100). Not in v1 because we have no production data showing this is a real concern; revisit if we see it in telemetry.

### 5.5 Retention boundary: event row about to be cleaned up while a delivery is still pending

The event log TTL is 30 days; the delivery TTL is 90 days. So a delivery row outliving its source event is possible.

**Why this is safe:** the delivery row's `event_payload JSONB` already carries a full copy of the envelope (per #180 design — payload is self-contained for retry independence). When the source event is janitored, deliveries keep working.

**Tradeoff:** if the customer queries `/events/{id}` for an expired event ID, they get 410 Gone — even though deliveries for that event might still be retrying. This is consistent with Stripe: the Events API is a 30-day window, longer-tail records live elsewhere.

**Reference behavior:** `webhook_subscriber_deliveries.event_id` is a *logical link only* — no FK. We considered a composite FK on `(event_created_at, event_id) → webhook_events (created_at, id)`, which would give referential integrity + cascade-safe cleanup, but rejected it because:
1. It would add an `event_created_at` column to every delivery row (+8 B/row, ~25 GB at 1M events × N matches).
2. The application-layer 410 Gone path already handles missing events cleanly.
3. Composite FKs serialize on the parent's PK lock during deletes; the janitor would slow.

If FK semantics turn out to matter in v2 (e.g., for a compliance auditor), revisit then. **Until then:** the delivery row's `event_id` may reference a janitored event; the replay handler returns 410, the retry path keeps using the inlined `event_payload`.

### 5.6 Event type deprecated mid-life

A future event type `email.opened` ships, then we deprecate it.

- Old `webhook_events` rows keep their original `type='email.opened'`. Queryable forever (within retention).
- Old subscriber rows on `webhook_subscriber_deliveries.event_type` keep firing as long as the catalog accepts them.
- New webhooks created with `events: ["email.opened"]` continue to be valid but match no future triggers (because the trigger stops emitting).
- Catalog validation in [webhookpub/event.go](../../internal/webhookpub/event.go) gates `events` array contents at webhook create/update — we'd remove the deprecated value from the catalog gradually.

Customer-visible behavior: a deprecated event type silently stops firing. Reasonable for the lifecycle pattern.

### 5.7 Replay of an event whose subscriber rotated signing secrets since

**Use current secret.** Period.

Rationale: the prev-secret grace window (24h after rotation) is for *first-delivery retries*, not for replays. A replay is a fresh delivery from the customer's perspective — they expect to verify it with whatever secret they have configured *now*. The historical secret may have been intentionally rotated for security reasons (suspected leak); reusing it on replay would defeat the rotation.

Customer-side dedup-on-event-ID still works because the envelope `id` is unchanged.

### 5.8 Outbox worker crashes mid-`fanOutOne`

Lease expires after 5 minutes. Another worker (same replica or different) picks the row up.

**Was there partial fan-out?** No — `fanOutOne` runs the inserts and the status update in one transaction. Either all delivery rows for the match set are written and the outbox row is marked `processed`, or none of it is and the row stays `pending`.

**On second attempt:** the worker re-runs the entire match. `ListEnabledWebhooksForRouting` returns the *current* set. If a webhook was added or deleted between attempts, the match set differs:

- **Webhook deleted since first attempt:** no row to insert, no problem.
- **Webhook added since first attempt:** a fresh `(event_id, webhook_id)` row is inserted normally — the partial unique index doesn't conflict because no prior row exists for that pair.
- **Webhook present in both attempts:** worker B's `INSERT … ON CONFLICT (event_id, webhook_id) WHERE event_id IS NOT NULL AND replay_id IS NULL DO NOTHING` matches the partial index predicate verbatim, no-ops the duplicate, transaction commits. **Per-row `ON CONFLICT DO NOTHING` is mandatory here** — a tx-aborting `ON CONFLICT` would roll back the entire batch of inserts in `fanOutOne`, leaving the lease to expire and the system to churn indefinitely.

### 5.9 NOTIFY lost during deploy / Postgres restart

Fallback 1-second poll picks up any pending events. Worst-case latency for a single event during a deploy: ~1 second. Acceptable.

### 5.10 Trigger writes outbox row but holds the tx open for a long time before COMMIT

NOTIFY only fires on COMMIT, so a long-running transaction with the outbox INSERT in it doesn't wake the worker prematurely. After commit, NOTIFY fires; worker picks up immediately.

If the transaction never commits (e.g. application bug, deadlock): no event in the table, nothing fans out. The trigger never declared "the business state is durable," so this is identical to the trigger never having run.

### 5.11 Outbox table grows faster than the janitor drains

At sustained 1M events/day with a 30-day TTL, the table holds ~30M rows. Hourly cleanup at 1000 rows/iteration won't keep up. Two responses:
- Tune the janitor to delete in larger batches (10K-50K).
- Partition the table by `created_at` weekly (see §6.4 — future evolution).

The design accommodates partitioning without an interface change because all queries are scoped by `user_id` + `created_at`, both of which work cleanly with native Postgres partitioning.

### 5.12 HITL state transitions with no webhook events (v1 limitation)

The HITL feature in #180 fires three events covering the most common transitions: `email.pending_approval` (created), `email.approved` (reviewer approved + SES.Send succeeded), `email.rejected` (reviewer rejected). Two state transitions intentionally **do not** fire webhook events in v1:

| Transition | What happens to the pending row | Why no event |
|---|---|---|
| Reviewer approves but SES.Send fails | Stays `pending`; reviewer sees the SES error in the API response | The reviewer is already aware (synchronous error); customer can retry approval. Adding `email.send_failed` is additive and non-breaking — defer until a real customer asks. |
| Pending message hits `approval_expires_at` without a decision | Stays `pending`; expiry is observable via the field but no automatic transition | Auto-expiry is a separate (future) feature. Today nothing changes the row state when the timestamp passes, so an event would be misleading. Adding `email.approval_expired` requires first adding an expiry worker. |

**How customers observe these states today:**
- *Send-failed-after-approve*: poll `GET /api/v1/agents/{email}/pending` and look for rows in `pending` status whose `last_send_error` is set. Alternatively, the reviewer's UI surfaces the error directly when they hit approve.
- *Pending expired*: poll `/pending` and filter on `approval_expires_at < now() AND status = 'pending'`. Or after the Stripe-tier event log ships: `GET /events?type=email.pending_approval&since=…` and reconcile against `/pending` to find rows that never transitioned.

**Forward path.** Adding either event later is purely additive:
1. New constant in [internal/webhookpub/event.go](../../internal/webhookpub/event.go) (e.g. `EventEmailSendFailed`).
2. New payload builder (similar to `buildRejectedEvent`).
3. New `PublishTx` call site: for `send_failed`, in the SES error branch of [hitl_api.go:362-388](../../internal/agent/hitl_api.go#L362-L388) (pre-side-effect — strong guarantee); for `approval_expired`, in the future expiry worker.
4. Webhook catalog validation accepts the new event type.

No schema change, no breaking change to existing subscribers. Customers who want the new event opt in by adding it to their `events` array.

The v1 decision to ship without these events is a scope choice, not an architectural limitation — fold in when a customer asks or when the HITL feature graduates from preview to GA.

---

## 6. Scalability and extensibility notes

### 6.1 Scaling the outbox worker

Single-replica is fine to ~100 events/sec aggregate (each event is one row scan + filter match + N inserts; with 32-row batches and sub-100ms processing per batch, ~300 events/sec is the ceiling).

Above that, run multiple `OutboxWorker` instances. The `FOR UPDATE SKIP LOCKED` lease pattern means no further coordination is needed. Linear scale to ~5 replicas (~1500 events/sec aggregate), at which point the publisher's `ListEnabledWebhooksForRouting` query becomes the bottleneck. Mitigation when that arrives: cache the (user_id, event_type) → webhooks map with short TTL.

**Hot-path UPDATE churn.** Each lease bumps `next_poll_at`; that column is in `idx_webhook_events_pending` so HOT (heap-only-tuple) updates are disabled. Each lease writes a new heap tuple AND an index entry. Beyond the lease, the `status` flip from `pending → processed` removes the row from the partial index — another index entry mutation. Per event: ~1 lease + ~1 status flip = 2 mutations on `idx_webhook_events_pending` + 1 dead heap tuple. At 100K events/day = ~300K mutations + ~100K dead tuples/day. Autovacuum at default settings handles this. At 1M events/day = ~3M mutations + ~1M dead tuples/day; tune `autovacuum_vacuum_scale_factor` down to 0.05 for this table specifically, and watch `pg_stat_progress_vacuum` during peak.

**Denormalization escape hatch (v3 if needed).** If at the 10M+ rows / 100GB+ scale the dead-tuple churn on the immutable `envelope` column becomes prohibitive, split into two tables: `webhook_events` (immutable: id, envelope, created_at, expires_at) and `webhook_events_state` (mutable: id, status, attempts, next_poll_at, processed_at, matched_webhook_ids). Updates only churn the small state table. Cost: every read joins; every API response needs both. Not worth it before the scale wall actually hits.

### 6.2 Scaling the events query

`GET /events` with cursor pagination and the `(user_id, created_at DESC)` index handles arbitrary depth efficiently. The `(user_id, type, created_at DESC)` and `(user_id, agent_id, created_at DESC)` indexes cover the common filter cases. The cursor tiebreak on `id` is satisfied from the heap (negligible cost at LIMIT ≤ 100).

Risk: a customer storing 1M+ events/month who runs an unfiltered `/events` page-walk. Mitigation: enforce `page_size <= 100` (already), require `created_after` or `created_before` on responses past page 10 (future feature — not v1).

**Redeliver-since query budget.** At 100K events/day × 7-day cap × user-fraction-of-traffic, the query reads roughly 100K-700K candidate rows and post-filters `ANY(matched_webhook_ids)` in memory (~5M string comparisons at array cardinality 5). Survivable under the 1/min rate limit (§4.6). **Concrete SLO before GA:** p95 < 500ms at 100K events/day/user. Bench in staging with synthetic load; if we miss, add the GIN index back with the partial predicate noted in §4.3 DDL.

### 6.3 Cursor pagination and clock semantics

`created_at` is set by Postgres `DEFAULT now()` at trigger time. `now()` is the transaction start time on the Postgres primary. All e2a application replicas connect to the same primary, so there is no application-side clock skew — every event's `created_at` is determined by one clock (the DB's), regardless of which app replica wrote it.

Cursor pagination uses `(created_at, id)` as the tiebreak tuple, ordering by `created_at DESC, id DESC`. Within a single transaction multiple rows share `created_at` (transaction start time); the `id` tiebreak resolves ordering deterministically. This is correct.

**When this stops being true:** if we ever go to a multi-primary or active-active deployment, different primaries' `now()` clocks can disagree (typically by sub-second under NTP). At that point cursor pagination could silently skip rows from a clock-lagging primary. Mitigations when we get there:
- Per-user monotonic sequence column (extra index, but defensively correct).
- Server-assigned `seq BIGSERIAL` instead of `created_at` for cursor.

For v1's single-primary model this is a non-issue.

### 6.4 Extensibility: new event types

Adding `email.bounced` (or any other event) requires:
1. New constant in [webhookpub/event.go](../../internal/webhookpub/event.go).
2. New payload builder.
3. New trigger site that calls `publisher.PublishTx(...)` inside its existing transaction.
4. SDK regeneration via `make generate-sdk`.

No schema change. No worker change. No API change beyond the constant being accepted in `events` arrays at webhook create time.

### 6.5 Future evolution accommodated

| Want to add… | Where it plugs in | LOC |
|---|---|---|
| Bounce/complaint/delivered (deliverability) events | New SNS handler trigger sites; otherwise zero change | ~150 |
| Open/click events | Outbound tracking (pixel injection, link rewriting) — separate infrastructure; emits events via the same outbox pattern when hits arrive | ~500+ |
| Svix wire format | Implement `WebhookSigner` interface (§4.8); register in main.go; dual-emit headers during transition | ~80 |
| Per-event schema versioning | Already in the envelope (`schema_version: 1`). Bump to 2 + add envelope-shape adapter in delivery path when needed | ~30 |
| Per-webhook rate limiting | Token bucket in Postgres (`webhooks.rate_limit_*` columns); promote to Redis if/when contention requires | ~100 |
| Outbox table partitioning | Weekly range partitions on `created_at`. Requires migrating PK from `(id)` to `(created_at, id)`, rewriting outbox-writer `ON CONFLICT` from `(id)` to `(created_at, id)`, and updating delivery `event_id` references. Non-trivial; do when row count > 30M or heap > 100 GB. Pre-warm the migration with `pg_partman` in a staging copy before committing. | ~300 |
| Webhook-as-a-service (multi-tenant) — i.e. e2a becomes Svix | Move outbox + delivery infrastructure to a separate service, add tenant_id everywhere | major rewrite |

---

## 7. Verification strategy

### 7.1 Unit tests

- **Outbox writer** (`internal/webhookpub/outbox_test.go`): event ID determinism, envelope serialization, `ON CONFLICT DO NOTHING` behavior, NOTIFY payload format.
- **Outbox worker** (`internal/webhookpub/publisher_worker_test.go`): lease lifecycle, single-event fan-out, multi-event batch, filter matching against current `webhooks` snapshot, status transitions (pending → processed, pending → no_match), failure recovery.
- **Replay handlers** (`internal/agent/events_api_test.go`): idempotency on per-event replay, webhook ownership checks, disabled-webhook 409, expired-event 410, `replay_id` propagation.
- **Retry schedule** (`internal/webhook/retry_test.go`): new 8-step schedule produces expected `next_retry_at` values; legacy 5-attempt rows still terminate correctly.

### 7.2 Integration tests (against real Postgres)

- **End-to-end inbound:** SMTP message arrives → DB shows `messages` + `webhook_events` rows in same tx → outbox worker fires → delivery row appears → mock customer endpoint receives signed POST.
- **End-to-end outbound:** `/send` → same shape, verifying `email.sent` uses `PublishBestEffortTx` (commit-on-outbox-failure).
- **HITL approval flow:** `/pending/{id}/approve` → `email.approved` event in outbox → delivery to subscriber.
- **Replay:** post `/events/{id}/redeliver` → new `whd_replay_…` row → mock customer receives same envelope (verify event ID identical).
- **Replay to all (empty body):** post `/events/{id}/redeliver` with empty body → response includes one delivery per webhook in `matched_webhook_ids`, with disabled webhooks reported as `skipped`.
- **Bulk replay since:** create 10 events for a webhook; janitor delivery rows for half; post `/redeliver-since` with timestamp covering all 10; verify 10 replay deliveries scheduled (5 fresh + 5 explicitly listed).
- **SMTP retry → outbox repair:** simulate inbound flow where the first tx commits the message row but the outbox INSERT errors before COMMIT. Verify MTA retry of the same `Message-ID` causes the dedup path to write the outbox row.

### 7.3 SDK contract tests (against `cmd/e2a-contract-server`)

Each new resource needs a contract test exercising the real handlers. Add to existing suites per the convention in CLAUDE.md:

- **TypeScript** at `sdks/typescript/src/v1/__tests__/contract.events.test.ts`:
  - `events.list()` returns 200 + events array + next_token shape
  - `events.list({type, since, until, token, page_size})` filters work
  - `events.get(id)` returns 200 + envelope + delivery_status
  - `events.get(unknown_id)` returns 404
  - `events.get(expired_id)` returns 410
  - `events.redeliver(id, {webhook_id})` returns 200 + delivery_id
  - `events.redeliver(id, {})` returns 200 + per-webhook deliveries array
- **Python** at `sdks/python/tests/test_contract_events.py`: identical shape.
- **MCP server** at `mcp/tests/tools.test.ts`: the existing `expect(tools.length).toBeGreaterThan(0)` assertion doesn't pin a count, but the harness's "tool present" assertions need new cases for `list_events`, `get_event`, `redeliver_event`.

### 7.3 At-least-once guarantee tests (the proof-of-design)

These tests specifically demonstrate the publish-loss window is closed:

- **Crash between trigger commit and outbox poll:** Test harness opens a transaction, INSERTs message + outbox event, COMMITs, and simulates process death by aborting before `pg_notify` fires (or by killing the LISTEN goroutine). Worker restart picks up the pending row within 1 second via the fallback poll. Test asserts: delivery row eventually appears for the matched subscriber.
- **Crash mid-fan-out:** Worker is patched to panic after inserting half the matched delivery rows. Test asserts: outbox row stays `pending` (transaction rolled back), `idx_wsd_event_webhook_uniq` prevents duplicates on the second attempt, all matched subscribers eventually have exactly one delivery row.
- **Lease expiry under contention:** Two simulated worker replicas race on the same outbox row. Test asserts: exactly one wins the lease, the other skips, no duplicate fan-out.
- **NOTIFY storm:** Burst 1000 events in one second. Worker drains FIFO; no events lost; `created_at` → `processed_at` lag bounded by `batch_size / processing_rate`.

### 7.4 Migration tests

- Migrations 026 and 027 apply idempotently on a fresh DB.
- 026 is non-destructive on a populated `webhooks` + `webhook_subscriber_deliveries` (it only ADDs).
- 027's `ALTER COLUMN ... SET DEFAULT` are metadata-only (verify with `pg_stat_progress_*` empty during `make migrate`).
- Pre-existing in-flight delivery rows on the old retry schedule terminate correctly when their (smaller) `max_attempts` is exhausted.

### 7.5 Manual / staging verification

- Deploy to staging with one customer subscriber. Trigger 100 inbound events. Verify `/events` shows all 100 in order. Verify each has `delivery_status.delivered = 1`.
- Disable the customer subscriber. Trigger 10 events. Verify `/events` shows all 10 with `status='no_match'` rather than `pending`. Re-enable; verify subsequent events deliver.
- Force-kill the e2a process while a burst of 50 events is in flight. Verify on restart: all 50 outbox rows are eventually `processed`, all deliveries land.

### 7.6 Telemetry to add

Before this design is considered production-ready, instrument:

- **Publisher lag** (gauge): time since oldest pending `webhook_events` row was created. Alert if > 30s.
- **Outbox throughput** (counter): events processed per minute.
- **Fan-out match rate** (histogram): matched_webhook_ids count distribution.
- **Replay rate** (counter): replays per hour, by event type.
- **Janitor throughput** (counter): rows deleted per hour, per table.
- **NOTIFY/poll ratio** (counter): how often the fallback poll picks up work the LISTEN missed. Should be ~0 in steady state.

### 7.7 Rollout plan

**Sequencing** (each row is a separate PR):

| Order | Slice | Depends on | Backward compat |
|---|---|---|---|
| 1 | `webhook_events` table migration, outbox writer, `PublishTx` interface | — | Old `Publish` becomes a thin wrapper that opens its own tx for callers not yet migrated |
| 2 | Outbox publisher worker, wiring in main.go | 1 | Worker is a no-op until trigger sites are migrated |
| 3 | Migrate `internal/relay/server.go` to use `PublishTx` in the message-insert tx. **Non-trivial:** relay has no existing `pgx.Tx` plumbing today; needs ~150 LOC of tx threading through `CreateInboundMessage` + `LookupConversationID` before the actual `PublishTx` call. Plan as a 2-3 day slice, not a 1-day. | 2 | Behind `WEBHOOKS_OUTBOX_ENABLED` env flag; default off in v1 → on in v2 |
| 4 | Migrate `internal/agent/api.go` and other trigger sites | 3 | Same flag |
| 5 | Retry schedule extension (migration 027 + retry.go) | — (independent) | Old rows continue with `max_attempts=5`; new rows get 8 |
| 6 | Events API handlers (`GET /events`, `GET /events/{id}`) | 1 (table exists) | Read-only; safe to ship even before triggers are migrated (returns empty initially) |
| 7 | Replay endpoints (`POST /events/{id}/redeliver`, `POST /webhooks/{id}/redeliver-since`) | 6 + delivery `event_id` / `replay_id` columns | New columns added in migration 026 |
| 8 | SDK + CLI + MCP surface | 6, 7 | New methods are additive |
| 9 | OpenAPI spec + customer docs | 6, 7 | |
| 10 | Telemetry + dashboards | 2, 7 | |
| 11 | Positioning: update marketing claim from "at-least-once from queue" to "at-least-once" | 10 (proven via telemetry) | |

**Feature flag.** `WEBHOOKS_OUTBOX_ENABLED` (env var, default `false` v1, `true` from v2). When off, trigger sites keep using `go publisher.Publish(...)` and the in-process fan-out. When on, they use `PublishTx`. Both paths can coexist during rollout because the outbox worker only acts on `webhook_events` rows, and there's nothing in that table until triggers start writing to it.

**Migration of in-flight rows.** None needed — `webhook_subscriber_deliveries` rows from the legacy publisher continue to be drained by the same `SubscriberRetryWorker`. The new `event_id` and `replay_id` columns are nullable, so existing rows have nulls and the partial unique index excludes them.

**Customer docs changes** (handled in slice 11):
- New "Events" page covering the event log concept.
- Update "Webhook delivery" page with the new at-least-once language.
- Add the replay endpoints to the API reference.
- Update the retry schedule table.
- Add a "Reconciliation" guide showing how to use `/events?created_after=…` to recover from receiver outages.

---

## 8. Open questions

The six headline decisions in §3 are made with defaults; everything below is genuinely still open and worth flagging.

### Q1. Storage-cost trigger for body-by-reference

At what storage threshold do we revisit the v2 body-by-reference path (Decision D5)? Suggest: when `webhook_events` median row size × 30-day retention × user count crosses ~50 GB. Anything before that is premature optimization.

### Q2. Cap on `redeliver-since` window

V1 caps at 7 days. Should this scale with retention (30 days)? Counterpoint: 7 days is a typical "recovery from outage" window; longer windows are usually intentional backfills that deserve a different ergonomic (a script that paginates `/events?webhook_id=… AND created_after=…` and calls `/events/{id}/redeliver` per event).

### Q3. Should `GET /events` expose internal-only events (`aud != 'webhook'`)?

Schema accommodates it. V1 doesn't expose. Q for product: do we want to ship a Stripe-style "every domain event is queryable" feature as a v2 product expansion? Has implications on what triggers need to write outbox rows (a lot more).

### Q4. Replay rate-limit interaction with the existing `/test` endpoint

Both schedule deliveries that count against the customer's webhook rate limit (when we have one). Today no rate limit exists; when we add one, replay endpoints should be exempt or have a separate quota. Defer to the rate-limit design.

### Q5. Telemetry coverage before customer-facing GA

Should we hold the "at-least-once" marketing claim until we have at least 30 days of production telemetry showing publisher lag stays under our SLO? Suggest yes — we don't want to advertise a guarantee that has an unknown failure rate.

### Q6. Operator override on `expires_at`

Some customers may want >30 day retention. Plumb a `webhook_events_retention_days` field on `users` (or `account_limits`) for v2? Defer.

---

## 9. Risks and mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Trigger latency increase from outbox INSERT | High (will measurably happen) | Low (sub-ms) | Benchmark `CreateInboundMessage` before/after; the INSERT is ~one extra index touch in the same tx. Document SLO impact. |
| Outbox table grows unbounded if janitor lags | Medium | Medium (disk fills, plans degrade) | Tune janitor batch size to keep up at projected steady state. Alert on delete-rate-below-arrival-rate. |
| Multi-replica fan-out duplicates if `idx_wsd_event_webhook_uniq` is missed (e.g. someone backfills delivery rows without setting `event_id`) | Low | Medium (customer-visible duplicates) | Worker's `InsertPendingTx` always writes `event_id`; the constraint is `WHERE event_id IS NOT NULL` so manual operator-inserted rows don't trigger false-conflicts. |
| LISTEN/NOTIFY queue overflows under burst | Low | Low (fallback poll catches up) | Postgres NOTIFY queue is 8 GB; messages are small. Even at 100K events/sec, headroom is hours. |
| Replay endpoint becomes a DoS vector | Medium | Medium | `/redeliver-since` rate-limited to 1/min per webhook. `/redeliver` idempotent within 5min. Customer can't loop these. |
| `webhook_events.envelope` JSONB stores PII past message TTL | Medium | High (compliance) | Default retention matches message TTL (both 30 days). Body-by-reference path in v2 if/when compliance asks. |
| Adding `event_id` + `replay_id` columns to `webhook_subscriber_deliveries` requires ALTER TABLE on a populated prod table | Medium | Low | Both are nullable `TEXT` with no default → metadata-only on Postgres 11+. Verify on a staging copy first. |
| Operator runs `WEBHOOKS_OUTBOX_ENABLED=true` with the new code on one replica but old code on another | Low | Medium (mixed-mode behavior) | Slice 3-4 are the migration; deploy is atomic per service. Document the no-mix invariant in the runbook. |

---

## 10. Out of scope (recap for reviewers)

- Per-event schema versioning *semantics* — the field is reserved but not interpreted.
- Open/click event tracking infrastructure.
- Outbound deliverability events (`email.bounced` etc.) — these land on the existing publisher path without design changes; they just gain at-least-once for free once this design ships.
- Per-webhook rate limiting.
- Svix wire format swap.
- Mailbox-level filtering or rules.

---

## Appendix A: Concrete code shape — `Publisher` interface evolution

**Today** ([internal/webhookpub/publisher.go:28-30](../../internal/webhookpub/publisher.go#L28-L30)):

```go
type Publisher interface {
    Publish(ctx context.Context, e Event)
}
```

**After this design:**

```go
type Publisher interface {
    // Publish keeps the LEGACY in-process fan-out path for compatibility
    // during the FeatureFlag rollout (slices 3-10). When the flag is on,
    // this becomes a thin wrapper that opens its own tx and calls
    // PublishTx. When the flag is off, the legacy in-process fan-out
    // (read enabled webhooks, filter, InsertPending) runs unchanged.
    //
    // After slice 11 (flag flipped to true permanently and verified in
    // production telemetry), the legacy in-process branch is deleted
    // and Publish becomes always the thin wrapper.
    //
    // Used by:
    //   - The /api/v1/webhooks/{id}/test endpoint? NO — that endpoint
    //     bypasses the outbox by writing directly to
    //     webhook_subscriber_deliveries.InsertPendingForTest. Do NOT
    //     route test events through Publish.
    //   - Future event sources without a surrounding business tx?
    //     Currently none — we removed the only such case (the
    //     fire-and-forget goroutine).
    Publish(ctx context.Context, e Event) error

    // PublishTx writes the outbox row inside the caller's transaction
    // and arranges for pg_notify to fire on commit. Caller must commit
    // their transaction for the event to become deliverable. On
    // outbox write failure, returns error so the caller can roll back
    // its business state — used for PRE-side-effect triggers.
    PublishTx(ctx context.Context, tx pgx.Tx, e Event) error

    // PublishBestEffortTx attempts the outbox write inside the caller's
    // transaction but never returns an error to the caller. On failure,
    // logs to webhook_publish_failures (for ops visibility +
    // future reconciliation) and lets the caller's tx commit anyway.
    // Used for POST-side-effect triggers (email.sent, email.approved)
    // where the irreversible action has already happened and rolling
    // back the business state would orphan an SES delivery.
    //
    // Guarantee downgrade: events fired via this path are
    // best-effort, not at-least-once. Customer docs reflect this.
    PublishBestEffortTx(ctx context.Context, tx pgx.Tx, e Event)
}
```

**`deliveryInserter` interface evolution** (currently at [internal/webhookpub/publisher.go:44-46](../../internal/webhookpub/publisher.go#L44-L46)):

```go
type deliveryInserter interface {
    // LEGACY — only used by the in-process fan-out path during
    // FeatureFlag-off rollout window. Deleted in slice 11.
    InsertPending(ctx context.Context, webhookID, eventType, messageID string, envelope []byte) error

    // InsertPendingBatchTx inserts ONE row per webhookID inside the
    // caller's tx, with per-row ON CONFLICT DO NOTHING that matches the
    // partial unique index in migration 026 verbatim:
    //   INSERT INTO webhook_subscriber_deliveries
    //     (id, webhook_id, event_id, event_type, event_payload, message_id, status, next_retry_at)
    //   VALUES ($1, $2, $3, $4, $5, $6, 'pending', now()), (...), ...
    //   ON CONFLICT (event_id, webhook_id)
    //     WHERE event_id IS NOT NULL AND replay_id IS NULL
    //     DO NOTHING
    //
    // Each row's id is generated fresh (whd_<32hex>) at INSERT time.
    // event_id is set to the originating webhook_events.id. replay_id
    // is NULL (this is the first-delivery path).
    InsertPendingBatchTx(ctx context.Context, tx pgx.Tx,
        eventID string, webhookIDs []string, eventType string,
        messageID *string, envelope []byte) error

    // InsertReplayTx inserts ONE delivery row for the /events/{id}/redeliver
    // endpoint. Sets event_id = the originating event and
    // replay_id = a fresh whd_<32hex> id (also used as the row's id),
    // so the partial unique index does NOT conflict with any existing
    // first-delivery or prior replay row.
    InsertReplayTx(ctx context.Context, tx pgx.Tx,
        eventID, webhookID, eventType string,
        messageID *string, envelope []byte) (replayDeliveryID string, err error)
}
```

**`InsertPendingForTest` forward-compat invariant.** The existing test-endpoint inserter at [internal/webhook/subscriber_store.go:178-190](../../internal/webhook/subscriber_store.go#L178-L190) currently doesn't write `event_id` or `replay_id` (those columns don't exist yet pre-#180). After migration 026, it must explicitly INSERT with `event_id = NULL, replay_id = NULL` so the partial unique index ignores test rows. Adding the columns without updating the test inserter is a silent bug — document this as part of slice 6's migration checklist.

## Appendix B: Migration 026 full text

(Sketched in §4.3 — final file lives at `migrations/026_webhook_events.sql`.)

## Appendix C: Migration 027 full text

(Sketched in §4.7 — final file at `migrations/027_retry_envelope_extension.sql`.)
