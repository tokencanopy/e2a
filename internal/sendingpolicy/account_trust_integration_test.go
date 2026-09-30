package sendingpolicy_test

import (
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAccountTrustAcrossSharedAndOwnIdentities(t *testing.T) {
	f := newFixture(t)
	p := sendingpolicy.DisabledPolicy()
	p.AccountTrustEnabled = true
	g := f.gate(p)
	user := f.user("standard")
	f.plan(user, "scale")
	shared := f.agent(user)
	own, _ := f.customDomainAgent(user)
	if d := f.send(g, f.message(shared, "relay", 12)); !d.Allow {
		t.Fatalf("shared: %+v", d)
	}
	if d := f.send(g, f.message(own, "own_address", 8)); !d.Allow {
		t.Fatalf("own: %+v", d)
	}
	if d := f.send(g, f.message(own, "own_address", 1)); d.Allow || d.Reason != sendingpolicy.ReasonRampCapacity {
		t.Fatalf("account cap: %+v", d)
	}
}

func TestAccountTrustInternalRecipientsDoNotUseDailyOrPlatformCapacity(t *testing.T) {
	f := newFixture(t)
	p := sendingpolicy.DisabledPolicy()
	p.AccountTrustEnabled = true
	p.DisableLegacyDailyBudgets = true
	p.BudgetMode = sendingpolicy.ModeEnforce
	p.AllCustomerGlobalDailyRecipients = 1
	g := f.gate(p)
	user := f.user("standard")
	f.plan(user, "free")
	sender := f.agent(user)
	recipient := "inside@agents.localhost"
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO domains(domain,user_id) VALUES('agents.localhost',$1) ON CONFLICT DO NOTHING`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO agent_identities(id,user_id,registered_domain,name) VALUES($1,$2,'agents.localhost','inside')`, recipient, user); err != nil {
		t.Fatal(err)
	}
	message := f.messageTo(sender, "relay", []string{recipient})
	if d := f.send(g, message); !d.Allow {
		t.Fatalf("internal send: %+v", d)
	}
	if d := f.send(g, f.message(sender, "relay", 1)); !d.Allow {
		t.Fatalf("internal consumed platform capacity: %+v", d)
	}
}

func TestAccountTrustConcurrentReservations(t *testing.T) {
	f := newFixture(t)
	p := sendingpolicy.DisabledPolicy()
	p.AccountTrustEnabled = true
	g := f.gate(p)
	user := f.user("standard")
	f.plan(user, "scale")
	agent := f.agent(user)
	var refs []sendingpolicy.OperationRef
	for i := 0; i < 25; i++ {
		_, ref := f.prepareMessage(g, f.message(agent, "relay", 1))
		refs = append(refs, ref)
	}
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for _, ref := range refs {
		wg.Add(1)
		go func(ref sendingpolicy.OperationRef) {
			defer wg.Done()
			d := f.authorize(g, ref)
			if d.Allow {
				allowed.Add(1)
			} else if d.DailyLimit == nil || d.DailyLimit.Limit != 20 {
				t.Errorf("missing limit on hold: %+v", d)
			}
		}(ref)
	}
	wg.Wait()
	if allowed.Load() != 20 {
		t.Fatalf("concurrent allowed=%d want 20", allowed.Load())
	}
}

func TestAccountTrustBareFreeAndSharedCeiling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cap    *int
		shared bool
		units  int
	}{{"bare free", intPtr(20), false, 21}, {"shared", nil, true, 51}, {"own", nil, false, 2001}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			p := sendingpolicy.DisabledPolicy()
			p.AccountTrustEnabled = true
			g := f.gate(p)
			user := f.user("standard")
			f.plan(user, "scale")
			if _, err := f.pool.Exec(f.ctx, `UPDATE account_limits SET max_messages_day=$2 WHERE user_id=$1`, user, tc.cap); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(f.ctx, `INSERT INTO account_sending_trust(user_id,grandfather_daily) VALUES($1,2000)`, user); err != nil {
				t.Fatal(err)
			}
			agent, _ := f.customDomainAgent(user)
			sentAs := "own_address"
			if tc.shared {
				agent = f.agent(user)
				sentAs = "relay"
			}
			if d := f.send(g, f.message(agent, sentAs, tc.units)); d.Allow || d.Reason != sendingpolicy.ReasonRampCapacity {
				t.Fatalf("ceiling: %+v", d)
			}
		})
	}
}
func intPtr(n int) *int { return &n }

func TestAccountTrustRedemptionRechecksPlanCap(t *testing.T) {
	f := newFixture(t)
	p := sendingpolicy.DisabledPolicy()
	p.AccountTrustEnabled = true
	g := f.gate(p)
	user := f.user("standard")
	f.plan(user, "scale")
	agent := f.agent(user)
	_, ref := f.prepareMessage(g, f.message(agent, "relay", 1))
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || !d.Allow || auth == nil {
		t.Fatalf("authorize: %+v %v", d, err)
	}
	if _, err = f.pool.Exec(f.ctx, `UPDATE account_limits SET max_messages_day=0 WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if err = g.RedeemProviderCall(f.ctx, *auth); err == nil {
		t.Fatal("old grant bypassed reduced plan cap")
	}
}

func TestAccountTrustMixedEnvelopeAndOwnerProofRevocation(t *testing.T) {
	f := newFixture(t)
	p := sendingpolicy.DisabledPolicy()
	p.AccountTrustEnabled = true
	p.DisableLegacyDailyBudgets = true
	p.BudgetMode = sendingpolicy.ModeEnforce
	p.AllCustomerGlobalDailyRecipients = 1
	g := f.gate(p)
	user := f.user("standard")
	f.plan(user, "scale")
	sender := f.agent(user)
	f.proveOwner(user)
	owner := f.ownerEmail(user)
	f.exec(`INSERT INTO domains(domain,user_id) VALUES('agents.localhost',$1) ON CONFLICT DO NOTHING`, user)
	f.exec(`INSERT INTO agent_identities(id,user_id,registered_domain,name) VALUES('inside@agents.localhost',$1,'agents.localhost','inside')`, user)
	message := f.esaMessage(sender, "relay", []string{owner, "unique@example.test"}, []string{strings.ToUpper(owner), "UNIQUE@EXAMPLE.TEST"}, []string{"inside@agents.localhost", "unique@example.test"})
	_, ref := f.prepareMessage(g, message)
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || !d.Allow || auth == nil {
		t.Fatalf("mixed envelope: %+v %v", d, err)
	}
	snapshot, err := g.(*sendingpolicy.Module).AccountDailyLimit(f.ctx, user)
	if err != nil || snapshot.Used != 1 || snapshot.SharedUsed != 1 {
		t.Fatalf("usage: %+v %v", snapshot, err)
	}
	// Losing the verified-owner exemption makes the old one-unit token stale.
	f.exec(`UPDATE users SET owner_email_verified_at=NULL,owner_email_verified_address=NULL,owner_email_verified_source=NULL WHERE id=$1`, user)
	if err = g.RedeemProviderCall(f.ctx, *auth); err == nil {
		t.Fatal("revoked owner proof expanded old grant")
	}
}

func TestAccountTrustGrantCannotKeepExternalCreditAfterOwnerVerification(t *testing.T) {
	f := newFixture(t)
	p := sendingpolicy.DisabledPolicy()
	p.AccountTrustEnabled = true
	g := f.gate(p)
	user := f.user("standard")
	f.plan(user, "scale")
	sender := f.agent(user)
	_, ref := f.prepareMessage(g, f.messageTo(sender, "relay", []string{f.ownerEmail(user)}))
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || !d.Allow || auth == nil {
		t.Fatalf("grant: %+v %v", d, err)
	}
	f.proveOwner(user)
	if err = g.RedeemProviderCall(f.ctx, *auth); err == nil {
		t.Fatal("newly internal recipient kept external-credit token")
	}
}

func TestAccountTrustDisabledRetryDoesNotEarnActivity(t *testing.T) {
	f := newFixture(t)
	p := sendingpolicy.DisabledPolicy()
	p.AccountTrustEnabled = true
	g := f.gate(p)
	user := f.user("standard")
	f.plan(user, "scale")
	sender := f.agent(user)
	_, ref := f.prepareMessage(g, f.messageTo(sender, "relay", []string{f.ownerEmail(user)}))
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || !d.Allow || auth == nil {
		t.Fatalf("grant: %+v %v", d, err)
	}
	f.proveOwner(user)
	disabled := f.gate(sendingpolicy.DisabledPolicy())
	if err = disabled.RedeemProviderCall(f.ctx, *auth); err == nil {
		t.Fatal("changed accounting mode kept old grant")
	}
	_, retry, err := disabled.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	d, auth, err = disabled.ConsumeAttempt(f.ctx, retry)
	if err != nil || !d.Allow || auth == nil {
		t.Fatalf("retry: %+v %v", d, err)
	}
	if err = disabled.RedeemProviderCall(f.ctx, *auth); err != nil {
		t.Fatal(err)
	}
	if err = disabled.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{Attempt: retry, Outcome: sendingpolicy.SettlementProviderAccepted}); err != nil {
		t.Fatal(err)
	}
	var activity int
	if err = f.pool.QueryRow(f.ctx, `SELECT COALESCE(sum(confirmed_count),0) FROM account_send_days WHERE user_id=$1`, user).Scan(&activity); err != nil {
		t.Fatal(err)
	}
	if activity != 0 {
		t.Fatalf("disabled internal retry earned %d units of clean activity", activity)
	}
}
