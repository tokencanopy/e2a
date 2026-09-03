package sendingpolicy_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// This file is about the durable attempt: the thing that makes "exactly one SES
// call per charged unit of capacity" true across crashes, retries, races, and
// deliberate abuse of the reference types.
//
// A fake socket counter stands in for SES. Every test that claims "zero
// provider calls" asserts it against that counter, not against an absence of
// errors, because the failure that matters is a call that happened anyway.

// socket is a stand-in for the SMTP adapter: it will only "connect" for a token
// it successfully redeemed, exactly as the real adapter must.
type socket struct {
	mu    sync.Mutex
	calls int
}

func (s *socket) send(t *testing.T, g sendingpolicy.Gate, auth *sendingpolicy.ProviderAuthorization, envelope []string) error {
	t.Helper()
	if auth == nil {
		return errors.New("no authorization")
	}
	if _, err := auth.ValidateEnvelope(envelope); err != nil {
		return err
	}
	if err := g.RedeemProviderCall(t.Context(), *auth); err != nil {
		return err
	}
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return nil
}

func (s *socket) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// prepareAndReserve runs the first two worker steps for a fresh message.
func (f *fixture) prepareAndReserve(g sendingpolicy.Gate, agentID string, count int) (sendingpolicy.OperationRef, sendingpolicy.AttemptRef) {
	f.t.Helper()
	_, ref := f.prepareMessage(g, f.message(agentID, "own_address", count))
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		f.t.Fatalf("reserve: %v", err)
	}
	return ref, attempt
}

func (f *fixture) reservationState(operationID string, attempt int) (state, callState string) {
	f.t.Helper()
	err := f.pool.QueryRow(f.ctx, `
		SELECT state, call_state FROM sending_budget_reservations
		 WHERE operation_id = $1 AND submission_attempt = $2`, operationID, attempt,
	).Scan(&state, &callState)
	if err != nil {
		f.t.Fatalf("read reservation: %v", err)
	}
	return state, callState
}

func (f *fixture) currentAttempt(operationID string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT current_attempt FROM sending_provider_operations WHERE operation_id = $1`, operationID,
	).Scan(&n); err != nil {
		f.t.Fatalf("read current attempt: %v", err)
	}
	return n
}

// TestOnlyOneWorkerAuthorizesOneOrdinal is the duplicate-worker invariant. Two
// executions of the same job routinely observe the same reserved ordinal; only
// one of them may ever get a token, or one message becomes two SES calls
// against one charge.
func TestOnlyOneWorkerAuthorizesOneOrdinal(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	_, attempt := f.prepareAndReserve(g, agent, 1)

	var wg sync.WaitGroup
	tokens := make([]*sendingpolicy.ProviderAuthorization, 2)
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
			tokens[i], errs[i] = auth, err
		}(i)
	}
	wg.Wait()

	granted := 0
	for i := range tokens {
		if tokens[i] != nil {
			granted++
			continue
		}
		if !errors.Is(errs[i], sendingpolicy.ErrAttemptStale) {
			t.Errorf("loser %d error = %v, want ErrAttemptStale", i, errs[i])
		}
	}
	if granted != 1 {
		t.Fatalf("%d workers were authorized for one ordinal, want exactly 1", granted)
	}
}

// TestConfirmedAttemptForcesTheNextOrdinal proves the one-way rule: once
// capacity is irrevocably spent, the only continuation is a greater ordinal
// with fresh capacity. This is what bounds physical exposure across a crash
// between authorization and the socket.
func TestConfirmedAttemptForcesTheNextOrdinal(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 4
	}))
	user := f.user("standard")
	agent := f.agent(user)
	ref, attempt := f.prepareAndReserve(g, agent, 1)

	if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
		t.Fatalf("first authorization: auth=%v err=%v", auth, err)
	}
	if got := f.currentAttempt(ref.ID()); got != 1 {
		t.Fatalf("current attempt = %d, want 1 before the retry", got)
	}

	// The worker dies here. A later execution starts again at Reserve.
	_, next, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if next.Attempt() != 2 {
		t.Fatalf("second ordinal = %d, want 2", next.Attempt())
	}
	if _, auth, err := g.ConsumeAttempt(f.ctx, next); err != nil || auth == nil {
		t.Fatalf("second authorization: auth=%v err=%v", auth, err)
	}

	// Both attempts are charged. A retry costs capacity precisely because it
	// exposes SES again; refunding the first would make crash-looping free.
	if _, confirmed := f.counter(sendingpolicy.ScopeAccountDaily, user); confirmed != 2 {
		t.Errorf("confirmed = %d, want 2 — a retry must consume fresh capacity", confirmed)
	}
}

// TestRedemptionIsSingleUseAndPrecedesIO proves the token is spent, not merely
// checked: a second redemption of the same token opens no socket.
func TestRedemptionIsSingleUseAndPrecedesIO(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	ref, attempt := f.prepareAndReserve(g, agent, 1)

	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	envelope := auth.AuthorizedRecipients()

	var s socket
	if err := s.send(t, g, auth, envelope); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if state, callState := f.reservationState(ref.ID(), 1); state != "confirmed" || callState != "started" {
		t.Errorf("after redemption state=%s call_state=%s, want confirmed/started", state, callState)
	}

	if err := s.send(t, g, auth, envelope); !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
		t.Fatalf("second redemption error = %v, want ErrAuthorizationInvalid", err)
	}
	if s.count() != 1 {
		t.Fatalf("socket opened %d times for one authorization, want 1", s.count())
	}
	if got := f.currentAttempt(ref.ID()); got <= 1 {
		t.Errorf("current attempt = %d, want > 1 after an invalidated redemption", got)
	}
}

// TestEnvelopeMismatchFailsBeforeRedemption proves a token cannot be pointed at
// recipients it did not authorize — the check runs before any redemption, so a
// mismatch costs nothing and sends nothing.
func TestEnvelopeMismatchFailsBeforeRedemption(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	ref, attempt := f.prepareAndReserve(g, agent, 2)

	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	authorized := auth.AuthorizedRecipients()

	var s socket
	for name, envelope := range map[string][]string{
		"extra recipient":   append(append([]string(nil), authorized...), "attacker@example.test"),
		"swapped recipient": {authorized[0], "attacker@example.test"},
		"missing recipient": {authorized[0]},
	} {
		if err := s.send(t, g, auth, envelope); !errors.Is(err, sendingpolicy.ErrEnvelopeMismatch) {
			t.Errorf("%s: error = %v, want ErrEnvelopeMismatch", name, err)
		}
	}
	if s.count() != 0 {
		t.Fatalf("socket opened %d times on mismatched envelopes, want 0", s.count())
	}
	if state, callState := f.reservationState(ref.ID(), 1); callState != "authorized" {
		t.Errorf("state=%s call_state=%s — a rejected envelope must not consume the token", state, callState)
	}
}

// TestEnvelopeComparisonIgnoresCaseAndOrder proves the normalization contract
// the adapter depends on: it may reassemble To/Cc/Bcc in any order, and a
// mailbox spelled with different case is not a different recipient.
func TestEnvelopeComparisonIgnoresCaseAndOrder(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	_, attempt := f.prepareAndReserve(g, agent, 3)

	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	authorized := auth.AuthorizedRecipients()
	reshaped := []string{
		upperFirst(authorized[2]),
		authorized[0],
		upperFirst(authorized[1]),
	}
	if _, err := auth.ValidateEnvelope(reshaped); err != nil {
		t.Fatalf("reshaped envelope rejected: %v", err)
	}
}

// TestDuplicateSpellingsCannotAmplifyOneChargedUnit is the reputation
// amplifier the envelope contract exists to close.
//
// Normalization collapses case, so a token for one mailbox would "match" an
// envelope of fifty case-variant spellings of that mailbox — one unit of
// charged budget authorizing fifty RCPT TO commands, and fifty chances to
// bounce against the shared SES reputation. The accounting rule that duplicates
// count once only holds if the submitted envelope IS the deduplicated set.
func TestDuplicateSpellingsCannotAmplifyOneChargedUnit(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	_, attempt := f.prepareAndReserve(g, agent, 1)

	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	authorized := auth.AuthorizedRecipients()
	if len(authorized) != 1 {
		t.Fatalf("authorized = %v, want one recipient", authorized)
	}

	padded := []string{authorized[0]}
	for i := 0; i < 49; i++ {
		padded = append(padded, caseVariant(authorized[0], i))
	}

	var s socket
	if err := s.send(t, g, auth, padded); !errors.Is(err, sendingpolicy.ErrEnvelopeMismatch) {
		t.Fatalf("50 case-variant spellings for 1 charged unit = %v, want ErrEnvelopeMismatch", err)
	}
	if s.count() != 0 {
		t.Fatalf("opened %d sockets for a padded envelope, want 0", s.count())
	}

	// An exact repeat of the same spelling is refused for the same reason: the
	// caller must collapse To/Cc overlap before submitting, because that is
	// what it was charged for.
	if _, err := auth.ValidateEnvelope([]string{authorized[0], authorized[0]}); !errors.Is(err, sendingpolicy.ErrEnvelopeMismatch) {
		t.Errorf("repeated recipient = %v, want ErrEnvelopeMismatch", err)
	}
}

// caseVariant flips the case of the i-th flippable byte of the local part.
func caseVariant(addr string, i int) string {
	b := []byte(addr)
	at := 0
	for at < len(b) && b[at] != '@' {
		at++
	}
	seen := 0
	for j := 0; j < at; j++ {
		if b[j] >= 'a' && b[j] <= 'z' {
			if seen == i%at {
				b[j] -= 32
				return string(b)
			}
			seen++
		}
	}
	return string(b)
}

func upperFirst(addr string) string {
	if addr == "" {
		return addr
	}
	return string(addr[0]-32) + addr[1:]
}

// TestForgedReferencesAuthorizeNothing proves the reference types are not
// capabilities. A caller that round-trips an operation through JSON — the only
// serialization this package permits — gets an ID and nothing else, and a
// fabricated ID resolves to no authority at all.
func TestForgedReferencesAuthorizeNothing(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	_, ref := f.prepareMessage(g, f.message(agent, "own_address", 1))

	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal ref: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("inspect wire form: %v", err)
	}
	if len(wire) != 2 || wire["v"] != float64(1) || wire["id"] != ref.ID() {
		t.Fatalf("wire form = %v, want exactly {v:1, id:%q}", wire, ref.ID())
	}

	var revived sendingpolicy.OperationRef
	if err := json.Unmarshal(raw, &revived); err != nil {
		t.Fatalf("unmarshal ref: %v", err)
	}
	if revived.Purpose() != "" {
		t.Errorf("a deserialized reference carried purpose %q — it must be reloaded from the row", revived.Purpose())
	}
	// It still works, because every method reloads from the durable row.
	if _, _, err := g.Reserve(f.ctx, revived); err != nil {
		t.Fatalf("reserve on a round-tripped reference: %v", err)
	}

	// A fabricated ID names nothing.
	var forged sendingpolicy.OperationRef
	if err := json.Unmarshal([]byte(`{"v":1,"id":"op_not_a_real_operation"}`), &forged); err != nil {
		t.Fatalf("unmarshal forged: %v", err)
	}
	if _, _, err := g.Reserve(f.ctx, forged); !errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
		t.Errorf("forged reference error = %v, want ErrSourceUnavailable", err)
	}

	// An unsupported version is refused rather than reinterpreted.
	if err := json.Unmarshal([]byte(`{"v":2,"id":"op_x"}`), &forged); err == nil {
		t.Error("an unsupported reference version must not decode")
	}
}

// TestDeferReleasesBudgetAndCancelDoesToo proves a message that never reaches
// the provider gives its capacity back, and that neither call can undo a
// started one.
func TestDeferReleasesBudgetAndCancelDoesToo(t *testing.T) {
	for name, release := range map[string]func(sendingpolicy.Gate, sendingpolicy.AttemptRef) error{
		"defer": func(g sendingpolicy.Gate, a sendingpolicy.AttemptRef) error {
			return g.DeferAttempt(t.Context(), a)
		},
		"cancel": func(g sendingpolicy.Gate, a sendingpolicy.AttemptRef) error {
			return g.CancelAttempt(t.Context(), a)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
				p.DefaultAccountDailyRecipients = 2
			}))
			user := f.user("standard")
			agent := f.agent(user)
			ref, attempt := f.prepareAndReserve(g, agent, 2)

			if reserved, _ := f.counter(sendingpolicy.ScopeAccountDaily, user); reserved != 2 {
				t.Fatalf("reserved = %d, want 2 after Reserve", reserved)
			}
			if err := release(g, attempt); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if reserved, _ := f.counter(sendingpolicy.ScopeAccountDaily, user); reserved != 0 {
				t.Fatalf("reserved = %d after %s, want 0", reserved, name)
			}
			// Repeating it is a no-op, not a second refund.
			if err := release(g, attempt); err != nil {
				t.Fatalf("repeat %s: %v", name, err)
			}
			if reserved, _ := f.counter(sendingpolicy.ScopeAccountDaily, user); reserved != 0 {
				t.Fatalf("reserved = %d after a repeated %s, want 0", reserved, name)
			}

			// Once the attempt is authorized its capacity is spent, so a
			// release is refused rather than silently recorded: the counter's
			// confirmed floor would keep the units anyway, and a row claiming
			// a give-back that never happened is worse than an error.
			_, again, err := g.Reserve(f.ctx, ref)
			if err != nil {
				t.Fatalf("re-reserve: %v", err)
			}
			_, auth, err := g.ConsumeAttempt(f.ctx, again)
			if err != nil || auth == nil {
				t.Fatalf("authorize: auth=%v err=%v", auth, err)
			}
			if err := release(g, again); !errors.Is(err, sendingpolicy.ErrAttemptStale) {
				t.Fatalf("%s after authorization = %v, want ErrAttemptStale", name, err)
			}
			if _, confirmed := f.counter(sendingpolicy.ScopeAccountDaily, user); confirmed != 2 {
				t.Fatalf("confirmed = %d after a refused %s, want 2", confirmed, name)
			}

			// And after a real provider call the reason is more specific.
			var s socket
			if err := s.send(t, g, auth, auth.AuthorizedRecipients()); err != nil {
				t.Fatalf("send: %v", err)
			}
			if err := release(g, again); !errors.Is(err, sendingpolicy.ErrProviderCallStarted) {
				t.Fatalf("%s after a started call = %v, want ErrProviderCallStarted", name, err)
			}
		})
	}
}

// TestPlanDowngradeBetweenReserveAndConsume is the cliff the pricing work
// flagged: two 75-unit reservations admitted under a paid cap must not both
// submit once the account is Free.
func TestPlanDowngradeBetweenReserveAndConsume(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 5000
	}))
	user := f.user("standard")
	f.plan(user, "pro")
	agent := f.agent(user)

	_, first := f.prepareAndReserve(g, agent, 75)
	_, second := f.prepareAndReserve(g, agent, 75)

	// The billing writer downgrades the account after both reservations.
	f.plan(user, "free")

	d1, auth1, err := g.ConsumeAttempt(f.ctx, first)
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	d2, auth2, err := g.ConsumeAttempt(f.ctx, second)
	if err != nil {
		t.Fatalf("second consume: %v", err)
	}
	allowed := 0
	for _, d := range []sendingpolicy.Decision{d1, d2} {
		if d.Allow {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("%d of two 75-unit attempts were authorized under a 100-unit cap, want exactly 1", allowed)
	}
	_ = auth1
	_ = auth2
	if _, confirmed := f.counter(sendingpolicy.ScopeAccountDaily, user); confirmed != 75 {
		t.Errorf("confirmed = %d, want 75 — the downgrade must bind at final authorization", confirmed)
	}
}

// TestPolicyChangeBetweenReserveAndConsumeArmsTheNewControls proves a control
// armed after Reserve still applies, and one disarmed after Reserve gives its
// units back rather than leaking them until midnight.
func TestPolicyChangeBetweenReserveAndConsumeArmsTheNewControls(t *testing.T) {
	f := newFixture(t)
	user := f.user("standard")
	agent := f.agent(user)

	// Reserve under a policy with no budgets at all.
	off := f.gate(sendingpolicy.DisabledPolicy())
	_, ref := f.prepareMessage(off, f.message(agent, "relay", 3))
	_, attempt, err := off.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeAccountSharedDaily, user); reserved != 0 {
		t.Fatalf("a disabled policy charged %d units at reserve", reserved)
	}

	// The operator arms enforcement with a cap this attempt cannot fit.
	on := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.SharedDomainAccountDailyRecip = 2
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
		p.ProbationGlobalDailyRecipients = 100
	}))
	d, auth, err := on.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if d.Allow || auth != nil {
		t.Fatal("a newly armed budget must bind an attempt reserved before it was armed")
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeAccountSharedDaily, user); reserved != 0 {
		t.Errorf("a denied attempt left %d units reserved, want 0", reserved)
	}
}

// TestDisarmedControlReleasesItsUnits is the mirror image: units taken under an
// armed control must come back when the control is disarmed, not sit reserved
// against a limit nobody is enforcing.
func TestDisarmedControlReleasesItsUnits(t *testing.T) {
	f := newFixture(t)
	user := f.user("standard")
	agent := f.agent(user)

	on := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 10
	}))
	_, ref := f.prepareMessage(on, f.message(agent, "own_address", 3))
	_, attempt, err := on.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeAccountDaily, user); reserved != 3 {
		t.Fatalf("reserved = %d, want 3", reserved)
	}

	off := f.gate(sendingpolicy.DisabledPolicy())
	d, auth, err := off.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !d.Allow || auth == nil {
		t.Fatalf("a disabled budget must allow, got %q", d.Reason)
	}
	reserved, confirmed := f.counter(sendingpolicy.ScopeAccountDaily, user)
	if reserved != 0 || confirmed != 0 {
		t.Errorf("counter reserved=%d confirmed=%d after disarming, want 0/0", reserved, confirmed)
	}
}

// TestAccountClassChangeBetweenReserveAndConsume proves the final class read is
// authoritative: promoting an account to a trusted class between the two calls
// releases its units, and it is the LOCKED read that decides, not the one
// Reserve happened to see.
func TestAccountClassChangeBetweenReserveAndConsume(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 10
	}))
	user := f.user("standard")
	agent := f.agent(user)
	_, attempt := f.prepareAndReserve(g, agent, 4)

	if _, err := f.pool.Exec(f.ctx, `UPDATE users SET account_class = 'internal' WHERE id = $1`, user); err != nil {
		t.Fatalf("promote account: %v", err)
	}

	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !d.Allow || auth == nil {
		t.Fatalf("a trusted account must not be budgeted, got hold %q", d.Reason)
	}
	if reserved, confirmed := f.counter(sendingpolicy.ScopeAccountDaily, user); reserved != 0 || confirmed != 0 {
		t.Errorf("counter reserved=%d confirmed=%d, want 0/0 after the class change", reserved, confirmed)
	}
}

// TestSharedClassificationIsImmutableAcrossAttempts proves the reputation class
// is decided once from the server-owned column and cannot be edited into
// something cheaper afterwards.
func TestSharedClassificationIsImmutableAcrossAttempts(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.SharedDomainAccountDailyRecip = 5
		p.DefaultAccountDailyRecipients = 100
		p.AllCustomerGlobalDailyRecipients = 100
		p.ProbationGlobalDailyRecipients = 100
	}))
	user := f.user("standard")
	agent := f.agent(user)
	messageID := f.message(agent, "relay", 1)
	_, ref := f.prepareMessage(g, messageID)

	// Rewrite the source row to claim a dedicated domain, which is the
	// cheapest possible lie: it would skip the 50/day shared cap entirely.
	if _, err := f.pool.Exec(f.ctx, `UPDATE messages SET sent_as = 'own_address' WHERE id = $1`, messageID); err != nil {
		t.Fatalf("rewrite sent_as: %v", err)
	}

	if d := f.authorize(g, ref); !d.Allow {
		t.Fatalf("authorize: %q", d.Reason)
	}
	if _, confirmed := f.counter(sendingpolicy.ScopeAccountSharedDaily, user); confirmed != 1 {
		t.Errorf("shared counter confirmed = %d, want 1 — the class is fixed at preparation", confirmed)
	}
}

// TestAdjacentDayPhysicalSubmissionBound proves the day boundary is honest.
// A run that spans midnight may expose at most one cap on each side, and every
// physical call must be backed by a confirmed attempt on the day it was charged.
func TestAdjacentDayPhysicalSubmissionBound(t *testing.T) {
	const cap = 3
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = cap
		p.AllCustomerGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	agent := f.agent(user)

	var s socket
	drain := func() int {
		sent := 0
		for i := 0; i < cap*3; i++ {
			_, ref := f.prepareMessage(g, f.message(agent, "own_address", 1))
			_, attempt, err := g.Reserve(f.ctx, ref)
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
			if err != nil {
				t.Fatalf("consume: %v", err)
			}
			if !d.Allow {
				continue
			}
			if err := s.send(t, g, auth, auth.AuthorizedRecipients()); err != nil {
				t.Fatalf("send: %v", err)
			}
			sent++
		}
		return sent
	}

	dayOne := drain()
	if dayOne != cap {
		t.Fatalf("day one sent %d, want exactly the cap %d", dayOne, cap)
	}

	// Roll the ledger forward a day by aging every counter row, which is what
	// midnight does from the ledger's point of view.
	if _, err := f.pool.Exec(f.ctx, `UPDATE sending_budget_counters SET day = day - 1`); err != nil {
		t.Fatalf("advance day: %v", err)
	}

	dayTwo := drain()
	if dayTwo != cap {
		t.Fatalf("day two sent %d, want exactly the cap %d", dayTwo, cap)
	}
	if s.count() != 2*cap {
		t.Fatalf("adjacent-day physical calls = %d, want at most %d", s.count(), 2*cap)
	}

	// Every physical call is backed by exactly one started attempt.
	var started int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM sending_budget_reservations WHERE call_state = 'started'`,
	).Scan(&started); err != nil {
		t.Fatalf("count started attempts: %v", err)
	}
	if started != s.count() {
		t.Errorf("started attempts = %d but sockets = %d — every call must be charged", started, s.count())
	}
}

// TestDeletionCannotRefundConfirmedExposure is the deletion-safety invariant.
// The ledger references accounts only by opaque ID and has no foreign key into
// the customer tree, so a delete-and-resend loop cannot mint free reputation
// exposure by erasing its own history.
func TestDeletionCannotRefundConfirmedExposure(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.AllCustomerGlobalDailyRecipients = 4
		p.DefaultAccountDailyRecipients = 100
	}))
	user := f.user("standard")
	agent := f.agent(user)

	if d := f.send(g, f.message(agent, "own_address", 3)); !d.Allow {
		t.Fatalf("first send: %q", d.Reason)
	}
	if _, confirmed := f.counter(sendingpolicy.ScopeGlobalAll, "all-customers"); confirmed != 3 {
		t.Fatalf("global confirmed = %d, want 3", confirmed)
	}

	// Irreversible account deletion, cascading through the customer tree.
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	reserved, confirmed := f.counter(sendingpolicy.ScopeGlobalAll, "all-customers")
	if confirmed != 3 || reserved != 3 {
		t.Fatalf("after deletion reserved=%d confirmed=%d, want 3/3 — exposure must survive its account", reserved, confirmed)
	}

	// The replacement account finds only the remaining headroom.
	fresh := f.agent(f.user("standard"))
	if d := f.send(g, f.message(fresh, "own_address", 1)); !d.Allow {
		t.Fatalf("the remaining headroom must be usable: %q", d.Reason)
	}
	if d := f.send(g, f.message(fresh, "own_address", 1)); d.Allow {
		t.Fatal("deleting an account must not refund the global pool")
	}
}

// TestDeletedOwnerHoldsRatherThanMails proves a customer purpose whose account
// vanished mid-flight is held, not sent, and that its early reservation is
// given back.
func TestDeletedOwnerHoldsRatherThanMails(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 10
	}))
	user := f.user("standard")
	agent := f.agent(user)
	_, attempt := f.prepareAndReserve(g, agent, 2)

	if _, err := f.pool.Exec(f.ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if d.Allow || auth != nil {
		t.Fatal("a deleted account must not be authorized")
	}
	if d.Reason != sendingpolicy.ReasonAccountDeleted {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonAccountDeleted)
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeAccountDaily, user); reserved != 0 {
		t.Errorf("a held attempt left %d units reserved, want 0", reserved)
	}
}

// TestOwnerNoticeRecipientChangeInvalidatesTheAttempt proves the last-moment
// recipient recheck: an owner who edits their address after authorization is
// never mailed at the retired one.
func TestOwnerNoticeRecipientChangeInvalidatesTheAttempt(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	user := f.user("standard")
	eventID := f.pauseNotice(user)

	var ref sendingpolicy.OperationRef
	f.inTx(func(tx pgx.Tx) error {
		var err error
		ref, err = g.PrepareProtectionNoticeTx(f.ctx, tx,
			sendingpolicy.NewProtectionNoticeRef(eventID, sendingpolicy.AudienceOwner))
		return err
	})
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	if got := auth.AuthorizedRecipients(); len(got) != 1 || got[0] != user+"@example.test" {
		t.Fatalf("authorized recipients = %v", got)
	}

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE users SET email = $2 WHERE id = $1`, user, "moved-"+user+"@example.test"); err != nil {
		t.Fatalf("change owner address: %v", err)
	}

	var s socket
	if err := s.send(t, g, auth, auth.AuthorizedRecipients()); !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
		t.Fatalf("redemption after an owner change = %v, want ErrAuthorizationInvalid", err)
	}
	if s.count() != 0 {
		t.Fatalf("mailed the retired address %d times, want 0", s.count())
	}
	if got := f.currentAttempt(ref.ID()); got <= 1 {
		t.Errorf("current attempt = %d, want a strictly greater ordinal after invalidation", got)
	}
}

// TestOperatorNoticeRotationInvalidatesTheAttempt is the operator-side mirror:
// rotating the mailbox map retires an authorization built against the old
// version rather than mailing a mailbox the operator has retired.
func TestOperatorNoticeRotationInvalidatesTheAttempt(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	user := f.user("standard")
	eventID := f.pauseNotice(user)

	var ref sendingpolicy.OperationRef
	f.inTx(func(tx pgx.Tx) error {
		var err error
		ref, err = g.PrepareProtectionNoticeTx(f.ctx, tx,
			sendingpolicy.NewProtectionNoticeRef(eventID, sendingpolicy.AudienceOperator))
		return err
	})
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}

	// A slot that has already picked up a rotated map and a policy selecting
	// version 2 must refuse to redeem a version-1 token.
	rotated := `{"commitment_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI",` +
		`"recipients":{"1":"gate-operator@example.test","2":"gate-operator-two@example.test"}}`
	recipients, err := sendingpolicy.LoadOperatorRecipients(rotated)
	if err != nil {
		t.Fatalf("load rotated map: %v", err)
	}
	secrets := f.secrets()
	secrets.Recipients = recipients
	module := sendingpolicy.NewModule(f.pool, secrets)
	if _, err := module.RegisterOperatorRecipients(f.ctx, "fixture", "rotate"); err != nil {
		t.Fatalf("register rotated map: %v", err)
	}
	policy := enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.OperatorNoticeRecipientVersion = 2
	})
	rotatedGate := sendingpolicy.NewGate(f.pool, secrets, sendingpolicy.PolicySourceConfig, policy)

	var s socket
	if err := s.send(t, rotatedGate, auth, auth.AuthorizedRecipients()); !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
		t.Fatalf("redemption after rotation = %v, want ErrAuthorizationInvalid", err)
	}
	if s.count() != 0 {
		t.Fatalf("mailed the retired operator mailbox %d times, want 0", s.count())
	}
}

// TestNoticeOperationIsStableAcrossRetries proves a retried notice keeps one
// logical identity: the same operation, a greater ordinal, never a second
// notice.
func TestNoticeOperationIsStableAcrossRetries(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	user := f.user("standard")
	eventID := f.pauseNotice(user)

	var first, second sendingpolicy.OperationRef
	for _, target := range []*sendingpolicy.OperationRef{&first, &second} {
		ref := target
		f.inTx(func(tx pgx.Tx) error {
			var err error
			*ref, err = g.PrepareProtectionNoticeTx(f.ctx, tx,
				sendingpolicy.NewProtectionNoticeRef(eventID, sendingpolicy.AudienceOwner))
			return err
		})
	}
	if first.ID() != second.ID() {
		t.Fatalf("notice operation changed across retries: %s vs %s", first.ID(), second.ID())
	}

	_, attempt, err := g.Reserve(f.ctx, first)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	_, next, err := g.Reserve(f.ctx, second)
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if next.Attempt() != attempt.Attempt()+1 {
		t.Errorf("retry ordinal = %d, want %d", next.Attempt(), attempt.Attempt()+1)
	}
}

// TestSettleProviderValidatesAndIsIdempotent pins the settlement contract this
// slice owns: closed outcomes, a real attempt, and no effect on the sending
// budget. The ramp half arrives with the ramp adapter.
func TestSettleProviderValidatesAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	user := f.user("standard")
	agent := f.agent(user)
	_, attempt := f.prepareAndReserve(g, agent, 2)
	if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}

	for i := 0; i < 2; i++ {
		if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
			Attempt: attempt, Outcome: sendingpolicy.SettlementProviderAccepted,
		}); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	if _, confirmed := f.counter(sendingpolicy.ScopeAccountDaily, user); confirmed != 2 {
		t.Errorf("confirmed = %d after settlement, want 2 — settlement must not move the budget", confirmed)
	}

	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: attempt, Outcome: sendingpolicy.SettlementOutcome("delivered_probably"),
	}); err == nil {
		t.Error("an outcome outside the closed set must be refused")
	}
}

// TestConcurrentAccountSendsNeverExceedTheCap runs the real race: many workers
// on one account with a cap that only some of them can fit. The ledger must
// admit exactly the cap, with no deadlock and no over-admission.
func TestConcurrentAccountSendsNeverExceedTheCap(t *testing.T) {
	const cap = 5
	const workers = 12

	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = cap
		p.SharedDomainAccountDailyRecip = cap
		p.ProbationGlobalDailyRecipients = 1000
		p.AllCustomerGlobalDailyRecipients = 1000
	}))
	user := f.user("standard")
	agent := f.agent(user)

	refs := make([]sendingpolicy.OperationRef, workers)
	for i := range refs {
		_, refs[i] = f.prepareMessage(g, f.message(agent, "relay", 1))
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	var failures []error
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(ref sendingpolicy.OperationRef) {
			defer wg.Done()
			_, attempt, err := g.Reserve(f.ctx, ref)
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Errorf("reserve: %w", err))
				mu.Unlock()
				return
			}
			d, _, err := g.ConsumeAttempt(f.ctx, attempt)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, fmt.Errorf("consume: %w", err))
				return
			}
			if d.Allow {
				allowed++
			}
		}(refs[i])
	}
	wg.Wait()

	for _, err := range failures {
		t.Errorf("worker error (a deadlock or lock-order bug shows up here): %v", err)
	}
	if allowed != cap {
		t.Fatalf("%d of %d concurrent workers were authorized, want exactly the cap %d", allowed, workers, cap)
	}
	_, confirmed := f.counter(sendingpolicy.ScopeAccountDaily, user)
	if confirmed != cap {
		t.Errorf("confirmed = %d, want %d", confirmed, cap)
	}
}

// TestHoldAdvisesTheNextRollover proves a day-bounded hold tells the worker
// when to come back, so a full account snoozes instead of spinning.
func TestHoldAdvisesTheNextRollover(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 1
	}))
	agent := f.agent(f.user("standard"))
	if d := f.send(g, f.message(agent, "own_address", 1)); !d.Allow {
		t.Fatalf("first send: %q", d.Reason)
	}
	d := f.send(g, f.message(agent, "own_address", 1))
	if d.Allow {
		t.Fatal("the second send must be held")
	}
	now := time.Now().UTC()
	if !d.RetryAt.After(now) || d.RetryAt.Sub(now) > 24*time.Hour+time.Minute {
		t.Errorf("retry at %s is not the next UTC midnight (now %s)", d.RetryAt, now)
	}
	if d.RetryAt.Hour() != 0 || d.RetryAt.Minute() != 0 {
		t.Errorf("retry at %s is not a midnight boundary", d.RetryAt)
	}
}

// TestTokenCorrelationMatchesTheDurableRow proves the header value the adapter
// will stamp on the SES submission is the one delivery feedback can be matched
// back to. A token carrying an unstored ID would leave the detector silently
// blind rather than visibly broken, so the durable row is authoritative.
func TestTokenCorrelationMatchesTheDurableRow(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	ref, attempt := f.prepareAndReserve(g, agent, 2)

	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	headers, err := auth.ValidateEnvelope(auth.AuthorizedRecipients())
	if err != nil {
		t.Fatalf("validate envelope: %v", err)
	}

	var stored string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT correlation_id FROM sending_feedback_correlations
		 WHERE operation_id = $1 AND submission_attempt = $2`, ref.ID(), attempt.Attempt(),
	).Scan(&stored); err != nil {
		t.Fatalf("read correlation: %v", err)
	}
	if headers.AttemptCorrelationID != stored {
		t.Errorf("token correlation %q != stored %q", headers.AttemptCorrelationID, stored)
	}
	if headers.TenantRequired || headers.TenantName != "" {
		t.Errorf("tenant headers = %v/%q, want none before the tenant rollout", headers.TenantRequired, headers.TenantName)
	}

	// One provenance row per authorized recipient, HMAC only: no address may
	// outlive the message in this table.
	var recipients int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM sending_feedback_recipients WHERE correlation_id = $1`, stored,
	).Scan(&recipients); err != nil {
		t.Fatalf("count recipients: %v", err)
	}
	if recipients != 2 {
		t.Errorf("recipient provenance rows = %d, want 2", recipients)
	}
}

// TestReserveJudgesTheAuthoritativePlanAndClass replaces an earlier design in
// which Reserve guessed.
//
// Guessing was wrong in both directions, and neither direction was cheap.
// Judging the account scope PESSIMISTICALLY deferred a paying customer's mail
// to the next midnight, because the worker treats an early hold as "snooze".
// Judging it OPTIMISTICALLY — while still charging the shared global pools at
// their real limits — let one Free account hold the whole platform pool in
// reservations it could never confirm. Reading the class and the plan costs one
// FOR SHARE each and removes both failure modes.
func TestReserveJudgesTheAuthoritativePlanAndClass(t *testing.T) {
	t.Run("paid account is not deferred", func(t *testing.T) {
		f := newFixture(t)
		g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
			p.DefaultAccountDailyRecipients = 2
			p.AllCustomerGlobalDailyRecipients = 50
		}))
		user := f.user("standard")
		f.plan(user, "pro")
		agent := f.agent(user)

		_, ref := f.prepareMessage(g, f.message(agent, "own_address", 10))
		early, attempt, err := g.Reserve(f.ctx, ref)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if !early.Allow {
			t.Fatalf("Reserve deferred a paid account within its cap (%q)", early.Reason)
		}
		if d, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || !d.Allow || auth == nil {
			t.Fatalf("final authorization: allow=%v reason=%q err=%v", d.Allow, d.Reason, err)
		}
	})

	t.Run("free account cannot hold the global pool", func(t *testing.T) {
		f := newFixture(t)
		g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
			p.DefaultAccountDailyRecipients = 2
			p.AllCustomerGlobalDailyRecipients = 200
		}))
		agent := f.agent(f.user("standard"))

		// Ten 20-recipient messages from an account entitled to 2/day. Every
		// one must be refused at its own scope WITHOUT taking global units.
		for i := 0; i < 10; i++ {
			_, ref := f.prepareMessage(g, f.message(agent, "own_address", 20))
			d, _, err := g.Reserve(f.ctx, ref)
			if err != nil {
				t.Fatalf("reserve %d: %v", i, err)
			}
			if d.Allow {
				t.Fatalf("reserve %d allowed 20 recipients against a cap of 2", i)
			}
			if d.Reason != sendingpolicy.ReasonAccountDailyBudget {
				t.Fatalf("reserve %d reason = %q, want the account scope", i, d.Reason)
			}
		}
		if reserved, _ := f.counter(sendingpolicy.ScopeGlobalAll, "all-customers"); reserved != 0 {
			t.Fatalf("a Free account parked %d units in the platform pool, want 0", reserved)
		}

		// A paying tenant still finds the pool empty and is not paged about it.
		paid := f.user("standard")
		f.plan(paid, "scale")
		if d := f.send(g, f.message(f.agent(paid), "own_address", 5)); !d.Allow {
			t.Fatalf("an unrelated paid tenant was denied: %q", d.Reason)
		}
		var guardrails int
		if err := f.pool.QueryRow(f.ctx,
			`SELECT count(*) FROM sending_protection_notice_events WHERE kind = 'global_guardrail'`,
		).Scan(&guardrails); err != nil {
			t.Fatalf("count guardrails: %v", err)
		}
		if guardrails != 0 {
			t.Errorf("phantom reservations paged the operator %d times, want 0", guardrails)
		}
	})

	t.Run("trusted class is not deferred either", func(t *testing.T) {
		f := newFixture(t)
		g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
			p.DefaultAccountDailyRecipients = 1
			p.AllCustomerGlobalDailyRecipients = 1
			p.ProbationGlobalDailyRecipients = 1
			p.SharedDomainAccountDailyRecip = 1
		}))
		// Exhaust every pool with ordinary traffic first.
		if d := f.send(g, f.message(f.agent(f.user("standard")), "relay", 1)); !d.Allow {
			t.Fatalf("priming send: %q", d.Reason)
		}

		// The prober must still work during exactly this situation. An
		// exemption that lives only in ConsumeAttempt is dead here, because the
		// worker snoozes on Reserve's hold and never gets that far.
		prober := f.agent(f.user("system"))
		_, ref := f.prepareMessage(g, f.message(prober, "relay", 5))
		early, attempt, err := g.Reserve(f.ctx, ref)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if !early.Allow {
			t.Fatalf("Reserve deferred a trusted account (%q) during an exhausted-pool window", early.Reason)
		}
		if d, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || !d.Allow || auth == nil {
			t.Fatalf("trusted final authorization: allow=%v reason=%q err=%v", d.Allow, d.Reason, err)
		}
	})
}

// TestEarlyDenialStillOwesItsNotice proves the violation notice is written
// wherever the denial happens.
//
// Every scope except the account-daily one is judged identically by Reserve and
// ConsumeAttempt, so a denial that stops at Reserve is a denial the customer
// and the operator would otherwise never hear about: the worker snoozes, and
// the notice writer that used to live only in final authorization is never
// reached.
func TestEarlyDenialStillOwesItsNotice(t *testing.T) {
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
		t.Fatalf("first unit: %q", d.Reason)
	}
	// This one is refused by Reserve, before ConsumeAttempt is ever called.
	_, ref := f.prepareMessage(g, f.message(agent, "relay", 1))
	early, _, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if early.Allow {
		t.Fatal("the second shared unit must be refused")
	}

	var events int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM sending_protection_notice_events
		 WHERE kind = 'budget_violation' AND account_ref = $1`, user,
	).Scan(&events); err != nil {
		t.Fatalf("count violation events: %v", err)
	}
	if events != 1 {
		t.Fatalf("violation events after an early denial = %d, want 1", events)
	}
	var audiences int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM sending_protection_notice_deliveries AS d
		  JOIN sending_protection_notice_events AS e ON e.id = d.event_id
		 WHERE e.kind = 'budget_violation'`).Scan(&audiences); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if audiences != 2 {
		t.Errorf("deliveries = %d, want owner + operator", audiences)
	}
}

// TestArmingDoesNotReleaseUnitsNobodyTook is the phantom-release invariant.
//
// A reservation made while budgets were disabled holds no capacity, so the
// first ConsumeAttempt after enforcement is armed must not hand capacity back
// on its behalf. Getting this wrong is invisible in isolation — the counter's
// confirmed floor hides it whenever nothing else is in flight — and shows up
// as the global pool silently over-admitting on the exact day phase 4 arms.
func TestArmingDoesNotReleaseUnitsNobodyTook(t *testing.T) {
	f := newFixture(t)
	armed := enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.AllCustomerGlobalDailyRecipients = 100
		p.DefaultAccountDailyRecipients = 100
	})

	// Account A reserves while the budget is disabled: nothing is charged.
	off := f.gate(sendingpolicy.DisabledPolicy())
	agentA := f.agent(f.user("standard"))
	_, refA := f.prepareMessage(off, f.message(agentA, "own_address", 5))
	_, attemptA, err := off.Reserve(f.ctx, refA)
	if err != nil {
		t.Fatalf("reserve A: %v", err)
	}

	// Account B reserves under the armed policy: 10 real units in flight.
	on := f.gate(armed)
	agentB := f.agent(f.user("standard"))
	_, refB := f.prepareMessage(on, f.message(agentB, "own_address", 10))
	if _, _, err := on.Reserve(f.ctx, refB); err != nil {
		t.Fatalf("reserve B: %v", err)
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeGlobalAll, "all-customers"); reserved != 10 {
		t.Fatalf("global reserved = %d, want 10", reserved)
	}

	if d, auth, err := on.ConsumeAttempt(f.ctx, attemptA); err != nil || !d.Allow || auth == nil {
		t.Fatalf("A final authorization: allow=%v reason=%q err=%v", d.Allow, d.Reason, err)
	}
	reserved, confirmed := f.counter(sendingpolicy.ScopeGlobalAll, "all-customers")
	if reserved != 15 || confirmed != 5 {
		t.Fatalf("global reserved=%d confirmed=%d, want 15/5 — B's in-flight units must survive A's arrival",
			reserved, confirmed)
	}
}

// TestDeletedSourceReleasesRatherThanStrands closes a repeatable denial-of-
// service against the shared pools.
//
// When the source row disappears between Reserve and ConsumeAttempt, treating
// that as an ERROR rolls the transaction back with the attempt still marked
// reserved — and because every later execution fails at the same point, those
// units sit on the global pools until midnight with nothing able to release
// them. Reserve, delete, repeat.
func TestDeletedSourceReleasesRatherThanStrands(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.AllCustomerGlobalDailyRecipients = 10
		p.DefaultAccountDailyRecipients = 10
	}))
	agent := f.agent(f.user("standard"))
	messageID := f.message(agent, "own_address", 4)
	_, ref := f.prepareMessage(g, messageID)
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeGlobalAll, "all-customers"); reserved != 4 {
		t.Fatalf("global reserved = %d, want 4", reserved)
	}

	if _, err := f.pool.Exec(f.ctx, `DELETE FROM messages WHERE id = $1`, messageID); err != nil {
		t.Fatalf("delete message: %v", err)
	}

	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("consume after delete: %v", err)
	}
	if d.Allow || auth != nil {
		t.Fatal("a deleted source must not authorize")
	}
	if d.Reason != sendingpolicy.ReasonSourceUnavailable {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonSourceUnavailable)
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeGlobalAll, "all-customers"); reserved != 0 {
		t.Fatalf("deleting the source stranded %d units on the shared pool, want 0", reserved)
	}

	// The pool is fully usable again by everyone else.
	if d := f.send(g, f.message(f.agent(f.user("standard")), "own_address", 10)); !d.Allow {
		t.Fatalf("the released capacity is not reusable: %q", d.Reason)
	}
}

// TestStaleRedemptionDoesNotRetireTheLiveAttempt proves a late worker cannot
// destroy a live authorization it has nothing to do with.
//
// A stale token is already stale; retiring it retires nothing. Bumping the
// ordinal unconditionally instead lets it retire the CURRENT attempt another
// worker is holding — whose confirmed capacity is spent and never refunded, so
// a supply of stale tokens becomes a livelock that burns the account's budget
// without a single SES call.
func TestStaleRedemptionDoesNotRetireTheLiveAttempt(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.DefaultAccountDailyRecipients = 20
	}))
	agent := f.agent(f.user("standard"))
	ref, first := f.prepareAndReserve(g, agent, 1)

	_, staleAuth, err := g.ConsumeAttempt(f.ctx, first)
	if err != nil || staleAuth == nil {
		t.Fatalf("first authorization: auth=%v err=%v", staleAuth, err)
	}

	// A second worker goes around and gets the live ordinal.
	_, second, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	_, liveAuth, err := g.ConsumeAttempt(f.ctx, second)
	if err != nil || liveAuth == nil {
		t.Fatalf("second authorization: auth=%v err=%v", liveAuth, err)
	}
	if got := f.currentAttempt(ref.ID()); got != 2 {
		t.Fatalf("current attempt = %d, want 2", got)
	}

	// The first worker finally wakes up and redeems its superseded token.
	var s socket
	if err := s.send(t, g, staleAuth, staleAuth.AuthorizedRecipients()); !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
		t.Fatalf("stale redemption = %v, want ErrAuthorizationInvalid", err)
	}
	if got := f.currentAttempt(ref.ID()); got != 2 {
		t.Fatalf("a stale redemption moved the ordinal to %d, want it left at 2", got)
	}

	// The live token still works.
	if err := s.send(t, g, liveAuth, liveAuth.AuthorizedRecipients()); err != nil {
		t.Fatalf("live redemption after a stale one: %v", err)
	}
	if s.count() != 1 {
		t.Fatalf("sockets = %d, want 1", s.count())
	}
}

// TestSettledNoticeCannotBeResent proves terminality lives in this module and
// not in the caller's query.
func TestSettledNoticeCannotBeResent(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	user := f.user("standard")
	eventID := f.pauseNotice(user)

	var ref sendingpolicy.OperationRef
	f.inTx(func(tx pgx.Tx) error {
		var err error
		ref, err = g.PrepareProtectionNoticeTx(f.ctx, tx,
			sendingpolicy.NewProtectionNoticeRef(eventID, sendingpolicy.AudienceOwner))
		return err
	})
	if d := f.authorize(g, ref); !d.Allow {
		t.Fatalf("first notice: %q", d.Reason)
	}

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE sending_protection_notice_deliveries SET state = 'sent'
		  WHERE event_id = $1 AND audience = 'owner'`, eventID); err != nil {
		t.Fatalf("settle delivery: %v", err)
	}

	// Preparation refuses outright.
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = g.PrepareProtectionNoticeTx(f.ctx, tx,
		sendingpolicy.NewProtectionNoticeRef(eventID, sendingpolicy.AudienceOwner))
	_ = tx.Rollback(f.ctx)
	if !errors.Is(err, sendingpolicy.ErrNoticeSettled) {
		t.Fatalf("re-preparing a sent notice = %v, want ErrNoticeSettled", err)
	}

	// And a caller still holding the old reference cannot authorize another
	// physical send either.
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if d.Allow || auth != nil {
		t.Fatal("a settled notice must not authorize a second send")
	}
	if d.Reason != sendingpolicy.ReasonNoticeSettled {
		t.Errorf("reason = %q, want %q", d.Reason, sendingpolicy.ReasonNoticeSettled)
	}
}

// TestTerminalHoldsAreMarkedTerminal proves the worker can tell "come back
// later" from "this can never proceed".
//
// Without the distinction every hold reads as retryable, and an operation that
// is permanently void — its account deleted, its notice already sent, its
// reputation class no longer the one it was derived from — would be snoozed
// forever instead of failed once. A terminal hold carries no RetryAt because
// there is no time at which the answer changes.
func TestTerminalHoldsAreMarkedTerminal(t *testing.T) {
	t.Run("deleted account", func(t *testing.T) {
		f := newFixture(t)
		g := f.gate(enforcingPolicy(nil))
		user := f.user("standard")
		_, attempt := f.prepareAndReserve(g, f.agent(user), 1)
		if _, err := f.pool.Exec(f.ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
			t.Fatalf("delete account: %v", err)
		}
		d, _, err := g.ConsumeAttempt(f.ctx, attempt)
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		assertTerminal(t, d, sendingpolicy.ReasonAccountDeleted)
	})

	t.Run("reputation class changed", func(t *testing.T) {
		f := newFixture(t)
		g := f.gate(enforcingPolicy(nil))
		agent := f.agent(f.user("standard"))
		messageID := f.message(agent, "own_address", 1)
		_, ref := f.prepareMessage(g, messageID)
		if _, err := f.pool.Exec(f.ctx,
			`UPDATE messages SET sent_as = 'relay' WHERE id = $1`, messageID); err != nil {
			t.Fatalf("downgrade sent_as: %v", err)
		}
		assertTerminal(t, f.authorize(g, ref), sendingpolicy.ReasonClassChanged)
	})

	t.Run("deleted source", func(t *testing.T) {
		f := newFixture(t)
		g := f.gate(enforcingPolicy(nil))
		agent := f.agent(f.user("standard"))
		messageID := f.message(agent, "own_address", 1)
		_, ref := f.prepareMessage(g, messageID)
		_, attempt, err := g.Reserve(f.ctx, ref)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if _, err := f.pool.Exec(f.ctx, `DELETE FROM messages WHERE id = $1`, messageID); err != nil {
			t.Fatalf("delete message: %v", err)
		}
		d, _, err := g.ConsumeAttempt(f.ctx, attempt)
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		assertTerminal(t, d, sendingpolicy.ReasonSourceUnavailable)
	})

	t.Run("budget hold is retryable, not terminal", func(t *testing.T) {
		f := newFixture(t)
		g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
			p.DefaultAccountDailyRecipients = 1
		}))
		agent := f.agent(f.user("standard"))
		if d := f.send(g, f.message(agent, "own_address", 1)); !d.Allow {
			t.Fatalf("first send: %q", d.Reason)
		}
		d := f.send(g, f.message(agent, "own_address", 1))
		if d.Allow || d.Terminal {
			t.Fatalf("a daily budget hold must be retryable (allow=%v terminal=%v)", d.Allow, d.Terminal)
		}
		if d.RetryAt.IsZero() {
			t.Error("a retryable hold must advise when to come back")
		}
	})
}

func assertTerminal(t *testing.T, d sendingpolicy.Decision, wantReason string) {
	t.Helper()
	if d.Allow {
		t.Fatalf("expected a hold, got allow")
	}
	if d.Reason != wantReason {
		t.Fatalf("reason = %q, want %q", d.Reason, wantReason)
	}
	if !d.Terminal {
		t.Errorf("reason %q must be terminal — a retry can never clear it", d.Reason)
	}
	if !d.RetryAt.IsZero() {
		t.Errorf("a terminal hold must not advise a retry time (got %s)", d.RetryAt)
	}
}

// TestSettlementRejectsAnAttemptThatWasNeverAuthorized proves settlement
// reports what the PROVIDER did, so it is meaningless for an attempt that was
// never allowed to reach one. Accepting a reserved or released attempt would
// advance ramp progress for a send that never happened.
func TestSettlementRejectsAnAttemptThatWasNeverAuthorized(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	_, attempt := f.prepareAndReserve(g, agent, 1)

	// Reserved but never consumed.
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: attempt, Outcome: sendingpolicy.SettlementProviderAccepted,
	}); !errors.Is(err, sendingpolicy.ErrAttemptStale) {
		t.Fatalf("settling an unauthorized attempt = %v, want ErrAttemptStale", err)
	}

	// Authorized: now it settles.
	if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: attempt, Outcome: sendingpolicy.SettlementProviderAccepted,
	}); err != nil {
		t.Fatalf("settle authorized attempt: %v", err)
	}
}

// TestTenantHeaderSurface exercises the tenant-header modes, which otherwise
// ship entirely unexecuted — every other fixture leaves the mode disabled.
//
// The two things that must not fail open: an operational or public-feedback
// operation gets the fixed system tenant rather than silently none, and a
// customer whose tenant has no name is held rather than handed a token whose
// required header value is empty.
func TestTenantHeaderSurface(t *testing.T) {
	t.Run("operational mail uses the system tenant", func(t *testing.T) {
		f := newFixture(t)
		g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
			p.TenantHeaderMode = sendingpolicy.TenantHeaderEnforce
		}))
		eventID := f.pauseNotice(f.user("standard"))
		var ref sendingpolicy.OperationRef
		f.inTx(func(tx pgx.Tx) error {
			var err error
			ref, err = g.PrepareProtectionNoticeTx(f.ctx, tx,
				sendingpolicy.NewProtectionNoticeRef(eventID, sendingpolicy.AudienceOperator))
			return err
		})
		_, attempt, err := g.Reserve(f.ctx, ref)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
		if err != nil || auth == nil {
			t.Fatalf("authorize: auth=%v err=%v", auth, err)
		}
		headers, err := auth.ValidateEnvelope(auth.AuthorizedRecipients())
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if !headers.TenantRequired || headers.TenantName != sendingpolicy.SystemPolicySubject {
			t.Errorf("operational tenant = %v/%q, want required/%q",
				headers.TenantRequired, headers.TenantName, sendingpolicy.SystemPolicySubject)
		}
	})

	t.Run("customer with an unnamed tenant is held", func(t *testing.T) {
		f := newFixture(t)
		g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
			p.TenantHeaderMode = sendingpolicy.TenantHeaderEnforce
		}))
		user := f.user("standard")
		agent := f.agent(user)
		// The control row exists with ses_tenant_name '' and ready=false.
		d := f.send(g, f.message(agent, "relay", 1))
		if d.Allow {
			t.Fatal("an enforcing tenant policy must not authorize an account with no tenant")
		}
		if d.Reason != sendingpolicy.ReasonTenantNotReady && d.Reason != sendingpolicy.ReasonTenantUnnamed {
			t.Errorf("reason = %q, want a tenant hold", d.Reason)
		}
	})

	t.Run("canary selects only the named account", func(t *testing.T) {
		f := newFixture(t)
		user := f.user("standard")
		g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
			p.TenantHeaderMode = sendingpolicy.TenantHeaderCanary
			p.TenantHeaderCanaryAccountIDs = []string{user}
		}))
		// The canary account is selected and therefore held (no tenant yet).
		if d := f.send(g, f.message(f.agent(user), "relay", 1)); d.Allow {
			t.Fatal("the canary account must be tenant-gated")
		}
		// An account outside the list is untouched by the canary.
		if d := f.send(g, f.message(f.agent(f.user("standard")), "relay", 1)); !d.Allow {
			t.Fatalf("a non-canary account must be unaffected: %q", d.Reason)
		}
	})
}
