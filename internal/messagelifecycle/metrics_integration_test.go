//go:build integration

package messagelifecycle_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/testutil"
)

var metricsBaseTime = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// seedMetricsAgent creates one user + agent and returns the agent ID. Callers
// add messages with explicit created_at so cohort-window behavior is pinned by
// the test rather than by wall-clock timing.
func seedMetricsAgent(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	ctx := context.Background()
	userID := "usr_" + slug
	agentID := "agt_" + slug
	domain := slug + ".localhost"
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, name, google_subject)
		VALUES ($1, $2, '', $1)
	`, userID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// Each agent gets its own synthetic domain rather than borrowing the
	// seeded shared one, so these fixtures carry no production-shaped host.
	if _, err := pool.Exec(ctx, `
		INSERT INTO domains (domain, user_id, verified, verified_at)
		VALUES ($1, $2, true, now())
	`, domain, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_identities (id, registered_domain, user_id, name)
		VALUES ($1, $2, $3, '')
	`, agentID, domain, userID); err != nil {
		t.Fatal(err)
	}
	return agentID
}

func seedMetricsMessage(t *testing.T, pool *pgxpool.Pool, messageID, agentID, direction string, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO messages (id, agent_id, direction, created_at)
		VALUES ($1, $2, $3, $4)
	`, messageID, agentID, direction, createdAt); err != nil {
		t.Fatal(err)
	}
}

func appendMetricsObservation(t *testing.T, pool *pgxpool.Pool, messageID, direction, dedupeKey string, code messagelifecycle.ReasonCode, occurredAt time.Time) {
	t.Helper()
	input := messagelifecycle.AppendInput{
		MessageID:  messageID,
		DedupeKey:  dedupeKey,
		Direction:  direction,
		ReasonCode: code,
		OccurredAt: occurredAt,
	}
	// Delivery and suppression observations are recipient-scoped; supply one
	// so the shared validator accepts them.
	switch code {
	case messagelifecycle.ReasonDeliveryRecipientServerAccepted,
		messagelifecycle.ReasonDeliveryPermanentBounce,
		messagelifecycle.ReasonDeliveryTransientBounce,
		messagelifecycle.ReasonDeliveryUndeterminedBounce,
		messagelifecycle.ReasonDeliveryTemporaryDelay,
		messagelifecycle.ReasonComplaintRecipientReported,
		messagelifecycle.ReasonSuppressionRecipientBlocked:
		input.Recipient = "recipient@example.test"
	}
	appendAndCommit(t, pool, input)
}

func countsByCode(metrics messagelifecycle.AgentMetrics) map[messagelifecycle.ReasonCode]messagelifecycle.ReasonCodeCount {
	byCode := make(map[messagelifecycle.ReasonCode]messagelifecycle.ReasonCodeCount, len(metrics.Counts))
	for _, count := range metrics.Counts {
		byCode[count.ReasonCode] = count
	}
	return byCode
}

// TestCountByReasonCodeSeparatesObservationsFromMessages pins the grain
// distinction the summary depends on: a message the pipeline retried must not
// be able to push a stage count above its own message count.
func TestCountByReasonCodeSeparatesObservationsFromMessages(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	agentID := seedMetricsAgent(t, pool, "metricsgrain")

	seedMetricsMessage(t, pool, "msg_grain_1", agentID, "outbound", metricsBaseTime)
	appendMetricsObservation(t, pool, "msg_grain_1", "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	// Three deferrals of the SAME message.
	for _, attempt := range []string{"a1", "a2", "a3"} {
		appendMetricsObservation(t, pool, "msg_grain_1", "outbound", "defer:"+attempt,
			messagelifecycle.ReasonSubmissionTemporaryFailure, metricsBaseTime.Add(time.Minute))
	}

	metrics, err := store.CountByReasonCode(context.Background(), agentID,
		metricsBaseTime.Add(-time.Hour), metricsBaseTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("CountByReasonCode: %v", err)
	}

	deferred := countsByCode(metrics)[messagelifecycle.ReasonSubmissionTemporaryFailure]
	if deferred.Observations != 3 {
		t.Errorf("observations = %d, want 3 (one per attempt)", deferred.Observations)
	}
	if deferred.Messages != 1 {
		t.Errorf("messages = %d, want 1 (retries are not new messages)", deferred.Messages)
	}
	if deferred.Stage != messagelifecycle.StageSubmission || deferred.Outcome != messagelifecycle.OutcomeDeferred {
		t.Errorf("semantic tuple = %s/%s, want submission/deferred", deferred.Stage, deferred.Outcome)
	}
	if !deferred.Retryable {
		t.Error("submission.temporary_failure must report retryable")
	}
}

// TestCountByReasonCodeWindowIsHalfOpenOnMessageCreation pins both the
// half-open boundary and the cohort anchor: membership follows the message's
// creation time, never the observation's own timestamp.
func TestCountByReasonCodeWindowIsHalfOpenOnMessageCreation(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	agentID := seedMetricsAgent(t, pool, "metricswindow")

	start := metricsBaseTime
	end := metricsBaseTime.Add(24 * time.Hour)

	// Exactly at start → included. Exactly at end → excluded. Before → excluded.
	seedMetricsMessage(t, pool, "msg_at_start", agentID, "outbound", start)
	seedMetricsMessage(t, pool, "msg_at_end", agentID, "outbound", end)
	seedMetricsMessage(t, pool, "msg_before", agentID, "outbound", start.Add(-time.Second))
	for _, id := range []string{"msg_at_start", "msg_at_end", "msg_before"} {
		appendMetricsObservation(t, pool, id, "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, start)
	}

	// A message created inside the window whose bounce lands well after it.
	// Cohort anchoring must still count the bounce against this window —
	// that is exactly what makes the ratio's numerator and denominator
	// describe the same population.
	seedMetricsMessage(t, pool, "msg_late_bounce", agentID, "outbound", start.Add(time.Hour))
	appendMetricsObservation(t, pool, "msg_late_bounce", "outbound", "accept",
		messagelifecycle.ReasonAcceptanceOutboundAPI, start.Add(time.Hour))
	appendMetricsObservation(t, pool, "msg_late_bounce", "outbound", "bounce",
		messagelifecycle.ReasonDeliveryPermanentBounce, end.Add(48*time.Hour))

	metrics, err := store.CountByReasonCode(context.Background(), agentID, start, end)
	if err != nil {
		t.Fatalf("CountByReasonCode: %v", err)
	}
	byCode := countsByCode(metrics)

	if got := byCode[messagelifecycle.ReasonAcceptanceOutboundAPI].Messages; got != 2 {
		t.Errorf("accepted messages = %d, want 2 (msg_at_start and msg_late_bounce only)", got)
	}
	if got := byCode[messagelifecycle.ReasonDeliveryPermanentBounce].Messages; got != 1 {
		t.Errorf("bounce messages = %d, want 1: feedback outside the window still belongs to its send cohort", got)
	}
	if metrics.MessagesInWindow != 2 {
		t.Errorf("messages_in_window = %d, want 2", metrics.MessagesInWindow)
	}
}

// TestCountByReasonCodeIsScopedToOneAgent guards the tenancy boundary: this
// aggregate is reached through a per-agent route, so a sibling agent's traffic
// must never leak into it.
func TestCountByReasonCodeIsScopedToOneAgent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	mine := seedMetricsAgent(t, pool, "metricsmine")
	theirs := seedMetricsAgent(t, pool, "metricstheirs")

	seedMetricsMessage(t, pool, "msg_mine", mine, "outbound", metricsBaseTime)
	appendMetricsObservation(t, pool, "msg_mine", "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	for _, id := range []string{"msg_theirs_1", "msg_theirs_2"} {
		seedMetricsMessage(t, pool, id, theirs, "outbound", metricsBaseTime)
		appendMetricsObservation(t, pool, id, "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	}

	metrics, err := store.CountByReasonCode(context.Background(), mine,
		metricsBaseTime.Add(-time.Hour), metricsBaseTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("CountByReasonCode: %v", err)
	}
	if got := countsByCode(metrics)[messagelifecycle.ReasonAcceptanceOutboundAPI].Messages; got != 1 {
		t.Errorf("accepted messages = %d, want 1: another agent's traffic leaked in", got)
	}
	if metrics.MessagesInWindow != 1 {
		t.Errorf("messages_in_window = %d, want 1", metrics.MessagesInWindow)
	}
}

// TestCountByReasonCodeReportsLedgerCoverage pins the honesty guarantee: a
// message with no persisted observations still shows up in the denominator, so
// a ledger gap reads as a coverage number instead of a delivery collapse.
func TestCountByReasonCodeReportsLedgerCoverage(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	agentID := seedMetricsAgent(t, pool, "metricscoverage")

	seedMetricsMessage(t, pool, "msg_with_ledger", agentID, "outbound", metricsBaseTime)
	appendMetricsObservation(t, pool, "msg_with_ledger", "outbound", "accept",
		messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	// Predates the ledger: no transitions at all.
	seedMetricsMessage(t, pool, "msg_without_ledger", agentID, "outbound", metricsBaseTime)

	metrics, err := store.CountByReasonCode(context.Background(), agentID,
		metricsBaseTime.Add(-time.Hour), metricsBaseTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("CountByReasonCode: %v", err)
	}
	if metrics.MessagesInWindow != 2 {
		t.Errorf("messages_in_window = %d, want 2", metrics.MessagesInWindow)
	}
	if metrics.MessagesWithLifecycle != 1 {
		t.Errorf("messages_with_lifecycle = %d, want 1", metrics.MessagesWithLifecycle)
	}
}

// TestCountByReasonCodeIncludesTrashedMessages: trashing mail is inbox
// housekeeping, not a restatement of what was sent. Excluding trashed rows
// would let a customer silently rewrite last month's delivery rate.
func TestCountByReasonCodeIncludesTrashedMessages(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	agentID := seedMetricsAgent(t, pool, "metricstrash")

	seedMetricsMessage(t, pool, "msg_trashed", agentID, "outbound", metricsBaseTime)
	appendMetricsObservation(t, pool, "msg_trashed", "outbound", "accept",
		messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	if _, err := pool.Exec(context.Background(),
		`UPDATE messages SET deleted_at = now() WHERE id = $1`, "msg_trashed"); err != nil {
		t.Fatal(err)
	}

	metrics, err := store.CountByReasonCode(context.Background(), agentID,
		metricsBaseTime.Add(-time.Hour), metricsBaseTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("CountByReasonCode: %v", err)
	}
	if got := countsByCode(metrics)[messagelifecycle.ReasonAcceptanceOutboundAPI].Messages; got != 1 {
		t.Errorf("accepted messages = %d, want 1: a trashed message was still sent", got)
	}
}

func TestCountByReasonCodeRejectsInvalidWindows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	agentID := seedMetricsAgent(t, pool, "metricsinvalid")
	ctx := context.Background()

	cases := []struct {
		name       string
		agentID    string
		start, end time.Time
		wantSubstr string
	}{
		{"empty agent", "", metricsBaseTime, metricsBaseTime.Add(time.Hour), "agent id required"},
		{"end equals start", agentID, metricsBaseTime, metricsBaseTime, "end must be after start"},
		{"end before start", agentID, metricsBaseTime, metricsBaseTime.Add(-time.Hour), "end must be after start"},
		{"window too wide", agentID, metricsBaseTime, metricsBaseTime.Add(messagelifecycle.MaxMetricsWindow + time.Second), "window exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.CountByReasonCode(ctx, tc.agentID, tc.start, tc.end); err == nil {
				t.Fatal("expected an error")
			} else if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestCountByReasonCodeEmptyWindow: an agent with no traffic must return an
// empty tally rather than an error, so a brand-new agent's dashboard renders.
func TestCountByReasonCodeEmptyWindow(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	agentID := seedMetricsAgent(t, pool, "metricsempty")

	metrics, err := store.CountByReasonCode(context.Background(), agentID,
		metricsBaseTime, metricsBaseTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("CountByReasonCode: %v", err)
	}
	if len(metrics.Counts) != 0 || metrics.MessagesInWindow != 0 || metrics.MessagesWithLifecycle != 0 {
		t.Fatalf("expected an empty aggregate, got %+v", metrics)
	}
}
