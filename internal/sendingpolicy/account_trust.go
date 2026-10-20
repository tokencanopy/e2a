package sendingpolicy

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/sendramp"
)

type accountCapacityError struct{ daily sendramp.AccountDailyLimit }

func (e *accountCapacityError) Error() string { return errRampCapacity.Error() }
func (e *accountCapacityError) Unwrap() error { return errRampCapacity }

// externalRecipientCount shares the admission gate's live-agent and verified
// owner definitions, independently of approval status or available unlocks.
func externalRecipientCount(ctx context.Context, q dbQuerier, user string, envelope []string) (int, error) {
	facts, err := loadAccountAccessFacts(ctx, q, user)
	if err != nil {
		return 0, err
	}
	own, err := ownAgentRecipients(ctx, q, user, envelope)
	if err != nil {
		return 0, err
	}
	seen := make(map[string]bool)
	n := 0
	for _, raw := range envelope {
		addr := NormalizeOwnerMailbox(raw)
		if seen[addr] {
			continue
		}
		seen[addr] = true
		if _, ok := own[addr]; ok {
			continue
		}
		if facts.ownerRecipientVerified() && addr == facts.proofAddress {
			continue
		}
		n++
	}
	return n, nil
}

func accountPlanCap(ctx context.Context, q dbQuerier, user string) (*int, error) {
	var cap *int
	err := q.QueryRow(ctx, `SELECT max_messages_day FROM account_limits WHERE user_id=$1`, user).Scan(&cap)
	if errors.Is(err, pgx.ErrNoRows) {
		v := 20
		return &v, nil
	}
	return cap, err
}

func (m *Module) accountTrustSubject(ctx context.Context, tx pgx.Tx, op operationRow) (rampSubject, error) {
	if op.Purpose != PurposeCustomerMessage {
		return rampSubject{}, nil
	}
	facts, err := loadAccountAccessFacts(ctx, tx, op.accountRef())
	if err != nil {
		return rampSubject{}, err
	}
	if accountClassExempt(facts.class) {
		return rampSubject{}, nil
	}
	envelope, err := messageEnvelopeQ(ctx, tx, op.OperationID)
	if err != nil {
		return rampSubject{}, err
	}
	n, err := externalRecipientCount(ctx, tx, op.accountRef(), envelope)
	if err != nil {
		return rampSubject{}, err
	}
	if n == 0 {
		return rampSubject{}, nil
	}
	if !op.Shared {
		agent, _, err := messageSender(ctx, tx, op.OperationID)
		if err != nil {
			return rampSubject{}, err
		}
		verified, err := customIdentityVerified(ctx, tx, op.accountRef(), agent)
		if err != nil {
			return rampSubject{}, err
		}
		if !verified {
			return rampSubject{}, errRampIdentityUnverified
		}
	}
	cap, err := accountPlanCap(ctx, tx, op.accountRef())
	if err != nil {
		return rampSubject{}, err
	}
	return rampSubject{messageID: op.OperationID, userID: op.accountRef(), units: n, applies: true, account: true, shared: op.Shared, planCap: cap}, nil
}

// AccountTrustEnabled is queried at runtime, including database-policy changes.
func (m *Module) AccountTrustEnabled(ctx context.Context) (bool, error) {
	p, err := m.policyForRead(ctx, m.pool)
	return p.AccountTrustEnabled, err
}

// AccountDailyLimit returns nil when the opt-in control is absent. The read
// transaction gives the account response a coherent plan/usage snapshot.
func (m *Module) AccountDailyLimit(ctx context.Context, user string) (*sendramp.AccountDailyLimit, error) {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	p, err := m.policyForRead(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !p.AccountTrustEnabled {
		return nil, nil
	}
	facts, err := loadAccountAccessFacts(ctx, tx, user)
	if err != nil {
		return nil, err
	}
	if accountClassExempt(facts.class) {
		return nil, nil
	}
	cap, err := accountPlanCap(ctx, tx, user)
	if err != nil {
		return nil, err
	}
	day, err := ledgerDay(ctx, tx)
	if err != nil {
		return nil, err
	}
	d, err := sendramp.AccountSnapshotTx(ctx, tx, user, day, cap)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// RecheckAccountTrustGrant is a refuse-only read immediately before provider
// I/O. It never takes account locks after the operation lock, avoiding a lock
// inversion with authorization. A stale classification must obtain a new grant.
func (m *Module) recheckAccountTrustGrant(ctx context.Context, tx pgx.Tx, op operationRow, stored reservationRow) (bool, error) {
	facts, err := loadAccountAccessFacts(ctx, tx, op.accountRef())
	if err != nil {
		return false, err
	}
	if accountClassExempt(facts.class) {
		return true, nil
	}
	envelope, err := messageEnvelopeQ(ctx, tx, op.OperationID)
	if err != nil {
		return false, err
	}
	n, err := externalRecipientCount(ctx, tx, op.accountRef(), envelope)
	if err != nil {
		return false, err
	}
	if stored.AccountTrustUnits == nil || n != *stored.AccountTrustUnits {
		return false, nil
	}
	if n == 0 {
		return true, nil
	}
	if !op.Shared {
		agent, _, err := messageSender(ctx, tx, op.OperationID)
		if err != nil {
			return false, err
		}
		ok, err := customIdentityVerified(ctx, tx, op.accountRef(), agent)
		if err != nil || !ok {
			return false, err
		}
	}
	cap, err := accountPlanCap(ctx, tx, op.accountRef())
	if err != nil {
		return false, err
	}
	now, err := ledgerDay(ctx, tx)
	if err != nil {
		return false, err
	}
	d, err := sendramp.AccountSnapshotTx(ctx, tx, op.accountRef(), now, cap)
	if err != nil {
		return false, err
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_send_reservations WHERE message_id=$1 AND user_id=$2 AND day=$3 AND units >= $4 AND shared=$5 AND state='reserved')`, op.OperationID, op.accountRef(), now, n, op.Shared).Scan(&valid)
	return valid && d.Used <= d.Limit && (!op.Shared || d.SharedUsed <= d.SharedLimit), err
}

// DailyLimitPreflight gives immediate sends a useful refusal before persistence.
// It does not reserve capacity: final authorization repeats the decision under
// account locks, so concurrent preflights cannot overrun the allowance.
func (m *Module) DailyLimitPreflight(ctx context.Context, user, agent string, recipients []string) (*sendramp.AccountDailyLimit, error) {
	enabled, err := m.AccountTrustEnabled(ctx)
	if err != nil || !enabled {
		return nil, err
	}
	envelope, err := normalizeEnvelope(recipients)
	if err != nil {
		return nil, err
	}
	n, err := externalRecipientCount(ctx, m.pool, user, envelope)
	if err != nil || n == 0 {
		return nil, err
	}
	facts, err := loadAccountAccessFacts(ctx, m.pool, user)
	if err != nil {
		return nil, err
	}
	if accountClassExempt(facts.class) {
		return nil, nil
	}
	d, err := m.AccountDailyLimit(ctx, user)
	if err != nil || d == nil {
		return d, err
	}
	own, err := customIdentityVerified(ctx, m.pool, user, agent)
	if err != nil {
		return nil, err
	}
	d.SharedBinding = !own && d.SharedLimit-d.SharedUsed < d.Limit-d.Used
	d.Allowed = n <= d.Limit-d.Used && (own || n <= d.SharedLimit-d.SharedUsed)
	return d, nil
}
