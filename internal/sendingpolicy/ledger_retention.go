package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/riverqueue/river"
)

// This file is the retention janitor for the sending ledger: the tables the
// gate writes on every provider-bound send (operations, attempt reservations,
// day counters) and the protection audit/outbox tables beside them. Until it
// existed nothing deleted any of them.
//
// It deliberately does NOT touch the B8 feedback provenance (correlations,
// recipient HMACs, feedback events) or the daily outcome aggregates — GCFeedback
// owns those — and it never deletes the append-only policy authority
// (runtime policy, its events, the runtime attestation and its events, the
// operator-recipient registry, the ramp grandfathering marker). Those rows are
// few, security-relevant, and have no retention horizon.
//
// Retention rules and their sources (spec 2026-08-19-sending-abuse-prevention
// §5.1 "Reservation semantics", §5.2, §9, §10):
//
//   - sending_budget_counters: "A retention janitor removes day-counter rows
//     after two closed UTC days". A row for day D is deleted once D and D+1
//     have both closed, i.e. day <= today-2 on the ledger's own clock. Today
//     and yesterday always survive.
//   - sending_provider_operations + sending_budget_reservations: "provider-
//     operation/reservation rows after 30 days", measured from the expires_at
//     each row was stamped with at creation (operationTTL; migration 113's
//     reservation default). An operation goes together with every attempt it
//     owns, and only once every one of those attempts is past its own expiry.
//   - sending_protection_notice_events/_deliveries: "sent, failed, and skipped
//     rows remain for 30 days from event creation and then the event/delivery
//     pair is removed". A pending delivery is never deleted by age.
//   - account_sending_control_events: 90-day security retention, stamped as
//     expires_at from the effective policy's sending_control_audit_retention_days
//     when the event is written.
//   - external_sending_access_events: the same control-audit retention, stamped
//     the same way (migration 121: "reaped only by its own expires_at").

// ledgerCounterAgeDays: a counter for day D is deleted once day <= today - 2.
const ledgerCounterAgeDays = 2

// noticeTerminalRetention is how long a finished notice event and its
// deliveries are kept after the event was created (spec §5.2, §10).
const noticeTerminalRetention = 30 * 24 * time.Hour

// ledgerBatchSize and ledgerMaxBatches bound one run: every batch is its own
// short transaction, and a table stops after ledgerMaxBatches so that the
// first run over weeks of production backlog neither holds a transaction open
// for long nor starves the tables after it. The next hourly run resumes.
const (
	ledgerBatchSize  = 1000
	ledgerMaxBatches = 50
)

// ledgerLockTimeout and ledgerStatementTimeout bound one batch. SKIP LOCKED
// already steps around any row a sender holds; lock_timeout covers the
// remaining waits (a relation lock, a row a concurrent writer locked after the
// victim scan) so the janitor fails fast rather than queueing behind — or in
// front of — the send path. A timeout fails only that table for this run.
const (
	ledgerLockTimeout      = "2s"
	ledgerStatementTimeout = "30s"
)

// ledgerRetentionInterval paces the janitor. Nothing it removes is urgent.
const ledgerRetentionInterval = time.Hour

// Ledger retention table labels. They double as the metric's closed `table`
// label set (e2a_janitor_rows_deleted_total), so they carry no identifiers.
const (
	LedgerTableOperations       = "sending_provider_operations"
	LedgerTableReservations     = "sending_budget_reservations"
	LedgerTableCounters         = "sending_budget_counters"
	LedgerTableNoticeEvents     = "sending_protection_notice_events"
	LedgerTableNoticeDeliveries = "sending_protection_notice_deliveries"
	LedgerTableControlEvents    = "account_sending_control_events"
	LedgerTableAccessEvents     = "external_sending_access_events"
)

// Run outcomes for the e2a_sending_ledger_retention_runs_total metric.
const (
	// LedgerRunComplete: every table was drained to its horizon.
	LedgerRunComplete = "complete"
	// LedgerRunPartial: at least one table hit its per-run batch cap; the
	// backlog continues next run. Expected on the first runs after rollout.
	LedgerRunPartial = "partial"
	// LedgerRunFailed: at least one table's batch failed (lock or statement
	// timeout, connection loss). Non-fatal: the next run retries.
	LedgerRunFailed = "failed"
)

// LedgerRetentionObserver receives the janitor's bounded samples. The process
// telemetry backend (telemetry.Metrics) satisfies it. Never carries an id.
type LedgerRetentionObserver interface {
	JanitorRowsDeleted(table string, count int)
	SendingLedgerRetentionRun(outcome string)
}

var ledgerObserver atomic.Value // ledgerObserverBox

type ledgerObserverBox struct{ o LedgerRetentionObserver }

// SetLedgerRetentionObserver installs the process-wide janitor observer. Set
// once at startup by the composition root; nil disables it.
func SetLedgerRetentionObserver(o LedgerRetentionObserver) {
	ledgerObserver.Store(ledgerObserverBox{o})
}

func currentLedgerObserver() LedgerRetentionObserver {
	if b, ok := ledgerObserver.Load().(ledgerObserverBox); ok {
		return b.o
	}
	return nil
}

// LedgerRetentionStats reports one run. Deleted is keyed by table label.
type LedgerRetentionStats struct {
	Deleted map[string]int64
	// Capped names the tables that hit the per-run batch cap.
	Capped []string
	// Failed maps a table label to the error that stopped it this run.
	Failed map[string]error
	// RetainedOperations counts expired operations kept because an attempt,
	// a live job, an in-flight message, or a pending notice still needs them
	// (capped at retainedCountCap). Diagnostic only.
	RetainedOperations int64
}

// Outcome classifies the run for the run-outcome metric.
func (s LedgerRetentionStats) Outcome() string {
	switch {
	case len(s.Failed) > 0:
		return LedgerRunFailed
	case len(s.Capped) > 0:
		return LedgerRunPartial
	default:
		return LedgerRunComplete
	}
}

// Err joins the per-table failures, nil when there were none.
func (s LedgerRetentionStats) Err() error {
	if len(s.Failed) == 0 {
		return nil
	}
	errs := make([]error, 0, len(s.Failed))
	for _, table := range ledgerTableOrder {
		if err, ok := s.Failed[table]; ok {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// retainedCountCap bounds the diagnostic count query.
const retainedCountCap = 10000

// ledgerSweep is one bounded, batched delete. The statement must return
// (primary rows deleted, secondary rows deleted); the loop stops when a batch
// deletes fewer primary rows than the batch size.
type ledgerSweep struct {
	table     string
	secondary string // label for the second count, "" when unused
	sql       string
	args      func(now time.Time) []any
}

// ledgerTableOrder is the order sweeps run and failures are reported in.
// Notice pairs go first (cheap, tiny); operations before counters so the
// heaviest table gets the freshest budget of the job timeout.
var ledgerTableOrder = []string{
	LedgerTableNoticeEvents, LedgerTableOperations, LedgerTableCounters,
	LedgerTableControlEvents, LedgerTableAccessEvents,
}

// liveJobStates are the River states in which a job may still run and
// therefore still dereference its operation_ref. Everything else
// (completed, cancelled, discarded) is final.
const liveJobStates = `'available', 'pending', 'retryable', 'running', 'scheduled'`

var ledgerSweeps = map[string]ledgerSweep{
	// Terminal notice pairs, 30 days after event creation. A pending delivery
	// keeps its event (and, through the operation guard below, its operation)
	// however old it is: the seven-day delivery deadline, not the janitor, is
	// what ends a pending delivery. The FK cascade would remove the deliveries
	// too; they are deleted explicitly so they can be counted.
	LedgerTableNoticeEvents: {
		table:     LedgerTableNoticeEvents,
		secondary: LedgerTableNoticeDeliveries,
		sql: `
			WITH victims AS (
			    SELECT e.id
			      FROM sending_protection_notice_events e
			     WHERE e.created_at <= $1
			       AND NOT EXISTS (
			           SELECT 1 FROM sending_protection_notice_deliveries d
			            WHERE d.event_id = e.id AND d.state = 'pending')
			     ORDER BY e.created_at, e.id
			     LIMIT $2
			       FOR UPDATE OF e SKIP LOCKED
			), gone_deliveries AS (
			    DELETE FROM sending_protection_notice_deliveries d
			     USING victims v WHERE d.event_id = v.id
			    RETURNING 1
			), gone_events AS (
			    DELETE FROM sending_protection_notice_events e
			     USING victims v WHERE e.id = v.id
			    RETURNING 1
			)
			SELECT (SELECT count(*) FROM gone_events), (SELECT count(*) FROM gone_deliveries)`,
		args: func(now time.Time) []any { return []any{now.Add(-noticeTerminalRetention)} },
	},

	// Provider operations with every attempt they own. An operation survives
	// its own expiry while ANY of these still needs it:
	//   - an attempt that is not terminal (state reserved: capacity held; or
	//     call_state authorized for the current or a future ordinal: a token
	//     may still be redeemed), or an attempt not yet past its own expiry;
	//     superseded authorized ordinals cannot redeem and do not hold an
	//     otherwise terminal operation forever; their confirmed exposure is
	//     retained in the day counters until those days close;
	//   - a River job that can still run and names it in operation_ref —
	//     this covers budget and pause holds (snoozed jobs), retries inside
	//     SendRetryHorizon, and every notification kind; deleting it would
	//     make the worker fail closed (ErrSourceUnavailable) and drop the send;
	//   - its customer message is still pre-terminal (accepted/queued/sending,
	//     or pending review), which the terminal reconciler may still settle;
	//   - a pending protection-notice delivery bound to it.
	// Feedback correlations are deliberately NOT a guard: they copy the
	// operation's attribution at authorization and no reader joins back to
	// this table (B8 is self-contained), so a correlation's account-lifetime
	// retention never extends an operation's 30 days.
	// The live-job set is small (the runnable backlog), so it is built once
	// per batch and probed as a hashed NOT IN rather than re-scanned per
	// candidate. SKIP LOCKED steps around an operation a worker holds FOR UPDATE; a
	// worker that locks it after this statement's lock waits and then sees
	// the row gone, which only happens for an operation no live job names.
	LedgerTableOperations: {
		table:     LedgerTableOperations,
		secondary: LedgerTableReservations,
		sql: `
			WITH live_refs AS MATERIALIZED (
			    SELECT DISTINCT j.args->'operation_ref'->>'id' AS operation_id
			      FROM river_job j
			     WHERE j.state IN (` + liveJobStates + `)
			       AND j.args ? 'operation_ref'
			), victims AS (
			    SELECT o.operation_id
			      FROM sending_provider_operations o
			     WHERE o.expires_at <= $1
			       AND NOT EXISTS (
			           SELECT 1 FROM sending_budget_reservations r
			            WHERE r.operation_id = o.operation_id
			              AND (r.expires_at > $1 OR r.state = 'reserved'
			                   OR (r.call_state = 'authorized' AND r.submission_attempt >= o.current_attempt)))
			       AND o.operation_id NOT IN (
			           SELECT l.operation_id FROM live_refs l WHERE l.operation_id IS NOT NULL)
			       AND NOT EXISTS (
			           SELECT 1 FROM messages m
			            WHERE m.id = o.operation_id AND m.direction = 'outbound'
			              AND (m.delivery_status IN ('accepted', 'queued', 'sending')
			                   OR m.status = 'pending_review'))
			       AND NOT EXISTS (
			           SELECT 1 FROM sending_protection_notice_deliveries d
			            WHERE d.current_operation_id = o.operation_id AND d.state = 'pending')
			     ORDER BY o.expires_at, o.operation_id
			     LIMIT $2
			       FOR UPDATE OF o SKIP LOCKED
			), gone_attempts AS (
			    DELETE FROM sending_budget_reservations r
			     USING victims v WHERE r.operation_id = v.operation_id
			    RETURNING 1
			), gone_operations AS (
			    DELETE FROM sending_provider_operations o
			     USING victims v WHERE o.operation_id = v.operation_id
			    RETURNING 1
			)
			SELECT (SELECT count(*) FROM gone_operations), (SELECT count(*) FROM gone_attempts)`,
		args: func(now time.Time) []any { return []any{now} },
	},

	// Day counters for closed days, on the same clock the gate stamps them
	// with (ledgerDay: the database's UTC date). Only past days are touched,
	// so deletion can never refund capacity: today's row is never a victim,
	// and the one path that writes an older row — releasing a stale
	// reservation — skips a missing counter by design (ledgerPlan.release).
	LedgerTableCounters: {
		table: LedgerTableCounters,
		sql: `
			WITH victims AS (
			    SELECT scope, scope_id, day
			      FROM sending_budget_counters
			     WHERE day <= (clock_timestamp() AT TIME ZONE 'UTC')::date - $1::int
			     ORDER BY day
			     LIMIT $2
			       FOR UPDATE SKIP LOCKED
			), gone AS (
			    DELETE FROM sending_budget_counters c
			     USING victims v
			     WHERE c.scope = v.scope AND c.scope_id = v.scope_id AND c.day = v.day
			    RETURNING 1
			)
			SELECT (SELECT count(*) FROM gone), 0`,
		args: func(time.Time) []any { return []any{ledgerCounterAgeDays} },
	},

	// Pause audit past its stamped expiry. One exception: an event recording
	// an ABUSE-class pause survives while its account still exists (live or
	// in the trash). The account purge derives the abuse hold from this
	// history ("ever paused as abuse", identity.writePurgeRecordsTx); deleting
	// it at 90 days would let an account resumed after an abuse pause wait out
	// the audit and then be purged without abuse tombstones. After the purge
	// has read it, the event goes at its (already passed) expiry.
	LedgerTableControlEvents: {
		table: LedgerTableControlEvents,
		sql: `
			WITH victims AS (
			    SELECT e.id
			      FROM account_sending_control_events e
			     WHERE e.expires_at <= $1
			       AND (e.pause_class IS DISTINCT FROM 'abuse'
			            OR NOT EXISTS (SELECT 1 FROM users u WHERE u.id = e.account_ref))
			     ORDER BY e.expires_at, e.id
			     LIMIT $2
			       FOR UPDATE OF e SKIP LOCKED
			), gone AS (
			    DELETE FROM account_sending_control_events e
			     USING victims v WHERE e.id = v.id
			    RETURNING 1
			)
			SELECT (SELECT count(*) FROM gone), 0`,
		args: func(now time.Time) []any { return []any{now} },
	},

	// Operator grant/revoke audit past its stamped expiry. Nothing reads it
	// for a later transition: the grant itself lives on
	// account_sending_controls.
	LedgerTableAccessEvents: {
		table: LedgerTableAccessEvents,
		sql: `
			WITH victims AS (
			    SELECT id
			      FROM external_sending_access_events
			     WHERE expires_at <= $1
			     ORDER BY expires_at, id
			     LIMIT $2
			       FOR UPDATE SKIP LOCKED
			), gone AS (
			    DELETE FROM external_sending_access_events e
			     USING victims v WHERE e.id = v.id
			    RETURNING 1
			)
			SELECT (SELECT count(*) FROM gone), 0`,
		args: func(now time.Time) []any { return []any{now} },
	},
}

// ledgerRetentionTuning lets tests shrink the batch geometry; production uses
// the constants.
type ledgerRetentionTuning struct {
	batchSize  int
	maxBatches int
	// beforeBatch, when set, runs inside each batch transaction after the
	// timeouts are set and before the delete. Test-only fault injection.
	beforeBatch func(ctx context.Context, table string, exec func(context.Context, string) error) error
}

func defaultLedgerTuning() ledgerRetentionTuning {
	return ledgerRetentionTuning{batchSize: ledgerBatchSize, maxBatches: ledgerMaxBatches}
}

// GCLedger runs one retention pass over the sending ledger. It never returns
// early on a table's failure: each table is swept independently and failures
// are reported in the stats, so one lock timeout cannot stall the others.
// The returned error is non-nil only for a cancelled context.
func (m *Module) GCLedger(ctx context.Context, now time.Time) (LedgerRetentionStats, error) {
	return m.gcLedger(ctx, now, defaultLedgerTuning())
}

func (m *Module) gcLedger(ctx context.Context, now time.Time, tuning ledgerRetentionTuning) (LedgerRetentionStats, error) {
	now = now.UTC()
	st := LedgerRetentionStats{Deleted: map[string]int64{}, Failed: map[string]error{}}
	for _, table := range ledgerTableOrder {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		sweep := ledgerSweeps[table]
		capped, err := m.runLedgerSweep(ctx, sweep, now, tuning, &st)
		if err != nil {
			st.Failed[table] = err
			continue
		}
		if capped {
			st.Capped = append(st.Capped, table)
		}
	}
	if n, err := m.countRetainedOperations(ctx, now); err == nil {
		st.RetainedOperations = n
	}
	return st, nil
}

func (m *Module) runLedgerSweep(ctx context.Context, sweep ledgerSweep, now time.Time, tuning ledgerRetentionTuning, st *LedgerRetentionStats) (capped bool, err error) {
	args := append(sweep.args(now), tuning.batchSize)
	for batch := 0; batch < tuning.maxBatches; batch++ {
		primary, secondary, err := m.runLedgerBatch(ctx, sweep, args, tuning)
		st.Deleted[sweep.table] += primary
		if sweep.secondary != "" {
			st.Deleted[sweep.secondary] += secondary
		}
		if err != nil {
			return false, err
		}
		if primary < int64(tuning.batchSize) {
			return false, nil
		}
	}
	return true, nil
}

func (m *Module) runLedgerBatch(ctx context.Context, sweep ledgerSweep, args []any, tuning ledgerRetentionTuning) (primary, secondary int64, err error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("sendingpolicy: ledger retention %s: begin: %w", sweep.table, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+ledgerLockTimeout+`'`); err != nil {
		return 0, 0, fmt.Errorf("sendingpolicy: ledger retention %s: lock_timeout: %w", sweep.table, err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '`+ledgerStatementTimeout+`'`); err != nil {
		return 0, 0, fmt.Errorf("sendingpolicy: ledger retention %s: statement_timeout: %w", sweep.table, err)
	}
	if tuning.beforeBatch != nil {
		exec := func(ctx context.Context, sql string) error {
			_, err := tx.Exec(ctx, sql)
			return err
		}
		if err := tuning.beforeBatch(ctx, sweep.table, exec); err != nil {
			return 0, 0, fmt.Errorf("sendingpolicy: ledger retention %s: %w", sweep.table, err)
		}
	}
	// The outer SELECT reads only the CTEs' RETURNING output, never a table
	// a CTE modified, so the statement-snapshot hazard does not apply.
	if err := tx.QueryRow(ctx, sweep.sql, args...).Scan(&primary, &secondary); err != nil {
		return 0, 0, fmt.Errorf("sendingpolicy: ledger retention %s: %w", sweep.table, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("sendingpolicy: ledger retention %s: commit: %w", sweep.table, err)
	}
	return primary, secondary, nil
}

// countRetainedOperations counts, up to a cap, the expired operations that a
// guard kept. A number that only grows is a stuck attempt or job, which an
// operator should look at; nothing here deletes it.
func (m *Module) countRetainedOperations(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := m.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
		    SELECT 1 FROM sending_provider_operations WHERE expires_at <= $1 LIMIT $2
		) AS expired`, now, retainedCountCap).Scan(&n)
	return n, err
}

// LedgerRetentionArgs is the periodic ledger retention job.
type LedgerRetentionArgs struct{}

func (LedgerRetentionArgs) Kind() string { return "sending_ledger_retention" }

// LedgerRetentionWorker runs one GCLedger pass.
type LedgerRetentionWorker struct {
	river.WorkerDefaults[LedgerRetentionArgs]
	module *Module
}

// NewLedgerRetentionWorker builds the ledger janitor over a module. Exported
// so a test can drive the component that deletes ledger rows directly.
func NewLedgerRetentionWorker(module *Module) *LedgerRetentionWorker {
	return &LedgerRetentionWorker{module: module}
}

// Work never returns a table failure to River: a failure is logged and
// counted, and the next hourly run retries. Returning it would only make
// River re-run the whole pass on its retry schedule against the same lock.
func (w *LedgerRetentionWorker) Work(ctx context.Context, _ *river.Job[LedgerRetentionArgs]) error {
	st, err := w.module.GCLedger(ctx, w.module.now())
	outcome := st.Outcome()
	if err != nil {
		outcome = LedgerRunFailed
	}
	if o := currentLedgerObserver(); o != nil {
		for _, table := range ledgerMetricTables {
			if n := st.Deleted[table]; n > 0 {
				o.JanitorRowsDeleted(table, int(n))
			}
		}
		o.SendingLedgerRetentionRun(outcome)
	}
	log.Printf("[sendingpolicy:ledger-gc] outcome=%s %s retained_expired_operations=%d",
		outcome, formatLedgerCounts(st.Deleted), st.RetainedOperations)
	if joined := st.Err(); joined != nil {
		log.Printf("[sendingpolicy:ledger-gc] table failures (retried next run): %v", joined)
	}
	if err != nil {
		log.Printf("[sendingpolicy:ledger-gc] run interrupted: %v", err)
	}
	return nil
}

// Timeout matches the cleanup janitor's budget: every batch autocommits, so a
// cut is safe and the next run resumes.
func (w *LedgerRetentionWorker) Timeout(*river.Job[LedgerRetentionArgs]) time.Duration {
	return 5 * time.Minute
}

// ledgerMetricTables is every label the janitor can emit, in a stable order.
var ledgerMetricTables = []string{
	LedgerTableOperations, LedgerTableReservations, LedgerTableCounters,
	LedgerTableNoticeEvents, LedgerTableNoticeDeliveries,
	LedgerTableControlEvents, LedgerTableAccessEvents,
}

func formatLedgerCounts(deleted map[string]int64) string {
	parts := make([]string, 0, len(ledgerMetricTables))
	for _, table := range ledgerMetricTables {
		parts = append(parts, fmt.Sprintf("%s=%d", table, deleted[table]))
	}
	return strings.Join(parts, " ")
}
