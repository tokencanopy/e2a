package janitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/identity"
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
	threadAuditCursors []string
	threadAuditResults []identity.ThreadIdentityAuditResult

	// per-method error injection
	messagesErr     error
	agentsErr       error
	sessionsErr     error
	deliveriesErr   error
	subscribersErr  error
	webhookEventErr error
	oauthErr        error
	idempotencyErr  error
	threadAuditErr  error
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

func (f *fakePruner) AuditThreadIdentity(_ context.Context, afterID string) (identity.ThreadIdentityAuditResult, error) {
	f.threadAuditCursors = append(f.threadAuditCursors, afterID)
	if f.threadAuditErr != nil {
		return identity.ThreadIdentityAuditResult{}, f.threadAuditErr
	}
	index := len(f.threadAuditCursors) - 1
	if index < len(f.threadAuditResults) {
		return f.threadAuditResults[index], nil
	}
	return identity.ThreadIdentityAuditResult{}, nil
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
	resolutions    []threadCountMetricCall
	nullThreads    []threadCountMetricCall
	violations     []threadCountMetricCall
	relationships  []threadPercentMetricCall
}

type terminalMetricCall struct {
	outcome string
	scope   string
	count   int
}

type threadCountMetricCall struct {
	kind  string
	count int
}

type threadPercentMetricCall struct {
	kind    string
	percent float64
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

func (m *fakeMetrics) ThreadResolution(source string, count int) {
	m.resolutions = append(m.resolutions, threadCountMetricCall{source, count})
}

func (m *fakeMetrics) SetThreadNullMessages(ageBucket string, count int) {
	m.nullThreads = append(m.nullThreads, threadCountMetricCall{ageBucket, count})
}

func (m *fakeMetrics) SetThreadInvariantViolations(kind string, count int) {
	m.violations = append(m.violations, threadCountMetricCall{kind, count})
}

func (m *fakeMetrics) SetThreadRelationshipPercent(kind string, percent float64) {
	m.relationships = append(m.relationships, threadPercentMetricCall{kind, percent})
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

func TestSweep_AuditsThreadIdentityAndAdvancesCursor(t *testing.T) {
	f := &fakePruner{threadAuditResults: []identity.ThreadIdentityAuditResult{
		{
			Scanned:    8,
			NextCursor: "msg_next",
			Violations: identity.ThreadInvariantViolations{
				DanglingParent:   1,
				CrossAgentParent: 2,
				ThreadMismatch:   3,
				Cycle:            4,
				CycleDepthLimit:  5,
			},
			RepairedParentPointers: 10,
			NullThreadsByAge: identity.ThreadNullAgeBuckets{
				LessThanOneHour:      6,
				OneToSixHours:        7,
				SixToTwentyFourHours: 8,
			},
			ThreadsSampled:           4,
			MultiConversationThreads: 1,
			ConversationsSampled:     5,
			MultiThreadConversations: 2,
		},
		{},
	}}
	j, m := newJanitorM(f, nil)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if got, want := f.threadAuditCursors, []string{"", "msg_next"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("audit cursors = %v, want %v", got, want)
	}

	wantNull := []threadCountMetricCall{{"lt_1h", 6}, {"1h_6h", 7}, {"6h_24h", 8}}
	wantViolations := []threadCountMetricCall{
		{"dangling_parent", 1},
		{"cross_agent_parent", 2},
		{"thread_mismatch", 3},
		{"cycle", 4},
		{"cycle_depth_limit", 5},
	}
	wantRelationships := []threadPercentMetricCall{
		{"threads_multi_conversation", 25},
		{"conversations_multi_thread", 40},
	}
	if len(m.nullThreads) < len(wantNull) || !equalThreadCounts(m.nullThreads[:min(len(m.nullThreads), len(wantNull))], wantNull) {
		t.Errorf("null-thread metrics = %v, want prefix %v", m.nullThreads, wantNull)
	}
	if len(m.violations) < len(wantViolations) || !equalThreadCounts(m.violations[:min(len(m.violations), len(wantViolations))], wantViolations) {
		t.Errorf("violation metrics = %v, want prefix %v", m.violations, wantViolations)
	}
	if len(m.relationships) < len(wantRelationships) || !equalThreadPercents(m.relationships[:min(len(m.relationships), len(wantRelationships))], wantRelationships) {
		t.Errorf("relationship metrics = %v, want prefix %v", m.relationships, wantRelationships)
	}
	if got, want := m.resolutions, []threadCountMetricCall{{"cycle_detected", 4}}; !equalThreadCounts(got, want) {
		t.Errorf("resolution metrics = %v, want %v", got, want)
	}
}

func TestSweep_ContinuesAfterThreadAuditError(t *testing.T) {
	auditErr := errors.New("thread audit boom")
	f := &fakePruner{threadAuditErr: auditErr}
	j := newJanitor(f, nil)

	err := j.Sweep(context.Background())
	if !errors.Is(err, auditErr) {
		t.Fatalf("Sweep error = %v, want joined thread audit error", err)
	}
	if f.idempotencyCalled != 1 {
		t.Fatalf("idempotency prune called %d times after audit error, want 1", f.idempotencyCalled)
	}
}

func equalThreadCounts(got, want []threadCountMetricCall) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalThreadPercents(got, want []threadPercentMetricCall) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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
