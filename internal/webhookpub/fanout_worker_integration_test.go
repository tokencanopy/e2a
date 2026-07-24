package webhookpub_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// fakeFanOutEnq is a jobs.Enqueuer that records the FanOutArgs it was asked to enqueue
// and hands back monotonic job ids — no real River client needed. Mirrors the
// inboundprocess reconcile fake.
type fakeFanOutEnq struct {
	mu   sync.Mutex
	n    int64
	args []webhookpub.FanOutArgs
}

func (f *fakeFanOutEnq) Insert(_ context.Context, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return f.record(args)
}

func (f *fakeFanOutEnq) InsertTx(_ context.Context, _ pgx.Tx, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return f.record(args)
}

func (f *fakeFanOutEnq) record(args river.JobArgs) (*rivertype.JobInsertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if fa, ok := args.(webhookpub.FanOutArgs); ok {
		f.args = append(f.args, fa)
	}
	return &rivertype.JobInsertResult{Job: &rivertype.JobRow{ID: f.n}}, nil
}

// seedPendingEvent inserts a pending webhook_events row of the given type and returns
// its id. Mirrors the worker_integration_test seeding.
func seedPendingEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID, msgKey, eventType string) string {
	t.Helper()
	eventID := webhookpub.DeterministicEventID(msgKey, eventType)
	env, _ := json.Marshal(webhookpub.Envelope{Type: eventType, ID: eventID, CreatedAt: time.Now().UTC()})
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_events (id, user_id, type, envelope, status) VALUES ($1, $2, $3, $4, 'pending')`,
		eventID, userID, eventType, env); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return eventID
}

// cleanupWebhookFixture tears down the chain seeded by seedWebhookFixture plus any
// events/deliveries the fan-out tests created.
func cleanupWebhookFixture(ctx context.Context, pool *pgxpool.Pool, userID, webhookID string) {
	_, _ = pool.Exec(ctx, `DELETE FROM webhook_subscriber_deliveries WHERE webhook_id = $1`, webhookID)
	_, _ = pool.Exec(ctx, `DELETE FROM webhook_events WHERE user_id = $1`, userID)
	_, _ = pool.Exec(ctx, `DELETE FROM webhooks WHERE id = $1`, webhookID)
	_, _ = pool.Exec(ctx, `DELETE FROM agent_identities WHERE user_id = $1`, userID)
	_, _ = pool.Exec(ctx, `DELETE FROM domains WHERE user_id = $1`, userID)
	_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
}

// TestFanOutWorker_Integration_FansOutAndEnqueues drives the River FanOutWorker over a
// pending event: it inserts the Layer 2 delivery row, enqueues its delivery job in the
// same tx (job_id stamped), and marks the event 'processed'. A second Work on the now-
// processed event is a no-op (idempotent re-run) — no duplicate enqueue.
func TestFanOutWorker_Integration_FansOutAndEnqueues(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	userID, _, webhookID := seedWebhookFixture(t, ctx, pool, store, "fo_enq")
	t.Cleanup(func() { cleanupWebhookFixture(ctx, pool, userID, webhookID) })

	eventID := seedPendingEvent(t, ctx, pool, userID, "msg_fo_1", webhookpub.EventEmailReceived)

	enq := &fakeDeliveryEnqueuer{}
	w := webhookpub.NewFanOutWorker(pool, store, enq, nil)
	if err := w.Work(ctx, &river.Job[webhookpub.FanOutArgs]{Args: webhookpub.FanOutArgs{EventID: eventID}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	var deliveryID string
	var jobID *int64
	if err := pool.QueryRow(ctx,
		`SELECT id, job_id FROM webhook_subscriber_deliveries WHERE event_id = $1 AND webhook_id = $2`,
		eventID, webhookID,
	).Scan(&deliveryID, &jobID); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if len(enq.ids) != 1 || enq.ids[0] != deliveryID {
		t.Fatalf("enqueued ids = %v, want [%s]", enq.ids, deliveryID)
	}
	if jobID == nil || *jobID != 1 {
		t.Errorf("job_id = %v, want 1 (stamped from the enqueue)", jobID)
	}

	var status string
	var matched []string
	if err := pool.QueryRow(ctx,
		`SELECT status, matched_webhook_ids FROM webhook_events WHERE id = $1`, eventID,
	).Scan(&status, &matched); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if status != "processed" {
		t.Errorf("status = %q, want processed", status)
	}
	if len(matched) != 1 || matched[0] != webhookID {
		t.Errorf("matched_webhook_ids = %v, want [%s]", matched, webhookID)
	}

	// Idempotent re-run: the event is 'processed' now, so Work is a no-op — no second
	// enqueue, status unchanged.
	if err := w.Work(ctx, &river.Job[webhookpub.FanOutArgs]{Args: webhookpub.FanOutArgs{EventID: eventID}}); err != nil {
		t.Fatalf("Work (re-run): %v", err)
	}
	if len(enq.ids) != 1 {
		t.Errorf("after re-run, enqueue calls = %d, want 1 (idempotent)", len(enq.ids))
	}
}

// TestFanOutWorker_Integration_NoMatch: an event whose type no enabled webhook
// subscribes to transitions to 'no_match' with zero deliveries and zero enqueues.
func TestFanOutWorker_Integration_NoMatch(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	// The fixture's webhook subscribes to email.received only.
	userID, _, webhookID := seedWebhookFixture(t, ctx, pool, store, "fo_nomatch")
	t.Cleanup(func() { cleanupWebhookFixture(ctx, pool, userID, webhookID) })

	eventID := seedPendingEvent(t, ctx, pool, userID, "msg_fo_nm", webhookpub.EventEmailSent)

	enq := &fakeDeliveryEnqueuer{}
	w := webhookpub.NewFanOutWorker(pool, store, enq, nil)
	if err := w.Work(ctx, &river.Job[webhookpub.FanOutArgs]{Args: webhookpub.FanOutArgs{EventID: eventID}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM webhook_events WHERE id = $1`, eventID).Scan(&status); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if status != "no_match" {
		t.Errorf("status = %q, want no_match", status)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhook_subscriber_deliveries WHERE event_id = $1`, eventID).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if n != 0 {
		t.Errorf("deliveries = %d, want 0", n)
	}
	if len(enq.ids) != 0 {
		t.Errorf("enqueue calls = %d, want 0", len(enq.ids))
	}
}

// TestFanOutWorker_Integration_EventGoneReturnsNil: a job for an event that no longer
// exists (30d GC before fan-out) returns nil — nothing to do, not an error to retry.
func TestFanOutWorker_Integration_EventGoneReturnsNil(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	w := webhookpub.NewFanOutWorker(pool, store, &fakeDeliveryEnqueuer{}, nil)
	err := w.Work(ctx, &river.Job[webhookpub.FanOutArgs]{Args: webhookpub.FanOutArgs{EventID: "evt_does_not_exist"}})
	if err != nil {
		t.Errorf("Work on missing event = %v, want nil", err)
	}
}

// fanOutJobCount counts the webhook_fanout river_jobs carrying the given event id
// with id > sinceID. The harness truncates e2a tables between tests but never
// river_job, and seedPendingEvent's ids are deterministic — so counts must be
// scoped past a baseline (maxRiverJobID at test start) or a previous run's
// leftover job for the same event id pollutes them.
func fanOutJobCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID string, sinceID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'webhook_fanout' AND args->>'event_id' = $1 AND id > $2`,
		eventID, sinceID).Scan(&n); err != nil {
		t.Fatalf("count fan-out jobs for %s: %v", eventID, err)
	}
	return n
}

// maxRiverJobID returns the current high-water river_job id (0 when empty), the
// baseline for fanOutJobCount.
func maxRiverJobID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM river_job`).Scan(&id); err != nil {
		t.Fatalf("read max river_job id: %v", err)
	}
	return id
}

// TestFanOutJobs_Integration_ReconcilePending: a pending event with no fan-out job is
// re-enqueued and its fanout_job_id stamped; a re-run does not double-enqueue it.
// Uses a REAL River client: the reconciler also rescues events whose stamped job is
// dead (missing/terminal), so the idempotency assertion requires the stamped id to
// reference a live river_job — a fake enqueuer's synthetic id would read as pruned.
func TestFanOutJobs_Integration_ReconcilePending(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}

	userID, _, webhookID := seedWebhookFixture(t, ctx, pool, store, "fo_recon")
	t.Cleanup(func() { cleanupWebhookFixture(ctx, pool, userID, webhookID) })

	eventID := seedPendingEvent(t, ctx, pool, userID, "msg_fo_rc", webhookpub.EventEmailReceived)
	baseJobID := maxRiverJobID(t, ctx, pool)

	j := webhookpub.NewFanOutJobs(pool, store, &fakeDeliveryEnqueuer{}, nil)
	client, err := jobs.New(pool, jobs.Config{}, j)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	j.SetEnqueuer(client)

	if _, err := j.ReconcilePending(ctx, pool); err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}

	// Our event got a job stamped and a real webhook_fanout job carries its id.
	var jobID *int64
	if err := pool.QueryRow(ctx, `SELECT fanout_job_id FROM webhook_events WHERE id = $1`, eventID).Scan(&jobID); err != nil {
		t.Fatalf("read fanout_job_id: %v", err)
	}
	if jobID == nil {
		t.Fatalf("fanout_job_id = nil, want stamped after reconcile")
	}
	if n := fanOutJobCount(t, ctx, pool, eventID, baseJobID); n != 1 {
		t.Errorf("webhook_fanout jobs for %s = %d, want 1", eventID, n)
	}

	// Re-run: our event now has a live job, so the reconciler skips it.
	if _, err := j.ReconcilePending(ctx, pool); err != nil {
		t.Fatalf("ReconcilePending (re-run): %v", err)
	}
	if n := fanOutJobCount(t, ctx, pool, eventID, baseJobID); n != 1 {
		t.Errorf("after re-run, webhook_fanout jobs for %s = %d, want 1 (idempotent)", eventID, n)
	}
}

// TestFanOutJobs_Integration_RescuesDeadFanOutJob is the fan-out strand
// regression test: a webhook_fanout job that River discarded (maxFanOutAttempts
// exhausted) — or that River's pruner already deleted — leaves the event
// 'pending' with fanout_job_id stamped, invisible to a reconciler keyed only on
// fanout_job_id IS NULL. The reconciler must rescue such events (fresh job,
// re-stamped id) and the rescued event must then fan out to completion.
func TestFanOutJobs_Integration_RescuesDeadFanOutJob(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}

	userID, _, webhookID := seedWebhookFixture(t, ctx, pool, store, "fo_dead")
	t.Cleanup(func() { cleanupWebhookFixture(ctx, pool, userID, webhookID) })

	evDiscarded := seedPendingEvent(t, ctx, pool, userID, "msg_fo_dead_d", webhookpub.EventEmailReceived)
	evMissing := seedPendingEvent(t, ctx, pool, userID, "msg_fo_dead_m", webhookpub.EventEmailReceived)
	evAlive := seedPendingEvent(t, ctx, pool, userID, "msg_fo_dead_a", webhookpub.EventEmailReceived)

	insertJob := func(state string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO river_job (state, kind, args, max_attempts, finalized_at)
			 VALUES ($1::river_job_state, 'webhook_fanout', '{}'::jsonb, 10,
			         CASE WHEN $1 IN ('cancelled','completed','discarded') THEN now() END)
			 RETURNING id`, state).Scan(&id); err != nil {
			t.Fatalf("insert river_job(%s): %v", state, err)
		}
		return id
	}
	discardedJob := insertJob("discarded")
	aliveJob := insertJob("available")
	missingJob := int64(1) << 60 // never a real river_job id (pruned strand)
	stamp := func(eventID string, jobID int64) {
		if _, err := pool.Exec(ctx,
			`UPDATE webhook_events SET fanout_job_id = $2 WHERE id = $1`, eventID, jobID); err != nil {
			t.Fatalf("stamp %s: %v", eventID, err)
		}
	}
	stamp(evDiscarded, discardedJob)
	stamp(evMissing, missingJob)
	stamp(evAlive, aliveJob)

	j := webhookpub.NewFanOutJobs(pool, store, &fakeDeliveryEnqueuer{}, nil)
	client, err := jobs.New(pool, jobs.Config{}, j)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	j.SetEnqueuer(client)

	n, err := j.ReconcilePending(ctx, pool)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if n != 2 {
		t.Errorf("reconcile enqueued %d events, want 2 (discarded-job + missing-job strands)", n)
	}

	fanoutJobIDOf := func(eventID string) int64 {
		var jobID *int64
		if err := pool.QueryRow(ctx, `SELECT fanout_job_id FROM webhook_events WHERE id = $1`, eventID).Scan(&jobID); err != nil {
			t.Fatalf("read fanout_job_id for %s: %v", eventID, err)
		}
		if jobID == nil {
			t.Fatalf("event %s has NULL fanout_job_id", eventID)
		}
		return *jobID
	}
	if got := fanoutJobIDOf(evDiscarded); got == discardedJob {
		t.Errorf("discarded-job event still stamped %d, want a fresh job id", got)
	}
	if got := fanoutJobIDOf(evMissing); got == missingJob {
		t.Errorf("missing-job event still stamped %d, want a fresh job id", got)
	}
	if got := fanoutJobIDOf(evAlive); got != aliveJob {
		t.Errorf("live-job event fanout_job_id = %d, want untouched %d", got, aliveJob)
	}

	// Idempotent: rescued events now carry live jobs — a re-run enqueues nothing.
	n2, err := j.ReconcilePending(ctx, pool)
	if err != nil {
		t.Fatalf("ReconcilePending re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("re-run enqueued %d events, want 0 (rescued events carry live jobs)", n2)
	}

	// And a rescued event actually fans out to completion.
	enq := &fakeDeliveryEnqueuer{}
	w := webhookpub.NewFanOutWorker(pool, store, enq, nil)
	if err := w.Work(ctx, &river.Job[webhookpub.FanOutArgs]{Args: webhookpub.FanOutArgs{EventID: evDiscarded}}); err != nil {
		t.Fatalf("Work on rescued event: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM webhook_events WHERE id = $1`, evDiscarded).Scan(&status); err != nil {
		t.Fatalf("read event status: %v", err)
	}
	if status != "processed" {
		t.Errorf("rescued event status = %q, want processed", status)
	}
	var deliveries int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_subscriber_deliveries WHERE event_id = $1 AND webhook_id = $2`,
		evDiscarded, webhookID).Scan(&deliveries); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveries != 1 {
		t.Errorf("deliveries for rescued event = %d, want 1", deliveries)
	}
}
