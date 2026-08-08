package outboundsend_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outboundsend"
)

type fakeStore struct {
	job         *outboundsend.SendJob
	loadErr     error
	markSentErr error
	releaseErr  error
	// settleStatus/settleAt override MarkFailed's default StatusFailed return,
	// mirroring the production store's evidence-settle branch, which rewrites
	// occurred_at to the provider-accept evidence time for the durable write.
	settleStatus delivery.Status
	settleAt     time.Time
	// terminalAfterFailure mirrors the production store: once MarkFailed commits,
	// a retry can no longer claim the terminal message and ClaimSend returns nil.
	terminalAfterFailure bool
	// suppressed / suppressedErr drive the pre-provider suppression guard.
	suppressed    []string
	suppressedErr error

	sent      []sentCall
	failed    []failedCall
	deferred  []failedCall
	temporary []failedCall
	released  []string
	// suppressionUserID records the tenant the guard was scoped to.
	suppressionUserID    string
	suppressionAgentID   string
	suppressionCheckedAt time.Time
}

type sentCall struct{ id, provider, sentAs string }
type failedCall struct {
	id                string
	attempt           int
	occurredAt        time.Time
	detail            string
	source            delivery.FailureSource
	blockedRecipients []string
}

func (f *fakeStore) ClaimSend(_ context.Context, _ string, _ int64) (*outboundsend.SendJob, error) {
	if f.terminalAfterFailure && len(f.failed) > 0 {
		return nil, f.loadErr
	}
	return f.job, f.loadErr
}
func (f *fakeStore) MarkSent(_ context.Context, id string, _ int64, _ int, _ time.Time, provider, sentAs string) error {
	f.sent = append(f.sent, sentCall{id, provider, sentAs})
	return f.markSentErr
}
func (f *fakeStore) MarkFailed(_ context.Context, id string, _ int64, attempt int, occurredAt time.Time, detail string, source delivery.FailureSource, _ messagelifecycle.ReasonCode, blockedRecipients []string) (delivery.Status, time.Time, error) {
	f.failed = append(f.failed, failedCall{id: id, attempt: attempt, occurredAt: occurredAt, detail: detail, source: source, blockedRecipients: blockedRecipients})
	status := f.settleStatus
	if status == "" {
		status = delivery.StatusFailed
	}
	at := f.settleAt
	if at.IsZero() {
		at = occurredAt
	}
	return status, at, nil
}
func (f *fakeStore) PreserveTerminalFailure(context.Context, string, int64, int, time.Time, string, delivery.FailureSource, messagelifecycle.ReasonCode, []string) error {
	return nil
}
func (f *fakeStore) DeferTerminalFailure(_ context.Context, id string, _ int64, _ int, _ time.Time, detail string) error {
	f.deferred = append(f.deferred, failedCall{id: id, detail: detail})
	return nil
}
func (f *fakeStore) RecordTemporaryFailure(_ context.Context, id string, _ int64, _ int, _ time.Time, _ string) error {
	f.released = append(f.released, id)
	f.temporary = append(f.temporary, failedCall{id: id})
	return f.releaseErr
}
func (f *fakeStore) ReleaseSend(_ context.Context, id string, _ int64) error {
	f.released = append(f.released, id)
	return f.releaseErr
}
func (f *fakeStore) SuppressedRecipients(_ context.Context, userID, agentID string, _ []string) ([]string, error) {
	f.suppressionUserID = userID
	f.suppressionAgentID = agentID
	f.suppressionCheckedAt = time.Now().UTC()
	return f.suppressed, f.suppressedErr
}

type fakeDeliverer struct {
	out        outboundsend.DeliverOutcome
	calls      int
	returnedAt time.Time
}

type fakeRampGate struct {
	decision   outboundsend.RampDecision
	err        error
	calls      []outboundsend.RampRequest
	confirmed  []string
	released   []string
	resolved   []string
	confirmErr error
	releaseErr error
}

func (f *fakeRampGate) Reserve(_ context.Context, req outboundsend.RampRequest) (outboundsend.RampDecision, error) {
	f.calls = append(f.calls, req)
	return f.decision, f.err
}

func (f *fakeRampGate) Confirm(_ context.Context, messageID string) error {
	f.confirmed = append(f.confirmed, messageID)
	return f.confirmErr
}

func (f *fakeRampGate) Release(_ context.Context, messageID string) error {
	f.released = append(f.released, messageID)
	return f.releaseErr
}

func (f *fakeRampGate) Resolve(_ context.Context, messageID string) error {
	f.resolved = append(f.resolved, messageID)
	return nil
}

type permanentRampError struct{ msg string }

func (e permanentRampError) Error() string   { return e.msg }
func (e permanentRampError) Permanent() bool { return true }

func (f *fakeDeliverer) Deliver(_ context.Context, _ *outboundsend.SendJob) outboundsend.DeliverOutcome {
	f.calls++
	f.returnedAt = time.Now().UTC()
	return f.out
}

func job(id string, attempt int) *river.Job[outboundsend.OutboundSendArgs] {
	return &river.Job[outboundsend.OutboundSendArgs]{
		JobRow: &rivertype.JobRow{ID: 1, Attempt: attempt, MaxAttempts: outboundsend.MaxSendAttempts, Kind: outboundsend.OutboundSendArgs{}.Kind()},
		Args:   outboundsend.OutboundSendArgs{MessageID: id},
	}
}

func acceptedJob(id string) *outboundsend.SendJob {
	return &outboundsend.SendJob{MessageID: id, UserID: "user_owner", AgentID: "sender@agents.test", Status: "accepted", EnvelopeFrom: "a@x.com", Recipients: []string{"b@y.com"}, RawMessage: []byte("raw")}
}

func TestSendWorker_Success(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1", SentAs: "relay"}}
	w := outboundsend.NewSendWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(st.sent) != 1 || st.sent[0].provider != "ses-1" || st.sent[0].sentAs != "relay" {
		t.Errorf("MarkSent = %+v, want one call with ses-1/relay", st.sent)
	}
	if len(st.failed) != 0 {
		t.Errorf("unexpected MarkFailed: %+v", st.failed)
	}
}

func TestSendWorker_AlreadyTerminalIsNoOp(t *testing.T) {
	st := &fakeStore{job: &outboundsend.SendJob{MessageID: "msg_1", Status: "sent"}}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "should-not-send"}}
	w := outboundsend.NewSendWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(st.sent) != 0 {
		t.Errorf("terminal message must not re-send, got MarkSent %+v", st.sent)
	}
}

func TestSendWorker_MessageGoneIsNoOp(t *testing.T) {
	st := &fakeStore{job: nil} // LoadForSend returns (nil, nil) → gone
	w := outboundsend.NewSendWorker(st, &fakeDeliverer{})
	if err := w.Work(context.Background(), job("msg_gone", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(st.sent) != 0 || len(st.failed) != 0 {
		t.Errorf("gone message must be a no-op")
	}
}

func TestSendWorker_PermanentFailureCancels(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("recipient rejected 550"), Permanent: true}}
	w := outboundsend.NewSendWorker(st, dl)
	err := w.Work(context.Background(), job("msg_1", 1))
	if err == nil {
		t.Fatal("permanent failure should return a (cancel) error")
	}
	if len(st.failed) != 1 {
		t.Errorf("permanent failure must MarkFailed once, got %+v", st.failed)
	}
	if st.failed[0].source != delivery.FailureSourceProvider {
		t.Errorf("permanent 5xx failure source = %q, want provider (never correctable)", st.failed[0].source)
	}
}

// TestSendWorker_LastAttemptDefersTerminalOutcome pins the §3.1 grace behavior:
// a final attempt that fails (possibly ambiguously — the connection may have
// died AFTER SES accepted the DATA) must NOT declare failed inline. It records
// the diagnostic + releases the claim via DeferTerminalFailure and returns an
// error so River discards; the terminal reconciler declares the outcome after
// the provider-evidence grace window.
func TestSendWorker_LastAttemptDefersTerminalOutcome(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("boom 421")}}
	w := outboundsend.NewSendWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", outboundsend.MaxSendAttempts)); err == nil {
		t.Fatal("final attempt failure should return an error so River discards")
	}
	if len(st.failed) != 0 {
		t.Errorf("final attempt must NOT MarkFailed inline (provider evidence may still arrive), got %+v", st.failed)
	}
	if len(st.deferred) != 1 || st.deferred[0].id != "msg_1" || st.deferred[0].detail != "boom 421" {
		t.Errorf("final attempt must defer the terminal outcome with its diagnostic, got %+v", st.deferred)
	}
}

// TestSendWorker_ProviderEvidenceSettlesWithoutResubmit pins the duplicate
// guard for the SMTP-accept↔mark-sent crash window: when the claim reports
// recorded provider-accept evidence, the worker settles the message as sent
// with the evidence-repaired provider id and performs NO provider I/O.
func TestSendWorker_ProviderEvidenceSettlesWithoutResubmit(t *testing.T) {
	j := acceptedJob("msg_1")
	j.SentAs = "relay"
	j.ProviderAccepted = true
	j.ProviderMessageID = "ses-evidence-1"
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "must-not-be-used"}}
	w := outboundsend.NewSendWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 2)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.calls != 0 {
		t.Errorf("Deliver called %d times, want 0 — evidence means the provider already has the message", dl.calls)
	}
	if len(st.sent) != 1 || st.sent[0].provider != "ses-evidence-1" || st.sent[0].sentAs != "relay" {
		t.Errorf("MarkSent = %+v, want one call with the evidence provider id", st.sent)
	}
	if len(st.failed) != 0 || len(st.deferred) != 0 {
		t.Errorf("evidence settle must not fail/defer, got failed=%+v deferred=%+v", st.failed, st.deferred)
	}
}

func TestSendWorker_ProviderOutageRecordsTemporaryLifecycleBeforeSnooze(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_outage")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("dial unavailable"), Outage: true}}
	err := outboundsend.NewSendWorker(st, dl).Work(context.Background(), job("msg_outage", 1))
	if err == nil {
		t.Fatal("outage must snooze")
	}
	if len(st.temporary) != 1 || len(st.released) != 1 {
		t.Fatalf("temporary=%+v released=%+v", st.temporary, st.released)
	}
}

func TestSendWorker_SuppressionObservationTimeFollowsDecision(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_suppressed"), suppressed: []string{"blocked@example.net"}}
	rj := job("msg_suppressed", 4)
	oldAttempt := time.Now().Add(-time.Hour).UTC()
	rj.AttemptedAt = &oldAttempt

	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).Work(context.Background(), rj); err == nil {
		t.Fatal("suppressed message must cancel")
	}
	if len(st.failed) != 1 {
		t.Fatalf("failed=%+v, want one terminal observation", st.failed)
	}
	if st.failed[0].occurredAt.Before(st.suppressionCheckedAt) {
		t.Fatalf("occurred_at=%s predates suppression decision=%s", st.failed[0].occurredAt, st.suppressionCheckedAt)
	}
}

func TestSendWorker_RampLimitedReleasesAndSnoozesWithoutProviderIO(t *testing.T) {
	j := acceptedJob("msg_1")
	j.Domain = "new.example.com"
	j.MessageType = "send"
	j.SentAs = "own_address"
	j.Recipients = []string{"One@example.net", "one@example.net", "two@example.net"}
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{}
	gate := &fakeRampGate{decision: outboundsend.RampDecision{
		Allowed: false,
		RetryAt: time.Now().Add(6 * time.Hour),
	}}

	err := outboundsend.NewSendWorker(st, dl, gate).Work(context.Background(), job("msg_1", 5))
	if err == nil {
		t.Fatal("limited send should snooze")
	}
	if dl.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", dl.calls)
	}
	if len(st.released) != 1 || st.released[0] != "msg_1" {
		t.Fatalf("released = %v, want msg_1", st.released)
	}
	if len(gate.calls) != 1 || gate.calls[0].Units != 2 || gate.calls[0].Domain != "new.example.com" {
		t.Fatalf("gate calls = %+v, want two deduplicated recipients", gate.calls)
	}
}

func TestSendWorker_RampErrorFailsClosedAndSnoozes(t *testing.T) {
	j := acceptedJob("msg_1")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{}
	gate := &fakeRampGate{err: errors.New("database unavailable")}

	if err := outboundsend.NewSendWorker(st, dl, gate).Work(context.Background(), job("msg_1", 1)); err == nil {
		t.Fatal("ramp storage error should snooze")
	}
	if dl.calls != 0 || len(st.released) != 1 {
		t.Fatalf("gate error must release without provider I/O: calls=%d released=%v", dl.calls, st.released)
	}
}

func TestSendWorker_RampExemptsPlatformTest(t *testing.T) {
	j := acceptedJob("msg_test")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "test", "relay"
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-test"}}
	gate := &fakeRampGate{decision: outboundsend.RampDecision{Allowed: false}}

	if err := outboundsend.NewSendWorker(st, dl, gate).Work(context.Background(), job("msg_test", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(gate.calls) != 0 || dl.calls != 1 {
		t.Fatalf("platform test should bypass ramp: gate=%d provider=%d", len(gate.calls), dl.calls)
	}
}

func TestSendWorker_ProviderEvidencePrecedesRamp(t *testing.T) {
	j := acceptedJob("msg_1")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	j.ProviderAccepted, j.ProviderMessageID = true, "ses-evidence"
	st := &fakeStore{job: j}
	gate := &fakeRampGate{decision: outboundsend.RampDecision{Allowed: false}}

	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}, gate).Work(context.Background(), job("msg_1", 2)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(gate.calls) != 0 {
		t.Fatalf("provider evidence must settle before ramp reservation, got %+v", gate.calls)
	}
}

func TestSendWorker_ConfirmsRampAfterMarkSent(t *testing.T) {
	j := acceptedJob("msg_confirm")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	st := &fakeStore{job: j}
	gate := &fakeRampGate{decision: outboundsend.RampDecision{Allowed: true}}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-confirm", SentAs: "own_address"}}
	if err := outboundsend.NewSendWorker(st, dl, gate).Work(context.Background(), job(j.MessageID, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(st.sent) != 1 || len(gate.confirmed) != 1 || gate.confirmed[0] != j.MessageID {
		t.Fatalf("sent=%v confirmed=%v", st.sent, gate.confirmed)
	}
}

func TestSendWorker_RepairsRampConfirmationForAlreadySentMessage(t *testing.T) {
	j := acceptedJob("msg_repair")
	j.Domain, j.MessageType, j.SentAs, j.Status = "new.example.com", "send", "own_address", "sent"
	gate := &fakeRampGate{}
	dl := &fakeDeliverer{}
	if err := outboundsend.NewSendWorker(&fakeStore{job: j}, dl, gate).Work(context.Background(), job(j.MessageID, 2)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.calls != 0 || len(gate.resolved) != 1 {
		t.Fatalf("deliver=%d resolved=%v", dl.calls, gate.resolved)
	}
}

func TestSendWorker_ReleasesRampOnPermanentProviderFailure(t *testing.T) {
	j := acceptedJob("msg_release")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	gate := &fakeRampGate{decision: outboundsend.RampDecision{Allowed: true}}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("rejected"), Permanent: true}}
	_ = outboundsend.NewSendWorker(&fakeStore{job: j}, dl, gate).Work(context.Background(), job(j.MessageID, 1))
	if len(gate.released) != 1 || gate.released[0] != j.MessageID {
		t.Fatalf("released=%v", gate.released)
	}
}

func TestSendWorker_RetainsRampOnAmbiguousFailure(t *testing.T) {
	j := acceptedJob("msg_ambiguous")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	gate := &fakeRampGate{decision: outboundsend.RampDecision{Allowed: true}}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("connection reset")}}
	_ = outboundsend.NewSendWorker(&fakeStore{job: j}, dl, gate).Work(context.Background(), job(j.MessageID, 1))
	if len(gate.released) != 0 {
		t.Fatalf("ambiguous failure released ramp: %v", gate.released)
	}
}

func TestSendWorker_FailsPermanentRampInvariant(t *testing.T) {
	j := acceptedJob("msg_bad_ramp")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	st := &fakeStore{job: j}
	gate := &fakeRampGate{err: permanentRampError{"domain missing"}}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}, gate).Work(context.Background(), job(j.MessageID, 1)); err == nil {
		t.Fatal("permanent ramp invariant should terminate")
	}
	if len(st.failed) != 1 {
		t.Fatalf("failed=%v", st.failed)
	}
}

func TestSendWorker_FailsRampDeferredMessagePastHorizon(t *testing.T) {
	j := acceptedJob("msg_ramp_timeout")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	j.AcceptedAt = time.Now().Add(-73 * time.Hour)
	st := &fakeStore{job: j}
	gate := &fakeRampGate{decision: outboundsend.RampDecision{Allowed: false, RetryAt: time.Now().Add(time.Hour)}}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}, gate).Work(context.Background(), job(j.MessageID, 1)); err == nil {
		t.Fatal("past-horizon ramp deferral should terminate")
	}
	if len(st.failed) != 1 || len(gate.released) != 1 {
		t.Fatalf("failed=%v released=%v", st.failed, gate.released)
	}
}

// A scheduled send measures its retry horizon from scheduled_at, not accept:
// accepted 10 days ago but firing ~now, a ramp deferral must snooze/retry — NOT
// terminally fail as the immediate-send case above does at the same accept age.
// Guards the fix for the long-scheduled-send false-failure blocker.
func TestSendWorker_ScheduledSendHorizonMeasuredFromScheduledAt(t *testing.T) {
	j := acceptedJob("msg_sched_horizon")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	j.AcceptedAt = time.Now().Add(-10 * 24 * time.Hour) // long before fire
	j.ScheduledAt = time.Now()                          // just fired — inside the horizon
	st := &fakeStore{job: j}
	gate := &fakeRampGate{decision: outboundsend.RampDecision{Allowed: false, RetryAt: time.Now().Add(time.Hour)}}
	err := outboundsend.NewSendWorker(st, &fakeDeliverer{}, gate).Work(context.Background(), job(j.MessageID, 1))
	if len(st.failed) != 0 {
		t.Fatalf("a just-fired long-scheduled send must not be terminated on a ramp deferral; failed=%v err=%v", st.failed, err)
	}
}

func TestSendWorker_RetryableFailureDoesNotMarkFailed(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("transient 421")}}
	w := outboundsend.NewSendWorker(st, dl)
	err := w.Work(context.Background(), job("msg_1", 1))
	if err == nil {
		t.Fatal("retryable failure must return an error so River retries")
	}
	if len(st.failed) != 0 {
		t.Errorf("a non-final retryable failure must NOT MarkFailed (status stays accepted), got %+v", st.failed)
	}
	if len(st.sent) != 0 {
		t.Errorf("failed send must not MarkSent")
	}
	if len(st.released) != 1 || st.released[0] != "msg_1" {
		t.Errorf("retryable failure must release the active claim, got %v", st.released)
	}
}

func TestSendWorker_RetryableFailureReleaseErrorRetries(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1"), releaseErr: errors.New("db unavailable")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("transient 421")}}
	w := outboundsend.NewSendWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 1)); err == nil || !errors.Is(err, st.releaseErr) {
		t.Fatalf("Work error = %v, want release error", err)
	}
	if len(st.released) != 1 {
		t.Fatalf("release calls = %v, want one", st.released)
	}
}

func TestSendWorker_TerminalRampReleaseFailureResolvesOnRetry(t *testing.T) {
	j := acceptedJob("msg_1")
	j.Domain, j.MessageType, j.SentAs = "new.example.com", "send", "own_address"
	st := &fakeStore{job: j, terminalAfterFailure: true}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("provider rejected message"), Permanent: true}}
	gate := &fakeRampGate{
		decision:   outboundsend.RampDecision{Allowed: true},
		releaseErr: errors.New("ramp database unavailable"),
	}
	w := outboundsend.NewSendWorker(st, dl, gate)

	if err := w.Work(context.Background(), job(j.MessageID, 1)); err == nil || !errors.Is(err, gate.releaseErr) {
		t.Fatalf("first Work error = %v, want ramp release failure", err)
	}
	if len(st.failed) != 1 || len(gate.released) != 1 {
		t.Fatalf("first Work failed/released = %v/%v, want one each", st.failed, gate.released)
	}

	// MarkFailed made the message terminal, so the retry cannot claim it. The
	// worker must still settle the orphaned reservation from the durable outcome.
	if err := w.Work(context.Background(), job(j.MessageID, 2)); err != nil {
		t.Fatalf("retry Work: %v", err)
	}
	if len(gate.resolved) != 1 || gate.resolved[0] != j.MessageID {
		t.Fatalf("resolved reservations = %v, want [%s]", gate.resolved, j.MessageID)
	}
}

func TestSendWorker_OutageSnoozesWithoutBurningAttempt(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now() // fresh accept — within the retry horizon
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("dial tcp 1.2.3.4:587: i/o timeout"), Outage: true}}
	w := outboundsend.NewSendWorker(st, dl)
	// Even at a high attempt number, an outage must snooze (not terminal-fail):
	// JobSnooze doesn't count the attempt, so MaxSendAttempts is never reached.
	err := w.Work(context.Background(), job("msg_1", outboundsend.MaxSendAttempts))
	if err == nil {
		t.Fatal("provider outage should snooze (non-nil JobSnooze error)")
	}
	if len(st.failed) != 0 {
		t.Errorf("an outage within the horizon must NOT MarkFailed, got %+v", st.failed)
	}
	if len(st.sent) != 0 {
		t.Errorf("an outage must not MarkSent")
	}
	if len(st.released) != 1 || st.released[0] != "msg_1" {
		t.Errorf("outage snooze must release the active claim, got %v", st.released)
	}
}

func TestSendWorker_OutagePastHorizonFailsTerminally(t *testing.T) {
	j := acceptedJob("msg_1")
	j.AcceptedAt = time.Now().Add(-73 * time.Hour) // past the 72h horizon
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("connection refused"), Outage: true}}
	w := outboundsend.NewSendWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 2)); err == nil {
		t.Fatal("an outage past the retry horizon should fail terminally")
	}
	if len(st.failed) != 1 {
		t.Errorf("an outage past the horizon must MarkFailed, got %+v", st.failed)
	}
	if st.failed[0].source != delivery.FailureSourceLocal {
		t.Errorf("outage-horizon failure source = %q, want local (correctable — the provider never confirmed a rejection)", st.failed[0].source)
	}
}

func TestSendWorker_NextRetryMatchesEnvelope(t *testing.T) {
	w := outboundsend.NewSendWorker(nil, nil)
	want := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 1 * time.Hour, 4 * time.Hour}
	for i, d := range want {
		got := time.Until(w.NextRetry(job("x", i))).Round(time.Second)
		if diff := got - d; diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("attempt %d: next retry in %v, want ~%v", i, got, d)
		}
	}
}
