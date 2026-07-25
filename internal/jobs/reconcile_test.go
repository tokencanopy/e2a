package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// reconcileScratch creates a throwaway row table shaped like a domain table
// ReconcilePending works against (id + a nullable bigint job column) and
// returns the spec pointing at it. The table is dropped on test cleanup so
// repeated runs and sibling tests never see each other's rows — the shared
// truncate-between-tests harness does not know about it.
func reconcileScratch(t *testing.T, pool *pgxpool.Pool, table string) jobs.ReconcileSpec {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id text PRIMARY KEY, state text NOT NULL, send_job_id bigint)`, table)); err != nil {
		t.Fatalf("create scratch table: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)); err != nil {
			t.Errorf("drop scratch table: %v", err)
		}
	})
	return jobs.ReconcileSpec{
		Table:     table,
		JobColumn: "send_job_id",
		Where:     "state = 'pending'",
		LogPrefix: "[jobs-test-reconcile]",
	}
}

func insertReconcileRow(t *testing.T, pool *pgxpool.Pool, table, id, state string, jobID *int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(
		`INSERT INTO %s (id, state, send_job_id) VALUES ($1, $2, $3)`, table), id, state, jobID); err != nil {
		t.Fatalf("insert row %s: %v", id, err)
	}
}

func rowJobID(t *testing.T, pool *pgxpool.Pool, table, id string) *int64 {
	t.Helper()
	var jobID *int64
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT send_job_id FROM %s WHERE id = $1`, table), id).Scan(&jobID); err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	return jobID
}

// stampEnqueue records the ids it was asked to enqueue and hands out
// sequential job ids, mimicking a domain's EnqueueXTx.
type stampEnqueue struct {
	ids  []string
	next int64
}

func (e *stampEnqueue) enqueue(_ context.Context, _ pgx.Tx, id string) (int64, error) {
	e.ids = append(e.ids, id)
	e.next++
	return 1000 + e.next, nil
}

// TestReconcilePending_EnqueuesOnlyStrandedRows covers the happy path: rows
// matching Where with a NULL job column get enqueued and stamped; rows already
// stamped or not matching Where are left alone.
func TestReconcilePending_EnqueuesOnlyStrandedRows(t *testing.T) {
	pool := testutil.TestDB(t)
	spec := reconcileScratch(t, pool, "jobs_reconcile_happy")
	ctx := context.Background()

	stamped := int64(42)
	insertReconcileRow(t, pool, spec.Table, "stranded", "pending", nil)
	insertReconcileRow(t, pool, spec.Table, "already", "pending", &stamped)
	insertReconcileRow(t, pool, spec.Table, "other-state", "done", nil)

	enq := &stampEnqueue{}
	// Batch 0 exercises the DefaultReconcileBatch fallback.
	res, err := jobs.ReconcilePending(ctx, pool, spec, enq.enqueue)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if res.Total() != 1 || res.Enqueued != 1 {
		t.Errorf("result = %+v, want exactly 1 on the IS-NULL arm", res)
	}
	if len(enq.ids) != 1 || enq.ids[0] != "stranded" {
		t.Errorf("enqueueTx called with %v, want [stranded]", enq.ids)
	}
	if got := rowJobID(t, pool, spec.Table, "stranded"); got == nil || *got != 1001 {
		t.Errorf("stranded row job id = %v, want 1001", got)
	}
	if got := rowJobID(t, pool, spec.Table, "already"); got == nil || *got != 42 {
		t.Errorf("already-stamped row job id = %v, want untouched 42", got)
	}
	if got := rowJobID(t, pool, spec.Table, "other-state"); got != nil {
		t.Errorf("non-matching row job id = %v, want NULL", *got)
	}
}

// TestReconcilePending_IdempotentSecondRun proves the documented guarantee: a
// re-run (startup cutover racing a live reconcile worker) never double-enqueues.
func TestReconcilePending_IdempotentSecondRun(t *testing.T) {
	pool := testutil.TestDB(t)
	spec := reconcileScratch(t, pool, "jobs_reconcile_idem")
	ctx := context.Background()

	insertReconcileRow(t, pool, spec.Table, "r1", "pending", nil)
	insertReconcileRow(t, pool, spec.Table, "r2", "pending", nil)

	enq := &stampEnqueue{}
	if res, err := jobs.ReconcilePending(ctx, pool, spec, enq.enqueue); err != nil || res.Total() != 2 {
		t.Fatalf("first pass: res=%+v err=%v, want 2 nil", res, err)
	}
	res, err := jobs.ReconcilePending(ctx, pool, spec, enq.enqueue)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if res.Total() != 0 {
		t.Errorf("second pass enqueued %d rows, want 0", res.Total())
	}
	if len(enq.ids) != 2 {
		t.Errorf("enqueueTx called %d times total, want 2", len(enq.ids))
	}
}

// TestReconcilePending_EnqueueFailureSkipsRowForNextPass covers the per-row
// failure path: a failing row is logged and skipped (no error returned), its
// job column stays NULL so the NEXT pass retries it, while healthy rows in the
// same pass still commit.
func TestReconcilePending_EnqueueFailureSkipsRowForNextPass(t *testing.T) {
	pool := testutil.TestDB(t)
	spec := reconcileScratch(t, pool, "jobs_reconcile_fail")
	ctx := context.Background()

	insertReconcileRow(t, pool, spec.Table, "bad", "pending", nil)
	insertReconcileRow(t, pool, spec.Table, "good", "pending", nil)

	boom := errors.New("boom")
	failOnce := true
	var calls atomic.Int64
	enqueue := func(_ context.Context, _ pgx.Tx, id string) (int64, error) {
		calls.Add(1)
		if id == "bad" && failOnce {
			return 0, boom
		}
		return 2001, nil
	}

	res, err := jobs.ReconcilePending(ctx, pool, spec, enqueue)
	if err != nil {
		t.Fatalf("first pass returned error %v, want nil (per-row failures are skipped)", err)
	}
	if res.Total() != 1 {
		t.Errorf("first pass enqueued %d rows, want 1 (only the healthy row)", res.Total())
	}
	if got := rowJobID(t, pool, spec.Table, "bad"); got != nil {
		t.Errorf("failed row job id = %v, want NULL (retry next pass)", *got)
	}
	if got := rowJobID(t, pool, spec.Table, "good"); got == nil || *got != 2001 {
		t.Errorf("healthy row job id = %v, want 2001", got)
	}

	failOnce = false
	res, err = jobs.ReconcilePending(ctx, pool, spec, enqueue)
	if err != nil {
		t.Fatalf("retry pass: %v", err)
	}
	if res.Total() != 1 {
		t.Errorf("retry pass enqueued %d rows, want 1 (the previously failed row)", res.Total())
	}
	if got := rowJobID(t, pool, spec.Table, "bad"); got == nil || *got != 2001 {
		t.Errorf("retried row job id = %v, want 2001", got)
	}
}

// TestReconcilePending_BatchCapsOnePass covers spec.Batch: one pass scans at
// most Batch rows, and the remainder is picked up by the following passes.
func TestReconcilePending_BatchCapsOnePass(t *testing.T) {
	pool := testutil.TestDB(t)
	spec := reconcileScratch(t, pool, "jobs_reconcile_batch")
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		insertReconcileRow(t, pool, spec.Table, fmt.Sprintf("r%d", i), "pending", nil)
	}

	spec.Batch = 2
	enq := &stampEnqueue{}
	res, err := jobs.ReconcilePending(ctx, pool, spec, enq.enqueue)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if res.Total() != 2 {
		t.Errorf("pass 1 enqueued %d rows, want capped at Batch=2", res.Total())
	}
	if len(enq.ids) != 2 {
		t.Errorf("pass 1 enqueueTx calls = %d, want 2", len(enq.ids))
	}

	total := res.Total()
	for res.Total() > 0 {
		res, err = jobs.ReconcilePending(ctx, pool, spec, enq.enqueue)
		if err != nil {
			t.Fatalf("drain pass: %v", err)
		}
		total += res.Total()
	}
	if total != 5 {
		t.Errorf("drained %d rows total, want 5", total)
	}
	if len(enq.ids) != 5 {
		t.Errorf("enqueueTx called %d times total, want 5 (each row exactly once)", len(enq.ids))
	}
}

// TestReconcilePending_SkipsRowStampedConcurrently covers the FOR UPDATE
// re-check: a row stranded at scan time but stamped by another process before
// this pass's per-row transaction takes its lock must be skipped, not
// double-enqueued. Deterministic: hold the row lock in an open transaction
// that stamps the row, run the reconcile (its scan sees the pre-commit NULL,
// its FOR UPDATE blocks), then commit.
func TestReconcilePending_SkipsRowStampedConcurrently(t *testing.T) {
	pool := testutil.TestDB(t)
	spec := reconcileScratch(t, pool, "jobs_reconcile_race")
	ctx := context.Background()

	insertReconcileRow(t, pool, spec.Table, "raced", "pending", nil)

	stamper, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin stamper tx: %v", err)
	}
	if _, err := stamper.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET send_job_id = 777 WHERE id = 'raced'`, spec.Table)); err != nil {
		t.Fatalf("stamper update: %v", err)
	}

	enq := &stampEnqueue{}
	type result struct {
		res jobs.ReconcileResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := jobs.ReconcilePending(ctx, pool, spec, enq.enqueue)
		done <- result{res, err}
	}()

	// Give the reconcile time to scan (sees committed NULL) and block on the
	// stamper's lock in its FOR UPDATE re-check.
	time.Sleep(500 * time.Millisecond)
	if err := stamper.Commit(ctx); err != nil {
		t.Fatalf("commit stamper tx: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("ReconcilePending: %v", res.err)
		}
		if res.res.Total() != 0 {
			t.Errorf("enqueued %d rows, want 0 (row was stamped concurrently)", res.res.Total())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile did not finish after the blocking lock was released")
	}
	if len(enq.ids) != 0 {
		t.Errorf("enqueueTx called with %v, want no calls (no double-enqueue)", enq.ids)
	}
	if got := rowJobID(t, pool, spec.Table, "raced"); got == nil || *got != 777 {
		t.Errorf("row job id = %v, want the concurrent stamp 777", got)
	}
}

// insertRiverJob inserts a bare river_job row in the given state and returns
// its id, so dead-job-rescue tests can stamp rows with jobs in every state.
// Terminal states get the finalized_at that river_job's CHECK constraint
// requires. The caller must have applied River's schema (jobs.Migrate).
func insertRiverJob(t *testing.T, pool *pgxpool.Pool, state string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO river_job (state, kind, args, max_attempts, finalized_at)
		 VALUES ($1::river_job_state, 'jobs_reconcile_test', '{}'::jsonb, 4,
		         CASE WHEN $1 IN ('cancelled','completed','discarded') THEN now() END)
		 RETURNING id`, state).Scan(&id); err != nil {
		t.Fatalf("insert river_job(%s): %v", state, err)
	}
	return id
}

// TestReconcilePending_RescueDeadJobs covers the opt-in dead-job rescue
// (spec.RescueDeadJobs): a row whose stamped job id refers to a river_job that
// no longer exists (pruned) or is in a terminal state (cancelled / discarded /
// completed) can never be worked again — it must be rescued (fresh job
// enqueued, id re-stamped) exactly like the JobColumn IS NULL strand. Rows
// whose stamped job is still live (available/running/scheduled/retryable) and
// rows not matching Where stay untouched, and the plain IS NULL strand keeps
// working alongside.
func TestReconcilePending_RescueDeadJobs(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	spec := reconcileScratch(t, pool, "jobs_reconcile_dead")
	// The rescue queries join river_job r, whose own `state` column would make
	// the scratch table's unqualified `state` ambiguous — qualify with the `t`
	// alias every ReconcilePending query binds to the row table.
	spec.Where = "t.state = 'pending'"
	spec.RescueDeadJobs = true

	missing := int64(1) << 60 // never a real river_job id
	dead := map[string]int64{
		"dead-missing":   missing,
		"dead-cancelled": insertRiverJob(t, pool, "cancelled"),
		"dead-discarded": insertRiverJob(t, pool, "discarded"),
		"dead-completed": insertRiverJob(t, pool, "completed"),
	}
	live := map[string]int64{
		"live-available": insertRiverJob(t, pool, "available"),
		"live-running":   insertRiverJob(t, pool, "running"),
		"live-scheduled": insertRiverJob(t, pool, "scheduled"),
		"live-retryable": insertRiverJob(t, pool, "retryable"),
	}
	for id, jobID := range dead {
		jid := jobID
		insertReconcileRow(t, pool, spec.Table, id, "pending", &jid)
	}
	for id, jobID := range live {
		jid := jobID
		insertReconcileRow(t, pool, spec.Table, id, "pending", &jid)
	}
	insertReconcileRow(t, pool, spec.Table, "stranded-null", "pending", nil)
	doneDead := dead["dead-discarded"]
	insertReconcileRow(t, pool, spec.Table, "done-dead-job", "done", &doneDead)

	enq := &stampEnqueue{}
	res, err := jobs.ReconcilePending(ctx, pool, spec, enq.enqueue)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if res.Enqueued != 1 || res.Rescued != 4 {
		t.Errorf("result = %+v, want Enqueued=1 (NULL row) Rescued=4 (dead-job rows)", res)
	}
	for id, oldJobID := range dead {
		got := rowJobID(t, pool, spec.Table, id)
		if got == nil || *got == oldJobID {
			t.Errorf("%s job id = %v, want re-stamped fresh id (old %d)", id, got, oldJobID)
		}
	}
	for id, jobID := range live {
		if got := rowJobID(t, pool, spec.Table, id); got == nil || *got != jobID {
			t.Errorf("%s job id = %v, want untouched live job %d", id, got, jobID)
		}
	}
	if got := rowJobID(t, pool, spec.Table, "stranded-null"); got == nil {
		t.Error("stranded-null row was not enqueued alongside the dead-job rescues")
	}
	if got := rowJobID(t, pool, spec.Table, "done-dead-job"); got == nil || *got != doneDead {
		t.Errorf("non-matching row job id = %v, want untouched %d (Where must still gate rescue)", got, doneDead)
	}
}

// TestReconcilePending_RescueWhereGatesDeadArmOnly pins RescueWhere's scope: it
// narrows ONLY the dead-job arm (scan + locked re-check) — a gated dead-job row
// is skipped, an ungated one is rescued, and the IS NULL arm ignores the gate
// entirely (a "gated" NULL row is still enqueued).
func TestReconcilePending_RescueWhereGatesDeadArmOnly(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	spec := reconcileScratch(t, pool, "jobs_reconcile_gate")
	spec.Where = "t.state = 'pending'"
	spec.RescueDeadJobs = true
	spec.RescueWhere = "t.id NOT LIKE 'gated-%'"

	gatedDead := insertRiverJob(t, pool, "discarded")
	openDead := insertRiverJob(t, pool, "discarded")
	insertReconcileRow(t, pool, spec.Table, "gated-dead", "pending", &gatedDead)
	insertReconcileRow(t, pool, spec.Table, "open-dead", "pending", &openDead)
	insertReconcileRow(t, pool, spec.Table, "gated-null", "pending", nil)

	enq := &stampEnqueue{}
	res, err := jobs.ReconcilePending(ctx, pool, spec, enq.enqueue)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if res.Enqueued != 1 || res.Rescued != 1 {
		t.Errorf("result = %+v, want Enqueued=1 (NULL arm ignores gate) Rescued=1 (only the ungated dead row)", res)
	}
	if got := rowJobID(t, pool, spec.Table, "gated-dead"); got == nil || *got != gatedDead {
		t.Errorf("gated dead row job id = %v, want untouched %d (RescueWhere must gate it)", got, gatedDead)
	}
	if got := rowJobID(t, pool, spec.Table, "open-dead"); got == nil || *got == openDead {
		t.Errorf("ungated dead row job id = %v, want re-stamped fresh id (old %d)", got, openDead)
	}
	if got := rowJobID(t, pool, spec.Table, "gated-null"); got == nil {
		t.Error("NULL-job row was not enqueued — RescueWhere must not gate the IS NULL arm")
	}
}

// TestReconcilePending_DefaultSpecIgnoresDeadJobs pins the default (opt-out)
// behavior: without RescueDeadJobs a stamped row is NEVER re-driven, even when
// its job is gone. Load-bearing for outboundsend, which shares this machinery
// but must not re-enqueue a discarded send job (its TerminalReconcileWorker
// settles those rows terminally instead — re-driving would resend email).
func TestReconcilePending_DefaultSpecIgnoresDeadJobs(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	spec := reconcileScratch(t, pool, "jobs_reconcile_nodead")

	missing := int64(1) << 60
	discarded := insertRiverJob(t, pool, "discarded")
	insertReconcileRow(t, pool, spec.Table, "dead-missing", "pending", &missing)
	insertReconcileRow(t, pool, spec.Table, "dead-discarded", "pending", &discarded)

	enq := &stampEnqueue{}
	res, err := jobs.ReconcilePending(ctx, pool, spec, enq.enqueue)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if res.Total() != 0 || len(enq.ids) != 0 {
		t.Errorf("default spec enqueued res=%+v ids=%v, want 0 (dead-job rescue must be opt-in)", res, enq.ids)
	}
	if got := rowJobID(t, pool, spec.Table, "dead-missing"); got == nil || *got != missing {
		t.Errorf("dead-missing job id = %v, want untouched %d", got, missing)
	}
	if got := rowJobID(t, pool, spec.Table, "dead-discarded"); got == nil || *got != discarded {
		t.Errorf("dead-discarded job id = %v, want untouched %d", got, discarded)
	}
}

// TestReconcilePending_BadSpecReturnsError covers the scan-query error return:
// a spec pointing at a nonexistent table surfaces the error instead of
// swallowing it.
func TestReconcilePending_BadSpecReturnsError(t *testing.T) {
	pool := testutil.TestDB(t)
	spec := jobs.ReconcileSpec{
		Table:     "jobs_reconcile_nonexistent_table",
		JobColumn: "send_job_id",
		Where:     "state = 'pending'",
		LogPrefix: "[jobs-test-reconcile]",
	}
	enq := &stampEnqueue{}
	res, err := jobs.ReconcilePending(context.Background(), pool, spec, enq.enqueue)
	if err == nil {
		t.Fatal("expected an error for a nonexistent table, got nil")
	}
	if res.Total() != 0 {
		t.Errorf("result = %+v, want 0 on scan error", res)
	}
	if len(enq.ids) != 0 {
		t.Errorf("enqueueTx called with %v, want no calls on scan error", enq.ids)
	}
}
