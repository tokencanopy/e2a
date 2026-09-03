package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/sendramp"
)

// This file composes the custom-domain ramp into final authorization.
//
// The ramp and the sending budget answer different questions. The budget asks
// "has this account, or the platform, already exposed SES enough today?" The
// ramp asks "has this particular domain earned the right to send this much
// yet?" A new custom domain can be well inside its account's daily allowance
// and still be limited to 150 recipients because it has no reputation history;
// an established domain can be far into its ramp and still be held because the
// platform pool is exhausted. Both apply, and the most restrictive wins.
//
// Composition is one-directional: the ramp is private to this package and is
// reached only through Gate. Nothing outside can reserve ramp capacity without
// also passing the budget, which is what stops the two ledgers from being
// satisfied by different callers at different times.

// ErrRampCapacity is the internal marker for a ramp hold. It never escapes to a
// caller — Gate turns it into a Decision — but it keeps "this domain has sent
// enough today" distinct from "the database is unhappy", because the first
// holds until midnight and the second must not be allowed to look like a hold.
var errRampCapacity = errors.New("sendingpolicy: ramp capacity exhausted")

// ReasonRampCapacity is the machine-readable hold reason for a domain that has
// used its ramp allowance for the day.
const ReasonRampCapacity = "sending_ramp_capacity_exhausted"

// rampSubject is everything the ramp needs about one customer message, read
// from the locked source rows.
type rampSubject struct {
	messageID string
	userID    string
	domain    string
	units     int
	// applies is false for every message the ramp does not govern: shared-relay
	// mail, platform test mail, non-message purposes, and every deployment
	// whose policy has the ramp disabled.
	applies bool
}

// rampSubjectFor derives the ramp's view of an operation.
//
// The eligibility rule matches the existing worker's exactly — own-address mail
// that is not platform test mail — because the ramp ledger is message-keyed and
// shared with that worker during the migration. Two different notions of
// "eligible" would let one path reserve capacity the other never released.
func (m *Module) rampSubjectFor(ctx context.Context, tx pgx.Tx, policy RuntimePolicy, op operationRow, units int) (rampSubject, error) {
	if !policy.RampEnabled || op.Purpose != PurposeCustomerMessage || op.Shared {
		return rampSubject{}, nil
	}

	var domain, messageType string
	err := tx.QueryRow(ctx, `
		SELECT agent.registered_domain, COALESCE(message.message_type, '')
		  FROM messages AS message
		  JOIN agent_identities AS agent ON agent.id = message.agent_id
		 WHERE message.id = $1`, op.OperationID,
	).Scan(&domain, &messageType)
	if errors.Is(err, pgx.ErrNoRows) {
		return rampSubject{}, ErrSourceUnavailable
	}
	if err != nil {
		return rampSubject{}, fmt.Errorf("sendingpolicy: read ramp subject: %w", err)
	}
	if messageType == "test" {
		return rampSubject{}, nil
	}

	return rampSubject{
		messageID: op.OperationID,
		userID:    op.accountRef(),
		domain:    domain,
		units:     units,
		applies:   true,
	}, nil
}

// rampProbation reports whether this operation still draws on the shared
// probation pool.
//
// Shared-relay traffic is probationary at every plan level and never graduates
// — it borrows platform reputation, so no amount of age or payment earns it
// higher volume; the way out is a customer-controlled domain. A custom domain
// is probationary until its scope has one qualified day behind it.
//
// When the ramp is disabled the answer is "not probationary", and that is
// correct rather than permissive: with the ramp pass-through there is no
// probation concept for custom domains at all, and the account and platform
// pools still bound the traffic.
func (m *Module) rampProbation(ctx context.Context, tx pgx.Tx, policy RuntimePolicy, op operationRow) (bool, error) {
	if op.Shared {
		return true, nil
	}
	subject, err := m.rampSubjectFor(ctx, tx, policy, op, 1)
	if err != nil {
		return false, err
	}
	if !subject.applies {
		return false, nil
	}
	state, err := sendramp.InspectScopeTx(ctx, tx, subject.userID, subject.domain)
	if err != nil {
		return false, fmt.Errorf("sendingpolicy: inspect ramp scope: %w", err)
	}
	return !state.Established, nil
}

// rampAuthorize acquires the message's ramp capacity, last in the normative
// lock order.
//
// It runs after the budget has been reacquired, so a ramp hold arrives with the
// budget already taken; the caller releases that reservation before returning.
// The order is not negotiable — the budget counters are global and highly
// contended, the ramp keys are per-domain, and taking the narrow keys first
// would let two accounts sharing a registrable domain deadlock against the
// platform pool.
func (m *Module) rampAuthorize(ctx context.Context, tx pgx.Tx, policy RuntimePolicy, subject rampSubject, day time.Time) error {
	if !subject.applies {
		return nil
	}
	decision, err := sendramp.ReserveTx(ctx, tx, sendramp.ReserveRequest{
		MessageID: subject.messageID,
		UserID:    subject.userID,
		Domain:    subject.domain,
		Units:     subject.units,
		Day:       day,
		Schedule: sendramp.Schedule{
			StartDaily:  policy.RampStartDaily,
			TargetDaily: policy.RampTargetDaily,
			RampDays:    policy.RampDays,
		},
	})
	if err != nil {
		return fmt.Errorf("sendingpolicy: reserve ramp capacity: %w", err)
	}
	if !decision.Allowed {
		return errRampCapacity
	}
	return nil
}

// rampRelease returns a message's ramp units for a terminal local cancellation.
//
// Only Cancel does this. A rate deferral deliberately retains the ramp
// reservation: the message was not rejected by anyone, it was merely slowed
// down, and releasing its ramp claim would let the same message re-qualify a
// stage it has already qualified.
func (m *Module) rampRelease(ctx context.Context, tx pgx.Tx, messageID string) error {
	if err := sendramp.ReleaseTx(ctx, tx, messageID); err != nil {
		return fmt.Errorf("sendingpolicy: release ramp capacity: %w", err)
	}
	return nil
}

// rampSettle applies an authoritative provider outcome to the ramp ledger.
//
// Acceptance is the only thing that advances a ramp day, because progress has
// to measure delivered volume rather than attempts — otherwise a domain could
// age into full allowance by failing repeatedly. A definite permanent rejection
// gives the units back. Retryable and ambiguous results are deliberately absent
// from the closed outcome set and leave the reservation standing: a message
// that might have been delivered must not release ramp capacity.
func (m *Module) rampSettle(ctx context.Context, tx pgx.Tx, messageID string, outcome SettlementOutcome) error {
	switch outcome {
	case SettlementProviderAccepted:
		if err := sendramp.ConfirmTx(ctx, tx, messageID); err != nil {
			return fmt.Errorf("sendingpolicy: confirm ramp capacity: %w", err)
		}
	case SettlementProviderPermanentlyRejected:
		if err := sendramp.ReleaseTx(ctx, tx, messageID); err != nil {
			return fmt.Errorf("sendingpolicy: release ramp capacity: %w", err)
		}
	}
	return nil
}
