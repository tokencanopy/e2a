package outboundsend_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/outboundsend"
)

// recordingMetrics captures every SLI emission for assertion.
type recordingMetrics struct {
	queueWaits []float64
	terminals  []string
	latencies  []float64
	attempts   []attemptSample
}

type attemptSample struct {
	outcome string
	seconds float64
}

func (r *recordingMetrics) OutboundQueueWait(seconds float64) {
	r.queueWaits = append(r.queueWaits, seconds)
}
func (r *recordingMetrics) OutboundTerminal(outcome string) {
	r.terminals = append(r.terminals, outcome)
}
func (r *recordingMetrics) OutboundTerminalLatency(seconds float64) {
	r.latencies = append(r.latencies, seconds)
}
func (r *recordingMetrics) OutboundAttempt(outcome string, seconds float64) {
	r.attempts = append(r.attempts, attemptSample{outcome: outcome, seconds: seconds})
}

func (r *recordingMetrics) attemptOutcomes() []string {
	out := make([]string, len(r.attempts))
	for i, a := range r.attempts {
		out[i] = a.outcome
	}
	return out
}

func stringsEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// timedJob builds a River job whose scheduled_at/attempted_at delta is the
// given queue wait. created_at is set a full hour earlier: a retried or
// snoozed job's cumulative age must NOT leak into the wait sample — only the
// due→pickup delta of THIS attempt counts.
func timedJob(id string, attempt int, wait time.Duration) *river.Job[outboundsend.OutboundSendArgs] {
	rj := job(id, attempt)
	scheduled := time.Now().Add(-wait).UTC()
	attempted := scheduled.Add(wait)
	rj.CreatedAt = scheduled.Add(-time.Hour)
	rj.ScheduledAt = scheduled
	rj.AttemptedAt = &attempted
	return rj
}

func TestSendWorker_MetricsQueueWaitRecordedOncePerAttempt(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1", SentAs: "relay"}}
	rec := &recordingMetrics{}
	w := outboundsend.NewSendWorker(st, dl).WithMetrics(rec)

	if err := w.Work(context.Background(), timedJob("msg_1", 1, 5*time.Second)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(rec.queueWaits) != 1 {
		t.Fatalf("queue waits = %v, want exactly one sample", rec.queueWaits)
	}
	if got := rec.queueWaits[0]; got < 4.9 || got > 5.1 {
		t.Errorf("queue wait = %.3fs, want ~5s", got)
	}
}

func TestSendWorker_MetricsQueueWaitGuardsMissingAndNegativeTimestamps(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  func() *river.Job[outboundsend.OutboundSendArgs]
	}{
		{"nil attempted_at", func() *river.Job[outboundsend.OutboundSendArgs] { return job("msg_1", 1) }},
		{"attempted before created", func() *river.Job[outboundsend.OutboundSendArgs] {
			return timedJob("msg_1", 1, -3*time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{job: acceptedJob("msg_1")}
			dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1"}}
			rec := &recordingMetrics{}
			if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), tc.job()); err != nil {
				t.Fatalf("Work: %v", err)
			}
			if len(rec.queueWaits) != 0 {
				t.Errorf("queue waits = %v, want none for %s", rec.queueWaits, tc.name)
			}
		})
	}
}

func TestSendWorker_MetricsSuccessRecordsAttemptAndSentTerminal(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1", SentAs: "relay"}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !stringsEqual(rec.attemptOutcomes(), []string{"success"}) {
		t.Errorf("attempts = %v, want [success]", rec.attemptOutcomes())
	}
	if !stringsEqual(rec.terminals, []string{"sent"}) {
		t.Errorf("terminals = %v, want [sent]", rec.terminals)
	}
	if rec.attempts[0].seconds < 0 {
		t.Errorf("attempt duration = %f, want >= 0", rec.attempts[0].seconds)
	}
}

func TestSendWorker_MetricsTemporaryFailureIsNotTerminal(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("transient 421")}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", 1)); err == nil {
		t.Fatal("retryable failure must return an error so River retries")
	}
	if !stringsEqual(rec.attemptOutcomes(), []string{"temporary_failure"}) {
		t.Errorf("attempts = %v, want [temporary_failure]", rec.attemptOutcomes())
	}
	if len(rec.terminals) != 0 {
		t.Errorf("terminals = %v, want none — a retryable failure is not a terminal outcome", rec.terminals)
	}
}

func TestSendWorker_MetricsSuppressedTerminalWithoutAttempt(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1"), suppressed: []string{"blocked@example.net"}}
	dl := &fakeDeliverer{}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", 1)); err == nil {
		t.Fatal("suppressed message must cancel")
	}
	if len(rec.attempts) != 0 {
		t.Errorf("attempts = %v, want none — the suppression guard fires before provider I/O", rec.attempts)
	}
	if !stringsEqual(rec.terminals, []string{"failed_suppressed"}) {
		t.Errorf("terminals = %v, want [failed_suppressed]", rec.terminals)
	}
}

func TestSendWorker_MetricsPermanentFailure(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("recipient rejected 550"), Permanent: true}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", 1)); err == nil {
		t.Fatal("permanent failure should return a (cancel) error")
	}
	if !stringsEqual(rec.attemptOutcomes(), []string{"permanent_failure"}) {
		t.Errorf("attempts = %v, want [permanent_failure]", rec.attemptOutcomes())
	}
	if !stringsEqual(rec.terminals, []string{"failed_provider"}) {
		t.Errorf("terminals = %v, want [failed_provider]", rec.terminals)
	}
}

func TestSendWorker_MetricsProviderEvidenceSettlesAsSentWithoutAttempt(t *testing.T) {
	j := acceptedJob("msg_1")
	j.ProviderAccepted = true
	j.ProviderMessageID = "ses-evidence-1"
	st := &fakeStore{job: j}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithMetrics(rec).Work(context.Background(), job("msg_1", 2)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(rec.attempts) != 0 {
		t.Errorf("attempts = %v, want none — the evidence settle performs no provider I/O", rec.attempts)
	}
	if !stringsEqual(rec.terminals, []string{"sent"}) {
		t.Errorf("terminals = %v, want [sent]", rec.terminals)
	}
}

func TestSendWorker_MetricsFinalAttemptDeferralEmitsNoTerminal(t *testing.T) {
	// A deferred final attempt is NOT a terminal outcome: the reconciler
	// declares (and counts) the real one — sent on provider evidence, failed
	// otherwise. Emitting here too would double-count the message in
	// e2a_outbound_terminal_total and inflate every SLI denominator built on it.
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("boom 421")}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", outboundsend.MaxSendAttempts)); err == nil {
		t.Fatal("final attempt failure should return an error so River discards")
	}
	if !stringsEqual(rec.attemptOutcomes(), []string{"temporary_failure"}) {
		t.Errorf("attempts = %v, want [temporary_failure]", rec.attemptOutcomes())
	}
	if len(rec.terminals) != 0 {
		t.Errorf("terminals = %v, want none (reconciler counts the deferred row later)", rec.terminals)
	}
	if len(st.deferred) != 1 {
		t.Errorf("DeferTerminalFailure calls = %v, want exactly one", st.deferred)
	}
}

func TestSendWorker_MetricsOutagePastHorizonIsLocalRetriesTerminal(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-73 * time.Hour)
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("connection refused"), Outage: true}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", 2)); err == nil {
		t.Fatal("an outage past the retry horizon should fail terminally")
	}
	if !stringsEqual(rec.attemptOutcomes(), []string{"temporary_failure"}) {
		t.Errorf("attempts = %v, want [temporary_failure]", rec.attemptOutcomes())
	}
	if !stringsEqual(rec.terminals, []string{"failed_local_retries"}) {
		t.Errorf("terminals = %v, want [failed_local_retries]", rec.terminals)
	}
}

func TestSendWorker_MetricsOutageWithinHorizonIsNotTerminal(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now()
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("dial unavailable"), Outage: true}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", 1)); err == nil {
		t.Fatal("outage must snooze")
	}
	if !stringsEqual(rec.attemptOutcomes(), []string{"temporary_failure"}) {
		t.Errorf("attempts = %v, want [temporary_failure]", rec.attemptOutcomes())
	}
	if len(rec.terminals) != 0 {
		t.Errorf("terminals = %v, want none — an in-horizon outage defers, it does not terminate", rec.terminals)
	}
}

// TestSendWorker_MetricsDefaultIsNilSafe pins the constructor default: a
// worker built without WithMetrics must run every path without panicking.
func TestSendWorker_MetricsDefaultIsNilSafe(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1"}}
	if err := outboundsend.NewSendWorker(st, dl).Work(context.Background(), timedJob("msg_1", 1, time.Second)); err != nil {
		t.Fatalf("Work without metrics: %v", err)
	}
	// WithMetrics(nil) must keep the no-op default rather than storing nil.
	st2 := &fakeStore{job: acceptedJob("msg_2")}
	if err := outboundsend.NewSendWorker(st2, dl).WithMetrics(nil).Work(context.Background(), timedJob("msg_2", 1, time.Second)); err != nil {
		t.Fatalf("Work with nil metrics: %v", err)
	}
}

// ── Acceptance→terminal latency SLI (docs/observability.md) ──────
//
// The latency histogram is observed EXACTLY ONCE per message, co-located
// with the e2a_outbound_terminal_total emission so the two share their
// exactly-once contract: a terminal that is not counted is not timed, and
// vice versa. The value is the terminal write's occurred_at −
// messages.created_at (SendJob.AcceptedAt), never a re-computed clock.

func TestSendWorker_TerminalLatencySuccessPath(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-90 * time.Second)
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1", SentAs: "relay"}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(rec.latencies) != 1 {
		t.Fatalf("latencies = %v, want exactly one sample (parity with the terminal count)", rec.latencies)
	}
	if got := rec.latencies[0]; got < 85 || got > 95 {
		t.Errorf("terminal latency = %.1fs, want ~90s (occurred_at − accepted_at)", got)
	}
}

func TestSendWorker_TerminalLatencyEvidenceSettleUsesProviderAcceptTime(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-90 * time.Second)
	providerAcceptedAt := time.Now().Add(-60 * time.Second).UTC()
	j.ProviderAccepted = true
	j.ProviderAcceptedAt = &providerAcceptedAt
	j.ProviderMessageID = "ses-evidence-1"
	st := &fakeStore{job: j}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithMetrics(rec).Work(context.Background(), job("msg_1", 2)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(rec.latencies) != 1 {
		t.Fatalf("latencies = %v, want exactly one sample", rec.latencies)
	}
	// The settle's occurred_at is the provider-accept evidence time, so the
	// latency measures acceptance→provider-accept (~30s), not
	// acceptance→settle (~90s).
	if got := rec.latencies[0]; got < 25 || got > 35 {
		t.Errorf("terminal latency = %.1fs, want ~30s (provider-accept evidence time − accepted_at)", got)
	}
}

func TestSendWorker_TerminalLatencyRecordedForFailureOutcomes(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-2 * time.Minute)
	st := &fakeStore{job: j, suppressed: []string{"blocked@example.net"}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithMetrics(rec).Work(context.Background(), job("msg_1", 1)); err == nil {
		t.Fatal("suppressed message must cancel")
	}
	if !stringsEqual(rec.terminals, []string{"failed_suppressed"}) {
		t.Errorf("terminals = %v, want [failed_suppressed]", rec.terminals)
	}
	if len(rec.latencies) != 1 {
		t.Fatalf("latencies = %v, want exactly one sample alongside the terminal count", rec.latencies)
	}
	if got := rec.latencies[0]; got < 110 || got > 130 {
		t.Errorf("terminal latency = %.1fs, want ~120s", got)
	}
}

func TestSendWorker_TerminalLatencyNotRecordedForDeferredFinalAttempt(t *testing.T) {
	// Parity with e2a_outbound_terminal_total: a deferred final attempt is
	// not terminal — the reconciler counts AND times the row when it
	// settles. Observing here too would double-count the message.
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-10 * time.Minute)
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("boom 421")}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", outboundsend.MaxSendAttempts)); err == nil {
		t.Fatal("final attempt failure should return an error so River discards")
	}
	if len(rec.terminals) != 0 || len(rec.latencies) != 0 {
		t.Errorf("terminals=%v latencies=%v, want neither (reconciler settles the deferred row later)", rec.terminals, rec.latencies)
	}
}

func TestSendWorker_TerminalLatencyGuardsZeroAcceptedAt(t *testing.T) {
	// acceptedJob leaves AcceptedAt zero (hand-built row): a terminal must
	// still be counted, but no latency sample can be computed — same
	// discipline as the queue-wait guard against missing timestamps.
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1", SentAs: "relay"}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !stringsEqual(rec.terminals, []string{"sent"}) {
		t.Errorf("terminals = %v, want [sent]", rec.terminals)
	}
	if len(rec.latencies) != 0 {
		t.Errorf("latencies = %v, want none with a zero accepted_at", rec.latencies)
	}
}

func TestSendWorker_TerminalLatencyMarkFailedSettlesSentUsesWriteTime(t *testing.T) {
	// When the guarded MarkFailed settles the row as SENT on raced-in
	// provider evidence, the store rewrites occurred_at to the provider-accept
	// time for the durable write. The latency SLI must use the write's
	// EFFECTIVE occurred_at (returned by MarkFailed), not the caller's
	// observation time — that is what the write actually did.
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-90 * time.Second)
	providerAcceptedAt := time.Now().Add(-30 * time.Second).UTC()
	st := &fakeStore{job: j, settleStatus: delivery.StatusSent, settleAt: providerAcceptedAt}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("rejected 550"), Permanent: true}}
	rec := &recordingMetrics{}
	if err := outboundsend.NewSendWorker(st, dl).WithMetrics(rec).Work(context.Background(), job("msg_1", 1)); err == nil {
		t.Fatal("permanent failure should cancel")
	}
	if !stringsEqual(rec.terminals, []string{"sent"}) {
		t.Fatalf("terminals = %v, want [sent] (evidence settle via the guarded write)", rec.terminals)
	}
	if len(rec.latencies) != 1 {
		t.Fatalf("latencies = %v, want exactly one sample", rec.latencies)
	}
	// provider-accept time − accepted_at ≈ 60s, NOT now − accepted_at ≈ 90s.
	if got := rec.latencies[0]; got < 55 || got > 65 {
		t.Errorf("latency = %.1fs, want ~60s (the write's effective occurred_at − accepted_at)", got)
	}
}
