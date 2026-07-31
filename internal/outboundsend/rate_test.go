package outboundsend_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/outboundsend"
)

// fakeRateGate records the agent ids it was asked to reserve for and returns
// a scripted decision/error.
type fakeRateGate struct {
	decision outboundsend.RateDecision
	err      error
	calls    []string
}

func (f *fakeRateGate) Reserve(_ context.Context, agentID string) (outboundsend.RateDecision, error) {
	f.calls = append(f.calls, agentID)
	return f.decision, f.err
}

// requireSnooze asserts Work returned a River snooze (not an attempt-burning
// error, not a cancel) and returns its duration.
func requireSnooze(t *testing.T, err error) time.Duration {
	t.Helper()
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("Work error = %v, want a river.JobSnoozeError (no attempt burned)", err)
	}
	return snooze.Duration
}

// TestSendWorker_RateLimitedReleasesClaimAndSnoozesWithoutProviderIO pins the
// deferral contract: an over-limit job releases its send claim and snoozes —
// no provider I/O, no terminal write, no temporary-failure record, one
// deferral metric.
func TestSendWorker_RateLimitedReleasesClaimAndSnoozesWithoutProviderIO(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{}
	gate := &fakeRateGate{decision: outboundsend.RateDecision{
		Allowed: false,
		RetryAt: time.Now().Add(30 * time.Second),
	}}
	rec := &recordingMetrics{}
	w := outboundsend.NewSendWorker(st, dl).WithRateGate(gate, time.Minute).WithMetrics(rec)

	d := requireSnooze(t, w.Work(context.Background(), job("msg_1", 1)))
	if d < 250*time.Millisecond || d > time.Minute {
		t.Errorf("snooze = %s, want within [250ms, window=1m]", d)
	}
	if dl.calls != 0 {
		t.Errorf("provider calls = %d, want 0", dl.calls)
	}
	if len(st.released) != 1 || st.released[0] != "msg_1" {
		t.Errorf("released = %v, want [msg_1] (claim back to accepted)", st.released)
	}
	if len(st.sent) != 0 || len(st.failed) != 0 || len(st.temporary) != 0 || len(st.deferred) != 0 {
		t.Errorf("no terminal/temporary write allowed on deferral: sent=%v failed=%v temporary=%v deferred=%v",
			st.sent, st.failed, st.temporary, st.deferred)
	}
	if len(gate.calls) != 1 || gate.calls[0] != "sender@agents.test" {
		t.Errorf("gate calls = %v, want one reserve for the message's agent", gate.calls)
	}
	if rec.rateDeferred != 1 {
		t.Errorf("rate deferrals = %d, want 1", rec.rateDeferred)
	}
	if len(rec.terminals) != 0 || len(rec.attempts) != 0 {
		t.Errorf("deferral emits no terminal/attempt samples: terminals=%v attempts=%v", rec.terminals, rec.attemptOutcomes())
	}
}

// TestSendWorker_RateLimitedSnoozeClamped pins the snooze bounds: a RetryAt in
// the past floors at 250ms (no hot loop), a skewed far-future RetryAt caps at
// the window (the job re-fires within ~1 window).
func TestSendWorker_RateLimitedSnoozeClamped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retryAt time.Time
		want    time.Duration
	}{
		{"past retry_at floors at 250ms", time.Now().Add(-time.Second), 250 * time.Millisecond},
		{"far-future retry_at caps at the window", time.Now().Add(2 * time.Hour), time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{job: acceptedJob("msg_1")}
			gate := &fakeRateGate{decision: outboundsend.RateDecision{Allowed: false, RetryAt: tc.retryAt}}
			w := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithRateGate(gate, time.Minute)
			if d := requireSnooze(t, w.Work(context.Background(), job("msg_1", 1))); d != tc.want {
				t.Errorf("snooze = %s, want %s", d, tc.want)
			}
		})
	}
}

// TestSendWorker_RateGateErrorFailsClosedAndSnoozes: a limiter outage must
// never submit unthrottled — release the claim and snooze on the fixed short
// interval (mirroring the ramp-error path), without a deferral metric.
func TestSendWorker_RateGateErrorFailsClosedAndSnoozes(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{}
	gate := &fakeRateGate{err: errors.New("rate store down")}
	rec := &recordingMetrics{}
	w := outboundsend.NewSendWorker(st, dl).WithRateGate(gate, time.Minute).WithMetrics(rec)

	if d := requireSnooze(t, w.Work(context.Background(), job("msg_1", 1))); d != time.Minute {
		t.Errorf("snooze = %s, want the fixed 1m error interval", d)
	}
	if dl.calls != 0 {
		t.Errorf("provider calls = %d, want 0 (fail closed)", dl.calls)
	}
	if len(st.released) != 1 {
		t.Errorf("released = %v, want the claim released", st.released)
	}
	if rec.rateDeferred != 0 {
		t.Errorf("rate deferrals = %d, want 0 (a store error is not a deferral)", rec.rateDeferred)
	}
}

// TestSendWorker_RateLimitedPastRetryHorizonFailsTerminally: a message that
// has been deferring past the 72h retry horizon takes the standard guarded
// terminal-failure path instead of snoozing forever.
func TestSendWorker_RateLimitedPastRetryHorizonFailsTerminally(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-73 * time.Hour)
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{}
	gate := &fakeRateGate{decision: outboundsend.RateDecision{
		Allowed: false,
		RetryAt: time.Now().Add(30 * time.Second),
	}}
	rec := &recordingMetrics{}
	w := outboundsend.NewSendWorker(st, dl).WithRateGate(gate, time.Minute).WithMetrics(rec)

	err := w.Work(context.Background(), job("msg_1", 4))
	if err == nil {
		t.Fatal("past-horizon deferral must terminate, not snooze")
	}
	var snooze *river.JobSnoozeError
	if errors.As(err, &snooze) {
		t.Fatal("past-horizon deferral must not snooze")
	}
	if dl.calls != 0 {
		t.Errorf("provider calls = %d, want 0", dl.calls)
	}
	if len(st.failed) != 1 {
		t.Fatalf("failed = %+v, want one terminal write", st.failed)
	}
	if !stringsEqual(rec.terminals, []string{"failed_local_retries"}) {
		t.Errorf("terminals = %v, want [failed_local_retries]", rec.terminals)
	}
	if rec.rateDeferred != 0 {
		t.Errorf("rate deferrals = %d, want 0 (this is a terminal, not a deferral)", rec.rateDeferred)
	}
}

// TestSendWorker_RateGateErrorPastRetryHorizonFailsTerminally: same horizon
// bound for the limiter-outage path.
func TestSendWorker_RateGateErrorPastRetryHorizonFailsTerminally(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-73 * time.Hour)
	st := &fakeStore{job: j}
	gate := &fakeRateGate{err: errors.New("rate store down")}
	rec := &recordingMetrics{}
	w := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithRateGate(gate, time.Minute).WithMetrics(rec)

	err := w.Work(context.Background(), job("msg_1", 4))
	if err == nil {
		t.Fatal("past-horizon gate outage must terminate, not snooze")
	}
	var snooze *river.JobSnoozeError
	if errors.As(err, &snooze) {
		t.Fatal("past-horizon gate outage must not snooze")
	}
	if len(st.failed) != 1 {
		t.Fatalf("failed = %+v, want one terminal write", st.failed)
	}
}

// TestSendWorker_RateGateAllowsSubmission: an allowed reservation falls
// through to the normal submission path untouched.
func TestSendWorker_RateGateAllowsSubmission(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1", SentAs: "relay"}}
	gate := &fakeRateGate{decision: outboundsend.RateDecision{Allowed: true}}
	w := outboundsend.NewSendWorker(st, dl).WithRateGate(gate, time.Minute)
	if err := w.Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.calls != 1 || len(st.sent) != 1 {
		t.Errorf("allowed job must submit once and mark sent: calls=%d sent=%v", dl.calls, st.sent)
	}
	if len(gate.calls) != 1 {
		t.Errorf("gate calls = %v, want exactly one reserve", gate.calls)
	}
}
