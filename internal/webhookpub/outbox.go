package webhookpub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Outbox is the Stripe-tier publisher entry point. Triggers write the
// webhook_events row inside the same transaction as their business
// state (e.g. the messages row for email.received). A separate
// publisher worker (slice 2) consumes pending rows and fans out into
// webhook_subscriber_deliveries.
//
// Outbox.PublishTx writes ONE row to webhook_events inside the trigger's
// transaction and arranges NOTIFY; the OutboxWorker (slice 2) fans it out into
// webhook_subscriber_deliveries and enqueues a River delivery job per row. This
// is now the sole event path — the legacy in-process fan-out publisher is
// retired and River is the sole delivery engine.
//
// See docs/design/2026-06-01-stripe-tier-webhooks.md §4.2 and Appendix A.
type Outbox interface {
	// PublishTx writes the event to webhook_events inside the
	// caller's transaction. Returns error so the caller can roll back
	// its business state if the outbox write fails.
	//
	// Used for PRE-side-effect triggers (email.received, future
	// email.bounced from SNS, email.review_requested, email.review_rejected).
	// If the outbox write fails the caller's tx rolls back; on
	// retry, the deterministic event id makes the second outbox
	// INSERT a no-op via ON CONFLICT (id) DO NOTHING.
	PublishTx(ctx context.Context, tx pgx.Tx, e Event) error

	// PublishBestEffortTx attempts the outbox write inside the
	// caller's transaction but never returns an error. On failure,
	// logs and lets the caller's tx commit anyway (a future slice may pipe these
	// to a webhook_publish_failures table — not yet built).
	//
	// Returns `wrote=true` iff the row was actually written (flag
	// enabled AND writeOutboxRow succeeded). The wrote signal is retained for
	// observability; with the outbox unconditional there is no legacy fallback
	// path left to gate.
	//
	// Used for POST-side-effect triggers (email.sent, email.review_approved)
	// where the irreversible action (SES.Send) has already happened
	// and rolling back the business state would orphan an SES
	// delivery.
	PublishBestEffortTx(ctx context.Context, tx pgx.Tx, e Event) (wrote bool)

	// DeleteExpiredWebhookEvents drops rows past their 30-day retention.
	// Called from the hourly cleanup loop in cmd/e2a/main.go.
	DeleteExpiredWebhookEvents(ctx context.Context) (int, error)

	// Enabled reports whether the outbox writes durable event rows for this
	// deployment. Now unconditional (StaticFlag(true) in production) — the events
	// API gates its list/get/redeliver endpoints on this, and the outbox writer
	// no-ops when false. Retained as a seam; there is no longer a legacy
	// fan-out path to fall back to.
	Enabled() bool

	// SetFanOutEnqueuer wires the River fan-out enqueuer (two-phase, called after the
	// shared client exists). When set (E2A_WEBHOOK_FANOUT_MODE=river), PublishTx /
	// PublishBestEffortTx enqueue a webhook_fanout job in the event's own tx and stamp
	// fanout_job_id — the River FanOutWorker then does the fan-out, replacing the
	// legacy in-process OutboxWorker drain. nil (default/legacy) ⇒ the event row is
	// written as before and the OutboxWorker fans it out via LISTEN/NOTIFY + poll.
	SetFanOutEnqueuer(e FanOutEnqueuer)
}

// FanOutEnqueuer enqueues a webhook fan-out job within the caller's transaction and
// returns the river_job id. *FanOutJobs satisfies it. Injected (not imported) so the
// outbox writer stays decoupled from the River wiring — mirrors DeliveryEnqueuer.
type FanOutEnqueuer interface {
	EnqueueFanOutTx(ctx context.Context, tx pgx.Tx, eventID string) (int64, error)
}

// outboxExecutor is the subset of pgx.Tx and pgxpool.Pool needed by
// the outbox writer. Mirrors the agentExecutor pattern at
// internal/identity/store.go:600 — same SQL body works for both
// stand-alone and in-transaction callers.
type outboxExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// outbox is the production Outbox backed by a pgxpool.
type outbox struct {
	pool      *pgxpool.Pool
	flag      FeatureFlag
	fanoutEnq FanOutEnqueuer // nil ⇒ legacy OutboxWorker drain; set ⇒ River fan-out
}

// NewOutbox constructs the Stripe-tier outbox writer. The FeatureFlag
// gates writes: when disabled, PublishTx is a no-op (returns nil with
// no DB write). Production passes StaticFlag(true) — the outbox is now the
// unconditional, sole event path.
func NewOutbox(pool *pgxpool.Pool, flag FeatureFlag) Outbox {
	if flag == nil {
		flag = StaticFlag(false)
	}
	return &outbox{pool: pool, flag: flag}
}

func (o *outbox) PublishTx(ctx context.Context, tx pgx.Tx, e Event) error {
	if !o.flag.Enabled() {
		return nil
	}
	inserted, err := writeOutboxRow(ctx, tx, e)
	if err != nil {
		return err
	}
	// Pre-side-effect trigger: the fan-out enqueue rides the SAME tx, and an enqueue
	// failure MUST fail the whole tx so the trigger retries with the (deterministic)
	// event id — no side effect has happened yet, so a rollback loses nothing. Only on
	// a real insert: a deduped retry (ON CONFLICT no-op) already has its fan-out job.
	if inserted {
		return o.enqueueFanOutTx(ctx, tx, e.ID)
	}
	return nil
}

// SetFanOutEnqueuer implements Outbox — see the interface doc.
func (o *outbox) SetFanOutEnqueuer(e FanOutEnqueuer) { o.fanoutEnq = e }

// enqueueFanOutTx enqueues the fan-out job in the caller's tx and stamps fanout_job_id.
// Returns any error to the caller (the pre-side-effect path rolls the tx back on it).
// No-op when the fan-out enqueuer is unset (legacy mode) — the OutboxWorker drains.
func (o *outbox) enqueueFanOutTx(ctx context.Context, tx pgx.Tx, eventID string) error {
	if o.fanoutEnq == nil {
		return nil
	}
	jobID, err := o.fanoutEnq.EnqueueFanOutTx(ctx, tx, eventID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE webhook_events SET fanout_job_id = $2 WHERE id = $1`, eventID, jobID)
	return err
}

// enqueueFanOutBestEffort does the same inside a SAVEPOINT so a failure can't poison
// the caller's must-commit tx (the post-side-effect path — SES already sent). On any
// error it rolls back to the savepoint and logs, leaving the event row committed
// without a job for the fan-out reconciler (fanout_job_id IS NULL) to re-drive within
// a minute.
func (o *outbox) enqueueFanOutBestEffort(ctx context.Context, tx pgx.Tx, eventID string) {
	if o.fanoutEnq == nil {
		return
	}
	sp, err := tx.Begin(ctx) // pgx nested tx == SAVEPOINT
	if err != nil {
		log.Printf("[outbox] fan-out savepoint begin (event=%s): %v — reconciler will re-drive", eventID, err)
		return
	}
	if err := o.enqueueFanOutTx(ctx, sp, eventID); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the caller's tx stays intact
		log.Printf("[outbox] fan-out enqueue (event=%s): %v — reconciler will re-drive", eventID, err)
		return
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		log.Printf("[outbox] fan-out savepoint release (event=%s): %v — reconciler will re-drive", eventID, err)
	}
}

// Enabled mirrors the flag state. Trigger sites check this to suppress
// the legacy publisher.Publish goroutine when the outbox is the
// durable fan-out path; see the Outbox interface docstring for the
// reasoning.
func (o *outbox) Enabled() bool {
	return o.flag.Enabled()
}

// DeleteExpiredWebhookEvents removes terminal rows whose expires_at has
// passed. Migration 026 sets a 30-day TTL on every event row; without
// this janitor the table grows monotonically and the (user_id,
// created_at) index degrades. The parallel slice-2 sweep
// (webhook.SubscriberStore.DeleteExpiredSubscriberDeliveries) differs
// deliberately: delivery rows expired while pending are marked failed
// then pruned (their retry budget is finite), whereas pending EVENT
// rows are retried forever — see below.
//
// Returns the number of rows deleted.
//
// The status guard is load-bearing. The outbox is at-least-once by
// design — see worker.go's recordFailure docstring: "there is NO
// terminal 'failed' state on the outbox … we let the row stay pending
// until human intervention or a successful retry." If we delete
// pending rows at 30 days the retry-forever guarantee silently breaks,
// dropping events that never reached any webhook. A row only becomes
// eligible for the sweep once the worker has marked it terminal
// (`processed` after a successful fan-out, `no_match` when no enabled
// webhook subscribed).
//
// Trade-off: a row that's broken on the retry path (e.g. a downstream
// SELECT panics every iteration) will accumulate forever rather than
// fall out at day 30. That's the "page ops after many attempts" case
// recordFailure calls out — preferable to silent loss.
// expiredDeleteBatch bounds one DELETE in the batched retention sweep — webhook_events
// grows monotonically with traffic, so the janitor prunes it in ctid-bounded chunks to
// keep each statement's lock/WAL small. Caller's ctx bounds total runtime; a partial
// sweep resumes next hour (idempotent).
const expiredDeleteBatch = 5000

func (o *outbox) DeleteExpiredWebhookEvents(ctx context.Context) (int, error) {
	var total int
	for {
		tag, err := o.pool.Exec(ctx,
			`DELETE FROM webhook_events WHERE ctid IN (
			   SELECT ctid FROM webhook_events WHERE expires_at <= now() AND status <> 'pending' LIMIT $1)`,
			expiredDeleteBatch)
		if err != nil {
			return total, fmt.Errorf("delete expired webhook_events: %w", err)
		}
		n := int(tag.RowsAffected())
		total += n
		if n < expiredDeleteBatch {
			return total, nil
		}
	}
}

func (o *outbox) PublishBestEffortTx(ctx context.Context, tx pgx.Tx, e Event) (wrote bool) {
	if !o.flag.Enabled() {
		return false
	}
	inserted, err := writeOutboxRow(ctx, tx, e)
	if err != nil {
		// Best-effort: log and return wrote=false. The caller's tx
		// commits the business state regardless because the
		// irreversible action (SES.Send) already happened — rolling
		// back would orphan a sent email. For now the log is the only
		// signal (a future slice may add a webhook_publish_failures table).
		log.Printf("[outbox] PublishBestEffortTx err (event=%s type=%s): %v", e.ID, e.Type, err)
		return false
	}
	// Post-side-effect trigger: the fan-out enqueue must NEVER fail the caller's tx (a
	// rollback would orphan a sent email), so it rides a savepoint — a failure re-drives
	// via the reconciler, not by losing the send. Only on a real insert.
	if inserted {
		o.enqueueFanOutBestEffort(ctx, tx, e.ID)
	}
	return true
}

// writeOutboxRow is the SQL body shared by PublishTx and PublishBestEffortTx.
// Idempotent on (id): a retried trigger with the same deterministic id no-ops the
// second INSERT (RowsAffected reports 0, returned as inserted=false). Issues pg_notify
// so the legacy OutboxWorker (LISTEN/NOTIFY mode) wakes immediately on commit; in
// river fan-out mode nothing LISTENs and the NOTIFY is a harmless no-op.
func writeOutboxRow(ctx context.Context, exec outboxExecutor, e Event) (inserted bool, err error) {
	if e.ID == "" {
		return false, fmt.Errorf("webhookpub: outbox event must have non-empty ID")
	}
	if e.UserID == "" {
		return false, fmt.Errorf("webhookpub: outbox event must have non-empty UserID")
	}
	if e.Type == "" {
		return false, fmt.Errorf("webhookpub: outbox event must have non-empty Type")
	}

	envelopeJSON, err := json.Marshal(e.AsEnvelope())
	if err != nil {
		return false, fmt.Errorf("webhookpub: marshal envelope: %w", err)
	}

	var messageID *string
	if e.MessageID != "" {
		mid := e.MessageID
		messageID = &mid
	}
	var agentID *string
	if e.AgentID != "" {
		aid := e.AgentID
		agentID = &aid
	}
	var conversationID *string
	if e.ConversationID != "" {
		cid := e.ConversationID
		conversationID = &cid
	}

	// created_at and expires_at use the column DEFAULTs so the
	// timestamps come from the Postgres server clock (one clock per
	// primary writer; no application-side skew).
	tag, err := exec.Exec(ctx,
		`INSERT INTO webhook_events
		    (id, user_id, type, aud, envelope, schema_version,
		     agent_id, conversation_id, message_id, status)
		 VALUES ($1, $2, $3, 'webhook', $4, $8, $5, $6, $7, 'pending')
		 ON CONFLICT (id) DO NOTHING`,
		e.ID, e.UserID, e.Type, envelopeJSON,
		agentID, conversationID, messageID, SchemaVersion,
	)
	if err != nil {
		return false, fmt.Errorf("webhookpub: insert webhook_events: %w", err)
	}
	// RowsAffected is 1 on a real insert, 0 when ON CONFLICT skipped a duplicate
	// (deterministic-id retry). The caller enqueues a fan-out job only on a real
	// insert — a deduped event already has one.
	inserted = tag.RowsAffected() > 0

	// In river fan-out mode no worker LISTENs webhook_events_new (the OutboxWorker
	// isn't started), so this NOTIFY is wasted — one cheap statement per event. It
	// stays unconditional to keep the legacy default working; drop it when the legacy
	// OutboxWorker is deleted post-cutover.
	//
	// pg_notify is best-effort: NOTIFY only fires on COMMIT (Postgres
	// queues it). If COMMIT fails, no notification is emitted. The
	// legacy OutboxWorker's 1s fallback poll catches missed wakeups
	// (deploy windows, LISTEN reconnect races). Payload is empty
	// because the worker rescans the table anyway.
	//
	// A pg_notify error here (NOTIFY queue overflow is the realistic
	// case; max_notify_queue_pages defaults to 1024 × 8KB = 8MB) is a
	// SOFT failure: we log and return nil so the caller's tx still
	// commits. The webhook_events row is what matters for at-least-
	// once delivery; the NOTIFY is only a latency optimization that
	// the worker's 1s fallback poll covers.
	//
	// Returning the error here would propagate out of writeOutboxRow,
	// out of PublishTx, and out of the caller's WithTx — rolling back
	// the entire trigger transaction. In the relay path that means
	// the inbound `messages` row is also lost; SMTP returns a 4xx to
	// the MTA which retries into the same broken state. (See PR for
	// C2 in the audit.)
	if _, nerr := exec.Exec(ctx, `SELECT pg_notify('webhook_events_new', '')`); nerr != nil {
		log.Printf("[outbox] pg_notify webhook_events_new failed (event=%s, type=%s): %v — worker fallback poll will recover",
			e.ID, e.Type, nerr)
	}
	return inserted, nil
}
