package sendingpolicy_test

import (
	"context"
	"testing"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// TestFeedbackMaintenanceWorkerHonorsTheEffectiveWindow is the guard on the
// one component in this slice that DELETES evidence.
//
// A database-source deployment reads its detector window from the audited
// policy row, not from the config file. An operator who widens the window
// there and gets a janitor still cutting at the config width would lose the
// daily outcomes the detector is summing — an abuser's bad days would age
// out early, and nothing would say so.
func TestFeedbackMaintenanceWorkerHonorsTheEffectiveWindow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Config says 30 days; the database singleton (generation zero) says 7.
	wide := sendingpolicy.DisabledPolicy()
	wide.DetectorWindowDays = 30
	dbSourced := sendingpolicy.NewGate(f.pool, f.secrets(), sendingpolicy.PolicySourceDatabase, wide).(*sendingpolicy.Module)
	configSourced := sendingpolicy.NewGate(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, wide).(*sendingpolicy.Module)

	got, err := dbSourced.EffectiveDetectorWindowDays(ctx)
	if err != nil {
		t.Fatalf("effective window: %v", err)
	}
	if got != 7 {
		t.Fatalf("database-source window = %d, want the policy row's 7 (not the config's 30)", got)
	}
	if got, err := configSourced.EffectiveDetectorWindowDays(ctx); err != nil || got != 30 {
		t.Fatalf("config-source window = %d (err %v), want 30", got, err)
	}

	// The worker must cut at the window it just read. Seed one aggregate
	// just inside the database window and one outside it.
	user := f.user("standard")
	for _, age := range []int{6, 9} {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO account_sending_outcomes_daily (user_id, outcome_epoch, day, shared_reputation, delivered_count)
			 VALUES ($1, 1, current_date - $2::int, true, 1)`, user, age); err != nil {
			t.Fatal(err)
		}
	}
	if err := sendingpolicy.NewFeedbackMaintenanceWorker(dbSourced).Work(ctx, &river.Job[sendingpolicy.FeedbackMaintenanceArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	var days []int
	rows, err := f.pool.Query(ctx, `SELECT current_date - day FROM account_sending_outcomes_daily WHERE user_id = $1 ORDER BY 1`, user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d int
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		days = append(days, d)
	}
	if len(days) != 1 || days[0] != 6 {
		t.Fatalf("remaining aggregate ages = %v, want only the 6-day-old row (window 7 + 1 day of safety)", days)
	}
}

// TestFeedbackMaintenanceWorkerRemovesExpiredProvenance: the worker is the
// only thing that honors the post-deletion horizon, so it has to actually
// remove what account deletion stamped.
func TestFeedbackMaintenanceWorkerRemovesExpiredProvenance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	module := sendingpolicy.NewModule(f.pool, f.secrets())

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO sending_feedback_correlations
		    (correlation_id, operation_id, submission_attempt, policy_subject_ref, purpose, shared_reputation, tenant_mode, expires_at)
		VALUES ('cor_gc_expired', 'op_gc_1', 1, 'usr_gone', 'customer_message', true, 'none', now() - interval '1 hour'),
		       ('cor_gc_live',    'op_gc_2', 1, 'usr_here', 'customer_message', true, 'none', NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO sending_feedback_recipients (correlation_id, recipient_hmac, hmac_key_version)
		VALUES ('cor_gc_expired', '\xaa', 1), ('cor_gc_live', '\xbb', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO sending_feedback_events (provider_event_id, correlation_id, provider_occurred_at, expires_at)
		VALUES ('evt_gc_expired', 'cor_gc_expired', now(), now() - interval '1 hour'),
		       ('evt_gc_live',    'cor_gc_live',    now(), NULL)`); err != nil {
		t.Fatal(err)
	}

	if err := sendingpolicy.NewFeedbackMaintenanceWorker(module).Work(ctx, &river.Job[sendingpolicy.FeedbackMaintenanceArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	for table, column := range map[string]string{
		"sending_feedback_correlations": "correlation_id",
		"sending_feedback_recipients":   "correlation_id",
		"sending_feedback_events":       "provider_event_id",
	} {
		var expired, live int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE `+column+` LIKE '%expired'), count(*) FILTER (WHERE `+column+` LIKE '%live')
			   FROM `+table+` WHERE `+column+` LIKE '%gc%'`).Scan(&expired, &live); err != nil {
			t.Fatal(err)
		}
		if expired != 0 {
			t.Errorf("%s: %d expired row(s) survived the janitor", table, expired)
		}
		if live != 1 {
			t.Errorf("%s: retained row was deleted (live=%d)", table, live)
		}
	}
}

// TestFeedbackMaintenanceRegistersOnTheMaintenanceQueue: the periodic has
// to reach River on the low-urgency queue, or the retention horizon is
// never enforced at all.
func TestFeedbackMaintenanceRegistersOnTheMaintenanceQueue(t *testing.T) {
	f := newFixture(t)
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	periodics := sendingpolicy.NewMaintenanceJobs(module).RegisterJobs(river.NewWorkers())
	if len(periodics) != 1 {
		t.Fatalf("periodic jobs = %d, want 1", len(periodics))
	}
	// River keeps the periodic's constructor unexported, so assert the two
	// facts that are observable and load-bearing: the job kind the worker
	// is registered under, and that the registrar hands River a real
	// client-buildable set. A registrar that returned no periodic, or a
	// kind rename that left the worker unreachable, both fail here.
	if kind := (sendingpolicy.FeedbackMaintenanceArgs{}).Kind(); kind != "sending_feedback_maintenance" {
		t.Fatalf("periodic kind = %q", kind)
	}
	if _, err := jobs.New(f.pool, jobs.Config{}, sendingpolicy.NewMaintenanceJobs(module)); err != nil {
		t.Fatalf("the registrar must produce a buildable River client: %v", err)
	}
}
