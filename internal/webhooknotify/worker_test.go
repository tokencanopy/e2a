package webhooknotify_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/identity"
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
	out    webhooknotify.DeliverOutcome
	called int
	kinds  []string
}

func (f *fakeDeliverer) Deliver(_ context.Context, _ *identity.Webhook, kind string) webhooknotify.DeliverOutcome {
	f.called++
	f.kinds = append(f.kinds, kind)
	return f.out
}

func job(webhookID, kind string, attempt int) *river.Job[webhooknotify.WebhookNotifyArgs] {
	return &river.Job[webhooknotify.WebhookNotifyArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: webhooknotify.MaxNotifyAttempts, Kind: webhooknotify.WebhookNotifyArgs{}.Kind()},
		Args:   webhooknotify.WebhookNotifyArgs{WebhookID: webhookID, NotifyKind: kind},
	}
}

func hook(enabled bool, warnedAt *time.Time) *identity.Webhook {
	return &identity.Webhook{
		ID:             "wh_test",
		UserID:         "user_test",
		URL:            "https://hooks.example.com/inbox",
		Enabled:        enabled,
		WarnNotifiedAt: warnedAt,
	}
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
