package sendingpolicy_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// These tests drive the real ledger against real Postgres. Every address is a
// .test or example.test domain and every account is synthetic: nothing here may
// resemble a customer.
//
// Where a test is about POLICY NUMBERS it asserts the documented defaults
// directly. Where it is about MECHANISM it uses deliberately distinct, small,
// non-default caps per pool — if the code ever reads
// critical_operational_daily_recipients where it meant violation_, a test using
// the real 100/100 defaults would pass and a test using 2/3 fails.

const (
	fxHMAC     = `{"active":1,"keys":{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`
	fxOperator = `{"commitment_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI","recipients":{"1":"gate-operator@example.test"}}`
)

type fixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, ctx: context.Background(), pool: testutil.TestDB(t)}
}

// secrets builds the trust roots a hosted gate holds.
func (f *fixture) secrets() sendingpolicy.Secrets {
	f.t.Helper()
	keyring, err := sendingpolicy.LoadKeyring(fxHMAC)
	if err != nil {
		f.t.Fatalf("load keyring: %v", err)
	}
	recipients, err := sendingpolicy.LoadOperatorRecipients(fxOperator)
	if err != nil {
		f.t.Fatalf("load operator map: %v", err)
	}
	return sendingpolicy.Secrets{Keyring: keyring, Recipients: recipients}
}

// gate returns a config-source gate running `policy`.
//
// Config source is the right harness for almost every behavior here: it lets
// one test pin an exact policy without an activation CAS, and it is the same
// code path a self-host runs. The tests that are specifically about a policy
// CHANGE use two gates with different policies, which is exactly the mixed-slot
// situation the design has to survive.
func (f *fixture) gate(policy sendingpolicy.RuntimePolicy) sendingpolicy.Gate {
	f.t.Helper()
	secrets := f.secrets()
	// The operator registry is the permanent record a notice recipient is
	// checked against; register it the way the audited operator command does.
	module := sendingpolicy.NewModule(f.pool, secrets)
	if _, err := module.RegisterOperatorRecipients(f.ctx, "fixture", "gate test bootstrap"); err != nil {
		f.t.Fatalf("register operator recipients: %v", err)
	}
	return sendingpolicy.NewGate(f.pool, secrets, sendingpolicy.PolicySourceConfig, policy)
}

// enforcing returns the disabled default policy with budgets armed and the
// named caps replaced. Distinct values per pool make a mis-wired field visible.
func enforcingPolicy(mutate func(*sendingpolicy.RuntimePolicy)) sendingpolicy.RuntimePolicy {
	p := sendingpolicy.DisabledPolicy()
	p.BudgetMode = sendingpolicy.ModeEnforce
	if mutate != nil {
		mutate(&p)
	}
	return p
}

var userSeq int

func (f *fixture) user(class string) string {
	f.t.Helper()
	userSeq++
	id := fmt.Sprintf("usr_gate_%d", userSeq)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO users (id, email, google_subject, account_class) VALUES ($1, $2, $3, $4)`,
		id, id+"@example.test", "sub_"+id, class,
	); err != nil {
		f.t.Fatalf("insert user: %v", err)
	}
	return id
}

func (f *fixture) plan(userID, planCode string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO account_limits (user_id, plan_code, max_agents, max_domains, max_messages_month, max_storage_bytes)
		 VALUES ($1, $2, 100, 100, 1000000, 1000000000)
		 ON CONFLICT (user_id) DO UPDATE SET plan_code = EXCLUDED.plan_code`,
		userID, planCode,
	); err != nil {
		f.t.Fatalf("insert account limits: %v", err)
	}
}

func (f *fixture) pause(userID string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO account_sending_controls (user_id, state, reason, actor)
		 VALUES ($1, 'paused', 'test', 'test')
		 ON CONFLICT (user_id) DO UPDATE SET state = 'paused'`, userID,
	); err != nil {
		f.t.Fatalf("pause account: %v", err)
	}
}

var agentSeq int

func (f *fixture) agent(userID string) string {
	f.t.Helper()
	agentSeq++
	id := fmt.Sprintf("agt_gate_%d", agentSeq)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO agent_identities (id, user_id, registered_domain, name) VALUES ($1, $2, $3, $4)`,
		id, userID, "agents.e2a.dev", id,
	); err != nil {
		f.t.Fatalf("insert agent: %v", err)
	}
	return id
}

var messageSeq int

// message inserts an outbound message with `count` distinct recipients.
func (f *fixture) message(agentID, sentAs string, count int) string {
	f.t.Helper()
	messageSeq++
	id := fmt.Sprintf("msg_gate_%d", messageSeq)
	to := make([]string, count)
	for i := range to {
		to[i] = fmt.Sprintf("rcpt-%d-%d@example.test", messageSeq, i)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO messages (id, agent_id, direction, to_recipients, sent_as, status)
		 VALUES ($1, $2, 'outbound', $3, $4, 'sent')`,
		id, agentID, to, sentAs,
	); err != nil {
		f.t.Fatalf("insert message: %v", err)
	}
	return id
}

func (f *fixture) webhook(userID string) string {
	f.t.Helper()
	id := fmt.Sprintf("wh_gate_%d", messageSeq+1000)
	messageSeq++
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO webhooks (id, user_id, url, signing_secret, events)
		 VALUES ($1, $2, $3, $4, ARRAY['message.received'])`,
		id, userID, "https://hook.example.test/"+id, "secret",
	); err != nil {
		f.t.Fatalf("insert webhook: %v", err)
	}
	return id
}

// inTx runs fn inside a committed transaction, the way an acceptance surface
// calls the Prepare* methods.
func (f *fixture) inTx(fn func(tx pgx.Tx) error) {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(f.ctx)
		f.t.Fatalf("tx body: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		f.t.Fatalf("commit: %v", err)
	}
}

// prepareMessage runs the acceptance half and returns the operation reference.
func (f *fixture) prepareMessage(g sendingpolicy.Gate, messageID string) (sendingpolicy.AcceptanceDecision, sendingpolicy.OperationRef) {
	f.t.Helper()
	var decision sendingpolicy.AcceptanceDecision
	var ref sendingpolicy.OperationRef
	f.inTx(func(tx pgx.Tx) error {
		var err error
		decision, ref, err = g.PrepareExternalTx(f.ctx, tx, messageID)
		return err
	})
	return decision, ref
}

// send runs the whole worker sequence for one message and reports the final
// authorization decision.
func (f *fixture) send(g sendingpolicy.Gate, messageID string) sendingpolicy.Decision {
	f.t.Helper()
	accept, ref := f.prepareMessage(g, messageID)
	if accept != sendingpolicy.AcceptanceAccept {
		return sendingpolicy.Decision{Allow: false, Reason: string(accept)}
	}
	return f.authorize(g, ref)
}

// authorize runs Reserve then ConsumeAttempt, asserting only that the two
// agree: an early hold must not be followed by a final allow for the same
// exhausted pool.
func (f *fixture) authorize(g sendingpolicy.Gate, ref sendingpolicy.OperationRef) sendingpolicy.Decision {
	f.t.Helper()
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		f.t.Fatalf("reserve: %v", err)
	}
	decision, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		f.t.Fatalf("consume: %v", err)
	}
	if decision.Allow != (auth != nil) {
		f.t.Fatalf("allow=%v but token presence=%v — a hold must never return a token", decision.Allow, auth != nil)
	}
	return decision
}

func (f *fixture) counter(scope sendingpolicy.Scope, scopeID string) (reserved, confirmed int) {
	f.t.Helper()
	err := f.pool.QueryRow(f.ctx, `
		SELECT COALESCE(SUM(reserved_count), 0), COALESCE(SUM(confirmed_count), 0)
		  FROM sending_budget_counters
		 WHERE scope = $1 AND scope_id = $2`, string(scope), scopeID,
	).Scan(&reserved, &confirmed)
	if err != nil {
		f.t.Fatalf("read counter: %v", err)
	}
	return reserved, confirmed
}

// --- Documented policy numbers -------------------------------------------

// TestDocumentedDefaultCaps pins the six numbers the rollout record approves.
// They are the ones an activation gate signs off on, so a silent edit to any of
// them must fail a test rather than ship as a config tweak.
func TestDocumentedDefaultCaps(t *testing.T) {
	p := sendingpolicy.DisabledPolicy()
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"free account daily", p.DefaultAccountDailyRecipients, 100},
		{"shared domain account daily", p.SharedDomainAccountDailyRecip, 50},
		{"probation global daily", p.ProbationGlobalDailyRecipients, 150},
		{"all customer global daily", p.AllCustomerGlobalDailyRecipients, 5000},
		{"critical operational daily", p.CriticalOperationalDailyRecip, 100},
		{"violation operational daily", p.ViolationOperationalDailyRecip, 100},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// --- Account caps and classification -------------------------------------

// TestFreeAccountDailyCapBoundsDedicatedSending proves the Free ceiling applies
// to a customer's own verified domain, where the shared cap does not.
func TestFreeAccountDailyCapBoundsDedicatedSending(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 4
	}))
	user := f.user("standard")
	agent := f.agent(user)

	if d := f.send(g, f.message(agent, "own_address", 4)); !d.Allow {
		t.Fatalf("first 4 recipients must fit the Free allowance, got hold %q", d.Reason)
	}
	d := f.send(g, f.message(agent, "own_address", 1))
	if d.Allow {
		t.Fatal("the 5th recipient must be held")
	}
	if d.Reason != sendingpolicy.ReasonAccountDailyBudget {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonAccountDailyBudget)
	}
	if _, confirmed := f.counter(sendingpolicy.ScopeAccountSharedDaily, user); confirmed != 0 {
		t.Errorf("dedicated sending charged the shared-domain counter (%d)", confirmed)
	}
}

// TestPaidPlanRaisesAccountCapToTheGlobalCeiling proves a paid plan is not
// "unlimited" — it is limited at the platform ceiling and still recorded per
// account, so one paid customer's day remains visible and bounded.
func TestPaidPlanRaisesAccountCapToTheGlobalCeiling(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 2
		p.AllCustomerGlobalDailyRecipients = 6
	}))
	user := f.user("standard")
	f.plan(user, "pro")
	agent := f.agent(user)

	if d := f.send(g, f.message(agent, "own_address", 6)); !d.Allow {
		t.Fatalf("a paid account may use the whole ceiling, got hold %q", d.Reason)
	}
	if _, confirmed := f.counter(sendingpolicy.ScopeAccountDaily, user); confirmed != 6 {
		t.Errorf("account_daily confirmed = %d, want 6 — paid usage must still be recorded", confirmed)
	}
	if d := f.send(g, f.message(agent, "own_address", 1)); d.Allow {
		t.Fatal("a paid account must still stop at the platform ceiling")
	}
}

// TestUnknownPlanCodeFallsBackToFree proves an unnamed plan is not evidence of
// payment. A stale or attacker-influenced plan_code must not widen a cap.
func TestUnknownPlanCodeFallsBackToFree(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 2
		p.AllCustomerGlobalDailyRecipients = 50
	}))
	user := f.user("standard")
	f.plan(user, "enterprise_unlimited_totally_real")
	agent := f.agent(user)

	if d := f.send(g, f.message(agent, "own_address", 2)); !d.Allow {
		t.Fatalf("first 2 must fit, got hold %q", d.Reason)
	}
	if d := f.send(g, f.message(agent, "own_address", 1)); d.Allow {
		t.Fatal("an unknown plan code must be capped at the Free default")
	}
}

// TestSharedDomainCapAppliesRegardlessOfPlan proves paying does not buy more
// shared-reputation sending: the way to send more is a verified custom domain.
func TestSharedDomainCapAppliesRegardlessOfPlan(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.SharedDomainAccountDailyRecip = 3
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
		p.ProbationGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	f.plan(user, "scale")
	agent := f.agent(user)

	if d := f.send(g, f.message(agent, "relay", 3)); !d.Allow {
		t.Fatalf("first 3 shared recipients must fit, got hold %q", d.Reason)
	}
	d := f.send(g, f.message(agent, "relay", 1))
	if d.Allow {
		t.Fatal("a paid account must still be bounded by the shared-domain cap")
	}
	if d.Reason != sendingpolicy.ReasonAccountSharedBudget {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonAccountSharedBudget)
	}
}

// TestSharedMailboxAndNotificationsShareOneAccountCounter is the mixed-path
// invariant: a customer cannot get a second 50-recipient allowance by causing
// platform notification mail instead of sending its own.
func TestSharedMailboxAndNotificationsShareOneAccountCounter(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.SharedDomainAccountDailyRecip = 3
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
		p.ProbationGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	agent := f.agent(user)

	// Two shared-mailbox recipients.
	if d := f.send(g, f.message(agent, "relay", 2)); !d.Allow {
		t.Fatalf("shared message must fit, got hold %q", d.Reason)
	}

	// One HITL notification: same pool, one unit.
	held := f.message(agent, "relay", 1)
	var noticeRef sendingpolicy.OperationRef
	f.inTx(func(tx pgx.Tx) error {
		var err error
		noticeRef, err = g.PrepareNotificationTx(f.ctx, tx, sendingpolicy.NewHITLNotificationRef(held))
		return err
	})
	if d := f.authorize(g, noticeRef); !d.Allow {
		t.Fatalf("the third unit must fit, got hold %q", d.Reason)
	}

	// A webhook-health notification is the fourth unit and must be held.
	hook := f.webhook(user)
	var hookRef sendingpolicy.OperationRef
	f.inTx(func(tx pgx.Tx) error {
		var err error
		hookRef, err = g.PrepareNotificationTx(f.ctx, tx, sendingpolicy.NewWebhookHealthNotificationRef(hook))
		return err
	})
	d := f.authorize(g, hookRef)
	if d.Allow {
		t.Fatal("customer-triggered notifications must share the account's shared-domain pool")
	}
	if d.Reason != sendingpolicy.ReasonAccountSharedBudget {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonAccountSharedBudget)
	}
	if _, confirmed := f.counter(sendingpolicy.ScopeAccountSharedDaily, user); confirmed != 3 {
		t.Errorf("shared counter confirmed = %d, want exactly the cap 3", confirmed)
	}
}

// TestProbationPoolBoundsSybilGrowth proves the shared probation pool is what
// stops "more accounts" from multiplying the per-account allowance.
func TestProbationPoolBoundsSybilGrowth(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.SharedDomainAccountDailyRecip = 2
		p.ProbationGlobalDailyRecipients = 4
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
	}))

	for i := 0; i < 2; i++ {
		agent := f.agent(f.user("standard"))
		if d := f.send(g, f.message(agent, "relay", 2)); !d.Allow {
			t.Fatalf("account %d must fit the probation pool, got hold %q", i, d.Reason)
		}
	}
	third := f.agent(f.user("standard"))
	d := f.send(g, f.message(third, "relay", 1))
	if d.Allow {
		t.Fatal("a fresh account must not find fresh probation capacity")
	}
	if d.Reason != sendingpolicy.ReasonGlobalProbation {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonGlobalProbation)
	}
}

// TestDedicatedDomainSkipsProbationAndSharedPools proves the classification is
// derived from the server-owned sent_as column, not from anything a caller says.
func TestDedicatedDomainSkipsProbationAndSharedPools(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.SharedDomainAccountDailyRecip = 1
		p.ProbationGlobalDailyRecipients = 1
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
	}))
	agent := f.agent(f.user("standard"))

	if d := f.send(g, f.message(agent, "own_address", 5)); !d.Allow {
		t.Fatalf("custom-domain sending must not touch the shared pools, got hold %q", d.Reason)
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeGlobalProbation, "probation"); reserved != 0 {
		t.Errorf("probation counter = %d, want 0", reserved)
	}
}

// TestUnknownSentAsIsTreatedAsShared proves the fail-closed direction: a row
// whose sent_as is absent or unrecognized gets the STRICTER cap, never the
// exemption.
func TestUnknownSentAsIsTreatedAsShared(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.SharedDomainAccountDailyRecip = 1
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
		p.ProbationGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	agent := f.agent(user)

	messageSeq++
	id := fmt.Sprintf("msg_gate_null_%d", messageSeq)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO messages (id, agent_id, direction, to_recipients, status)
		 VALUES ($1, $2, 'outbound', ARRAY['a@example.test'], 'sent')`, id, agent,
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if d := f.send(g, id); !d.Allow {
		t.Fatalf("first unit must fit, got hold %q", d.Reason)
	}
	if _, confirmed := f.counter(sendingpolicy.ScopeAccountSharedDaily, user); confirmed != 1 {
		t.Fatalf("an unknown sent_as must charge the shared counter, confirmed = %d", confirmed)
	}
}

// TestTrustedClassesBypassEveryPoolAndOthersDoNot proves the exemption list is
// closed and positive. `demo`, an unknown class, and the empty string are all
// budgeted — a public demo must not become the abuse bypass.
func TestTrustedClassesBypassEveryPoolAndOthersDoNot(t *testing.T) {
	for _, tc := range []struct {
		class  string
		exempt bool
	}{
		{"system", true},
		{"internal", true},
		{"standard", false},
		{"demo", false},
	} {
		t.Run(tc.class, func(t *testing.T) {
			f := newFixture(t)
			g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
				p.DefaultAccountDailyRecipients = 1
				p.SharedDomainAccountDailyRecip = 1
				p.ProbationGlobalDailyRecipients = 1
				p.AllCustomerGlobalDailyRecipients = 1
			}))
			user := f.user(tc.class)
			agent := f.agent(user)

			d := f.send(g, f.message(agent, "relay", 5))
			if d.Allow != tc.exempt {
				t.Fatalf("class %q allow = %v, want %v (reason %q)", tc.class, d.Allow, tc.exempt, d.Reason)
			}
			reserved, _ := f.counter(sendingpolicy.ScopeGlobalAll, "all-customers")
			if tc.exempt && reserved != 0 {
				t.Errorf("a trusted class charged the global pool (%d)", reserved)
			}
		})
	}
}

// TestPublicFeedbackUsesGlobalPoolsAndNoAccountCounter proves the
// unauthenticated form shares the reputation surface without inventing an
// account to blame.
func TestPublicFeedbackUsesGlobalPoolsAndNoAccountCounter(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))

	ref, err := g.PreparePublicFeedback(f.ctx, sendingpolicy.NewPublicFeedbackRef(
		"sub_1", []string{"feedback@example.test", "Feedback@example.test", "ops@example.test"}))
	if err != nil {
		t.Fatalf("prepare public feedback: %v", err)
	}
	if d := f.authorize(g, ref); !d.Allow {
		t.Fatalf("public feedback must be allowed with capacity, got hold %q", d.Reason)
	}

	// The duplicate spelling counts once.
	if _, confirmed := f.counter(sendingpolicy.ScopeGlobalAll, "all-customers"); confirmed != 2 {
		t.Errorf("global_all confirmed = %d, want 2 (duplicates collapse)", confirmed)
	}
	if _, confirmed := f.counter(sendingpolicy.ScopeGlobalProbation, "probation"); confirmed != 2 {
		t.Errorf("global_probation confirmed = %d, want 2", confirmed)
	}
	var accountRows int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM sending_budget_counters WHERE scope IN ('account_daily','account_shared_daily')`,
	).Scan(&accountRows); err != nil {
		t.Fatalf("count account counters: %v", err)
	}
	if accountRows != 0 {
		t.Errorf("public feedback created %d account counter rows, want 0", accountRows)
	}
}

// --- Notices --------------------------------------------------------------

// noticeEvent inserts a committed pause event with both audiences, the way the
// pause transition does.
func (f *fixture) pauseNotice(userID string) string {
	f.t.Helper()
	controlEvent := fmt.Sprintf("ace_%s", userID)
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO account_sending_control_events (id, account_ref, old_state, new_state, reason, actor, expires_at)
		VALUES ($1, $2, 'active', 'paused', 'test', 'test', now() + interval '90 days')`,
		controlEvent, userID,
	); err != nil {
		f.t.Fatalf("insert control event: %v", err)
	}
	eventID := fmt.Sprintf("spn_%s", userID)
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO sending_protection_notice_events
		    (id, account_ref, kind, reason_code, source_event_id, expires_at)
		VALUES ($1, $2, 'pause', 'manual', $3, now() + interval '90 days')`,
		eventID, userID, controlEvent,
	); err != nil {
		f.t.Fatalf("insert notice event: %v", err)
	}
	for _, audience := range []string{"owner", "operator"} {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO sending_protection_notice_deliveries (event_id, audience) VALUES ($1, $2)`,
			eventID, audience,
		); err != nil {
			f.t.Fatalf("insert notice delivery: %v", err)
		}
	}
	return eventID
}

// TestPauseNoticeUsesTheCriticalPoolAndReachesBothAudiences proves a paused
// account still gets told, on a pool no customer traffic can exhaust, and that
// the operator copy goes to the registered mailbox version rather than to
// anything derived from the customer.
func TestPauseNoticeUsesTheCriticalPoolAndReachesBothAudiences(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		// Every customer pool is exhausted at zero headroom; the notice must
		// still go out.
		p.DefaultAccountDailyRecipients = 1
		p.SharedDomainAccountDailyRecip = 1
		p.ProbationGlobalDailyRecipients = 1
		p.AllCustomerGlobalDailyRecipients = 1
		p.CriticalOperationalDailyRecip = 5
	}))
	user := f.user("standard")
	f.pause(user)
	eventID := f.pauseNotice(user)

	for _, audience := range []sendingpolicy.Audience{sendingpolicy.AudienceOwner, sendingpolicy.AudienceOperator} {
		var ref sendingpolicy.OperationRef
		f.inTx(func(tx pgx.Tx) error {
			var err error
			ref, err = g.PrepareProtectionNoticeTx(f.ctx, tx, sendingpolicy.NewProtectionNoticeRef(eventID, audience))
			return err
		})
		_, attempt, err := g.Reserve(f.ctx, ref)
		if err != nil {
			t.Fatalf("%s reserve: %v", audience, err)
		}
		decision, auth, err := g.ConsumeAttempt(f.ctx, attempt)
		if err != nil {
			t.Fatalf("%s consume: %v", audience, err)
		}
		if !decision.Allow {
			t.Fatalf("%s notice held (%q) — a pause notice must survive exhausted customer pools", audience, decision.Reason)
		}
		got := auth.AuthorizedRecipients()
		want := user + "@example.test"
		if audience == sendingpolicy.AudienceOperator {
			want = "gate-operator@example.test"
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s recipients = %v, want [%s]", audience, got, want)
		}
	}
	if _, confirmed := f.counter(sendingpolicy.ScopeGlobalCritical, "critical-operational"); confirmed != 2 {
		t.Errorf("critical pool confirmed = %d, want 2 (one per audience)", confirmed)
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeGlobalViolation, "violation-operational"); reserved != 0 {
		t.Errorf("a pause notice touched the violation pool (%d) — the pools must be independent", reserved)
	}
}

// TestEnforcedAccountDenialEnqueuesExactlyOneNoticePerScopePerDay proves the
// customer hears about its own violation once a day per failed scope, not once
// per held message.
func TestEnforcedAccountDenialEnqueuesExactlyOneNoticePerScopePerDay(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.SharedDomainAccountDailyRecip = 1
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
		p.ProbationGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	agent := f.agent(user)

	if d := f.send(g, f.message(agent, "relay", 1)); !d.Allow {
		t.Fatalf("first unit must fit: %q", d.Reason)
	}
	for i := 0; i < 3; i++ {
		if d := f.send(g, f.message(agent, "relay", 1)); d.Allow {
			t.Fatalf("denial %d unexpectedly allowed", i)
		}
	}

	var events int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM sending_protection_notice_events
		 WHERE kind = 'budget_violation' AND account_ref = $1`, user,
	).Scan(&events); err != nil {
		t.Fatalf("count violation events: %v", err)
	}
	if events != 1 {
		t.Fatalf("violation events = %d, want exactly 1 for three denials", events)
	}

	rows, err := f.pool.Query(f.ctx, `
		SELECT audience FROM sending_protection_notice_deliveries AS d
		  JOIN sending_protection_notice_events AS e ON e.id = d.event_id
		 WHERE e.kind = 'budget_violation' ORDER BY audience`)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	defer rows.Close()
	var audiences []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan audience: %v", err)
		}
		audiences = append(audiences, a)
	}
	if len(audiences) != 2 || audiences[0] != "operator" || audiences[1] != "owner" {
		t.Errorf("audiences = %v, want [operator owner]", audiences)
	}
}

// TestGlobalDenialBlamesNobodyAndCoalesces proves a platform guardrail is
// reported once per scope and day as an operator incident — never as a
// violation email to the innocent accounts that happened to collide with it.
func TestGlobalDenialBlamesNobodyAndCoalesces(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.AllCustomerGlobalDailyRecipients = 1
		p.DefaultAccountDailyRecipients = 100
		p.SharedDomainAccountDailyRecip = 100
		p.ProbationGlobalDailyRecipients = 100
	}))

	first := f.agent(f.user("standard"))
	if d := f.send(g, f.message(first, "own_address", 1)); !d.Allow {
		t.Fatalf("first unit must fit: %q", d.Reason)
	}
	for i := 0; i < 2; i++ {
		agent := f.agent(f.user("standard"))
		if d := f.send(g, f.message(agent, "own_address", 1)); d.Allow {
			t.Fatal("global ceiling must hold later accounts")
		}
	}
	// A public-feedback caller hitting the same exhausted pool must coalesce
	// into the same incident rather than opening a second one.
	feedback, err := g.PreparePublicFeedback(f.ctx, sendingpolicy.NewPublicFeedbackRef("sub_g", []string{"ops@example.test"}))
	if err != nil {
		t.Fatalf("prepare feedback: %v", err)
	}
	if d := f.authorize(g, feedback); d.Allow {
		t.Fatal("public feedback must also be held by the exhausted global pool")
	}

	var guardrails, violations int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FILTER (WHERE kind = 'global_guardrail'), count(*) FILTER (WHERE kind = 'budget_violation')
		   FROM sending_protection_notice_events`,
	).Scan(&guardrails, &violations); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if guardrails != 1 {
		t.Errorf("guardrail events = %d, want 1", guardrails)
	}
	if violations != 0 {
		t.Errorf("global exhaustion produced %d customer violation notices, want 0", violations)
	}

	var audiences int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM sending_protection_notice_deliveries AS d
		  JOIN sending_protection_notice_events AS e ON e.id = d.event_id
		 WHERE e.kind = 'global_guardrail'`).Scan(&audiences); err != nil {
		t.Fatalf("count guardrail deliveries: %v", err)
	}
	if audiences != 1 {
		t.Errorf("guardrail deliveries = %d, want 1 (operator only)", audiences)
	}
}

// TestOperationalDenialDoesNotRecurse proves the notice system cannot feed
// itself: a notice held by its own exhausted pool must not enqueue a notice
// about being held.
func TestOperationalDenialDoesNotRecurse(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.CriticalOperationalDailyRecip = 1
	}))
	user := f.user("standard")
	eventID := f.pauseNotice(user)

	for _, audience := range []sendingpolicy.Audience{sendingpolicy.AudienceOwner, sendingpolicy.AudienceOperator} {
		var ref sendingpolicy.OperationRef
		f.inTx(func(tx pgx.Tx) error {
			var err error
			ref, err = g.PrepareProtectionNoticeTx(f.ctx, tx, sendingpolicy.NewProtectionNoticeRef(eventID, audience))
			return err
		})
		f.authorize(g, ref)
	}

	var extra int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM sending_protection_notice_events WHERE kind <> 'pause'`,
	).Scan(&extra); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if extra != 0 {
		t.Errorf("an exhausted operational pool enqueued %d further notices, want 0", extra)
	}
}

// TestGlobalGuardrailHasNoOwnerAudience proves the schema invariant is also an
// interface invariant: there is nobody to blame for a platform incident.
func TestGlobalGuardrailHasNoOwnerAudience(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))

	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO sending_protection_notice_events
		    (id, kind, reason_code, budget_scope, ledger_day, expires_at)
		VALUES ('spn_guard', 'global_guardrail', 'global_budget_exhausted', 'global_all',
		        CURRENT_DATE, now() + interval '90 days')`); err != nil {
		t.Fatalf("insert guardrail event: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO sending_protection_notice_deliveries (event_id, audience) VALUES ('spn_guard', 'owner')`,
	); err != nil {
		t.Fatalf("insert owner delivery: %v", err)
	}

	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	_, err = g.PrepareProtectionNoticeTx(f.ctx, tx,
		sendingpolicy.NewProtectionNoticeRef("spn_guard", sendingpolicy.AudienceOwner))
	if !errors.Is(err, sendingpolicy.ErrAudienceNotAllowed) {
		t.Fatalf("error = %v, want ErrAudienceNotAllowed", err)
	}
}

// --- Modes ----------------------------------------------------------------

// TestShadowModeRecordsDemandWithoutBlockingOrBlaming proves the shadow window
// produces the number the activation gate has to approve: real demand, past the
// cap, with no customer effect at all.
func TestShadowModeRecordsDemandWithoutBlockingOrBlaming(t *testing.T) {
	f := newFixture(t)
	policy := sendingpolicy.DisabledPolicy()
	policy.BudgetMode = sendingpolicy.ModeShadow
	policy.SharedDomainAccountDailyRecip = 1
	g := f.gate(policy)

	user := f.user("standard")
	agent := f.agent(user)
	for i := 0; i < 3; i++ {
		if d := f.send(g, f.message(agent, "relay", 1)); !d.Allow {
			t.Fatalf("shadow mode must never deny (attempt %d held: %q)", i, d.Reason)
		}
	}
	_, confirmed := f.counter(sendingpolicy.ScopeAccountSharedDaily, user)
	if confirmed != 3 {
		t.Errorf("shadow confirmed = %d, want 3 — the counter must record demand past the cap", confirmed)
	}
	var notices int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sending_protection_notice_events`).Scan(&notices); err != nil {
		t.Fatalf("count notices: %v", err)
	}
	if notices != 0 {
		t.Errorf("shadow mode enqueued %d notices, want 0", notices)
	}
}

// TestDisabledModeChargesNothingButStillAuthorizes proves the production state
// this slice actually ships in: the authorization seam is live, the pools are
// untouched.
func TestDisabledModeChargesNothingButStillAuthorizes(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	agent := f.agent(f.user("standard"))

	if d := f.send(g, f.message(agent, "relay", 500)); !d.Allow {
		t.Fatalf("disabled mode must allow, got hold %q", d.Reason)
	}
	var counters int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sending_budget_counters`).Scan(&counters); err != nil {
		t.Fatalf("count counters: %v", err)
	}
	if counters != 0 {
		t.Errorf("disabled mode wrote %d counter rows, want 0", counters)
	}
}

// TestPausedAccountIsRefusedAtBothDoors proves the pause is checked at
// acceptance (so no unsendable mail is queued) and again at final authorization
// (so mail queued before the pause never leaves), independently of budget mode.
func TestPausedAccountIsRefusedAtBothDoors(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	user := f.user("standard")
	agent := f.agent(user)

	// Queued while active.
	queued := f.message(agent, "relay", 1)
	accept, ref := f.prepareMessage(g, queued)
	if accept != sendingpolicy.AcceptanceAccept {
		t.Fatalf("acceptance = %q, want accept", accept)
	}

	f.pause(user)

	if d := f.authorize(g, ref); d.Allow {
		t.Fatal("a pause must stop mail that was already queued")
	} else if d.Reason != sendingpolicy.ReasonAccountPaused {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonAccountPaused)
	}

	if accept, _ := f.prepareMessage(g, f.message(agent, "relay", 1)); accept != sendingpolicy.AcceptanceSendingPaused {
		t.Errorf("acceptance = %q, want sending_paused", accept)
	}
}

// TestLoopbackIsNotProviderBound proves the one exempt path stays exempt: an
// agent writing to itself never reaches SES, so it gets no operation and
// consumes nothing.
func TestLoopbackIsNotProviderBound(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 1
		p.SharedDomainAccountDailyRecip = 1
	}))
	agent := f.agent(f.user("standard"))

	messageSeq++
	id := fmt.Sprintf("msg_gate_loop_%d", messageSeq)
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO messages (id, agent_id, direction, method, to_recipients, sent_as, status)
		VALUES ($1, $2, 'outbound', 'loopback', ARRAY['self@example.test'], 'own_address', 'sent')`,
		id, agent,
	); err != nil {
		t.Fatalf("insert loopback message: %v", err)
	}
	accept, ref := f.prepareMessage(g, id)
	if accept != sendingpolicy.AcceptanceAccept {
		t.Fatalf("acceptance = %q, want accept", accept)
	}
	if !ref.IsZero() {
		t.Error("a loopback message must not get a provider operation")
	}
	var ops int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sending_provider_operations`).Scan(&ops); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	if ops != 0 {
		t.Errorf("loopback created %d operations, want 0", ops)
	}
}
