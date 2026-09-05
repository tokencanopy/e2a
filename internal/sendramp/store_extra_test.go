package sendramp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/sendramp"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func TestPermanentErrorErrorReturnsWrappedMessage(t *testing.T) {
	err := &sendramp.PermanentError{Err: errors.New("boom")}
	if err.Error() != "boom" {
		t.Fatalf("Error() = %q, want %q", err.Error(), "boom")
	}
	var permanent permanentMarker = err
	if !permanent.Permanent() {
		t.Fatal("PermanentError must report Permanent() = true")
	}
}

func TestExemptRejectsDomainOwnedByAnotherUser(t *testing.T) {
	store, pool, _, domain, _ := seedRampMessage(t, "exempt-foreign")
	other, err := identity.NewStore(pool).CreateOrGetUser(context.Background(), "exempt-other@example.com", "Other", "exempt-other")
	if err != nil {
		t.Fatal(err)
	}
	err = store.Exempt(context.Background(), other.ID, domain)
	var permanent permanentMarker
	if !errors.As(err, &permanent) || !permanent.Permanent() {
		t.Fatalf("Exempt by non-owner err=%v, want permanent owner-mismatch error", err)
	}
}

func TestExemptNoopsWhenDomainCannotTransition(t *testing.T) {
	ctx := context.Background()

	t.Run("unverified domain stays inactive", func(t *testing.T) {
		pool := testutil.TestDB(t)
		store := sendramp.NewStore(pool)
		ids := identity.NewStore(pool)
		user, err := ids.CreateOrGetUser(ctx, "exempt-unverified@example.com", "Unverified", "exempt-unverified")
		if err != nil {
			t.Fatal(err)
		}
		domain := "exempt-unverified.example.com"
		if _, err := ids.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.Exempt(ctx, user.ID, domain); err != nil {
			t.Fatalf("Exempt on unverified domain: %v", err)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT sending_ramp_status FROM domains WHERE domain=$1`, domain).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != sendramp.StatusInactive {
			t.Fatalf("status=%q, want %q", status, sendramp.StatusInactive)
		}
	})

	t.Run("ramping domain stays ramping", func(t *testing.T) {
		store, pool, userID, domain, messageID := seedRampMessage(t, "exempt-ramping")
		day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
		if got := reserve(t, store, userID, domain, messageID, 1, day, sendramp.DefaultSchedule); !got.Allowed {
			t.Fatalf("reserve = %+v, want allowed", got)
		}
		if err := store.Exempt(ctx, userID, domain); err != nil {
			t.Fatalf("Exempt on ramping domain: %v", err)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT sending_ramp_status FROM domains WHERE domain=$1`, domain).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != sendramp.StatusRamping {
			t.Fatalf("status=%q, want %q", status, sendramp.StatusRamping)
		}
	})
}

func TestSnapshotErrorsForUnknownDomain(t *testing.T) {
	store, _, userID, _, _ := seedRampMessage(t, "snapshot-missing")
	if _, err := store.Snapshot(context.Background(), userID, "missing.example.com", time.Now()); err == nil {
		t.Fatal("Snapshot on unknown domain must return an error")
	}
}

func TestSnapshotReturnsStatusOnlyForNonRampingDomain(t *testing.T) {
	store, _, userID, domain, _ := seedRampMessage(t, "snapshot-inactive")
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	inactive, err := store.Snapshot(context.Background(), userID, domain, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if inactive.Status != sendramp.StatusInactive || inactive.DailyLimit != 0 || inactive.UsedToday != 0 || inactive.StartedAt != nil {
		t.Fatalf("inactive snapshot = %+v, want status-only", inactive)
	}

	if err := store.Exempt(context.Background(), userID, domain); err != nil {
		t.Fatalf("Exempt: %v", err)
	}
	exempt, err := store.Snapshot(context.Background(), userID, domain, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if exempt.Status != sendramp.StatusExempt || exempt.DailyLimit != 0 || exempt.StartedAt != nil {
		t.Fatalf("exempt snapshot = %+v, want status-only exempt", exempt)
	}
}

func TestSnapshotDerivesLimitFromScheduleWhenNoCounterRow(t *testing.T) {
	store, pool, userID, domain, _ := seedRampMessage(t, "snapshot-no-counter")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE domains SET sending_ramp_status='ramping' WHERE domain=$1`, domain); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sending_ramp_scopes (user_id,domain,start_daily,target_daily,ramp_days) VALUES ($1,'example.com',50,200,4)`, userID); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Snapshot(ctx, userID, domain, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Status != sendramp.StatusRamping || snap.StartedAt == nil || snap.CompletedAt != nil ||
		snap.ActiveDays != 0 || snap.StartDaily != 50 || snap.TargetDaily != 200 || snap.RampDays != 4 ||
		snap.DailyLimit != 50 || snap.UsedToday != 0 {
		t.Fatalf("snapshot = %+v, want ramping day-zero limit 50 unused", snap)
	}
}

func TestSnapshotReportsCompleteWhenScopeCompleted(t *testing.T) {
	store, pool, userID, domain, _ := seedRampMessage(t, "snapshot-complete")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE domains SET sending_ramp_status='ramping' WHERE domain=$1`, domain); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sending_ramp_scopes (user_id,domain,status,active_days,start_daily,target_daily,ramp_days) VALUES ($1,'example.com','complete',4,50,200,4)`, userID); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Snapshot(ctx, userID, domain, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Status != sendramp.StatusComplete || snap.TargetDaily != 200 || snap.ActiveDays != 4 {
		t.Fatalf("snapshot = %+v, want completed scope snapshot", snap)
	}
}

// TestReserveHoldsAnUnverifiedDomainWithoutAccounting pins the refusal that
// replaced the old pass-through.
//
// A message's composed From and its reputation class are frozen at acceptance;
// the agent's registered domain is not. Verifying a child subdomain rebinds an
// account's agents onto it, that child's SES identity stays unverified while
// its DKIM records are never published, and this reserve resolves the domain
// live — so passing through handed an accepted backlog an uncapped day under a
// frozen custom-domain identity. The refusal must still write NOTHING: an
// unverified identity has no scope to arm and no counter to charge.
func TestReserveHoldsAnUnverifiedDomainWithoutAccounting(t *testing.T) {
	pool := testutil.TestDB(t)
	store := sendramp.NewStore(pool)
	ids := identity.NewStore(pool)
	ctx := context.Background()
	user, err := ids.CreateOrGetUser(ctx, "reserve-unverified@example.com", "Unverified", "reserve-unverified")
	if err != nil {
		t.Fatal(err)
	}
	domain := "reserve-unverified.example.com"
	if _, err := ids.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatal(err)
	}
	agent, err := ids.CreateAgent(ctx, "agent@"+domain, domain, "", "", "local", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	messageID := createMessageForAgent(t, pool, agent.ID, "unverified-first")

	d, err := store.Reserve(ctx, sendramp.ReserveRequest{
		MessageID: messageID, UserID: user.ID, Domain: domain,
		Units: 1, Day: time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC), Schedule: sendramp.DefaultSchedule,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if d.Allowed || !d.IdentityUnverified || d.Status != sendramp.StatusInactive {
		t.Fatalf("decision = %+v, want a hold flagged as an unverified identity", d)
	}
	var counters, reservations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM domain_send_counters WHERE user_id=$1`, user.ID).Scan(&counters); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_ramp_reservations WHERE message_id=$1`, messageID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if counters != 0 || reservations != 0 {
		t.Fatalf("counters=%d reservations=%d, want no ramp accounting for unverified domain", counters, reservations)
	}
}

func TestReserveRejectsDomainOwnedByAnotherUser(t *testing.T) {
	store, pool, _, domain, messageID := seedRampMessage(t, "reserve-foreign")
	other, err := identity.NewStore(pool).CreateOrGetUser(context.Background(), "reserve-other@example.com", "Other", "reserve-other")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Reserve(context.Background(), sendramp.ReserveRequest{
		MessageID: messageID, UserID: other.ID, Domain: domain,
		Units: 1, Day: time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC), Schedule: sendramp.DefaultSchedule,
	})
	var permanent permanentMarker
	if !errors.As(err, &permanent) || !permanent.Permanent() {
		t.Fatalf("Reserve by non-owner err=%v, want permanent owner-mismatch error", err)
	}
}

func TestReserveCompletesDomainWhenScopeAlreadyComplete(t *testing.T) {
	store, pool, userID, domain, messageID := seedRampMessage(t, "reserve-scope-complete")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO sending_ramp_scopes (user_id,domain,status,active_days,start_daily,target_daily,ramp_days) VALUES ($1,'example.com','complete',2,50,100,2)`, userID); err != nil {
		t.Fatal(err)
	}
	d := reserve(t, store, userID, domain, messageID, 1, time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC), sendramp.NewSchedule(50, 100, 2))
	if !d.Allowed || d.Status != sendramp.StatusComplete || d.DailyLimit != 0 {
		t.Fatalf("decision = %+v, want allowed complete without accounting", d)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT sending_ramp_status FROM domains WHERE domain=$1`, domain).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != sendramp.StatusComplete {
		t.Fatalf("domain status=%q, want persisted complete", status)
	}
}

func TestReserveDeniesWhenUnitsExceedLimit(t *testing.T) {
	store, pool, userID, domain, messageID := seedRampMessage(t, "reserve-over-limit")
	day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	d := reserve(t, store, userID, domain, messageID, 60, day, sendramp.NewSchedule(50, 100, 2))
	if d.Allowed || d.Status != sendramp.StatusRamping || d.DailyLimit != 50 || d.UsedToday != 0 {
		t.Fatalf("decision = %+v, want denied at 0/50", d)
	}
	if !d.RetryAt.Equal(day.Add(24 * time.Hour)) {
		t.Fatalf("RetryAt = %v, want next UTC day %v", d.RetryAt, day.Add(24*time.Hour))
	}
	var counters, reservations int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM domain_send_counters WHERE user_id=$1`, userID).Scan(&counters); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sending_ramp_reservations WHERE message_id=$1`, messageID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if counters != 0 || reservations != 0 {
		t.Fatalf("counters=%d reservations=%d, want no accounting rows for over-limit single send", counters, reservations)
	}
}

func TestReserveRejectsReplayAfterRelease(t *testing.T) {
	store, _, userID, domain, messageID := seedRampMessage(t, "reserve-after-release")
	day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	schedule := sendramp.NewSchedule(50, 100, 2)
	if got := reserve(t, store, userID, domain, messageID, 2, day, schedule); !got.Allowed {
		t.Fatalf("reserve = %+v, want allowed", got)
	}
	if err := store.Release(context.Background(), messageID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_, err := store.Reserve(context.Background(), sendramp.ReserveRequest{
		MessageID: messageID, UserID: userID, Domain: domain, Units: 2, Day: day, Schedule: schedule,
	})
	var permanent permanentMarker
	if !errors.As(err, &permanent) || !permanent.Permanent() {
		t.Fatalf("re-reserve after release err=%v, want permanent already-released error", err)
	}
}

func TestReserveRejectsUnitMismatchOnRetry(t *testing.T) {
	store, _, userID, domain, messageID := seedRampMessage(t, "reserve-unit-mismatch")
	day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	schedule := sendramp.NewSchedule(50, 100, 2)
	if got := reserve(t, store, userID, domain, messageID, 2, day, schedule); !got.Allowed {
		t.Fatalf("reserve = %+v, want allowed", got)
	}
	_, err := store.Reserve(context.Background(), sendramp.ReserveRequest{
		MessageID: messageID, UserID: userID, Domain: domain, Units: 3, Day: day, Schedule: schedule,
	})
	var permanent permanentMarker
	if !errors.As(err, &permanent) || !permanent.Permanent() {
		t.Fatalf("retry with different units err=%v, want permanent unit-mismatch error", err)
	}
}

func TestReserveReplaysConfirmedReservationAsAllowed(t *testing.T) {
	store, _, userID, domain, messageID := seedRampMessage(t, "reserve-after-confirm")
	day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	schedule := sendramp.NewSchedule(50, 100, 2)
	if got := reserve(t, store, userID, domain, messageID, 2, day, schedule); !got.Allowed {
		t.Fatalf("reserve = %+v, want allowed", got)
	}
	if err := store.Confirm(context.Background(), messageID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	replay := reserve(t, store, userID, domain, messageID, 2, day, schedule)
	if !replay.Allowed || replay.Status != sendramp.StatusRamping {
		t.Fatalf("replay after confirm = %+v, want allowed ramping", replay)
	}
}

func TestReserveWithZeroDayUsesCurrentUTCDay(t *testing.T) {
	store, pool, userID, domain, messageID := seedRampMessage(t, "reserve-zero-day")
	now := time.Now().UTC()
	wantDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if got := reserve(t, store, userID, domain, messageID, 3, time.Time{}, sendramp.DefaultSchedule); !got.Allowed {
		t.Fatalf("reserve with zero day = %+v, want allowed", got)
	}
	var day time.Time
	var reserved int
	if err := pool.QueryRow(context.Background(), `SELECT day, reserved_count FROM domain_send_counters WHERE user_id=$1 AND domain='example.com'`, userID).Scan(&day, &reserved); err != nil {
		t.Fatal(err)
	}
	if !day.Equal(wantDay) || reserved != 3 {
		t.Fatalf("counter day=%v reserved=%d, want %v/3", day, reserved, wantDay)
	}
}

func TestReserveScopesSingleLabelDomainToItself(t *testing.T) {
	pool := testutil.TestDB(t)
	store := sendramp.NewStore(pool)
	ids := identity.NewStore(pool)
	ctx := context.Background()
	user, err := ids.CreateOrGetUser(ctx, "single-label@example.com", "Single", "single-label")
	if err != nil {
		t.Fatal(err)
	}
	domain := "localhost"
	if _, err := ids.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET sending_status = 'verified' WHERE domain = $1`, domain); err != nil {
		t.Fatal(err)
	}
	agent, err := ids.CreateAgent(ctx, "agent@"+domain, domain, "", "", "local", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	messageID := createMessageForAgent(t, pool, agent.ID, "single-label-first")

	day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	if got := reserve(t, store, user.ID, domain, messageID, 4, day, sendramp.DefaultSchedule); !got.Allowed {
		t.Fatalf("reserve = %+v, want allowed", got)
	}
	var reserved int
	if err := pool.QueryRow(ctx, `SELECT reserved_count FROM domain_send_counters WHERE user_id=$1 AND domain='localhost' AND day=$2`, user.ID, day).Scan(&reserved); err != nil {
		t.Fatalf("counter scoped to raw single-label domain: %v", err)
	}
	if reserved != 4 {
		t.Fatalf("reserved=%d, want 4", reserved)
	}
}

func TestConfirmRejectsEmptyMessageID(t *testing.T) {
	store, _, _, _, _ := seedRampMessage(t, "confirm-empty")
	err := store.Confirm(context.Background(), "")
	var permanent permanentMarker
	if !errors.As(err, &permanent) || !permanent.Permanent() {
		t.Fatalf("Confirm(\"\") err=%v, want permanent empty-id error", err)
	}
}

func TestConfirmUnknownReservationIsNoop(t *testing.T) {
	store, _, _, _, _ := seedRampMessage(t, "confirm-missing")
	if err := store.Confirm(context.Background(), "msg_ramp_never_reserved"); err != nil {
		t.Fatalf("Confirm on unknown reservation: %v", err)
	}
}

func TestReleaseRejectsEmptyMessageID(t *testing.T) {
	store, _, _, _, _ := seedRampMessage(t, "release-empty")
	err := store.Release(context.Background(), "")
	var permanent permanentMarker
	if !errors.As(err, &permanent) || !permanent.Permanent() {
		t.Fatalf("Release(\"\") err=%v, want permanent empty-id error", err)
	}
}

func TestReleaseUnknownReservationIsNoop(t *testing.T) {
	store, _, _, _, _ := seedRampMessage(t, "release-missing")
	if err := store.Release(context.Background(), "msg_ramp_never_reserved"); err != nil {
		t.Fatalf("Release on unknown reservation: %v", err)
	}
}

func TestReleaseConfirmedReservationIsNoop(t *testing.T) {
	store, pool, userID, domain, messageID := seedRampMessage(t, "release-confirmed")
	day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	if got := reserve(t, store, userID, domain, messageID, 5, day, sendramp.DefaultSchedule); !got.Allowed {
		t.Fatalf("reserve = %+v, want allowed", got)
	}
	if err := store.Confirm(context.Background(), messageID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if err := store.Release(context.Background(), messageID); err != nil {
		t.Fatalf("Release on confirmed reservation: %v", err)
	}
	var state string
	var reserved, confirmed int
	if err := pool.QueryRow(context.Background(),
		`SELECT r.state, c.reserved_count, c.confirmed_count
		   FROM sending_ramp_reservations r
		   JOIN domain_send_counters c ON c.user_id=r.user_id AND c.domain=r.domain AND c.day=r.day
		  WHERE r.message_id=$1`, messageID).Scan(&state, &reserved, &confirmed); err != nil {
		t.Fatal(err)
	}
	if state != "confirmed" || reserved != 5 || confirmed != 5 {
		t.Fatalf("state=%q reserved=%d confirmed=%d, want confirmed/5/5 untouched", state, reserved, confirmed)
	}
}

func TestResolveUnknownMessageIsNoop(t *testing.T) {
	store, _, _, _, _ := seedRampMessage(t, "resolve-missing")
	if err := store.Resolve(context.Background(), "msg_ramp_never_existed"); err != nil {
		t.Fatalf("Resolve on unknown message: %v", err)
	}
}

func TestResolveIgnoresNonTerminalDeliveryStatus(t *testing.T) {
	store, pool, userID, domain, messageID := seedRampMessage(t, "resolve-nonterminal")
	day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	if got := reserve(t, store, userID, domain, messageID, 2, day, sendramp.DefaultSchedule); !got.Allowed {
		t.Fatalf("reserve = %+v, want allowed", got)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE messages SET delivery_status='sending' WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	if err := store.Resolve(context.Background(), messageID); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM sending_ramp_reservations WHERE message_id=$1`, messageID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "reserved" {
		t.Fatalf("state=%q, want reserved (non-terminal outcome must not settle)", state)
	}
}

// TestConfirmAfterTheDaysCounterWasReapedIsANoOp covers the one legitimate way
// a reservation can outlive its own day counter.
//
// Maintenance deletes counter rows older than 35 days once nothing `reserved`
// still points at them, while a reservation gets its own seven-day window from
// the moment it settles — so a message released long after its send day leaves
// a released reservation whose counter row is already gone. Late authoritative
// acceptance still arrives for it, and the restoration it would perform has
// nothing left to restore: no capacity to give back and no day left to qualify.
// Reporting that as an error makes the finalizer retry a correction that can
// never apply.
func TestConfirmAfterTheDaysCounterWasReapedIsANoOp(t *testing.T) {
	store, pool, userID, domain, messageID := seedRampMessage(t, "confirm-reaped")
	ctx := context.Background()
	day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	if got := reserve(t, store, userID, domain, messageID, 5, day, sendramp.DefaultSchedule); !got.Allowed {
		t.Fatalf("seed reservation = %+v, want allowed", got)
	}
	if err := store.Release(ctx, messageID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM domain_send_counters WHERE user_id=$1`, userID); err != nil {
		t.Fatalf("reap the day counter: %v", err)
	}

	if err := store.Confirm(ctx, messageID); err != nil {
		t.Fatalf("a correction for a reaped day must be a no-op, not an error: %v", err)
	}
}

// TestReleaseRefusesToRecordAGiveBackThatNeverHappened is the mirror of the
// restoration above.
//
// The decrement is deliberately guarded, so it can match nothing — and when it
// does, no units came back. Writing `released` anyway makes the reservation
// claim a give-back that never happened, and ConfirmTx's released→confirmed
// restoration believes it: a later authoritative acceptance adds those units
// back to a counter that never returned them, minting daily allowance out of an
// inconsistency.
func TestReleaseRefusesToRecordAGiveBackThatNeverHappened(t *testing.T) {
	store, pool, userID, domain, messageID := seedRampMessage(t, "release-short")
	ctx := context.Background()
	day := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	if got := reserve(t, store, userID, domain, messageID, 10, day, sendramp.DefaultSchedule); !got.Allowed {
		t.Fatalf("seed reservation = %+v, want allowed", got)
	}
	// The counter now holds fewer reserved units than the reservation claims,
	// which is the only shape in which the guarded decrement matches nothing
	// while the row is still there.
	if _, err := pool.Exec(ctx,
		`UPDATE domain_send_counters SET reserved_count = 4 WHERE user_id=$1`, userID); err != nil {
		t.Fatalf("short the counter: %v", err)
	}

	err := store.Release(ctx, messageID)
	var permanent permanentMarker
	if !errors.As(err, &permanent) || !permanent.Permanent() {
		t.Fatalf("Release err = %v, want a permanent error — the counter never returned the units", err)
	}
	var state string
	if err := pool.QueryRow(ctx,
		`SELECT state FROM sending_ramp_reservations WHERE message_id=$1`, messageID).Scan(&state); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	if state != "reserved" {
		t.Errorf("reservation state = %q: it claims a give-back of 10 units the counter never returned, "+
			"which a later restoration would add back out of nothing", state)
	}
}
