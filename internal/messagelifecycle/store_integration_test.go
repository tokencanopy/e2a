//go:build integration

package messagelifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

var reconstructBaseTime = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

func TestMessageLifecycleMigrationCatalogMatchesCanonicalCatalog(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)

	rows, err := pool.Query(ctx, `
		SELECT code, stage, outcome, retryable
		FROM message_lifecycle_reason_codes
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	got := make(map[messagelifecycle.ReasonCode]messagelifecycle.Definition)
	for rows.Next() {
		var code messagelifecycle.ReasonCode
		var definition messagelifecycle.Definition
		if err := rows.Scan(&code, &definition.Stage, &definition.Outcome, &definition.Retryable); err != nil {
			t.Fatal(err)
		}
		got[code] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := migrationLifecycleCatalog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("database catalog mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestMessageLifecycleMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	sql, err := migrations.FS.ReadFile("073_message_lifecycle.sql")
	if err != nil {
		t.Fatal(err)
	}

	before := lifecycleCatalogRows(t, pool)
	for i := 0; i < 2; i++ {
		if err := identity.RunMigrations(ctx, pool, migrations.FS, identity.ModeAuto); err != nil {
			t.Fatalf("full migration run %d: %v", i+1, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("migration rerun %d: %v", i+1, err)
		}
	}
	after := lifecycleCatalogRows(t, pool)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("catalog changed after migration replay\nbefore: %#v\nafter:  %#v", before, after)
	}
	if want := len(migrationLifecycleCatalog()); len(after) != want {
		t.Fatalf("catalog row count = %d, want %d", len(after), want)
	}
}

// migrationLifecycleCatalog includes the dormant reason codes reserved by the
// sending-protection foundation migration. They stay outside Catalog until B10
// adds their emitters and public lifecycle documentation.
func migrationLifecycleCatalog() map[messagelifecycle.ReasonCode]messagelifecycle.Definition {
	result := messagelifecycle.Catalog()
	result[messagelifecycle.ReasonCode("submission.policy_budget_expired")] = messagelifecycle.Definition{
		Stage:   messagelifecycle.StageSubmission,
		Outcome: messagelifecycle.OutcomeFailed,
	}
	result[messagelifecycle.ReasonCode("submission.sending_setup_expired")] = messagelifecycle.Definition{
		Stage:   messagelifecycle.StageSubmission,
		Outcome: messagelifecycle.OutcomeFailed,
	}
	return result
}

func TestMessageLifecycleMigrationRejectsCatalogDrift(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	sql, err := migrations.FS.ReadFile("073_message_lifecycle.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	const code = "delivery.recipient_server_accepted"
	if _, err := tx.Exec(ctx, `
		UPDATE message_lifecycle_reason_codes
		SET outcome = 'deferred'
		WHERE code = $1
	`, code); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, string(sql))
	if err == nil {
		t.Fatal("migration accepted a drifted immutable catalog tuple")
	}
	if !strings.Contains(err.Error(), code) {
		t.Fatalf("migration error %q does not identify drifted code", err)
	}
	for _, forbidden := range []string{"deferred", "delivered", "false"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("migration error exposes tuple detail %q: %v", forbidden, err)
		}
	}
}

func TestMessageLifecycleMigrationBuildsTransitionsFromExistingExactCatalog(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	sql, err := migrations.FS.ReadFile("073_message_lifecycle.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DROP TABLE message_lifecycle_transitions`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("migration with existing exact catalog: %v", err)
	}
	var indexExists bool
	if err := tx.QueryRow(ctx, `
		SELECT to_regclass('message_lifecycle_message_order_idx') IS NOT NULL
	`).Scan(&indexExists); err != nil {
		t.Fatal(err)
	}
	if !indexExists {
		t.Fatal("message lifecycle ordering index was not created")
	}
	var transitionConstraints string
	if err := tx.QueryRow(ctx, `
		SELECT string_agg(pg_get_constraintdef(oid), E'\n' ORDER BY contype, conname)
		FROM pg_constraint
		WHERE conrelid = 'message_lifecycle_transitions'::regclass
	`).Scan(&transitionConstraints); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"PRIMARY KEY (id)",
		"CHECK ((direction = ANY",
		"FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE",
		"UNIQUE (message_id, dedupe_key)",
		"FOREIGN KEY (reason_code, stage, outcome, retryable) REFERENCES message_lifecycle_reason_codes(code, stage, outcome, retryable)",
	} {
		if !strings.Contains(transitionConstraints, required) {
			t.Errorf("transition constraints missing %q:\n%s", required, transitionConstraints)
		}
	}
	var catalogConstraints string
	if err := tx.QueryRow(ctx, `
		SELECT string_agg(pg_get_constraintdef(oid), E'\n' ORDER BY contype, conname)
		FROM pg_constraint
		WHERE conrelid = 'message_lifecycle_reason_codes'::regclass
	`).Scan(&catalogConstraints); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"PRIMARY KEY (code)",
		"CHECK ((stage = ANY",
		"CHECK ((outcome = ANY",
		"UNIQUE (code, stage, outcome, retryable)",
	} {
		if !strings.Contains(catalogConstraints, required) {
			t.Errorf("catalog constraints missing %q:\n%s", required, catalogConstraints)
		}
	}
}

func TestMessageLifecycleMigrationRejectsContradictoryTuple(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_fk", "outbound")

	_, err := pool.Exec(ctx, `
		INSERT INTO message_lifecycle_transitions
			(id, message_id, dedupe_key, direction, stage, outcome, reason_code, retryable, occurred_at)
		VALUES
			('mlt_fk', 'msg_fk', 'fk', 'outbound', 'accepted', 'failed', 'acceptance.outbound_api', false, now())
	`)
	if err == nil {
		t.Fatal("contradictory reason tuple unexpectedly persisted")
	}
}

func TestMessageLifecycleDirectionMigrationIsIdempotentAndExistingRowSafe(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_direction_migration", "outbound")
	sql, err := migrations.FS.ReadFile("078_message_lifecycle_direction.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DROP TRIGGER IF EXISTS message_lifecycle_direction_matches_message ON message_lifecycle_transitions`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO message_lifecycle_transitions
			(id, message_id, dedupe_key, direction, stage, outcome, reason_code, retryable, occurred_at)
		VALUES ('mlt_existing_mismatch', 'msg_direction_migration', 'existing', 'inbound',
		        'accepted', 'accepted', 'acceptance.inbound_smtp', false, now())
	`); err != nil {
		t.Fatalf("seed historical mismatch with trigger absent: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("direction migration replay %d scanned/rejected existing rows: %v", i+1, err)
		}
	}
	var triggerCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM pg_trigger
		WHERE tgrelid = 'message_lifecycle_transitions'::regclass
		  AND tgname = 'message_lifecycle_direction_matches_message'
		  AND NOT tgisinternal
	`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 1 {
		t.Fatalf("direction trigger count = %d, want 1", triggerCount)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM pg_trigger
		WHERE tgrelid = 'messages'::regclass
		  AND tgname = 'message_direction_matches_lifecycle'
		  AND NOT tgisinternal
	`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 1 {
		t.Fatalf("parent direction trigger count = %d, want 1", triggerCount)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO message_lifecycle_transitions
			(id, message_id, dedupe_key, direction, stage, outcome, reason_code, retryable, occurred_at)
		VALUES ('mlt_new_mismatch', 'msg_direction_migration', 'new', 'inbound',
		        'accepted', 'accepted', 'acceptance.inbound_smtp', false, now())
	`)
	assertDirectionConstraintError(t, err)
}

func TestMessageLifecycleDirectSQLDirectionInvariant(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_direction_sql", "outbound")
	if _, err := pool.Exec(ctx, `UPDATE messages SET direction='inbound' WHERE id='msg_direction_sql'`); err != nil {
		t.Fatalf("direction change without lifecycle child rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET direction='outbound' WHERE id='msg_direction_sql'`); err != nil {
		t.Fatalf("direction restoration without lifecycle child rejected: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO message_lifecycle_transitions
			(id, message_id, dedupe_key, direction, stage, outcome, reason_code, retryable, occurred_at)
		VALUES ('mlt_direction_ok', 'msg_direction_sql', 'ok', 'outbound',
		        'accepted', 'accepted', 'acceptance.outbound_api', false, now())
	`); err != nil {
		t.Fatalf("matching direction rejected: %v", err)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO message_lifecycle_transitions
			(id, message_id, dedupe_key, direction, stage, outcome, reason_code, retryable, occurred_at)
		VALUES ('mlt_direction_bad', 'msg_direction_sql', 'bad', 'inbound',
		        'accepted', 'accepted', 'acceptance.inbound_smtp', false, now())
	`)
	assertDirectionConstraintError(t, err)
	_, err = pool.Exec(ctx, `UPDATE message_lifecycle_transitions SET direction='inbound' WHERE id='mlt_direction_ok'`)
	assertDirectionConstraintError(t, err)
	_, err = pool.Exec(ctx, `UPDATE messages SET direction='inbound' WHERE id='msg_direction_sql'`)
	assertConstraintError(t, err, "message_direction_matches_lifecycle")
}

func TestMessageLifecycleDirectionConcurrencyChildFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testutil.TestDB(t)
	const messageID = "msg_direction_child_first"
	insertLifecycleMessage(t, pool, messageID, "outbound")

	childTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = childTx.Rollback(context.Background()) }()
	if _, err := childTx.Exec(ctx, `
		INSERT INTO message_lifecycle_transitions
			(id, message_id, dedupe_key, direction, stage, outcome, reason_code, retryable, occurred_at)
		VALUES ('mlt_direction_child_first', $1, 'acceptance', 'outbound',
		        'accepted', 'accepted', 'acceptance.outbound_api', false, now())
	`, messageID); err != nil {
		t.Fatalf("insert child before race: %v", err)
	}

	parentDone := make(chan error, 1)
	parentStarted := make(chan struct{})
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			parentDone <- err
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		close(parentStarted)
		if _, err = tx.Exec(ctx, `UPDATE messages SET direction='inbound' WHERE id=$1`, messageID); err != nil {
			parentDone <- err
			return
		}
		parentDone <- tx.Commit(ctx)
	}()
	select {
	case <-parentStarted:
	case err := <-parentDone:
		_ = childTx.Rollback(ctx)
		t.Fatalf("start parent update: %v", err)
	case <-ctx.Done():
		t.Fatal("parent update did not start")
	}

	select {
	case err := <-parentDone:
		_ = childTx.Rollback(ctx)
		t.Fatalf("parent update completed before child transaction released its lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := childTx.Commit(ctx); err != nil {
		t.Fatalf("commit child transaction: %v", err)
	}
	select {
	case err := <-parentDone:
		assertConstraintError(t, err, "message_direction_matches_lifecycle")
	case <-ctx.Done():
		t.Fatal("parent update deadlocked after child commit")
	}
	assertNoLifecycleDirectionMismatch(t, pool, messageID)
}

func TestMessageLifecycleDirectionConcurrencyParentFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testutil.TestDB(t)
	const messageID = "msg_direction_parent_first"
	insertLifecycleMessage(t, pool, messageID, "outbound")

	parentTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parentTx.Rollback(context.Background()) }()
	if _, err := parentTx.Exec(ctx, `UPDATE messages SET direction='inbound' WHERE id=$1`, messageID); err != nil {
		t.Fatalf("update parent before race: %v", err)
	}

	childDone := make(chan error, 1)
	childStarted := make(chan struct{})
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			childDone <- err
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		close(childStarted)
		if _, err = tx.Exec(ctx, `
			INSERT INTO message_lifecycle_transitions
				(id, message_id, dedupe_key, direction, stage, outcome, reason_code, retryable, occurred_at)
			VALUES ('mlt_direction_parent_first', $1, 'acceptance', 'outbound',
			        'accepted', 'accepted', 'acceptance.outbound_api', false, now())
		`, messageID); err != nil {
			childDone <- err
			return
		}
		childDone <- tx.Commit(ctx)
	}()
	select {
	case <-childStarted:
	case err := <-childDone:
		_ = parentTx.Rollback(ctx)
		t.Fatalf("start child insert: %v", err)
	case <-ctx.Done():
		t.Fatal("child insert did not start")
	}

	select {
	case err := <-childDone:
		_ = parentTx.Rollback(ctx)
		t.Fatalf("child insert completed before parent transaction released its lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := parentTx.Commit(ctx); err != nil {
		t.Fatalf("commit parent transaction: %v", err)
	}
	select {
	case err := <-childDone:
		assertDirectionConstraintError(t, err)
	case <-ctx.Done():
		t.Fatal("child insert deadlocked after parent commit")
	}
	assertNoLifecycleDirectionMismatch(t, pool, messageID)
}

func assertNoLifecycleDirectionMismatch(t *testing.T, pool *pgxpool.Pool, messageID string) {
	t.Helper()
	var mismatches int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM message_lifecycle_transitions l
		JOIN messages m ON m.id = l.message_id
		WHERE m.id = $1 AND l.direction IS DISTINCT FROM m.direction
	`, messageID).Scan(&mismatches); err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("message %s has %d contradictory lifecycle rows", messageID, mismatches)
	}
}

func assertDirectionConstraintError(t *testing.T, err error) {
	t.Helper()
	assertConstraintError(t, err, "message_lifecycle_direction_matches_message")
}

func assertConstraintError(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != constraint {
		t.Fatalf("direction constraint error = %#v", err)
	}
}

func TestAppendTxDirectionMismatchIsTypedAndProducerTransactionRollsBack(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	const messageID = "msg_direction_store"
	insertLifecycleMessage(t, pool, messageID, "outbound")
	var initialStatus string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(status, '') FROM messages WHERE id=$1`, messageID).Scan(&initialStatus); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE messages SET status='pending_review' WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO webhook_events (id, user_id, type, envelope, message_id, created_at)
		VALUES ('evt_direction_partial', $1, 'email.sent', '{}', $2, now())
	`, "usr_"+messageID, messageID); err != nil {
		t.Fatal(err)
	}
	_, err = messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
		MessageID: messageID, DedupeKey: "wrong-direction", Direction: "inbound",
		ReasonCode: messagelifecycle.ReasonAcceptanceInboundSMTP, OccurredAt: time.Now().UTC(),
	})
	if !errors.Is(err, messagelifecycle.ErrDirectionMismatch) {
		_ = tx.Rollback(ctx)
		t.Fatalf("AppendTx mismatch error = %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(status, '') FROM messages WHERE id=$1`, messageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != initialStatus || lifecycleCount(t, pool, messageID) != 0 {
		t.Fatalf("partial state survived rollback: status=%q lifecycle=%d", status, lifecycleCount(t, pool, messageID))
	}
	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE id='evt_direction_partial'`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("partial event survived rollback: %d", eventCount)
	}
}

func TestAppendTxStoresCanonicalTransition(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_append", "outbound")
	input := lifecycleInput("msg_append", "first")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := messagelifecycle.AppendTx(ctx, tx, input)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(got.ID, "mlt_") || len(got.ID) != len("mlt_")+32 {
		t.Fatalf("ID = %q, want mlt_ plus 32 lowercase hex characters", got.ID)
	}
	if got.OccurredAt.Location() != time.UTC {
		t.Fatalf("OccurredAt location = %v, want UTC", got.OccurredAt.Location())
	}
	if got.Evidence == nil || got.CorrelationIDs == nil {
		t.Fatalf("maps must be non-nil: evidence=%#v correlation=%#v", got.Evidence, got.CorrelationIDs)
	}
	if got.Reconstructed {
		t.Fatal("new transition must not be reconstructed")
	}
	if got.MessageID != input.MessageID || got.Direction != input.Direction || got.Recipient != input.Recipient || got.ReasonCode != input.ReasonCode {
		t.Fatalf("stored transition does not match input: %#v", got)
	}
}

func TestAppendTxIdenticalReplayReturnsOriginal(t *testing.T) {
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_replay", "outbound")
	input := lifecycleInput("msg_replay", "same")

	first := appendAndCommit(t, pool, input)
	second := appendAndCommit(t, pool, input)
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("replay differs\nfirst:  %#v\nsecond: %#v", first, second)
	}
	if got := lifecycleCount(t, pool, input.MessageID); got != 1 {
		t.Fatalf("row count = %d, want 1", got)
	}
}

func TestAppendTxReplayNormalizesEquivalentRepresentations(t *testing.T) {
	tests := []struct {
		name     string
		original func(*messagelifecycle.AppendInput)
		replayed func(*messagelifecycle.AppendInput)
	}{
		{
			name: "JSON key order",
			original: func(in *messagelifecycle.AppendInput) {
				in.Evidence = map[string]any{"smtp_detail": "accepted", "failure_code": "none"}
			},
			replayed: func(in *messagelifecycle.AppendInput) {
				in.Evidence = map[string]any{"failure_code": "none", "smtp_detail": "accepted"}
			},
		},
		{
			name: "nil and empty maps",
			original: func(in *messagelifecycle.AppendInput) {
				in.Evidence = nil
				in.CorrelationIDs = nil
			},
			replayed: func(in *messagelifecycle.AppendInput) {
				in.Evidence = map[string]any{}
				in.CorrelationIDs = map[string]string{}
			},
		},
		{
			name: "empty correlation filtering",
			original: func(in *messagelifecycle.AppendInput) {
				in.CorrelationIDs = map[string]string{"event_id": ""}
			},
			replayed: func(in *messagelifecycle.AppendInput) { in.CorrelationIDs = nil },
		},
		{
			name: "timezone and sub-microsecond timestamp",
			original: func(in *messagelifecycle.AppendInput) {
				in.OccurredAt = time.Date(2026, 7, 21, 12, 0, 0, 123456100, time.FixedZone("PDT", -7*60*60))
			},
			replayed: func(in *messagelifecycle.AppendInput) {
				in.OccurredAt = time.Date(2026, 7, 21, 19, 0, 0, 123456999, time.UTC)
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testutil.TestDB(t)
			messageID := fmt.Sprintf("msg_equivalent_%d", i)
			insertLifecycleMessage(t, pool, messageID, "outbound")
			originalInput := lifecycleInput(messageID, "equivalent")
			tt.original(&originalInput)
			original := appendAndCommit(t, pool, originalInput)
			replayedInput := lifecycleInput(messageID, "equivalent")
			tt.replayed(&replayedInput)
			replayed := appendAndCommit(t, pool, replayedInput)
			if !reflect.DeepEqual(replayed, original) {
				t.Fatalf("equivalent replay differs\noriginal: %#v\nreplayed: %#v", original, replayed)
			}
		})
	}
}

func TestAppendTxDedupeConflictForSemanticDifferences(t *testing.T) {
	baseTime := time.Date(2026, 7, 21, 12, 0, 0, 123000000, time.FixedZone("PDT", -7*60*60))
	tests := []struct {
		name   string
		modify func(*messagelifecycle.AppendInput)
		want   error
	}{
		{"reason", func(in *messagelifecycle.AppendInput) {
			in.ReasonCode = messagelifecycle.ReasonDeliveryTemporaryDelay
		}, messagelifecycle.ErrDedupeConflict},
		{"direction", func(in *messagelifecycle.AppendInput) { in.Direction = "inbound" }, messagelifecycle.ErrDirectionMismatch},
		{"recipient", func(in *messagelifecycle.AppendInput) { in.Recipient = "other@example.com" }, messagelifecycle.ErrDedupeConflict},
		{"evidence", func(in *messagelifecycle.AppendInput) { in.Evidence = map[string]any{"smtp_detail": "251 forwarded"} }, messagelifecycle.ErrDedupeConflict},
		{"correlation", func(in *messagelifecycle.AppendInput) { in.CorrelationIDs = map[string]string{"event_id": "evt_other"} }, messagelifecycle.ErrDedupeConflict},
		{"occurred_at", func(in *messagelifecycle.AppendInput) { in.OccurredAt = in.OccurredAt.Add(time.Second) }, messagelifecycle.ErrDedupeConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			pool := testutil.TestDB(t)
			messageID := "msg_conflict_" + tt.name
			insertLifecycleMessage(t, pool, messageID, "outbound")
			const sensitiveDedupeKey = "customer-secret-dedupe-key"
			originalInput := lifecycleInput(messageID, sensitiveDedupeKey)
			originalInput.OccurredAt = baseTime
			original := appendAndCommit(t, pool, originalInput)

			conflicting := originalInput
			tt.modify(&conflicting)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = messagelifecycle.AppendTx(ctx, tx, conflicting)
			if !errors.Is(err, tt.want) {
				_ = tx.Rollback(ctx)
				t.Fatalf("AppendTx error = %v, want %v", err, tt.want)
			}
			for _, sensitive := range []string{
				sensitiveDedupeKey, "250 2.0.0 accepted", "251 forwarded", "evt_123", "evt_other",
			} {
				if strings.Contains(err.Error(), sensitive) {
					_ = tx.Rollback(ctx)
					t.Fatalf("conflict error exposes sensitive content %q: %v", sensitive, err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if got := lifecycleCount(t, pool, messageID); got != 1 {
				t.Fatalf("row count = %d, want 1", got)
			}
			stored := loadOnlyTransition(t, pool, messageID)
			if !reflect.DeepEqual(stored, original) {
				t.Fatalf("original mutated\ngot:  %#v\nwant: %#v", stored, original)
			}
		})
	}
}

func TestAppendTxRollbackLeavesNoTransition(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_rollback", "outbound")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := messagelifecycle.AppendTx(ctx, tx, lifecycleInput("msg_rollback", "rollback")); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := lifecycleCount(t, pool, "msg_rollback"); got != 0 {
		t.Fatalf("row count after rollback = %d, want 0", got)
	}
}

func TestLifecycleOrderedListBreaksEqualTimestampsByID(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_order", "outbound")
	when := time.Date(2026, 7, 21, 19, 0, 0, 0, time.UTC)
	var wantIDs []string
	for _, key := range []string{"one", "two", "three"} {
		input := lifecycleInput("msg_order", key)
		input.OccurredAt = when
		wantIDs = append(wantIDs, appendAndCommit(t, pool, input).ID)
	}
	sort.Strings(wantIDs)

	rows, err := pool.Query(ctx, `
		SELECT id FROM message_lifecycle_transitions
		WHERE message_id = $1
		ORDER BY occurred_at ASC, id ASC
	`, "msg_order")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var gotIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		gotIDs = append(gotIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("ordered IDs = %v, want %v", gotIDs, wantIDs)
	}
}

func TestLifecycleMessageDeletionCascades(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_cascade", "outbound")
	appendAndCommit(t, pool, lifecycleInput("msg_cascade", "cascade"))
	if _, err := pool.Exec(ctx, `DELETE FROM messages WHERE id = 'msg_cascade'`); err != nil {
		t.Fatal(err)
	}
	if got := lifecycleCount(t, pool, "msg_cascade"); got != 0 {
		t.Fatalf("row count after message deletion = %d, want 0", got)
	}
}

func TestAppendTxConcurrentDuplicateConverges(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_concurrent", "outbound")
	input := lifecycleInput("msg_concurrent", "concurrent")
	winnerTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	winner, err := messagelifecycle.AppendTx(ctx, winnerTx, input)
	if err != nil {
		_ = winnerTx.Rollback(ctx)
		t.Fatal(err)
	}

	testCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	type result struct {
		transition messagelifecycle.MessageLifecycleTransition
		err        error
	}
	started := make(chan struct{})
	resultCh := make(chan result, 1)
	go func() {
		loserTx, err := pool.Begin(testCtx)
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		close(started)
		transition, err := messagelifecycle.AppendTx(testCtx, loserTx, input)
		if err == nil {
			err = loserTx.Commit(testCtx)
		} else {
			_ = loserTx.Rollback(testCtx)
		}
		resultCh <- result{transition: transition, err: err}
	}()
	<-started
	select {
	case result := <-resultCh:
		_ = winnerTx.Rollback(ctx)
		t.Fatalf("loser returned before uncommitted winner resolved: %#v", result)
	case <-time.After(100 * time.Millisecond):
	}
	if err := winnerTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("concurrent loser append: %v", result.err)
		}
		if !reflect.DeepEqual(result.transition, winner) {
			t.Fatalf("loser result differs\nwinner: %#v\nloser:  %#v", winner, result.transition)
		}
	case <-testCtx.Done():
		t.Fatalf("concurrent loser did not finish: %v", testCtx.Err())
	}
	if count := lifecycleCount(t, pool, input.MessageID); count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}
}

func TestListForMessageOwnedForeignAndMissing(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_list_scope", "outbound")
	store := lifecycleStore(t, pool)

	got, err := store.ListForMessage(ctx, "msg_list_scope", "agt_msg_list_scope")
	if err != nil || len(got) != 1 || got[0].ReasonCode != messagelifecycle.ReasonAcceptanceOutboundAPI {
		t.Fatalf("owned list = %#v, %v", got, err)
	}
	for _, tc := range []struct{ messageID, agentID string }{
		{"msg_list_scope", "agt_foreign"},
		{"msg_missing", "agt_msg_list_scope"},
	} {
		got, err := store.ListForMessage(ctx, tc.messageID, tc.agentID)
		if !errors.Is(err, messagelifecycle.ErrMessageNotFound) || got != nil {
			t.Fatalf("ListForMessage(%q,%q) = %#v, %v; want hidden not found", tc.messageID, tc.agentID, got, err)
		}
	}
}

func TestListForMessageHistoricalInboundAuthentication(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_list_auth", "inbound")
	if _, err := pool.Exec(ctx, `
		UPDATE messages
		SET method='smtp', authentication=$2, created_at=$3
		WHERE id=$1
	`, "msg_list_auth", authenticationJSON("pass"), reconstructBaseTime); err != nil {
		t.Fatal(err)
	}

	got, err := lifecycleStore(t, pool).ListForMessage(ctx, "msg_list_auth", "agt_msg_list_auth")
	if err != nil {
		t.Fatal(err)
	}
	assertReasons(t, got, messagelifecycle.ReasonAcceptanceInboundSMTP, messagelifecycle.ReasonAuthenticationDMARCPass)
	if findReason(got, messagelifecycle.ReasonAuthenticationDMARCPass).Evidence["authentication"] == nil {
		t.Fatal("historical authentication evidence missing")
	}
}

func TestListForMessageHistoricalOutboundRecipientEventAndSuppression(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_list_outbound", "outbound")
	if _, err := pool.Exec(ctx, `UPDATE messages SET method='smtp', created_at=$2 WHERE id=$1`, "msg_list_outbound", reconstructBaseTime); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_recipients (id, message_id, address, status, detail, updated_at)
		VALUES ('rcp_list', $1, 'delivered@example.com', 'delivered', 'state detail', $2)
	`, "msg_list_outbound", reconstructBaseTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppressions (id, user_id, address, source, source_message_id, created_at)
		VALUES ('sup_list', $1, 'bounce@example.com', 'bounce', $2, $3)
	`, "usr_msg_list_outbound", "msg_list_outbound", reconstructBaseTime.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	event := eventSnapshot("evt_list", "email.delivered", reconstructBaseTime.Add(time.Minute), map[string]any{
		"message_id": "msg_list_outbound", "delivered_to": "delivered@example.com", "smtp_detail": "event detail",
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO webhook_events (id, user_id, type, envelope, message_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, event.ID, "usr_msg_list_outbound", event.Type, event.Envelope, "msg_list_outbound", event.CreatedAt); err != nil {
		t.Fatal(err)
	}

	got, err := lifecycleStore(t, pool).ListForMessage(ctx, "msg_list_outbound", "agt_msg_list_outbound")
	if err != nil {
		t.Fatal(err)
	}
	assertReasons(t, got, messagelifecycle.ReasonAcceptanceOutboundAPI, messagelifecycle.ReasonDeliveryRecipientServerAccepted, messagelifecycle.ReasonSuppressionHardBounceApplied)
	delivery := findReason(got, messagelifecycle.ReasonDeliveryRecipientServerAccepted)
	if delivery.CorrelationIDs["event_id"] != "evt_list" || delivery.Evidence["smtp_detail"] != "event detail" {
		t.Fatalf("delivery did not prefer retained event: %#v", delivery)
	}
}

func TestListForMessageDeterministicPersistedPrecedenceOrderingAndNoWrites(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_list_merge", "outbound")
	when := reconstructBaseTime
	if _, err := pool.Exec(ctx, `UPDATE messages SET method='smtp', created_at=$2 WHERE id=$1`, "msg_list_merge", when); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_recipients (id, message_id, address, status, updated_at)
		VALUES ('rcp_merge', $1, 'a@example.com', 'delivered', $2)
	`, "msg_list_merge", when); err != nil {
		t.Fatal(err)
	}
	persisted := appendAndCommit(t, pool, messagelifecycle.AppendInput{
		MessageID: "msg_list_merge", DedupeKey: "persisted-delivery", Direction: "outbound",
		Recipient: "a@example.com", ReasonCode: messagelifecycle.ReasonDeliveryRecipientServerAccepted,
		OccurredAt: when, Evidence: map[string]any{"smtp_detail": "persisted"},
	})

	store := lifecycleStore(t, pool)
	before := lifecycleCount(t, pool, "msg_list_merge")
	first, err := store.ListForMessage(ctx, "msg_list_merge", "agt_msg_list_merge")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ListForMessage(ctx, "msg_list_merge", "agt_msg_list_merge")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeat reads differ\nfirst: %#v\nsecond:%#v", first, second)
	}
	if after := lifecycleCount(t, pool, "msg_list_merge"); after != before {
		t.Fatalf("ListForMessage wrote transitions: before=%d after=%d", before, after)
	}
	if len(first) != 2 || findReason(first, messagelifecycle.ReasonAcceptanceOutboundAPI) == nil {
		t.Fatalf("missing reconstructed earlier acceptance: %#v", first)
	}
	delivery := findReason(first, messagelifecycle.ReasonDeliveryRecipientServerAccepted)
	if delivery == nil || delivery.ID != persisted.ID || delivery.Reconstructed || delivery.Evidence["smtp_detail"] != "persisted" {
		t.Fatalf("persisted transition did not win: %#v", delivery)
	}
	for i := 1; i < len(first); i++ {
		if first[i].OccurredAt.Before(first[i-1].OccurredAt) || (first[i].OccurredAt.Equal(first[i-1].OccurredAt) && first[i].ID < first[i-1].ID) {
			t.Fatalf("not ordered by (occurred_at,id): %#v", first)
		}
	}
}

func TestListForMessageHistoricalSourceIndexes(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	tests := []struct {
		name, table, definition string
	}{
		{
			"idx_suppressions_source_message_created", "suppressions",
			"CREATE INDEX idx_suppressions_source_message_created ON public.suppressions USING btree (source_message_id, created_at, id) WHERE (source_message_id IS NOT NULL)",
		},
		{
			"idx_webhook_events_message_created", "webhook_events",
			"CREATE INDEX idx_webhook_events_message_created ON public.webhook_events USING btree (message_id, created_at, id) WHERE (message_id IS NOT NULL)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var definition string
			var valid bool
			err := pool.QueryRow(ctx, `
				SELECT pg_get_indexdef(i.indexrelid), i.indisvalid
				FROM pg_index i
				JOIN pg_class idx ON idx.oid = i.indexrelid
				JOIN pg_class tbl ON tbl.oid = i.indrelid
				WHERE idx.relname = $1 AND tbl.relname = $2
			`, tt.name, tt.table).Scan(&definition, &valid)
			if err != nil {
				t.Fatalf("load index: %v", err)
			}
			if !valid || definition != tt.definition {
				t.Fatalf("index valid=%v definition=%q, want valid and %q", valid, definition, tt.definition)
			}
		})
	}
}

func TestListForMessageHistoricalSourceIndexMigrationsEmbedded(t *testing.T) {
	tests := []struct {
		file, statement string
	}{
		{
			"074_suppressions_source_message_idx.sql",
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_suppressions_source_message_created\n    ON suppressions (source_message_id, created_at, id)\n    WHERE source_message_id IS NOT NULL;",
		},
		{
			"075_webhook_events_message_idx.sql",
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_webhook_events_message_created\n    ON webhook_events (message_id, created_at, id)\n    WHERE message_id IS NOT NULL;",
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			contents, err := migrations.FS.ReadFile(tt.file)
			if err != nil {
				t.Fatal(err)
			}
			text := string(contents)
			if !strings.Contains(text, "-- e2a:no-transaction") || !strings.Contains(text, "OPS NOTE — invalid-index recovery") || !strings.Contains(text, tt.statement) {
				t.Fatalf("migration missing required concurrent-index convention:\n%s", text)
			}
		})
	}
}

func TestListForMessageRejectsCrossTenantHistoricalPoisonRows(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_tenant_scope", "outbound")
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, name, google_subject)
		VALUES ('usr_poison', 'poison@example.com', '', 'subject_poison')
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppressions (id, user_id, address, source, source_message_id, created_at)
		VALUES ('sup_poison', 'usr_poison', 'suppression-secret@example.com', 'complaint', 'msg_tenant_scope', $1)
	`, reconstructBaseTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	event := eventSnapshot("evt_poison", "email.delivered", reconstructBaseTime.Add(2*time.Minute), map[string]any{
		"message_id": "msg_tenant_scope", "direction": "outbound",
		"delivered_to": "event-secret@example.com", "smtp_detail": "foreign tenant secret",
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO webhook_events (id, user_id, type, envelope, message_id, created_at)
		VALUES ($1, 'usr_poison', $2, $3, 'msg_tenant_scope', $4)
	`, event.ID, event.Type, event.Envelope, event.CreatedAt); err != nil {
		t.Fatal(err)
	}

	got, err := lifecycleStore(t, pool).ListForMessage(ctx, "msg_tenant_scope", "agt_msg_tenant_scope")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ReasonCode != messagelifecycle.ReasonAcceptanceOutboundAPI {
		t.Fatalf("cross-tenant historical rows leaked: %#v", got)
	}
}

func TestListForMessageUsesRepeatableReadOnlyTransaction(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	insertLifecycleMessage(t, pool, "msg_repeatable", "outbound")
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	config, err := pgxpool.ParseConfig(testutil.TestDBURL())
	if err != nil {
		t.Fatal(err)
	}
	tracer := &lifecycleQueryTracer{}
	config.ConnConfig.Tracer = tracer
	tracedPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracedPool.Close)
	if _, err := messagelifecycle.NewStore(tracedPool).ListForMessage(ctx, "msg_repeatable", "agt_msg_repeatable"); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, sql := range tracer.snapshot() {
		normalized := strings.ToLower(strings.Join(strings.Fields(sql), " "))
		if strings.HasPrefix(normalized, "begin") {
			found = true
			if !strings.Contains(normalized, "isolation level repeatable read") || !strings.Contains(normalized, "read only") {
				t.Fatalf("transaction begin = %q, want repeatable read and read only", normalized)
			}
		}
	}
	if !found {
		t.Fatalf("no BEGIN traced: %v", tracer.snapshot())
	}
}

type lifecycleQueryTracer struct {
	mu      sync.Mutex
	queries []string
}

func (t *lifecycleQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries = append(t.queries, data.SQL)
	t.mu.Unlock()
	return ctx
}

func (*lifecycleQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (t *lifecycleQueryTracer) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.queries...)
}

func authenticationJSON(status string) json.RawMessage {
	return json.RawMessage(`{"spf":{"status":"none","domain":null,"aligned":null},"dkim":[],"dmarc":{"status":"` + status + `","domain":null,"policy":null,"aligned_by":[]}}`)
}

func eventSnapshot(id, eventType string, at time.Time, data map[string]any) messagelifecycle.EventSnapshot {
	envelope, _ := json.Marshal(map[string]any{"type": eventType, "id": id, "created_at": at, "data": data})
	return messagelifecycle.EventSnapshot{ID: id, Type: eventType, Envelope: envelope, CreatedAt: at.Add(time.Hour)}
}

func assertReasons(t *testing.T, got []messagelifecycle.MessageLifecycleTransition, want ...messagelifecycle.ReasonCode) {
	t.Helper()
	reasons := make([]messagelifecycle.ReasonCode, len(got))
	for i := range got {
		reasons[i] = got[i].ReasonCode
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("reasons = %v, want %v", reasons, want)
	}
}

func findReason(items []messagelifecycle.MessageLifecycleTransition, reason messagelifecycle.ReasonCode) *messagelifecycle.MessageLifecycleTransition {
	for i := range items {
		if items[i].ReasonCode == reason {
			return &items[i]
		}
	}
	return nil
}

func lifecycleStore(t *testing.T, pool *pgxpool.Pool) *messagelifecycle.Store {
	t.Helper()
	if err := jobs.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return messagelifecycle.NewStore(pool)
}

func lifecycleInput(messageID, dedupeKey string) messagelifecycle.AppendInput {
	return messagelifecycle.AppendInput{
		MessageID:      messageID,
		DedupeKey:      dedupeKey,
		Direction:      "outbound",
		Recipient:      "person@example.com",
		ReasonCode:     messagelifecycle.ReasonDeliveryRecipientServerAccepted,
		Evidence:       map[string]any{"smtp_detail": "250 2.0.0 accepted"},
		CorrelationIDs: map[string]string{"event_id": "evt_123"},
		OccurredAt:     time.Date(2026, 7, 21, 12, 0, 0, 123000000, time.FixedZone("PDT", -7*60*60)),
	}
}

func insertLifecycleMessage(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, messageID, direction string) {
	t.Helper()
	ctx := context.Background()
	userID := "usr_" + messageID
	agentID := "agt_" + messageID
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO users (id, email, name, google_subject)
		VALUES ($1, $2, '', $3)
	`, userID, userID+"@example.com", userID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_identities (id, registered_domain, user_id, name)
		VALUES ($1, 'agents.e2a.dev', $2, '')
	`, agentID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO messages (id, agent_id, direction)
		VALUES ($1, $2, $3)
	`, messageID, agentID, direction); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func appendAndCommit(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, input messagelifecycle.AppendInput) messagelifecycle.MessageLifecycleTransition {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := messagelifecycle.AppendTx(ctx, tx, input)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return transition
}

func lifecycleCount(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, messageID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM message_lifecycle_transitions WHERE message_id = $1
	`, messageID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func loadOnlyTransition(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, messageID string) messagelifecycle.MessageLifecycleTransition {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM message_lifecycle_transitions WHERE message_id = $1
	`, messageID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("transition count = %d, want 1", count)
	}
	var transition messagelifecycle.MessageLifecycleTransition
	var recipient *string
	var evidence, correlations []byte
	err := pool.QueryRow(context.Background(), `
		SELECT id, message_id, dedupe_key, direction, recipient, stage, outcome, reason_code,
		       retryable, evidence, correlation_ids, occurred_at, reconstructed
		FROM message_lifecycle_transitions
		WHERE message_id = $1
	`, messageID).Scan(
		&transition.ID, &transition.MessageID, &transition.DedupeKey, &transition.Direction, &recipient,
		&transition.Stage, &transition.Outcome, &transition.ReasonCode,
		&transition.Retryable, &evidence, &correlations, &transition.OccurredAt,
		&transition.Reconstructed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recipient != nil {
		transition.Recipient = *recipient
	}
	if err := json.Unmarshal(evidence, &transition.Evidence); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(correlations, &transition.CorrelationIDs); err != nil {
		t.Fatal(err)
	}
	transition.OccurredAt = transition.OccurredAt.UTC()
	return transition
}

func lifecycleCatalogRows(t *testing.T, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) [][4]any {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT code, stage, outcome, retryable
		FROM message_lifecycle_reason_codes
		ORDER BY code
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result [][4]any
	for rows.Next() {
		var code, stage, outcome string
		var retryable bool
		if err := rows.Scan(&code, &stage, &outcome, &retryable); err != nil {
			t.Fatal(err)
		}
		result = append(result, [4]any{code, stage, outcome, retryable})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
