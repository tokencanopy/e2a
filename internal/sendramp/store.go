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

// Exempt flips one verified, ramp-inactive domain to 'exempt'.
//
// No automatic path calls this. The disabled ramp gate used to, once per
// eligible send, which made "this sender is established" a decision taken
// silently by the send path (see internal/agent.outboundRampGate.Reserve);
// hosted grandfathering is the audited one-shot in internal/sendingpolicy
// instead. This stays as the store-level primitive for an explicit,
// single-domain operator exemption and must not be re-wired to a hot path.
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

func (s *Store) Reserve(ctx context.Context, req ReserveRequest) (Decision, error) {
	if req.MessageID == "" || req.UserID == "" || req.Domain == "" || req.Units < 1 {
		return Decision{}, permanentf("sendramp: invalid reservation request")
	}
	day := utcDay(req.Day)
	schedule := NewSchedule(req.Schedule.StartDaily, req.Schedule.TargetDaily, req.Schedule.RampDays)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(ctx)

	var owner, sendingStatus, domainStatus string
	err = tx.QueryRow(ctx, `SELECT COALESCE(user_id,''), sending_status, sending_ramp_status FROM domains WHERE domain=$1 FOR UPDATE`, req.Domain).Scan(&owner, &sendingStatus, &domainStatus)
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
		return commitDecision(ctx, tx, Decision{Allowed: true, Status: domainStatus})
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
		return commitDecision(ctx, tx, Decision{Allowed: true, Status: StatusComplete})
	}
	if activeDays >= schedule.RampDays && lastQualifiedDay != nil && utcDay(*lastQualifiedDay).Before(day) {
		if _, err := tx.Exec(ctx, `UPDATE sending_ramp_scopes SET status='complete',completed_at=now() WHERE user_id=$1 AND domain=$2`, req.UserID, scope); err != nil {
			return Decision{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE domains SET sending_ramp_status='complete' WHERE user_id=$1 AND domain=$2`, req.UserID, req.Domain); err != nil {
			return Decision{}, err
		}
		return commitDecision(ctx, tx, Decision{Allowed: true, Status: StatusComplete})
	}

	var priorDay time.Time
	var priorUnits int
	var priorState string
	err = tx.QueryRow(ctx, `SELECT day,units,state FROM sending_ramp_reservations WHERE message_id=$1 FOR UPDATE`, req.MessageID).Scan(&priorDay, &priorUnits, &priorState)
	if err == nil {
		if priorState == "confirmed" {
			return commitDecision(ctx, tx, Decision{Allowed: true, Status: StatusRamping})
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
			return commitDecision(ctx, tx, Decision{Allowed: true, Status: StatusRamping, DailyLimit: limit, UsedToday: used})
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
		return commitDecision(ctx, tx, Decision{Allowed: false, Status: StatusRamping, DailyLimit: appliedLimit, UsedToday: used, RetryAt: day.Add(24 * time.Hour)})
	}
	if err != nil {
		return Decision{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sending_ramp_reservations (message_id,day,user_id,domain,units) VALUES ($1,$2,$3,$4,$5)`, req.MessageID, day, req.UserID, scope, req.Units); err != nil {
		return Decision{}, err
	}
	return commitDecision(ctx, tx, Decision{Allowed: true, Status: StatusRamping, DailyLimit: appliedLimit, UsedToday: used})
}

func (s *Store) Confirm(ctx context.Context, messageID string) error {
	if messageID == "" {
		return permanentf("sendramp: empty message id")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var day time.Time
	var userID, domain, state string
	var units int
	err = tx.QueryRow(ctx, `SELECT day,user_id,domain,units,state FROM sending_ramp_reservations WHERE message_id=$1 FOR UPDATE`, messageID).Scan(&day, &userID, &domain, &units, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if state == "confirmed" {
		return tx.Commit(ctx)
	}
	var confirmed, limit int
	var query string
	switch state {
	case "reserved":
		query = `UPDATE domain_send_counters
		            SET confirmed_count=confirmed_count+$4
		          WHERE user_id=$1 AND domain=$2 AND day=$3
		      RETURNING confirmed_count,daily_limit`
	case "released":
		// A locally inferred failure can be corrected by later authoritative
		// provider feedback. Release returned its units to the daily counter, so
		// confirming that real send must restore consumed as well as accepted
		// volume. The reservation row lock makes this transition idempotent.
		query = `UPDATE domain_send_counters
		            SET reserved_count=reserved_count+$4,
		                confirmed_count=confirmed_count+$4
		          WHERE user_id=$1 AND domain=$2 AND day=$3
		      RETURNING confirmed_count,daily_limit`
	default:
		return permanentf("sendramp: invalid reservation state %q", state)
	}
	if err := tx.QueryRow(ctx, query, userID, domain, utcDay(day), units).Scan(&confirmed, &limit); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sending_ramp_reservations SET state='confirmed',updated_at=now() WHERE message_id=$1`, messageID); err != nil {
		return err
	}
	if Qualifies(confirmed, limit) {
		if _, err := tx.Exec(ctx, `UPDATE sending_ramp_scopes SET active_days=active_days+1,last_qualified_day=$3 WHERE user_id=$1 AND domain=$2 AND (last_qualified_day IS NULL OR last_qualified_day < $3)`, userID, domain, utcDay(day)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) Release(ctx context.Context, messageID string) error {
	if messageID == "" {
		return permanentf("sendramp: empty message id")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var day time.Time
	var userID, domain, state string
	var units int
	err = tx.QueryRow(ctx, `SELECT day,user_id,domain,units,state FROM sending_ramp_reservations WHERE message_id=$1 FOR UPDATE`, messageID).Scan(&day, &userID, &domain, &units, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if state != "reserved" {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE domain_send_counters SET reserved_count=reserved_count-$4 WHERE user_id=$1 AND domain=$2 AND day=$3 AND reserved_count >= $4`, userID, domain, utcDay(day), units); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sending_ramp_reservations SET state='released',updated_at=now() WHERE message_id=$1`, messageID); err != nil {
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
