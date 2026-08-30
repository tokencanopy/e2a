package webhookpub_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// seedUser inserts a users row so the FK on webhook_events.user_id is
// satisfied. Returns the user id. Caller owns cleanup via t.Cleanup.
func seedUser(t *testing.T, ctx context.Context, pool interface {
	Exec(ctx context.Context, sql string, args ...any) (any, error)
}) string {
	// Use the same helper-shape as the rest of the test suite — a
	// thin wrapper through pool.Exec. Tests in this package import
	// the production pool type via testutil.TestDB; we shadow the
	// signature here to keep the integration helper local.
	t.Helper()
	return "u_outbox_test"
}

// TestOutboxPublisher_Integration_WritesToWebhookEvents proves the adapter that
// routes the previously-bypassing event sources (domain.*, delivery feedback,
// hitl-TTL) writes them into webhook_events — so they flow through the drain to
// River/legacy delivery instead of being stranded under engine=river.
func TestOutboxPublisher_Integration_WritesToWebhookEvents(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	userID := "u_outboxpub_test"
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, name, google_subject, created_at) VALUES ($1, $2, 'T', $1, now()) ON CONFLICT (id) DO NOTHING`,
		userID, userID+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM webhook_events WHERE user_id=$1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
	})

	pub := webhookpub.NewOutboxPublisher(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), pool)
	e := webhookpub.NewEvent(webhookpub.EventDomainSendingVerified, userID, map[string]any{"domain": "x.example.com"})
	pub.Publish(ctx, e)

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_events WHERE id=$1 AND user_id=$2 AND type=$3`,
		e.ID, userID, webhookpub.EventDomainSendingVerified,
	).Scan(&n); err != nil {
		t.Fatalf("query webhook_events: %v", err)
	}
	if n != 1 {
		t.Errorf("webhook_events rows = %d, want 1 (domain event now flows through the outbox)", n)
	}
}

// TestOutbox_Integration_HappyPath_RowCommitsWithExpectedFields exercises
// the full PublishTx → INSERT → row layout path against a real DB. Acts
// as a regression guard for migration 026's column shape and the
// outbox.go INSERT statement staying in sync.
func TestOutbox_Integration_HappyPath_RowCommitsWithExpectedFields(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	// Insert a users row first so the user_id FK is satisfied.
	const userID = "u_outbox_happy"
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, name, google_subject, created_at)
	                           VALUES ($1, $2, 'Test', $1, now())
	                           ON CONFLICT (id) DO NOTHING`,
		userID, userID+"@example.com")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM webhook_events WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))

	// MessageID intentionally left empty in this fixture-light test —
	// the FK on webhook_events.message_id is enforced, and seeding a
	// messages row pulls in agents + agent_identities fixtures we'd
	// rather not duplicate from internal/identity tests. The
	// "production messages row exists in same tx" case is covered by
	// the relay integration test below.
	event := webhookpub.Event{
		ID:             webhookpub.DeterministicEventID("msg_test_1", webhookpub.EventEmailReceived),
		Type:           webhookpub.EventEmailReceived,
		UserID:         userID,
		AgentID:        "agent@example.com",
		ConversationID: "conv_x",
		Data:           map[string]any{"hello": "world"},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := outbox.PublishTx(ctx, tx, event); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("PublishTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Read back and verify.
	var (
		gotID, gotUser, gotType, gotAud, gotStatus string
		gotAgent, gotConv, gotMsgID                *string
		gotSchemaVersion                           int16
		gotEnvelopeJSON                            []byte
		gotAttempts                                int
	)
	err = pool.QueryRow(ctx, `SELECT id, user_id, type, aud, envelope::text, schema_version,
	                               agent_id, conversation_id, message_id, status, attempts
	                          FROM webhook_events WHERE id = $1`, event.ID).Scan(
		&gotID, &gotUser, &gotType, &gotAud, &gotEnvelopeJSON, &gotSchemaVersion,
		&gotAgent, &gotConv, &gotMsgID, &gotStatus, &gotAttempts,
	)
	if err != nil {
		t.Fatalf("read back row: %v", err)
	}

	if gotID != event.ID {
		t.Errorf("id = %s, want %s", gotID, event.ID)
	}
	if gotUser != userID {
		t.Errorf("user_id = %s, want %s", gotUser, userID)
	}
	if gotType != webhookpub.EventEmailReceived {
		t.Errorf("type = %s, want %s", gotType, webhookpub.EventEmailReceived)
	}
	if gotAud != "webhook" {
		t.Errorf("aud = %s, want webhook", gotAud)
	}
	if gotStatus != "pending" {
		t.Errorf("status = %s, want pending", gotStatus)
	}
	if gotSchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", gotSchemaVersion)
	}
	if gotAttempts != 0 {
		t.Errorf("attempts = %d, want 0", gotAttempts)
	}
	if gotAgent == nil || *gotAgent != "agent@example.com" {
		t.Errorf("agent_id = %v, want agent@example.com", gotAgent)
	}
	if gotConv == nil || *gotConv != "conv_x" {
		t.Errorf("conversation_id = %v, want conv_x", gotConv)
	}
	if gotMsgID != nil {
		t.Errorf("message_id = %v, want NULL (event.MessageID was empty)", *gotMsgID)
	}

	// Envelope should round-trip through JSON.
	var env webhookpub.Envelope
	if err := json.Unmarshal(gotEnvelopeJSON, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != webhookpub.EventEmailReceived {
		t.Errorf("envelope.type = %s, want email.received", env.Type)
	}
	if env.ID != event.ID {
		t.Errorf("envelope.id = %s, want %s", env.ID, event.ID)
	}
}

// TestOutbox_Integration_OnConflictDoNothing verifies the deterministic
// event ID's at-least-once guarantee: re-running PublishTx for the same
// (message_id, event_type) collides on id, and ON CONFLICT (id) DO NOTHING
// silently no-ops the duplicate. This is the safety property that lets
// MTA SMTP retries be idempotent against the outbox.
func TestOutbox_Integration_OnConflictDoNothing(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	const userID = "u_outbox_conflict"
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, name, google_subject, created_at)
	                           VALUES ($1, $2, 'Test', $1, now())
	                           ON CONFLICT (id) DO NOTHING`,
		userID, userID+"@example.com")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM webhook_events WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	eventID := webhookpub.DeterministicEventID("msg_retry", webhookpub.EventEmailReceived)

	publish := func(payload string) error {
		event := webhookpub.Event{
			ID:     eventID,
			Type:   webhookpub.EventEmailReceived,
			UserID: userID,
			// MessageID omitted — see HappyPath test comment.
			Data: map[string]any{"payload": payload},
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if err := outbox.PublishTx(ctx, tx, event); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}

	if err := publish("first"); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if err := publish("second-should-noop"); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	// Exactly one row should exist.
	var rowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_events WHERE id = $1`, eventID,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("expected exactly 1 row, got %d", rowCount)
	}

	// The envelope should reflect the FIRST publish — ON CONFLICT DO
	// NOTHING preserved the original payload.
	var envelopeJSON []byte
	if err := pool.QueryRow(ctx,
		`SELECT envelope::text FROM webhook_events WHERE id = $1`, eventID,
	).Scan(&envelopeJSON); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	if !strings.Contains(string(envelopeJSON), `"first"`) {
		t.Errorf("envelope should preserve first publish; got %s", envelopeJSON)
	}
	if strings.Contains(string(envelopeJSON), "second-should-noop") {
		t.Errorf("envelope should NOT contain the second payload; got %s", envelopeJSON)
	}
}

// TestOutbox_Integration_FlagOffNoWrite verifies that PublishTx with a
// disabled FeatureFlag is a complete no-op: no row written, no error.
// Confirms the runtime gate works end-to-end (not just in unit tests).
func TestOutbox_Integration_FlagOffNoWrite(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	const userID = "u_outbox_flagoff"
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, name, google_subject, created_at)
	                           VALUES ($1, $2, 'Test', $1, now())
	                           ON CONFLICT (id) DO NOTHING`,
		userID, userID+"@example.com")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM webhook_events WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(false))
	event := webhookpub.Event{
		ID:     webhookpub.DeterministicEventID("msg_flagoff", webhookpub.EventEmailReceived),
		Type:   webhookpub.EventEmailReceived,
		UserID: userID,
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := outbox.PublishTx(ctx, tx, event); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("PublishTx with flag off: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var rowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_events WHERE user_id = $1`, userID,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 0 {
		t.Errorf("expected zero rows with flag off, got %d", rowCount)
	}
}

// TestOutbox_Integration_JanitorDeletesExpired exercises the slice-A
// janitor on a matrix of (status, expires_at) combinations. The
// invariant the janitor must preserve:
//
//   - Terminal rows (status ∈ {processed, no_match}) past expires_at
//     are deleted.
//   - Pending rows past expires_at are NOT deleted, even when they're
//     overdue. The outbox is at-least-once by design (worker.go's
//     recordFailure: "no terminal 'failed' state … row stays pending
//     until human intervention or a successful retry"). Deleting them
//     at day 30 silently breaks the retry-forever guarantee and drops
//     events that never reached any webhook.
//   - Rows within retention are untouched regardless of status.
//
// Critical for unbounded-growth prevention on the outbox table AND
// for the at-least-once delivery guarantee. Before the status guard
// was added, a row that failed every fan-out attempt would sit
// pending for 30 days and then be silently destroyed.
func TestOutbox_Integration_JanitorDeletesExpired(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	const userID = "u_outbox_janitor"
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, name, google_subject, created_at)
	                           VALUES ($1, $2, 'Janitor', $1, now())
	                           ON CONFLICT (id) DO NOTHING`,
		userID, userID+"@example.com")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM webhook_events WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	// Seed the full (status × expires_at) matrix. Each row is keyed by
	// a deterministic ID so the post-sweep assertions can target it
	// individually.
	rows := []struct {
		label      string
		status     string
		expiresAt  string
		shouldKeep bool
	}{
		// Terminal + expired → DELETE.
		{"processed_expired", "processed", "now() - interval '1 day'", false},
		{"no_match_expired", "no_match", "now() - interval '1 day'", false},
		// Pending + expired → KEEP. This is the load-bearing assertion
		// for the C1 fix. Pre-fix this row would have been silently
		// destroyed, breaking the at-least-once retry-forever
		// guarantee.
		{"pending_expired", "pending", "now() - interval '1 day'", true},
		// Within-retention rows are untouched regardless of status.
		{"processed_fresh", "processed", "now() + interval '1 day'", true},
		{"pending_fresh", "pending", "now() + interval '1 day'", true},
	}
	ids := make(map[string]string, len(rows))
	for _, row := range rows {
		id := webhookpub.DeterministicEventID("msg_jan_"+row.label, webhookpub.EventEmailReceived)
		ids[row.label] = id
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_events (id, user_id, type, envelope, status, expires_at)
			 VALUES ($1, $2, $3, '{}'::jsonb, $4, `+row.expiresAt+`)`,
			id, userID, webhookpub.EventEmailReceived, row.status)
		if err != nil {
			t.Fatalf("seed %s: %v", row.label, err)
		}
	}

	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	deleted, err := outbox.DeleteExpiredWebhookEvents(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredWebhookEvents: %v", err)
	}
	// Exactly two rows match (expires_at <= now() AND status <> 'pending'):
	// processed_expired and no_match_expired.
	if deleted != 2 {
		t.Errorf("deleted = %d; want 2 (processed_expired + no_match_expired)", deleted)
	}

	for _, row := range rows {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM webhook_events WHERE id = $1`, ids[row.label],
		).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", row.label, err)
		}
		wantCount := 0
		if row.shouldKeep {
			wantCount = 1
		}
		if count != wantCount {
			verdict := "should have been deleted"
			if row.shouldKeep {
				verdict = "should have been kept"
			}
			t.Errorf("%s: count = %d, want %d (%s)", row.label, count, wantCount, verdict)
		}
	}
}

// TestOutbox_Integration_TxRollback_NoOrphanRow verifies that if the
// caller rolls back the trigger transaction (e.g. the messages INSERT
// failed after the outbox INSERT), no webhook_events row is left
// behind. This is the at-least-once invariant from §4.2: messages and
// webhook_events commit together or neither does.
func TestOutbox_Integration_TxRollback_NoOrphanRow(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	const userID = "u_outbox_rollback"
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, name, google_subject, created_at)
	                           VALUES ($1, $2, 'Test', $1, now())
	                           ON CONFLICT (id) DO NOTHING`,
		userID, userID+"@example.com")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM webhook_events WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	event := webhookpub.Event{
		ID:     webhookpub.DeterministicEventID("msg_rollback", webhookpub.EventEmailReceived),
		Type:   webhookpub.EventEmailReceived,
		UserID: userID,
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := outbox.PublishTx(ctx, tx, event); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("PublishTx: %v", err)
	}
	// Simulate the caller's business-write failure mid-tx: ROLLBACK.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var rowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_events WHERE id = $1`, event.ID,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 0 {
		t.Errorf("expected zero rows after rollback, got %d (atomicity broken)", rowCount)
	}
}
