package sendramp

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the ramp's single lock order.
//
// The store originally used three different ones: Reserve took the domain, then
// the scope, then the reservation, then the day counter; Confirm took the
// reservation, then the counter, then the scope; Release took the reservation
// and the counter. Three orders over four keys is a deadlock waiting for
// traffic, and it becomes unavoidable the moment the sending-protection gate
// composes both ledgers into one transaction — that transaction already holds
// budget counters when it reaches the ramp, so any disagreement here closes a
// cycle across two subsystems.
//
// Every mutation now acquires, in exactly this suborder:
//
//	ramp domain identity → registrable-domain scope → message reservation → UTC day counter
//
// An operation may SKIP a key it does not need. It may never take one it does
// need out of order. The exported Store methods are thin wrappers so there is
// no second implementation to drift.

// probeReservation reads a reservation's owning scope WITHOUT locking, so a
// caller that only knows a message ID can still take the scope lock first.
//
// Reading before locking is safe because the values it recovers — the owning
// account and registrable domain — are immutable for the life of the row: the
// reservation is keyed by message, and a message never changes hands. Every
// value the decision actually rests on is re-read under the lock below.
func probeReservation(ctx context.Context, tx pgx.Tx, messageID string) (userID, scope string, found bool, err error) {
	err = tx.QueryRow(ctx,
		`SELECT user_id, domain FROM sending_ramp_reservations WHERE message_id = $1`, messageID,
	).Scan(&userID, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return userID, scope, true, nil
}

// lockScope takes the registrable-domain scope row, creating nothing.
func lockScope(ctx context.Context, tx pgx.Tx, userID, scope string) error {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT true FROM sending_ramp_scopes WHERE user_id = $1 AND domain = $2 FOR UPDATE`,
		userID, scope,
	).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// ConfirmTx records authoritative provider acceptance inside a caller's
// transaction, in the normative suborder.
//
// Confirmation is the only thing that advances the ramp. That is deliberate:
// progress must measure delivered volume, not attempts, or a domain could age
// into full allowance by repeatedly failing.
func ConfirmTx(ctx context.Context, tx pgx.Tx, messageID string) error {
	if messageID == "" {
		return permanentf("sendramp: empty message id")
	}
	userID, scope, found, err := probeReservation(ctx, tx, messageID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	// Scope before reservation. Confirmation may advance active_days on this
	// row, and taking it after the reservation is what made the old Confirm
	// disagree with Reserve.
	if err := lockScope(ctx, tx, userID, scope); err != nil {
		return err
	}

	var day time.Time
	var lockedUser, lockedScope, state string
	var units int
	err = tx.QueryRow(ctx, `
		SELECT day, user_id, domain, units, state
		  FROM sending_ramp_reservations WHERE message_id = $1 FOR UPDATE`, messageID,
	).Scan(&day, &lockedUser, &lockedScope, &units, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if lockedUser != userID || lockedScope != scope {
		return permanentf("sendramp: reservation ownership changed under lock")
	}
	if state == "confirmed" {
		return nil
	}

	var query string
	switch state {
	case "reserved":
		query = `UPDATE domain_send_counters
		            SET confirmed_count = confirmed_count + $4
		          WHERE user_id=$1 AND domain=$2 AND day=$3
		      RETURNING confirmed_count, daily_limit`
	case "released":
		// A locally inferred failure can be corrected by later authoritative
		// provider evidence. Release returned the units to the daily counter,
		// so confirming that real send must restore consumed as well as
		// accepted volume. The reservation row lock makes this idempotent.
		query = `UPDATE domain_send_counters
		            SET reserved_count = reserved_count + $4,
		                confirmed_count = confirmed_count + $4
		          WHERE user_id=$1 AND domain=$2 AND day=$3
		      RETURNING confirmed_count, daily_limit`
	default:
		return permanentf("sendramp: invalid reservation state %q", state)
	}

	var confirmed, limit int
	err = tx.QueryRow(ctx, query, userID, scope, utcDay(day), units).Scan(&confirmed, &limit)
	if errors.Is(err, pgx.ErrNoRows) && state == "released" {
		// The day's counter row is gone. Maintenance deletes counters older
		// than 35 days whose reservation is no longer `reserved`, and a
		// reservation released long after its own day outlives its counter by
		// exactly that window. There is nothing left to restore and nothing
		// left to qualify, so a late correction for that day is a no-op rather
		// than an error that would retry forever. The `reserved` branch is
		// deliberately NOT forgiven the same way: maintenance never reaps a
		// counter that still has a reserved reservation, so a missing row there
		// is a real inconsistency.
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sending_ramp_reservations SET state='confirmed', updated_at=now() WHERE message_id=$1`,
		messageID,
	); err != nil {
		return err
	}
	if Qualifies(confirmed, limit) {
		if _, err := tx.Exec(ctx, `
			UPDATE sending_ramp_scopes
			   SET active_days = active_days + 1, last_qualified_day = $3
			 WHERE user_id=$1 AND domain=$2
			   AND (last_qualified_day IS NULL OR last_qualified_day < $3)`,
			userID, scope, utcDay(day),
		); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseTx returns a still-reserved message's ramp units inside a caller's
// transaction.
//
// It skips the scope key, which it never writes. Skipping is permitted; what is
// not permitted is taking the reservation before a key that comes earlier, and
// this takes only the reservation and then the counter — a suffix of the
// normative order.
func ReleaseTx(ctx context.Context, tx pgx.Tx, messageID string) error {
	if messageID == "" {
		return permanentf("sendramp: empty message id")
	}
	var day time.Time
	var userID, scope, state string
	var units int
	err := tx.QueryRow(ctx, `
		SELECT day, user_id, domain, units, state
		  FROM sending_ramp_reservations WHERE message_id = $1 FOR UPDATE`, messageID,
	).Scan(&day, &userID, &scope, &units, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state != "reserved" {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE domain_send_counters SET reserved_count = reserved_count - $4
		 WHERE user_id=$1 AND domain=$2 AND day=$3 AND reserved_count >= $4`,
		userID, scope, utcDay(day), units)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// The guarded decrement matched nothing, so no units came back — and
		// the reservation must not claim they did. ConfirmTx's
		// released→confirmed restoration adds `units` BACK to this counter when
		// late provider evidence corrects a locally inferred failure, so a
		// reservation that says "released" without having returned anything
		// lets that correction mint capacity out of nothing.
		//
		// Two shapes reach here and they are not the same. If the counter row
		// is gone the release is honest — there is nothing to give back and
		// nothing to restore either, because the restoration targets the same
		// missing row. If the row is present but holds fewer reserved units
		// than this reservation claims, the ledger and the reservation already
		// disagree; papering over that with a state write is what turns one
		// inconsistency into free capacity, so it fails permanently instead.
		var exists bool
		err := tx.QueryRow(ctx,
			`SELECT true FROM domain_send_counters WHERE user_id=$1 AND domain=$2 AND day=$3`,
			userID, scope, utcDay(day),
		).Scan(&exists)
		switch {
		case err == nil:
			return permanentf("sendramp: counter holds fewer than the %d reserved units of message %s", units, messageID)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
	}
	_, err = tx.Exec(ctx,
		`UPDATE sending_ramp_reservations SET state='released', updated_at=now() WHERE message_id=$1`,
		messageID)
	return err
}

// ScopeState is the ramp's answer to "has this domain proved itself yet".
type ScopeState struct {
	// Status is the domain's ramp status: inactive, ramping, complete, exempt.
	Status string
	// ActiveDays is how many UTC days reached the qualifying accepted volume.
	ActiveDays int
	// Established reports whether the scope has left probation.
	Established bool
}

// InspectScopeTx classifies a domain without locking or writing anything.
//
// The unlocked read is deliberate and safe in the only direction that matters.
// Ramp progress is monotonic — a scope goes inactive → ramping → qualified →
// complete and never regresses — so a stale read can only be stale in the
// STRICT direction, reporting probation for a scope that has just graduated.
// Charging the probation pool for one extra send is harmless; the reverse
// would not be, and cannot happen.
//
// This matters because the probation classification decides which budget
// counters a transaction must lock, and the budget counters come BEFORE the
// ramp keys in the normative order. Something has to be read before the ramp
// lock is taken, and monotonicity is what makes that sound.
//
// The sending identity below is the one input that is NOT monotonic — a domain
// can lose verification as well as gain it — so a stale read there could report
// established for an identity that has just gone unverified. That costs nothing
// this argument needs: ReserveTx re-reads the same column under the domain row
// lock and refuses the send outright, so the only consequence of the stale
// classification is which pool the refused attempt briefly charged.
func InspectScopeTx(ctx context.Context, tx pgx.Tx, userID, domain string) (ScopeState, error) {
	var state ScopeState
	var sendingStatus string
	err := tx.QueryRow(ctx,
		`SELECT sending_ramp_status, sending_status FROM domains WHERE domain = $1 AND user_id = $2`,
		domain, userID,
	).Scan(&state.Status, &sendingStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		// No domain row means no customer-controlled identity, so nothing here
		// has been proven. Probation is the answer.
		state.Status = StatusInactive
		return state, nil
	}
	if err != nil {
		return ScopeState{}, err
	}

	// Legacy exemption and completed ramps are established from the start —
	// they are the two states that mean "this domain already earned its
	// volume".
	if state.Status == StatusExempt || state.Status == StatusComplete {
		state.Established = true
		return state, nil
	}

	// Qualified days belong to the scope, but they vouch for the identity that
	// earned them. A rebind onto a child subdomain whose SES identity was never
	// verified inherits the parent's progress without inheriting anything that
	// proved it, so an unverified identity is probationary however old its
	// scope is. Reserve refuses that send outright; this keeps the class honest
	// for the early hold, which is decided before the ramp is consulted and is
	// the only thing bounding the probation pool at that point.
	if sendingStatus != "verified" {
		return state, nil
	}

	var scopeStatus string
	err = tx.QueryRow(ctx,
		`SELECT status, active_days FROM sending_ramp_scopes WHERE user_id = $1 AND domain = $2`,
		userID, registrableDomain(domain),
	).Scan(&scopeStatus, &state.ActiveDays)
	if errors.Is(err, pgx.ErrNoRows) {
		// Ramping was stamped on the domain but the scope has not armed yet:
		// day zero, still probationary.
		return state, nil
	}
	if err != nil {
		return ScopeState{}, err
	}
	if scopeStatus == StatusComplete {
		state.Status = StatusComplete
		state.Established = true
		return state, nil
	}
	// One qualified day is the bar. Day zero is the first 150-recipient stage
	// and has proved nothing yet; from day one the account/registrable-domain
	// scope is established for classification even though its ramp keeps
	// enforcing 213, 277, and onward.
	state.Established = state.ActiveDays >= 1
	return state, nil
}
