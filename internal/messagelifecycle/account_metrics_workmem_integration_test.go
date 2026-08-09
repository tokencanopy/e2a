//go:build integration

package messagelifecycle_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// TestAccountMetricsWorkMemScopedToTx pins the two properties the account
// aggregates rely on: the raised work_mem is accepted inside the read-only
// repeatable-read transaction they run in, and SET LOCAL reverts on rollback
// so the raised value cannot leak onto the pooled connection the next
// checkout gets.
func TestAccountMetricsWorkMemScopedToTx(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	var baseline string
	if err := pool.QueryRow(ctx, "SHOW work_mem").Scan(&baseline); err != nil {
		t.Fatalf("show baseline work_mem: %v", err)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	if err := messagelifecycle.SetAccountMetricsWorkMemForTest(ctx, tx); err != nil {
		t.Fatalf("SetAccountMetricsWorkMemForTest: %v", err)
	}
	var inside string
	if err := tx.QueryRow(ctx, "SHOW work_mem").Scan(&inside); err != nil {
		t.Fatalf("show work_mem inside tx: %v", err)
	}
	if inside != messagelifecycle.AccountMetricsWorkMemForTest {
		t.Errorf("work_mem inside tx = %q, want %q", inside, messagelifecycle.AccountMetricsWorkMemForTest)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var after string
	if err := pool.QueryRow(ctx, "SHOW work_mem").Scan(&after); err != nil {
		t.Fatalf("show work_mem after rollback: %v", err)
	}
	if after != baseline {
		t.Errorf("work_mem after rollback = %q, want the pre-tx %q: SET LOCAL leaked onto a pooled connection", after, baseline)
	}
}
