package outboundsend_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/sendrate"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// rateWorkerFixture wires a real SendWorker (real claim store, real usage
// tracker, real outbox) over the shared terminalFixture seeding, with a
// short-window sendrate gate. Only the deliverer and metrics are fakes.
type rateWorkerFixture struct {
	fixture   *terminalFixture
	worker    *outboundsend.SendWorker
	deliverer *fakeDeliverer
	metrics   *recordingMetrics
	pool      *pgxpool.Pool
}

func newRateWorkerFixture(t *testing.T, window time.Duration, limit int) *rateWorkerFixture {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	// The REAL usage tracker: the deferral path must provably not meter.
	adapter := agent.NewOutboundSendStore(store, webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewUsageTracker(usage.NewStore(pool)))
	f := newTerminalFixture(t, pool, store, adapter)
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-rate", SentAs: "relay"}}
	rec := &recordingMetrics{}
	w := outboundsend.NewSendWorker(adapter, dl).
		WithRateGate(sendrate.NewStore(pool, window, limit)).
		WithMetrics(rec)
	return &rateWorkerFixture{fixture: f, worker: w, deliverer: dl, metrics: rec, pool: pool}
}

// drive runs one Work pass for the message's stamped send job, the way River
// would when the job becomes available.
func (r *rateWorkerFixture) drive(t *testing.T, messageID string) error {
	t.Helper()
	jobID := mustSendJobID(t, r.pool, messageID)
	rj := &river.Job[outboundsend.OutboundSendArgs]{
		JobRow: &rivertype.JobRow{ID: jobID, Attempt: 1, MaxAttempts: outboundsend.MaxSendAttempts, CreatedAt: time.Now().UTC()},
		Args:   outboundsend.OutboundSendArgs{MessageID: messageID},
	}
	return r.worker.Work(context.Background(), rj)
}

func (r *rateWorkerFixture) usageEvents(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM usage_events WHERE user_id=(SELECT user_id FROM agent_identities WHERE id=$1)`,
		r.fixture.agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// claimState returns the message's delivery_status and whether a send claim
// is currently held.
func (r *rateWorkerFixture) claimState(t *testing.T, messageID string) (string, bool) {
	t.Helper()
	var status string
	var claimed *time.Time
	if err := r.pool.QueryRow(context.Background(),
		`SELECT delivery_status, send_claimed_at FROM messages WHERE id=$1`, messageID).Scan(&status, &claimed); err != nil {
		t.Fatal(err)
	}
	return status, claimed != nil
}

// TestSendWorker_RateGateDefersOverLimitJobThenFiresAfterWindow is the
// end-to-end deferral contract against real Postgres: the over-limit job
// snoozes (no attempt burned), its claim is released back to 'accepted', and
// NOTHING else happens — no MarkSent/MarkFailed, no usage metering, no
// email.sent/email.failed outbox event. After the window frees capacity the
// same job re-drives and submits successfully.
func TestSendWorker_RateGateDefersOverLimitJobThenFiresAfterWindow(t *testing.T) {
	window := 400 * time.Millisecond
	r := newRateWorkerFixture(t, window, 1)
	msgA := r.fixture.seed(t, "rate-a", "accepted", "", false)
	msgB := r.fixture.seed(t, "rate-b", "accepted", "", false)

	// First job for the agent fires and consumes the single slot.
	if err := r.drive(t, msgA); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if status, claimed := r.claimState(t, msgA); status != "sent" || claimed {
		t.Fatalf("msgA status=%q claimed=%v, want sent/unclaimed", status, claimed)
	}

	// Second job for the SAME agent, inside the window: deferral.
	err := r.drive(t, msgB)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("over-limit job must snooze (not error/cancel), got %v", err)
	}
	if snooze.Duration < 250*time.Millisecond || snooze.Duration > window+window/4 {
		t.Errorf("snooze = %s, want within [250ms, window+jitter=%s]", snooze.Duration, window+window/4)
	}
	if status, claimed := r.claimState(t, msgB); status != "accepted" || claimed {
		t.Errorf("deferred msgB status=%q claimed=%v, want accepted with send_claimed_at NULL", status, claimed)
	}
	if r.deliverer.calls != 1 {
		t.Errorf("provider calls = %d, want exactly 1 (msgA only)", r.deliverer.calls)
	}
	if got := r.fixture.sentEventCount(t, msgB); got != 0 {
		t.Errorf("email.sent events for deferred msgB = %d, want 0", got)
	}
	if got := r.fixture.failedEventCount(t, msgB); got != 0 {
		t.Errorf("email.failed events for deferred msgB = %d, want 0", got)
	}
	if got := r.usageEvents(t); got != 1 {
		t.Errorf("usage events = %d, want 1 (msgA metered, msgB NOT)", got)
	}
	if r.metrics.rateDeferred != 1 {
		t.Errorf("rate deferrals = %d, want 1", r.metrics.rateDeferred)
	}
	if !stringsEqual(r.metrics.terminals, []string{"sent"}) || len(r.metrics.attempts) != 1 {
		t.Errorf("deferral emitted terminal/attempt samples: terminals=%v attempts=%v",
			r.metrics.terminals, r.metrics.attemptOutcomes())
	}

	// After the window frees capacity, the same job re-drives and submits.
	time.Sleep(window + 150*time.Millisecond)
	if err := r.drive(t, msgB); err != nil {
		t.Fatalf("re-drive after window: %v", err)
	}
	if status, claimed := r.claimState(t, msgB); status != "sent" || claimed {
		t.Errorf("msgB status=%q claimed=%v after re-drive, want sent/unclaimed", status, claimed)
	}
	if r.deliverer.calls != 2 {
		t.Errorf("provider calls = %d, want 2", r.deliverer.calls)
	}
	if got := r.fixture.sentEventCount(t, msgB); got != 1 {
		t.Errorf("email.sent events for msgB = %d, want 1 after the fire", got)
	}
	if got := r.usageEvents(t); got != 2 {
		t.Errorf("usage events = %d, want 2 (each message metered exactly once)", got)
	}
}

// TestSendWorker_ScheduledAndImmediateShareOneRateBucket: immediate and
// scheduled submissions draw on ONE per-agent fire-time budget — a scheduled
// job's fire consumes the same slot an immediate send would.
func TestSendWorker_ScheduledAndImmediateShareOneRateBucket(t *testing.T) {
	r := newRateWorkerFixture(t, time.Minute, 1)
	msgScheduled := r.fixture.seed(t, "rate-scheduled", "accepted", "", false)
	if _, err := r.pool.Exec(context.Background(),
		`UPDATE messages SET scheduled_at = now() + interval '1 hour' WHERE id=$1`, msgScheduled); err != nil {
		t.Fatal(err)
	}
	msgImmediate := r.fixture.seed(t, "rate-immediate", "accepted", "", false)

	// The scheduled job fires (its schedule time arrived) and takes the slot.
	if err := r.drive(t, msgScheduled); err != nil {
		t.Fatalf("scheduled fire: %v", err)
	}
	if status, _ := r.claimState(t, msgScheduled); status != "sent" {
		t.Fatalf("scheduled message status=%q, want sent", status)
	}

	// The immediate send for the same agent defers — same bucket.
	err := r.drive(t, msgImmediate)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("immediate send behind a scheduled fire must snooze, got %v", err)
	}
	if status, claimed := r.claimState(t, msgImmediate); status != "accepted" || claimed {
		t.Errorf("immediate message status=%q claimed=%v, want accepted/unclaimed", status, claimed)
	}
	if r.deliverer.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (the scheduled fire only)", r.deliverer.calls)
	}
	if got := r.usageEvents(t); got != 1 {
		t.Errorf("usage events = %d, want 1 (deferral meters nothing)", got)
	}
}
