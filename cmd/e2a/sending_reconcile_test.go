package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhooknotify"
)

// insertLegacyJob enqueues a River job the way a pre-floor slot did: the
// args carry no operation_ref. Raw SQL on purpose — the typed enqueuers
// always prepare a reference now, so the only way to produce a legacy job in
// a test is to write one the old way.
func insertLegacyJob(t *testing.T, pool *pgxpool.Pool, kind, args string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO river_job (args, kind, max_attempts) VALUES ($1::jsonb, $2, 3) RETURNING id`,
		args, kind).Scan(&id); err != nil {
		t.Fatalf("insert legacy %s job: %v", kind, err)
	}
	return id
}

// resetRiverJobs empties the shared per-package river_job table: the test DB
// helper leaves River's tables alone, so legacy rows one test writes would
// otherwise be scanned by the next.
func resetRiverJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := jobs.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE river_job RESTART IDENTITY`); err != nil {
		t.Fatalf("reset river_job: %v", err)
	}
}

func legacyJobState(t *testing.T, pool *pgxpool.Pool, id int64) (state, opID string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT state, COALESCE(args->'operation_ref'->>'id', '') FROM river_job WHERE id = $1`, id,
	).Scan(&state, &opID); err != nil {
		t.Fatalf("read job %d: %v", id, err)
	}
	return state, opID
}

func seedReconcileSource(t *testing.T, pool *pgxpool.Pool, store *identity.Store, slug string) (*identity.Message, *identity.Webhook) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "owner-"+slug+"@reviewer.test", "Owner", "google-reconcile-"+slug)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, slug+".bot.test", user.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyDomain(ctx, slug+".bot.test", user.ID); err != nil {
		t.Fatal(err)
	}
	a, err := store.CreateAgent(ctx, "bot@"+slug+".bot.test", slug+".bot.test", "", "https://example.com/webhook", "", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"alice@example.com"}, nil, nil,
		"Held draft", "body", "", nil, "send", "conv_"+slug, "", "", 3600)
	if err != nil {
		t.Fatal(err)
	}
	wh, err := store.CreateWebhook(ctx, user.ID, "https://hooks.example.com/e2a", "",
		[]string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatal(err)
	}
	// The sweep stamps the warning episode before it enqueues the notice;
	// a legacy warning job's operation is keyed by that stamp.
	if _, err := pool.Exec(ctx, `UPDATE webhooks SET warn_notified_at = now() WHERE id = $1`, wh.ID); err != nil {
		t.Fatal(err)
	}
	wh, err = store.GetWebhookByIDInternal(ctx, wh.ID)
	if err != nil {
		t.Fatal(err)
	}
	return msg, wh
}

// TestReconcileLegacySendingJobs: every pending provider-submitting job
// without an operation reference is decided in one pass — stamped when its
// source row exists, cancelled when it does not — and a second pass finds
// nothing left. A job River already finalized is out of scope.
func TestReconcileLegacySendingJobs(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	resetRiverJobs(t, pool)
	store := identity.NewStore(pool)
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	msg, wh := seedReconcileSource(t, pool, store, "reconcile")

	sendLive := insertLegacyJob(t, pool, "outbound_send", `{"message_id":"`+msg.ID+`"}`)
	sendGone := insertLegacyJob(t, pool, "outbound_send", `{"message_id":"msg_does_not_exist"}`)
	hitlLive := insertLegacyJob(t, pool, "hitl_notify", `{"message_id":"`+msg.ID+`"}`)
	whLive := insertLegacyJob(t, pool, "webhook_notify", `{"webhook_id":"`+wh.ID+`","kind":"warning"}`)
	whGone := insertLegacyJob(t, pool, "webhook_notify", `{"webhook_id":"wh_does_not_exist","kind":"disabled"}`)
	finalized := insertLegacyJob(t, pool, "outbound_send", `{"message_id":"msg_finalized"}`)
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, finalized); err != nil {
		t.Fatal(err)
	}
	other := insertLegacyJob(t, pool, "outbound_terminal_reconcile", `{"message_id":"`+msg.ID+`"}`)

	var out bytes.Buffer
	if err := runReconcileLegacySendingJobs(ctx, pool, gate, &out); err != nil {
		t.Fatalf("reconcile: %v\nOUTPUT:\n%s", err, out.String())
	}
	for _, want := range []string{"scanned:   5", "stamped:   3", "cancelled: 2", "failed:    0", "remaining: 0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}

	if state, op := legacyJobState(t, pool, sendLive); state != "available" || op != msg.ID {
		t.Errorf("live send job: state=%s op=%q, want available with the message id", state, op)
	}
	if state, op := legacyJobState(t, pool, hitlLive); state != "available" || op != sendingpolicy.HITLNotificationOperationID(msg.ID) {
		t.Errorf("live hitl job: state=%s op=%q, want available with the message's notification operation", state, op)
	}
	if state, op := legacyJobState(t, pool, whLive); state != "available" || op != webhooknotify.ExpectedOperationID(wh, webhooknotify.KindWarning) {
		t.Errorf("live webhook job: state=%s op=%q, want available with the warning episode's operation", state, op)
	}
	for name, id := range map[string]int64{"send": sendGone, "webhook": whGone} {
		if state, op := legacyJobState(t, pool, id); state != "cancelled" || op != "" {
			t.Errorf("orphan %s job: state=%s op=%q, want cancelled and unstamped", name, state, op)
		}
	}
	if state, op := legacyJobState(t, pool, finalized); state != "completed" || op != "" {
		t.Errorf("finalized job touched: state=%s op=%q", state, op)
	}
	if state, op := legacyJobState(t, pool, other); state != "available" || op != "" {
		t.Errorf("non-submitting kind touched: state=%s op=%q", state, op)
	}

	// The stamped reference must round-trip: the same bytes a native enqueue
	// would have written, so a worker reading it authorizes identically.
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT args->'operation_ref' FROM river_job WHERE id = $1`, sendLive).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ref sendingpolicy.OperationRef
	if err := ref.UnmarshalJSON(raw); err != nil || ref.ID() != msg.ID {
		t.Fatalf("stamped reference does not decode to the message operation: err=%v id=%q", err, ref.ID())
	}

	out.Reset()
	if err := runReconcileLegacySendingJobs(ctx, pool, gate, &out); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !strings.Contains(out.String(), "scanned:   0") {
		t.Errorf("second pass should find nothing:\n%s", out.String())
	}
}

// TestReconcileLegacySendingJobsReportsUndecided: a job the resolver cannot
// decide is reported, left untouched, and makes the command exit nonzero so a
// cutover script cannot mistake a partial pass for a clean one.
func TestReconcileLegacySendingJobsReportsUndecided(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	resetRiverJobs(t, pool)
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	broken := insertLegacyJob(t, pool, "outbound_send", `{"message_id":123}`)

	var out bytes.Buffer
	err := runReconcileLegacySendingJobs(ctx, pool, gate, &out)
	if err == nil || !strings.Contains(err.Error(), "1 legacy sending job(s) could not be reconciled") {
		t.Fatalf("err = %v, want the undecided count", err)
	}
	for _, want := range []string{"failed:    1", "remaining: 1", "decode args"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	if state, op := legacyJobState(t, pool, broken); state != "available" || op != "" {
		t.Errorf("undecided job touched: state=%s op=%q", state, op)
	}
}

// TestReconcileLegacySendingJobsLeavesClaimedJobsToTheirWorker: a job that
// left the reconcilable states (a worker claimed it) or was stamped by its
// worker between the scan and its own transaction is skipped untouched — no
// second operation, no cancel under a running worker.
func TestReconcileLegacySendingJobsLeavesClaimedJobsToTheirWorker(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	resetRiverJobs(t, pool)
	store := identity.NewStore(pool)
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	msg, _ := seedReconcileSource(t, pool, store, "claimed")

	running := insertLegacyJob(t, pool, "hitl_notify", `{"message_id":"`+msg.ID+`"}`)
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state = 'running', attempted_at = now() WHERE id = $1`, running); err != nil {
		t.Fatal(err)
	}
	orphanRunning := insertLegacyJob(t, pool, "outbound_send", `{"message_id":"msg_gone"}`)
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state = 'running', attempted_at = now() WHERE id = $1`, orphanRunning); err != nil {
		t.Fatal(err)
	}

	var ops int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_provider_operations`).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runReconcileLegacySendingJobs(ctx, pool, gate, &out); err != nil {
		t.Fatalf("reconcile: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "scanned:   0") {
		t.Fatalf("running jobs must not be scanned:\n%s", out.String())
	}
	for name, id := range map[string]int64{"running": running, "orphan running": orphanRunning} {
		if state, op := legacyJobState(t, pool, id); state != "running" || op != "" {
			t.Errorf("%s job touched: state=%s op=%q", name, state, op)
		}
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_provider_operations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != ops {
		t.Errorf("operations minted for jobs the command did not own: %d → %d", ops, after)
	}

	// The per-job transaction re-checks under lock: simulate a worker that
	// claimed the job after the scan by driving the per-job step directly.
	claimed := insertLegacyJob(t, pool, "hitl_notify", `{"message_id":"`+msg.ID+`"}`)
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state = 'running', attempted_at = now() WHERE id = $1`, claimed); err != nil {
		t.Fatal(err)
	}
	client, err := jobs.New(pool, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := reconcileLegacySendingJob(ctx, pool, client, gate, claimed, "hitl_notify", []byte(`{"message_id":"`+msg.ID+`"}`))
	if err != nil || outcome != legacyOutcomeSkipped {
		t.Fatalf("claimed job: outcome=%v err=%v, want skipped", outcome, err)
	}
	stampedByWorker := insertLegacyJob(t, pool, "hitl_notify", `{"message_id":"`+msg.ID+`","operation_ref":{"v":1,"id":"op_hitl_`+msg.ID+`"}}`)
	outcome, err = reconcileLegacySendingJob(ctx, pool, client, gate, stampedByWorker, "hitl_notify", []byte(`{"message_id":"`+msg.ID+`"}`))
	if err != nil || outcome != legacyOutcomeSkipped {
		t.Fatalf("already stamped job: outcome=%v err=%v, want skipped", outcome, err)
	}
}

// TestReconcileLegacySendingJobsReKeysPreDerivationReferences: a notify job
// migration 113 stamped with op_<md5> is scanned, re-resolved through the
// Prepare path and re-keyed to the derived id; a conforming one is left alone.
func TestReconcileLegacySendingJobsReKeysPreDerivationReferences(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	resetRiverJobs(t, pool)
	store := identity.NewStore(pool)
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	msg, wh := seedReconcileSource(t, pool, store, "rekey")

	md5Hitl := insertLegacyJob(t, pool, "hitl_notify", `{"message_id":"`+msg.ID+`","operation_ref":{"v":1,"id":"op_0123456789abcdef0123456789abcdef"}}`)
	md5Wh := insertLegacyJob(t, pool, "webhook_notify", `{"webhook_id":"`+wh.ID+`","kind":"warning","operation_ref":{"v":1,"id":"op_fedcba9876543210fedcba9876543210"}}`)
	conforming := insertLegacyJob(t, pool, "hitl_notify", `{"message_id":"`+msg.ID+`","operation_ref":{"v":1,"id":"`+sendingpolicy.HITLNotificationOperationID(msg.ID)+`"}}`)
	send := insertLegacyJob(t, pool, "outbound_send", `{"message_id":"`+msg.ID+`","operation_ref":{"v":1,"id":"`+msg.ID+`"}}`)

	var out bytes.Buffer
	if err := runReconcileLegacySendingJobs(ctx, pool, gate, &out); err != nil {
		t.Fatalf("reconcile: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "scanned:   2") || !strings.Contains(out.String(), "stamped:   2") {
		t.Fatalf("want the two md5-keyed jobs scanned and re-keyed:\n%s", out.String())
	}
	if _, op := legacyJobState(t, pool, md5Hitl); op != sendingpolicy.HITLNotificationOperationID(msg.ID) {
		t.Errorf("hitl job op = %q, want the derived id", op)
	}
	if _, op := legacyJobState(t, pool, md5Wh); op != webhooknotify.ExpectedOperationID(wh, webhooknotify.KindWarning) {
		t.Errorf("webhook job op = %q, want the warning episode's derived id", op)
	}
	if _, op := legacyJobState(t, pool, conforming); op != sendingpolicy.HITLNotificationOperationID(msg.ID) {
		t.Errorf("conforming hitl job touched: %q", op)
	}
	if _, op := legacyJobState(t, pool, send); op != msg.ID {
		t.Errorf("conforming send job touched: %q", op)
	}
}
