package janitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/janitor"
	"github.com/tokencanopy/e2a/internal/oauth"
)

// fakePruner implements every prune interface (MessagePruner, DeliveryPruner,
// SubscriberPruner, WebhookEventPruner, IdempotencyPruner, OAuthPruner) so a
// single fake can stand in for all of the janitor's dependencies. Each method
// records that it was called and returns a configurable error.
type fakePruner struct {
	messagesCalled     int
	agentsCalled       int
	sessionsCalled     int
	deliveriesCalled   int
	subscribersCalled  int
	webhookEventCalled int
	oauthCalled        int
	idempotencyCalled  int

	// per-method error injection
	messagesErr     error
	agentsErr       error
	sessionsErr     error
	deliveriesErr   error
	subscribersErr  error
	webhookEventErr error
	oauthErr        error
	idempotencyErr  error
}

func (f *fakePruner) DeleteExpiredMessages(context.Context) (int64, error) {
	f.messagesCalled++
	return 3, f.messagesErr // distinct count so the metric test can map count→label
}

func (f *fakePruner) PurgeDeletedAgents(context.Context) (int64, error) {
	f.agentsCalled++
	return 2, f.agentsErr // distinct count for the metric test
}

func (f *fakePruner) DeleteExpiredUserSessions(context.Context) (int64, error) {
	f.sessionsCalled++
	return 1, f.sessionsErr
}

func (f *fakePruner) DeleteExpiredDeliveries(context.Context) (int64, error) {
	f.deliveriesCalled++
	return 1, f.deliveriesErr
}

func (f *fakePruner) DeleteExpiredSubscriberDeliveries(context.Context) (int, int, error) {
	f.subscribersCalled++
	return 5, 4, f.subscribersErr // distinct deleted + marked counts for the metric test
}

func (f *fakePruner) DeleteExpiredWebhookEvents(context.Context) (int, error) {
	f.webhookEventCalled++
	return 7, f.webhookEventErr // distinct count for the metric test
}

func (f *fakePruner) Sweep(context.Context) (int64, error) {
	f.idempotencyCalled++
	return 1, f.idempotencyErr
}

func (f *fakePruner) CleanupExpired(context.Context, time.Time) (oauth.RetentionResult, error) {
	f.oauthCalled++
	return oauth.RetentionResult{AuthCodesDeleted: 1}, f.oauthErr
}

// metricCall captures one JanitorRowsDeleted emission (table + count).
type metricCall struct {
	table string
	count int
}

// fakeMetrics records every JanitorRowsDeleted call so a test can assert exactly
// which prunes emit a metric and with what count, plus WebhookExpiredPending
// emissions from the subscriber prune's mark-failed phase.
type fakeMetrics struct {
	calls          []metricCall
	expiredPending []int
	terminals      []terminalMetricCall
}

type terminalMetricCall struct {
	outcome string
	scope   string
	count   int
}

func (m *fakeMetrics) JanitorRowsDeleted(table string, count int) {
	m.calls = append(m.calls, metricCall{table, count})
}

func (m *fakeMetrics) WebhookExpiredPending(count int) {
	m.expiredPending = append(m.expiredPending, count)
}

func (m *fakeMetrics) WebhookTerminal(outcome, scope string, count int) {
	m.terminals = append(m.terminals, terminalMetricCall{outcome, scope, count})
}

func newJanitor(f *fakePruner, oauth janitor.OAuthPruner) *janitor.Janitor {
	return janitor.New(f, f, f, f, oauth, f, &fakeMetrics{})
}

// newJanitorM is newJanitor but also returns the fake metrics sink so a test can
// assert the emitted (table, count) pairs.
func newJanitorM(f *fakePruner, oauth janitor.OAuthPruner) (*janitor.Janitor, *fakeMetrics) {
	m := &fakeMetrics{}
	return janitor.New(f, f, f, f, oauth, f, m), m
}

// TestSweep_CallsEveryPruneOnce: one Sweep drives each prune method exactly once
// (with a non-nil oauth dep, all seven passes run).
func TestSweep_CallsEveryPruneOnce(t *testing.T) {
	f := &fakePruner{}
	j := newJanitor(f, f) // f satisfies OAuthPruner too

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: unexpected error: %v", err)
	}

	checks := []struct {
		name string
		got  int
	}{
		{"DeleteExpiredMessages", f.messagesCalled},
		{"PurgeDeletedAgents", f.agentsCalled},
		{"DeleteExpiredUserSessions", f.sessionsCalled},
		{"DeleteExpiredDeliveries", f.deliveriesCalled},
		{"DeleteExpiredSubscriberDeliveries", f.subscribersCalled},
		{"DeleteExpiredWebhookEvents", f.webhookEventCalled},
		{"CleanupExpired", f.oauthCalled},
		{"Sweep(idempotency)", f.idempotencyCalled},
	}
	for _, c := range checks {
		if c.got != 1 {
			t.Errorf("%s called %d times, want 1", c.name, c.got)
		}
	}
}

// TestSweep_EmitsMetricsForCorrectTables: exactly the three metric-emitting prunes
// (messages, webhook_subscriber_deliveries, webhook_events) fire JanitorRowsDeleted
// with the count that prune returned — and sessions/deliveries/oauth/idempotency
// emit NO metric. Guards against a mislabeled, dropped, spurious, or wrong-count
// metric (incl. the int64→int cast on messages).
func TestSweep_EmitsMetricsForCorrectTables(t *testing.T) {
	f := &fakePruner{}
	j, m := newJanitorM(f, f) // non-nil oauth so every pass runs
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: unexpected error: %v", err)
	}
	want := []metricCall{
		{"messages", 3},
		{"agent_identities", 2},
		{"webhook_subscriber_deliveries", 5},
		{"webhook_events", 7},
	}
	if len(m.calls) != len(want) {
		t.Fatalf("emitted %d metrics %+v, want exactly %d %+v", len(m.calls), m.calls, len(want), want)
	}
	for i, w := range want {
		if m.calls[i] != w {
			t.Errorf("metric[%d] = %+v, want %+v", i, m.calls[i], w)
		}
	}
	// The subscriber prune's mark-failed phase emits WebhookExpiredPending with
	// the marked count it returned.
	if len(m.expiredPending) != 1 || m.expiredPending[0] != 4 {
		t.Errorf("WebhookExpiredPending emissions = %v, want [4]", m.expiredPending)
	}
	if len(m.terminals) != 1 || m.terminals[0] != (terminalMetricCall{"e2a_failure", "unknown", 4}) {
		t.Errorf("WebhookTerminal emissions = %v, want e2a_failure/unknown count 4", m.terminals)
	}
}

// TestSweep_ContinuesPastErrors: an early prune failing does NOT prevent the
// subsequent prunes from running (continue-on-error preserved), and Sweep
// returns a joined error carrying every failure.
func TestSweep_ContinuesPastErrors(t *testing.T) {
	errMsg := errors.New("messages boom")
	errSub := errors.New("subscribers boom")
	f := &fakePruner{messagesErr: errMsg, subscribersErr: errSub}
	j := newJanitor(f, f)

	err := j.Sweep(context.Background())
	if err == nil {
		t.Fatal("Sweep: expected an error, got nil")
	}
	if !errors.Is(err, errMsg) {
		t.Errorf("joined error missing messages failure: %v", err)
	}
	if !errors.Is(err, errSub) {
		t.Errorf("joined error missing subscribers failure: %v", err)
	}

	// Every later prune still ran despite the first prune erroring.
	if f.agentsCalled != 1 || f.sessionsCalled != 1 || f.deliveriesCalled != 1 || f.subscribersCalled != 1 ||
		f.webhookEventCalled != 1 || f.oauthCalled != 1 || f.idempotencyCalled != 1 {
		t.Errorf("a prune was skipped after an earlier error: %+v", f)
	}
}

// TestSweep_NilOAuthSkipped: a nil OAuth dependency (OAuth provider disabled) is
// skipped without panicking, and the other prunes still run.
func TestSweep_NilOAuthSkipped(t *testing.T) {
	f := &fakePruner{}
	j := newJanitor(f, nil) // nil OAuthPruner interface

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep with nil oauth: unexpected error: %v", err)
	}
	if f.oauthCalled != 0 {
		t.Errorf("CleanupExpired called %d times with nil oauth dep, want 0", f.oauthCalled)
	}
	// The remaining prunes still ran.
	if f.messagesCalled != 1 || f.idempotencyCalled != 1 {
		t.Errorf("nil oauth dep disturbed the other prunes: %+v", f)
	}
}

// TestMaintenanceWorker_WorkSwallowsError: Work returns nil even when Sweep
// errors, so a transient DB blip never spins River's retry.
func TestMaintenanceWorker_WorkSwallowsError(t *testing.T) {
	f := &fakePruner{messagesErr: errors.New("boom")}
	w := janitor.NewMaintenanceWorker(newJanitor(f, f))

	if err := w.Work(context.Background(), &river.Job[janitor.JanitorArgs]{}); err != nil {
		t.Fatalf("Work returned %v, want nil (errors are swallowed)", err)
	}
	// Sweep still ran to completion.
	if f.idempotencyCalled != 1 {
		t.Errorf("Work did not run the full sweep: idempotency called %d times", f.idempotencyCalled)
	}
}

// TestMaintenanceJobs_RegistersOnePeriodic: RegisterJobs contributes exactly one
// periodic (the janitor schedule) and wires its worker.
func TestMaintenanceJobs_RegistersOnePeriodic(t *testing.T) {
	m := janitor.NewMaintenanceJobs(newJanitor(&fakePruner{}, nil))
	periodics := m.RegisterJobs(river.NewWorkers())
	if len(periodics) != 1 {
		t.Fatalf("RegisterJobs returned %d periodic jobs, want 1", len(periodics))
	}
}
