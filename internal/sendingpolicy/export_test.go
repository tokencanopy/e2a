package sendingpolicy

import (
	"context"
	"time"
)

// LedgerTuning is the test-only batch geometry and fault hook for GCLedger.
type LedgerTuning struct {
	BatchSize  int
	MaxBatches int
	// BeforeBatch runs inside each batch transaction, after its timeouts are
	// set and before the delete; exec runs SQL in that transaction.
	BeforeBatch func(ctx context.Context, table string, exec func(context.Context, string) error) error
}

// GCLedgerTuned runs GCLedger with a test batch geometry.
func (m *Module) GCLedgerTuned(ctx context.Context, now time.Time, t LedgerTuning) (LedgerRetentionStats, error) {
	return m.gcLedger(ctx, now, ledgerRetentionTuning{batchSize: t.BatchSize, maxBatches: t.MaxBatches, beforeBatch: t.BeforeBatch})
}

// LedgerBatchSize exposes the production batch size.
const LedgerBatchSize = ledgerBatchSize
