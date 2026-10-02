package sendingpolicy

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// BudgetSnapshot is a bounded, identifier-free view of the four global pools.
// Reserved counts include confirmed units. Ratios deliberately exceed 1 in
// shadow mode. No account-level rows are read or exposed.
type BudgetSnapshot struct {
	UsedRatio      map[string]float64
	Generation     int64
	ConfigMismatch bool
	ObservedAt     time.Time
}

// BudgetSnapshot reads policy and counters in one consistent read-only snapshot.
// Config source reports generation 0 and no mismatch: it has no stored-policy
// authority. A failed sample returns no usable observation.
func (m *Module) BudgetSnapshot(ctx context.Context) (BudgetSnapshot, error) {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return BudgetSnapshot{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	now := m.now().UTC()
	out := BudgetSnapshot{UsedRatio: map[string]float64{}, ObservedAt: now}
	policy := m.configPolicy
	if m.source == PolicySourceDatabase {
		snapshot, err := scanPolicy(tx.QueryRow(ctx, policySelect))
		if err != nil {
			return BudgetSnapshot{}, err
		}
		policy = snapshot.Policy
		out.Generation = snapshot.Generation
		hash, err := Hash(m.configPolicy)
		if err != nil {
			return BudgetSnapshot{}, err
		}
		out.ConfigMismatch = hash != snapshot.PolicySHA256
	}
	keys := []counterKey{{ScopeGlobalAll, scopeIDAllCustomers}, {ScopeGlobalProbation, scopeIDProbation}, {ScopeGlobalCritical, scopeIDCritical}, {ScopeGlobalViolation, scopeIDViolation}}
	// Four indexed point reads, independent of the number of accounts or old days.
	for _, key := range keys {
		var used int64
		err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT reserved_count FROM sending_budget_counters WHERE scope=$1 AND scope_id=$2 AND day=$3),0)`, key.Scope, key.ScopeID, now.Truncate(24*time.Hour)).Scan(&used)
		if err != nil {
			return BudgetSnapshot{}, err
		}
		limit := limitFor(key, policy, "")
		ratio := float64(0)
		if observeBudgetScope(policy, key.Scope) && limit > 0 {
			ratio = float64(used) / float64(limit)
		}
		out.UsedRatio[string(key.Scope)] = ratio
	}
	if err := tx.Commit(ctx); err != nil {
		return BudgetSnapshot{}, fmt.Errorf("sendingpolicy: commit budget snapshot: %w", err)
	}
	return out, nil
}
