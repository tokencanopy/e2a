package sendramp_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/sendramp"
)

func TestAccountTrustSchedule(t *testing.T) {
	for _, tc := range []struct{ days, want int }{{0, 20}, {1, 88}, {14, 975}, {29, 2000}, {90, 2000}} {
		if got := sendramp.AccountTrustLimit(tc.days); got != tc.want {
			t.Errorf("days %d: got %d want %d", tc.days, got, tc.want)
		}
	}
}

func TestAccountTrustReservationAndCleanDays(t *testing.T) {
	_, pool, user, _, message := seedRampMessage(t, "account-trust")
	ctx := context.Background()
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reserve := func(units int, at time.Time) sendramp.AccountDailyLimit {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		d, err := sendramp.ReserveAccountTx(ctx, tx, sendramp.AccountReserveRequest{UserID: user, MessageID: message, Units: units, Day: at})
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := reserve(21, day)
	if d.Allowed || d.Limit != 20 || d.Used != 0 {
		t.Fatalf("initial over-limit: %+v", d)
	}
	d = reserve(20, day)
	if !d.Allowed || d.Used != 20 {
		t.Fatalf("reserve: %+v", d)
	}
	d = reserve(20, day)
	if !d.Allowed || d.Used != 20 {
		t.Fatalf("retry double charged: %+v", d)
	}
	for i := 0; i < 2; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = sendramp.SettleAccountTx(ctx, tx, message, true, day, 20); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check := func(at time.Time, limit, days int) {
		t.Helper()
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		d, err := sendramp.AccountSnapshotTx(ctx, tx, user, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if d.Limit != limit || d.CleanActiveDays != days {
			t.Fatalf("snapshot: %+v", d)
		}
	}
	check(day, 20, 0) // Today cannot earn additional capacity during the same day.
	check(day.AddDate(0, 0, 1), 88, 1)
	check(day.AddDate(0, 0, 30), 88, 1) // Idle time earns no trust.
}

func TestAccountTrustRetentionKeepsEarnedDaysAndUnresolvedReservations(t *testing.T) {
	store, pool, user, _, message := seedRampMessage(t, "account-retention")
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -100)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = sendramp.ReserveAccountTx(ctx, tx, sendramp.AccountReserveRequest{UserID: user, MessageID: message, Units: 1, Day: old}); err != nil {
		t.Fatal(err)
	}
	if err = sendramp.SettleAccountTx(ctx, tx, message, true, old, 1); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.Sweep(ctx, now); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM account_send_days WHERE user_id=$1`, user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("old account buckets retained: %d", rows)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	d, err := sendramp.AccountSnapshotTx(ctx, tx, user, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Limit != 88 || d.CleanActiveDays != 1 {
		t.Fatalf("pruning lost trust: %+v", d)
	}
}

func TestAccountTrustRetentionPreservesUnresolvedAndBreachedDays(t *testing.T) {
	store, pool, user, domain, message := seedRampMessage(t, "account-retain-open")
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -100)
	second := createMessageForAgent(t, pool, "agent@"+domain, "account-retain-breach")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for i, msg := range []string{message, second} {
		if _, err = sendramp.ReserveAccountTx(ctx, tx, sendramp.AccountReserveRequest{UserID: user, MessageID: msg, Units: 1, Day: old.AddDate(0, 0, i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err = sendramp.SettleAccountTx(ctx, tx, second, true, old.AddDate(0, 0, 1), 1); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE account_send_days SET breached=true WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = store.Sweep(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	var reserved int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM account_send_reservations WHERE user_id=$1 AND state='reserved'`, user).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 1 {
		t.Fatal("lost unresolved reservation")
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	d, err := sendramp.AccountSnapshotTx(ctx, tx, user, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Limit != 20 || d.CleanActiveDays != 0 {
		t.Fatalf("breached day earned trust: %+v", d)
	}
}

func TestAccountTrustMidnightRefusalRetainsLateEvidence(t *testing.T) {
	_, pool, user, _, message := seedRampMessage(t, "account-midnight")
	ctx := context.Background()
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	req := sendramp.AccountReserveRequest{UserID: user, MessageID: message, Units: 1, Day: day, Shared: true}
	if d, err := sendramp.ReserveAccountTx(ctx, tx, req); err != nil || !d.Allowed {
		t.Fatalf("reserve: %+v %v", d, err)
	}
	zero := 0
	req.PlanCap = &zero
	req.Day = day.AddDate(0, 0, 1)
	if d, err := sendramp.ReserveAccountTx(ctx, tx, req); err != nil || d.Allowed {
		t.Fatalf("midnight cap: %+v %v", d, err)
	}
	// A permanent rejection can be corrected by authoritative acceptance;
	// neither a midnight hold nor duplicate outcomes may lose/double credit.
	for _, accepted := range []bool{false, false, true, true, false} {
		if err = sendramp.SettleAccountTx(ctx, tx, message, accepted, day, 1); err != nil {
			t.Fatal(err)
		}
	}
	var reserved, confirmed int
	if err = tx.QueryRow(ctx, `SELECT reserved_count,confirmed_count FROM account_send_days WHERE user_id=$1 AND day=$2`, user, day).Scan(&reserved, &confirmed); err != nil {
		t.Fatal(err)
	}
	if reserved != 1 || confirmed != 1 {
		t.Fatalf("late evidence: reserved=%d confirmed=%d", reserved, confirmed)
	}
	d, err := sendramp.AccountSnapshotTx(ctx, tx, user, req.Day, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.CleanActiveDays != 1 || d.Used != 0 {
		t.Fatalf("day attribution: %+v", d)
	}
}

func TestAccountTrustReclassifiedRecipientsAcquireAdditionalCapacity(t *testing.T) {
	_, pool, user, _, message := seedRampMessage(t, "account-reclassified")
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	r := sendramp.AccountReserveRequest{UserID: user, MessageID: message, Units: 1, Day: time.Now().UTC()}
	if d, err := sendramp.ReserveAccountTx(ctx, tx, r); err != nil || !d.Allowed {
		t.Fatalf("initial: %+v %v", d, err)
	}
	r.Units = 2
	if d, err := sendramp.ReserveAccountTx(ctx, tx, r); err != nil || !d.Allowed || d.Used != 2 {
		t.Fatalf("reclassification must acquire difference: %+v %v", d, err)
	}
	r.Units = 21
	if d, err := sendramp.ReserveAccountTx(ctx, tx, r); err != nil || d.Allowed || d.Used != 2 {
		t.Fatalf("refused resize must preserve reservation: %+v %v", d, err)
	}
}

func TestAccountTrustAcceptedAttemptActivity(t *testing.T) {
	for _, units := range []int{0, 1} {
		t.Run(fmt.Sprintf("accepted-units-%d", units), func(t *testing.T) {
			_, pool, user, _, message := seedRampMessage(t, "account-attempt")
			ctx := context.Background()
			day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			for _, at := range []time.Time{day, day.AddDate(0, 0, 1)} {
				d, err := sendramp.ReserveAccountTx(ctx, tx, sendramp.AccountReserveRequest{UserID: user, MessageID: message, Units: 1, Day: at})
				if err != nil || !d.Allowed {
					t.Fatalf("reserve: %+v %v", d, err)
				}
			}
			if err = sendramp.SettleAccountTx(ctx, tx, message, true, day, units); err != nil {
				t.Fatal(err)
			}
			var original, retry int
			if err = tx.QueryRow(ctx, `SELECT confirmed_count FROM account_send_days WHERE user_id=$1 AND day=$2`, user, day).Scan(&original); err != nil {
				t.Fatal(err)
			}
			if err = tx.QueryRow(ctx, `SELECT confirmed_count FROM account_send_days WHERE user_id=$1 AND day=$2`, user, day.AddDate(0, 0, 1)).Scan(&retry); err != nil {
				t.Fatal(err)
			}
			if original != units || retry != 0 {
				t.Fatalf("activity original=%d retry=%d; want %d,0", original, retry, units)
			}
		})
	}
}
