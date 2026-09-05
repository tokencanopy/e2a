package outboundsend_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/outboundsend"
)

// fakeRateGate records the agent ids it was asked to reserve for and returns
// a scripted decision/error. window defaults to time.Minute when unset.
type fakeRateGate struct {
	decision outboundsend.RateDecision
	err      error
	window   time.Duration
	calls    []string
}

func (f *fakeRateGate) Reserve(_ context.Context, agentID string) (outboundsend.RateDecision, error) {
	f.calls = append(f.calls, agentID)
	return f.decision, f.err
}

func (f *fakeRateGate) Window() time.Duration {
	if f.window > 0 {
		return f.window
	}
	return time.Minute
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
	w := outboundsend.NewSendWorker(st, dl).WithRateGate(gate).WithMetrics(rec)

	d := requireSnooze(t, w.Work(context.Background(), job("msg_1", 1)))
	// Deferral = clamp(until RetryAt) + deterministic jitter < window/4.
	if d < 250*time.Millisecond || d > time.Minute+time.Minute/4 {
		t.Errorf("snooze = %s, want within [250ms, window+jitter=1m15s]", d)
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
// the window (the job re-fires within ~1 window) — each plus the deterministic
// per-message jitter (< window/4) that keeps a deferred backlog from
// re-waking in lockstep.
func TestSendWorker_RateLimitedSnoozeClamped(t *testing.T) {
	jitterBound := time.Minute / 4
	for _, tc := range []struct {
		name    string
		retryAt time.Time
		wantMin time.Duration
		wantMax time.Duration
	}{
		{"past retry_at floors at 250ms", time.Now().Add(-time.Second), 250 * time.Millisecond, 250*time.Millisecond + jitterBound},
		{"far-future retry_at caps at the window", time.Now().Add(2 * time.Hour), time.Minute, time.Minute + jitterBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{job: acceptedJob("msg_1")}
			gate := &fakeRateGate{decision: outboundsend.RateDecision{Allowed: false, RetryAt: tc.retryAt}}
			w := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithRateGate(gate)
			if d := requireSnooze(t, w.Work(context.Background(), job("msg_1", 1))); d < tc.wantMin || d > tc.wantMax {
				t.Errorf("snooze = %s, want within [%s, %s]", d, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// TestSendWorker_RateLimitedJitterDeterministicPerMessage: the anti-herd
// jitter must be stable for a given message (no RNG state drifting across
// workers) but DIFFERENT across messages (a burst must fan out, not
// re-wake in lockstep).
func TestSendWorker_RateLimitedJitterDeterministicPerMessage(t *testing.T) {
	snoozeFor := func(messageID string) time.Duration {
		st := &fakeStore{job: acceptedJob(messageID)}
		gate := &fakeRateGate{decision: outboundsend.RateDecision{
			Allowed: false,
			RetryAt: time.Now().Add(30 * time.Second),
		}}
		w := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithRateGate(gate)
		return requireSnooze(t, w.Work(context.Background(), job(messageID, 1)))
	}
	// The base delay (time.Until) moves at ns scale between drives; the
	// jitter term is the deterministic part — equal within a small tolerance,
	// whereas RNG jitter would differ by up to window/4 (15s).
	diff := func(a, b time.Duration) time.Duration {
		if a > b {
			return a - b
		}
		return b - a
	}
	if d1, d2 := snoozeFor("msg_1"), snoozeFor("msg_1"); diff(d1, d2) > 100*time.Millisecond {
		t.Errorf("same message jittered differently across drives: %s vs %s", d1, d2)
	}

	// Cross-message fan-out: all drives share ONE fixed RetryAt, so the base
	// term time.Until(RetryAt) varies only by execution drift (µs) and any
	// spread beyond that can only come from the per-message jitter — the
	// previous per-drive RetryAt made inequality vacuous (base drift alone
	// guaranteed it, even if rateJitter always returned 0).
	retryAt := time.Now().Add(30 * time.Second)
	snoozeShared := func(messageID string) time.Duration {
		st := &fakeStore{job: acceptedJob(messageID)}
		gate := &fakeRateGate{decision: outboundsend.RateDecision{Allowed: false, RetryAt: retryAt}}
		w := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithRateGate(gate)
		return requireSnooze(t, w.Work(context.Background(), job(messageID, 1)))
	}
	distinct := map[time.Duration]bool{}
	var lo, hi time.Duration
	for i := 1; i <= 8; i++ {
		d := snoozeShared(fmt.Sprintf("msg_%d", i))
		distinct[d] = true
		if lo == 0 || d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
	}
	// 8 sequential drives drift the base by well under 1ms; the jitter range
	// is window/4 = 15s. A spread >100ms proves the burst actually fans out.
	if len(distinct) < 2 || hi-lo < 100*time.Millisecond {
		t.Errorf("jitter did not fan out across messages: spread=%s distinct=%d — herd not spread", hi-lo, len(distinct))
	}
}

// TestSendWorker_RateLimitedTinyWindowNoPanic: a gate window under 4ms
// truncates maxJitter.Milliseconds() to 0 — the jitter modulo must not
// divide by zero (found by review probe; test-shaped windows only, prod is
// 1m).
func TestSendWorker_RateLimitedTinyWindowNoPanic(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	gate := &fakeRateGate{
		decision: outboundsend.RateDecision{Allowed: false, RetryAt: time.Now().Add(30 * time.Second)},
		window:   2 * time.Millisecond,
	}
	w := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithRateGate(gate)
	if d := requireSnooze(t, w.Work(context.Background(), job("msg_1", 1))); d != 2*time.Millisecond {
		t.Errorf("snooze = %s, want the tiny window cap with zero jitter", d)
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
	w := outboundsend.NewSendWorker(st, dl).WithRateGate(gate).WithMetrics(rec)

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
// terminal-failure path instead of snoozing forever. The failure keeps its
// send_rate_timeout provenance (docs/observability.md tells operators to
// look for exactly that detail) and, for a ramp-eligible send, releases the
// ramp reservation the job still holds.
func TestSendWorker_RateLimitedPastRetryHorizonFailsTerminally(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-73 * time.Hour)
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{}
	gate := &fakeRateGate{decision: outboundsend.RateDecision{
		Allowed: false,
		RetryAt: time.Now().Add(30 * time.Second),
	}}
	rec := &recordingMetrics{}
	w := outboundsend.NewSendWorker(st, dl).WithRateGate(gate).WithMetrics(rec)

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
	if got := st.failed[0]; got.detail != "send_rate_timeout" || got.source != delivery.FailureSourceLocal {
		t.Errorf("terminal = {detail %q, source %v}, want {send_rate_timeout, local}",
			got.detail, got.source)
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
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	st := &fakeStore{job: j}
	gate := &fakeRateGate{err: errors.New("rate store down")}
	rec := &recordingMetrics{}
	w := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithRateGate(gate).WithMetrics(rec)

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
	if got := st.failed[0]; got.detail != "send_rate_timeout: rate store down" || got.source != delivery.FailureSourceLocal {
		t.Errorf("terminal = {detail %q, source %v}, want {send_rate_timeout: rate store down, local}",
			got.detail, got.source)
	}
}

// TestSendWorker_RateGateAllowsSubmission: an allowed reservation falls
// through to the normal submission path untouched.
func TestSendWorker_RateGateAllowsSubmission(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1", SentAs: "relay"}}
	gate := &fakeRateGate{decision: outboundsend.RateDecision{Allowed: true}}
	w := outboundsend.NewSendWorker(st, dl).WithRateGate(gate)
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
