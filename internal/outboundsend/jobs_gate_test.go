package outboundsend_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// These tests drive the real gate against real Postgres through the jobs
// bundle: the accept transaction prepares the operation, a paused account is
// refused at the door, and a legacy job with no reference authorizes through
// the same path as a new one.

type gateFixture struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	store   *identity.Store
	adapter outboundsend.Store
	gate    sendingpolicy.Gate
	userID  string
	agentID string
	client  jobs.Enqueuer
	gated   *outboundsend.Jobs
	legacy  *outboundsend.Jobs
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	ctx := context.Background()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	user, err := store.CreateOrGetUser(ctx, "owner-gate@example.test", "Owner", "google-gate")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain := "gate.example.test"
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
	adapter := agent.NewOutboundSendStore(store, webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), usage.NewNoopUsageTracker())
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	gated := outboundsend.NewJobs(adapter, &fakeDeliverer{}, pool).WithGate(gate)
	legacy := outboundsend.NewJobs(adapter, &fakeDeliverer{}, pool)
	client, err := jobs.New(pool, jobs.Config{}, gated)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	gated.SetEnqueuer(client)
	legacy.SetEnqueuer(client)
	return &gateFixture{t: t, ctx: ctx, pool: pool, store: store, adapter: adapter, gate: gate, userID: user.ID, agentID: ag.ID, client: client, gated: gated, legacy: legacy}
}

// accept runs the accept transaction the API runs, through the given bundle.
func (f *gateFixture) accept(bundle *outboundsend.Jobs, label string) (messageID string, jobID int64, err error) {
	f.t.Helper()
	err = f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
		m, err := f.store.CreateOutboundMessageTx(f.ctx, tx, f.agentID,
			[]string{label + "@example.test"}, nil, nil, label, "send", "smtp", "", "conv-"+label,
			[]byte("From: bot\r\n\r\nbody"), "accepted", "bot@gate.example.test", "relay")
		if err != nil {
			return err
		}
		messageID = m.ID
		jobID, err = bundle.EnqueueSendTx(f.ctx, tx, messageID)
		if err != nil {
			return err
		}
		return f.store.StampSendJobIDTx(f.ctx, tx, messageID, jobID)
	})
	return messageID, jobID, err
}

func (f *gateFixture) operationExists(messageID string) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sending_provider_operations WHERE operation_id = $1 AND purpose = 'customer_message'`, messageID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

func TestJobs_EnqueuePreparesTheOperationInTheAcceptTransaction(t *testing.T) {
	f := newGateFixture(t)
	messageID, jobID, err := f.accept(f.gated, "prepared")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	var refID string
	if err := f.pool.QueryRow(f.ctx, `SELECT args->'operation_ref'->>'id' FROM river_job WHERE id = $1`, jobID).Scan(&refID); err != nil {
		t.Fatal(err)
	}
	if refID != messageID {
		t.Fatalf("job carries operation_ref id %q, want the message id %q", refID, messageID)
	}
	if !f.operationExists(messageID) {
		t.Fatal("no customer_message operation was prepared in the accept transaction")
	}
}

func TestJobs_EnqueueRefusesAPausedAccountAndRollsBack(t *testing.T) {
	f := newGateFixture(t)
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO account_sending_controls (user_id, state, reason, actor) VALUES ($1, 'paused', 'test', 'test')
		ON CONFLICT (user_id) DO UPDATE SET state = 'paused'`, f.userID); err != nil {
		t.Fatal(err)
	}
	messageID, _, err := f.accept(f.gated, "paused")
	if !errors.Is(err, outboundsend.ErrSendingPaused) {
		t.Fatalf("accept on a paused account err = %v, want ErrSendingPaused", err)
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM messages WHERE id = $1`, messageID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("message row survived the refused accept; the transaction must roll back")
	}
}

func TestJobs_LegacyJobResolvesAndAuthorizesThroughTheGate(t *testing.T) {
	f := newGateFixture(t)
	// A pre-floor slot enqueued this job: no operation reference in its args.
	messageID, jobID, err := f.accept(f.legacy, "legacy")
	if err != nil {
		t.Fatalf("legacy accept: %v", err)
	}
	var hasRef bool
	if err := f.pool.QueryRow(f.ctx, `SELECT args ? 'operation_ref' FROM river_job WHERE id = $1`, jobID).Scan(&hasRef); err != nil {
		t.Fatal(err)
	}
	if hasRef || f.operationExists(messageID) {
		t.Fatal("the legacy enqueue must carry no reference and prepare nothing")
	}

	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "<ses-legacy@example.test>"}}
	w := outboundsend.NewSendWorker(f.adapter, dl).WithGate(f.gate).WithOperationResolver(f.gated.ResolveLegacyOperation)
	rj := &river.Job[outboundsend.OutboundSendArgs]{
		JobRow: &rivertype.JobRow{ID: jobID, Attempt: 1, MaxAttempts: outboundsend.MaxSendAttempts, Kind: outboundsend.OutboundSendArgs{}.Kind()},
		Args:   outboundsend.OutboundSendArgs{MessageID: messageID},
	}
	if err := w.Work(f.ctx, rj); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly one", dl.calls)
	}
	if !f.operationExists(messageID) {
		t.Fatal("the resolver did not prepare the operation")
	}
	var state, callState string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT state, call_state FROM sending_budget_reservations
		 WHERE operation_id = $1 AND submission_attempt = 1`, messageID).Scan(&state, &callState); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	if state != "confirmed" {
		t.Fatalf("attempt state = %s, want confirmed (final authorization ran)", state)
	}
	var status string
	if err := f.pool.QueryRow(f.ctx, `SELECT delivery_status FROM messages WHERE id = $1`, messageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("delivery_status = %s, want sent", status)
	}
}

func TestJobs_GatedWorkerAuthorizesANewJob(t *testing.T) {
	f := newGateFixture(t)
	messageID, jobID, err := f.accept(f.gated, "gated")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "<ses-gated@example.test>"}}
	w := outboundsend.NewSendWorker(f.adapter, dl).WithGate(f.gate)
	ref := refFor(messageID)
	rj := &river.Job[outboundsend.OutboundSendArgs]{
		JobRow: &rivertype.JobRow{ID: jobID, Attempt: 1, MaxAttempts: outboundsend.MaxSendAttempts, Kind: outboundsend.OutboundSendArgs{}.Kind()},
		Args:   outboundsend.OutboundSendArgs{MessageID: messageID, OperationRef: &ref},
	}
	if err := w.Work(f.ctx, rj); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.calls != 1 || len(dl.auths) != 1 || dl.auths[0].IsZero() {
		t.Fatalf("calls=%d auths=%d, want one provider call carrying a real authorization", dl.calls, len(dl.auths))
	}
	// A re-drive of the sent row is a no-op: no new ordinal, no new call.
	if err := w.Work(f.ctx, rj); err != nil {
		t.Fatalf("re-drive: %v", err)
	}
	var attempts int
	if err := f.pool.QueryRow(f.ctx, `SELECT current_attempt FROM sending_provider_operations WHERE operation_id = $1`, messageID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if dl.calls != 1 || attempts != 1 {
		t.Fatalf("after re-drive calls=%d current_attempt=%d, want 1/1", dl.calls, attempts)
	}
}

// TestJobs_ReconcilerSettlesTheDialedAttemptFromEvidence: the worker dialed
// (the token was redeemed) but lost the 250; SES's feedback later proved
// acceptance; the job is terminal. The reconciler settles the row as sent and,
// through the gate, settles the attempt that dialed — binding the provider id
// to its correlation — without resubmitting or reserving anything.
func TestJobs_ReconcilerSettlesTheDialedAttemptFromEvidence(t *testing.T) {
	f := newGateFixture(t)
	messageID, jobID, err := f.accept(f.gated, "evidence")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("data final: lost"), AcceptanceUnknown: true}}
	w := outboundsend.NewSendWorker(f.adapter, dl).WithGate(f.gate)
	ref := refFor(messageID)
	rj := &river.Job[outboundsend.OutboundSendArgs]{
		JobRow: &rivertype.JobRow{ID: jobID, Attempt: 1, MaxAttempts: outboundsend.MaxSendAttempts, Kind: outboundsend.OutboundSendArgs{}.Kind()},
		Args:   outboundsend.OutboundSendArgs{MessageID: messageID, OperationRef: &ref},
	}
	if err := w.Work(f.ctx, rj); err == nil {
		t.Fatal("an acceptance-unknown failure must return a retryable error")
	}
	// The production submitter redeems before it dials; the fake did not, so
	// redeem the token it was handed to reproduce "dialed, answer lost".
	if len(dl.auths) != 1 {
		t.Fatalf("auths = %d, want the one the worker handed over", len(dl.auths))
	}
	if err := f.gate.RedeemProviderCall(f.ctx, dl.auths[0]); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	// SES feedback proved acceptance; River gave up on the job.
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE messages SET provider_accepted_at = now(), provider_message_id = '<ses-evidence-000000@us-east-2.amazonses.com>'
		 WHERE id = $1`, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE river_job SET state = 'discarded', finalized_at = now() - interval '16 minutes' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}

	if err := outboundsend.NewTerminalReconcileWorker(f.pool, f.adapter).WithGate(f.gate).Work(f.ctx, &river.Job[outboundsend.TerminalReconcileArgs]{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var status string
	if err := f.pool.QueryRow(f.ctx, `SELECT delivery_status FROM messages WHERE id = $1`, messageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("delivery_status = %s, want sent from evidence", status)
	}
	var bound *string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT provider_message_id FROM sending_feedback_correlations
		 WHERE operation_id = $1 AND submission_attempt = 1`, messageID).Scan(&bound); err != nil {
		t.Fatalf("read correlation: %v", err)
	}
	if bound == nil || *bound != "ses-evidence-000000" {
		t.Fatalf("correlation provider id = %v, want the bare evidence id bound to the dialed attempt", bound)
	}
	if dl.calls != 1 {
		t.Fatalf("provider calls = %d, want the original one only", dl.calls)
	}
}

func TestJobs_RateDeferralReleasesTheRealReservation(t *testing.T) {
	f := newGateFixture(t)
	messageID, jobID, err := f.accept(f.gated, "rate")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	gate := &fakeRateGate{decision: outboundsend.RateDecision{Allowed: false, RetryAt: time.Now().Add(30 * time.Second)}, window: time.Minute}
	dl := &fakeDeliverer{}
	w := outboundsend.NewSendWorker(f.adapter, dl).WithGate(f.gate).WithRateGate(gate)
	ref := refFor(messageID)
	rj := &river.Job[outboundsend.OutboundSendArgs]{
		JobRow: &rivertype.JobRow{ID: jobID, Attempt: 1, MaxAttempts: outboundsend.MaxSendAttempts, Kind: outboundsend.OutboundSendArgs{}.Kind()},
		Args:   outboundsend.OutboundSendArgs{MessageID: messageID, OperationRef: &ref},
	}
	if err := w.Work(f.ctx, rj); !isSnooze(err) || dl.calls != 0 {
		t.Fatalf("err=%v delivers=%d, want snooze with no I/O", err, dl.calls)
	}
	var state string
	if err := f.pool.QueryRow(f.ctx, `SELECT state FROM sending_budget_reservations WHERE operation_id = $1 AND submission_attempt = 1`, messageID).Scan(&state); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	if state != "released" {
		t.Fatalf("reservation state = %s, want released — the deferral must give the budget back", state)
	}
}

// zeroRefGate accepts but prepares nothing — the shape only an exact
// self-send produces, which never enqueues.
type zeroRefGate struct{ *fakeGate }

func (zeroRefGate) PrepareExternalTx(context.Context, pgx.Tx, string) (sendingpolicy.AcceptanceDecision, sendingpolicy.OperationRef, error) {
	return sendingpolicy.AcceptanceAccept, sendingpolicy.OperationRef{}, nil
}

func TestJobs_EnqueueRefusesAnAcceptWithoutAnOperation(t *testing.T) {
	f := newGateFixture(t)
	bundle := outboundsend.NewJobs(f.adapter, &fakeDeliverer{}, f.pool).WithGate(zeroRefGate{allowAll()})
	bundle.SetEnqueuer(f.client)
	messageID, _, err := f.accept(bundle, "zero-ref")
	if err == nil {
		t.Fatal("an accept that prepared no operation was enqueued as a legacy-looking job")
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM messages WHERE id = $1`, messageID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("the refused accept left a message row behind")
	}
}
