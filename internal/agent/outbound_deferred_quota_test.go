package agent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/usage"
)

// TestSendWorker_ReviewedSendOverMonthlyQuotaRefusedAtFire pins the
// deferred-settlement gate for review-released sends (usage-based pricing
// v1): a held draft is never metered at accept, so the fire-time gate must
// also cover messages with reviewed_at set — without it, approving a hold
// backlog would bypass the monthly cap entirely. Monthly exhaustion at fire
// time is terminal (delivery_status=failed), matching scheduled sends.
func TestSendWorker_ReviewedSendOverMonthlyQuotaRefusedAtFire(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	installTask6DurableJobs(t, pool)
	api.SetOutboundEnqueuer(txSentinelEnqueuer{})
	ctx := context.Background()
	user, ag := selfAgent(t, store, "revquota")

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "held then approved", Body: "b",
	}, "send", "", nil, nil)
	if oerr != nil || res == nil {
		t.Fatalf("accept: res=%+v err=%+v", res, oerr)
	}
	var sendJobID *int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&sendJobID); err != nil || sendJobID == nil {
		t.Fatalf("send_job_id: %v", err)
	}
	// Stamp the review-release marker the approve funnel would have written.
	if _, err := pool.Exec(ctx, `UPDATE messages SET reviewed_at = now() WHERE id = $1`, res.MessageID); err != nil {
		t.Fatalf("stamp reviewed_at: %v", err)
	}

	adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
	quotaCalls := 0
	adapter.SetScheduledSendQuota(func(_ context.Context, _ string, units int) (string, bool, error) {
		quotaCalls++
		if units != 1 {
			t.Errorf("fire-time units = %d, want 1 (single recipient)", units)
		}
		return "messages_month", true, nil
	})
	deliverer := &countingAsyncDeliverer{}
	if err := outboundsend.NewSendWorker(adapter, deliverer).Work(ctx, workerJobWithID(res.MessageID, *sendJobID, 1)); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if quotaCalls != 1 {
		t.Fatalf("fire-time quota gate ran %d times for a reviewed message, want 1", quotaCalls)
	}
	if deliverer.calls != 0 {
		t.Errorf("over-quota reviewed send must NOT be submitted; deliverer calls=%d", deliverer.calls)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM messages WHERE id=$1`, res.MessageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("delivery_status = %q, want failed (refused at fire for monthly quota)", status)
	}
}

// TestSendWorker_DailyQuotaDefersInsteadOfFailing pins the messages_day
// outcome: daily exhaustion at fire time must NOT terminally fail the
// message — the claim is released, the worker snoozes, and the send stays
// 'accepted' to fire after the UTC-midnight reset.
func TestSendWorker_DailyQuotaDefersInsteadOfFailing(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	installTask6DurableJobs(t, pool)
	api.SetOutboundEnqueuer(txSentinelEnqueuer{})
	ctx := context.Background()
	user, ag := selfAgent(t, store, "dayquota")
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
	adapter.SetScheduledSendQuota(func(context.Context, string, int) (string, bool, error) {
		return "messages_day", true, nil
	})
	deliverer := &countingAsyncDeliverer{}
	err := outboundsend.NewSendWorker(adapter, deliverer).Work(ctx, workerJobWithID(res.MessageID, *sendJobID, 1))
	// The worker snoozes (a non-nil River sentinel), it does not fail.
	if err == nil {
		t.Fatal("worker.Work returned nil; want a snooze sentinel for daily-cap deferral")
	}
	var dqd *outboundsend.DailyQuotaDeferredError
	if errors.As(err, &dqd) {
		t.Fatalf("DailyQuotaDeferredError leaked to River unwrapped: %v (worker must convert it to a snooze)", err)
	}
	if deliverer.calls != 0 {
		t.Errorf("daily-deferred send must NOT be submitted; deliverer calls=%d", deliverer.calls)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM messages WHERE id=$1`, res.MessageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" {
		t.Errorf("delivery_status = %q, want accepted (claim released, send preserved for tomorrow)", status)
	}
}

// TestSendWorker_ReviewedDailyQuotaDefers pins the combination the TTL/approve
// funnels rely on: a REVIEW-RELEASED (reviewed_at set, not scheduled) send
// over the DAILY cap at fire time snoozes rather than terminally failing —
// and the released claim keeps its send_job_id, which is what makes the
// snoozed job re-claimable tomorrow.
func TestSendWorker_ReviewedDailyQuotaDefers(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	installTask6DurableJobs(t, pool)
	api.SetOutboundEnqueuer(txSentinelEnqueuer{})
	ctx := context.Background()
	user, ag := selfAgent(t, store, "revday")

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "held then approved", Body: "b",
	}, "send", "", nil, nil)
	if oerr != nil || res == nil {
		t.Fatalf("accept: res=%+v err=%+v", res, oerr)
	}
	var sendJobID *int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&sendJobID); err != nil || sendJobID == nil {
		t.Fatalf("send_job_id: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET reviewed_at = now() WHERE id = $1`, res.MessageID); err != nil {
		t.Fatalf("stamp reviewed_at: %v", err)
	}

	adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
	adapter.SetScheduledSendQuota(func(context.Context, string, int) (string, bool, error) {
		return "messages_day", true, nil
	})
	deliverer := &countingAsyncDeliverer{}
	err := outboundsend.NewSendWorker(adapter, deliverer).Work(ctx, workerJobWithID(res.MessageID, *sendJobID, 1))
	if err == nil {
		t.Fatal("worker.Work returned nil; want a snooze sentinel for daily-cap deferral")
	}
	if deliverer.calls != 0 {
		t.Errorf("daily-deferred reviewed send must NOT be submitted; deliverer calls=%d", deliverer.calls)
	}
	var status string
	var jobAfter *int64
	if err := pool.QueryRow(ctx, `SELECT delivery_status, send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&status, &jobAfter); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" {
		t.Errorf("delivery_status = %q, want accepted", status)
	}
	if jobAfter == nil || *jobAfter != *sendJobID {
		t.Errorf("send_job_id after deferral = %v, want %d preserved (re-claimability)", jobAfter, *sendJobID)
	}
}

// TestSendWorker_DailyQuotaPastHorizonFailsTerminally pins the bound on the
// deferral: a message whose accept (or scheduled fire) instant is older than
// the retry horizon must terminally fail instead of snoozing forever — the
// daily cap can be a deliberate permanent hard block (0), and limbo with no
// email.failed is worse than a terminal outcome.
func TestSendWorker_DailyQuotaPastHorizonFailsTerminally(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	installTask6DurableJobs(t, pool)
	api.SetOutboundEnqueuer(txSentinelEnqueuer{})
	ctx := context.Background()
	user, ag := selfAgent(t, store, "dayhorizon")

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "old", Body: "b",
	}, "send", "", nil, nil)
	if oerr != nil || res == nil {
		t.Fatalf("accept: res=%+v err=%+v", res, oerr)
	}
	var sendJobID *int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, res.MessageID).Scan(&sendJobID); err != nil || sendJobID == nil {
		t.Fatalf("send_job_id: %v", err)
	}
	// Age the accept past the retry horizon (72h) and mark it review-released.
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET created_at = now() - interval '80 hours', reviewed_at = now() - interval '79 hours' WHERE id = $1`,
		res.MessageID); err != nil {
		t.Fatalf("age message: %v", err)
	}

	adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
	adapter.SetScheduledSendQuota(func(context.Context, string, int) (string, bool, error) {
		return "messages_day", true, nil
	})
	deliverer := &countingAsyncDeliverer{}
	if err := outboundsend.NewSendWorker(adapter, deliverer).Work(ctx, workerJobWithID(res.MessageID, *sendJobID, 5)); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if deliverer.calls != 0 {
		t.Errorf("past-horizon daily-capped send must NOT be submitted; calls=%d", deliverer.calls)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM messages WHERE id=$1`, res.MessageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("delivery_status = %q, want failed (terminal past the retry horizon)", status)
	}
}
