package outboundsend_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

func TestTerminalReconcileIndex(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	var (
		valid      bool
		keyColumns int
		allColumns int
		definition string
		predicate  string
	)
	if err := pool.QueryRow(ctx,
		`SELECT i.indisvalid, i.indnkeyatts, i.indnatts,
		        pg_get_indexdef(i.indexrelid), pg_get_expr(i.indpred, i.indrelid)
		   FROM pg_class c
		   JOIN pg_index i ON i.indexrelid = c.oid
		  WHERE c.relname = 'idx_messages_outbound_terminal_reconcile'`,
	).Scan(&valid, &keyColumns, &allColumns, &definition, &predicate); err != nil {
		t.Fatalf("read terminal reconcile index: %v", err)
	}
	if !valid {
		t.Error("terminal reconcile index is invalid")
	}
	if keyColumns != 2 || allColumns != 3 {
		t.Errorf("terminal reconcile index columns = (%d key, %d total), want (2 key, 3 total)", keyColumns, allColumns)
	}
	for _, want := range []string{"(created_at, id)", "INCLUDE (send_job_id)"} {
		if !strings.Contains(definition, want) {
			t.Errorf("terminal reconcile index definition %q missing %q", definition, want)
		}
	}
	for _, want := range []string{"direction = 'outbound'", "delivery_status", "'accepted'", "'sending'", "send_job_id IS NOT NULL"} {
		if !strings.Contains(predicate, want) {
			t.Errorf("terminal reconcile index predicate %q missing %q", predicate, want)
		}
	}
}

// TestReconcilePending_EnqueuesStrandedAndStamps stands up a REAL River client and
// asserts the slice-C startup cutover: an accepted message with send_job_id IS NULL
// (stranded by an accept-tx that crashed before the job committed) gets an
// outbound_send river_job carrying its message id, its send_job_id stamped, and a
// re-run is idempotent (no second job). Store/Deliverer are nil — the reconciler
// never runs the worker, only the enqueue path.
func TestReconcilePending_EnqueuesStrandedAndStamps(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	store := identity.NewStore(pool)

	// Seed a verified agent + one accepted outbound message with NO send job.
	user, err := store.CreateOrGetUser(ctx, "owner-recon@example.com", "Owner", "google-recon")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain := "recon.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	ag, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	var msgID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := store.CreateOutboundMessageTx(ctx, tx, ag.ID,
			[]string{"a@gmail.com"}, nil, nil, "S", "send", "smtp", "", "conv-recon",
			[]byte("raw"), "accepted", "agent@test.e2a.dev", "relay")
		msgID = m.ID
		return err // NB: no StampSendJobIDTx → send_job_id stays NULL (stranded)
	}); err != nil {
		t.Fatalf("accept tx: %v", err)
	}

	// Build the integration on a real shared River client so EnqueueSendTx inserts
	// an actual river_job row. Store/Deliverer unused by the reconcile path.
	j := outboundsend.NewJobs(nil, nil, pool)
	client, err := jobs.New(pool, jobs.Config{}, j)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	j.SetEnqueuer(client)

	n, err := j.ReconcilePending(ctx, pool)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReconcilePending enqueued %d, want 1", n)
	}

	var jobCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind='outbound_send' AND args->>'message_id' = $1`,
		msgID).Scan(&jobCount); err != nil {
		t.Fatalf("count river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("outbound_send river_job for %s = %d, want 1", msgID, jobCount)
	}
	var sendJobID *int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, msgID).Scan(&sendJobID); err != nil {
		t.Fatalf("read send_job_id: %v", err)
	}
	if sendJobID == nil {
		t.Errorf("send_job_id not stamped after ReconcilePending")
	}

	// Idempotent: a second cutover pass enqueues nothing more.
	if n2, err := j.ReconcilePending(ctx, pool); err != nil || n2 != 0 {
		t.Errorf("second ReconcilePending = (%d, %v), want (0, nil)", n2, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind='outbound_send' AND args->>'message_id' = $1`,
		msgID).Scan(&jobCount); err != nil {
		t.Fatalf("recount river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("after re-run: river_job for %s = %d, want 1 (idempotent)", msgID, jobCount)
	}
}

type terminalFixture struct {
	pool    *pgxpool.Pool
	store   *identity.Store
	agentID string
	jobs    *outboundsend.Jobs
}

func newTerminalFixture(t *testing.T, pool *pgxpool.Pool, store *identity.Store, outboundStore outboundsend.Store) *terminalFixture {
	t.Helper()
	if pool == nil {
		pool = testutil.TestDB(t)
		store = identity.NewStore(pool)
	}
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	user, err := store.CreateOrGetUser(ctx, "owner-terminal@example.com", "Owner", "google-terminal")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain := "terminal.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	ag, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	j := outboundsend.NewJobs(outboundStore, nil, pool)
	client, err := jobs.New(pool, jobs.Config{}, j)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	j.SetEnqueuer(client)
	return &terminalFixture{pool: pool, store: store, agentID: ag.ID, jobs: j}
}

func (f *terminalFixture) seed(t *testing.T, label, messageStatus, jobState string, missing bool) string {
	t.Helper()
	ctx := context.Background()
	var messageID string
	var jobID int64
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := f.store.CreateOutboundMessageTx(ctx, tx, f.agentID,
			[]string{label + "@gmail.com"}, nil, nil, label, "send", "smtp", "", "conv-"+label,
			[]byte("raw"), "accepted", "bot@terminal.example.com", "relay")
		if err != nil {
			return err
		}
		messageID = m.ID
		jobID, err = f.jobs.EnqueueSendTx(ctx, tx, messageID)
		if err != nil {
			return err
		}
		if err := f.store.StampSendJobIDTx(ctx, tx, messageID, jobID); err != nil {
			return err
		}
		if messageStatus == "sent" {
			if _, err = tx.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, messageID); err != nil {
				return err
			}
			_, err = f.store.MarkOutboundSentTx(ctx, tx, messageID, "<provider-"+label+">")
		}
		return err
	}); err != nil {
		t.Fatalf("seed %s: %v", label, err)
	}

	if missing {
		if _, err := f.pool.Exec(ctx, `DELETE FROM river_job WHERE id=$1`, jobID); err != nil {
			t.Fatalf("prune job for %s: %v", label, err)
		}
	} else if jobState != "" {
		query := `UPDATE river_job SET state=$2, finalized_at=NULL WHERE id=$1`
		if jobState == "cancelled" || jobState == "discarded" || jobState == "completed" {
			// Finalized just past the provider-evidence grace window, so the
			// reconciler processes the row on its next pass.
			query = `UPDATE river_job SET state=$2, finalized_at=now() - interval '16 minutes' WHERE id=$1`
		}
		if _, err := f.pool.Exec(ctx, query, jobID, jobState); err != nil {
			t.Fatalf("set job %s state to %s: %v", label, jobState, err)
		}
	}
	return messageID
}

// freshenJob moves the stamped job's finalized_at back inside the
// provider-evidence grace window.
func (f *terminalFixture) freshenJob(t *testing.T, messageID string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE river_job SET finalized_at = now()
		  WHERE id = (SELECT send_job_id FROM messages WHERE id=$1)`, messageID); err != nil {
		t.Fatalf("freshen job for %s: %v", messageID, err)
	}
}

func (f *terminalFixture) status(t *testing.T, messageID string) (string, string) {
	t.Helper()
	var status, detail string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT delivery_status, COALESCE(delivery_detail,'') FROM messages WHERE id=$1`, messageID,
	).Scan(&status, &detail); err != nil {
		t.Fatalf("read message %s: %v", messageID, err)
	}
	return status, detail
}

func mustSendJobID(t *testing.T, pool *pgxpool.Pool, messageID string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT send_job_id FROM messages WHERE id=$1`, messageID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *terminalFixture) failedEventCount(t *testing.T, messageID string) int {
	return f.eventCount(t, messageID, webhookpub.EventEmailFailed)
}

func (f *terminalFixture) sentEventCount(t *testing.T, messageID string) int {
	return f.eventCount(t, messageID, webhookpub.EventEmailSent)
}

func (f *terminalFixture) eventCount(t *testing.T, messageID, eventType string) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM webhook_events WHERE id=$1`,
		webhookpub.DeterministicEventID(messageID, eventType),
	).Scan(&count); err != nil {
		t.Fatalf("count %s for %s: %v", eventType, messageID, err)
	}
	return count
}

func (f *terminalFixture) lifecycleReason(t *testing.T, messageID string, reason messagelifecycle.ReasonCode) *messagelifecycle.MessageLifecycleTransition {
	t.Helper()
	var tr messagelifecycle.MessageLifecycleTransition
	var evidence, correlations []byte
	err := f.pool.QueryRow(context.Background(), `SELECT id,message_id,direction,stage,outcome,reason_code,retryable,evidence,correlation_ids,occurred_at,reconstructed FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code=$2`, messageID, reason).Scan(&tr.ID, &tr.MessageID, &tr.Direction, &tr.Stage, &tr.Outcome, &tr.ReasonCode, &tr.Retryable, &evidence, &correlations, &tr.OccurredAt, &tr.Reconstructed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(evidence, &tr.Evidence); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(correlations, &tr.CorrelationIDs); err != nil {
		t.Fatal(err)
	}
	return &tr
}

func (f *terminalFixture) assertEventCarriesOnly(t *testing.T, messageID, eventType string, want *messagelifecycle.MessageLifecycleTransition) {
	t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT envelope->'data'->'lifecycle_transitions' FROM webhook_events WHERE message_id=$1 AND type=$2`, messageID, eventType).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got []messagelifecycle.MessageLifecycleTransition
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || want == nil || got[0].ID != want.ID {
		t.Fatalf("%s lifecycle=%+v want exact %+v", eventType, got, want)
	}
}

func TestTerminalReconcileWorker_EvidenceSettleLatencyUsesProviderAcceptTime(t *testing.T) {
	// The §3.1 evidence-settle path rewrites the terminal write's occurred_at
	// to the provider-accept evidence time (the durable write records
	// acceptance→provider-accept, not acceptance→sweep). The latency SLI must
	// use that SAME effective occurred_at — the sweep runs a full grace
	// window (15m+) after the job died, so measuring to sweep time would
	// systematically burn the 5-minute SLO on the reconciler's priority
	// population (submitted, crashed before MarkSent).
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())

	f := newTerminalFixture(t, pool, store, adapter)
	evidenceID := f.seed(t, "lat-evidence", "accepted", "discarded", false)
	if err := store.RecordProviderAcceptEvidence(context.Background(), evidenceID, "ses-evidence-lat"); err != nil {
		t.Fatalf("RecordProviderAcceptEvidence: %v", err)
	}
	// Acceptance 20 minutes ago; provider accepted 30 seconds after
	// acceptance; the job was discarded 16 minutes ago.
	if _, err := pool.Exec(context.Background(),
		`UPDATE messages SET created_at = now() - interval '20 minutes',
		                    provider_accepted_at = now() - interval '19 minutes 30 seconds'
		  WHERE id=$1`, evidenceID); err != nil {
		t.Fatalf("age acceptance/evidence: %v", err)
	}

	rec := &recordingMetrics{}
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter).WithMetrics(rec)
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !stringsEqual(rec.terminals, []string{"sent"}) {
		t.Fatalf("terminals = %v, want [sent] (evidence settle)", rec.terminals)
	}
	if len(rec.latencies) != 1 {
		t.Fatalf("latencies = %v, want exactly one sample", rec.latencies)
	}
	if got := rec.latencies[0]; got < 25 || got > 35 {
		t.Errorf("evidence-settle latency = %.0fs, want ~30s (provider-accept time − accepted_at), not acceptance→sweep (minutes)", got)
	}
}

func TestTerminalReconcileWorker_RecordsTerminalLatencyPerSettledRow(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())

	f := newTerminalFixture(t, pool, store, adapter)
	discardedID := f.seed(t, "lat-discarded", "accepted", "discarded", false)
	missingID := f.seed(t, "lat-missing", "accepted", "", true)
	// Pin acceptance 20 minutes in the past so the latency values are
	// deterministic: the discarded row settles at its finalized_at (16 min
	// ago → ~4 min latency); the missing-job row settles at sweep time
	// (~20 min latency).
	for _, id := range []string{discardedID, missingID} {
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE messages SET created_at = now() - interval '20 minutes' WHERE id=$1`, id); err != nil {
			t.Fatalf("age acceptance for %s: %v", id, err)
		}
	}

	rec := &recordingMetrics{}
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter).WithMetrics(rec)
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	// Latency parity with the terminal counter: exactly one sample per
	// settled row, co-located with the e2a_outbound_terminal_total emission.
	if len(rec.terminals) != 2 {
		t.Fatalf("terminals = %v, want two settled rows", rec.terminals)
	}
	if len(rec.latencies) != 2 {
		t.Fatalf("latencies = %v, want one per settled row", rec.latencies)
	}
	for _, got := range rec.latencies {
		// 4 min (discarded, settled at finalized_at) and 20 min (missing,
		// settled at sweep time); generous band covers sweep scheduling.
		if got < 3*time.Minute.Seconds() || got > 21*time.Minute.Seconds() {
			t.Errorf("terminal latency = %.0fs outside the expected 3–21 min band", got)
		}
	}

	// A second pass settles nothing — no terminal, no latency (exactly-once).
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if len(rec.terminals) != 2 || len(rec.latencies) != 2 {
		t.Errorf("after idempotent re-pass: terminals=%v latencies=%v, want no new samples", rec.terminals, rec.latencies)
	}
}

// The reconciler reads its own gates out of SQL, so the shared anchor helper
// being correct proves nothing about the sweep path: only a store-backed pass
// shows that the candidate query actually selects reviewed_at/scheduled_at and
// that the emit site uses them. A held row stranded by a dead job is exactly
// the population that burned the error budget on 2026-08-03 — settled minutes
// after a review that may have taken hours.
func TestTerminalReconcileWorker_TerminalLatencyExcludesReviewAndScheduleDwell(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())

	f := newTerminalFixture(t, pool, store, adapter)
	heldID := f.seed(t, "lat-held", "accepted", "discarded", false)
	scheduledID := f.seed(t, "lat-scheduled", "accepted", "discarded", false)
	// Both drafted three hours ago and released into the pipeline 30 seconds
	// before their job died. A discarded row settles at its finalized_at, so
	// pinning each gate off that column makes the gate-anchored latency exactly
	// ~30s — where the created_at anchor would record hours.
	for _, tc := range []struct{ id, column string }{
		{heldID, "reviewed_at"},
		{scheduledID, "scheduled_at"},
	} {
		gate := `(SELECT r.finalized_at - interval '30 seconds' FROM river_job r WHERE r.id = m.send_job_id)`
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE messages m SET created_at = now() - interval '3 hours', `+tc.column+` = `+gate+` WHERE m.id=$1`, tc.id); err != nil {
			t.Fatalf("gate %s on %s: %v", tc.column, tc.id, err)
		}
	}

	rec := &recordingMetrics{}
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter).WithMetrics(rec)
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(rec.terminals) != 2 || len(rec.latencies) != 2 {
		t.Fatalf("terminals=%v latencies=%v, want one of each per settled row", rec.terminals, rec.latencies)
	}
	for _, got := range rec.latencies {
		if got < 25 || got > 35 {
			t.Errorf("reconciled terminal latency = %.0fs, want ~30s (settle − the gate); the sweep must anchor at reviewed_at/scheduled_at, not created_at", got)
		}
	}
}

func TestTerminalReconcileWorker_ReconcilesOnlyTerminalJobs(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())

	f := newTerminalFixture(t, pool, store, adapter)
	discardedID := f.seed(t, "discarded", "accepted", "discarded", false)
	cancelledID := f.seed(t, "cancelled", "accepted", "cancelled", false)
	completedID := f.seed(t, "completed", "accepted", "completed", false)
	retryableID := f.seed(t, "retryable", "accepted", "retryable", false)
	sentID := f.seed(t, "sent", "sent", "completed", false)
	missingID := f.seed(t, "missing", "accepted", "", true)

	gate := &fakeRampGate{}
	rec := &recordingMetrics{}
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter, gate).WithMetrics(rec)
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	for _, tc := range []struct {
		name, id, wantStatus, wantState string
	}{
		{"discarded", discardedID, "failed", "discarded"},
		{"cancelled", cancelledID, "failed", "cancelled"},
		{"completed", completedID, "failed", "completed"},
		{"retryable", retryableID, "accepted", ""},
		{"sent", sentID, "sent", ""},
		{"missing", missingID, "failed", "missing"},
	} {
		status, detail := f.status(t, tc.id)
		if status != tc.wantStatus {
			t.Errorf("%s status = %q, want %q", tc.name, status, tc.wantStatus)
		}
		if tc.wantState != "" {
			wantDetail := "outbound send job " + tc.wantState + " before terminal delivery status was recorded"
			if detail != wantDetail {
				t.Errorf("%s detail = %q, want %q", tc.name, detail, wantDetail)
			}
		}
	}
	for _, id := range []string{discardedID, cancelledID, completedID, missingID} {
		if got := f.failedEventCount(t, id); got != 1 {
			t.Errorf("email.failed count for %s = %d, want 1", id, got)
		}
		// Reconciler failures are locally inferred — correctable by later
		// authoritative provider evidence (§3.1).
		var source string
		if err := f.pool.QueryRow(context.Background(),
			`SELECT COALESCE(delivery_failure_source,'') FROM messages WHERE id=$1`, id,
		).Scan(&source); err != nil {
			t.Fatalf("read failure source for %s: %v", id, err)
		}
		if source != "local" {
			t.Errorf("failure source for %s = %q, want local", id, source)
		}
	}
	for _, tc := range []struct {
		id     string
		reason messagelifecycle.ReasonCode
	}{
		{discardedID, messagelifecycle.ReasonSubmissionLocalRetriesExhausted},
		{completedID, messagelifecycle.ReasonSubmissionLocalRetriesExhausted},
		{missingID, messagelifecycle.ReasonSubmissionLocalRetriesExhausted},
		{cancelledID, messagelifecycle.ReasonSubmissionCancelled},
	} {
		tr := f.lifecycleReason(t, tc.id, tc.reason)
		if tr == nil {
			t.Errorf("%s missing %s", tc.id, tc.reason)
			continue
		}
		f.assertEventCarriesOnly(t, tc.id, webhookpub.EventEmailFailed, tr)
	}
	if len(gate.resolved) != 4 {
		t.Errorf("ramp resolutions = %v, want four terminal outcomes", gate.resolved)
	}
	// One terminal metric per settled row; all four sweeps here wrote a
	// locally inferred failure (no provider provenance, no suppression list).
	// One terminal per settled row, labeled by provenance: the cancelled-state
	// job maps to failed_cancelled (policy cancel, not a retry give-up — it
	// must not pollute the local-retries alert signal); the rest are local.
	counts := map[string]int{}
	for _, o := range rec.terminals {
		counts[o]++
	}
	if len(rec.terminals) != 4 || counts["failed_local_retries"] != 3 || counts["failed_cancelled"] != 1 {
		t.Errorf("reconciler terminal metrics = %v, want three failed_local_retries + one failed_cancelled", rec.terminals)
	}
	for _, id := range []string{retryableID, sentID} {
		if got := f.failedEventCount(t, id); got != 0 {
			t.Errorf("email.failed count for untouched %s = %d, want 0", id, got)
		}
	}

	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	for _, id := range []string{discardedID, cancelledID, completedID, missingID} {
		if got := f.failedEventCount(t, id); got != 1 {
			t.Errorf("email.failed count after second pass for %s = %d, want 1", id, got)
		}
	}
}

func TestTerminalReconcileWorker_ResolvesReservedRampForTerminalMessage(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())
	f := newTerminalFixture(t, pool, store, adapter)
	messageID := f.seed(t, "terminal-ramp-cleanup", "accepted", "cancelled", false)

	ctx := context.Background()
	var userID string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM agent_identities WHERE id=$1`, f.agentID).Scan(&userID); err != nil {
		t.Fatalf("read agent owner: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET delivery_status='failed' WHERE id=$1`, messageID); err != nil {
		t.Fatalf("make message terminal: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO domain_send_counters (user_id, domain, day, reserved_count, confirmed_count, daily_limit)
		 VALUES ($1, 'example.com', current_date, 1, 0, 50)`, userID); err != nil {
		t.Fatalf("seed ramp counter: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO sending_ramp_reservations (message_id, day, user_id, domain, units)
		 VALUES ($1, current_date, $2, 'example.com', 1)`, messageID, userID); err != nil {
		t.Fatalf("seed reserved ramp: %v", err)
	}

	gate := &fakeRampGate{}
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter, gate)
	if err := worker.Work(ctx, &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(gate.resolved) != 1 || gate.resolved[0] != messageID {
		t.Fatalf("ramp resolutions = %v, want [%s]", gate.resolved, messageID)
	}
}

func TestTerminalReconcileWorker_ResolvesReleasedRampAfterProviderCorrection(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())
	f := newTerminalFixture(t, pool, store, adapter)
	messageID := f.seed(t, "released-ramp-provider-correction", "accepted", "cancelled", false)

	ctx := context.Background()
	var userID string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM agent_identities WHERE id=$1`, f.agentID).Scan(&userID); err != nil {
		t.Fatalf("read agent owner: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET delivery_status='delivered' WHERE id=$1`, messageID); err != nil {
		t.Fatalf("apply provider correction: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO domain_send_counters (user_id, domain, day, reserved_count, confirmed_count, daily_limit)
		 VALUES ($1, 'example.com', current_date, 0, 0, 50)`, userID); err != nil {
		t.Fatalf("seed ramp counter: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO sending_ramp_reservations (message_id, day, user_id, domain, units, state)
		 VALUES ($1, current_date, $2, 'example.com', 1, 'released')`, messageID, userID); err != nil {
		t.Fatalf("seed released ramp: %v", err)
	}

	gate := &fakeRampGate{}
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter, gate)
	if err := worker.Work(ctx, &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(gate.resolved) != 1 || gate.resolved[0] != messageID {
		t.Fatalf("ramp resolutions = %v, want [%s]", gate.resolved, messageID)
	}
}

// TestTerminalReconcileWorker_GraceWindowHoldsFreshTerminalJobs pins the §3.1
// grace behavior: a row whose job just reached a terminal state is NOT failed
// while provider evidence may still be arriving; it is failed once the job has
// been terminal past the grace window.
func TestTerminalReconcileWorker_GraceWindowHoldsFreshTerminalJobs(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())

	f := newTerminalFixture(t, pool, store, adapter)
	freshID := f.seed(t, "fresh-discard", "accepted", "discarded", false)
	f.freshenJob(t, freshID) // terminal seconds ago — inside the grace window

	gate := &fakeRampGate{}
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter, gate)
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	status, _ := f.status(t, freshID)
	if status != "accepted" {
		t.Fatalf("fresh terminal job's row = %q, want accepted (grace window holds)", status)
	}
	if got := f.failedEventCount(t, freshID); got != 0 {
		t.Fatalf("email.failed count within grace = %d, want 0", got)
	}

	// Age the job past the grace → the genuinely abandoned row is failed with
	// exactly one email.failed.
	if _, err := pool.Exec(context.Background(),
		`UPDATE river_job SET finalized_at = now() - interval '16 minutes'
		  WHERE id = (SELECT send_job_id FROM messages WHERE id=$1)`, freshID); err != nil {
		t.Fatalf("age job: %v", err)
	}
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	status, _ = f.status(t, freshID)
	if status != "failed" {
		t.Fatalf("aged abandoned row = %q, want failed", status)
	}
	if got := f.failedEventCount(t, freshID); got != 1 {
		t.Fatalf("email.failed count past grace = %d, want 1", got)
	}
}

// TestTerminalReconcileWorker_ProviderEvidenceSettlesAsSent pins the guard's
// positive branch: a row with recorded provider-accept evidence is settled as
// SENT immediately (no grace needed, no false email.failed), with the
// evidence-repaired provider id, recipient rows, and one email.sent.
func TestTerminalReconcileWorker_ProviderEvidenceSettlesAsSent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())

	f := newTerminalFixture(t, pool, store, adapter)
	evidenceID := f.seed(t, "evidence", "accepted", "discarded", false)
	if _, err := pool.Exec(context.Background(), `UPDATE messages SET delivery_failure_source='local',delivery_failure_reason_code='submission.cancelled',delivery_detail='stale',delivery_failure_occurred_at=now(),delivery_failure_attempt=4,delivery_failure_blocked_recipients=ARRAY['stale@example.test'] WHERE id=$1`, evidenceID); err != nil {
		t.Fatal(err)
	}
	f.freshenJob(t, evidenceID) // even inside the grace window, evidence settles now

	// The SNS consumer recorded provider-accept evidence (a header-correlated
	// Send/Delivery notification) before the reconciler ran.
	if err := store.RecordProviderAcceptEvidence(context.Background(), evidenceID, "ses-evidence-abc"); err != nil {
		t.Fatalf("RecordProviderAcceptEvidence: %v", err)
	}
	var providerAcceptedAt time.Time
	if err := pool.QueryRow(context.Background(), `SELECT provider_accepted_at FROM messages WHERE id=$1`, evidenceID).Scan(&providerAcceptedAt); err != nil {
		t.Fatal(err)
	}

	gate := &fakeRampGate{}
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter, gate)
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	var status, providerID string
	if err := pool.QueryRow(context.Background(),
		`SELECT delivery_status, COALESCE(provider_message_id,'') FROM messages WHERE id=$1`, evidenceID,
	).Scan(&status, &providerID); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "sent" {
		t.Fatalf("evidence row = %q, want sent (never a false failure)", status)
	}
	if providerID != "ses-evidence-abc" {
		t.Errorf("provider_message_id = %q, want the evidence-repaired id", providerID)
	}
	var staleSource, staleReason, staleDetail string
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(delivery_failure_source,''),COALESCE(delivery_failure_reason_code,''),COALESCE(delivery_detail,'') FROM messages WHERE id=$1`, evidenceID).Scan(&staleSource, &staleReason, &staleDetail); err != nil {
		t.Fatal(err)
	}
	if staleSource != "" || staleReason != "" || staleDetail != "" {
		t.Fatalf("sent correction retained failure provenance source=%q reason=%q detail=%q", staleSource, staleReason, staleDetail)
	}
	var retainedFallback bool
	if err := pool.QueryRow(context.Background(), `SELECT delivery_failure_occurred_at IS NOT NULL OR delivery_failure_attempt IS NOT NULL OR delivery_failure_blocked_recipients IS NOT NULL FROM messages WHERE id=$1`, evidenceID).Scan(&retainedFallback); err != nil {
		t.Fatal(err)
	}
	if retainedFallback {
		t.Fatal("sent correction retained fallback provenance")
	}
	var rcpts int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM message_recipients WHERE message_id=$1 AND status='sent'`, evidenceID,
	).Scan(&rcpts); err != nil {
		t.Fatalf("count recipients: %v", err)
	}
	if rcpts != 1 {
		t.Errorf("sent recipient rows = %d, want 1", rcpts)
	}
	if got := f.sentEventCount(t, evidenceID); got != 1 {
		t.Errorf("email.sent count = %d, want 1", got)
	}
	accepted := f.lifecycleReason(t, evidenceID, messagelifecycle.ReasonSubmissionUpstreamAccepted)
	if accepted == nil {
		t.Fatal("provider correction missing upstream acceptance lifecycle")
	}
	f.assertEventCarriesOnly(t, evidenceID, webhookpub.EventEmailSent, accepted)
	if !accepted.OccurredAt.Equal(providerAcceptedAt) {
		t.Fatalf("correction occurred_at=%s want provider_accepted_at=%s", accepted.OccurredAt, providerAcceptedAt)
	}
	var dedupe string
	if err := pool.QueryRow(context.Background(), `SELECT dedupe_key FROM message_lifecycle_transitions WHERE id=$1`, accepted.ID).Scan(&dedupe); err != nil {
		t.Fatal(err)
	}
	wantDedupe := fmt.Sprintf("submission:job:%d:attempt:0:submission.upstream_accepted", mustSendJobID(t, pool, evidenceID))
	if dedupe != wantDedupe {
		t.Fatalf("correction dedupe=%q want %q", dedupe, wantDedupe)
	}
	if got := f.failedEventCount(t, evidenceID); got != 0 {
		t.Errorf("email.failed count = %d, want 0 — evidence must suppress the false failure", got)
	}
	if len(gate.resolved) != 1 || gate.resolved[0] != evidenceID {
		t.Errorf("ramp resolutions = %v, want evidence message", gate.resolved)
	}

	// Idempotent: a second pass no-ops (the row left accepted/sending).
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if got := f.sentEventCount(t, evidenceID); got != 1 {
		t.Errorf("email.sent count after second pass = %d, want 1", got)
	}
}

func TestTerminalReconcileWorker_InvalidStoredReasonFallsBackConservatively(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store, webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())
	f := newTerminalFixture(t, pool, store, adapter)
	messageID := f.seed(t, "invalid-stored-reason", "accepted", "discarded", false)
	if _, err := pool.Exec(context.Background(), `UPDATE messages SET delivery_failure_source='local',delivery_failure_reason_code='submission.arbitrary' WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	if err := outboundsend.NewTerminalReconcileWorker(pool, adapter).Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatal(err)
	}
	if f.lifecycleReason(t, messageID, messagelifecycle.ReasonSubmissionLocalRetriesExhausted) == nil {
		t.Fatal("invalid reason did not fall back to local retries exhausted")
	}
	var invalid int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.arbitrary'`, messageID).Scan(&invalid); err != nil {
		t.Fatal(err)
	}
	if invalid != 0 {
		t.Fatal("invalid stored reason reached lifecycle")
	}
}

func TestReconcileLifecycleFailureRollsBackAndConverges(t *testing.T) {
	testReconcileAtomicFailure(t, "lifecycle", `
		CREATE FUNCTION test_fail_submission_lifecycle() RETURNS trigger AS $f$ BEGIN
			IF NEW.reason_code='submission.upstream_accepted' THEN RAISE EXCEPTION 'forced lifecycle failure'; END IF; RETURN NEW;
		END; $f$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_submission_lifecycle BEFORE INSERT ON message_lifecycle_transitions FOR EACH ROW EXECUTE FUNCTION test_fail_submission_lifecycle();`,
		`DROP TRIGGER IF EXISTS test_fail_submission_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_submission_lifecycle();`)
}

func TestReconcileOutboxFailureRollsBackAndConverges(t *testing.T) {
	testReconcileAtomicFailure(t, "outbox", `
		CREATE FUNCTION test_fail_submission_outbox() RETURNS trigger AS $f$ BEGIN
			IF NEW.type='email.sent' THEN RAISE EXCEPTION 'forced outbox failure'; END IF; RETURN NEW;
		END; $f$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_submission_outbox BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION test_fail_submission_outbox();`,
		`DROP TRIGGER IF EXISTS test_fail_submission_outbox ON webhook_events; DROP FUNCTION IF EXISTS test_fail_submission_outbox();`)
}

func TestReconcileMeterFailureRollsBackAndConvergesOnce(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store, webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewUsageTracker(usage.NewStore(pool)))
	f := newTerminalFixture(t, pool, store, adapter)
	messageID := f.seed(t, "atomic-meter", "accepted", "discarded", false)
	if err := store.RecordProviderAcceptEvidence(context.Background(), messageID, "provider-meter"); err != nil {
		t.Fatal(err)
	}
	install := `CREATE FUNCTION test_fail_submission_meter() RETURNS trigger AS $f$ BEGIN RAISE EXCEPTION 'forced meter failure'; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_submission_meter BEFORE INSERT ON usage_events FOR EACH ROW EXECUTE FUNCTION test_fail_submission_meter();`
	uninstall := `DROP TRIGGER IF EXISTS test_fail_submission_meter ON usage_events; DROP FUNCTION IF EXISTS test_fail_submission_meter();`
	if _, err := pool.Exec(context.Background(), install); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), uninstall) })
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter)
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err == nil {
		t.Fatal("forced meter failure succeeded")
	}
	status, _ := f.status(t, messageID)
	var usageEvents, summary int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM usage_events WHERE user_id=(SELECT user_id FROM agent_identities WHERE id=$1)`, f.agentID).Scan(&usageEvents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(outbound_count),0) FROM usage_summaries WHERE user_id=(SELECT user_id FROM agent_identities WHERE id=$1)`, f.agentID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" || usageEvents != 0 || summary != 0 || f.sentEventCount(t, messageID) != 0 || f.lifecycleReason(t, messageID, messagelifecycle.ReasonSubmissionUpstreamAccepted) != nil {
		t.Fatalf("partial meter commit status=%s events=%d summary=%d", status, usageEvents, summary)
	}
	if _, err := pool.Exec(context.Background(), uninstall); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM usage_events WHERE user_id=(SELECT user_id FROM agent_identities WHERE id=$1)`, f.agentID).Scan(&usageEvents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(outbound_count),0) FROM usage_summaries WHERE user_id=(SELECT user_id FROM agent_identities WHERE id=$1)`, f.agentID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if usageEvents != 1 || summary != 1 {
		t.Fatalf("recovered metering events=%d summary=%d", usageEvents, summary)
	}
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM usage_events WHERE user_id=(SELECT user_id FROM agent_identities WHERE id=$1)`, f.agentID).Scan(&usageEvents); err != nil {
		t.Fatal(err)
	}
	if usageEvents != 1 {
		t.Fatalf("duplicate metering=%d", usageEvents)
	}
}

func TestSendWorkerProviderRejectionLifecycleFailurePreservesProvenanceForReconcile(t *testing.T) {
	testProviderRejectionAtomicFailure(t, "lifecycle", `CREATE FUNCTION test_fail_provider_reject_lifecycle() RETURNS trigger AS $f$ BEGIN IF NEW.reason_code='submission.provider_rejected' THEN RAISE EXCEPTION 'forced lifecycle failure'; END IF; RETURN NEW; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_provider_reject_lifecycle BEFORE INSERT ON message_lifecycle_transitions FOR EACH ROW EXECUTE FUNCTION test_fail_provider_reject_lifecycle();`, `DROP TRIGGER IF EXISTS test_fail_provider_reject_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_provider_reject_lifecycle();`)
}

func TestSendWorkerProviderRejectionOutboxFailurePreservesProvenanceForReconcile(t *testing.T) {
	testProviderRejectionAtomicFailure(t, "outbox", `CREATE FUNCTION test_fail_provider_reject_outbox() RETURNS trigger AS $f$ BEGIN IF NEW.type='email.failed' THEN RAISE EXCEPTION 'forced outbox failure'; END IF; RETURN NEW; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_provider_reject_outbox BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION test_fail_provider_reject_outbox();`, `DROP TRIGGER IF EXISTS test_fail_provider_reject_outbox ON webhook_events; DROP FUNCTION IF EXISTS test_fail_provider_reject_outbox();`)
}

func TestSendWorkerLocalExhaustionLifecycleFailurePreservesExactReason(t *testing.T) {
	testLocalFallbackReason(t, "exhaust-lifecycle", messagelifecycle.ReasonSubmissionLocalRetriesExhausted, "lifecycle")
}
func TestSendWorkerLocalExhaustionOutboxFailurePreservesExactReason(t *testing.T) {
	testLocalFallbackReason(t, "exhaust-outbox", messagelifecycle.ReasonSubmissionLocalRetriesExhausted, "outbox")
}
func TestSendWorkerCancellationFailurePreservesExactReason(t *testing.T) {
	testLocalFallbackReason(t, "cancel-lifecycle", messagelifecycle.ReasonSubmissionCancelled, "lifecycle")
}

func testLocalFallbackReason(t *testing.T, label string, want messagelifecycle.ReasonCode, fault string) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store, webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())
	f := newTerminalFixture(t, pool, store, adapter)
	messageID := f.seed(t, label, "accepted", "discarded", false)
	jobID := mustSendJobID(t, pool, messageID)
	var install, uninstall string
	if fault == "outbox" {
		install = `CREATE FUNCTION test_fail_local_reason_outbox() RETURNS trigger AS $f$ BEGIN IF NEW.type='email.failed' THEN RAISE EXCEPTION 'forced outbox'; END IF; RETURN NEW; END;$f$ LANGUAGE plpgsql;CREATE TRIGGER test_fail_local_reason_outbox BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION test_fail_local_reason_outbox();`
		uninstall = `DROP TRIGGER IF EXISTS test_fail_local_reason_outbox ON webhook_events;DROP FUNCTION IF EXISTS test_fail_local_reason_outbox();`
	} else {
		install = `CREATE FUNCTION test_fail_local_reason_lifecycle() RETURNS trigger AS $f$ BEGIN IF NEW.reason_code IN ('submission.local_retries_exhausted','submission.cancelled') THEN RAISE EXCEPTION 'forced lifecycle'; END IF; RETURN NEW; END;$f$ LANGUAGE plpgsql;CREATE TRIGGER test_fail_local_reason_lifecycle BEFORE INSERT ON message_lifecycle_transitions FOR EACH ROW EXECUTE FUNCTION test_fail_local_reason_lifecycle();`
		uninstall = `DROP TRIGGER IF EXISTS test_fail_local_reason_lifecycle ON message_lifecycle_transitions;DROP FUNCTION IF EXISTS test_fail_local_reason_lifecycle();`
	}
	if _, err := pool.Exec(context.Background(), install); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), uninstall) })
	var worker *outboundsend.SendWorker
	if want == messagelifecycle.ReasonSubmissionCancelled {
		if _, err := pool.Exec(context.Background(), `UPDATE messages SET sent_as='own_address' WHERE id=$1`, messageID); err != nil {
			t.Fatal(err)
		}
		worker = outboundsend.NewSendWorker(adapter, &fakeDeliverer{}, &fakeRampGate{err: permanentRampError{msg: "invalid ramp"}})
	} else {
		if _, err := pool.Exec(context.Background(), `UPDATE messages SET created_at=now()-interval '73 hours' WHERE id=$1`, messageID); err != nil {
			t.Fatal(err)
		}
		worker = outboundsend.NewSendWorker(adapter, &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("provider unavailable"), Outage: true}})
	}
	rj := &river.Job[outboundsend.OutboundSendArgs]{JobRow: &rivertype.JobRow{ID: jobID, Attempt: 3, CreatedAt: time.Now().UTC()}, Args: outboundsend.OutboundSendArgs{MessageID: messageID}}
	if err := worker.Work(context.Background(), rj); err == nil {
		t.Fatal("terminal branch must return cancellation/error")
	}
	var status, source, reason string
	if err := pool.QueryRow(context.Background(), `SELECT delivery_status,COALESCE(delivery_failure_source,''),COALESCE(delivery_failure_reason_code,'') FROM messages WHERE id=$1`, messageID).Scan(&status, &source, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" || source != "local" || reason != string(want) {
		t.Fatalf("fallback status=%q source=%q reason=%q want %q", status, source, reason, want)
	}
	if f.failedEventCount(t, messageID) != 0 || f.lifecycleReason(t, messageID, want) != nil {
		t.Fatal("fallback committed partial terminal observation")
	}
	if _, err := pool.Exec(context.Background(), uninstall); err != nil {
		t.Fatal(err)
	}
	if err := outboundsend.NewTerminalReconcileWorker(pool, adapter).Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatal(err)
	}
	tr := f.lifecycleReason(t, messageID, want)
	if tr == nil {
		t.Fatalf("reconciler lost exact reason %s", want)
	}
	f.assertEventCarriesOnly(t, messageID, webhookpub.EventEmailFailed, tr)
}

func testProviderRejectionAtomicFailure(t *testing.T, label, install, uninstall string) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store, webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())
	f := newTerminalFixture(t, pool, store, adapter)
	messageID := f.seed(t, "provider-reject-"+label, "accepted", "discarded", false)
	jobID := mustSendJobID(t, pool, messageID)
	if _, err := pool.Exec(context.Background(), `UPDATE messages SET sent_as='own_address' WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), install); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), uninstall) })
	deliverer := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("550 explicit rejection"), Permanent: true}}
	ramp := &fakeRampGate{decision: outboundsend.RampDecision{Allowed: true}}
	w := outboundsend.NewSendWorker(adapter, deliverer, ramp)
	rj := &river.Job[outboundsend.OutboundSendArgs]{JobRow: &rivertype.JobRow{ID: jobID, Attempt: 2, CreatedAt: time.Now().UTC()}, Args: outboundsend.OutboundSendArgs{MessageID: messageID}}
	if err := w.Work(context.Background(), rj); err == nil {
		t.Fatal("provider rejection must cancel")
	}
	var status, source, detail, storedReason string
	var storedOccurredAt *time.Time
	var storedAttempt *int
	if err := pool.QueryRow(context.Background(), `SELECT delivery_status,COALESCE(delivery_failure_source,''),COALESCE(delivery_detail,''),COALESCE(delivery_failure_reason_code,''),delivery_failure_occurred_at,delivery_failure_attempt FROM messages WHERE id=$1`, messageID).Scan(&status, &source, &detail, &storedReason, &storedOccurredAt, &storedAttempt); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" || source != "provider" || detail != "550 explicit rejection" || storedReason != "submission.provider_rejected" {
		t.Fatalf("fallback status=%q source=%q detail=%q reason=%q", status, source, detail, storedReason)
	}
	if storedOccurredAt == nil || storedAttempt == nil || *storedAttempt != 2 {
		t.Fatalf("fallback occurred_at=%v attempt=%v, want exact provider observation and attempt 2", storedOccurredAt, storedAttempt)
	}
	if storedOccurredAt.Before(deliverer.returnedAt.Truncate(time.Microsecond)) {
		t.Fatalf("fallback occurred_at=%s predates provider result=%s", *storedOccurredAt, deliverer.returnedAt)
	}
	if f.failedEventCount(t, messageID) != 0 || f.lifecycleReason(t, messageID, messagelifecycle.ReasonSubmissionProviderRejected) != nil {
		t.Fatal("failed terminal tx published partial event/lifecycle")
	}
	if err := w.Work(context.Background(), rj); err != nil {
		t.Fatalf("fallback re-drive: %v", err)
	}
	if deliverer.calls != 1 {
		t.Fatalf("fallback re-drive provider calls=%d, want exactly the original call", deliverer.calls)
	}
	if len(ramp.calls) != 1 || len(ramp.released) != 1 || len(ramp.resolved) != 1 {
		t.Fatalf("fallback ramp reserve=%d release=%v resolve=%v, want one of each without re-reserve", len(ramp.calls), ramp.released, ramp.resolved)
	}
	if _, err := pool.Exec(context.Background(), uninstall); err != nil {
		t.Fatal(err)
	}
	if err := outboundsend.NewTerminalReconcileWorker(pool, adapter).Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatal(err)
	}
	tr := f.lifecycleReason(t, messageID, messagelifecycle.ReasonSubmissionProviderRejected)
	if tr == nil {
		t.Fatal("reconciler lost provider rejection provenance")
	}
	if !tr.OccurredAt.Equal(storedOccurredAt.UTC()) {
		t.Fatalf("reconciled occurred_at=%s want preserved %s", tr.OccurredAt, storedOccurredAt.UTC())
	}
	var dedupeKey string
	if err := pool.QueryRow(context.Background(), `SELECT dedupe_key FROM message_lifecycle_transitions WHERE id=$1`, tr.ID).Scan(&dedupeKey); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dedupeKey, ":attempt:2:") {
		t.Fatalf("reconciled dedupe_key=%q want preserved attempt 2", dedupeKey)
	}
	f.assertEventCarriesOnly(t, messageID, webhookpub.EventEmailFailed, tr)
	if err := outboundsend.NewTerminalReconcileWorker(pool, adapter).Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatal(err)
	}
	if f.failedEventCount(t, messageID) != 1 {
		t.Fatal("provider rejection reconciliation duplicated event")
	}
}

func testReconcileAtomicFailure(t *testing.T, label, install, uninstall string) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store, webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())
	f := newTerminalFixture(t, pool, store, adapter)
	messageID := f.seed(t, "atomic-"+label, "accepted", "discarded", false)
	if err := store.RecordProviderAcceptEvidence(context.Background(), messageID, "provider-"+label); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), install); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), uninstall) })
	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter)
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err == nil {
		t.Fatal("forced transactional failure unexpectedly succeeded")
	}
	status, _ := f.status(t, messageID)
	if status != "accepted" || f.sentEventCount(t, messageID) != 0 || f.lifecycleReason(t, messageID, messagelifecycle.ReasonSubmissionUpstreamAccepted) != nil {
		t.Fatalf("partial commit after %s failure: status=%s sent_events=%d transition=%+v", label, status, f.sentEventCount(t, messageID), f.lifecycleReason(t, messageID, messagelifecycle.ReasonSubmissionUpstreamAccepted))
	}
	if _, err := pool.Exec(context.Background(), uninstall); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("reconcile after repair: %v", err)
	}
	if status, _ = f.status(t, messageID); status != "sent" {
		t.Fatalf("reconciled status=%s want sent", status)
	}
	tr := f.lifecycleReason(t, messageID, messagelifecycle.ReasonSubmissionUpstreamAccepted)
	if tr == nil || f.sentEventCount(t, messageID) != 1 {
		t.Fatalf("did not converge: transition=%+v events=%d", tr, f.sentEventCount(t, messageID))
	}
	f.assertEventCarriesOnly(t, messageID, webhookpub.EventEmailSent, tr)
}

// TestTerminalReconcileWorker_DeferredDetailPreferred pins that a final
// attempt's deferred diagnostic survives the reconciler's sweep (the generic
// sweep detail must not clobber it).
func TestTerminalReconcileWorker_DeferredDetailPreferred(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	adapter := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())

	f := newTerminalFixture(t, pool, store, adapter)
	deferredID := f.seed(t, "deferred-detail", "accepted", "discarded", false)

	// The worker's final attempt deferred with its real diagnostic.
	var jobID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT send_job_id FROM messages WHERE id=$1`, deferredID).Scan(&jobID); err != nil {
		t.Fatalf("read job id: %v", err)
	}
	if err := store.DeferOutboundTerminalFailure(context.Background(), deferredID, jobID, "451 4.3.0 timeout after DATA"); err != nil {
		t.Fatalf("DeferOutboundTerminalFailure: %v", err)
	}

	worker := outboundsend.NewTerminalReconcileWorker(pool, adapter)
	if err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	status, detail := f.status(t, deferredID)
	if status != "failed" {
		t.Fatalf("deferred row = %q, want failed", status)
	}
	if detail != "451 4.3.0 timeout after DATA" {
		t.Errorf("detail = %q, want the deferred diagnostic preserved", detail)
	}
	if got := f.failedEventCount(t, deferredID); got != 1 {
		t.Errorf("email.failed count = %d, want 1", got)
	}
}

type failingTerminalStore struct{ err error }

func (s failingTerminalStore) ClaimSend(context.Context, string, int64) (*outboundsend.SendJob, error) {
	return nil, nil
}
func (s failingTerminalStore) ReleaseSend(context.Context, string, int64) error { return nil }
func (s failingTerminalStore) MarkSent(context.Context, string, int64, int, time.Time, string, string) error {
	return nil
}
func (s failingTerminalStore) MarkFailed(_ context.Context, _ string, _ int64, _ int, occurredAt time.Time, _ string, _ delivery.FailureSource, _ messagelifecycle.ReasonCode, _ []string) (delivery.Status, time.Time, error) {
	if s.err != nil {
		return "", time.Time{}, s.err
	}
	return delivery.StatusFailed, occurredAt, nil
}
func (s failingTerminalStore) PreserveTerminalFailure(context.Context, string, int64, int, time.Time, string, delivery.FailureSource, messagelifecycle.ReasonCode, []string) error {
	return nil
}
func (s failingTerminalStore) SuppressedRecipients(context.Context, string, string, []string) ([]string, error) {
	return nil, nil
}
func (s failingTerminalStore) DeferTerminalFailure(context.Context, string, int64, int, time.Time, string) error {
	return nil
}
func (s failingTerminalStore) RecordTemporaryFailure(context.Context, string, int64, int, time.Time, string) error {
	return nil
}

func TestTerminalReconcileWorker_PropagatesStoreFailure(t *testing.T) {
	sentinel := errors.New("mark failed unavailable")
	f := newTerminalFixture(t, nil, nil, failingTerminalStore{err: sentinel})
	f.seed(t, "store-error", "accepted", "discarded", false)

	worker := outboundsend.NewTerminalReconcileWorker(f.pool, failingTerminalStore{err: sentinel})
	err := worker.Work(context.Background(), &river.Job[outboundsend.TerminalReconcileArgs]{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Work error = %v, want %v", err, sentinel)
	}
}

func TestRegisterJobs_RegistersTerminalReconcilePeriodic(t *testing.T) {
	j := outboundsend.NewJobs(nil, nil, nil)
	periodics := j.RegisterJobs(river.NewWorkers())
	if len(periodics) != 1 {
		t.Fatalf("RegisterJobs periodics = %d, want 1", len(periodics))
	}
}
