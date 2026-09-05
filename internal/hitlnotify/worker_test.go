package hitlnotify_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/hitlnotify"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

type fakeStore struct {
	pn      *identity.PendingNotify
	loadErr error
	markErr error

	notified []string
}

func (f *fakeStore) LoadPendingNotify(_ context.Context, _ string) (*identity.PendingNotify, error) {
	return f.pn, f.loadErr
}
func (f *fakeStore) MarkMessageNotified(_ context.Context, id string) error {
	f.notified = append(f.notified, id)
	return f.markErr
}
func (f *fakeStore) StampNotifyJobIDTx(_ context.Context, _ pgx.Tx, _ string, _ int64) error {
	return nil
}

type fakeDeliverer struct {
	out        hitlnotify.DeliverOutcome // Submit's outcome
	composeOut hitlnotify.DeliverOutcome // Compose's outcome
	called     int                       // Submit calls
	composed   int
	auths      []sendingpolicy.ProviderAuthorization
	trace      *[]string // shared with fakeGate to pin ordering
}

func (f *fakeDeliverer) Compose(_ context.Context, _ *identity.PendingNotify) (outbound.Envelope, hitlnotify.DeliverOutcome) {
	f.composed++
	f.record("compose")
	if f.composeOut.Err != nil {
		return outbound.Envelope{}, f.composeOut
	}
	return outbound.Envelope{From: "e2a@notify.test", Recipients: []string{"owner@reviewer.test"}, Message: []byte("Subject: x\r\n\r\nbody")}, hitlnotify.DeliverOutcome{}
}

func (f *fakeDeliverer) Submit(_ context.Context, _ outbound.Envelope, auth sendingpolicy.ProviderAuthorization) hitlnotify.DeliverOutcome {
	f.called++
	f.record("submit")
	f.auths = append(f.auths, auth)
	return f.out
}

func (f *fakeDeliverer) record(step string) {
	if f.trace != nil {
		*f.trace = append(*f.trace, step)
	}
}

func job(id string, attempt int) *river.Job[hitlnotify.HITLNotifyArgs] {
	return &river.Job[hitlnotify.HITLNotifyArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: hitlnotify.MaxNotifyAttempts, Kind: hitlnotify.HITLNotifyArgs{}.Kind()},
		Args:   hitlnotify.HITLNotifyArgs{MessageID: id},
	}
}

// pending builds a live pending_review PendingNotify (TTL in the future).
func pending(id string) *identity.PendingNotify {
	exp := time.Now().Add(1 * time.Hour)
	return &identity.PendingNotify{
		Message: &identity.Message{ID: id, Status: identity.MessageStatusPendingReview, ApprovalExpiresAt: &exp},
		Agent:   &identity.AgentIdentity{ID: "agent@x.test"},
	}
}

func TestNotifyWorker_Success(t *testing.T) {
	st := &fakeStore{pn: pending("msg_1")}
	dl := &fakeDeliverer{}
	w := hitlnotify.NewNotifyWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.called != 1 {
		t.Errorf("Deliver called %d times, want 1", dl.called)
	}
	if len(st.notified) != 1 || st.notified[0] != "msg_1" {
		t.Errorf("MarkMessageNotified = %v, want [msg_1]", st.notified)
	}
}

func TestNotifyWorker_MessageGoneIsNoOp(t *testing.T) {
	st := &fakeStore{pn: nil} // LoadPendingNotify returns (nil, nil) → gone
	dl := &fakeDeliverer{}
	w := hitlnotify.NewNotifyWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_gone", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.called != 0 || len(st.notified) != 0 {
		t.Errorf("gone message must be a no-op (deliver=%d notified=%v)", dl.called, st.notified)
	}
}

func TestNotifyWorker_SuppressedAgentIsNoOp(t *testing.T) {
	pn := pending("msg_suppressed")
	pn.Agent.SuppressNotifications = true
	st := &fakeStore{pn: pn}
	dl := &fakeDeliverer{}
	w := hitlnotify.NewNotifyWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_suppressed", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.called != 0 || len(st.notified) != 0 {
		t.Errorf("suppressed agent must be a no-op (deliver=%d notified=%v)", dl.called, st.notified)
	}
}

func TestNotifyWorker_ResolvedIsNoOp(t *testing.T) {
	pn := pending("msg_1")
	pn.Message.Status = identity.MessageStatusSent // approved/resolved before we notified
	st := &fakeStore{pn: pn}
	dl := &fakeDeliverer{}
	w := hitlnotify.NewNotifyWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.called != 0 {
		t.Errorf("a resolved hold must not send, Deliver called %d", dl.called)
	}
}

func TestNotifyWorker_ExpiredHoldIsNoOp(t *testing.T) {
	pn := pending("msg_1")
	past := time.Now().Add(-1 * time.Minute)
	pn.Message.ApprovalExpiresAt = &past
	st := &fakeStore{pn: pn}
	dl := &fakeDeliverer{}
	w := hitlnotify.NewNotifyWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.called != 0 {
		t.Errorf("an expired hold must not send, Deliver called %d", dl.called)
	}
}

func TestNotifyWorker_AlreadyNotifiedIsNoOp(t *testing.T) {
	pn := pending("msg_1")
	pn.Notified = true // notified_at already set (crash-after-send re-drive)
	st := &fakeStore{pn: pn}
	dl := &fakeDeliverer{}
	w := hitlnotify.NewNotifyWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.called != 0 {
		t.Errorf("an already-notified hold must not re-send, Deliver called %d", dl.called)
	}
}

func TestNotifyWorker_MarkFailedAfterSendDoesNotRetry(t *testing.T) {
	// The email is already out; only the dedup marker failed. Returning an error
	// would re-send on retry, so Work must swallow it and complete.
	st := &fakeStore{pn: pending("msg_1"), markErr: errors.New("db blip")}
	dl := &fakeDeliverer{}
	w := hitlnotify.NewNotifyWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 1)); err != nil {
		t.Fatalf("mark-notified failure after a successful send must not error, got %v", err)
	}
}

func TestNotifyWorker_PermanentCancels(t *testing.T) {
	st := &fakeStore{pn: pending("msg_1")}
	dl := &fakeDeliverer{out: hitlnotify.DeliverOutcome{Err: errors.New("550 user unknown"), Permanent: true}}
	w := hitlnotify.NewNotifyWorker(st, dl)
	err := w.Work(context.Background(), job("msg_1", 1))
	if err == nil {
		t.Fatal("a permanent failure should return a (cancel) error")
	}
	if len(st.notified) != 0 {
		t.Errorf("a failed send must not mark notified, got %v", st.notified)
	}
}

func TestNotifyWorker_OutageSnoozes(t *testing.T) {
	st := &fakeStore{pn: pending("msg_1")}
	dl := &fakeDeliverer{out: hitlnotify.DeliverOutcome{Err: errors.New("connection refused"), Outage: true}}
	w := hitlnotify.NewNotifyWorker(st, dl)
	// Even at a high attempt number an outage must snooze (JobSnooze doesn't burn
	// an attempt), never terminal-fail.
	err := w.Work(context.Background(), job("msg_1", hitlnotify.MaxNotifyAttempts))
	if err == nil {
		t.Fatal("a relay outage should snooze (non-nil JobSnooze error)")
	}
	if len(st.notified) != 0 {
		t.Errorf("an outage must not mark notified, got %v", st.notified)
	}
}

func TestNotifyWorker_TransientRetries(t *testing.T) {
	st := &fakeStore{pn: pending("msg_1")}
	dl := &fakeDeliverer{out: hitlnotify.DeliverOutcome{Err: errors.New("owner lookup blip")}}
	w := hitlnotify.NewNotifyWorker(st, dl)
	if err := w.Work(context.Background(), job("msg_1", 1)); err == nil {
		t.Fatal("a transient failure must return an error so River retries")
	}
	if len(st.notified) != 0 {
		t.Errorf("a failed send must not mark notified, got %v", st.notified)
	}
}

func TestNotifyWorker_LoadErrorRetries(t *testing.T) {
	st := &fakeStore{loadErr: errors.New("db down")}
	w := hitlnotify.NewNotifyWorker(st, &fakeDeliverer{})
	if err := w.Work(context.Background(), job("msg_1", 1)); err == nil {
		t.Fatal("a load error must propagate so River retries")
	}
}

func TestNotifyWorker_NextRetryMatchesEnvelope(t *testing.T) {
	w := hitlnotify.NewNotifyWorker(nil, nil)
	want := []time.Duration{15 * time.Second, 1 * time.Minute, 5 * time.Minute, 15 * time.Minute, 1 * time.Hour}
	for i, d := range want {
		got := time.Until(w.NextRetry(job("x", i))).Round(time.Second)
		if diff := got - d; diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("attempt %d: next retry in %v, want ~%v", i, got, d)
		}
	}
}

// fakeGate is a scriptable sendingpolicy.Gate for the worker-order tests.
type fakeGate struct {
	trace      *[]string
	reserve    sendingpolicy.Decision
	consume    sendingpolicy.Decision
	reserves   int
	consumes   int
	reserveErr error
}

func allowAll() *fakeGate {
	return &fakeGate{reserve: sendingpolicy.Decision{Allow: true}, consume: sendingpolicy.Decision{Allow: true}}
}

func (g *fakeGate) PrepareExternalTx(context.Context, pgx.Tx, string) (sendingpolicy.AcceptanceDecision, sendingpolicy.OperationRef, error) {
	return sendingpolicy.AcceptanceAccept, sendingpolicy.OperationRef{}, nil
}
func (g *fakeGate) PrepareNotificationTx(context.Context, pgx.Tx, sendingpolicy.NotificationRef) (sendingpolicy.OperationRef, error) {
	return refFor("op_prepared"), nil
}
func (g *fakeGate) PrepareProtectionNoticeTx(context.Context, pgx.Tx, sendingpolicy.ProtectionNoticeRef) (sendingpolicy.OperationRef, error) {
	return sendingpolicy.OperationRef{}, nil
}
func (g *fakeGate) PreparePublicFeedback(context.Context, sendingpolicy.PublicFeedbackRef) (sendingpolicy.OperationRef, error) {
	return sendingpolicy.OperationRef{}, nil
}
func (g *fakeGate) Reserve(context.Context, sendingpolicy.OperationRef) (sendingpolicy.Decision, sendingpolicy.AttemptRef, error) {
	g.reserves++
	g.record("reserve")
	return g.reserve, sendingpolicy.AttemptRef{}, g.reserveErr
}
func (g *fakeGate) ConsumeAttempt(context.Context, sendingpolicy.AttemptRef) (sendingpolicy.Decision, *sendingpolicy.ProviderAuthorization, error) {
	g.consumes++
	g.record("consume")
	if !g.consume.Allow {
		return g.consume, nil, nil
	}
	return g.consume, &sendingpolicy.ProviderAuthorization{}, nil
}
func (g *fakeGate) RedeemProviderCall(context.Context, sendingpolicy.ProviderAuthorization) error {
	return nil
}
func (g *fakeGate) DeferAttempt(context.Context, sendingpolicy.AttemptRef) error  { return nil }
func (g *fakeGate) CancelAttempt(context.Context, sendingpolicy.AttemptRef) error { return nil }
func (g *fakeGate) SettleProvider(context.Context, sendingpolicy.ProviderSettlement) error {
	return nil
}
func (g *fakeGate) SettleOperation(context.Context, sendingpolicy.OperationRef, sendingpolicy.SettlementOutcome, string) error {
	return nil
}
func (g *fakeGate) LookupOperation(_ context.Context, id string) (sendingpolicy.OperationRef, error) {
	return refFor(id), nil
}

func refFor(id string) sendingpolicy.OperationRef {
	var ref sendingpolicy.OperationRef
	if err := json.Unmarshal([]byte(`{"v":1,"id":"`+id+`"}`), &ref); err != nil {
		panic(err)
	}
	return ref
}

func gatedJob(id string, attempt int) *river.Job[hitlnotify.HITLNotifyArgs] {
	j := job(id, attempt)
	ref := refFor(sendingpolicy.HITLNotificationOperationID(id))
	j.Args.OperationRef = &ref
	return j
}

func isSnooze(err error) bool {
	var snooze *river.JobSnoozeError
	return errors.As(err, &snooze)
}

func isCancel(err error) bool {
	var cancel *river.JobCancelError
	return errors.As(err, &cancel)
}

func TestNotifyWorker_GatedPathAuthorizesThenDelivers(t *testing.T) {
	st := &fakeStore{pn: pending("msg_gated")}
	dl := &fakeDeliverer{}
	g := allowAll()
	if err := hitlnotify.NewNotifyWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_gated", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if g.reserves != 1 || g.consumes != 1 || dl.called != 1 || len(st.notified) != 1 {
		t.Fatalf("reserves=%d consumes=%d delivers=%d notified=%d, want 1/1/1/1", g.reserves, g.consumes, dl.called, len(st.notified))
	}
}

func TestNotifyWorker_GateHoldSnoozesWithoutDelivery(t *testing.T) {
	for name, g := range map[string]*fakeGate{
		"early hold": {reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonAccountPaused}},
		"late hold":  {reserve: sendingpolicy.Decision{Allow: true}, consume: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonAccountSharedBudget, RetryAt: time.Now().Add(2 * time.Hour)}},
		"gate error": {reserveErr: errors.New("policy db down")},
	} {
		st := &fakeStore{pn: pending("msg_hold")}
		dl := &fakeDeliverer{}
		err := hitlnotify.NewNotifyWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_hold", 1))
		if !isSnooze(err) || dl.called != 0 || len(st.notified) != 0 {
			t.Fatalf("%s: err=%v delivers=%d notified=%d, want snooze with no I/O", name, err, dl.called, len(st.notified))
		}
	}
}

func TestNotifyWorker_TerminalHoldCancels(t *testing.T) {
	st := &fakeStore{pn: pending("msg_terminal")}
	dl := &fakeDeliverer{}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonAccountDeleted, Terminal: true}}
	if err := hitlnotify.NewNotifyWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_terminal", 1)); !isCancel(err) || dl.called != 0 {
		t.Fatalf("err=%v delivers=%d, want cancel with no I/O", err, dl.called)
	}
}

func TestNotifyWorker_LegacyJobResolvesAndStampsOnce(t *testing.T) {
	st := &fakeStore{pn: pending("msg_legacy")}
	dl := &fakeDeliverer{}
	resolved, stamped := 0, 0
	w := hitlnotify.NewNotifyWorker(st, dl).WithGate(allowAll()).
		WithOperationResolver(func(_ context.Context, id string) (sendingpolicy.OperationRef, error) {
			resolved++
			return refFor(sendingpolicy.HITLNotificationOperationID(id)), nil
		}).
		WithArgStamper(func(_ context.Context, _ int64, _ sendingpolicy.OperationRef) error { stamped++; return nil })
	if err := w.Work(context.Background(), job("msg_legacy", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if resolved != 1 || stamped != 1 || dl.called != 1 {
		t.Fatalf("resolved=%d stamped=%d delivers=%d, want 1/1/1", resolved, stamped, dl.called)
	}
	// A legacy job whose source is gone is a no-op, never a retry loop.
	w = hitlnotify.NewNotifyWorker(&fakeStore{pn: pending("msg_gone")}, dl).WithGate(allowAll()).
		WithOperationResolver(func(context.Context, string) (sendingpolicy.OperationRef, error) {
			return sendingpolicy.OperationRef{}, sendingpolicy.ErrSourceUnavailable
		})
	if err := w.Work(context.Background(), job("msg_gone", 1)); err != nil || dl.called != 1 {
		t.Fatalf("orphan legacy: err=%v delivers=%d, want nil and no new delivery", err, dl.called)
	}
}

func (g *fakeGate) record(step string) {
	if g.trace != nil {
		*g.trace = append(*g.trace, step)
	}
}

// TestNotifyWorker_ComposeRunsBeforeAnyChargeAndConsumeIsLast pins the order
// the seam depends on: compose (every fallible, provider-free step) precedes
// Reserve, and ConsumeAttempt is the last call before Submit.
func TestNotifyWorker_ComposeRunsBeforeAnyChargeAndConsumeIsLast(t *testing.T) {
	var trace []string
	fd := &fakeDeliverer{trace: &trace}
	g := allowAll()
	g.trace = &trace
	st := &fakeStore{pn: pending("msg_1")}
	w := hitlnotify.NewNotifyWorker(st, fd).WithGate(g)
	if err := w.Work(context.Background(), gatedJob("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := strings.Join(trace, ","); got != "compose,reserve,consume,submit" {
		t.Fatalf("order = %s, want compose,reserve,consume,submit", got)
	}
}

// TestNotifyWorker_ComposeFailureChargesNothing: a compose failure (owner
// lookup, signing, MIME) happens before Reserve, so it burns no ordinal; it
// is classified exactly like a send failure.
func TestNotifyWorker_ComposeFailureChargesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		out       hitlnotify.DeliverOutcome
		wantErr   func(error) bool
		wantMsgID bool
	}{
		"transient": {out: hitlnotify.DeliverOutcome{Err: errors.New("owner lookup blip")}, wantErr: func(err error) bool { return err != nil && !isCancel(err) && !isSnooze(err) }},
		"permanent": {out: hitlnotify.DeliverOutcome{Err: errors.New("no owner email"), Permanent: true}, wantErr: isCancel},
		"outage":    {out: hitlnotify.DeliverOutcome{Err: errors.New("dkim store down"), Outage: true}, wantErr: isSnooze},
	} {
		fd := &fakeDeliverer{composeOut: tc.out}
		g := allowAll()
		st := &fakeStore{pn: pending("msg_1")}
		w := hitlnotify.NewNotifyWorker(st, fd).WithGate(g)
		err := w.Work(context.Background(), gatedJob("msg_1", 1))
		if !tc.wantErr(err) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if g.reserves != 0 || g.consumes != 0 || fd.called != 0 {
			t.Fatalf("%s: reserves=%d consumes=%d submits=%d, want 0/0/0", name, g.reserves, g.consumes, fd.called)
		}
		if len(st.notified) != 0 {
			t.Fatalf("%s: marked notified without a send", name)
		}
	}
}

// TestNotifyWorker_ForeignOperationReferenceIsCancelled: a job whose
// reference names another message's operation would charge that operation's
// account; it is cancelled before Reserve, never retried.
func TestNotifyWorker_ForeignOperationReferenceIsCancelled(t *testing.T) {
	fd := &fakeDeliverer{}
	g := allowAll()
	st := &fakeStore{pn: pending("msg_1")}
	w := hitlnotify.NewNotifyWorker(st, fd).WithGate(g)
	j := job("msg_1", 1)
	ref := refFor(sendingpolicy.HITLNotificationOperationID("msg_other"))
	j.Args.OperationRef = &ref
	if err := w.Work(context.Background(), j); !isCancel(err) {
		t.Fatalf("err = %v, want cancel", err)
	}
	if g.reserves != 0 || fd.called != 0 {
		t.Fatalf("reserves=%d submits=%d, want 0/0", g.reserves, fd.called)
	}

	// The same binding applies to a legacy resolve that returns a foreign id.
	fd, g = &fakeDeliverer{}, allowAll()
	w = hitlnotify.NewNotifyWorker(&fakeStore{pn: pending("msg_1")}, fd).WithGate(g).
		WithOperationResolver(func(context.Context, string) (sendingpolicy.OperationRef, error) {
			return refFor(sendingpolicy.HITLNotificationOperationID("msg_other")), nil
		})
	if err := w.Work(context.Background(), job("msg_1", 1)); !isCancel(err) {
		t.Fatalf("legacy: err = %v, want cancel", err)
	}
	if g.reserves != 0 || fd.called != 0 {
		t.Fatalf("legacy: reserves=%d submits=%d, want 0/0", g.reserves, fd.called)
	}
}
