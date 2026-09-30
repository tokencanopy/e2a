package sendramp

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// AccountDailyLimit reports external-recipient capacity. Shared identity usage
// is a subset of account usage, with a ceiling of 50. Reserved uncertain sends
// remain charged until an authoritative outcome resolves them.
type AccountDailyLimit struct {
	Limit           int       `json:"limit"`
	Used            int       `json:"used"`
	SharedLimit     int       `json:"shared_limit"`
	SharedUsed      int       `json:"shared_used"`
	CleanActiveDays int       `json:"clean_active_days"`
	ResetsAt        time.Time `json:"resets_at"`
	Allowed         bool      `json:"-"`
	SharedBinding   bool      `json:"-"`
}

// AccountTrustLimit reaches 2,000 on the thirtieth clean active day. Only
// completed UTC days earn capacity; neither elapsed time nor retries do.
func AccountTrustLimit(days int) int {
	if days < 0 {
		days = 0
	}
	if days > 29 {
		days = 29
	}
	return 20 + 1980*days/29
}

type AccountReserveRequest struct {
	UserID, MessageID string
	Units             int
	Shared            bool
	Day               time.Time
	PlanCap           *int
}

func lockAccount(ctx context.Context, tx pgx.Tx, user string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO account_sending_trust(user_id) VALUES($1) ON CONFLICT DO NOTHING`, user); err != nil {
		return err
	}
	var id string
	return tx.QueryRow(ctx, `SELECT user_id FROM account_sending_trust WHERE user_id=$1 FOR UPDATE`, user).Scan(&id)
}

// AccountSnapshotTx is read-only. Callers reserving capacity first lock the
// account trust row; UI reads need no locks. A plan cap of nil is unlimited.
func AccountSnapshotTx(ctx context.Context, tx pgx.Tx, user string, now time.Time, planCap *int) (AccountDailyLimit, error) {
	day := utcDay(now)
	d := AccountDailyLimit{ResetsAt: day.AddDate(0, 0, 1)}
	var floor int
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT grandfather_daily FROM account_sending_trust WHERE user_id=$1),0),
 COALESCE((SELECT archived_clean_days FROM account_sending_trust WHERE user_id=$1),0) + (SELECT count(*) FROM account_send_days WHERE user_id=$1 AND day<$2 AND confirmed_count>0 AND NOT breached),
 COALESCE((SELECT reserved_count FROM account_send_days WHERE user_id=$1 AND day=$2),0),
 COALESCE((SELECT shared_count FROM account_send_days WHERE user_id=$1 AND day=$2),0)`, user, day).Scan(&floor, &d.CleanActiveDays, &d.Used, &d.SharedUsed)
	if err != nil {
		return d, err
	}
	d.Limit = max(floor, AccountTrustLimit(d.CleanActiveDays))
	if planCap != nil {
		d.Limit = min(d.Limit, max(0, *planCap))
	}
	d.SharedLimit = min(50, d.Limit)
	return d, nil
}

// ReserveAccountTx serializes all identities of one account. Call it after
// account-control and platform-budget locks, before issuing a provider grant.
func ReserveAccountTx(ctx context.Context, tx pgx.Tx, r AccountReserveRequest) (AccountDailyLimit, error) {
	if r.UserID == "" || r.MessageID == "" || r.Units < 1 {
		return AccountDailyLimit{}, permanentf("sendramp: invalid account reservation")
	}
	if err := lockAccount(ctx, tx, r.UserID); err != nil {
		return AccountDailyLimit{}, err
	}
	day := utcDay(r.Day)
	d, err := AccountSnapshotTx(ctx, tx, r.UserID, day, r.PlanCap)
	if err != nil {
		return d, err
	}
	var owner, state string
	var oldDay time.Time
	var units int
	var shared bool
	err = tx.QueryRow(ctx, `SELECT user_id,day,units,shared,state FROM account_send_reservations WHERE message_id=$1 FOR UPDATE`, r.MessageID).Scan(&owner, &oldDay, &units, &shared, &state)
	switch {
	case err == nil:
		if owner != r.UserID || shared != r.Shared {
			return d, permanentf("sendramp: account reservation changed")
		}
		if state == "released" {
			return d, permanentf("sendramp: account reservation released")
		}
		if state == "confirmed" {
			d.Allowed = true
			return d, nil
		}
		// Classification may change when the owner mailbox or live-agent set
		// changes. Acquire newly external units, but retain uncertain exposure
		// already reserved by a prior attempt rather than refunding it here.
		r.Units = max(r.Units, units)
		if oldDay.Equal(day) {
			extra := r.Units - units
			if extra > d.Limit-d.Used || (r.Shared && extra > d.SharedLimit-d.SharedUsed) {
				return d, nil
			}
			if extra > 0 {
				if _, err = tx.Exec(ctx, `UPDATE account_send_days SET reserved_count=reserved_count+$3,shared_count=shared_count+$4 WHERE user_id=$1 AND day=$2`, r.UserID, day, extra, sharedUnits(extra, r.Shared)); err != nil {
					return d, err
				}
				if _, err = tx.Exec(ctx, `UPDATE account_send_reservations SET units=$2 WHERE message_id=$1`, r.MessageID, r.Units); err != nil {
					return d, err
				}
			}
			d.Used += extra
			d.SharedUsed += sharedUnits(extra, r.Shared)
			d.Allowed = true
			return d, nil
		}
		if r.Units > d.Limit-d.Used || (r.Shared && r.Units > d.SharedLimit-d.SharedUsed) {
			return d, nil
		}
		// Retries on a later day move the unresolved reservation. Both day buckets
		// remain serialized by the account lock, including late settlement.
		if _, err = tx.Exec(ctx, `UPDATE account_send_days SET reserved_count=reserved_count-$3, shared_count=shared_count-$4 WHERE user_id=$1 AND day=$2`, r.UserID, oldDay, units, sharedUnits(units, shared)); err != nil {
			return d, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM account_send_reservations WHERE message_id=$1`, r.MessageID); err != nil {
			return d, err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return d, err
	}
	if r.Units > d.Limit-d.Used || (r.Shared && r.Units > d.SharedLimit-d.SharedUsed) {
		return d, nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO account_send_days(user_id,day,reserved_count,shared_count) VALUES($1,$2,$3,$4)
 ON CONFLICT(user_id,day) DO UPDATE SET reserved_count=account_send_days.reserved_count+EXCLUDED.reserved_count,shared_count=account_send_days.shared_count+EXCLUDED.shared_count`, r.UserID, day, r.Units, sharedUnits(r.Units, r.Shared))
	if err != nil {
		return d, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO account_send_reservations(message_id,user_id,day,units,shared,state) VALUES($1,$2,$3,$4,$5,'reserved')`, r.MessageID, r.UserID, day, r.Units, r.Shared)
	if err != nil {
		return d, err
	}
	d.Used += r.Units
	d.SharedUsed += sharedUnits(r.Units, r.Shared)
	d.Allowed = true
	return d, nil
}

func sharedUnits(units int, shared bool) int {
	if shared {
		return units
	}
	return 0
}

// SettleAccountTx preserves idempotency and authoritative late acceptance.
// It probes immutable ownership before taking locks in account->message order.
func SettleAccountTx(ctx context.Context, tx pgx.Tx, message string, accepted bool, acceptedDay time.Time, acceptedUnits int) error {
	var user string
	err := tx.QueryRow(ctx, `SELECT user_id FROM account_send_reservations WHERE message_id=$1`, message).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = lockAccount(ctx, tx, user); err != nil {
		return err
	}
	var state string
	var day time.Time
	var units int
	var shared bool
	err = tx.QueryRow(ctx, `SELECT day,units,shared,state FROM account_send_reservations WHERE message_id=$1 FOR UPDATE`, message).Scan(&day, &units, &shared, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "confirmed" || (!accepted && state == "released") {
		return nil
	}
	next := "released"
	reserved := -units
	if accepted {
		next = "confirmed"
		reserved = 0
		if state == "released" {
			reserved = units
		}
	}
	_, err = tx.Exec(ctx, `UPDATE account_send_days SET reserved_count=reserved_count+$3,shared_count=shared_count+$4 WHERE user_id=$1 AND day=$2`, user, day, reserved, sharedUnits(reserved, shared))
	if err != nil {
		return err
	}
	// Capacity follows the conservative message reservation, but activity follows
	// the immutable accepted attempt. Internal-only retries earn no trust. Update
	// only retained day buckets: recreating pruned history could double-credit a
	// day already folded into archived_clean_days.
	if accepted && acceptedUnits > 0 {
		if acceptedDay.IsZero() {
			return errors.New("sendramp: accepted attempt day is required")
		}
		if _, err = tx.Exec(ctx, `UPDATE account_send_days SET confirmed_count=confirmed_count+$3 WHERE user_id=$1 AND day=$2`, user, utcDay(acceptedDay), acceptedUnits); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE account_send_reservations SET state=$2 WHERE message_id=$1`, message, next)
	return err
}
