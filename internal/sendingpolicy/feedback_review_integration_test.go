package sendingpolicy_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/migrations"
)

// recordFeedbackMetrics installs a process-wide observer for the test and
// returns the samples it saw. Tests using it must not run in parallel.
func recordFeedbackMetrics(t *testing.T) func() map[string]int {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]int{}
	sendingpolicy.SetFeedbackObserver(func(outcome, bucket string) {
		mu.Lock()
		defer mu.Unlock()
		seen[outcome+"/"+bucket]++
	})
	t.Cleanup(func() { sendingpolicy.SetFeedbackObserver(nil) })
	return func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(seen))
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

func (f *fixture) correlationExpiry(correlationID string) *time.Time {
	f.t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT expires_at FROM sending_feedback_correlations WHERE correlation_id = $1`, correlationID).Scan(&at); err != nil {
		f.t.Fatalf("correlation expiry: %v", err)
	}
	return at
}

func (f *fixture) eventExpiry(eventID string) *time.Time {
	f.t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT expires_at FROM sending_feedback_events WHERE provider_event_id = $1`, eventID).Scan(&at); err != nil {
		f.t.Fatalf("event expiry: %v", err)
	}
	return at
}

// provenanceRows counts the three B8 tables' rows for one correlation.
func (f *fixture) provenanceRows(correlationID string) (correlations, recipients, events int) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, `
		SELECT (SELECT count(*) FROM sending_feedback_correlations WHERE correlation_id = $1),
		       (SELECT count(*) FROM sending_feedback_recipients WHERE correlation_id = $1),
		       (SELECT count(*) FROM sending_feedback_events WHERE correlation_id = $1)`, correlationID,
	).Scan(&correlations, &recipients, &events); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *fixture) waitForLockWaiter() {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := f.pool.QueryRow(f.ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			f.t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatal("the concurrent feedback transaction never blocked on the seal")
}

// TestFeedbackRacingThePurgeSealInheritsTheSealExpiry is the NULL-expiry
// race: the purge seal stamps the account's correlations and events, deletes
// the user, and is still open when a notification for that account arrives.
// The notification must end up with the seal's expiry on its event row (not
// NULL, which no janitor would ever remove), and the GC past the horizon
// must empty all three tables.
func TestFeedbackRacingThePurgeSealInheritsTheSealExpiry(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	msg := f.messageTo(agent, "relay", []string{"race@example.test"})
	_, corrID, sesID := f.authorizedSend(g, msg, []string{"race@example.test"})

	// The seal's shape: stamp correlations, stamp events, delete the user —
	// and hold the transaction open.
	seal, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seal.Rollback(f.ctx) }()
	horizon := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Microsecond)
	if _, err := seal.Exec(f.ctx, `UPDATE sending_feedback_correlations SET expires_at = $2 WHERE source_account_ref = $1 AND expires_at IS NULL`, user, horizon); err != nil {
		t.Fatal(err)
	}
	if _, err := seal.Exec(f.ctx, `
		UPDATE sending_feedback_events e SET expires_at = c.expires_at
		  FROM sending_feedback_correlations c
		 WHERE c.correlation_id = e.correlation_id AND c.source_account_ref = $1 AND e.expires_at IS NULL`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := seal.Exec(f.ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		res delivery.FeedbackResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := module.ProcessProviderFeedback(context.Background(),
			feedback("race-evt", time.Now().UTC(), delivery.KindComplaint, sesID, "", "race@example.test"))
		done <- outcome{res, err}
	}()
	f.waitForLockWaiter()
	if err := seal.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	var got outcome
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("feedback never finished after the seal committed")
	}
	if got.err != nil {
		t.Fatalf("feedback: %v", got.err)
	}
	if !got.res.Correlated || got.res.AccountRef != "" {
		t.Fatalf("result = %+v, want correlated provenance-only", got.res)
	}

	corrExp, evtExp := f.correlationExpiry(corrID), f.eventExpiry("race-evt")
	if corrExp == nil || evtExp == nil || !evtExp.Equal(*corrExp) {
		t.Fatalf("event expiry = %v, want the correlation's %v", evtExp, corrExp)
	}
	if _, err := module.GCFeedback(f.ctx, corrExp.Add(time.Minute), 7); err != nil {
		t.Fatal(err)
	}
	if c, r, e := f.provenanceRows(corrID); c+r+e != 0 {
		t.Fatalf("after the horizon: correlations=%d recipients=%d events=%d, want none", c, r, e)
	}
}

// TestGCRemovesEventsByCorrelationEvenWithoutTheirOwnExpiry: an event row
// that a pre-fix race left with a NULL expiry still goes when its
// correlation does, and the reconcile sweep removes an event whose
// correlation is already gone.
func TestGCRemovesEventsByCorrelationEvenWithoutTheirOwnExpiry(t *testing.T) {
	f := newFixture(t)
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	f.exec(`
		INSERT INTO sending_feedback_correlations
		    (correlation_id, operation_id, submission_attempt, source_account_ref, policy_subject_ref, purpose, shared_reputation, tenant_mode, expires_at)
		VALUES ('cor_nullevt', 'op_nullevt', 1, 'usr_gone_1', 'usr_gone_1', 'customer_message', true, 'none', now() - interval '1 minute')`)
	f.exec(`INSERT INTO sending_feedback_recipients (correlation_id, recipient_hmac, hmac_key_version) VALUES ('cor_nullevt', '\xaa', 1)`)
	f.exec(`INSERT INTO sending_feedback_events (provider_event_id, correlation_id, provider_occurred_at, expires_at)
	        VALUES ('evt_null_1', 'cor_nullevt', now(), NULL),
	               ('evt_orphan_1', 'cor_never_existed', now(), NULL)`)

	st, err := module.GCFeedback(f.ctx, time.Now(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if st.Correlations != 1 || st.Recipients != 1 || st.Events != 1 {
		t.Fatalf("gc stats = %+v, want the correlation with its recipient and its NULL-expiry event", st)
	}
	if c, r, e := f.provenanceRows("cor_nullevt"); c+r+e != 0 {
		t.Fatalf("left behind: correlations=%d recipients=%d events=%d", c, r, e)
	}

	rs, err := module.ReconcileFeedbackRetention(f.ctx, time.Now(), 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rs.OrphanEvents != 1 {
		t.Fatalf("reconcile = %+v, want the one orphaned event swept", rs)
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sending_feedback_events WHERE provider_event_id = 'evt_orphan_1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("orphaned event survived the sweep")
	}
}

// seedOrphanProvenance lays down the backfill cases: a customer correlation
// of an account erased before B8 (and its NULL-expiry event), a live
// account's, a trashed account's, and a non-customer correlation that
// already carries its own horizon.
func (f *fixture) seedOrphanProvenance() (live, trashed string) {
	f.t.Helper()
	live, trashed = f.user("standard"), f.user("standard")
	f.trash(trashed)
	f.exec(`
		INSERT INTO sending_feedback_correlations
		    (correlation_id, operation_id, submission_attempt, source_account_ref, policy_subject_ref, purpose, shared_reputation, tenant_mode, expires_at)
		VALUES ('cor_bf_erased', 'op_bf_1', 1, 'usr_erased_before_b8', 'usr_erased_before_b8', 'customer_message', true, 'none', NULL),
		       ('cor_bf_notice', 'op_bf_2', 1, 'usr_erased_before_b8', 'usr_erased_before_b8', 'customer_notification', true, 'none', NULL),
		       ('cor_bf_live',   'op_bf_3', 1, $1, $1, 'customer_message', true, 'none', NULL),
		       ('cor_bf_trash',  'op_bf_4', 1, $2, $2, 'customer_message', true, 'none', NULL),
		       ('cor_bf_system', 'op_bf_5', 1, NULL, 'system', 'critical_operational', true, 'none', now() + interval '3 days')`,
		live, trashed)
	f.exec(`INSERT INTO sending_feedback_events (provider_event_id, correlation_id, provider_occurred_at, expires_at)
	        VALUES ('evt_bf_erased', 'cor_bf_erased', now(), NULL),
	               ('evt_bf_live',   'cor_bf_live',   now(), NULL)`)
	return live, trashed
}

func (f *fixture) assertBackfilled(label string) {
	f.t.Helper()
	lo, hi := time.Now().Add(29*24*time.Hour), time.Now().Add(31*24*time.Hour)
	for _, id := range []string{"cor_bf_erased", "cor_bf_notice"} {
		at := f.correlationExpiry(id)
		if at == nil || at.Before(lo) || at.After(hi) {
			f.t.Fatalf("%s: %s expiry = %v, want ~30 days out", label, id, at)
		}
	}
	if at, want := f.eventExpiry("evt_bf_erased"), f.correlationExpiry("cor_bf_erased"); at == nil || !at.Equal(*want) {
		f.t.Fatalf("%s: erased account's event expiry = %v, want the correlation's %v", label, at, want)
	}
	for _, id := range []string{"cor_bf_live", "cor_bf_trash"} {
		if at := f.correlationExpiry(id); at != nil {
			f.t.Fatalf("%s: %s (account still has a users row) was stamped: %v", label, id, at)
		}
	}
	if at := f.eventExpiry("evt_bf_live"); at != nil {
		f.t.Fatalf("%s: a live account's event was stamped: %v", label, at)
	}
	if at := f.correlationExpiry("cor_bf_system"); at == nil || at.After(time.Now().Add(4*24*time.Hour)) {
		f.t.Fatalf("%s: a non-customer correlation's own horizon was rewritten: %v", label, at)
	}
}

// TestMigration124BackfillsOrphanedCustomerProvenance: the one-time backlog
// pass stamps exactly the rows of accounts that no longer exist, and is
// idempotent.
func TestMigration124BackfillsOrphanedCustomerProvenance(t *testing.T) {
	f := newFixture(t)
	f.seedOrphanProvenance()
	sql, err := migrations.FS.ReadFile("124_sending_feedback_orphan_retention.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, string(sql)); err != nil {
		t.Fatalf("apply 124: %v", err)
	}
	f.assertBackfilled("first application")
	first := f.correlationExpiry("cor_bf_erased")

	if _, err := f.pool.Exec(f.ctx, string(sql)); err != nil {
		t.Fatalf("re-apply 124: %v", err)
	}
	f.assertBackfilled("second application")
	if again := f.correlationExpiry("cor_bf_erased"); !again.Equal(*first) {
		t.Fatalf("re-applying moved an already-stamped expiry: %v -> %v", first, again)
	}
}

// TestReconcileJobStampsOrphanedProvenanceFromTheEffectivePolicy: the daily
// backstop runs the same predicate as migration 124, with the horizon read
// from the effective policy.
func TestReconcileJobStampsOrphanedProvenanceFromTheEffectivePolicy(t *testing.T) {
	f := newFixture(t)
	f.seedOrphanProvenance()
	short := sendingpolicy.DisabledPolicy()
	short.SendingFeedbackPostAcctRetention = 5
	// Database source: the singleton's retention (the 30-day default), not
	// the config's 5, is what the job must stamp.
	module := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceDatabase, short)
	if err := sendingpolicy.NewFeedbackReconcileWorker(module).Work(f.ctx, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.assertBackfilled("reconcile")
}

// TestLateComplaintAfterRealAccountEraseIsProvenanceOnly drives the REAL
// deletion path (identity.EraseAccount → purge seal) instead of a raw
// DELETE FROM users: the seal stamps the horizon from the effective policy,
// a late complaint then lands as provenance only — its event carrying the
// correlation's expiry, no aggregate row, no customer state — and the GC
// past the horizon removes all of it.
func TestLateComplaintAfterRealAccountEraseIsProvenanceOnly(t *testing.T) {
	f := newFixture(t)
	metrics := recordFeedbackMetrics(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	msg := f.messageTo(agent, "relay", []string{"late@example.test"})
	_, corrID, sesID := f.authorizedSend(g, msg, []string{"late@example.test"})

	// The purge reads its horizon from a DATABASE-source module whose
	// config-file policy says 5 days, while the activated (effective) policy
	// says 45 — neither is the 30-day default, so the stamp proves which
	// source the seal read.
	short := sendingpolicy.DisabledPolicy()
	short.SendingFeedbackPostAcctRetention = 5
	dbSourced := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceDatabase, short)
	f.activateRetention(dbSourced, 45)
	store := identity.NewStore(f.pool)
	store.SetFeedbackRetentionResolver(dbSourced.EffectiveFeedbackRetention)
	if _, err := store.EraseAccount(f.ctx, user, nil); err != nil {
		t.Fatalf("EraseAccount: %v", err)
	}
	var users int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM users WHERE id = $1`, user).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Fatal("EraseAccount must purge the user row")
	}
	corrExp := f.correlationExpiry(corrID)
	if corrExp == nil || corrExp.Before(time.Now().Add(44*24*time.Hour)) || corrExp.After(time.Now().Add(46*24*time.Hour)) {
		t.Fatalf("seal stamped %v, want ~45 days (the effective policy), not the config's 5 or the default 30", corrExp)
	}

	res, err := module.ProcessProviderFeedback(f.ctx, feedback("late-evt", time.Now().UTC(), delivery.KindComplaint, sesID, "", "late@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Correlated || res.AccountRef != "" || len(res.RepairNeeded) != 0 {
		t.Fatalf("result = %+v, want provenance only", res)
	}
	if evtExp := f.eventExpiry("late-evt"); evtExp == nil || !evtExp.Equal(*corrExp) {
		t.Fatalf("event expiry = %v, want the correlation's %v", evtExp, corrExp)
	}
	if got := f.outcomes(user); len(got) != 0 {
		t.Fatalf("an erased account must have no aggregate rows: %v", got)
	}
	if b, _, epoch := f.recipientRow(corrID); b != "complaint" || epoch != nil {
		t.Fatalf("provenance = %s epoch=%v, want complaint with no epoch", b, epoch)
	}
	if got := metrics()["dead_account/complaint"]; got != 1 {
		t.Fatalf("metrics = %v, want one dead_account/complaint sample", metrics())
	}

	if _, err := module.GCFeedback(f.ctx, corrExp.Add(time.Minute), 7); err != nil {
		t.Fatal(err)
	}
	if c, r, e := f.provenanceRows(corrID); c+r+e != 0 {
		t.Fatalf("after the horizon: correlations=%d recipients=%d events=%d, want none", c, r, e)
	}
}

// activateRetention activates a database policy whose post-account
// feedback retention is `days`.
func (f *fixture) activateRetention(m *sendingpolicy.Module, days int) {
	f.t.Helper()
	before, err := m.InspectPolicy(f.ctx)
	if err != nil {
		f.t.Fatalf("inspect policy: %v", err)
	}
	next := before.Policy
	next.SendingFeedbackPostAcctRetention = days
	if _, err := m.ActivatePolicy(f.ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration: before.Generation, Policy: next,
		Actor: "integration-test", Reason: "non-default feedback retention",
	}); err != nil {
		f.t.Fatalf("activate policy: %v", err)
	}
}

// TestPurgeRetentionResolverErrorLeavesTheAccountUnpurged: a seal that
// cannot resolve its horizon must not stamp a guess — it fails, the user row
// survives for the purge's next pass, and nothing is stamped.
func TestPurgeRetentionResolverErrorLeavesTheAccountUnpurged(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	user := f.user("standard")
	agent := f.agent(user)
	msg := f.messageTo(agent, "relay", []string{"unresolved@example.test"})
	_, corrID, _ := f.authorizedSend(g, msg, []string{"unresolved@example.test"})

	store := identity.NewStore(f.pool)
	store.SetFeedbackRetentionResolver(func(context.Context) (time.Duration, error) {
		return 0, errors.New("policy store unavailable")
	})
	if _, err := store.EraseAccount(f.ctx, user, nil); err == nil {
		t.Fatal("EraseAccount must fail when the retention horizon cannot be resolved")
	}
	var users int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM users WHERE id = $1`, user).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Fatal("the user row must survive a seal that could not resolve its horizon")
	}
	if at := f.correlationExpiry(corrID); at != nil {
		t.Fatalf("nothing may be stamped without a resolved horizon, got %v", at)
	}
}

// TestLegacyRawFormHMACStillMatches: a recipient row signed before
// canonicalization (over the raw Unicode spelling, which differs from the
// canonical A-label form) is still matched through the raw-form fallback.
func TestLegacyRawFormHMACStillMatches(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	raw := "alt@bücher.example"
	msg := f.messageTo(agent, "relay", []string{raw})
	_, corrID, sesID := f.authorizedSend(g, msg, []string{raw})

	keyring, err := sendingpolicy.LoadKeyring(fxHMAC)
	if err != nil {
		t.Fatal(err)
	}
	version, legacy := keyring.Sign([]byte(raw))
	_, canonical := keyring.Sign([]byte("alt@xn--bcher-kva.example"))
	if string(legacy) == string(canonical) {
		t.Fatal("fixture must sign two distinct subjects")
	}
	f.exec(`UPDATE sending_feedback_recipients SET recipient_hmac = $2, hmac_key_version = $3 WHERE correlation_id = $1`, corrID, legacy, version)

	if _, err := module.ProcessProviderFeedback(f.ctx, feedback("legacy-1", time.Now().UTC(), delivery.KindDelivery, sesID, "", raw)); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := f.recipientRow(corrID); b != "delivered" {
		t.Fatalf("a legacy raw-form row must still match: bucket %s", b)
	}
}

// TestFeedbackMetricCarriesAppliedBucketAndOnlyAfterCommit: the bucket label
// is the one actually APPLIED (a simulator delivery is correlated/none, not
// delivered), and a pass whose transaction rolls back emits nothing.
func TestFeedbackMetricCarriesAppliedBucketAndOnlyAfterCommit(t *testing.T) {
	f := newFixture(t)
	metrics := recordFeedbackMetrics(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	sim := "bounce-check@simulator.amazonses.com"
	msg := f.messageTo(agent, "relay", []string{sim})
	_, _, sesID := f.authorizedSend(g, msg, []string{sim})
	if _, err := module.ProcessProviderFeedback(f.ctx, feedback("applied-1", time.Now().UTC(), delivery.KindDelivery, sesID, "", sim)); err != nil {
		t.Fatal(err)
	}
	if got := metrics(); len(got) != 1 || got["correlated/none"] != 1 {
		t.Fatalf("metrics = %v, want exactly one correlated/none sample", got)
	}

	// Rollback: the simulator recipient is processed first (a sample is
	// collected, no aggregate write); the ordinary one's aggregate insert
	// then fails, rolling the whole pass back.
	f.exec(`
		CREATE OR REPLACE FUNCTION fx_fail_outcome_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'fixture: aggregate write refused'; END $$`)
	f.exec(`CREATE TRIGGER fx_fail_outcome_insert BEFORE INSERT ON account_sending_outcomes_daily
	        FOR EACH ROW EXECUTE FUNCTION fx_fail_outcome_insert()`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS fx_fail_outcome_insert ON account_sending_outcomes_daily`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS fx_fail_outcome_insert()`)
	})
	both := []string{sim, "ordinary@example.test"}
	msg2 := f.messageTo(agent, "relay", both)
	_, _, ses2 := f.authorizedSend(g, msg2, both)
	before := metrics()
	if _, err := module.ProcessProviderFeedback(f.ctx, feedback("rollback-1", time.Now().UTC(), delivery.KindDelivery, ses2, "", both...)); err == nil {
		t.Fatal("the fixture trigger must fail the pass")
	}
	if after := metrics(); len(after) != len(before) || after["correlated/none"] != before["correlated/none"] {
		t.Fatalf("a rolled-back pass emitted samples: before=%v after=%v", before, after)
	}
}

// TestProvenanceRepairInALabelBlocksAUnicodeTypedSend: SES reports an IDN
// recipient in A-label form, so that is the spelling the repair stores; the
// send-time suppression lookup must still block the customer's next send to
// the Unicode spelling they typed.
func TestProvenanceRepairInALabelBlocksAUnicodeTypedSend(t *testing.T) {
	f := newFixture(t)
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	if _, err := module.RepairSuppressions(f.ctx, user, []delivery.FeedbackRepair{
		{Address: "leser@xn--bcher-kva.example", Source: "bounce", Reason: "bounce:General"},
	}); err != nil {
		t.Fatal(err)
	}
	store := identity.NewStore(f.pool)
	typed := []string{"Leser@Bücher.example"}
	eff, err := store.EffectiveSuppressions(f.ctx, user, agent, typed)
	if err != nil || len(eff) != 1 {
		t.Fatalf("EffectiveSuppressions(%v) = %v (err %v), want the A-label row", typed, eff, err)
	}
	acct, err := store.SuppressedAddresses(f.ctx, user, typed)
	if err != nil || len(acct) != 1 {
		t.Fatalf("SuppressedAddresses(%v) = %v (err %v), want the A-label row", typed, acct, err)
	}
	if other, err := store.EffectiveSuppressions(f.ctx, user, agent, []string{"leser@bucher.example"}); err != nil || len(other) != 0 {
		t.Fatalf("a different domain matched: %v (err %v)", other, err)
	}
}

// TestFeedbackIngestionMetrics pins the bounded outcome × bucket samples:
// correlated per matched recipient, unmatched_recipient per stranger,
// duplicate per replay, and the two uncorrelated flavours — the one
// carrying e2a's attempt marker being the alerting signal.
func TestFeedbackIngestionMetrics(t *testing.T) {
	f := newFixture(t)
	metrics := recordFeedbackMetrics(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	msg := f.messageTo(agent, "relay", []string{"m1@example.test"})
	_, _, sesID := f.authorizedSend(g, msg, []string{"m1@example.test"})
	at := time.Now().UTC()

	for _, fb := range []delivery.ProviderFeedback{
		feedback("mx-1", at, delivery.KindBounce, sesID, "", "m1@example.test", "stranger@example.test"),
		feedback("mx-1", at, delivery.KindBounce, sesID, "", "m1@example.test"),
		feedback("mx-2", at, delivery.KindDelivery, "<unknown-ses@example.test>", "cor_00aa11bb", "m1@example.test"),
		feedback("mx-3", at, delivery.KindDelivery, "<unknown-ses@example.test>", "", "m1@example.test"),
	} {
		if fb.Kind == delivery.KindBounce {
			fb.BounceType = "permanent"
		}
		if _, err := module.ProcessProviderFeedback(f.ctx, fb); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]int{
		"correlated/hard_bounce":             1,
		"unmatched_recipient/hard_bounce":    1,
		"duplicate/hard_bounce":              1,
		"uncorrelated_with_marker/delivered": 1,
		"uncorrelated/delivered":             1,
	}
	got := metrics()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected samples: %v", got)
	}
}

// TestSelfAddressedDeliveriesDoNotDiluteTheDenominator: deliveries to the
// SES simulator, the configured shared agent domain, a platform-owned shared
// domain row, and the sender's own verified domain are all mail a sender can
// manufacture; none may count toward the denominator. A delivery to an
// ordinary recipient counts, and a complaint from an excluded recipient is
// still evidence against the sender.
func TestSelfAddressedDeliveriesDoNotDiluteTheDenominator(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets()).WithFeedbackExcludedDomains("Agents.Localhost")
	user := f.user("standard")
	agent := f.agent(user)
	f.customDomain(user, "own.example.test", "verified")
	f.exec(`INSERT INTO domains (domain, user_id, verified, verified_at) VALUES ('platform-shared.example.test', NULL, true, now())`)
	other := f.user("standard")
	f.customDomain(other, "someone-else.example.test", "verified")

	excluded := []string{
		"success@simulator.amazonses.com",
		"peer@agents.localhost",
		"self@own.example.test",
		"peer@platform-shared.example.test",
	}
	counted := []string{"customer@example.test", "partner@someone-else.example.test"}
	all := append(append([]string{}, excluded...), counted...)
	msg := f.messageTo(agent, "relay", all)
	_, corrID, sesID := f.authorizedSend(g, msg, all)

	res, err := module.ProcessProviderFeedback(f.ctx, feedback("dil-1", time.Now().UTC(), delivery.KindDelivery, sesID, "", all...))
	if err != nil || !res.Correlated {
		t.Fatalf("delivery: %+v %v", res, err)
	}
	if c := f.outcomes(user)[todayKey(1, true)]; c != [4]int{2, 0, 0, 0} {
		t.Fatalf("aggregate = %v, want only the two ordinary recipients delivered", c)
	}
	var none int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sending_feedback_recipients WHERE correlation_id = $1 AND detector_bucket = 'none'`, corrID).Scan(&none); err != nil {
		t.Fatal(err)
	}
	if none != len(excluded) {
		t.Fatalf("recipients left in bucket none = %d, want %d", none, len(excluded))
	}

	// A soft (terminal_other) bounce from an excluded recipient is equally
	// denominator-only and equally excluded.
	soft := feedback("dil-2", time.Now().UTC(), delivery.KindBounce, sesID, "", "success@simulator.amazonses.com")
	soft.BounceType = "transient"
	if _, err := module.ProcessProviderFeedback(f.ctx, soft); err != nil {
		t.Fatal(err)
	}
	if c := f.outcomes(user)[todayKey(1, true)]; c != [4]int{2, 0, 0, 0} {
		t.Fatalf("aggregate after an excluded soft bounce = %v, want terminal_other still 0", c)
	}
	// Complaints and hard bounces from every excluded class still count.
	for i, addr := range excluded {
		kind := delivery.KindComplaint
		fb := feedback("dil-c-"+addr, time.Now().UTC(), kind, sesID, "", addr)
		if i%2 == 1 {
			fb.Kind, fb.BounceType = delivery.KindBounce, "permanent"
		}
		if _, err := module.ProcessProviderFeedback(f.ctx, fb); err != nil {
			t.Fatal(err)
		}
	}
	if c := f.outcomes(user)[todayKey(1, true)]; c != [4]int{2, 0, 2, 2} {
		t.Fatalf("aggregate = %v, want delivered 2, hard bounces 2, complaints 2", c)
	}
}

// TestInternationalizedRecipientDomainMatchesAcrossSpellings: authorization
// accepts an IDN recipient as typed (Unicode) while the provider may report
// it in A-label (punycode) form. Both sides HMAC the IDNA-ASCII canonical
// form, so feedback in either spelling matches the authorized envelope.
func TestInternationalizedRecipientDomainMatchesAcrossSpellings(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	unicode := "leser@bücher.example"
	msg := f.messageTo(agent, "relay", []string{unicode})
	_, corrID, sesID := f.authorizedSend(g, msg, []string{unicode})

	res, err := module.ProcessProviderFeedback(f.ctx, feedback("idn-1", time.Now().UTC(), delivery.KindDelivery, sesID, "", "leser@xn--bcher-kva.example"))
	if err != nil || !res.Correlated {
		t.Fatalf("punycode feedback: %+v %v", res, err)
	}
	if b, _, _ := f.recipientRow(corrID); b != "delivered" {
		t.Fatalf("punycode spelling did not match the Unicode authorization: bucket %s", b)
	}
	c := feedback("idn-2", time.Now().UTC(), delivery.KindComplaint, sesID, "", "LESER@BÜCHER.example")
	if _, err := module.ProcessProviderFeedback(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := f.recipientRow(corrID); b != "complaint" {
		t.Fatalf("Unicode spelling did not match: bucket %s", b)
	}
}

// TestRepairReportsOnlyInsertedRows: the provenance repair refreshes an
// existing suppression (manual or bounce) but reports only rows it actually
// inserted, which is what the consumer announces.
func TestRepairReportsOnlyInsertedRows(t *testing.T) {
	f := newFixture(t)
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	f.exec(`INSERT INTO suppressions (id, user_id, address, reason, source) VALUES ('supp_manual_fx', $1, 'manual@example.test', 'customer request', 'manual')`, user)
	inserted, err := module.RepairSuppressions(f.ctx, user, []delivery.FeedbackRepair{
		{Address: "manual@example.test", Source: "bounce", Reason: "bounce:General"},
		{Address: "fresh@example.test", Source: "bounce", Reason: "bounce:General"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 1 || inserted[0].Address != "fresh@example.test" {
		t.Fatalf("inserted = %+v, want only the fresh address", inserted)
	}
	var source string
	if err := f.pool.QueryRow(f.ctx, `SELECT source FROM suppressions WHERE user_id = $1 AND address = 'manual@example.test'`, user).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "manual" {
		t.Fatalf("the manual row was rewritten to %q", source)
	}
	again, err := module.RepairSuppressions(f.ctx, user, []delivery.FeedbackRepair{{Address: "fresh@example.test", Source: "bounce", Reason: "bounce:General"}})
	if err != nil || len(again) != 0 {
		t.Fatalf("a repeated repair reported %+v (err %v), want nothing", again, err)
	}
}

// TestExpiredCorrelationIsPastRetentionNotLost: a correlation past its
// horizon is never matched (the janitor may be deleting it), and feedback
// carrying its marker counts as plain uncorrelated — retention working —
// not as the lost-correlation alert.
func TestExpiredCorrelationIsPastRetentionNotLost(t *testing.T) {
	f := newFixture(t)
	metrics := recordFeedbackMetrics(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	msg := f.messageTo(agent, "relay", []string{"aged@example.test"})
	_, corrID, sesID := f.authorizedSend(g, msg, []string{"aged@example.test"})
	f.exec(`UPDATE sending_feedback_correlations SET expires_at = now() - interval '1 minute' WHERE correlation_id = $1`, corrID)

	res, err := module.ProcessProviderFeedback(f.ctx, feedback("aged-1", time.Now().UTC(), delivery.KindComplaint, sesID, corrID, "aged@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Correlated {
		t.Fatal("an expired correlation must not be matched")
	}
	if got := metrics(); got["uncorrelated/complaint"] != 1 || got["uncorrelated_with_marker/complaint"] != 0 {
		t.Fatalf("metrics = %v, want uncorrelated (past retention), not the alert", got)
	}
}
