package usage

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5"
)

type TransactionalUsageTracker interface {
	// RecordAndCheckTx records one usage event of `units` recipient-deliveries
	// inside the caller's transaction. Units < 1 are normalized to 1.
	RecordAndCheckTx(ctx context.Context, tx pgx.Tx, userID, agentID, domain, direction string, units int) (bool, error)
}

// UsageTracker records usage events. Always allows the action (no quota enforcement).
type UsageTracker interface {
	// RecordAndCheck records a usage event of `units` recipient-deliveries.
	// Always returns true. Units < 1 are normalized to 1.
	RecordAndCheck(ctx context.Context, userID, agentID, domain, direction string, units int) (allowed bool, err error)
}

// LiveUsageTracker is the real implementation backed by the billing store.
type LiveUsageTracker struct {
	store *Store
}

func NewUsageTracker(store *Store) *LiveUsageTracker {
	return &LiveUsageTracker{store: store}
}

func (t *LiveUsageTracker) RecordAndCheck(ctx context.Context, userID, agentID, domain, direction string, units int) (bool, error) {
	// Metering gate: resolve the account class once and short-circuit
	// non-standard accounts (internal/system/demo) BEFORE any write, so probe
	// and internal traffic never lands in usage_events/usage_summaries and thus
	// never counts against quota. On a lookup error we fail toward metering
	// (GetAccountClass returns ClassStandard) — a real customer is never
	// silently exempted from billing.
	class, err := t.store.GetAccountClass(ctx, userID)
	if err != nil {
		log.Printf("[billing] account class lookup failed for user %s, metering as standard: %v", userID, err)
	}
	if !PolicyFor(class).Meter {
		return true, nil
	}

	// Record the event
	event := &UsageEvent{
		UserID:    userID,
		AgentID:   agentID,
		Domain:    domain,
		Direction: direction,
		Units:     units,
	}
	if err := t.store.RecordUsageEvent(ctx, event); err != nil {
		log.Printf("[billing] failed to record usage event: %v", err)
		// Don't block on recording failure
	}

	bucketDate := CurrentDate()
	if err := t.store.IncrementUsageSummary(ctx, userID, bucketDate, direction, units); err != nil {
		log.Printf("[billing] failed to increment usage summary: %v", err)
	}

	return true, nil
}

func (t *LiveUsageTracker) RecordAndCheckTx(ctx context.Context, tx pgx.Tx, userID, agentID, domain, direction string, units int) (bool, error) {
	class, err := t.store.GetAccountClassTx(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	if !PolicyFor(class).Meter {
		return true, nil
	}
	e := &UsageEvent{UserID: userID, AgentID: agentID, Domain: domain, Direction: direction, Units: units}
	if err := t.store.RecordUsageEventTx(ctx, tx, e); err != nil {
		return false, err
	}
	if err := t.store.IncrementUsageSummaryTx(ctx, tx, userID, CurrentDate(), direction, units); err != nil {
		return false, err
	}
	return true, nil
}

// NoopUsageTracker always allows everything. Used when billing is disabled.
type NoopUsageTracker struct{}

func NewNoopUsageTracker() *NoopUsageTracker {
	return &NoopUsageTracker{}
}

func (t *NoopUsageTracker) RecordAndCheck(ctx context.Context, userID, agentID, domain, direction string, units int) (bool, error) {
	return true, nil
}

func (t *NoopUsageTracker) RecordAndCheckTx(context.Context, pgx.Tx, string, string, string, string, int) (bool, error) {
	return true, nil
}
