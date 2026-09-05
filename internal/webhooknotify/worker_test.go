package webhooknotify_test

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

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/webhooknotify"
)

type fakeStore struct {
	wh  *identity.Webhook
	err error
}

func (f *fakeStore) GetWebhookByIDInternal(_ context.Context, _ string) (*identity.Webhook, error) {
	return f.wh, f.err
}

type fakeDeliverer struct {
	out        webhooknotify.DeliverOutcome // Submit's outcome
	composeOut webhooknotify.DeliverOutcome // Compose's outcome
	called     int                          // Submit calls
	composed   int
	kinds      []string
	auths      []sendingpolicy.ProviderAuthorization
	trace      *[]string
}

func (f *fakeDeliverer) Compose(_ context.Context, _ *identity.Webhook, kind string) (outbound.Envelope, webhooknotify.DeliverOutcome) {
	f.composed++
	f.kinds = append(f.kinds, kind)
	f.record("compose")
	if f.composeOut.Err != nil {
		return outbound.Envelope{}, f.composeOut
	}
	return outbound.Envelope{From: "e2a@notify.test", Recipients: []string{"owner@reviewer.test"}, Message: []byte("Subject: x\r\n\r\nbody")}, webhooknotify.DeliverOutcome{}
}

func (f *fakeDeliverer) Submit(_ context.Context, _ outbound.Envelope, auth sendingpolicy.ProviderAuthorization) webhooknotify.DeliverOutcome {
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

func job(webhookID, kind string, attempt int) *river.Job[webhooknotify.WebhookNotifyArgs] {
	return &river.Job[webhooknotify.WebhookNotifyArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: webhooknotify.MaxNotifyAttempts, Kind: webhooknotify.WebhookNotifyArgs{}.Kind()},
		Args:   webhooknotify.WebhookNotifyArgs{WebhookID: webhookID, NotifyKind: kind},
	}
}

// episodeAt is the fixed auto-disable timestamp every disabled fixture
// carries: the breaker stamps it when it flips a webhook, and the operation
// a disable notice authorizes under is keyed by it.
var episodeAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func hook(enabled bool, warnedAt *time.Time) *identity.Webhook {
	wh := &identity.Webhook{
		ID:             "wh_test",
		UserID:         "user_test",
		URL:            "https://hooks.example.com/inbox",
		Enabled:        enabled,
		WarnNotifiedAt: warnedAt,
	}
	if !enabled {
		at := episodeAt
		wh.AutoDisabledAt = &at
	}
	return wh
}

func now() *time.Time { t := time.Now(); return &t }

// notifyRec is one recorded WebhookNotify call: the (kind, outcome) pair
// the e2a_webhook_notify_total series is labelled with.
type notifyRec struct{ kind, outcome string }

// fakeMetrics records WebhookNotify calls for assertion.
type fakeMetrics struct{ calls []notifyRec }

func (f *fakeMetrics) WebhookNotify(kind, outcome string) {
	f.calls = append(f.calls, notifyRec{kind, outcome})
}

// only asserts that exactly one sample was emitted, with the expected
// labels. Every exit from Work must emit exactly one — a branch emitting
// none goes dark, and one emitting twice double-counts.
func (f *fakeMetrics) only(t *testing.T, kind, outcome string) {
	t.Helper()
	want := []notifyRec{{kind, outcome}}
	if len(f.calls) != 1 || f.calls[0] != want[0] {
		t.Errorf("WebhookNotify emissions = %v, want %v", f.calls, want)
	}
}

// The four staleness guards: each drops the notification (nil, no send)
// because the email would be misleading if it went out.
func TestNotifyWorker_Guards(t *testing.T) {
	cases := []struct {
		name string
		st   *fakeStore
		kind string
	}{
		{"webhook deleted", &fakeStore{err: identity.ErrWebhookNotFound}, webhooknotify.KindDisabled},
		{"disabled kind but re-enabled since", &fakeStore{wh: hook(true, nil)}, webhooknotify.KindDisabled},
		{"warning kind but disabled since", &fakeStore{wh: hook(false, now())}, webhooknotify.KindWarning},
		{"warning kind but recovered (marker cleared)", &fakeStore{wh: hook(true, nil)}, webhooknotify.KindWarning},
		{"unknown kind fails closed", &fakeStore{wh: hook(true, now())}, "surprise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fd := &fakeDeliverer{}
			fm := &fakeMetrics{}
			w := webhooknotify.NewNotifyWorker(tc.st, fd).WithMetrics(fm)
			if err := w.Work(context.Background(), job("wh_test", tc.kind, 1)); err != nil {
				t.Fatalf("Work: %v (guards must be silent no-ops)", err)
			}
			if fd.called != 0 {
				t.Errorf("deliverer called %d times, want 0", fd.called)
			}
			// A guard is "we decided not to send", not "we failed to send":
			// counted as skipped so a fall in sends stays unambiguous.
			// (An unrecognized kind reaches the label verbatim here; the
			// Prometheus backend's enum allowlist collapses it to "other".)
			fm.only(t, tc.kind, "skipped")
		})
	}
}

func TestNotifyWorker_DeliversWarning(t *testing.T) {
	fd := &fakeDeliverer{}
	fm := &fakeMetrics{}
	w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(true, now())}, fd).WithMetrics(fm)
	if err := w.Work(context.Background(), job("wh_test", webhooknotify.KindWarning, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if fd.called != 1 || fd.kinds[0] != webhooknotify.KindWarning {
		t.Errorf("deliverer calls = %d kinds = %v, want one warning", fd.called, fd.kinds)
	}
	fm.only(t, webhooknotify.KindWarning, "sent")
}

// An unwired metrics backend must never panic — the worker is constructed
// without one in tests and on any build that leaves telemetry off.
func TestNotifyWorker_NilMetricsDoesNotPanic(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *fakeStore
		out  webhooknotify.DeliverOutcome
	}{
		{"sent", &fakeStore{wh: hook(false, nil)}, webhooknotify.DeliverOutcome{}},
		{"permanent", &fakeStore{wh: hook(false, nil)}, webhooknotify.DeliverOutcome{Err: errors.New("nope"), Permanent: true}},
		{"outage", &fakeStore{wh: hook(false, nil)}, webhooknotify.DeliverOutcome{Err: errors.New("nope"), Outage: true}},
		{"retryable", &fakeStore{wh: hook(false, nil)}, webhooknotify.DeliverOutcome{Err: errors.New("nope")}},
		{"skipped", &fakeStore{err: identity.ErrWebhookNotFound}, webhooknotify.DeliverOutcome{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both shapes of "unwired": never called, and called with a
			// nil interface value.
			for _, w := range []*webhooknotify.NotifyWorker{
				webhooknotify.NewNotifyWorker(tc.st, &fakeDeliverer{out: tc.out}),
				webhooknotify.NewNotifyWorker(tc.st, &fakeDeliverer{out: tc.out}).WithMetrics(nil),
			} {
				_ = w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1))
			}
		})
	}
}

func TestNotifyWorker_DeliversDisabled(t *testing.T) {
	fd := &fakeDeliverer{}
	fm := &fakeMetrics{}
	w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(fm)
	if err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if fd.called != 1 || fd.kinds[0] != webhooknotify.KindDisabled {
		t.Errorf("deliverer calls = %d kinds = %v, want one disabled", fd.called, fd.kinds)
	}
	fm.only(t, webhooknotify.KindDisabled, "sent")
}

func TestNotifyWorker_StoreErrorIsRetryable(t *testing.T) {
	fd := &fakeDeliverer{}
	fm := &fakeMetrics{}
	w := webhooknotify.NewNotifyWorker(&fakeStore{err: errors.New("db down")}, fd).WithMetrics(fm)
	err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1))
	if err == nil {
		t.Fatal("a DB error must be returned (retryable), got nil")
	}
	if fd.called != 0 {
		t.Errorf("deliverer called on a failed load")
	}
	fm.only(t, webhooknotify.KindDisabled, "retryable")
}

// The three error-triage branches mirror hitlnotify exactly.
func TestNotifyWorker_ErrorTriage(t *testing.T) {
	base := errors.New("smtp said no")

	t.Run("permanent cancels", func(t *testing.T) {
		fd := &fakeDeliverer{out: webhooknotify.DeliverOutcome{Err: base, Permanent: true}}
		fm := &fakeMetrics{}
		w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(fm)
		err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1))
		if err == nil {
			t.Fatal("expected a cancel error")
		}
		if !strings.Contains(err.Error(), "smtp said no") {
			t.Errorf("cancel error should carry the cause, got: %v", err)
		}
		var cancelErr *river.JobCancelError
		if !errors.As(err, &cancelErr) {
			t.Errorf("permanent failure must be a river JobCancel, got %T: %v", err, err)
		}
		// The whole point of the counter: a cancelled send is otherwise
		// log-only, so the notifier can die silently in production.
		fm.only(t, webhooknotify.KindDisabled, "permanent")
	})

	t.Run("outage snoozes", func(t *testing.T) {
		fd := &fakeDeliverer{out: webhooknotify.DeliverOutcome{Err: base, Outage: true}}
		fm := &fakeMetrics{}
		w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(fm)
		err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1))
		if err == nil {
			t.Fatal("expected a snooze error")
		}
		var snoozeErr *river.JobSnoozeError
		if !errors.As(err, &snoozeErr) {
			t.Errorf("outage must snooze, got %T: %v", err, err)
		}
		fm.only(t, webhooknotify.KindDisabled, "outage")
	})

	t.Run("transient retries", func(t *testing.T) {
		fd := &fakeDeliverer{out: webhooknotify.DeliverOutcome{Err: base}}
		fm := &fakeMetrics{}
		w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(fm)
		err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1))
		if err == nil {
			t.Fatal("transient failure must return an error so River retries")
		}
		var cancelErr *river.JobCancelError
		var snoozeErr *river.JobSnoozeError
		if errors.As(err, &cancelErr) || errors.As(err, &snoozeErr) {
			t.Errorf("transient failure must be a plain error, got %T: %v", err, err)
		}
		fm.only(t, webhooknotify.KindDisabled, "retryable")
	})
}

// fakeGate is a scriptable sendingpolicy.Gate for the worker-order tests.
type fakeGate struct {
	trace      *[]string
	reserve    sendingpolicy.Decision
	consume    sendingpolicy.Decision
	reserveErr error
	reserves   int
	consumes   int
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

// gatedJob carries the operation a notice of this kind for the disabled
// fixture (hook(false, …)) is keyed by; a warning fixture passes its own
// webhook through gatedJobFor.
func gatedJob(webhookID, kind string, attempt int) *river.Job[webhooknotify.WebhookNotifyArgs] {
	wh := hook(false, nil)
	wh.ID = webhookID
	if kind == webhooknotify.KindWarning {
		wh.Enabled = true
		wh.WarnNotifiedAt = now()
	}
	return gatedJobFor(wh, kind, attempt)
}

func gatedJobFor(wh *identity.Webhook, kind string, attempt int) *river.Job[webhooknotify.WebhookNotifyArgs] {
	j := job(wh.ID, kind, attempt)
	ref := refFor(webhooknotify.ExpectedOperationID(wh, kind))
	j.Args.OperationRef = &ref
	return j
}

func isSnooze(err error) bool {
	var snooze *river.JobSnoozeError
	return errors.As(err, &snooze)
}

func TestNotifyWorker_GatedPathAuthorizesThenDelivers(t *testing.T) {
	fd := &fakeDeliverer{}
	fm := &fakeMetrics{}
	g := allowAll()
	w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(fm).WithGate(g)
	if err := w.Work(context.Background(), gatedJob("wh_test", webhooknotify.KindDisabled, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if g.reserves != 1 || g.consumes != 1 || fd.called != 1 {
		t.Fatalf("reserves=%d consumes=%d delivers=%d, want 1/1/1", g.reserves, g.consumes, fd.called)
	}
}

func TestNotifyWorker_GateHoldSnoozesWithoutDelivery(t *testing.T) {
	for name, g := range map[string]*fakeGate{
		"early hold": {reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonAccountPaused}},
		"late hold":  {reserve: sendingpolicy.Decision{Allow: true}, consume: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonGlobalAllBudget, RetryAt: time.Now().Add(time.Hour)}},
		"gate error": {reserveErr: errors.New("policy db down")},
	} {
		fd := &fakeDeliverer{}
		w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(&fakeMetrics{}).WithGate(g)
		if err := w.Work(context.Background(), gatedJob("wh_test", webhooknotify.KindDisabled, 1)); !isSnooze(err) || fd.called != 0 {
			t.Fatalf("%s: err=%v delivers=%d, want snooze with no I/O", name, err, fd.called)
		}
	}
}

func TestNotifyWorker_LegacyJobResolvesAndStampsOnce(t *testing.T) {
	fd := &fakeDeliverer{}
	resolved, stamped := 0, 0
	w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(&fakeMetrics{}).WithGate(allowAll()).
		WithOperationResolver(func(_ context.Context, id, kind string) (sendingpolicy.OperationRef, error) {
			resolved++
			wh := hook(false, nil)
			wh.ID = id
			return refFor(webhooknotify.ExpectedOperationID(wh, kind)), nil
		}).
		WithArgStamper(func(context.Context, int64, sendingpolicy.OperationRef) error { stamped++; return nil })
	if err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if resolved != 1 || stamped != 1 || fd.called != 1 {
		t.Fatalf("resolved=%d stamped=%d delivers=%d, want 1/1/1", resolved, stamped, fd.called)
	}
}

func (g *fakeGate) record(step string) {
	if g.trace != nil {
		*g.trace = append(*g.trace, step)
	}
}

// TestNotifyWorker_ComposeRunsBeforeAnyChargeAndConsumeIsLast pins the order
// the seam depends on: compose precedes Reserve, ConsumeAttempt is the last
// call before Submit.
func TestNotifyWorker_ComposeRunsBeforeAnyChargeAndConsumeIsLast(t *testing.T) {
	var trace []string
	fd := &fakeDeliverer{trace: &trace}
	g := allowAll()
	g.trace = &trace
	w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(&fakeMetrics{}).WithGate(g)
	if err := w.Work(context.Background(), gatedJob("wh_test", webhooknotify.KindDisabled, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := strings.Join(trace, ","); got != "compose,reserve,consume,submit" {
		t.Fatalf("order = %s, want compose,reserve,consume,submit", got)
	}
}

// TestNotifyWorker_ComposeFailureChargesNothing: a compose failure precedes
// Reserve, so it burns no ordinal.
func TestNotifyWorker_ComposeFailureChargesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		out     webhooknotify.DeliverOutcome
		wantErr func(error) bool
	}{
		"transient": {out: webhooknotify.DeliverOutcome{Err: errors.New("stats blip")}, wantErr: func(err error) bool { return err != nil && !isSnooze(err) && !isCancel(err) }},
		"permanent": {out: webhooknotify.DeliverOutcome{Err: errors.New("no owner email"), Permanent: true}, wantErr: isCancel},
		"outage":    {out: webhooknotify.DeliverOutcome{Err: errors.New("dkim store down"), Outage: true}, wantErr: isSnooze},
	} {
		fd := &fakeDeliverer{composeOut: tc.out}
		g := allowAll()
		w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(&fakeMetrics{}).WithGate(g)
		err := w.Work(context.Background(), gatedJob("wh_test", webhooknotify.KindDisabled, 1))
		if !tc.wantErr(err) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if g.reserves != 0 || g.consumes != 0 || fd.called != 0 {
			t.Fatalf("%s: reserves=%d consumes=%d submits=%d, want 0/0/0", name, g.reserves, g.consumes, fd.called)
		}
	}
}

// TestNotifyWorker_ForeignOrStaleOperationReferenceIsCancelled: a reference
// naming another webhook's operation, or a superseded episode of this one,
// is cancelled before Reserve.
func TestNotifyWorker_ForeignOrStaleOperationReferenceIsCancelled(t *testing.T) {
	other := hook(false, nil)
	other.ID = "wh_other"
	stale := hook(false, nil)
	at := episodeAt.Add(-time.Hour)
	stale.AutoDisabledAt = &at
	for name, ref := range map[string]sendingpolicy.OperationRef{
		"foreign webhook": refFor(webhooknotify.ExpectedOperationID(other, webhooknotify.KindDisabled)),
		"stale episode":   refFor(webhooknotify.ExpectedOperationID(stale, webhooknotify.KindDisabled)),
	} {
		fd := &fakeDeliverer{}
		g := allowAll()
		w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(&fakeMetrics{}).WithGate(g)
		j := job("wh_test", webhooknotify.KindDisabled, 1)
		r := ref
		j.Args.OperationRef = &r
		if err := w.Work(context.Background(), j); !isCancel(err) {
			t.Fatalf("%s: err = %v, want cancel", name, err)
		}
		if g.reserves != 0 || fd.called != 0 {
			t.Fatalf("%s: reserves=%d submits=%d, want 0/0", name, g.reserves, fd.called)
		}
	}
}

// TestNotifyWorker_StaleNoticeIsDropped: a notice older than the age bound
// is dropped instead of snoozing forever behind a hold.
func TestNotifyWorker_StaleNoticeIsDropped(t *testing.T) {
	fd := &fakeDeliverer{}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonAccountPaused}}
	w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(&fakeMetrics{}).WithGate(g)
	j := gatedJob("wh_test", webhooknotify.KindDisabled, 1)
	j.CreatedAt = time.Now().Add(-8 * 24 * time.Hour)
	if err := w.Work(context.Background(), j); err != nil {
		t.Fatalf("err = %v, want a silent drop", err)
	}
	if g.reserves != 0 || fd.composed != 0 || fd.called != 0 {
		t.Fatalf("reserves=%d composes=%d submits=%d, want 0/0/0", g.reserves, fd.composed, fd.called)
	}
}

// TestKindVocabularyMatchesGate: the job's kinds are the gate's episode kinds.
func TestKindVocabularyMatchesGate(t *testing.T) {
	if webhooknotify.KindWarning != sendingpolicy.WebhookHealthKindWarning || webhooknotify.KindDisabled != sendingpolicy.WebhookHealthKindDisabled {
		t.Fatal("webhooknotify kinds and sendingpolicy webhook health kinds disagree")
	}
}

func isCancel(err error) bool {
	var cancel *river.JobCancelError
	return errors.As(err, &cancel)
}

// TestNotifyWorker_PreDerivationReferenceIsReKeyed: a job stamped before the
// episode-derived ids existed (migration 113's op_<md5>) is re-resolved and
// its reference replaced, not cancelled.
func TestNotifyWorker_PreDerivationReferenceIsReKeyed(t *testing.T) {
	fd := &fakeDeliverer{}
	g := allowAll()
	resolved, stamped, restamped := 0, 0, 0
	var restampedWith string
	w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd).WithMetrics(&fakeMetrics{}).WithGate(g).
		WithOperationResolver(func(_ context.Context, id, kind string) (sendingpolicy.OperationRef, error) {
			resolved++
			wh := hook(false, nil)
			wh.ID = id
			return refFor(webhooknotify.ExpectedOperationID(wh, kind)), nil
		}).
		WithArgStamper(func(context.Context, int64, sendingpolicy.OperationRef) error { stamped++; return nil }).
		WithArgRestamper(func(_ context.Context, _ int64, ref sendingpolicy.OperationRef) error {
			restamped++
			restampedWith = ref.ID()
			return nil
		})
	j := job("wh_test", webhooknotify.KindDisabled, 1)
	legacy := refFor("op_0123456789abcdef0123456789abcdef")
	j.Args.OperationRef = &legacy
	if err := w.Work(context.Background(), j); err != nil {
		t.Fatalf("Work: %v", err)
	}
	want := webhooknotify.ExpectedOperationID(hook(false, nil), webhooknotify.KindDisabled)
	if resolved != 1 || restamped != 1 || stamped != 0 || restampedWith != want {
		t.Fatalf("resolved=%d restamped=%d stamped=%d with=%q, want 1/1/0 with %q", resolved, restamped, stamped, restampedWith, want)
	}
	if g.reserves != 1 || fd.called != 1 {
		t.Fatalf("reserves=%d submits=%d, want 1/1", g.reserves, fd.called)
	}
}
