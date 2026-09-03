package sendingpolicy_test

import (
	"fmt"
	"testing"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/sendramp"
	"golang.org/x/net/publicsuffix"
)

// registrable mirrors the ramp ledger's own key derivation. The ledger is keyed
// by registrable domain, not hostname, so a test that asserts against the
// hostname silently reads an empty row and passes for the wrong reason.
//
// It also constrains the fixtures: `ramp-1.example.test` and
// `ramp-2.example.test` share the registrable domain `example.test`, so every
// fixture built that way would share one scope. Each fixture domain is
// therefore its own eTLD+1 (`ramp-N.test`) unless a test is specifically about
// sharing.
func registrable(t *testing.T, domain string) string {
	t.Helper()
	d, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		t.Fatalf("registrable domain for %q: %v", domain, err)
	}
	return d
}

// These tests cover the composition of two independent ledgers. The budget asks
// whether the account and the platform have exposed SES enough today; the ramp
// asks whether this particular domain has earned this volume yet. Both apply,
// and the tests below are mostly about which one wins and what happens to the
// other one's units when it loses.

// rampPolicy returns a policy with the ramp armed at the hosted generation-zero
// schedule (150 → 2000 over 30 days) and budgets wide enough not to interfere.
func rampPolicy(mutate func(*sendingpolicy.RuntimePolicy)) sendingpolicy.RuntimePolicy {
	p := sendingpolicy.DisabledPolicy()
	p.RampEnabled = true
	p.RampStartDaily = 150
	p.RampTargetDaily = 2000
	p.RampDays = 30
	if mutate != nil {
		mutate(&p)
	}
	return p
}

var rampSeq int

// customDomainAgent creates a user with a verified custom domain and an agent
// on it, which is the only shape the ramp governs.
func (f *fixture) customDomainAgent(userID string) (agentID, domain string) {
	f.t.Helper()
	rampSeq++
	// Its own registrable domain, so one fixture's ramp cannot advance another's.
	domain = fmt.Sprintf("ramp-%d.test", rampSeq)
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO domains (domain, user_id, verified, verified_at, sending_status, sending_ramp_status)
		VALUES ($1, $2, true, now(), 'verified', 'inactive')`, domain, userID,
	); err != nil {
		f.t.Fatalf("insert domain: %v", err)
	}
	agentID = fmt.Sprintf("agt_ramp_%d", rampSeq)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO agent_identities (id, user_id, registered_domain, name) VALUES ($1, $2, $3, $4)`,
		agentID, userID, domain, agentID,
	); err != nil {
		f.t.Fatalf("insert agent: %v", err)
	}
	return agentID, domain
}

// rampScope reads the account/registrable-domain scope's progress.
func (f *fixture) rampScope(userID, domain string) (status string, activeDays int, found bool) {
	f.t.Helper()
	err := f.pool.QueryRow(f.ctx,
		`SELECT status, active_days FROM sending_ramp_scopes WHERE user_id = $1 AND domain = $2`,
		userID, registrable(f.t, domain),
	).Scan(&status, &activeDays)
	if err != nil {
		return "", 0, false
	}
	return status, activeDays, true
}

func (f *fixture) domainCounter(userID, domain string) (reserved, confirmed, limit int) {
	f.t.Helper()
	err := f.pool.QueryRow(f.ctx, `
		SELECT COALESCE(SUM(reserved_count), 0), COALESCE(SUM(confirmed_count), 0), COALESCE(MAX(daily_limit), 0)
		  FROM domain_send_counters WHERE user_id = $1 AND domain = $2`, userID, registrable(f.t, domain),
	).Scan(&reserved, &confirmed, &limit)
	if err != nil {
		f.t.Fatalf("read domain counter: %v", err)
	}
	return reserved, confirmed, limit
}

// --- Schedule progression ------------------------------------------------

// TestRampStageCapsAndQualificationThresholds pins the numbers the rollout
// record approves: a new custom domain starts at 150 recipients/day and reaches
// 2,000 over 30 qualified days, qualifying each stage at half its allowance.
//
// These are the hosted values. The OSS/self-host default deliberately starts at
// 50, so this asserts the schedule arithmetic against the hosted schedule
// rather than against whatever `DefaultSchedule` happens to be.
func TestRampStageCapsAndQualificationThresholds(t *testing.T) {
	schedule := sendramp.NewSchedule(150, 2000, 30)
	for _, tc := range []struct {
		activeDay int
		cap       int
		qualifyAt int
	}{
		{0, 150, 75},
		{1, 213, 107},
		{2, 277, 139},
		{29, 2000, 1000},
	} {
		got := schedule.CapForActiveDay(tc.activeDay)
		if got != tc.cap {
			t.Errorf("active day %d cap = %d, want %d", tc.activeDay, got, tc.cap)
		}
		if !sendramp.Qualifies(tc.qualifyAt, got) {
			t.Errorf("active day %d: %d accepted must qualify against %d", tc.activeDay, tc.qualifyAt, got)
		}
		if sendramp.Qualifies(tc.qualifyAt-1, got) {
			t.Errorf("active day %d: %d accepted must NOT qualify against %d", tc.activeDay, tc.qualifyAt-1, got)
		}
	}
}

// --- Probation classification --------------------------------------------

// TestProbationClassification is the rule that decides whether an operation
// draws on the shared probation pool, which is the pool that bounds Sybil
// growth. Getting it wrong in the permissive direction is how "more accounts"
// starts multiplying allowance again.
func TestProbationClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		setup     func(f *fixture, userID, domain string)
		probation bool
	}{
		"inactive domain": {
			setup:     func(*fixture, string, string) {},
			probation: true,
		},
		"ramping, day zero": {
			setup: func(f *fixture, userID, domain string) {
				f.armScope(userID, domain, 0)
			},
			probation: true,
		},
		"ramping, one qualified day": {
			setup: func(f *fixture, userID, domain string) {
				f.armScope(userID, domain, 1)
			},
			probation: false,
		},
		"legacy exempt": {
			setup: func(f *fixture, _, domain string) {
				f.setDomainRampStatus(domain, sendramp.StatusExempt)
			},
			probation: false,
		},
		"completed ramp": {
			setup: func(f *fixture, _, domain string) {
				f.setDomainRampStatus(domain, sendramp.StatusComplete)
			},
			probation: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			policy := rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
				p.BudgetMode = sendingpolicy.ModeEnforce
				// A probation pool of exactly one unit turns the
				// classification into an observable allow/deny.
				p.ProbationGlobalDailyRecipients = 1
				p.DefaultAccountDailyRecipients = 100
				p.AllCustomerGlobalDailyRecipients = 100
			})
			g := f.gate(policy)
			user := f.user("standard")
			agent, domain := f.customDomainAgent(user)
			tc.setup(f, user, domain)

			// Burn the single probation unit with unrelated shared traffic,
			// which is probationary by definition.
			other := f.agent(f.user("standard"))
			if d := f.send(g, f.message(other, "relay", 1)); !d.Allow {
				t.Fatalf("priming shared send: %q", d.Reason)
			}

			d := f.send(g, f.message(agent, "own_address", 1))
			if tc.probation && d.Allow {
				t.Fatal("a probationary domain must be bounded by the exhausted probation pool")
			}
			if !tc.probation && !d.Allow {
				t.Fatalf("an established domain must not draw on the probation pool (held: %q)", d.Reason)
			}
		})
	}
}

// armScope creates the registrable-domain scope at a given qualified-day count.
func (f *fixture) armScope(userID, domain string, activeDays int) {
	f.t.Helper()
	f.setDomainRampStatus(domain, sendramp.StatusRamping)
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO sending_ramp_scopes (user_id, domain, start_daily, target_daily, ramp_days, active_days)
		VALUES ($1, $2, 150, 2000, 30, $3)
		ON CONFLICT (user_id, domain) DO UPDATE SET active_days = EXCLUDED.active_days`,
		userID, registrable(f.t, domain), activeDays,
	); err != nil {
		f.t.Fatalf("arm ramp scope: %v", err)
	}
}

func (f *fixture) setDomainRampStatus(domain, status string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE domains SET sending_ramp_status = $2 WHERE domain = $1`, domain, status,
	); err != nil {
		f.t.Fatalf("set ramp status: %v", err)
	}
}

// TestSharedTrafficNeverGraduates proves the one asymmetry that makes the whole
// scheme hold: shared-relay mail is probationary at every plan level and cannot
// age or pay its way out. The only exit is a customer-controlled domain.
func TestSharedTrafficNeverGraduates(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.ProbationGlobalDailyRecipients = 1
		p.DefaultAccountDailyRecipients = 100
		p.SharedDomainAccountDailyRecip = 100
		p.AllCustomerGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	f.plan(user, "scale")
	// Even with a fully established custom domain on the same account, the
	// account's SHARED traffic stays probationary.
	_, domain := f.customDomainAgent(user)
	f.armScope(user, domain, 5)

	relayAgent := f.agent(user)
	if d := f.send(g, f.message(relayAgent, "relay", 1)); !d.Allow {
		t.Fatalf("first shared unit: %q", d.Reason)
	}
	if d := f.send(g, f.message(relayAgent, "relay", 1)); d.Allow {
		t.Fatal("shared traffic must remain bounded by the probation pool regardless of plan or sibling domains")
	}
}

// --- Composition ----------------------------------------------------------

// TestRampHoldReturnsTheBudgetUnits is the most-restrictive-wins case that
// matters for correctness.
//
// The budget is decided first and the ramp last, so a ramp hold arrives with
// budget units already taken. Keeping them would charge an account for a send
// its own domain was not allowed to make, and that charge would persist until
// midnight.
func TestRampHoldReturnsTheBudgetUnits(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.RampStartDaily = 150
		p.DefaultAccountDailyRecipients = 1000
		p.AllCustomerGlobalDailyRecipients = 1000
		p.ProbationGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	f.plan(user, "scale")
	agent, domain := f.customDomainAgent(user)

	// Fill the first ramp stage exactly.
	if d := f.send(g, f.message(agent, "own_address", 150)); !d.Allow {
		t.Fatalf("first stage must fit: %q", d.Reason)
	}
	if reserved, _, limit := f.domainCounter(user, domain); reserved != 150 || limit != 150 {
		t.Fatalf("domain counter reserved=%d limit=%d, want 150/150", reserved, limit)
	}
	budgetBefore, _ := f.counter(sendingpolicy.ScopeAccountDaily, user)

	d := f.send(g, f.message(agent, "own_address", 1))
	if d.Allow {
		t.Fatal("the 151st recipient must be held by the ramp")
	}
	if d.Reason != sendingpolicy.ReasonRampCapacity {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonRampCapacity)
	}

	budgetAfter, _ := f.counter(sendingpolicy.ScopeAccountDaily, user)
	if budgetAfter != budgetBefore {
		t.Errorf("a ramp hold left %d extra budget units charged (was %d)", budgetAfter-budgetBefore, budgetBefore)
	}
}

// TestBudgetHoldLeavesTheRampUntouched is the mirror. The budget is decided
// first, so a budget hold must never have reached the ramp at all — otherwise a
// message held for the platform's reasons would silently consume the customer
// domain's daily allowance.
func TestBudgetHoldLeavesTheRampUntouched(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.DefaultAccountDailyRecipients = 2
		p.AllCustomerGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	agent, domain := f.customDomainAgent(user)

	if d := f.send(g, f.message(agent, "own_address", 2)); !d.Allow {
		t.Fatalf("first send: %q", d.Reason)
	}
	reservedBefore, _, _ := f.domainCounter(user, domain)

	d := f.send(g, f.message(agent, "own_address", 1))
	if d.Allow {
		t.Fatal("the third recipient must be held by the account budget")
	}
	if d.Reason != sendingpolicy.ReasonAccountDailyBudget {
		t.Errorf("reason = %q, want the account budget", d.Reason)
	}
	reservedAfter, _, _ := f.domainCounter(user, domain)
	if reservedAfter != reservedBefore {
		t.Errorf("a budget hold consumed %d ramp units", reservedAfter-reservedBefore)
	}
}

// TestSettlementIsTheOnlyThingThatAdvancesTheRamp proves progress measures
// delivered volume, not attempts. Without this, a domain could age into full
// allowance by failing repeatedly.
func TestSettlementIsTheOnlyThingThatAdvancesTheRamp(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.RampStartDaily = 150
		p.DefaultAccountDailyRecipients = 1000
		p.AllCustomerGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	agent, domain := f.customDomainAgent(user)

	// 75 recipients is exactly the first stage's qualification bar.
	_, ref := f.prepareMessage(g, f.message(agent, "own_address", 75))
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}

	// Authorized but unsettled: the day has not qualified.
	if _, days, ok := f.rampScope(user, domain); !ok || days != 0 {
		t.Fatalf("active days before settlement = %d (found=%v), want 0", days, ok)
	}

	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: attempt, Outcome: sendingpolicy.SettlementProviderAccepted,
	}); err != nil {
		t.Fatalf("settle accepted: %v", err)
	}
	if _, days, _ := f.rampScope(user, domain); days != 1 {
		t.Fatalf("active days after acceptance = %d, want 1", days)
	}

	// Settlement is idempotent — both the synchronous success branch and the
	// delayed delivery-feedback finalizer call it for the same attempt.
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: attempt, Outcome: sendingpolicy.SettlementProviderAccepted,
	}); err != nil {
		t.Fatalf("repeat settle: %v", err)
	}
	if _, days, _ := f.rampScope(user, domain); days != 1 {
		t.Fatalf("active days after a repeated settlement = %d, want 1", days)
	}
}

// TestPermanentRejectionReleasesRampCapacity proves a definitively refused
// message gives its ramp units back, while an unsettled one does not: a message
// that might have been delivered must not release capacity.
func TestPermanentRejectionReleasesRampCapacity(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 1000
		p.AllCustomerGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	agent, domain := f.customDomainAgent(user)

	_, ref := f.prepareMessage(g, f.message(agent, "own_address", 10))
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	if reserved, _, _ := f.domainCounter(user, domain); reserved != 10 {
		t.Fatalf("ramp reserved = %d, want 10", reserved)
	}

	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: attempt, Outcome: sendingpolicy.SettlementProviderPermanentlyRejected,
	}); err != nil {
		t.Fatalf("settle rejected: %v", err)
	}
	if reserved, _, _ := f.domainCounter(user, domain); reserved != 0 {
		t.Errorf("ramp reserved = %d after a permanent rejection, want 0", reserved)
	}
	if _, days, _ := f.rampScope(user, domain); days != 0 {
		t.Errorf("a rejected message advanced the ramp to day %d", days)
	}
}

// TestDeferRetainsTheRampWhileCancelReleasesIt is the difference between "slow
// down" and "never mind".
//
// A rate deferral has not been rejected by anyone, so releasing its ramp claim
// would let the same message re-qualify a stage it already qualified. A
// suppression match is terminal and gives both ledgers back.
func TestDeferRetainsTheRampWhileCancelReleasesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		release   func(*fixture, sendingpolicy.Gate, sendingpolicy.AttemptRef) error
		rampAfter int
	}{
		"defer keeps the ramp": {
			release: func(f *fixture, g sendingpolicy.Gate, a sendingpolicy.AttemptRef) error {
				return g.DeferAttempt(f.ctx, a)
			},
			rampAfter: 7,
		},
		"cancel releases the ramp": {
			release: func(f *fixture, g sendingpolicy.Gate, a sendingpolicy.AttemptRef) error {
				return g.CancelAttempt(f.ctx, a)
			},
			rampAfter: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
				p.BudgetMode = sendingpolicy.ModeEnforce
				p.DefaultAccountDailyRecipients = 1000
				p.AllCustomerGlobalDailyRecipients = 1000
				p.ProbationGlobalDailyRecipients = 1000
			}))
			user := f.user("standard")
			agent, domain := f.customDomainAgent(user)

			_, ref := f.prepareMessage(g, f.message(agent, "own_address", 7))
			_, attempt, err := g.Reserve(f.ctx, ref)
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			// The ramp reservation is created at final authorization, so drive
			// one and then step back to a pre-provider state.
			if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
				t.Fatalf("authorize: auth=%v err=%v", auth, err)
			}
			if reserved, _, _ := f.domainCounter(user, domain); reserved != 7 {
				t.Fatalf("ramp reserved = %d, want 7", reserved)
			}

			// Re-arm a fresh pre-provider ordinal, which is what a worker that
			// hits its rate limiter or a suppression actually holds.
			_, next, err := g.Reserve(f.ctx, ref)
			if err != nil {
				t.Fatalf("second reserve: %v", err)
			}
			if err := tc.release(f, g, next); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if reserved, _, _ := f.domainCounter(user, domain); reserved != tc.rampAfter {
				t.Errorf("ramp reserved = %d after %s, want %d", reserved, name, tc.rampAfter)
			}
		})
	}
}

// TestDisabledRampIsPassThroughAndWritesNoExemption proves the production state
// this slice ships in.
//
// Pass-through has to mean pass-through: writing `exempt` while the ramp is off
// would permanently grandfather every domain that happened to send during the
// disabled window, and the phase-3 activation would then find nothing left to
// ramp.
func TestDisabledRampIsPassThroughAndWritesNoExemption(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy()) // ramp_enabled: false
	user := f.user("standard")
	agent, domain := f.customDomainAgent(user)

	if d := f.send(g, f.message(agent, "own_address", 500)); !d.Allow {
		t.Fatalf("a disabled ramp must pass through: %q", d.Reason)
	}

	var status string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT sending_ramp_status FROM domains WHERE domain = $1`, domain,
	).Scan(&status); err != nil {
		t.Fatalf("read ramp status: %v", err)
	}
	if status != sendramp.StatusInactive {
		t.Errorf("ramp status = %q, want it left inactive", status)
	}
	if _, _, found := f.rampScope(user, domain); found {
		t.Error("a disabled ramp created a scope row")
	}
	if reserved, _, _ := f.domainCounter(user, domain); reserved != 0 {
		t.Errorf("a disabled ramp reserved %d units", reserved)
	}
}

// TestRampSharesOneScopeAcrossAnAccountsSubdomains proves the ledger is keyed by
// registrable domain, not by hostname — otherwise a customer could mint fresh
// 150-recipient allowances by inventing subdomains.
func TestRampSharesOneScopeAcrossAnAccountsSubdomains(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.RampStartDaily = 150
		p.DefaultAccountDailyRecipients = 1000
		p.AllCustomerGlobalDailyRecipients = 1000
		p.ProbationGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	f.plan(user, "scale")

	// Two hostnames under one registrable domain.
	rampSeq++
	// One registrable domain, two hostnames under it.
	base := fmt.Sprintf("shared-%d.test", rampSeq)
	agents := make([]string, 2)
	for i, host := range []string{"a." + base, "b." + base} {
		if _, err := f.pool.Exec(f.ctx, `
			INSERT INTO domains (domain, user_id, verified, verified_at, sending_status, sending_ramp_status)
			VALUES ($1, $2, true, now(), 'verified', 'inactive')`, host, user,
		); err != nil {
			t.Fatalf("insert domain %s: %v", host, err)
		}
		agents[i] = fmt.Sprintf("agt_sub_%d_%d", rampSeq, i)
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO agent_identities (id, user_id, registered_domain, name) VALUES ($1, $2, $3, $4)`,
			agents[i], user, host, agents[i],
		); err != nil {
			t.Fatalf("insert agent: %v", err)
		}
	}

	if d := f.send(g, f.message(agents[0], "own_address", 150)); !d.Allow {
		t.Fatalf("first subdomain must fill the stage: %q", d.Reason)
	}
	d := f.send(g, f.message(agents[1], "own_address", 1))
	if d.Allow {
		t.Fatal("a sibling subdomain must not find a fresh ramp allowance")
	}
	if d.Reason != sendingpolicy.ReasonRampCapacity {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonRampCapacity)
	}
}

// TestFreePlanCanQualifyStageOneButNotStageTwo is the interaction the design
// calls out explicitly: a Free account's 100/day ceiling lets it clear the
// 75-recipient stage-one bar but not the 107-recipient stage-two bar, and the
// progress it already made survives the upgrade rather than resetting.
func TestFreePlanCanQualifyStageOneButNotStageTwo(t *testing.T) {
	f := newFixture(t)
	policy := rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 5000
		p.ProbationGlobalDailyRecipients = 5000
	})
	g := f.gate(policy)
	user := f.user("standard")
	agent, domain := f.customDomainAgent(user)

	// Stage one qualifies at 75, inside the Free ceiling of 100.
	_, ref := f.prepareMessage(g, f.message(agent, "own_address", 75))
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: attempt, Outcome: sendingpolicy.SettlementProviderAccepted,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, days, _ := f.rampScope(user, domain); days != 1 {
		t.Fatalf("stage one did not qualify (active days = %d)", days)
	}

	// Stage two needs 107 accepted recipients in one day, which the Free
	// ceiling forbids. The account is stuck at day one — but not reset.
	if d := f.send(g, f.message(agent, "own_address", 25)); !d.Allow {
		t.Fatalf("the rest of the Free allowance must still send: %q", d.Reason)
	}
	if d := f.send(g, f.message(agent, "own_address", 1)); d.Allow {
		t.Fatal("a Free account must stop at its own daily ceiling")
	}
	if _, days, _ := f.rampScope(user, domain); days != 1 {
		t.Errorf("active days = %d, want progress retained at 1", days)
	}

	// After an upgrade, progression resumes from where it stopped.
	f.plan(user, "scale")
	if _, days, _ := f.rampScope(user, domain); days != 1 {
		t.Errorf("an upgrade reset ramp progress to %d", days)
	}
}
