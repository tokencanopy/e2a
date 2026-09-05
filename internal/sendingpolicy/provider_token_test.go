package sendingpolicy_test

import (
	"errors"
	"testing"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// These tests are about what a settlement may bind to the attempt's feedback
// correlation. The provider id is how most delivery feedback finds its
// attempt, so it has to be written exactly once, by an acceptance, and never
// rewritten.

func (f *fixture) providerMessageID(operationID string, attempt int) *string {
	f.t.Helper()
	var id *string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT provider_message_id FROM sending_feedback_correlations
		 WHERE operation_id = $1 AND submission_attempt = $2`, operationID, attempt,
	).Scan(&id); err != nil {
		f.t.Fatalf("read provider_message_id: %v", err)
	}
	return id
}

// redeemed mints a token and redeems it, leaving the attempt in the state a
// settlement expects.
func (f *fixture) redeemed(g sendingpolicy.Gate, agent string) (sendingpolicy.OperationRef, *sendingpolicy.ProviderAuthorization) {
	f.t.Helper()
	ref, attempt := f.prepareAndReserve(g, agent, 1)
	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		f.t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	if err := g.RedeemProviderCall(f.ctx, *auth); err != nil {
		f.t.Fatalf("redeem: %v", err)
	}
	return ref, auth
}

func TestProviderTokenSettlementBindsProviderMessageIDOnce(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	ref, auth := f.redeemed(g, f.agent(f.user("standard")))

	accepted := sendingpolicy.ProviderSettlement{
		Attempt: auth.Attempt(), Outcome: sendingpolicy.SettlementProviderAccepted, ProviderMessageID: "ses-id-one",
	}
	if err := g.SettleProvider(f.ctx, accepted); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := f.providerMessageID(ref.ID(), 1); got == nil || *got != "ses-id-one" {
		t.Fatalf("bound = %v, want ses-id-one", got)
	}

	// The delayed feedback finalizer settles the same attempt again.
	if err := g.SettleProvider(f.ctx, accepted); err != nil {
		t.Fatalf("replay: %v", err)
	}

	// A different id for the same attempt is two physical sends for one
	// charge. Refuse it and keep the first.
	conflicting := accepted
	conflicting.ProviderMessageID = "ses-id-two"
	if err := g.SettleProvider(f.ctx, conflicting); !errors.Is(err, sendingpolicy.ErrProviderMessageIDConflict) {
		t.Fatalf("conflict err = %v, want ErrProviderMessageIDConflict", err)
	}
	if got := f.providerMessageID(ref.ID(), 1); got == nil || *got != "ses-id-one" {
		t.Fatalf("bound after conflict = %v, want ses-id-one kept", got)
	}
}

func TestProviderTokenRejectionNeverBindsProviderMessageID(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	ref, auth := f.redeemed(g, f.agent(f.user("standard")))

	// A rejection carrying an id is a caller bug: nothing was accepted, so
	// there is nothing to attribute feedback to.
	err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: auth.Attempt(), Outcome: sendingpolicy.SettlementProviderPermanentlyRejected, ProviderMessageID: "ses-id",
	})
	if err == nil {
		t.Fatal("a rejection with a provider id was accepted")
	}
	if got := f.providerMessageID(ref.ID(), 1); got != nil {
		t.Fatalf("bound = %q on a refused settlement", *got)
	}

	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: auth.Attempt(), Outcome: sendingpolicy.SettlementProviderPermanentlyRejected,
	}); err != nil {
		t.Fatalf("plain rejection: %v", err)
	}
	if got := f.providerMessageID(ref.ID(), 1); got != nil {
		t.Fatalf("bound = %q after a rejection", *got)
	}
}

// TestProviderTokenAcceptanceWithoutIDStillSettles keeps the pre-existing
// contract: an acceptance whose id was lost (crash between DATA and the
// result) settles normally and leaves the correlation open for the feedback
// path to bind by attempt header.
func TestProviderTokenAcceptanceWithoutIDStillSettles(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	ref, auth := f.redeemed(g, f.agent(f.user("standard")))

	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: auth.Attempt(), Outcome: sendingpolicy.SettlementProviderAccepted,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := f.providerMessageID(ref.ID(), 1); got != nil {
		t.Fatalf("bound = %q, want nothing bound", *got)
	}
	// And a later settlement that does know the id may still bind it.
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: auth.Attempt(), Outcome: sendingpolicy.SettlementProviderAccepted, ProviderMessageID: "ses-late",
	}); err != nil {
		t.Fatalf("late bind: %v", err)
	}
	if got := f.providerMessageID(ref.ID(), 1); got == nil || *got != "ses-late" {
		t.Fatalf("bound = %v, want ses-late", got)
	}
}
