package jobs_test

import (
	"context"
	"testing"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// TestStampJobArg: the key is added once, existing fields survive, a present
// key is never overwritten, and a missing job is a no-op rather than an
// error (River may have pruned it).
func TestStampJobArg(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO river_job (args, kind, max_attempts) VALUES ('{"message_id":"msg_1"}'::jsonb, 'argstamp_test', 3) RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}

	if err := jobs.StampJobArg(ctx, pool, id, "operation_ref", map[string]any{"v": 1, "id": "op_1"}); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if err := jobs.StampJobArg(ctx, pool, id, "operation_ref", map[string]any{"v": 1, "id": "op_2"}); err != nil {
		t.Fatalf("second stamp: %v", err)
	}
	var messageID, opID string
	if err := pool.QueryRow(ctx,
		`SELECT args->>'message_id', args->'operation_ref'->>'id' FROM river_job WHERE id = $1`, id,
	).Scan(&messageID, &opID); err != nil {
		t.Fatal(err)
	}
	if messageID != "msg_1" || opID != "op_1" {
		t.Fatalf("args = message_id=%q operation_ref.id=%q, want msg_1 / op_1 (first stamp wins, existing field kept)", messageID, opID)
	}

	if err := jobs.StampJobArg(ctx, pool, id+1000, "operation_ref", "x"); err != nil {
		t.Fatalf("missing job must be a no-op, got %v", err)
	}

	// SetJobArg replaces the key and keeps the rest.
	if err := jobs.SetJobArg(ctx, pool, id, "operation_ref", map[string]any{"v": 1, "id": "op_3"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT args->>'message_id', args->'operation_ref'->>'id' FROM river_job WHERE id = $1`, id,
	).Scan(&messageID, &opID); err != nil {
		t.Fatal(err)
	}
	if messageID != "msg_1" || opID != "op_3" {
		t.Fatalf("after set: message_id=%q operation_ref.id=%q, want msg_1 / op_3", messageID, opID)
	}
	if err := jobs.SetJobArg(ctx, nil, id, "k", "v"); err == nil {
		t.Fatal("nil database must be refused")
	}
	if err := jobs.SetJobArg(ctx, pool, id, "k", make(chan int)); err == nil {
		t.Fatal("unencodable value must be refused")
	}
}

// TestStampJobArgRefusesBadInputs: no database and an unencodable value are
// errors before any SQL runs; a failed statement is reported, not swallowed.
func TestStampJobArgRefusesBadInputs(t *testing.T) {
	ctx := context.Background()
	if err := jobs.StampJobArg(ctx, nil, 1, "k", "v"); err == nil {
		t.Fatal("nil database must be refused")
	}
	pool := testutil.TestDB(t)
	if err := jobs.StampJobArg(ctx, pool, 1, "k", make(chan int)); err == nil {
		t.Fatal("unencodable value must be refused")
	}
	if err := jobs.StampJobArg(ctx, pool, 1, "k", "v"); err == nil {
		// river_job may not exist on this fresh pool (no Migrate): the
		// statement fails and the error must surface.
		if _, qerr := pool.Exec(ctx, `SELECT 1 FROM river_job LIMIT 1`); qerr != nil {
			t.Fatal("statement failure must be reported")
		}
	}
}
