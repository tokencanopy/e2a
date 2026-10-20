package sendingpolicy_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// The sending-ledger retention janitor is the one component in the ledger
// that DELETES rows, so every guard it has is pinned here against real
// Postgres. All identifiers are synthetic.

// ledgerFixture is a fixture whose database also has River's schema: the
// operation guard reads river_job.
func ledgerFixture(t *testing.T) (*fixture, *sendingpolicy.Module) {
	t.Helper()
	f := newFixture(t)
	if err := jobs.Migrate(f.ctx, f.pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	// River rows are not part of the harness truncation; clear ours.
	f.exec(`DELETE FROM river_job WHERE kind LIKE 'ledger_test_%'`)
	return f, sendingpolicy.NewModule(f.pool, f.secrets())
}

// op inserts one provider operation whose expiry is `expires` relative to now
// (a Postgres interval literal such as '-1 hour').
func (f *fixture) op(id, expires string) {
	f.t.Helper()
	f.exec(`INSERT INTO sending_provider_operations
	            (operation_id, source_account_ref, policy_subject_ref, purpose, expires_at)
	        VALUES ($1, 'usr_ledger', 'usr_ledger', 'customer_message', now() + $2::interval)`, id, expires)
}

// attempt inserts one reservation row in a consistent (state, call_state)
// shape with the given expiry.
func (f *fixture) attempt(opID string, n int, state, callState, expires string) {
	f.t.Helper()
	var nonce, started any
	if callState != "none" {
		nonce = fmt.Sprintf("nonce_%s_%d", opID, n)
	}
	if callState == "started" {
		started = time.Now().UTC()
	}
	f.exec(`INSERT INTO sending_budget_reservations
	            (operation_id, submission_attempt, source_account_ref, policy_subject_ref, purpose,
	             day, units, probation, state, call_state, authorization_nonce,
	             provider_call_started_at, expires_at)
	        VALUES ($1, $2, 'usr_ledger', 'usr_ledger', 'customer_message',
	                current_date, 1, false, $3, $4, $5, $6, now() + $7::interval)`,
		opID, n, state, callState, nonce, started, expires)
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (f *fixture) opExists(id string) bool {
	return f.count(`SELECT count(*) FROM sending_provider_operations WHERE operation_id = $1`, id) == 1
}

func (f *fixture) attempts(opID string) int {
	return f.count(`SELECT count(*) FROM sending_budget_reservations WHERE operation_id = $1`, opID)
}

func gcLedger(t *testing.T, m *sendingpolicy.Module) sendingpolicy.LedgerRetentionStats {
	t.Helper()
	st, err := m.GCLedger(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("GCLedger: %v", err)
	}
	if st.Outcome() != sendingpolicy.LedgerRunComplete {
		t.Fatalf("outcome = %s (failed %v, capped %v)", st.Outcome(), st.Failed, st.Capped)
	}
	return st
}

// TestLedgerRetentionOperationsAndAttempts pins the 30-day operation/attempt
// horizon and every reason an expired operation must survive.
func TestLedgerRetentionOperationsAndAttempts(t *testing.T) {
	f, m := ledgerFixture(t)

	// Inside the window.
	f.op("op_fresh", "1 day")
	f.attempt("op_fresh", 1, "confirmed", "started", "-1 hour")

	// Past the window, every attempt terminal and expired: deleted, with
	// both of its attempts.
	f.op("op_done", "-1 hour")
	f.attempt("op_done", 1, "confirmed", "started", "-2 hours")
	f.attempt("op_done", 2, "released", "none", "-1 hour")

	// Past the window, no attempt at all (never reserved): deleted.
	f.op("op_bare", "-1 hour")

	// Non-terminal attempts hold their operation however old it is.
	f.op("op_reserved", "-10 days")
	f.attempt("op_reserved", 1, "reserved", "none", "-10 days")
	f.op("op_authorized", "-10 days")
	f.attempt("op_authorized", 1, "confirmed", "authorized", "-10 days")

	// A later attempt still inside its own window holds the operation.
	f.op("op_retry", "-1 hour")
	f.attempt("op_retry", 1, "confirmed", "started", "-1 hour")
	f.attempt("op_retry", 2, "confirmed", "started", "5 days")

	st := gcLedger(t, m)

	for _, id := range []string{"op_done", "op_bare"} {
		if f.opExists(id) || f.attempts(id) != 0 {
			t.Errorf("%s: expired terminal operation or its attempts survived", id)
		}
	}
	for id, attempts := range map[string]int{"op_fresh": 1, "op_reserved": 1, "op_authorized": 1, "op_retry": 2} {
		if !f.opExists(id) || f.attempts(id) != attempts {
			t.Errorf("%s: retained operation lost (exists=%v attempts=%d, want %d)", id, f.opExists(id), f.attempts(id), attempts)
		}
	}
	if st.Deleted[sendingpolicy.LedgerTableOperations] != 2 || st.Deleted[sendingpolicy.LedgerTableReservations] != 2 {
		t.Errorf("deleted = %v, want 2 operations and 2 attempts", st.Deleted)
	}
	if st.RetainedOperations != 3 {
		t.Errorf("retained expired operations = %d, want 3 (reserved, authorized, retry)", st.RetainedOperations)
	}
}

// TestLedgerRetentionKeepsOperationsInFlight: an operation a send still
// needs is never deleted by age — a live River job (budget or pause hold,
// retry), a pre-terminal customer message, or a pending protection notice.
// Deleting any of these would make the worker fail closed and drop the send.
func TestLedgerRetentionKeepsOperationsInFlight(t *testing.T) {
	f, m := ledgerFixture(t)

	// River jobs: snoozed (a budget or pause hold is a snooze), retryable,
	// and running all hold; a completed job does not.
	for _, state := range []string{"scheduled", "retryable", "running", "available"} {
		id := "op_job_" + state
		f.op(id, "-40 days")
		f.exec(`INSERT INTO river_job (kind, state, args, attempted_at)
		        VALUES ('ledger_test_send', $1::river_job_state, jsonb_build_object('operation_ref', jsonb_build_object('v', 1, 'id', $2::text)),
		                CASE WHEN $1 = 'running' THEN now() END)`, state, id)
	}
	f.op("op_job_done", "-40 days")
	f.exec(`INSERT INTO river_job (kind, state, args, finalized_at)
	        VALUES ('ledger_test_send', 'completed', jsonb_build_object('operation_ref', jsonb_build_object('v', 1, 'id', 'op_job_done')), now())`)

	// Customer messages: the operation id IS the message id.
	user := f.user("standard")
	agent := f.agent(user)
	inFlight := map[string]bool{}
	for _, status := range []string{"accepted", "sending", "delivered", "failed"} {
		msg := f.message(agent, "relay", 1)
		f.exec(`UPDATE messages SET delivery_status = $2 WHERE id = $1`, msg, status)
		f.op(msg, "-40 days")
		inFlight[msg] = status == "accepted" || status == "sending"
	}
	held := f.pendingMessage(agent, "relay")
	f.op(held, "-40 days")
	inFlight[held] = true

	// Protection notices: a pending delivery holds its operation; a sent one
	// does not.
	f.exec(`INSERT INTO sending_protection_notice_events (id, account_ref, kind, reason_code, source_event_id, expires_at)
	        VALUES ('spn_ledger_1', 'usr_ledger', 'pause', 'manual', 'sce_ledger_1', now() + interval '90 days'),
	               ('spn_ledger_2', 'usr_ledger', 'pause', 'manual', 'sce_ledger_2', now() + interval '90 days')`)
	f.op("opn_pending", "-40 days")
	f.op("opn_sent", "-40 days")
	f.exec(`INSERT INTO sending_protection_notice_deliveries (event_id, audience, current_operation_id, state)
	        VALUES ('spn_ledger_1', 'owner', 'opn_pending', 'pending'),
	               ('spn_ledger_2', 'owner', 'opn_sent', 'sent')`)

	gcLedger(t, m)

	for _, state := range []string{"scheduled", "retryable", "running", "available"} {
		if !f.opExists("op_job_" + state) {
			t.Errorf("operation named by a %s River job was deleted", state)
		}
	}
	if f.opExists("op_job_done") {
		t.Error("operation named only by a completed job survived")
	}
	for msg, keep := range inFlight {
		if f.opExists(msg) != keep {
			t.Errorf("message operation %s: exists=%v, want %v", msg, f.opExists(msg), keep)
		}
	}
	if !f.opExists("opn_pending") {
		t.Error("operation bound to a pending notice delivery was deleted")
	}
	if f.opExists("opn_sent") {
		t.Error("operation bound only to a sent notice delivery survived")
	}
}

// TestLedgerRetentionLeavesFeedbackProvenanceIntact: B8 correlations
// reference (operation_id, submission_attempt) by value with no foreign key,
// and they are kept for the account's lifetime — far past the operation's 30
// days. Deleting the operation must not touch them, and feedback must still
// correlate afterwards: that is what makes it safe for operations NOT to
// outlive their correlations.
func TestLedgerRetentionLeavesFeedbackProvenanceIntact(t *testing.T) {
	f, m := ledgerFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	user := f.user("standard")
	agent := f.agent(user)
	rcpts := []string{"carol@example.test"}
	msg := f.messageTo(agent, "relay", rcpts)
	_, corrID, sesID := f.authorizedSend(g, msg, rcpts)

	// Age the operation and its attempt past the 30-day horizon.
	f.exec(`UPDATE sending_provider_operations SET expires_at = now() - interval '1 hour' WHERE operation_id = $1`, msg)
	f.exec(`UPDATE sending_budget_reservations SET expires_at = now() - interval '1 hour' WHERE operation_id = $1`, msg)

	gcLedger(t, m)

	if f.opExists(msg) || f.attempts(msg) != 0 {
		t.Fatal("expired, settled operation was not deleted")
	}
	if n := f.count(`SELECT count(*) FROM sending_feedback_correlations WHERE correlation_id = $1 AND expires_at IS NULL`, corrID); n != 1 {
		t.Fatal("the account-lifetime correlation was touched by the ledger janitor")
	}
	if n := f.count(`SELECT count(*) FROM sending_feedback_recipients WHERE correlation_id = $1`, corrID); n != 1 {
		t.Fatal("recipient provenance was touched by the ledger janitor")
	}
	res, err := m.ProcessProviderFeedback(f.ctx, delivery.ProviderFeedback{
		ProviderEventID: "evt-ledger-complaint", OccurredAt: time.Now().UTC(), Kind: delivery.KindComplaint,
		ProviderMessageID: sesID, Recipients: rcpts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Correlated || res.AccountRef != user {
		t.Fatalf("feedback after operation deletion = %+v, want correlated to the sending account", res)
	}
}

// TestLedgerRetentionCounters: rows for today and yesterday survive; a day is
// deleted once it and the day after it have both closed. Deletion can never
// refund capacity because the open day is never a victim.
func TestLedgerRetentionCounters(t *testing.T) {
	f, m := ledgerFixture(t)
	for _, age := range []int{0, 1, 2, 3, 30} {
		f.exec(`INSERT INTO sending_budget_counters (scope, scope_id, day, reserved_count, confirmed_count, daily_limit)
		        VALUES ('account_daily', 'usr_ledger', (now() AT TIME ZONE 'UTC')::date - $1::int, 5, 5, 20)`, age)
	}
	st := gcLedger(t, m)

	var ages []int
	rows, err := f.pool.Query(f.ctx, `SELECT (now() AT TIME ZONE 'UTC')::date - day FROM sending_budget_counters ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a int
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		ages = append(ages, a)
	}
	if fmt.Sprint(ages) != "[0 1]" {
		t.Fatalf("remaining counter ages = %v, want [0 1] (today and yesterday)", ages)
	}
	if st.Deleted[sendingpolicy.LedgerTableCounters] != 3 {
		t.Fatalf("deleted counters = %d, want 3", st.Deleted[sendingpolicy.LedgerTableCounters])
	}
}

// TestLedgerRetentionNoticeOutbox: terminal pairs go 30 days after event
// creation; a pending delivery keeps its event however old.
func TestLedgerRetentionNoticeOutbox(t *testing.T) {
	f, m := ledgerFixture(t)
	insert := func(id, age string, states ...string) {
		f.exec(`INSERT INTO sending_protection_notice_events (id, account_ref, kind, reason_code, source_event_id, created_at, expires_at)
		        VALUES ($1, 'usr_ledger', 'pause', 'manual', $1 || '_src', now() - $2::interval, now() + interval '60 days')`, id, age)
		for i, state := range states {
			audience := []string{"owner", "operator"}[i]
			f.exec(`INSERT INTO sending_protection_notice_deliveries (event_id, audience, state) VALUES ($1, $2, $3)`, id, audience, state)
		}
	}
	insert("spn_old_terminal", "31 days", "sent", "failed")
	insert("spn_old_skipped", "31 days", "skipped_account_deleted")
	insert("spn_old_pending", "45 days", "sent", "pending")
	insert("spn_young_terminal", "29 days", "sent", "sent")

	st := gcLedger(t, m)

	gone := func(id string) bool {
		return f.count(`SELECT count(*) FROM sending_protection_notice_events WHERE id = $1`, id) == 0 &&
			f.count(`SELECT count(*) FROM sending_protection_notice_deliveries WHERE event_id = $1`, id) == 0
	}
	for id, wantGone := range map[string]bool{
		"spn_old_terminal": true, "spn_old_skipped": true,
		"spn_old_pending": false, "spn_young_terminal": false,
	} {
		if gone(id) != wantGone {
			t.Errorf("%s: gone=%v, want %v", id, gone(id), wantGone)
		}
	}
	if f.count(`SELECT count(*) FROM sending_protection_notice_deliveries WHERE event_id = 'spn_old_pending'`) != 2 {
		t.Error("the pending event lost a delivery")
	}
	if st.Deleted[sendingpolicy.LedgerTableNoticeEvents] != 2 || st.Deleted[sendingpolicy.LedgerTableNoticeDeliveries] != 3 {
		t.Errorf("deleted = %v, want 2 events and 3 deliveries", st.Deleted)
	}
}

// TestLedgerRetentionControlAndAccessAudit: both audits go at their stamped
// expiry, except an abuse-class pause event whose account still exists —
// the account purge derives the abuse hold from that history.
func TestLedgerRetentionControlAndAccessAudit(t *testing.T) {
	f, m := ledgerFixture(t)
	live := f.user("standard")
	control := func(id, account string, class any, expires string) {
		f.exec(`INSERT INTO account_sending_control_events
		            (id, account_ref, old_state, new_state, reason, actor, pause_class, created_at, expires_at)
		        VALUES ($1, $2, 'active', 'paused', 'synthetic', 'operator', $3,
		                now() + $4::interval - interval '90 days', now() + $4::interval)`, id, account, class, expires)
	}
	control("sce_expired", live, "operator", "-1 hour")
	control("sce_expired_null_class", live, nil, "-1 hour")
	control("sce_unexpired", live, "operator", "1 day")
	control("sce_abuse_live", live, "abuse", "-1 hour")
	control("sce_abuse_purged", "usr_ledger_purged", "abuse", "-1 hour")

	access := func(id, expires string) {
		f.exec(`INSERT INTO external_sending_access_events
		            (id, account_ref, old_approved, new_approved, old_revision, new_revision, actor, reason, created_at, expires_at)
		        VALUES ($1, 'usr_ledger', false, true, 0, 1, 'operator', 'synthetic',
		                now() + $2::interval - interval '90 days', now() + $2::interval)`, id, expires)
	}
	access("esa_expired", "-1 hour")
	access("esa_unexpired", "1 day")

	st := gcLedger(t, m)

	for id, keep := range map[string]bool{
		"sce_expired": false, "sce_expired_null_class": false, "sce_unexpired": true,
		"sce_abuse_live": true, "sce_abuse_purged": false,
	} {
		if got := f.count(`SELECT count(*) FROM account_sending_control_events WHERE id = $1`, id) == 1; got != keep {
			t.Errorf("control event %s: kept=%v, want %v", id, got, keep)
		}
	}
	for id, keep := range map[string]bool{"esa_expired": false, "esa_unexpired": true} {
		if got := f.count(`SELECT count(*) FROM external_sending_access_events WHERE id = $1`, id) == 1; got != keep {
			t.Errorf("access event %s: kept=%v, want %v", id, got, keep)
		}
	}
	if st.Deleted[sendingpolicy.LedgerTableControlEvents] != 3 || st.Deleted[sendingpolicy.LedgerTableAccessEvents] != 1 {
		t.Errorf("deleted = %v", st.Deleted)
	}
}

// TestLedgerRetentionBatches: the loop drains across multiple batches, and a
// per-run cap stops a table (outcome partial) for the next run to finish.
func TestLedgerRetentionBatches(t *testing.T) {
	f, m := ledgerFixture(t)
	for i := 0; i < 25; i++ {
		f.op(fmt.Sprintf("op_batch_%02d", i), "-1 hour")
	}

	st, err := m.GCLedgerTuned(f.ctx, time.Now(), sendingpolicy.LedgerTuning{BatchSize: 10, MaxBatches: 2})
	if err != nil {
		t.Fatal(err)
	}
	if st.Deleted[sendingpolicy.LedgerTableOperations] != 20 || st.Outcome() != sendingpolicy.LedgerRunPartial {
		t.Fatalf("capped run: deleted=%d outcome=%s, want 20 and partial", st.Deleted[sendingpolicy.LedgerTableOperations], st.Outcome())
	}
	if len(st.Capped) != 1 || st.Capped[0] != sendingpolicy.LedgerTableOperations {
		t.Fatalf("capped = %v", st.Capped)
	}

	st, err = m.GCLedgerTuned(f.ctx, time.Now(), sendingpolicy.LedgerTuning{BatchSize: 10, MaxBatches: 10})
	if err != nil {
		t.Fatal(err)
	}
	if st.Deleted[sendingpolicy.LedgerTableOperations] != 5 || st.Outcome() != sendingpolicy.LedgerRunComplete {
		t.Fatalf("resumed run: deleted=%d outcome=%s, want 5 and complete", st.Deleted[sendingpolicy.LedgerTableOperations], st.Outcome())
	}
	if n := f.count(`SELECT count(*) FROM sending_provider_operations WHERE operation_id LIKE 'op_batch_%'`); n != 0 {
		t.Fatalf("%d operations left after draining", n)
	}
}

// TestLedgerRetentionLockTimeoutIsNonFatal: a table whose lock cannot be
// acquired fails only itself for this run; the other tables are still swept,
// the worker returns nil, and the next run finishes the job.
func TestLedgerRetentionLockTimeoutIsNonFatal(t *testing.T) {
	f, m := ledgerFixture(t)
	f.exec(`INSERT INTO sending_budget_counters (scope, scope_id, day, daily_limit)
	        VALUES ('global_all', 'global', (now() AT TIME ZONE 'UTC')::date - 5, 5000)`)
	f.exec(`INSERT INTO external_sending_access_events
	            (id, account_ref, old_approved, new_approved, old_revision, new_revision, actor, reason, created_at, expires_at)
	        VALUES ('esa_lock', 'usr_ledger', false, true, 0, 1, 'operator', 'synthetic', now() - interval '91 days', now() - interval '1 hour')`)

	// Another session holds a lock that conflicts with the janitor's
	// FOR UPDATE on the counters table.
	holder, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = holder.Rollback(f.ctx) }) }
	defer release()
	if _, err := holder.Exec(f.ctx, `LOCK TABLE sending_budget_counters IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	fast := sendingpolicy.LedgerTuning{BatchSize: sendingpolicy.LedgerBatchSize, MaxBatches: 5,
		BeforeBatch: func(ctx context.Context, _ string, exec func(context.Context, string) error) error {
			return exec(ctx, `SET LOCAL lock_timeout = '100ms'`)
		}}
	st, err := m.GCLedgerTuned(f.ctx, time.Now(), fast)
	if err != nil {
		t.Fatalf("a table lock timeout must not fail the run: %v", err)
	}
	failure, failed := st.Failed[sendingpolicy.LedgerTableCounters]
	var pgErr *pgconn.PgError
	if !failed || !errors.As(failure, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("counters failure = %v, want a lock_not_available (55P03) error", failure)
	}
	if st.Outcome() != sendingpolicy.LedgerRunFailed {
		t.Fatalf("outcome = %s, want failed", st.Outcome())
	}
	if st.Deleted[sendingpolicy.LedgerTableAccessEvents] != 1 {
		t.Fatal("a failing table stopped the tables after it")
	}

	release()
	st, err = m.GCLedgerTuned(f.ctx, time.Now(), fast)
	if err != nil || st.Outcome() != sendingpolicy.LedgerRunComplete || st.Deleted[sendingpolicy.LedgerTableCounters] != 1 {
		t.Fatalf("retry run: err=%v outcome=%s deleted=%v", err, st.Outcome(), st.Deleted)
	}
}

// recordingLedgerObserver captures the janitor's metric samples.
type recordingLedgerObserver struct {
	mu      sync.Mutex
	deleted map[string]int
	runs    []string
}

func (r *recordingLedgerObserver) JanitorRowsDeleted(table string, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleted == nil {
		r.deleted = map[string]int{}
	}
	r.deleted[table] += count
}

func (r *recordingLedgerObserver) SendingLedgerRetentionRun(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, outcome)
}

// TestLedgerRetentionWorkerEmitsBoundedMetrics: the worker reports rows
// deleted per ledger table and one run outcome, with table names as the only
// labels — never an operation, account, or event id.
func TestLedgerRetentionWorkerEmitsBoundedMetrics(t *testing.T) {
	f, m := ledgerFixture(t)
	rec := &recordingLedgerObserver{}
	sendingpolicy.SetLedgerRetentionObserver(rec)
	t.Cleanup(func() { sendingpolicy.SetLedgerRetentionObserver(nil) })

	f.op("op_metric", "-1 hour")
	f.attempt("op_metric", 1, "confirmed", "started", "-1 hour")
	f.exec(`INSERT INTO sending_budget_counters (scope, scope_id, day, daily_limit)
	        VALUES ('global_all', 'global', (now() AT TIME ZONE 'UTC')::date - 3, 5000)`)

	if err := sendingpolicy.NewLedgerRetentionWorker(m).Work(f.ctx, &river.Job[sendingpolicy.LedgerRetentionArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	want := map[string]int{
		sendingpolicy.LedgerTableOperations:   1,
		sendingpolicy.LedgerTableReservations: 1,
		sendingpolicy.LedgerTableCounters:     1,
	}
	if fmt.Sprint(rec.deleted) != fmt.Sprint(want) {
		t.Fatalf("deleted samples = %v, want %v", rec.deleted, want)
	}
	if len(rec.runs) != 1 || rec.runs[0] != sendingpolicy.LedgerRunComplete {
		t.Fatalf("run samples = %v, want one complete", rec.runs)
	}
	for table := range rec.deleted {
		if strings.Contains(table, "op_") || strings.Contains(table, "usr_") {
			t.Fatalf("metric label %q carries an identifier", table)
		}
	}
}

// TestLedgerRetentionRegistersOnTheMaintenanceQueue: the janitor is one of
// the sending-protection maintenance periodics, so a registrar that dropped
// it would leave the ledger growing forever with nothing failing.
func TestLedgerRetentionRegistersOnTheMaintenanceQueue(t *testing.T) {
	if kind := (sendingpolicy.LedgerRetentionArgs{}).Kind(); kind != "sending_ledger_retention" {
		t.Fatalf("kind = %q", kind)
	}
	periodics := sendingpolicy.NewMaintenanceJobs(nil).RegisterJobs(river.NewWorkers())
	if len(periodics) != 3 {
		t.Fatalf("periodic jobs = %d, want 3 (feedback retention, reconcile, ledger retention)", len(periodics))
	}
}

// Superseded tokens can never open a socket again, even though their
// reservation preserves confirmed exposure and call_state=authorized.
func TestLedgerRetentionCollectsSupersededAuthorization(t *testing.T) {
	f, m := ledgerFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	ref, first := f.prepareAndReserve(g, agent, 1)
	_, oldAuth, err := g.ConsumeAttempt(f.ctx, first)
	if err != nil || oldAuth == nil {
		t.Fatalf("old auth: %v", err)
	}
	_, second, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	_, liveAuth, err := g.ConsumeAttempt(f.ctx, second)
	if err != nil || liveAuth == nil {
		t.Fatalf("live auth: %v", err)
	}
	if err := g.RedeemProviderCall(f.ctx, *oldAuth); !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
		t.Fatalf("stale token: %v", err)
	}
	if err := g.RedeemProviderCall(f.ctx, *liveAuth); err != nil {
		t.Fatal(err)
	}
	if err := g.SettleOperation(f.ctx, ref, sendingpolicy.SettlementProviderAccepted, "ses_synthetic_gc"); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE messages SET delivery_status = 'delivered' WHERE id = $1`, ref.ID())
	f.exec(`UPDATE sending_provider_operations SET expires_at = now() - interval '1 hour' WHERE operation_id = $1`, ref.ID())
	f.exec(`UPDATE sending_budget_reservations SET expires_at = now() - interval '1 hour' WHERE operation_id = $1`, ref.ID())
	gcLedger(t, m)
	if f.opExists(ref.ID()) || f.attempts(ref.ID()) != 0 {
		t.Fatal("terminal operation retained forever by its superseded authorization")
	}
}

func TestLedgerRetentionSupersededAuthorizationKeepsOtherGuards(t *testing.T) {
	f, m := ledgerFixture(t)
	for _, tc := range []struct {
		id                  string
		attempt             int
		state, call, expiry string
	}{
		{"op_stale_unexpired", 1, "confirmed", "authorized", "1 day"},
		{"op_current_authorized", 2, "confirmed", "authorized", "-1 hour"},
		{"op_future_authorized", 3, "confirmed", "authorized", "-1 hour"},
		{"op_old_reserved", 1, "reserved", "none", "-1 hour"},
	} {
		f.op(tc.id, "-1 hour")
		f.exec(`UPDATE sending_provider_operations SET current_attempt = 2 WHERE operation_id = $1`, tc.id)
		f.attempt(tc.id, tc.attempt, tc.state, tc.call, tc.expiry)
	}
	gcLedger(t, m)
	for _, id := range []string{"op_stale_unexpired", "op_current_authorized", "op_future_authorized", "op_old_reserved"} {
		if !f.opExists(id) || f.attempts(id) != 1 {
			t.Errorf("guarded operation %s lost", id)
		}
	}
}
