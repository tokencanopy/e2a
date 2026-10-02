package sendingpolicy_test

import (
	"testing"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

func TestBudgetObservationShadowRecordsEveryExceededScope(t *testing.T) {
	f := newFixture(t)
	samples := map[string]int{}
	sendingpolicy.SetBudgetObserver(func(scope, decision string) { samples[scope+"/"+decision]++ })
	t.Cleanup(func() { sendingpolicy.SetBudgetObserver(nil) })
	p := enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeShadow
		p.AllCustomerGlobalDailyRecipients = 1
		p.DefaultAccountDailyRecipients = 1
		p.SharedDomainAccountDailyRecip = 1
		p.ProbationGlobalDailyRecipients = 1
	})
	g := f.gate(p)
	user := f.user("standard")
	agent := f.agent(user)
	for i := 0; i < 3; i++ {
		if d := f.send(g, f.message(agent, "relay", 1)); !d.Allow {
			t.Fatal(d)
		}
	}
	for _, scope := range []string{"global_all", "account_daily", "account_shared_daily", "global_probation"} {
		if samples[scope+"/allow"] != 1 || samples[scope+"/would_hold"] != 2 {
			t.Fatalf("samples: %v", samples)
		}
	}
	if len(samples) != 8 {
		t.Fatalf("unexpected observations: %v", samples)
	}
}

func TestBudgetObservationEarlyHoldAndDisabled(t *testing.T) {
	f := newFixture(t)
	samples := map[string]int{}
	sendingpolicy.SetBudgetObserver(func(scope, decision string) { samples[scope+"/"+decision]++ })
	t.Cleanup(func() { sendingpolicy.SetBudgetObserver(nil) })
	user := f.user("standard")
	agent := f.agent(user)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) { p.AllCustomerGlobalDailyRecipients = 1 }))
	_, ref := f.prepareMessage(g, f.message(agent, "relay", 2))
	d, _, err := g.Reserve(f.ctx, ref)
	if err != nil || d.Allow {
		t.Fatalf("reserve: %+v %v", d, err)
	}
	if len(samples) != 1 || samples["global_all/hold"] != 1 {
		t.Fatalf("early hold: %v", samples)
	}
	if d := f.send(f.gate(sendingpolicy.DisabledPolicy()), f.message(agent, "relay", 1)); !d.Allow {
		t.Fatal(d)
	}
	if len(samples) != 1 {
		t.Fatalf("disabled emitted: %v", samples)
	}
}

func TestBudgetObservationDoesNotPublishRolledBackHold(t *testing.T) {
	f := newFixture(t)
	count := 0
	sendingpolicy.SetBudgetObserver(func(string, string) { count++ })
	t.Cleanup(func() { sendingpolicy.SetBudgetObserver(nil) })
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) { p.AllCustomerGlobalDailyRecipients = 1 }))
	_, ref := f.prepareMessage(g, f.message(f.agent(f.user("standard")), "relay", 2))
	_, err := f.pool.Exec(f.ctx, `CREATE FUNCTION reject_budget_metric_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic commit rejection'; END $$;
 CREATE CONSTRAINT TRIGGER reject_budget_metric_commit AFTER INSERT OR UPDATE ON sending_budget_reservations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_budget_metric_commit()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(f.ctx, `DROP TRIGGER IF EXISTS reject_budget_metric_commit ON sending_budget_reservations; DROP FUNCTION IF EXISTS reject_budget_metric_commit()`)
	})
	if _, _, err = g.Reserve(f.ctx, ref); err == nil {
		t.Fatal("expected commit failure")
	}
	if count != 0 {
		t.Fatalf("rolled-back hold emitted %d samples", count)
	}
}

func TestBudgetObservationAccountTrustWithLegacyBudgetsDisabled(t *testing.T) {
	f := newFixture(t)
	samples := map[string]int{}
	sendingpolicy.SetBudgetObserver(func(scope, decision string) { samples[scope+"/"+decision]++ })
	t.Cleanup(func() { sendingpolicy.SetBudgetObserver(nil) })
	p := sendingpolicy.DisabledPolicy()
	p.AccountTrustEnabled = true
	p.DisableLegacyDailyBudgets = true
	g := f.gate(p)
	user := f.user("standard")
	f.plan(user, "scale")
	agent := f.agent(user)
	if d := f.send(g, f.message(agent, "relay", 20)); !d.Allow {
		t.Fatal(d)
	}
	if d := f.send(g, f.message(agent, "relay", 1)); d.Allow {
		t.Fatal("expected trust hold")
	}
	if samples["account_daily/allow"] != 1 || samples["account_shared_daily/allow"] != 1 || samples["account_daily/hold"] != 1 {
		t.Fatalf("trust samples: %v", samples)
	}
	if len(samples) != 3 {
		t.Fatalf("disabled platform budgets emitted: %v", samples)
	}
}

func TestBudgetObservationDoesNotPublishFailedConsume(t *testing.T) {
	f := newFixture(t)
	count := 0
	sendingpolicy.SetBudgetObserver(func(string, string) { count++ })
	t.Cleanup(func() { sendingpolicy.SetBudgetObserver(nil) })
	off := f.gate(sendingpolicy.DisabledPolicy())
	_, ref := f.prepareMessage(off, f.message(f.agent(f.user("standard")), "relay", 1))
	_, attempt, err := off.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.pool.Exec(f.ctx, `CREATE FUNCTION reject_budget_consume_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic consume commit rejection'; END $$;
 CREATE CONSTRAINT TRIGGER reject_budget_consume_commit AFTER UPDATE ON sending_budget_reservations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_budget_consume_commit()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(f.ctx, `DROP TRIGGER IF EXISTS reject_budget_consume_commit ON sending_budget_reservations; DROP FUNCTION IF EXISTS reject_budget_consume_commit()`)
	})
	if _, _, err = f.gate(enforcingPolicy(nil)).ConsumeAttempt(f.ctx, attempt); err == nil {
		t.Fatal("expected commit failure")
	}
	if count != 0 {
		t.Fatalf("failed consume emitted %d samples", count)
	}
}
