package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/eventpayload/goldenassert"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

func lifecycleRows(t *testing.T, pool *pgxpool.Pool, messageID string) []messagelifecycle.MessageLifecycleTransition {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, message_id, direction, COALESCE(recipient,''), stage, outcome, reason_code, retryable, evidence, correlation_ids, occurred_at, reconstructed FROM message_lifecycle_transitions WHERE message_id=$1 ORDER BY occurred_at,id`, messageID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []messagelifecycle.MessageLifecycleTransition
	for rows.Next() {
		var tr messagelifecycle.MessageLifecycleTransition
		var evidence, correlations []byte
		if err := rows.Scan(&tr.ID, &tr.MessageID, &tr.Direction, &tr.Recipient, &tr.Stage, &tr.Outcome, &tr.ReasonCode, &tr.Retryable, &evidence, &correlations, &tr.OccurredAt, &tr.Reconstructed); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(evidence, &tr.Evidence); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(correlations, &tr.CorrelationIDs); err != nil {
			t.Fatal(err)
		}
		got = append(got, tr)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func eventLifecycle(t *testing.T, pool *pgxpool.Pool, messageID, eventType string) []messagelifecycle.MessageLifecycleTransition {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT envelope->'data'->'lifecycle_transitions' FROM webhook_events WHERE message_id=$1 AND type=$2`, messageID, eventType).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got []messagelifecycle.MessageLifecycleTransition
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// fakeOutboundEnqueuer stands in for the shared River client: it records the
// in-tx enqueue and returns a stable job id the accept-tx stamps on the row. For
// a scheduled send it also records the requested run instant so tests can assert
// the send_at was threaded through to the job.
type fakeOutboundEnqueuer struct {
	jobID       int64
	scheduledAt time.Time // set when EnqueueScheduledSendTx was used
	cancelled   []int64
}

func (f *fakeOutboundEnqueuer) EnqueueSendTx(_ context.Context, _ pgx.Tx, _ string) (int64, error) {
	return f.jobID, nil
}

func (f *fakeOutboundEnqueuer) EnqueueScheduledSendTx(_ context.Context, _ pgx.Tx, _ string, at time.Time) (int64, error) {
	f.scheduledAt = at
	return f.jobID, nil
}

func (f *fakeOutboundEnqueuer) CancelTx(_ context.Context, _ pgx.Tx, jobID int64) error {
	f.cancelled = append(f.cancelled, jobID)
	return nil
}

// EnqueueBatchTx satisfies the widened OutboundEnqueuer interface. Batch
// tests use a dedicated fake below; single-send fake tests never invoke
// this method — a call would indicate a wiring mistake.
func (f *fakeOutboundEnqueuer) EnqueueBatchTx(_ context.Context, _ pgx.Tx, messageIDs []string) ([]int64, error) {
	ids := make([]int64, len(messageIDs))
	for i := range ids {
		ids[i] = f.jobID + int64(i)
	}
	return ids, nil
}

type txSentinelEnqueuer struct{}

func (txSentinelEnqueuer) EnqueueSendTx(ctx context.Context, tx pgx.Tx, messageID string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO task6_durable_jobs (message_id) VALUES ($1) RETURNING id`, messageID).Scan(&id)
	return id, err
}

func (s txSentinelEnqueuer) EnqueueScheduledSendTx(ctx context.Context, tx pgx.Tx, messageID string, _ time.Time) (int64, error) {
	return s.EnqueueSendTx(ctx, tx, messageID)
}

func (s txSentinelEnqueuer) EnqueueBatchTx(ctx context.Context, tx pgx.Tx, messageIDs []string) ([]int64, error) {
	ids := make([]int64, len(messageIDs))
	for i, mid := range messageIDs {
		id, err := s.EnqueueSendTx(ctx, tx, mid)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

func installTask6DurableJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS task6_durable_jobs; CREATE TABLE task6_durable_jobs (id bigserial PRIMARY KEY, message_id text NOT NULL UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS task6_durable_jobs`) })
}

type fakeNotifyEnqueuer struct{ called int }

func (f *fakeNotifyEnqueuer) EnqueueNotifyTx(_ context.Context, _ pgx.Tx, _ string) (int64, error) {
	f.called++
	return 123, nil
}

// fakeAsyncDeliverer is the SMTP submit the SendWorker calls — no network.
type fakeAsyncDeliverer struct{ out outboundsend.DeliverOutcome }

func (f fakeAsyncDeliverer) Deliver(_ context.Context, _ *outboundsend.SendJob) outboundsend.DeliverOutcome {
	return f.out
}

type countingAsyncDeliverer struct{ calls int }

func (d *countingAsyncDeliverer) Deliver(context.Context, *outboundsend.SendJob) outboundsend.DeliverOutcome {
	d.calls++
	return outboundsend.DeliverOutcome{ProviderMessageID: "unexpected"}
}

type timedAsyncDeliverer struct {
	out        outboundsend.DeliverOutcome
	returnedAt time.Time
}

func (d *timedAsyncDeliverer) Deliver(context.Context, *outboundsend.SendJob) outboundsend.DeliverOutcome {
	d.returnedAt = time.Now().UTC()
	return d.out
}

type legacyOnlyUsageTracker struct{}

func (legacyOnlyUsageTracker) RecordAndCheck(context.Context, string, string, string, string) (bool, error) {
	return true, nil
}

type blockingAsyncDeliverer struct {
	entered chan struct{}
	release chan struct{}
	out     outboundsend.DeliverOutcome
}

func (d *blockingAsyncDeliverer) Deliver(_ context.Context, _ *outboundsend.SendJob) outboundsend.DeliverOutcome {
	close(d.entered)
	<-d.release
	return d.out
}

func workerJob(id string, attempt int) *river.Job[outboundsend.OutboundSendArgs] {
	return workerJobWithID(id, 999, attempt)
}

func workerJobWithID(id string, jobID int64, attempt int) *river.Job[outboundsend.OutboundSendArgs] {
	return &river.Job[outboundsend.OutboundSendArgs]{
		JobRow: &rivertype.JobRow{ID: jobID, Attempt: attempt, MaxAttempts: outboundsend.MaxSendAttempts},
		Args:   outboundsend.OutboundSendArgs{MessageID: id},
	}
}

func setupAsyncAPI(t *testing.T) (*agent.API, *identity.Store, webhookpub.Outbox, *fakeOutboundEnqueuer) {
	t.Helper()
	api, store, outbox, enq, _ := setupAsyncAPIWithPool(t)
	return api, store, outbox, enq
}

func setupAsyncAPIWithPool(t *testing.T) (*agent.API, *identity.Store, webhookpub.Outbox, *fakeOutboundEnqueuer, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	sender.SetSendingStatusLookup(store)
	api := agent.NewAPI(store, sender, smtpRelay, nil, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	api.SetOutbox(outbox)
	enq := &fakeOutboundEnqueuer{jobID: 999}
	api.SetOutboundEnqueuer(enq)
	store.SetOutboundJobCanceller(enq)
	return api, store, outbox, enq, pool
}

func TestHoldForApprovalCore_SuppressedAgentCreatesHoldWithoutNotificationJob(t *testing.T) {
	api, store, _ := newReviewAPI(t)
	_, ag := selfAgent(t, store, "suppressedhold")
	ag.SuppressNotifications = true
	notify := &fakeNotifyEnqueuer{}
	api.SetNotifyEnqueuer(notify)

	msg, err := api.HoldForApprovalCore(context.Background(), ag, outbound.SendRequest{
		To: []string{"review-target@example.test"}, Subject: "held quietly", Body: "body",
	}, "send", "")
	if err != nil {
		t.Fatalf("HoldForApprovalCore: %v", err)
	}
	if msg == nil || msg.Status != identity.MessageStatusPendingReview {
		t.Fatalf("held message = %+v, want pending_review", msg)
	}
	if notify.called != 0 {
		t.Errorf("notification jobs enqueued = %d, want 0", notify.called)
	}
}

func TestHoldForApprovalCore_ReviewRequestedUsesExactPersistedHoldTransition(t *testing.T) {
	api, store, _, _, pool := setupAsyncAPIWithPool(t)
	_, ag := selfAgent(t, store, "reviewrequestedlifecycle")
	ag.HITLTTLSeconds = identity.HITLDefaultTTLSeconds

	msg, err := api.HoldForApprovalCore(context.Background(), ag, outbound.SendRequest{
		To: []string{"review-target@example.test"}, Subject: "held with lifecycle", Body: "body",
	}, "send", "")
	if err != nil {
		t.Fatalf("HoldForApprovalCore: %v", err)
	}
	assertReviewEventLifecycleMatchesRow(t, pool, msg.ID, webhookpub.EventEmailReviewRequested, messagelifecycle.ReasonReviewHoldCreated)
}

// TestDeliverOutbound_MissingQueueFailsClosed prevents a regression to the
// pre-GA submit-inline fallback. Queue wiring is mandatory: a miswired process
// must return an error before provider I/O, never send an email synchronously.
func TestDeliverOutbound_MissingQueueFailsClosed(t *testing.T) {
	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: smtpAddr.Host, Port: smtpAddr.Port})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	api := agent.NewAPI(store, sender, smtpRelay, nil, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	user, ag := selfAgent(t, store, "missingqueue")

	res, oerr := api.DeliverOutbound(context.Background(), user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "must queue", Body: "never submit inline",
	}, "send", "", nil, nil)
	if res != nil {
		t.Fatalf("result = %+v, want nil when outbound queue is unavailable", res)
	}
	if oerr == nil || oerr.Status != 500 || oerr.Code != "internal_error" {
		t.Fatalf("error = %+v, want 500 internal_error", oerr)
	}
	if got := smtpDone(); len(got) != 0 {
		t.Fatalf("missing queue submitted %d SMTP messages; want zero", len(got))
	}
}

// TestDeliverOutbound_AcceptTransactionRollsBackAsAUnit pins the pre-commit
// half of the response-loss boundary. If completing the idempotency record
// fails, the accepted message and stamped job must roll back with it.
func TestDeliverOutbound_AcceptTransactionLifecycleRollsBackAsAUnit(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "acceptrollback")
	callbackCalled := false
	var lifecycleBaseline int
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleBaseline)
	}); err != nil {
		t.Fatal(err)
	}

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "rollback boundary", Body: "never accepted",
	}, "send", "", nil, func(_ context.Context, _ pgx.Tx, result *agent.OutboundResult) error {
		callbackCalled = true
		if result.Status != "accepted" || !strings.HasPrefix(result.MessageID, "msg_") {
			t.Fatalf("accept idempotency result=%+v want accepted msg_ resource", result)
		}
		return errors.New("idempotency completion failed")
	})
	if !callbackCalled {
		t.Fatal("idempotency completion callback was not called")
	}
	if res != nil {
		t.Fatalf("result = %+v, want nil when accept transaction aborts", res)
	}
	if oerr == nil || oerr.Status != 500 || oerr.Code != "internal_error" {
		t.Fatalf("error = %+v, want 500 internal_error", oerr)
	}

	var messages int
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM messages WHERE agent_id=$1 AND subject=$2`,
			ag.ID, "rollback boundary",
		).Scan(&messages)
	}); err != nil {
		t.Fatalf("count rolled-back messages: %v", err)
	}
	if messages != 0 {
		t.Fatalf("accepted messages after aborted transaction = %d, want 0", messages)
	}
	var lifecycleAfter int
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleAfter)
	}); err != nil {
		t.Fatal(err)
	}
	if lifecycleAfter != lifecycleBaseline {
		t.Fatalf("lifecycle rows after abort before=%d after=%d", lifecycleBaseline, lifecycleAfter)
	}
}

func TestDeliverOutbound_QueueLifecycleFailureRollsBack(t *testing.T) {
	api, store, _, _, pool := setupAsyncAPIWithPool(t)
	installTask6DurableJobs(t, pool)
	api.SetOutboundEnqueuer(txSentinelEnqueuer{})
	ctx := context.Background()
	user, ag := selfAgent(t, store, "queuelifecyclerb")
	_, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_fail_queue_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_queue_lifecycle(); CREATE FUNCTION test_fail_queue_lifecycle() RETURNS trigger AS $f$ BEGIN IF NEW.reason_code='queue.outbound_submission' THEN RAISE EXCEPTION 'forced queue lifecycle failure'; END IF; RETURN NEW; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_queue_lifecycle BEFORE INSERT ON message_lifecycle_transitions FOR EACH ROW EXECUTE FUNCTION test_fail_queue_lifecycle();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_fail_queue_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_queue_lifecycle();`)
	})
	var lifecycleBaseline int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleBaseline); err != nil {
		t.Fatal(err)
	}
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{"a@example.net"}, Subject: "queue lifecycle rollback", Body: "body"}, "send", "", nil, nil)
	if res != nil || oerr == nil {
		t.Fatalf("result=%+v error=%+v want failure", res, oerr)
	}
	var messages, jobsCount, lifecycleAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE agent_id=$1 AND subject='queue lifecycle rollback'`, ag.ID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("message survived queue lifecycle failure: %d", messages)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task6_durable_jobs`).Scan(&jobsCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleAfter); err != nil {
		t.Fatal(err)
	}
	if jobsCount != 0 || lifecycleAfter != lifecycleBaseline {
		t.Fatalf("partial accept jobs=%d lifecycle before=%d after=%d", jobsCount, lifecycleBaseline, lifecycleAfter)
	}
}

// TestDeliverOutbound_ScheduledAccept pins the scheduled-send accept path
// (migration 084): a future ScheduledAt is accepted as status=scheduled, threads
// the instant into the River enqueue (EnqueueScheduledSendTx), persists
// scheduled_at on the row, and — deliberately — leaves delivery_status='accepted'
// (no new status; a future-scheduled River job is invisible to the reconciler).
func TestDeliverOutbound_ScheduledAccept(t *testing.T) {
	api, store, _, enq := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "scheduledaccept")
	at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "later", Body: "scheduled body",
		ScheduledAt: &at,
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	if res == nil || res.Status != "scheduled" || res.ScheduledAt == nil || !res.ScheduledAt.Equal(at) {
		t.Fatalf("result = %+v, want status=scheduled with ScheduledAt=%v", res, at)
	}
	// The scheduled instant was threaded to the River enqueue (not the immediate path).
	if !enq.scheduledAt.Equal(at) {
		t.Fatalf("enqueued ScheduledAt = %v, want %v", enq.scheduledAt, at)
	}
	// The row persists scheduled_at and stays delivery_status='accepted'.
	m, err := store.GetMessageWithContent(ctx, res.MessageID, ag.ID)
	if err != nil {
		t.Fatalf("GetMessageWithContent: %v", err)
	}
	if m.DeliveryStatus != "accepted" {
		t.Errorf("delivery_status = %q, want accepted (scheduled rows stay accepted)", m.DeliveryStatus)
	}
	if m.ScheduledAt == nil || !m.ScheduledAt.Equal(at) {
		t.Errorf("stored scheduled_at = %v, want %v", m.ScheduledAt, at)
	}
}

// A scheduled send that is over the monthly cap at FIRE time is refused
// terminally (delivery_status=failed) and never submitted — closing the
// "schedule into a future month to bypass quota" gap. Immediate sends are gated
// at accept and are unaffected (the quota gate here only runs for scheduled_at
// rows). Review item 2.
func TestSendWorker_ScheduledSendOverMonthlyQuotaRefusedAtFire(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	installTask6DurableJobs(t, pool)
	api.SetOutboundEnqueuer(txSentinelEnqueuer{})
	ctx := context.Background()
	user, ag := selfAgent(t, store, "schedquota")
	at := time.Now().Add(48 * time.Hour).UTC()

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "later", Body: "b", ScheduledAt: &at,
	}, "send", "", nil, nil)
	if oerr != nil || res == nil || res.Status != "scheduled" {
		t.Fatalf("scheduled accept: res=%+v err=%+v", res, oerr)
	}
	var sendJobID *int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&sendJobID); err != nil || sendJobID == nil {
		t.Fatalf("send_job_id: %v", err)
	}

	adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
	adapter.SetScheduledSendQuota(func(context.Context, string) (bool, error) { return true, nil }) // over cap at fire
	deliverer := &countingAsyncDeliverer{}
	if err := outboundsend.NewSendWorker(adapter, deliverer).Work(ctx, workerJobWithID(res.MessageID, *sendJobID, 1)); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if deliverer.calls != 0 {
		t.Errorf("over-quota scheduled send must NOT be submitted; deliverer calls=%d", deliverer.calls)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM messages WHERE id=$1`, res.MessageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("delivery_status = %q, want failed (refused at fire for quota)", status)
	}
}

// Self-send delivers immediately in-process (the scheduling path never runs), so
// a future send_at on a self-send is rejected rather than silently delivered
// early. Review item 3.
func TestDeliverOutbound_SelfSendRejectsScheduledAt(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "selfsched")
	at := time.Now().Add(24 * time.Hour).UTC()

	_, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{ag.EmailAddress()}, Subject: "self later", Body: "b", ScheduledAt: &at,
	}, "send", "", nil, nil)
	if oerr == nil || oerr.Status != 400 || oerr.Code != "invalid_request" {
		t.Fatalf("self-send with send_at: want 400 invalid_request, got %+v", oerr)
	}
}

// The conversation view is the third projection of the summary contract (after
// list + detail); it must also carry scheduled_at so a scheduled send is
// distinguishable there. Review item 4.
func TestGetConversationByID_CarriesScheduledAt(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "convsched")
	at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "later", Body: "b",
		ConversationID: "conv_sched_1", ScheduledAt: &at,
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	conv, err := store.GetConversationByID(ctx, ag.ID, "conv_sched_1")
	if err != nil {
		t.Fatalf("GetConversationByID: %v", err)
	}
	var found *identity.Message
	for i := range conv.Messages {
		if conv.Messages[i].ID == res.MessageID {
			found = &conv.Messages[i]
		}
	}
	if found == nil {
		t.Fatal("scheduled message not present in conversation view")
	}
	if found.ScheduledAt == nil || !found.ScheduledAt.Equal(at) {
		t.Errorf("conversation view scheduled_at = %v, want %v", found.ScheduledAt, at)
	}
}

// TestDeliverOutbound_AsyncAccept is the end-to-end slice-C check: with the async
// enqueuer wired, a real (non-self) send is durably accepted (not submitted
// inline) — it returns status=accepted, persists a delivery_status='accepted' row
// with the send_job_id stamped, and does NOT yet emit email.sent. Running the real
// SendWorker over the store adapter + a fake deliverer then flips the row to sent,
// records the provider id, and emits email.sent.
func TestDeliverOutbound_AsyncAcceptLifecycle(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	installTask6DurableJobs(t, pool)
	api.SetOutboundEnqueuer(txSentinelEnqueuer{})
	ctx := context.Background()
	user, ag := selfAgent(t, store, "asyncacc")

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@gmail.com"}, Subject: "async hi", Body: "queued not sent",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	if res.Status != "accepted" {
		t.Fatalf("Status = %q, want accepted", res.Status)
	}
	if res.MessageID == "" || res.Method != "smtp" {
		t.Fatalf("res = %+v, want a msg id + method=smtp", res)
	}

	// Row is accepted, job stamped, not yet sent, no provider id.
	var (
		deliveryStatus, providerID string
		sendJobID                  *int64
		raw                        []byte
	)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT delivery_status, COALESCE(provider_message_id,''), send_job_id, raw_message FROM messages WHERE id=$1`,
			res.MessageID,
		).Scan(&deliveryStatus, &providerID, &sendJobID, &raw)
	}); err != nil {
		t.Fatalf("read accepted row: %v", err)
	}
	if deliveryStatus != "accepted" {
		t.Errorf("delivery_status = %q, want accepted", deliveryStatus)
	}
	if providerID != "" {
		t.Errorf("provider_message_id = %q, want empty at accept", providerID)
	}
	if sendJobID == nil {
		t.Fatal("send_job_id is nil")
	}
	var durableMessageID string
	if err := pool.QueryRow(ctx, `SELECT message_id FROM task6_durable_jobs WHERE id=$1`, *sendJobID).Scan(&durableMessageID); err != nil {
		t.Fatalf("durable job: %v", err)
	}
	if durableMessageID != res.MessageID {
		t.Fatalf("durable job message=%s want %s", durableMessageID, res.MessageID)
	}
	if len(raw) == 0 {
		t.Errorf("raw_message empty; the composed bytes must be persisted for the worker")
	}
	// No email.sent yet (the message isn't sent).
	if n := countEvents(t, store, ag.UserID, webhookpub.EventEmailSent); n != 0 {
		t.Errorf("email.sent events at accept = %d, want 0", n)
	}
	var acceptance, queued int
	var lifecycleJobID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE reason_code='acceptance.outbound_api'), count(*) FILTER (WHERE reason_code='queue.outbound_submission'), COALESCE(max(correlation_ids->>'job_id') FILTER (WHERE reason_code='queue.outbound_submission'),'') FROM message_lifecycle_transitions WHERE message_id=$1`, res.MessageID).Scan(&acceptance, &queued, &lifecycleJobID)
	}); err != nil {
		t.Fatal(err)
	}
	if acceptance != 1 || queued != 1 || lifecycleJobID != fmt.Sprint(*sendJobID) {
		t.Fatalf("accept lifecycle acceptance=%d queued=%d job=%q", acceptance, queued, lifecycleJobID)
	}

	// Run the real SendWorker over the production store adapter + a fake submit.
	adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
	worker := outboundsend.NewSendWorker(adapter, fakeAsyncDeliverer{
		out: outboundsend.DeliverOutcome{ProviderMessageID: "<ses-async-1@amazonses.com>", SentAs: "relay"},
	})
	if err := worker.Work(ctx, workerJobWithID(res.MessageID, *sendJobID, 1)); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT delivery_status, COALESCE(provider_message_id,'') FROM messages WHERE id=$1`,
			res.MessageID,
		).Scan(&deliveryStatus, &providerID)
	}); err != nil {
		t.Fatalf("read sent row: %v", err)
	}
	if deliveryStatus != "sent" {
		t.Errorf("post-worker delivery_status = %q, want sent", deliveryStatus)
	}
	if providerID != "<ses-async-1@amazonses.com>" {
		t.Errorf("provider_message_id = %q, want the SES id from the worker", providerID)
	}
	if n := countEvents(t, store, ag.UserID, webhookpub.EventEmailSent); n != 1 {
		t.Errorf("email.sent events after worker = %d, want 1", n)
	}

	// Re-running the worker is a no-op (delivery_status past accepted/sending).
	if err := worker.Work(ctx, workerJobWithID(res.MessageID, *sendJobID, 2)); err != nil {
		t.Fatalf("worker.Work re-drive: %v", err)
	}
	if n := countEvents(t, store, ag.UserID, webhookpub.EventEmailSent); n != 1 {
		t.Errorf("email.sent events after re-drive = %d, want 1 (idempotent)", n)
	}
}

func TestSendWorker_UpstreamAcceptedLifecycleEventParity(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "submissionsuccess")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{"alice@example.net"}, Subject: "submission accepted", Body: "body"}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	worker := outboundsend.NewSendWorker(agent.NewOutboundSendStore(store, outbox, usage.NewUsageTracker(usage.NewStore(pool))), fakeAsyncDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "0100019283abcdef-1a2b3c4d-0000", SentAs: "relay"}})
	if err := worker.Work(ctx, workerJobWithID(res.MessageID, jobID, 2)); err != nil {
		t.Fatal(err)
	}

	rows := lifecycleRows(t, pool, res.MessageID)
	var submission []messagelifecycle.MessageLifecycleTransition
	for _, tr := range rows {
		if tr.Stage == messagelifecycle.StageSubmission {
			submission = append(submission, tr)
		}
	}
	var usageEvents, summary int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE user_id=$1`, user.ID).Scan(&usageEvents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(outbound_count),0) FROM usage_summaries WHERE user_id=$1`, user.ID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if usageEvents != 1 || summary != 1 {
		t.Fatalf("submission metering events=%d summary=%d", usageEvents, summary)
	}
	if len(submission) != 1 || submission[0].ReasonCode != messagelifecycle.ReasonSubmissionUpstreamAccepted {
		t.Fatalf("submission lifecycle = %+v, want one upstream acceptance", submission)
	}
	if submission[0].CorrelationIDs["job_id"] != fmt.Sprint(jobID) || submission[0].CorrelationIDs["provider_message_id"] != "0100019283abcdef-1a2b3c4d-0000" {
		t.Fatalf("correlations = %#v", submission[0].CorrelationIDs)
	}
	if len(eventLifecycle(t, pool, res.MessageID, webhookpub.EventEmailSent)) != 1 || eventLifecycle(t, pool, res.MessageID, webhookpub.EventEmailSent)[0].ID != submission[0].ID {
		t.Fatal("email.sent must embed only the exact stored submission transition")
	}
	goldenassert.Lifecycle(t, "../eventpayload/testdata/email.sent.json", eventLifecycle(t, pool, res.MessageID, webhookpub.EventEmailSent))
	for _, tr := range rows {
		if tr.Stage == messagelifecycle.StageDelivery {
			t.Fatalf("provider acceptance overstated as delivery: %+v", tr)
		}
	}

	// Same logical job/attempt is a no-op and must preserve the stored envelope.
	var before []byte
	if err := pool.QueryRow(ctx, `SELECT envelope FROM webhook_events WHERE message_id=$1 AND type='email.sent'`, res.MessageID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(ctx, workerJobWithID(res.MessageID, jobID, 2)); err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err := pool.QueryRow(ctx, `SELECT envelope FROM webhook_events WHERE message_id=$1 AND type='email.sent'`, res.MessageID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("duplicate execution rewrote stored envelope")
	}
}

func TestSendWorker_RejectsLegacyUsageTrackerBeforeClaimOrProviderIO(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "legacyusage")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{"legacy@example.net"}, Subject: "legacy usage", Body: "body"}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	d := &countingAsyncDeliverer{}
	w := outboundsend.NewSendWorker(agent.NewOutboundSendStore(store, outbox, legacyOnlyUsageTracker{}), d)
	if err := w.Work(ctx, workerJob(res.MessageID, 1)); err == nil {
		t.Fatal("legacy usage tracker should fail configuration")
	}
	if d.calls != 0 {
		t.Fatalf("provider calls=%d", d.calls)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM messages WHERE id=$1`, res.MessageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" {
		t.Fatalf("message status=%q want accepted", status)
	}
}

func TestSendWorker_TemporaryAndProviderRejectedLifecycle(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "submissionfailures")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{"bob@example.net"}, Subject: "submission failures", Body: "body"}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	retry := outboundsend.NewSendWorker(agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()), fakeAsyncDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New(strings.Repeat("remote diagnostic ", 300))}})
	if err := retry.Work(ctx, workerJobWithID(res.MessageID, jobID, 1)); err == nil {
		t.Fatal("temporary failure must retry")
	}
	if err := retry.Work(ctx, workerJobWithID(res.MessageID, jobID, 2)); err == nil {
		t.Fatal("second temporary failure must retry")
	}
	rows := lifecycleRows(t, pool, res.MessageID)
	var temporary []messagelifecycle.MessageLifecycleTransition
	for _, tr := range rows {
		if tr.ReasonCode == messagelifecycle.ReasonSubmissionTemporaryFailure {
			temporary = append(temporary, tr)
		}
	}
	if len(temporary) != 2 {
		t.Fatalf("temporary transitions=%d want 2: %+v", len(temporary), temporary)
	}
	var dedupeKeys []string
	keyRows, err := pool.Query(ctx, `SELECT dedupe_key FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.temporary_failure' ORDER BY dedupe_key`, res.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	for keyRows.Next() {
		var key string
		if err := keyRows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		dedupeKeys = append(dedupeKeys, key)
	}
	keyRows.Close()
	wantKeys := []string{fmt.Sprintf("submission:job:%d:attempt:1:submission.temporary_failure", jobID), fmt.Sprintf("submission:job:%d:attempt:2:submission.temporary_failure", jobID)}
	if fmt.Sprint(dedupeKeys) != fmt.Sprint(wantKeys) {
		t.Fatalf("dedupe keys=%v want %v", dedupeKeys, wantKeys)
	}
	if temporary[0].Evidence["failure_reason"] == strings.Repeat("remote diagnostic ", 300) {
		t.Fatal("unbounded remote SMTP diagnostic stored")
	}

	timedReject := &timedAsyncDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("550 rejected"), Permanent: true}}
	reject := outboundsend.NewSendWorker(agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()), timedReject)
	rejectJob := workerJobWithID(res.MessageID, jobID, 3)
	ancient := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	rejectJob.AttemptedAt = &ancient
	if err := reject.Work(ctx, rejectJob); err == nil {
		t.Fatal("provider rejection must cancel")
	}
	rows = lifecycleRows(t, pool, res.MessageID)
	var terminal *messagelifecycle.MessageLifecycleTransition
	for i := range rows {
		if rows[i].ReasonCode == messagelifecycle.ReasonSubmissionProviderRejected {
			terminal = &rows[i]
		}
	}
	if terminal == nil {
		t.Fatalf("missing provider rejection lifecycle: %+v", rows)
	}
	if terminal.OccurredAt.Before(timedReject.returnedAt.Truncate(time.Microsecond)) {
		t.Fatalf("provider rejection occurred_at=%s before outcome returned=%s", terminal.OccurredAt, timedReject.returnedAt)
	}
	event := eventLifecycle(t, pool, res.MessageID, webhookpub.EventEmailFailed)
	if len(event) != 1 || event[0].ID != terminal.ID {
		t.Fatalf("email.failed lifecycle=%+v terminal=%+v", event, terminal)
	}
	var reasonCode string
	var retryable bool
	if err := pool.QueryRow(ctx, `SELECT envelope->'data'->>'reason_code',(envelope->'data'->>'retryable')::boolean FROM webhook_events WHERE message_id=$1 AND type='email.failed'`, res.MessageID).Scan(&reasonCode, &retryable); err != nil {
		t.Fatal(err)
	}
	if reasonCode != string(terminal.ReasonCode) || retryable != terminal.Retryable {
		t.Fatalf("email.failed classification=%q/%v transition=%q/%v", reasonCode, retryable, terminal.ReasonCode, terminal.Retryable)
	}
}

func TestSendWorker_DuplicateAttemptLifecycleReusesLogicalTransition(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "submissionduplicate")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{"dup@example.net"}, Subject: "duplicate attempt", Body: "body"}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimOutboundForSend(ctx, res.MessageID, jobID); err != nil || claimed == nil {
		t.Fatalf("claim=(%+v,%v)", claimed, err)
	}
	adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
	observedAt := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := adapter.DeferTerminalFailure(ctx, res.MessageID, jobID, 6, observedAt.Add(time.Duration(i)*time.Minute), fmt.Sprintf("451 retry later probe %d", i)); err != nil {
			t.Fatalf("defer %d: %v", i, err)
		}
	}
	var count int
	var key string
	if err := pool.QueryRow(ctx, `SELECT count(*),max(dedupe_key) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.temporary_failure'`, res.MessageID).Scan(&count, &key); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("submission:job:%d:attempt:6:submission.temporary_failure", jobID)
	if count != 1 || key != want {
		t.Fatalf("duplicate lifecycle count=%d key=%q want 1/%q", count, key, want)
	}
}

func TestSendWorker_ProviderOutageSnoozeRedriveLifecycleIsFirstWins(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "outagesnooze")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{"outage@example.net"}, Subject: "outage", Body: "body"}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	w := outboundsend.NewSendWorker(agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()), fakeAsyncDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("dial provider unavailable"), Outage: true}})
	base := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		j := workerJobWithID(res.MessageID, jobID, 1)
		at := base.Add(time.Duration(i) * time.Minute)
		j.AttemptedAt = &at
		if err := w.Work(ctx, j); err == nil {
			t.Fatal("outage must snooze")
		}
	}
	j := workerJobWithID(res.MessageID, jobID, 2)
	at := base.Add(2 * time.Minute)
	j.AttemptedAt = &at
	if err := w.Work(ctx, j); err == nil {
		t.Fatal("next outage attempt must snooze")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.temporary_failure'`, res.MessageID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("outage temporary rows=%d want one for attempt 1 plus one for attempt 2", count)
	}
}

func TestSendWorker_ProviderEvidenceRepairUsesDurableLifecycleAttribution(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "evidencerepair")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{"repair@example.net"}, Subject: "evidence repair", Body: "body"}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordProviderAcceptEvidence(ctx, res.MessageID, "provider-repair"); err != nil {
		t.Fatal(err)
	}
	var acceptedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT provider_accepted_at FROM messages WHERE id=$1`, res.MessageID).Scan(&acceptedAt); err != nil {
		t.Fatal(err)
	}
	deliverer := &countingAsyncDeliverer{}
	worker := outboundsend.NewSendWorker(agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()), deliverer)
	if err := worker.Work(ctx, workerJobWithID(res.MessageID, jobID, 5)); err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 0 {
		t.Fatal("provider evidence repair resubmitted")
	}
	var occurred time.Time
	var dedupe string
	if err := pool.QueryRow(ctx, `SELECT occurred_at,dedupe_key FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.upstream_accepted'`, res.MessageID).Scan(&occurred, &dedupe); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("submission:job:%d:attempt:0:submission.upstream_accepted", jobID)
	if !occurred.Equal(acceptedAt) || dedupe != want {
		t.Fatalf("repair occurred=%s/%s key=%q want %q", occurred, acceptedAt, dedupe, want)
	}
}

func TestSendWorker_TrashLinearizesWithDurableClaim(t *testing.T) {
	api, store, outbox, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "asynctrashrace")
	trashPool, err := pgxpool.New(ctx, testutil.TestDBURL())
	if err != nil {
		t.Fatalf("open independent trash pool: %v", err)
	}
	t.Cleanup(trashPool.Close)
	if err := trashPool.Ping(ctx); err != nil {
		t.Fatalf("warm independent trash pool: %v", err)
	}
	trashStore := identity.NewStore(trashPool)

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"race@gmail.com"}, Subject: "trash race", Body: "x",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}

	deliverer := &blockingAsyncDeliverer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		out: outboundsend.DeliverOutcome{
			ProviderMessageID: "<ses-trash-race@amazonses.com>",
			SentAs:            "relay",
		},
	}
	worker := outboundsend.NewSendWorker(
		agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()),
		deliverer,
	)
	workDone := make(chan error, 1)
	go func() { workDone <- worker.Work(ctx, workerJob(res.MessageID, 1)) }()
	<-deliverer.entered

	duplicateDeliverer := &blockingAsyncDeliverer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		out:     outboundsend.DeliverOutcome{ProviderMessageID: "duplicate"},
	}
	duplicateWorker := outboundsend.NewSendWorker(
		agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()),
		duplicateDeliverer,
	)
	duplicateDone := make(chan error, 1)
	go func() { duplicateDone <- duplicateWorker.Work(ctx, workerJobWithID(res.MessageID, 1000, 1)) }()
	select {
	case err := <-duplicateDone:
		if err != nil {
			t.Fatalf("duplicate worker: %v", err)
		}
	case <-duplicateDeliverer.entered:
		close(duplicateDeliverer.release)
		<-duplicateDone
		close(deliverer.release)
		<-workDone
		t.Fatal("a different River job ID submitted an already-claimed message")
	case <-time.After(200 * time.Millisecond):
		close(deliverer.release)
		<-workDone
		t.Fatal("duplicate worker did not resolve the foreign claim promptly")
	}

	trashDone := make(chan error, 1)
	go func() { trashDone <- trashStore.SoftDeleteMessage(ctx, res.MessageID, ag.ID) }()
	select {
	case err := <-trashDone:
		if !errors.Is(err, identity.ErrSendInProgress) {
			close(deliverer.release)
			<-workDone
			t.Fatalf("SoftDeleteMessage during provider call = %v, want ErrSendInProgress", err)
		}
	case <-time.After(200 * time.Millisecond):
		close(deliverer.release)
		<-workDone
		t.Fatal("SoftDeleteMessage blocked after the send was durably claimed")
	}
	if err := trashStore.PurgeMessage(ctx, res.MessageID, ag.ID); !errors.Is(err, identity.ErrSendInProgress) {
		close(deliverer.release)
		<-workDone
		t.Fatalf("PurgeMessage during provider call = %v, want ErrSendInProgress", err)
	}

	close(deliverer.release)
	if err := <-workDone; err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if err := trashStore.SoftDeleteMessage(ctx, res.MessageID, ag.ID); err != nil {
		t.Fatalf("SoftDeleteMessage after provider call: %v", err)
	}

	var deliveryStatus, providerID string
	var deleted bool
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT delivery_status, COALESCE(provider_message_id,''), deleted_at IS NOT NULL
			   FROM messages WHERE id=$1`, res.MessageID,
		).Scan(&deliveryStatus, &providerID, &deleted)
	}); err != nil {
		t.Fatalf("read trashed row: %v", err)
	}
	if deliveryStatus != "sent" || providerID != "<ses-trash-race@amazonses.com>" || !deleted {
		t.Fatalf("post-race row = status %q, provider %q, deleted %v; want sent/provider/deleted", deliveryStatus, providerID, deleted)
	}

	res, oerr = api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"agent-race@gmail.com"}, Subject: "agent trash race", Body: "x",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound(agent race): %+v", oerr)
	}
	deliverer = &blockingAsyncDeliverer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		out: outboundsend.DeliverOutcome{
			Err:       errors.New("550 rejected"),
			Permanent: true,
		},
	}
	worker = outboundsend.NewSendWorker(
		agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()),
		deliverer,
	)
	workDone = make(chan error, 1)
	go func() { workDone <- worker.Work(ctx, workerJob(res.MessageID, 1)) }()
	<-deliverer.entered

	trashDone = make(chan error, 1)
	go func() { trashDone <- trashStore.SoftDeleteAgent(ctx, ag.ID, user.ID) }()
	select {
	case err := <-trashDone:
		if err != nil {
			close(deliverer.release)
			<-workDone
			t.Fatalf("SoftDeleteAgent: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		close(deliverer.release)
		<-workDone
		t.Fatal("SoftDeleteAgent blocked after the send was durably claimed")
	}
	if _, err := trashStore.DeleteAgent(ctx, ag.ID, user.ID); !errors.Is(err, identity.ErrSendInProgress) {
		close(deliverer.release)
		<-workDone
		t.Fatalf("DeleteAgent during provider call = %v, want ErrSendInProgress", err)
	}

	close(deliverer.release)
	if err := <-workDone; err == nil {
		t.Fatal("worker.Work(agent race) succeeded after a permanent provider failure")
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT m.delivery_status, COALESCE(m.provider_message_id,''), a.deleted_at IS NOT NULL
			   FROM messages m JOIN agent_identities a ON a.id = m.agent_id
			  WHERE m.id=$1`, res.MessageID,
		).Scan(&deliveryStatus, &providerID, &deleted)
	}); err != nil {
		t.Fatalf("read agent-trashed row: %v", err)
	}
	if deliveryStatus != "failed" || providerID != "" || !deleted {
		t.Fatalf("post-agent-race row = status %q, provider %q, agent deleted %v; want failed/empty/deleted", deliveryStatus, providerID, deleted)
	}
}

func TestSendWorker_TrashBeforeClaimCancelsWithoutSubmit(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "asynctrashfirst")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"never@gmail.com"}, Subject: "trash first", Body: "x",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	if err := store.SoftDeleteMessage(ctx, res.MessageID, ag.ID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}

	deliverer := &blockingAsyncDeliverer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		out:     outboundsend.DeliverOutcome{ProviderMessageID: "must-not-send"},
	}
	worker := outboundsend.NewSendWorker(
		agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()),
		deliverer,
	)
	if err := worker.Work(ctx, workerJob(res.MessageID, 1)); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	select {
	case <-deliverer.entered:
		close(deliverer.release)
		t.Fatal("provider called after trash won before the send claim")
	default:
	}

	var deliveryStatus, detail string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT delivery_status, COALESCE(delivery_detail,'') FROM messages WHERE id=$1`,
			res.MessageID,
		).Scan(&deliveryStatus, &detail)
	}); err != nil {
		t.Fatalf("read canceled row: %v", err)
	}
	if deliveryStatus != "failed" || detail == "" {
		t.Fatalf("canceled row = status %q detail %q, want failed with detail", deliveryStatus, detail)
	}
	rows := lifecycleRows(t, pool, res.MessageID)
	var cancelled int
	for _, tr := range rows {
		if tr.ReasonCode == messagelifecycle.ReasonSubmissionCancelled {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Fatalf("trash-before-claim cancellation lifecycle=%d", cancelled)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(delivery_failure_reason_code,'') FROM messages WHERE id=$1`, res.MessageID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "submission.cancelled" {
		t.Fatalf("trash reason=%q", reason)
	}
}

func TestSendWorker_TrashCancellationLifecycleFailureRollsBackState(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "trashlifecyclerb")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{"never@example.net"}, Subject: "trash lifecycle rollback", Body: "body"}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	if err := store.SoftDeleteMessage(ctx, res.MessageID, ag.ID); err != nil {
		t.Fatal(err)
	}
	install := `CREATE FUNCTION test_fail_trash_cancel_lifecycle() RETURNS trigger AS $f$ BEGIN IF NEW.reason_code='submission.cancelled' THEN RAISE EXCEPTION 'forced trash lifecycle failure'; END IF; RETURN NEW; END;$f$ LANGUAGE plpgsql;CREATE TRIGGER test_fail_trash_cancel_lifecycle BEFORE INSERT ON message_lifecycle_transitions FOR EACH ROW EXECUTE FUNCTION test_fail_trash_cancel_lifecycle();`
	uninstall := `DROP TRIGGER IF EXISTS test_fail_trash_cancel_lifecycle ON message_lifecycle_transitions;DROP FUNCTION IF EXISTS test_fail_trash_cancel_lifecycle();`
	if _, err := pool.Exec(ctx, install); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), uninstall) })
	d := &countingAsyncDeliverer{}
	w := outboundsend.NewSendWorker(agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()), d)
	if err := w.Work(ctx, workerJob(res.MessageID, 1)); err == nil {
		t.Fatal("forced lifecycle failure succeeded")
	}
	var status, reason string
	if err := pool.QueryRow(ctx, `SELECT delivery_status,COALESCE(delivery_failure_reason_code,'') FROM messages WHERE id=$1`, res.MessageID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" || reason != "" || d.calls != 0 {
		t.Fatalf("partial trash cancellation status=%q reason=%q calls=%d", status, reason, d.calls)
	}
	if _, err := pool.Exec(ctx, uninstall); err != nil {
		t.Fatal(err)
	}
	if err := w.Work(ctx, workerJob(res.MessageID, 1)); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT delivery_status,COALESCE(delivery_failure_reason_code,'') FROM messages WHERE id=$1`, res.MessageID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "submission.cancelled" {
		t.Fatalf("recovered trash cancellation status=%q reason=%q", status, reason)
	}
}

func TestSendWorker_RetryBackoffReleasesClaimForPurge(t *testing.T) {
	api, store, outbox, enq := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "asyncreleasepurge")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"retry@gmail.com"}, Subject: "retry then trash", Body: "x",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	worker := outboundsend.NewSendWorker(
		agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()),
		fakeAsyncDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("421 retry later")}},
	)
	if err := worker.Work(ctx, workerJob(res.MessageID, 1)); err == nil {
		t.Fatal("worker.Work succeeded after retryable provider failure")
	}
	if err := store.SoftDeleteMessage(ctx, res.MessageID, ag.ID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	if err := store.PurgeMessage(ctx, res.MessageID, ag.ID); err != nil {
		t.Fatalf("PurgeMessage after released retry claim: %v", err)
	}
	if len(enq.cancelled) != 1 || enq.cancelled[0] != 999 {
		t.Fatalf("cancelled jobs = %v, want [999]", enq.cancelled)
	}
}

func TestPurgeMessage_AllowsStaleOrphanedSendClaim(t *testing.T) {
	api, store, _, enq := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "asyncstalepurge")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"stale@gmail.com"}, Subject: "stale claim", Body: "x",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	if payload, err := store.ClaimOutboundForSend(ctx, res.MessageID, 999); err != nil || payload == nil {
		t.Fatalf("ClaimOutboundForSend = (%v, %v), want payload", payload, err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE messages SET send_claimed_at = now() - make_interval(secs => $2) WHERE id=$1`,
			res.MessageID, int64(identity.OutboundSendClaimStaleWindow/time.Second)+1)
		return err
	}); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}
	if err := store.SoftDeleteMessage(ctx, res.MessageID, ag.ID); err != nil {
		t.Fatalf("SoftDeleteMessage(stale claim): %v", err)
	}
	if err := store.PurgeMessage(ctx, res.MessageID, ag.ID); err != nil {
		t.Fatalf("PurgeMessage(stale claim): %v", err)
	}
	if len(enq.cancelled) != 1 || enq.cancelled[0] != 999 {
		t.Fatalf("cancelled jobs = %v, want [999]", enq.cancelled)
	}
}

func TestDeleteAgent_AllowsStaleOrphanedSendClaim(t *testing.T) {
	api, store, _, enq := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "asyncstaleagent")
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"stale-agent@gmail.com"}, Subject: "stale agent claim", Body: "x",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	if payload, err := store.ClaimOutboundForSend(ctx, res.MessageID, 999); err != nil || payload == nil {
		t.Fatalf("ClaimOutboundForSend = (%v, %v), want payload", payload, err)
	}
	if err := store.SoftDeleteAgent(ctx, ag.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE messages SET send_claimed_at = now() - make_interval(secs => $2) WHERE id=$1`,
			res.MessageID, int64(identity.OutboundSendClaimStaleWindow/time.Second)+1)
		return err
	}); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}
	if _, err := store.DeleteAgent(ctx, ag.ID, user.ID); err != nil {
		t.Fatalf("DeleteAgent(stale claim): %v", err)
	}
	if len(enq.cancelled) != 1 || enq.cancelled[0] != 999 {
		t.Fatalf("cancelled jobs = %v, want [999]", enq.cancelled)
	}
}

// TestOutboundSendStore_MarkFailed pins the failure path: the adapter flips the
// row to failed and emits email.failed.
func TestOutboundSendStore_MarkFailed(t *testing.T) {
	api, store, outbox, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "asyncfail")

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"bob@gmail.com"}, Subject: "will fail", Body: "x",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, res.MessageID)
		return err
	}); err != nil {
		t.Fatalf("claim row: %v", err)
	}

	adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
	occurredAt := time.Now().UTC()
	settled, settledAt, err := adapter.MarkFailed(ctx, res.MessageID, 999, 6, occurredAt, "550 mailbox unavailable", delivery.FailureSourceProvider, messagelifecycle.ReasonSubmissionProviderRejected, nil)
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if settled != delivery.StatusFailed {
		t.Fatalf("MarkFailed settled = %q, want %q (no provider evidence present)", settled, delivery.StatusFailed)
	}
	if !settledAt.Equal(occurredAt) {
		t.Errorf("MarkFailed effective occurred_at = %s, want the caller's %s on the failure path", settledAt, occurredAt)
	}

	var deliveryStatus, detail string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT delivery_status, COALESCE(delivery_detail,'') FROM messages WHERE id=$1`,
			res.MessageID,
		).Scan(&deliveryStatus, &detail)
	}); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if deliveryStatus != "failed" {
		t.Errorf("delivery_status = %q, want failed", deliveryStatus)
	}
	if detail != "550 mailbox unavailable" {
		t.Errorf("delivery_detail = %q, want the failure detail", detail)
	}
	if n := countEvents(t, store, ag.UserID, webhookpub.EventEmailFailed); n != 1 {
		t.Errorf("email.failed events = %d, want 1", n)
	}
}

func countEvents(t *testing.T, store *identity.Store, userID, eventType string) int {
	t.Helper()
	var n int
	if err := store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM webhook_events WHERE user_id=$1 AND type=$2`, userID, eventType,
		).Scan(&n)
	}); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// recordingSendMetrics captures the terminal SLI emissions of a store-backed
// send (the outboundsend package has its own recorder for fake-store tests).
type recordingSendMetrics struct {
	terminals []string
	latencies []float64
}

func (r *recordingSendMetrics) OutboundQueueWait(float64) {}
func (r *recordingSendMetrics) OutboundTerminal(outcome string) {
	r.terminals = append(r.terminals, outcome)
}
func (r *recordingSendMetrics) OutboundTerminalLatency(seconds float64) {
	r.latencies = append(r.latencies, seconds)
}
func (r *recordingSendMetrics) OutboundAttempt(string, float64) {}
func (r *recordingSendMetrics) OutboundRateDeferred()           {}

// The acceptance→terminal SLI anchors a HITL hold at its approve time and a
// scheduled send at its fire time, but the worker only ever sees those gates if
// ClaimSend carries them out of the store payload — the messages row →
// OutboundSendPayload → SendJob seam that lives in this package. Every
// worker-level latency test builds SendJob by hand against a fake store, so
// this store-backed pass is the only thing standing between a dropped field at
// that seam and a silent return of the reviewer's dwell to e2a's error budget.
func TestSendWorker_ClaimCarriesSubmissionGatesIntoTerminalLatency(t *testing.T) {
	for _, tc := range []struct {
		name  string
		label string
		held  bool // gate the row on reviewed_at (a HITL hold) vs scheduled_at
	}{
		{"hitl hold anchors at approve", "gatehold", true},
		{"scheduled send anchors at fire time", "gatesched", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
			ctx := context.Background()
			user, ag := selfAgent(t, store, tc.label)
			res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
				To: []string{"gated@external.test"}, Subject: "gated", Body: "b",
			}, "send", "", nil, nil)
			if oerr != nil || res == nil {
				t.Fatalf("DeliverOutbound: res=%+v err=%+v", res, oerr)
			}
			// Drafted two hours ago, released into the send pipeline 30 seconds
			// ago. Anchored at the gate the sample is ~30s; anchored at
			// created_at it is ~2h — an automatic miss against the 300s SLO.
			if _, err := pool.Exec(ctx,
				`UPDATE messages
				    SET created_at   = now() - interval '2 hours',
				        reviewed_at  = CASE WHEN $2 THEN now() - interval '30 seconds' END,
				        scheduled_at = CASE WHEN $2 THEN NULL ELSE now() - interval '30 seconds' END
				  WHERE id = $1`, res.MessageID, tc.held); err != nil {
				t.Fatalf("age the row: %v", err)
			}

			adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
			deliverer := fakeAsyncDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-gated", SentAs: "relay"}}
			rec := &recordingSendMetrics{}
			if err := outboundsend.NewSendWorker(adapter, deliverer).WithMetrics(rec).Work(ctx, workerJob(res.MessageID, 1)); err != nil {
				t.Fatalf("worker.Work: %v", err)
			}
			if len(rec.terminals) != 1 || rec.terminals[0] != "sent" {
				t.Fatalf("terminals = %v, want [sent]", rec.terminals)
			}
			if len(rec.latencies) != 1 {
				t.Fatalf("latencies = %v, want exactly one sample", rec.latencies)
			}
			gate := "scheduled_at"
			if tc.held {
				gate = "reviewed_at"
			}
			if got := rec.latencies[0]; got > 120 {
				t.Errorf("terminal latency = %.0fs, want ~30s: ClaimSend must carry messages.%s into SendJob, or the SLI charges the caller's own wait to e2a", got, gate)
			}
		})
	}
}
