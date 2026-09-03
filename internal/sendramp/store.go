package sendramp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/publicsuffix"
)

const (
	StatusInactive = "inactive"
	StatusRamping  = "ramping"
	StatusComplete = "complete"
	StatusExempt   = "exempt"
)

type PermanentError struct{ Err error }

func (e *PermanentError) Error() string   { return e.Err.Error() }
func (e *PermanentError) Unwrap() error   { return e.Err }
func (e *PermanentError) Permanent() bool { return true }

func permanentf(format string, args ...any) error {
	return &PermanentError{Err: fmt.Errorf(format, args...)}
}

type ReserveRequest struct {
	MessageID string
	UserID    string
	Domain    string
	Units     int
	Day       time.Time
	Schedule  Schedule
}

type Decision struct {
	Allowed    bool
	Status     string
	DailyLimit int
	UsedToday  int
	RetryAt    time.Time
}

type Snapshot struct {
	Status      string
	StartedAt   *time.Time
	CompletedAt *time.Time
	ActiveDays  int
	StartDaily  int
	TargetDaily int
	RampDays    int
	DailyLimit  int
	UsedToday   int
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Exempt(ctx context.Context, userID, domain string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE domains SET sending_ramp_status = 'exempt'
		 WHERE domain = $1 AND user_id = $2 AND sending_status = 'verified'
		   AND sending_ramp_status = 'inactive'`, domain, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM domains WHERE domain=$1 AND user_id=$2)`, domain, userID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return permanentf("sendramp: domain owner mismatch")
		}
	}
	return nil
}

func (s *Store) Snapshot(ctx context.Context, userID, domain string, now time.Time) (Snapshot, error) {
	var snap Snapshot
	if err := s.pool.QueryRow(ctx, `SELECT sending_ramp_status FROM domains WHERE domain=$1 AND user_id=$2`, domain, userID).Scan(&snap.Status); err != nil {
		return Snapshot{}, err
	}
	if snap.Status != StatusRamping {
		return snap, nil
	}
	var scopeStatus string
	err := s.pool.QueryRow(ctx, `
		SELECT status, started_at, completed_at, active_days, start_daily, target_daily, ramp_days
		  FROM sending_ramp_scopes WHERE user_id=$1 AND domain=$2`, userID, registrableDomain(domain)).Scan(
		&scopeStatus, &snap.StartedAt, &snap.CompletedAt, &snap.ActiveDays,
		&snap.StartDaily, &snap.TargetDaily, &snap.RampDays)
	if err != nil {
		return Snapshot{}, err
	}
	if scopeStatus == StatusComplete {
		snap.Status = StatusComplete
		return snap, nil
	}
	day := utcDay(now)
	err = s.pool.QueryRow(ctx, `SELECT reserved_count, daily_limit FROM domain_send_counters WHERE user_id=$1 AND domain=$2 AND day=$3`, userID, registrableDomain(domain), day).Scan(&snap.UsedToday, &snap.DailyLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		snap.DailyLimit = NewSchedule(snap.StartDaily, snap.TargetDaily, snap.RampDays).CapForActiveDay(snap.ActiveDays)
		return snap, nil
	}
	return snap, err
}

// Reserve is the pool-owning wrapper. The logic lives in ReserveTx so the
// sending-protection gate can compose the ramp into its own transaction
// without a second implementation drifting from this one.
func (s *Store) Reserve(ctx context.Context, req ReserveRequest) (Decision, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(ctx)
	d, err := ReserveTx(ctx, tx, req)
	if err != nil {
		return Decision{}, err
	}
	return commitDecision(ctx, tx, d)
}

// ReserveTx acquires ramp capacity for one message inside a caller's
// transaction, in the normative suborder: domain identity, registrable-domain
// scope, message reservation, UTC day counter.
func ReserveTx(ctx context.Context, tx pgx.Tx, req ReserveRequest) (Decision, error) {
	if req.MessageID == "" || req.UserID == "" || req.Domain == "" || req.Units < 1 {
		return Decision{}, permanentf("sendramp: invalid reservation request")
	}
	day := utcDay(req.Day)
	schedule := NewSchedule(req.Schedule.StartDaily, req.Schedule.TargetDaily, req.Schedule.RampDays)

	var owner, sendingStatus, domainStatus string
	err := tx.QueryRow(ctx, `SELECT COALESCE(user_id,''), sending_status, sending_ramp_status FROM domains WHERE domain=$1 FOR UPDATE`, req.Domain).Scan(&owner, &sendingStatus, &domainStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, permanentf("sendramp: domain not found")
	}
	if err != nil {
		return Decision{}, err
	}
	if owner != req.UserID {
		return Decision{}, permanentf("sendramp: domain owner mismatch")
	}
	if domainStatus == StatusExempt || domainStatus == StatusComplete || sendingStatus != "verified" {
		return decisionOnly(Decision{Allowed: true, Status: domainStatus})
	}

	scope := registrableDomain(req.Domain)
	if _, err := tx.Exec(ctx, `INSERT INTO sending_ramp_scopes (user_id,domain,start_daily,target_daily,ramp_days) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, req.UserID, scope, schedule.StartDaily, schedule.TargetDaily, schedule.RampDays); err != nil {
		return Decision{}, err
	}
	var scopeStatus string
	var activeDays, storedStart, storedTarget, storedDays int
	var lastQualifiedDay *time.Time
	if err := tx.QueryRow(ctx, `SELECT status,active_days,last_qualified_day,start_daily,target_daily,ramp_days FROM sending_ramp_scopes WHERE user_id=$1 AND domain=$2 FOR UPDATE`, req.UserID, scope).Scan(&scopeStatus, &activeDays, &lastQualifiedDay, &storedStart, &storedTarget, &storedDays); err != nil {
		return Decision{}, err
	}
	if storedStart < MinimumStartDaily || storedTarget < storedStart || storedDays < 1 {
		return Decision{}, permanentf("sendramp: invalid persisted schedule")
	}
	if domainStatus == StatusInactive {
		if _, err := tx.Exec(ctx, `UPDATE domains SET sending_ramp_status='ramping' WHERE domain=$1`, req.Domain); err != nil {
			return Decision{}, err
		}
	}
	schedule = Schedule{StartDaily: storedStart, TargetDaily: storedTarget, RampDays: storedDays}
	if scopeStatus == StatusComplete {
		if _, err := tx.Exec(ctx, `UPDATE domains SET sending_ramp_status='complete' WHERE domain=$1`, req.Domain); err != nil {
			return Decision{}, err
		}
		return decisionOnly(Decision{Allowed: true, Status: StatusComplete})
	}
	if activeDays >= schedule.RampDays && lastQualifiedDay != nil && utcDay(*lastQualifiedDay).Before(day) {
		if _, err := tx.Exec(ctx, `UPDATE sending_ramp_scopes SET status='complete',completed_at=now() WHERE user_id=$1 AND domain=$2`, req.UserID, scope); err != nil {
			return Decision{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE domains SET sending_ramp_status='complete' WHERE user_id=$1 AND domain=$2`, req.UserID, req.Domain); err != nil {
			return Decision{}, err
		}
		return decisionOnly(Decision{Allowed: true, Status: StatusComplete})
	}

	var priorDay time.Time
	var priorUnits int
	var priorState string
	err = tx.QueryRow(ctx, `SELECT day,units,state FROM sending_ramp_reservations WHERE message_id=$1 FOR UPDATE`, req.MessageID).Scan(&priorDay, &priorUnits, &priorState)
	if err == nil {
		if priorState == "confirmed" {
			return decisionOnly(Decision{Allowed: true, Status: StatusRamping})
		}
		if priorState == "released" {
			return Decision{}, permanentf("sendramp: reservation already released")
		}
		if priorUnits != req.Units {
			return Decision{}, permanentf("sendramp: reservation unit mismatch")
		}
		if utcDay(priorDay).Equal(day) {
			var used, limit int
			if err := tx.QueryRow(ctx, `SELECT reserved_count,daily_limit FROM domain_send_counters WHERE user_id=$1 AND domain=$2 AND day=$3`, req.UserID, scope, day).Scan(&used, &limit); err != nil {
				return Decision{}, err
			}
			return decisionOnly(Decision{Allowed: true, Status: StatusRamping, DailyLimit: limit, UsedToday: used})
		}
		if _, err := tx.Exec(ctx, `UPDATE domain_send_counters SET reserved_count=reserved_count-$4 WHERE user_id=$1 AND domain=$2 AND day=$3 AND reserved_count >= $4`, req.UserID, scope, utcDay(priorDay), priorUnits); err != nil {
			return Decision{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sending_ramp_reservations WHERE message_id=$1`, req.MessageID); err != nil {
			return Decision{}, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, err
	}

	limit := schedule.CapForActiveDay(activeDays)
	var used, appliedLimit int
	err = tx.QueryRow(ctx, `
		INSERT INTO domain_send_counters (user_id,domain,day,reserved_count,confirmed_count,daily_limit)
		SELECT $1::text,$2::text,$3::date,$4::integer,0,$5::integer WHERE $4::integer <= $5::integer
		ON CONFLICT (user_id,domain,day) DO UPDATE
		 SET reserved_count=domain_send_counters.reserved_count+EXCLUDED.reserved_count
		 WHERE domain_send_counters.reserved_count+EXCLUDED.reserved_count <= domain_send_counters.daily_limit
		RETURNING reserved_count,daily_limit`, req.UserID, scope, day, req.Units, limit).Scan(&used, &appliedLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `SELECT reserved_count,daily_limit FROM domain_send_counters WHERE user_id=$1 AND domain=$2 AND day=$3`, req.UserID, scope, day).Scan(&used, &appliedLimit); errors.Is(err, pgx.ErrNoRows) {
			used, appliedLimit = 0, limit
		} else if err != nil {
			return Decision{}, err
		}
		return decisionOnly(Decision{Allowed: false, Status: StatusRamping, DailyLimit: appliedLimit, UsedToday: used, RetryAt: day.Add(24 * time.Hour)})
	}
	if err != nil {
		return Decision{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sending_ramp_reservations (message_id,day,user_id,domain,units) VALUES ($1,$2,$3,$4,$5)`, req.MessageID, day, req.UserID, scope, req.Units); err != nil {
		return Decision{}, err
	}
	return decisionOnly(Decision{Allowed: true, Status: StatusRamping, DailyLimit: appliedLimit, UsedToday: used})
}

func (s *Store) Confirm(ctx context.Context, messageID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error { return ConfirmTx(ctx, tx, messageID) })
}

func (s *Store) Release(ctx context.Context, messageID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error { return ReleaseTx(ctx, tx, messageID) })
}

// inTx runs one of the tx-aware helpers in its own transaction.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Resolve settles a pending reservation from the message's durable provider
// outcome. It is used by terminal reconciliation after ambiguous worker exits.
func (s *Store) Resolve(ctx context.Context, messageID string) error {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT delivery_status FROM messages WHERE id=$1`, messageID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch status {
	case "sent", "deferred", "delivered", "bounced", "complained":
		return s.Confirm(ctx, messageID)
	case "failed":
		return s.Release(ctx, messageID)
	default:
		return nil
	}
}

func commitDecision(ctx context.Context, tx pgx.Tx, d Decision) (Decision, error) {
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	return d, nil
}

// decisionOnly is the in-transaction counterpart of commitDecision: the caller
// owns the transaction, so returning is all there is to do.
func decisionOnly(d Decision) (Decision, error) { return d, nil }

func utcDay(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now()
	}
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func registrableDomain(domain string) string {
	if d, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil && d != "" {
		return d
	}
	return domain
}
