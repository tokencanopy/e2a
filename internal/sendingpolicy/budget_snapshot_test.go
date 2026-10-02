package sendingpolicy_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

func TestBudgetSnapshotUsesCurrentLimitsAndUTCDate(t *testing.T) {
	f := newFixture(t)
	p := enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) { p.AllCustomerGlobalDailyRecipients = 10 })
	m := f.gate(p).(*sendingpolicy.Module)
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	m.WithClock(func() time.Time { return day })
	_, err := f.pool.Exec(f.ctx, `INSERT INTO sending_budget_counters(scope,scope_id,day,daily_limit,reserved_count,confirmed_count) VALUES ('global_all','all-customers',$1,999,15,10),('global_all','all-customers',$1::date-1,999,500,500),('account_daily','usr_synthetic',$1,20,20,20)`, day)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.BudgetSnapshot(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedRatio["global_all"] != 1.5 || len(got.UsedRatio) != 4 {
		t.Fatalf("snapshot: %+v", got)
	}
	if got.UsedRatio["global_violation"] != 0 {
		t.Fatal("missing scope did not zero-fill")
	}
	m.WithClock(func() time.Time { return day.Add(24 * time.Hour) })
	got, err = m.BudgetSnapshot(f.ctx)
	if err != nil || got.UsedRatio["global_all"] != 0 {
		t.Fatalf("rollover: %+v %v", got, err)
	}
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err = m.BudgetSnapshot(cancelled); err == nil {
		t.Fatal("canceled snapshot succeeded")
	}
}

func TestBudgetSnapshotDatabasePolicyAndCorruption(t *testing.T) {
	f := newFixture(t)
	config := sendingpolicy.DisabledPolicy()
	m := sendingpolicy.NewGate(f.pool, f.secrets(), sendingpolicy.PolicySourceDatabase, config).(*sendingpolicy.Module)
	p := config
	p.AllCustomerGlobalDailyRecipients = 17
	raw, err := sendingpolicy.CanonicalBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := sendingpolicy.Hash(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(f.ctx, `UPDATE sending_protection_runtime_policy SET generation=7,policy=$1,policy_sha256=$2 WHERE singleton`, raw, hash); err != nil {
		t.Fatal(err)
	}
	got, err := m.BudgetSnapshot(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 7 || !got.ConfigMismatch {
		t.Fatalf("database policy snapshot: %+v", got)
	}
	if _, err = f.pool.Exec(f.ctx, `UPDATE sending_protection_runtime_policy SET policy_sha256=repeat('0',64) WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if _, err = m.BudgetSnapshot(f.ctx); err == nil {
		t.Fatal("corrupt policy accepted")
	}
}
