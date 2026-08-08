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
			w := webhooknotify.NewNotifyWorker(tc.st, fd)
			if err := w.Work(context.Background(), job("wh_test", tc.kind, 1)); err != nil {
				t.Fatalf("Work: %v (guards must be silent no-ops)", err)
			}
			if fd.called != 0 {
				t.Errorf("deliverer called %d times, want 0", fd.called)
			}
		})
	}
}

func TestNotifyWorker_DeliversWarning(t *testing.T) {
	fd := &fakeDeliverer{}
	w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(true, now())}, fd)
	if err := w.Work(context.Background(), job("wh_test", webhooknotify.KindWarning, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if fd.called != 1 || fd.kinds[0] != webhooknotify.KindWarning {
		t.Errorf("deliverer calls = %d kinds = %v, want one warning", fd.called, fd.kinds)
	}
}

func TestNotifyWorker_DeliversDisabled(t *testing.T) {
	fd := &fakeDeliverer{}
	w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd)
	if err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if fd.called != 1 || fd.kinds[0] != webhooknotify.KindDisabled {
		t.Errorf("deliverer calls = %d kinds = %v, want one disabled", fd.called, fd.kinds)
	}
}

func TestNotifyWorker_StoreErrorIsRetryable(t *testing.T) {
	fd := &fakeDeliverer{}
	w := webhooknotify.NewNotifyWorker(&fakeStore{err: errors.New("db down")}, fd)
	err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1))
	if err == nil {
		t.Fatal("a DB error must be returned (retryable), got nil")
	}
	if fd.called != 0 {
		t.Errorf("deliverer called on a failed load")
	}
}

// The three error-triage branches mirror hitlnotify exactly.
func TestNotifyWorker_ErrorTriage(t *testing.T) {
	base := errors.New("smtp said no")

	t.Run("permanent cancels", func(t *testing.T) {
		fd := &fakeDeliverer{out: webhooknotify.DeliverOutcome{Err: base, Permanent: true}}
		w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd)
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
	})

	t.Run("outage snoozes", func(t *testing.T) {
		fd := &fakeDeliverer{out: webhooknotify.DeliverOutcome{Err: base, Outage: true}}
		w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd)
		err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1))
		if err == nil {
			t.Fatal("expected a snooze error")
		}
		var snoozeErr *river.JobSnoozeError
		if !errors.As(err, &snoozeErr) {
			t.Errorf("outage must snooze, got %T: %v", err, err)
		}
	})

	t.Run("transient retries", func(t *testing.T) {
		fd := &fakeDeliverer{out: webhooknotify.DeliverOutcome{Err: base}}
		w := webhooknotify.NewNotifyWorker(&fakeStore{wh: hook(false, nil)}, fd)
		err := w.Work(context.Background(), job("wh_test", webhooknotify.KindDisabled, 1))
		if err == nil {
			t.Fatal("transient failure must return an error so River retries")
		}
		var cancelErr *river.JobCancelError
		var snoozeErr *river.JobSnoozeError
		if errors.As(err, &cancelErr) || errors.As(err, &snoozeErr) {
			t.Errorf("transient failure must be a plain error, got %T: %v", err, err)
		}
	})
}
