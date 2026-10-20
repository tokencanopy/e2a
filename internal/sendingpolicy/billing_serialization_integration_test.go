package sendingpolicy_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// The ops repo separately drives the real billing writer. Keep this canonical
// SQL proof here so the repositories do not import each other's implementation.
func billingControlLock(ctx context.Context, tx pgx.Tx, user string) error {
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR SHARE`, user).Scan(&id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO account_sending_controls(user_id) VALUES($1) ON CONFLICT(user_id) DO UPDATE SET user_id=EXCLUDED.user_id`, user)
	return err
}
func billingPlanWrite(ctx context.Context, tx pgx.Tx, user, plan string) error {
	_, err := tx.Exec(ctx, `INSERT INTO account_limits(user_id,plan_code,max_agents,max_domains,max_messages_month,max_storage_bytes) VALUES($1,$2,100,100,1000000,1000000000) ON CONFLICT(user_id) DO UPDATE SET plan_code=EXCLUDED.plan_code`, user, plan)
	return err
}

// Observe an actual PostgreSQL lock wait, not a scheduling delay. All tests
// use their workspace/package-isolated database, so another suite cannot match.
func waitForBlockedBackend(t *testing.T, ctx context.Context, pool *pgxpool.Pool, blocker int32) int32 {
	t.Helper()
	for ctx.Err() == nil {
		var pid int32
		err := pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1`, blocker).Scan(&pid)
		if err == nil {
			return pid
		}
		if err != pgx.ErrNoRows {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected PostgreSQL lock wait was not observed")
	return 0
}

type consumeResult struct {
	decision sendingpolicy.Decision
	err      error
}

func TestBillingCommitSerializesBeforeConsume(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-plan-%v", missing), func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
			defer cancel()
			off := f.gate(sendingpolicy.DisabledPolicy())
			enforce := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) { p.DefaultAccountDailyRecipients = 1 }))
			user := f.user("standard")
			if !missing {
				f.plan(user, "pro")
			}
			agent := f.agent(user)
			_, ref := f.prepareMessage(off, f.message(agent, "relay", 2))
			_, attempt, err := off.Reserve(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			billing, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer billing.Rollback(ctx)
			var pid int32
			if err = billing.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			if err = billingControlLock(ctx, billing, user); err != nil {
				t.Fatal(err)
			}
			plan := "free"
			if missing {
				plan = "pro"
			}
			if err = billingPlanWrite(ctx, billing, user, plan); err != nil {
				t.Fatal(err)
			}
			result := make(chan consumeResult, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				d, _, e := enforce.ConsumeAttempt(ctx, attempt)
				result <- consumeResult{d, e}
			}()
			defer func() { cancel(); <-done }()
			waitForBlockedBackend(t, ctx, f.pool, pid)
			if err = billing.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-result
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.decision.Allow != missing {
				t.Fatalf("committed plan %s: %+v", plan, got.decision)
			}
			if !missing && got.decision.Reason != sendingpolicy.ReasonAccountDailyBudget {
				t.Fatalf("downgrade reason: %+v", got.decision)
			}
		})
	}
}

func TestConsumeSerializesBeforeBillingCommit(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	off := f.gate(sendingpolicy.DisabledPolicy())
	enforce := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) { p.DefaultAccountDailyRecipients = 1 }))
	user := f.user("standard")
	f.plan(user, "pro")
	agent := f.agent(user)
	_, ref := f.prepareMessage(off, f.message(agent, "relay", 2))
	_, attempt, err := off.Reserve(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	// Consume locks the control and reads limits before locking its operation.
	// Holding the later lock gives us a deterministic pause inside real Consume.
	barrier, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(ctx)
	var barrierPID int32
	if err = barrier.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&barrierPID); err != nil {
		t.Fatal(err)
	}
	if _, err = barrier.Exec(ctx, `SELECT operation_id FROM sending_provider_operations WHERE operation_id=$1 FOR UPDATE`, attempt.OperationID()); err != nil {
		t.Fatal(err)
	}
	consumed := make(chan consumeResult, 1)
	consumeDone := make(chan struct{})
	go func() {
		defer close(consumeDone)
		d, _, e := enforce.ConsumeAttempt(ctx, attempt)
		consumed <- consumeResult{d, e}
	}()
	defer func() { cancel(); <-consumeDone }()
	consumePID := waitForBlockedBackend(t, ctx, f.pool, barrierPID)
	billing, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	billingOwnedByWorker := false
	defer func() {
		if !billingOwnedByWorker {
			cleanup, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_ = billing.Rollback(cleanup)
		}
	}()
	var billingPID int32
	if err = billing.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&billingPID); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	billingDone := make(chan struct{})
	billingOwnedByWorker = true
	go func() {
		defer close(billingDone)
		defer func() {
			cleanup, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_ = billing.Rollback(cleanup)
		}()
		if e := billingControlLock(ctx, billing, user); e != nil {
			written <- e
			return
		}
		if e := billingPlanWrite(ctx, billing, user, "free"); e != nil {
			written <- e
			return
		}
		written <- billing.Commit(ctx)
	}()
	defer func() { cancel(); <-billingDone }()
	if got := waitForBlockedBackend(t, ctx, f.pool, consumePID); got != billingPID {
		t.Fatalf("blocked pid=%d want billing %d", got, billingPID)
	}
	var waitingQuery string
	if err = f.pool.QueryRow(ctx, `SELECT query FROM pg_stat_activity WHERE pid=$1`, billingPID).Scan(&waitingQuery); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(waitingQuery, "INSERT INTO account_sending_controls") {
		t.Fatalf("billing blocked after the control boundary: %s", waitingQuery)
	}
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-consumed
	if got.err != nil || !got.decision.Allow {
		t.Fatalf("authorization must use preceding paid plan: %+v %v", got.decision, got.err)
	}
	if err = <-written; err != nil {
		t.Fatal(err)
	}
	var plan string
	if err = f.pool.QueryRow(ctx, `SELECT plan_code FROM account_limits WHERE user_id=$1`, user).Scan(&plan); err != nil || plan != "free" {
		t.Fatalf("downgrade commit: %s %v", plan, err)
	}
}
