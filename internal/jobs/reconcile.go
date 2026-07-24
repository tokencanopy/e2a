package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultReconcileBatch bounds one reconcile scan. In steady state the stranded set is
// ~empty; under a systemic enqueue failure it caps how many rows one pass re-drives
// (one tx each) so an unhealthy River isn't amplified by fanning the whole backlog
// every tick — the remainder is picked up on the next pass.
const DefaultReconcileBatch = 1000

// deadJobStates is the SQL list of river_job states a job can never leave nor run
// again from. A row stamped with a job in one of these states (or whose job row was
// already pruned by River's reaper) is as stranded as a row with no job at all —
// nothing will ever work it again. Mirrors the state set
// outboundsend.TerminalReconcileWorker keys its safety-net scan on.
const deadJobStates = `('cancelled','discarded','completed')`

// ReconcileSpec describes one table's stranded-row reconcile.
//
// SECURITY: Table, JobColumn, and Where are COMPILE-TIME CONSTANTS supplied by the
// calling package — never runtime or user input. They are interpolated directly into
// SQL (there is no parameter form for identifiers), so callers must pass only literal
// strings. The only runtime value (the row id) is always a bound parameter.
type ReconcileSpec struct {
	// Table is the row table (e.g. "messages", "webhook_events").
	Table string
	// JobColumn is the nullable bigint column holding the enqueued River job id
	// (e.g. "send_job_id", "fanout_job_id"). A row is "stranded" when it matches
	// Where AND JobColumn IS NULL (plus, with RescueDeadJobs, when its stamped
	// job is dead — see below).
	JobColumn string
	// Where is the domain predicate identifying rows that SHOULD carry a job
	// (e.g. "direction='outbound' AND delivery_status='accepted'"). ANDed with
	// the strandedness condition; pass no leading/trailing "AND". Every query
	// binds the row table to the alias `t`, so unqualified column names resolve
	// to it — but with RescueDeadJobs the queries also join river_job as `r`, so
	// qualify any column whose name river_job shares (id, state, kind, queue,
	// attempt, ...) as t.<col> to avoid ambiguity.
	Where string
	// LogPrefix tags per-row enqueue-failure logs (e.g. "[outbound-reconcile]").
	LogPrefix string
	// Batch caps rows scanned per pass; 0 uses DefaultReconcileBatch.
	Batch int
	// RescueDeadJobs additionally treats a row as stranded when JobColumn IS NOT
	// NULL but the stamped river_job no longer exists (pruned by River's reaper)
	// or is in a terminal state (cancelled/discarded/completed) — a job that can
	// never run again while the row still matches Where. Same "job is dead,
	// reconcile the row" test as outboundsend's TerminalReconcileWorker, but the
	// remedy here is RE-DRIVING (fresh job, re-stamped id), so it is OPT-IN:
	// only set it when re-running the work is both idempotent and desired.
	// outboundsend, for example, must NOT re-enqueue a discarded send job (its
	// terminal reconciler settles those rows as sent/failed instead — re-driving
	// would resend email); the webhook pipeline's fan-out and delivery rows DO
	// want re-driving, because their at-least-once contract already tolerates it.
	//
	// Cost note: the dead-job scan cannot use the JobColumn-IS-NULL partial
	// indexes; it walks the rows matching Where with a stamped job (the in-flight
	// set) and probes river_job by primary key per row. Callers' Where predicates
	// keep that set small and TTL-bounded; the plain IS NULL scan still runs
	// first and stays index-backed.
	RescueDeadJobs bool
}

// ReconcilePending re-drives every stranded row matching spec.Where: rows whose
// spec.JobColumn IS NULL and — when spec.RescueDeadJobs is set — rows whose stamped
// river_job is missing or terminal. It scans up to spec.Batch ids, then per id opens a
// tx, re-checks strandedness under FOR UPDATE (skipping rows another process or a
// prior pass already gave a live job), calls enqueueTx to insert the River job in that
// tx, and stamps the returned job id back onto the row — all atomically. Idempotent:
// the FOR UPDATE re-check means a re-run (or a concurrent replica) never
// double-enqueues. A per-row failure is logged and skipped (the next pass retries it);
// the returned count is the rows enqueued.
//
// This is the shared body behind every domain's startup cutover + live reconcile
// worker (outboundsend, inboundprocess, hitlnotify, webhookdelivery, webhookpub
// fan-out). Each domain supplies a ReconcileSpec + its own EnqueueXTx.
func ReconcilePending(ctx context.Context, pool *pgxpool.Pool, spec ReconcileSpec,
	enqueueTx func(ctx context.Context, tx pgx.Tx, id string) (int64, error)) (int, error) {

	batch := spec.Batch
	if batch <= 0 {
		batch = DefaultReconcileBatch
	}

	ids, err := scanIDs(ctx, pool, fmt.Sprintf(
		`SELECT t.id FROM %s t WHERE %s AND t.%s IS NULL LIMIT $1`,
		spec.Table, spec.Where, spec.JobColumn),
		batch)
	if err != nil {
		return 0, err
	}
	if spec.RescueDeadJobs && len(ids) < batch {
		deadIDs, err := scanIDs(ctx, pool, fmt.Sprintf(
			`SELECT t.id FROM %s t
			   LEFT JOIN river_job r ON r.id = t.%s
			  WHERE %s AND t.%s IS NOT NULL
			    AND (r.id IS NULL OR r.state IN %s)
			  LIMIT $1`,
			spec.Table, spec.JobColumn, spec.Where, spec.JobColumn, deadJobStates),
			batch-len(ids))
		if err != nil {
			return 0, err
		}
		ids = append(ids, deadIDs...)
	}

	// The per-row re-check must apply the SAME strandedness test under the row
	// lock: without RescueDeadJobs any stamped id means "already enqueued"; with
	// it, a stamped id only counts when its job is still alive. The rescue
	// variant also re-verifies Where (scanning no row = the row terminalized or
	// vanished since the scan — skip), so a row that went terminal between scan
	// and lock is never given a fresh job.
	var recheckSQL string
	if spec.RescueDeadJobs {
		recheckSQL = fmt.Sprintf(
			`SELECT t.%s, (t.%s IS NOT NULL AND (r.id IS NULL OR r.state IN %s))
			   FROM %s t
			   LEFT JOIN river_job r ON r.id = t.%s
			  WHERE t.id = $1 AND (%s)
			  FOR UPDATE OF t`,
			spec.JobColumn, spec.JobColumn, deadJobStates, spec.Table, spec.JobColumn, spec.Where)
	} else {
		recheckSQL = fmt.Sprintf(`SELECT %s, false FROM %s WHERE id=$1 FOR UPDATE`, spec.JobColumn, spec.Table)
	}
	stampSQL := fmt.Sprintf(`UPDATE %s SET %s=$2 WHERE id=$1`, spec.Table, spec.JobColumn)

	n := 0
	for _, id := range ids {
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			// Re-check under a row lock: another process (or a prior run) may have
			// enqueued it already. Skip if the row now carries a live job — or, in
			// rescue mode, no longer matches Where at all.
			var jobID *int64
			var jobDead bool
			if err := tx.QueryRow(ctx, recheckSQL, id).Scan(&jobID, &jobDead); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil // row gone or no longer stranded-eligible — nothing to do
				}
				return err
			}
			if jobID != nil && !jobDead {
				return nil // already enqueued (live job)
			}
			newJobID, err := enqueueTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, stampSQL, id, newJobID); err != nil {
				return err
			}
			n++
			return nil
		}); err != nil {
			log.Printf("%s enqueue %s: %v", spec.LogPrefix, id, err)
		}
	}
	return n, nil
}

// scanIDs runs one bounded stranded-row scan and returns the matched ids.
func scanIDs(ctx context.Context, pool *pgxpool.Pool, sql string, limit int) ([]string, error) {
	rows, err := pool.Query(ctx, sql, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
