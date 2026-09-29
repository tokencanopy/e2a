package sendingpolicy_test

import (
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/migrations"
)

// Synthetic traffic from system and internal accounts (the standing prober,
// monitors, conformance) keeps its feedback provenance for 30 days, not for
// the account's lifetime: those accounts are never deleted, so the lifetime
// rule would retain every prober correlation forever. Standard and demo
// accounts keep the account-lifetime rule.

// TestSyntheticAccountCorrelationsGetThirtyDayHorizon drives real authorized
// sends for each class and then the B8 janitor past the horizon.
func TestSyntheticAccountCorrelationsGetThirtyDayHorizon(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	correlation := map[string]string{}
	for _, class := range []string{"system", "internal", "standard", "demo"} {
		agent := f.agent(f.user(class))
		rcpts := []string{class + "-rcpt@example.test"}
		msg := f.messageTo(agent, "relay", rcpts)
		_, corrID, _ := f.authorizedSend(g, msg, rcpts)
		correlation[class] = corrID
	}

	for class, corrID := range correlation {
		var created time.Time
		var expires *time.Time
		if err := f.pool.QueryRow(f.ctx,
			`SELECT created_at, expires_at FROM sending_feedback_correlations WHERE correlation_id = $1`, corrID,
		).Scan(&created, &expires); err != nil {
			t.Fatal(err)
		}
		synthetic := class == "system" || class == "internal"
		if !synthetic {
			if expires != nil {
				t.Errorf("%s-class correlation got expiry %v, want NULL (account lifetime)", class, expires)
			}
			continue
		}
		if expires == nil {
			t.Fatalf("%s-class correlation has no expiry; synthetic traffic must expire after 30 days", class)
		}
		if d := expires.Sub(created); d < 30*24*time.Hour-time.Minute || d > 30*24*time.Hour+time.Minute {
			t.Errorf("%s-class correlation horizon = %v, want 30 days after creation", class, d)
		}
	}

	// Past the horizon, B8's existing janitor removes the synthetic rows and
	// their recipient provenance; the customer rows stay.
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	if _, err := module.GCFeedback(f.ctx, time.Now().Add(31*24*time.Hour), 7); err != nil {
		t.Fatal(err)
	}
	for class, corrID := range correlation {
		corr, recipients, _ := f.provenanceRows(corrID)
		wantGone := class == "system" || class == "internal"
		if gone := corr == 0 && recipients == 0; gone != wantGone {
			t.Errorf("%s-class provenance after 31 days: correlations=%d recipients=%d, want gone=%v", class, corr, recipients, wantGone)
		}
	}
}

// TestMigration129BackfillsOnlySyntheticAccounts: the one-time pass stamps
// created_at + 30 days on existing system/internal correlations (and their
// events) and nothing else, and is idempotent.
func TestMigration129BackfillsOnlySyntheticAccounts(t *testing.T) {
	f := newFixture(t)
	users := map[string]string{}
	for _, class := range []string{"system", "internal", "standard", "demo"} {
		users[class] = f.user(class)
	}
	seed := func(id, account, purpose string, expires any) {
		f.exec(`INSERT INTO sending_feedback_correlations
		            (correlation_id, operation_id, submission_attempt, source_account_ref, policy_subject_ref,
		             purpose, tenant_mode, created_at, expires_at)
		        VALUES ($1, 'op_' || $1, 1, $2, COALESCE($2, 'system'), $3, 'none', now() - interval '10 days', $4)`,
			id, account, purpose, expires)
		f.exec(`INSERT INTO sending_feedback_events (provider_event_id, correlation_id, provider_occurred_at)
		        VALUES ('evt_' || $1, $1, now())`, id)
	}
	seed("cor_bf129_system", users["system"], "customer_message", nil)
	seed("cor_bf129_internal", users["internal"], "customer_notification", nil)
	seed("cor_bf129_standard", users["standard"], "customer_message", nil)
	seed("cor_bf129_demo", users["demo"], "customer_message", nil)
	stamped := time.Now().UTC().Add(5 * 24 * time.Hour).Truncate(time.Second)
	seed("cor_bf129_system_stamped", users["system"], "customer_message", stamped)

	sql, err := migrations.FS.ReadFile("129_sending_feedback_synthetic_retention.sql")
	if err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ {
		if _, err := f.pool.Exec(f.ctx, string(sql)); err != nil {
			t.Fatalf("apply 129 (pass %d): %v", pass, err)
		}
		for id, want := range map[string]bool{
			"cor_bf129_system": true, "cor_bf129_internal": true,
			"cor_bf129_standard": false, "cor_bf129_demo": false,
		} {
			var created time.Time
			var expires, eventExpires *time.Time
			if err := f.pool.QueryRow(f.ctx, `
				SELECT c.created_at, c.expires_at, e.expires_at
				  FROM sending_feedback_correlations c
				  JOIN sending_feedback_events e ON e.correlation_id = c.correlation_id
				 WHERE c.correlation_id = $1`, id).Scan(&created, &expires, &eventExpires); err != nil {
				t.Fatal(err)
			}
			if !want {
				if expires != nil || eventExpires != nil {
					t.Errorf("pass %d: %s was stamped (%v / %v); only system/internal rows may be", pass, id, expires, eventExpires)
				}
				continue
			}
			if expires == nil || !expires.Equal(created.Add(30*24*time.Hour)) {
				t.Errorf("pass %d: %s expiry = %v, want created_at + 30 days", pass, id, expires)
			}
			if eventExpires == nil || !eventExpires.Equal(*expires) {
				t.Errorf("pass %d: %s event expiry = %v, want the correlation's %v", pass, id, eventExpires, expires)
			}
		}
		if got := f.correlationExpiry("cor_bf129_system_stamped"); got == nil || !got.Equal(stamped) {
			t.Errorf("pass %d: an already-stamped expiry was rewritten: %v", pass, got)
		}
	}
}
