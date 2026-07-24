package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SubscriberDelivery is one row in webhook_subscriber_deliveries.
// Distinct from the legacy Delivery struct (which is keyed by
// message_id and tracks legacy single-URL delivery state).
type SubscriberDelivery struct {
	ID             string
	WebhookID      string
	EventType      string
	EventPayload   []byte // pre-marshalled envelope bytes; POSTed verbatim
	MessageID      *string
	Status         string // pending | delivered | failed
	Attempts       int
	MaxAttempts    int
	LastError      string
	LastStatusCode *int
	LastAttemptAt  *time.Time
	NextRetryAt    time.Time
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// SubscriberStore manages webhook_subscriber_deliveries. Parallel to
// the legacy DeliveryStore (which manages webhook_deliveries).
type SubscriberStore struct {
	pool *pgxpool.Pool
}

func NewSubscriberStore(pool *pgxpool.Pool) *SubscriberStore {
	return &SubscriberStore{pool: pool}
}

// MarkDelivered transitions a row to status='delivered' and stamps
// last_attempt_at + last_status_code. Also bumps webhooks.last_delivered_at
// in the same transaction so list views show the freshest activity.
func (s *SubscriberStore) MarkDelivered(ctx context.Context, deliveryID string, statusCode int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var webhookID string
	if err := tx.QueryRow(ctx,
		`UPDATE webhook_subscriber_deliveries
		 SET status = 'delivered',
		     last_attempt_at = now(),
		     last_status_code = $2,
		     attempts = attempts + 1
		 WHERE id = $1
		 RETURNING webhook_id`,
		deliveryID, statusCode,
	).Scan(&webhookID); err != nil {
		return fmt.Errorf("mark delivered: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE webhooks SET last_delivered_at = now() WHERE id = $1`,
		webhookID,
	); err != nil {
		return fmt.Errorf("bump last_delivered_at: %w", err)
	}
	return tx.Commit(ctx)
}

// GetSubscriberDeliveryByID loads a single delivery row by id — the River
// DeliverWorker's entry point (it holds only the delivery id and reads the
// payload + webhook_id here). Returns pgx.ErrNoRows if the row is gone.
func (s *SubscriberStore) GetSubscriberDeliveryByID(ctx context.Context, deliveryID string) (*SubscriberDelivery, error) {
	var d SubscriberDelivery
	err := s.pool.QueryRow(ctx,
		`SELECT id, webhook_id, event_type, event_payload, message_id,
		        status, attempts, max_attempts, COALESCE(last_error, ''),
		        last_status_code, last_attempt_at, next_retry_at, created_at, expires_at
		   FROM webhook_subscriber_deliveries WHERE id = $1`,
		deliveryID,
	).Scan(&d.ID, &d.WebhookID, &d.EventType, &d.EventPayload, &d.MessageID,
		&d.Status, &d.Attempts, &d.MaxAttempts, &d.LastError,
		&d.LastStatusCode, &d.LastAttemptAt, &d.NextRetryAt, &d.CreatedAt, &d.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// RecordSubscriberAttempt records ONE failed attempt without deciding retry or
// terminality — under River the retry schedule and the give-up decision belong to
// the job, not the store. Status stays 'pending'; attempts/last_error/
// last_status_code/last_attempt_at are updated. (Contrast RecordAttemptFailure,
// the hand-rolled path, which also computed next_retry_at and flipped to 'failed'
// at the cap — retired with the legacy worker.) attemptN is the River job's
// attempt number, written verbatim so the history API's attempts count matches
// River's.
func (s *SubscriberStore) RecordSubscriberAttempt(ctx context.Context, deliveryID string, attemptN int, errMsg string, statusCode int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE webhook_subscriber_deliveries
		    SET status = 'pending', attempts = $2, last_attempt_at = now(),
		        last_error = $3, last_status_code = $4
		  WHERE id = $1`,
		deliveryID, attemptN, errMsg, statusCode,
	)
	return err
}

// MarkSubscriberFailed transitions a delivery to terminal 'failed' — called by
// the DeliverWorker on the last (River-exhausted) attempt, and as an ErrorHandler
// backstop on discard. Records the final attempt count + error.
func (s *SubscriberStore) MarkSubscriberFailed(ctx context.Context, deliveryID string, attemptN int, errMsg string, statusCode int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE webhook_subscriber_deliveries
		    SET status = 'failed', attempts = $2, last_attempt_at = now(),
		        last_error = $3, last_status_code = $4
		  WHERE id = $1`,
		deliveryID, attemptN, errMsg, statusCode,
	)
	return err
}

// generateDeliveryID returns a prefixed id of the form whd_<32-hex>.
// 16 bytes of entropy is more than enough — the row is short-lived
// (30-day expiry), and the prefix follows the rest of the e2a id
// scheme so logs and dashboards can spot a delivery id at a glance.
// Matches the publisher's inline generator at webhookpub/publisher.go
// so customers see one consistent prefix regardless of which path
// created the row (publisher fan-out vs. /test endpoint).
func generateDeliveryID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("webhook: crypto/rand failed: %v", err))
	}
	return "whd_" + hex.EncodeToString(b)
}

// InsertPendingForTest creates a single delivery row tied to the given
// webhook + event type with the supplied envelope bytes. The retry
// worker picks it up on the next tick. Used by the
// POST /v1/webhooks/{id}/test endpoint to schedule a one-off
// delivery without going through the publisher's filter-matching path.
func (s *SubscriberStore) InsertPendingForTest(ctx context.Context, webhookID, eventType string, envelope []byte) (string, error) {
	id := generateDeliveryID()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries
		     (id, webhook_id, event_type, event_payload, status, next_retry_at)
		 VALUES ($1, $2, $3, $4, 'pending', now())`,
		id, webhookID, eventType, envelope,
	)
	if err != nil {
		return "", fmt.Errorf("insert test delivery: %w", err)
	}
	return id, nil
}

// expiredDeleteBatch bounds one statement in the batched retention sweep —
// webhook_subscriber_deliveries scales with delivery volume, so the janitor prunes it
// in ctid-bounded chunks to keep each statement's lock/WAL small. Caller's ctx bounds
// total runtime; a partial sweep resumes next hour (idempotent).
const expiredDeleteBatch = 5000

// DeleteExpiredSubscriberDeliveries enforces the 30-day TTL (migration 025) in two
// phases; without it the table grows monotonically and query plans degrade. Mirrors
// DeliveryStore.DeleteExpiredDeliveries for the legacy table, with one refinement:
//
//  1. DELETE expired rows already in a terminal state (delivered/failed) — as before.
//  2. Expired rows still 'pending' are NOT deleted but marked terminally 'failed'
//     ("expired before delivery"), so a delivery record never silently vanishes
//     while pending: the transition is logged here and visible in the delivery-
//     history API until the NEXT sweep deletes the now-terminal row.
//
// Phase 2's population should be ~empty now that the delivery reconciler also
// rescues dead-job strands; what remains is essentially rows snoozing behind a
// webhook disabled for longer than the TTL. Marking those failed (instead of a
// bare `status <> 'pending'` guard on the DELETE) keeps them from becoming
// immortal — unbounded growth behind a permanently-disabled webhook. Phase 2 runs
// AFTER phase 1 so a freshly marked row survives until the following sweep. The
// DeliverWorker treats any non-pending row as a no-op, so a still-snoozing job
// waking up later never POSTs a row marked here.
//
// Returns the number of rows deleted (marked rows are logged, not counted).
func (s *SubscriberStore) DeleteExpiredSubscriberDeliveries(ctx context.Context) (int, error) {
	var total int
	for {
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM webhook_subscriber_deliveries WHERE ctid IN (
			   SELECT ctid FROM webhook_subscriber_deliveries
			    WHERE expires_at <= now() AND status <> 'pending' LIMIT $1)`,
			expiredDeleteBatch)
		if err != nil {
			return total, err
		}
		n := int(tag.RowsAffected())
		total += n
		if n < expiredDeleteBatch {
			break
		}
	}

	var marked int
	for {
		tag, err := s.pool.Exec(ctx,
			`UPDATE webhook_subscriber_deliveries
			    SET status = 'failed', last_error = 'expired before delivery'
			  WHERE ctid IN (
			   SELECT ctid FROM webhook_subscriber_deliveries
			    WHERE expires_at <= now() AND status = 'pending' LIMIT $1)`,
			expiredDeleteBatch)
		if err != nil {
			return total, err
		}
		n := int(tag.RowsAffected())
		marked += n
		if n < expiredDeleteBatch {
			break
		}
	}
	if marked > 0 {
		log.Printf("[webhook-janitor] marked %d expired pending subscriber deliver(y/ies) failed (expired before delivery)", marked)
	}
	return total, nil
}

// ListDeliveriesByWebhook returns one page of delivery rows for the webhook,
// most-recent first, keyset-paginated on (created_at, id). When status is
// non-empty, restricts to that status (pending|delivered|failed). The caller
// passes limit (fetch limit+1 to detect a further page) and the after-key from
// the previous page's last row (zero afterCreatedAt = first page). The delivery
// log grows unbounded on a busy webhook, so it needs real pagination rather than
// silently truncating at a fixed cap. Limit is bounded by the caller; this
// method does not enforce a cap.
func (s *SubscriberStore) ListDeliveriesByWebhook(ctx context.Context, webhookID, status string, limit int, afterCreatedAt time.Time, afterID string) ([]SubscriberDelivery, error) {
	var (
		rowsErr error
		out     []SubscriberDelivery
	)
	q := `SELECT id, webhook_id, event_type, event_payload, message_id,
	             status, attempts, max_attempts, last_error,
	             last_status_code, last_attempt_at, next_retry_at,
	             created_at, expires_at
	      FROM webhook_subscriber_deliveries
	      WHERE webhook_id = $1`
	args := []interface{}{webhookID}
	if status != "" {
		q += fmt.Sprintf(` AND status = $%d`, len(args)+1)
		args = append(args, status)
	}
	if !afterCreatedAt.IsZero() {
		i := len(args) + 1
		q += fmt.Sprintf(` AND (created_at < $%d OR (created_at = $%d AND id < $%d))`, i, i, i+1)
		args = append(args, afterCreatedAt, afterID)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d SubscriberDelivery
		if err := rows.Scan(
			&d.ID, &d.WebhookID, &d.EventType, &d.EventPayload, &d.MessageID,
			&d.Status, &d.Attempts, &d.MaxAttempts, &d.LastError,
			&d.LastStatusCode, &d.LastAttemptAt, &d.NextRetryAt,
			&d.CreatedAt, &d.ExpiresAt,
		); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	rowsErr = rows.Err()
	return out, rowsErr
}
