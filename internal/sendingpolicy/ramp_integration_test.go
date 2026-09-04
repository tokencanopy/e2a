package sendingpolicy_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

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
		// A scope's history cannot vouch for an identity it no longer sends
		// under. A rebind onto an unverified child subdomain leaves the
		// qualified days behind while the mail goes out on an unproven
		// identity, so classification has to follow the identity.
		"one qualified day, sending identity unverified": {
			setup: func(f *fixture, userID, domain string) {
				f.armScope(userID, domain, 1)
				f.setDomainSendingStatus(domain, "pending")
			},
			probation: true,
		},
		// The two legacy states mean "this domain already earned its volume",
		// and they say so about the domain rather than about today's SES
		// verification record. They must stay established.
		"legacy exempt, sending identity unverified": {
			setup: func(f *fixture, _, domain string) {
				f.setDomainRampStatus(domain, sendramp.StatusExempt)
				f.setDomainSendingStatus(domain, "pending")
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
			if tc.probation {
				if d.Allow {
					t.Fatal("a probationary domain must be bounded by the exhausted probation pool")
				}
				// The reason matters as much as the verdict: any other hold
				// would mean the send was stopped by something that is not the
				// Sybil guardrail, and the classification would be untested.
				if d.Reason != sendingpolicy.ReasonGlobalProbation {
					t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonGlobalProbation)
				}
				return
			}
			if !d.Allow {
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
	d := f.send(g, f.message(relayAgent, "relay", 1))
	if d.Allow {
		t.Fatal("shared traffic must remain bounded by the probation pool regardless of plan or sibling domains")
	}
	if d.Reason != sendingpolicy.ReasonGlobalProbation {
		t.Errorf("reason = %q, want %q — any other hold would leave the classification untested",
			d.Reason, sendingpolicy.ReasonGlobalProbation)
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
	// Every pool the transaction charges has to be given back, not just the
	// account's: the two global pools are the ones an abusive account would
	// otherwise pin for the whole platform by farming ramp holds.
	pools := []struct {
		scope sendingpolicy.Scope
		id    string
	}{
		{sendingpolicy.ScopeGlobalAll, "all-customers"},
		{sendingpolicy.ScopeGlobalProbation, "probation"},
		{sendingpolicy.ScopeAccountDaily, user},
	}
	before := make([]int, len(pools))
	for i, pool := range pools {
		before[i], _ = f.counter(pool.scope, pool.id)
		if before[i] == 0 {
			t.Fatalf("%s was never charged, so its release proves nothing", pool.scope)
		}
	}

	d := f.send(g, f.message(agent, "own_address", 1))
	if d.Allow {
		t.Fatal("the 151st recipient must be held by the ramp")
	}
	if d.Reason != sendingpolicy.ReasonRampCapacity {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonRampCapacity)
	}

	for i, pool := range pools {
		after, _ := f.counter(pool.scope, pool.id)
		if after != before[i] {
			t.Errorf("a ramp hold left %d extra units charged on %s (was %d)", after-before[i], pool.scope, before[i])
		}
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
// suppression match is terminal and gives both ledgers back — but only while
// the ramp units are still provably pre-provider, which is why the reservation
// here is taken before any attempt is authorized.
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

			messageID := f.message(agent, "own_address", 7)
			f.legacyRampReserve(user, domain, messageID, 7, time.Time{})
			if reserved, _, _ := f.domainCounter(user, domain); reserved != 7 {
				t.Fatalf("ramp reserved = %d, want 7", reserved)
			}

			_, ref := f.prepareMessage(g, messageID)
			_, attempt, err := g.Reserve(f.ctx, ref)
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if err := tc.release(f, g, attempt); err != nil {
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

	// After an upgrade, progression RESUMES from where it stopped. That the
	// plan write left the scope alone is the weaker half; what the design
	// promises is that the next stage can now be qualified without a reset.
	f.plan(user, "scale")
	if _, days, _ := f.rampScope(user, domain); days != 1 {
		t.Errorf("an upgrade reset ramp progress to %d", days)
	}

	// Roll both ledgers to the next UTC day, which is what midnight does.
	for _, stmt := range []string{
		`UPDATE sending_budget_counters SET day = day - 1`,
		`UPDATE domain_send_counters SET day = day - 1`,
		`UPDATE sending_ramp_reservations SET day = day - 1`,
		// The qualified-day marker moves with them, or the scope would refuse
		// to count a second day it believes it already counted.
		`UPDATE sending_ramp_scopes SET last_qualified_day = last_qualified_day - 1`,
	} {
		if _, err := f.pool.Exec(f.ctx, stmt); err != nil {
			t.Fatalf("advance day (%s): %v", stmt, err)
		}
	}

	// Stage two allows 213 recipients and qualifies at 107 — the bar the Free
	// ceiling forbade and the upgraded plan can now afford.
	_, second := f.prepareMessage(g, f.message(agent, "own_address", 107))
	_, stageTwo, err := g.Reserve(f.ctx, second)
	if err != nil {
		t.Fatalf("stage two reserve: %v", err)
	}
	if d, auth, err := g.ConsumeAttempt(f.ctx, stageTwo); err != nil || auth == nil {
		t.Fatalf("stage two authorize: decision=%+v auth=%v err=%v", d, auth, err)
	}
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: stageTwo, Outcome: sendingpolicy.SettlementProviderAccepted,
	}); err != nil {
		t.Fatalf("stage two settle: %v", err)
	}
	if _, days, _ := f.rampScope(user, domain); days != 2 {
		t.Errorf("active days = %d after the upgraded account cleared stage two, want 2", days)
	}
	if _, _, limit := f.domainCounter(user, domain); limit != 213 {
		t.Errorf("stage two daily limit = %d, want 213", limit)
	}
}

// --- Helpers for the composition tests -----------------------------------

// legacyRampReserve takes ramp capacity the way today's outbound worker does:
// straight through the pool-owning store, BEFORE any provider authorization
// exists.
//
// That shape matters for more than migration compatibility. It is the only way
// a ramp reservation can exist for a message no attempt has been authorized to
// send, and therefore the only state in which a local cancellation is still
// allowed to give those units back.
func (f *fixture) legacyRampReserve(userID, domain, messageID string, units int, day time.Time) sendramp.Decision {
	f.t.Helper()
	d, err := sendramp.NewStore(f.pool).Reserve(f.ctx, sendramp.ReserveRequest{
		MessageID: messageID,
		UserID:    userID,
		Domain:    domain,
		Units:     units,
		Day:       day,
		Schedule:  sendramp.NewSchedule(150, 2000, 30),
	})
	if err != nil {
		f.t.Fatalf("legacy ramp reserve: %v", err)
	}
	return d
}

// setDomainSendingStatus rewrites the domain's SENDING identity state, which is
// what SES verification drives and what a subdomain rebind can move under a
// message that was already accepted.
func (f *fixture) setDomainSendingStatus(domain, status string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE domains SET sending_status = $2 WHERE domain = $1`, domain, status,
	); err != nil {
		f.t.Fatalf("set sending status: %v", err)
	}
}

// reservationProbation reads the probation class Reserve actually stored, which
// is the value every later release targets.
func (f *fixture) reservationProbation(operationID string, attempt int) bool {
	f.t.Helper()
	var probation bool
	if err := f.pool.QueryRow(f.ctx, `
		SELECT probation FROM sending_budget_reservations
		 WHERE operation_id = $1 AND submission_attempt = $2`, operationID, attempt,
	).Scan(&probation); err != nil {
		f.t.Fatalf("read reservation probation: %v", err)
	}
	return probation
}

// ledgerToday is the UTC date the module's own clock read would produce.
func (f *fixture) ledgerToday() time.Time {
	f.t.Helper()
	var day time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT (clock_timestamp() AT TIME ZONE 'UTC')::date`).Scan(&day); err != nil {
		f.t.Fatalf("read ledger day: %v", err)
	}
	return day.UTC()
}

func (f *fixture) counterOn(scope sendingpolicy.Scope, scopeID string, day time.Time) (reserved, confirmed int) {
	f.t.Helper()
	err := f.pool.QueryRow(f.ctx, `
		SELECT COALESCE(SUM(reserved_count), 0), COALESCE(SUM(confirmed_count), 0)
		  FROM sending_budget_counters
		 WHERE scope = $1 AND scope_id = $2 AND day = $3`, string(scope), scopeID, day,
	).Scan(&reserved, &confirmed)
	if err != nil {
		f.t.Fatalf("read counter for %s: %v", day.Format("2006-01-02"), err)
	}
	return reserved, confirmed
}

func (f *fixture) domainCounterOn(userID, domain string, day time.Time) (reserved, confirmed int) {
	f.t.Helper()
	err := f.pool.QueryRow(f.ctx, `
		SELECT COALESCE(SUM(reserved_count), 0), COALESCE(SUM(confirmed_count), 0)
		  FROM domain_send_counters WHERE user_id = $1 AND domain = $2 AND day = $3`,
		userID, registrable(f.t, domain), day,
	).Scan(&reserved, &confirmed)
	if err != nil {
		f.t.Fatalf("read domain counter for %s: %v", day.Format("2006-01-02"), err)
	}
	return reserved, confirmed
}

// --- Cancellation versus provider exposure --------------------------------

// TestCancelCannotRefundRampUnitsAnEarlierAttemptSpent is the difference
// between the two ledgers' keys, and it is the whole reason the stage cap is
// not advisory.
//
// The sending budget is reserved per ATTEMPT and refuses to refund a confirmed
// one. The ramp reservation is keyed by MESSAGE, so it has no ordinal of its
// own: the units attempt one handed to the provider are the same units attempt
// two would be giving back. An ambiguous provider result deliberately leaves
// attempt one unsettled, River allocates attempt two, and a suppression added
// in between cancels it — at which point a refund would hand back capacity for
// mail that may already be in flight, repeatably.
func TestCancelCannotRefundRampUnitsAnEarlierAttemptSpent(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.DefaultAccountDailyRecipients = 1000
		p.AllCustomerGlobalDailyRecipients = 1000
		p.ProbationGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	f.plan(user, "scale")
	agent, domain := f.customDomainAgent(user)

	_, ref := f.prepareMessage(g, f.message(agent, "own_address", 100))
	_, first, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, auth, err := g.ConsumeAttempt(f.ctx, first); err != nil || auth == nil {
		t.Fatalf("authorize attempt one: auth=%v err=%v", auth, err)
	}
	if reserved, _, _ := f.domainCounter(user, domain); reserved != 100 {
		t.Fatalf("ramp reserved = %d after authorization, want 100", reserved)
	}

	// The provider result was ambiguous, so nothing settled and the reservation
	// stands. The retry allocates a strictly greater ordinal.
	_, second, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if got := f.currentAttempt(ref.ID()); got != 2 {
		t.Fatalf("current attempt = %d, want a fresh ordinal 2", got)
	}
	if err := g.CancelAttempt(f.ctx, second); err != nil {
		t.Fatalf("cancel attempt two: %v", err)
	}

	if reserved, _, _ := f.domainCounter(user, domain); reserved != 100 {
		t.Errorf("cancelling attempt two refunded %d ramp units attempt one already handed to the provider",
			100-reserved)
	}

	// Repeatability is what turns the leak into an unlimited allowance, so
	// prove the second cycle refunds nothing either.
	_, third, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("third reserve: %v", err)
	}
	if err := g.CancelAttempt(f.ctx, third); err != nil {
		t.Fatalf("cancel attempt three: %v", err)
	}
	if reserved, _, _ := f.domainCounter(user, domain); reserved != 100 {
		t.Errorf("a repeated cancel drained the ramp to %d, want it pinned at 100", reserved)
	}
}

// TestCancelAfterADeferStillReleasesTheRamp closes the mirror gap.
//
// DeferAttempt gives the budget back and marks the attempt released; a
// suppression discovered immediately afterwards cancels the same ordinal. The
// budget half is correctly a no-op the second time, but the ramp half had never
// run at all, so a message that will never be sent kept its domain's allowance
// until midnight.
func TestCancelAfterADeferStillReleasesTheRamp(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.DefaultAccountDailyRecipients = 1000
		p.AllCustomerGlobalDailyRecipients = 1000
		p.ProbationGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	agent, domain := f.customDomainAgent(user)

	messageID := f.message(agent, "own_address", 7)
	f.legacyRampReserve(user, domain, messageID, 7, time.Time{})
	_, ref := f.prepareMessage(g, messageID)
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// Rate deferral first: the budget comes back, the ramp deliberately does
	// not.
	if err := g.DeferAttempt(f.ctx, attempt); err != nil {
		t.Fatalf("defer: %v", err)
	}
	if reserved, _, _ := f.domainCounter(user, domain); reserved != 7 {
		t.Fatalf("ramp reserved = %d after a deferral, want it retained at 7", reserved)
	}

	// Then the suppression match, on the same ordinal the worker still holds.
	if err := g.CancelAttempt(f.ctx, attempt); err != nil {
		t.Fatalf("cancel after defer: %v", err)
	}
	if reserved, _, _ := f.domainCounter(user, domain); reserved != 0 {
		t.Errorf("a cancel following a deferral left %d ramp units held for a message that will never send", reserved)
	}
}

// --- Permanent ramp failure ------------------------------------------------

// TestAPermanentRampRefusalReleasesTheBudgetUnits is the stranding case.
//
// A permanent ramp error is not "the database is unhappy" — it is a definite
// refusal that every later execution will repeat. Returning it as an error
// rolls the transaction back with the attempt still `reserved`, so the units it
// holds on the SHARED pools can never be released by anything: not by the
// retry, which fails at the same point, and not by a cancel, which the worker
// never reaches. A handful of those exhausts the probation pool for the day.
func TestAPermanentRampRefusalReleasesTheBudgetUnits(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
		p.ProbationGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	agent, domain := f.customDomainAgent(user)

	messageID := f.message(agent, "own_address", 50)
	f.legacyRampReserve(user, domain, messageID, 50, time.Time{})
	// A local failure released those units. The ramp treats a released
	// reservation as terminal and permanently refuses to reserve it again.
	if err := sendramp.NewStore(f.pool).Release(f.ctx, messageID); err != nil {
		t.Fatalf("release ramp reservation: %v", err)
	}

	_, ref := f.prepareMessage(g, messageID)
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("a permanent ramp refusal must be a hold, not an error: %v", err)
	}
	if auth != nil {
		t.Fatal("a hold must never return a token")
	}
	assertTerminal(t, d, sendingpolicy.ReasonRampUnavailable)

	for _, pool := range []struct {
		scope sendingpolicy.Scope
		id    string
	}{
		{sendingpolicy.ScopeGlobalAll, "all-customers"},
		{sendingpolicy.ScopeGlobalProbation, "probation"},
		{sendingpolicy.ScopeAccountDaily, user},
	} {
		if reserved, _ := f.counter(pool.scope, pool.id); reserved != 0 {
			t.Errorf("%s left %d units stranded with nothing able to release them", pool.scope, reserved)
		}
	}
	if state, callState := f.reservationState(ref.ID(), 1); state != "released" || callState != "none" {
		t.Errorf("attempt left as %s/%s, want released/none", state, callState)
	}
}

// --- Sending identity ------------------------------------------------------

// TestAnUnverifiedSendingIdentityHoldsInsteadOfPassingThrough closes the
// rebind bypass.
//
// The message's reputation class and its composed From are frozen at
// acceptance, but the agent's registered domain is not: verifying a child
// subdomain rebinds the account's agents onto it, and that child's SES identity
// stays unverified while its DKIM records are never published. The ramp reads
// the registered domain live, so an unverified identity that passes through
// hands an accepted backlog an uncapped day — and, because a scope with a
// qualified day behind it also reports established, it does not even pay the
// probation pool on the way out.
func TestAnUnverifiedSendingIdentityHoldsInsteadOfPassingThrough(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.DefaultAccountDailyRecipients = 5000
		p.AllCustomerGlobalDailyRecipients = 5000
		p.ProbationGlobalDailyRecipients = 5000
	}))
	user := f.user("standard")
	f.plan(user, "scale")
	agent, domain := f.customDomainAgent(user)
	// A qualified day behind it, so nothing about this scope's history explains
	// the refusal — only its identity does.
	f.armScope(user, domain, 1)
	f.setDomainSendingStatus(domain, "pending")

	d := f.send(g, f.message(agent, "own_address", 5000))
	if d.Allow {
		t.Fatal("an unverified sending identity must not be ramp pass-through")
	}
	if d.Reason != sendingpolicy.ReasonSendingIdentityUnverified {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonSendingIdentityUnverified)
	}
	if reserved, _, _ := f.domainCounter(user, domain); reserved != 0 {
		t.Errorf("the ramp counter moved by %d for a send it never governed", reserved)
	}
}

// --- Reserve's probation classification ------------------------------------

// TestReserveClassifiesProbationFromTheRamp pins the early half of the
// composition.
//
// Reserve writes the probation column every later release targets and charges
// the pools it names. A stand-in answer there is wrong three ways: the stored
// class disagrees with the one final authorization computes, the early hold
// never bounds the probation pool at all, and every authorization pays a
// needless release-and-reacquire on the platform's hottest counter rows.
func TestReserveClassifiesProbationFromTheRamp(t *testing.T) {
	for name, tc := range map[string]struct {
		setup     func(f *fixture, userID, domain string)
		probation bool
	}{
		"day zero": {
			setup:     func(f *fixture, u, d string) { f.armScope(u, d, 0) },
			probation: true,
		},
		"one qualified day": {
			setup:     func(f *fixture, u, d string) { f.armScope(u, d, 1) },
			probation: false,
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
			f.plan(user, "scale")
			agent, domain := f.customDomainAgent(user)
			tc.setup(f, user, domain)

			_, ref := f.prepareMessage(g, f.message(agent, "own_address", 3))
			if _, _, err := g.Reserve(f.ctx, ref); err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if got := f.reservationProbation(ref.ID(), 1); got != tc.probation {
				t.Errorf("Reserve stored probation = %v, want %v", got, tc.probation)
			}
			reserved, _ := f.counter(sendingpolicy.ScopeGlobalProbation, "probation")
			if (reserved > 0) != tc.probation {
				t.Errorf("Reserve charged %d probation units, want charged=%v", reserved, tc.probation)
			}
		})
	}
}

// TestReserveHoldsAnUnprovenDomainOnTheProbationPool proves the early hold
// actually bounds the pool it classifies into. Without it a worker composes and
// signs a message before learning the platform's Sybil guardrail is exhausted.
func TestReserveHoldsAnUnprovenDomainOnTheProbationPool(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.ProbationGlobalDailyRecipients = 1
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	agent, _ := f.customDomainAgent(user)

	// Burn the single probation unit with unrelated shared traffic.
	other := f.agent(f.user("standard"))
	if d := f.send(g, f.message(other, "relay", 1)); !d.Allow {
		t.Fatalf("priming shared send: %q", d.Reason)
	}

	_, ref := f.prepareMessage(g, f.message(agent, "own_address", 1))
	d, _, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if d.Allow {
		t.Fatal("Reserve must bound an unproven custom domain by the probation pool")
	}
	if d.Reason != sendingpolicy.ReasonGlobalProbation {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonGlobalProbation)
	}
}

// --- Terminal source loss --------------------------------------------------

// TestARampSourceThatVanishedIsTerminal is a regression on hold shape rather
// than on accounting.
//
// The ramp's own source read runs before the envelope resolution that already
// answers this correctly, so a retryable answer here pre-empts the terminal one
// whenever the ramp is armed. A non-terminal hold makes the worker snooze
// forever on an operation whose message no longer exists.
func TestARampSourceThatVanishedIsTerminal(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
		p.ProbationGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	agent, _ := f.customDomainAgent(user)

	messageID := f.message(agent, "own_address", 2)
	_, ref := f.prepareMessage(g, messageID)
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM messages WHERE id = $1`, messageID); err != nil {
		t.Fatalf("delete message: %v", err)
	}

	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if auth != nil {
		t.Fatal("a hold must never return a token")
	}
	assertTerminal(t, d, sendingpolicy.ReasonSourceUnavailable)
}

// --- Concurrency -----------------------------------------------------------

// TestConcurrentAuthorizationsCannotExceedOneStageCapOrDeadlock is the reason
// the ramp store was collapsed onto a single lock order.
//
// Every one of these transactions holds the platform's hottest budget counters
// before it reaches the ramp's per-domain keys, so any disagreement about
// suborder closes a cycle across two subsystems — and Postgres resolves a cycle
// by killing a transaction, which on this path is a message that errors instead
// of being held. The assertion is therefore both halves at once: exactly one
// stage cap admitted, and not one deadlock.
func TestConcurrentAuthorizationsCannotExceedOneStageCapOrDeadlock(t *testing.T) {
	const (
		workers = 8
		units   = 30 // 8 x 30 = 240 against a 150-recipient first stage
	)
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.RampStartDaily = 150
		p.DefaultAccountDailyRecipients = 5000
		p.AllCustomerGlobalDailyRecipients = 5000
		p.ProbationGlobalDailyRecipients = 5000
	}))
	user := f.user("standard")
	f.plan(user, "scale")
	agent, domain := f.customDomainAgent(user)

	refs := make([]sendingpolicy.OperationRef, workers)
	for i := range refs {
		_, refs[i] = f.prepareMessage(g, f.message(agent, "own_address", units))
	}

	var wg sync.WaitGroup
	granted := make([]bool, workers)
	errs := make([]error, workers)
	for i := range refs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			early, attempt, err := g.Reserve(f.ctx, refs[i])
			if err != nil {
				errs[i] = fmt.Errorf("reserve: %w", err)
				return
			}
			if !early.Allow {
				return
			}
			d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
			if err != nil {
				errs[i] = fmt.Errorf("consume: %w", err)
				return
			}
			granted[i] = d.Allow && auth != nil
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			// A deadlock arrives here as SQLSTATE 40P01.
			t.Fatalf("worker %d failed instead of being held: %v", i, err)
		}
	}
	admitted := 0
	for _, ok := range granted {
		if ok {
			admitted++
		}
	}
	if admitted != 5 {
		t.Errorf("%d of %d workers were admitted, want exactly the 150/%d that fit one stage", admitted, workers, units)
	}
	reserved, _, limit := f.domainCounter(user, domain)
	if reserved != 150 || limit != 150 {
		t.Errorf("domain counter reserved=%d limit=%d, want the stage filled exactly once (150/150)", reserved, limit)
	}
}

// --- Midnight --------------------------------------------------------------

// TestOneAuthorizationReAgesBothLedgersAcrossMidnight proves the two ledgers
// roll over together.
//
// Final authorization re-derives the UTC day after taking its locks, so an
// attempt reserved before midnight must give yesterday's units back on BOTH
// ledgers and take today's on both. Rolling only one leaves the other holding a
// dead day's capacity until a janitor notices.
func TestOneAuthorizationReAgesBothLedgersAcrossMidnight(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.BudgetMode = sendingpolicy.ModeEnforce
		p.DefaultAccountDailyRecipients = 1000
		p.AllCustomerGlobalDailyRecipients = 1000
		p.ProbationGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	agent, domain := f.customDomainAgent(user)
	today := f.ledgerToday()
	yesterday := today.AddDate(0, 0, -1)

	// The ramp reservation was taken yesterday, before the rollover.
	messageID := f.message(agent, "own_address", 5)
	f.legacyRampReserve(user, domain, messageID, 5, yesterday)
	if reserved, _ := f.domainCounterOn(user, domain, yesterday); reserved != 5 {
		t.Fatalf("yesterday's ramp counter = %d, want 5", reserved)
	}

	// So was the budget reservation. Ageing the rows is exactly what midnight
	// does from the ledger's point of view.
	_, ref := f.prepareMessage(g, messageID)
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE sending_budget_counters SET day = day - 1`,
		`UPDATE sending_budget_reservations SET day = day - 1`,
	} {
		if _, err := f.pool.Exec(f.ctx, stmt); err != nil {
			t.Fatalf("advance day (%s): %v", stmt, err)
		}
	}

	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize across midnight: auth=%v err=%v decision=%+v", auth, err, d)
	}

	for _, pool := range []struct {
		scope sendingpolicy.Scope
		id    string
	}{
		{sendingpolicy.ScopeGlobalAll, "all-customers"},
		{sendingpolicy.ScopeGlobalProbation, "probation"},
		{sendingpolicy.ScopeAccountDaily, user},
	} {
		if reserved, _ := f.counterOn(pool.scope, pool.id, yesterday); reserved != 0 {
			t.Errorf("%s still holds %d units on yesterday", pool.scope, reserved)
		}
		if reserved, confirmed := f.counterOn(pool.scope, pool.id, today); reserved != 5 || confirmed != 5 {
			t.Errorf("%s today reserved=%d confirmed=%d, want 5/5", pool.scope, reserved, confirmed)
		}
	}
	if reserved, _ := f.domainCounterOn(user, domain, yesterday); reserved != 0 {
		t.Errorf("the ramp still holds %d units on yesterday", reserved)
	}
	if reserved, _ := f.domainCounterOn(user, domain, today); reserved != 5 {
		t.Errorf("today's ramp counter = %d, want the re-aged 5", reserved)
	}
}

// --- Delayed provider acceptance -------------------------------------------

// TestDelayedAcceptanceCreditsTheAttemptsOwnDay proves settlement is bound to
// the attempt, not to the clock that happens to be running when the evidence
// arrives.
//
// SES delivery evidence can land days after submission, and the finalizer that
// replays it calls the same SettleProvider. Crediting "today" would let a
// domain qualify a day it never sent on, and would leave the real day's counter
// reserved-but-never-confirmed forever.
func TestDelayedAcceptanceCreditsTheAttemptsOwnDay(t *testing.T) {
	f := newFixture(t)
	g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 1000
		p.AllCustomerGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	agent, domain := f.customDomainAgent(user)
	sendDay := f.ledgerToday().AddDate(0, 0, -3)

	// 80 recipients clears the first stage's 75-recipient qualification bar.
	_, ref := f.prepareMessage(g, f.message(agent, "own_address", 80))
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	// Age the ramp ledger so the attempt reads as three days old.
	for _, stmt := range []string{
		`UPDATE domain_send_counters SET day = day - 3`,
		`UPDATE sending_ramp_reservations SET day = day - 3`,
	} {
		if _, err := f.pool.Exec(f.ctx, stmt); err != nil {
			t.Fatalf("age ramp ledger (%s): %v", stmt, err)
		}
	}

	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: attempt, Outcome: sendingpolicy.SettlementProviderAccepted,
	}); err != nil {
		t.Fatalf("delayed settle: %v", err)
	}

	if _, confirmed := f.domainCounterOn(user, domain, sendDay); confirmed != 80 {
		t.Errorf("the send day's confirmed volume = %d, want 80", confirmed)
	}
	if reserved, _ := f.domainCounterOn(user, domain, f.ledgerToday()); reserved != 0 {
		t.Errorf("a delayed settlement created %d units on today's counter", reserved)
	}
	_, days, _ := f.rampScope(user, domain)
	if days != 1 {
		t.Errorf("active days = %d, want the send day credited once", days)
	}
	var qualified *time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT last_qualified_day FROM sending_ramp_scopes WHERE user_id = $1 AND domain = $2`,
		user, registrable(t, domain),
	).Scan(&qualified); err != nil {
		t.Fatalf("read last qualified day: %v", err)
	}
	if qualified == nil || !qualified.UTC().Equal(sendDay) {
		t.Errorf("last qualified day = %v, want the attempt's own day %s", qualified, sendDay.Format("2006-01-02"))
	}
}

// --- Lock order ------------------------------------------------------------

// TestTheRampTakesItsKeysInTheNamedSuborder makes the deadlock argument
// checkable instead of merely documented.
//
// The order is domain identity → registrable-domain scope → message
// reservation → UTC day counter, and prose cannot keep it. Each phase parks a
// competing session on one key, waits until the authorization is provably
// blocked on exactly that key, and then probes the rest with FOR UPDATE NOWAIT:
// every key EARLIER in the order must already be held, and every LATER one must
// still be free.
func TestTheRampTakesItsKeysInTheNamedSuborder(t *testing.T) {
	type probe struct {
		name  string
		query string
		args  func(user, domain, scope string, day time.Time) []any
		held  bool
	}
	domainProbe := probe{
		name:  "domain identity",
		query: `SELECT 1 FROM domains WHERE domain = $1 FOR UPDATE NOWAIT`,
		args:  func(_, domain, _ string, _ time.Time) []any { return []any{domain} },
		held:  true,
	}
	scopeProbe := probe{
		name:  "registrable-domain scope",
		query: `SELECT 1 FROM sending_ramp_scopes WHERE user_id = $1 AND domain = $2 FOR UPDATE NOWAIT`,
		args:  func(user, _, scope string, _ time.Time) []any { return []any{user, scope} },
	}
	counterProbe := probe{
		name:  "UTC day counter",
		query: `SELECT 1 FROM domain_send_counters WHERE user_id = $1 AND domain = $2 AND day = $3 FOR UPDATE NOWAIT`,
		args:  func(user, _, scope string, day time.Time) []any { return []any{user, scope, day} },
	}

	for _, phase := range []struct {
		name string
		// block is the key a competing session holds, which the authorization
		// must stop at.
		block  func(user, domain, scope string, day time.Time) (string, []any)
		probes []probe
		// preReserve pre-creates the message's ramp reservation, which is the
		// only way a competitor can hold that key before the gate does.
		preReserve bool
	}{
		{
			name: "blocked on the scope",
			block: func(user, _, scope string, _ time.Time) (string, []any) {
				return `SELECT 1 FROM sending_ramp_scopes WHERE user_id = $1 AND domain = $2 FOR UPDATE`,
					[]any{user, scope}
			},
			probes: []probe{domainProbe, counterProbe},
		},
		{
			name:       "blocked on the message reservation",
			preReserve: true,
			block: func(_, _, _ string, _ time.Time) (string, []any) {
				return `SELECT 1 FROM sending_ramp_reservations WHERE message_id = $1 FOR UPDATE`, nil
			},
			probes: []probe{domainProbe, func() probe { p := scopeProbe; p.held = true; return p }(), counterProbe},
		},
	} {
		t.Run(phase.name, func(t *testing.T) {
			f := newFixture(t)
			g := f.gate(rampPolicy(func(p *sendingpolicy.RuntimePolicy) {
				p.BudgetMode = sendingpolicy.ModeEnforce
				p.DefaultAccountDailyRecipients = 1000
				p.AllCustomerGlobalDailyRecipients = 1000
				p.ProbationGlobalDailyRecipients = 1000
			}))
			user := f.user("standard")
			agent, domain := f.customDomainAgent(user)
			scope := registrable(t, domain)
			day := f.ledgerToday()

			// One completed send so every key in the order exists and can be
			// probed rather than trivially returning no rows.
			if d := f.send(g, f.message(agent, "own_address", 1)); !d.Allow {
				t.Fatalf("priming send: %q", d.Reason)
			}

			messageID := f.message(agent, "own_address", 1)
			if phase.preReserve {
				f.legacyRampReserve(user, domain, messageID, 1, day)
			}
			_, ref := f.prepareMessage(g, messageID)
			_, attempt, err := g.Reserve(f.ctx, ref)
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}

			blockerConn, err := f.pool.Acquire(f.ctx)
			if err != nil {
				t.Fatalf("acquire blocker connection: %v", err)
			}
			defer blockerConn.Release()
			blocker, err := blockerConn.Begin(f.ctx)
			if err != nil {
				t.Fatalf("begin blocker: %v", err)
			}
			defer func() { _ = blocker.Rollback(f.ctx) }()
			query, args := phase.block(user, domain, scope, day)
			if args == nil {
				args = []any{messageID}
			}
			if _, err := blocker.Exec(f.ctx, query, args...); err != nil {
				t.Fatalf("hold the blocking key: %v", err)
			}

			done := make(chan error, 1)
			go func() {
				_, _, err := g.ConsumeAttempt(f.ctx, attempt)
				done <- err
			}()
			waitForLockWaiter(t, f)

			for _, p := range phase.probes {
				err := probeRowLock(t, f, p.query, p.args(user, domain, scope, day))
				switch {
				case p.held && err == nil:
					t.Errorf("%s is free, but it comes BEFORE the blocking key and must already be held", p.name)
				case !p.held && err != nil:
					t.Errorf("%s is already held, but it comes AFTER the blocking key: %v", p.name, err)
				}
			}

			if err := blocker.Rollback(f.ctx); err != nil {
				t.Fatalf("release the blocking key: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("authorization failed once the key was released: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("authorization never completed after the blocking key was released")
			}
		})
	}
}

// waitForLockWaiter blocks until some backend on this database is waiting on a
// row lock, which is how the test knows the authorization has reached — and
// stopped at — the parked key rather than racing past it.
func waitForLockWaiter(t *testing.T, f *fixture) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := f.pool.QueryRow(f.ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("read lock waiters: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the authorization never blocked on the parked key")
}

// probeRowLock reports whether a row lock is currently held by someone else,
// without ever waiting for it. Its own transaction is always rolled back, so
// the probe cannot become the next blocker.
func probeRowLock(t *testing.T, f *fixture, query string, args []any) error {
	t.Helper()
	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("acquire probe connection: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	_, err = tx.Exec(f.ctx, query, args...)
	return err
}
