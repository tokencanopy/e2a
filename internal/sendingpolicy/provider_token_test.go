package sendingpolicy_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
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

// TestProviderTokenSettlementNormalizesProviderMessageID: the relay reports
// SES's id qualified (<id@region.amazonses.com>) and SES's feedback reports it
// bare. Both spellings must be one binding, or the two writers that settle the
// same attempt will refuse each other.
func TestProviderTokenSettlementNormalizesProviderMessageID(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	ref, auth := f.redeemed(g, f.agent(f.user("standard")))

	qualified := sendingpolicy.ProviderSettlement{
		Attempt: auth.Attempt(), Outcome: sendingpolicy.SettlementProviderAccepted,
		ProviderMessageID: "<010f0193abcdef00-000000@us-east-2.amazonses.com>",
	}
	if err := g.SettleProvider(f.ctx, qualified); err != nil {
		t.Fatalf("settle qualified: %v", err)
	}
	if got := f.providerMessageID(ref.ID(), 1); got == nil || *got != "010f0193abcdef00-000000" {
		t.Fatalf("bound = %v, want the bare id", got)
	}
	bare := qualified
	bare.ProviderMessageID = "010f0193abcdef00-000000"
	if err := g.SettleProvider(f.ctx, bare); err != nil {
		t.Fatalf("settle bare after qualified: %v (want idempotent)", err)
	}
	bracketed := qualified
	bracketed.ProviderMessageID = "<010f0193abcdef00-000000>"
	if err := g.SettleProvider(f.ctx, bracketed); err != nil {
		t.Fatalf("settle bracketed after qualified: %v (want idempotent)", err)
	}
	other := qualified
	other.ProviderMessageID = "<010f0193abcdef00-000001@us-east-2.amazonses.com>"
	if err := g.SettleProvider(f.ctx, other); !errors.Is(err, sendingpolicy.ErrProviderMessageIDConflict) {
		t.Fatalf("different id err = %v, want ErrProviderMessageIDConflict", err)
	}
}

// TestProviderTokenSettlementRequiresTheSocketToHaveOpened: an attempt that
// was authorized but never redeemed cannot be settled as accepted — nothing
// reached the provider, so there is no provider outcome to record.
func TestProviderTokenSettlementRequiresTheSocketToHaveOpened(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	ref, attempt := f.prepareAndReserve(g, f.agent(f.user("standard")), 1)
	if _, auth, err := g.ConsumeAttempt(f.ctx, attempt); err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}

	err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: attempt, Outcome: sendingpolicy.SettlementProviderAccepted, ProviderMessageID: "ses-id-never-sent",
	})
	if !errors.Is(err, sendingpolicy.ErrAttemptStale) {
		t.Fatalf("settle without redeem err = %v, want ErrAttemptStale", err)
	}
	if got := f.providerMessageID(ref.ID(), 1); got != nil {
		t.Fatalf("bound = %q for an attempt that never dialed", *got)
	}
	if _, callState := f.reservationState(ref.ID(), 1); callState != "authorized" {
		t.Fatalf("call_state = %s, want authorized (untouched)", callState)
	}
}

// TestProviderTokenHoldsOnATenantNameThatCannotBeAHeader: a tenant name with a
// line break would be refused by the adapter, silently and forever. The gate
// holds the send with a visible reason instead.
func TestProviderTokenHoldsOnATenantNameThatCannotBeAHeader(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(func(p *sendingpolicy.RuntimePolicy) {
		p.TenantHeaderMode = sendingpolicy.TenantHeaderEnforce
	}))
	user := f.user("standard")
	agent := f.agent(user)
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO account_sending_controls (user_id, ses_tenant_name, ses_tenant_ready, ses_tenant_ready_at)
		VALUES ($1, $2, true, now())
		ON CONFLICT (user_id) DO UPDATE
		    SET ses_tenant_name = EXCLUDED.ses_tenant_name, ses_tenant_ready = true, ses_tenant_ready_at = now()`,
		user, "good\r\nX-SES-CONFIGURATION-SET: attacker-set",
	); err != nil {
		t.Fatal(err)
	}
	_, ref := f.prepareMessage(g, f.message(agent, "own_address", 1))
	d := f.authorize(g, ref)
	if d.Allow || d.Reason != sendingpolicy.ReasonTenantUnnamed {
		t.Fatalf("decision = %+v, want a hold with reason %q", d, sendingpolicy.ReasonTenantUnnamed)
	}
}

// TestProviderTokenPauseNoticeToAPausedOwnerStillRedeems pins the customer-only
// guard on RedeemProviderCall's pause re-check: the notice telling an account
// it was paused is SOURCED from that paused account, so an unguarded re-read
// would refuse the one email the pause exists to send.
func TestProviderTokenPauseNoticeToAPausedOwnerStillRedeems(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	user := f.user("standard")
	f.pause(user)
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
	decision, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("consume: decision=%+v auth=%v err=%v", decision, auth, err)
	}
	if err := g.RedeemProviderCall(f.ctx, *auth); err != nil {
		t.Fatalf("redeem of a pause notice to a PAUSED owner: %v — the notice must still go out", err)
	}
}

// TestProviderTokenSettlementComparesNormalizedProviderMessageID: a row bound
// by another writer in the qualified spelling must not refuse a bare replay.
func TestProviderTokenSettlementComparesNormalizedProviderMessageID(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	ref, auth := f.redeemed(g, f.agent(f.user("standard")))
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE sending_feedback_correlations SET provider_message_id = $3
		 WHERE operation_id = $1 AND submission_attempt = $2`,
		ref.ID(), 1, "<010f0193abcdef00-000000@us-east-2.amazonses.com>",
	); err != nil {
		t.Fatal(err)
	}
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: auth.Attempt(), Outcome: sendingpolicy.SettlementProviderAccepted, ProviderMessageID: "010f0193abcdef00-000000",
	}); err != nil {
		t.Fatalf("bare replay over a qualified binding: %v", err)
	}
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: auth.Attempt(), Outcome: sendingpolicy.SettlementProviderAccepted, ProviderMessageID: "other-000000",
	}); !errors.Is(err, sendingpolicy.ErrProviderMessageIDConflict) {
		t.Fatalf("different id err = %v, want ErrProviderMessageIDConflict", err)
	}
}

// TestProviderTokenSettleOperationTargetsTheLatestDialedAttempt: evidence that
// arrives without a token settles the most recent attempt that opened a
// socket — not a later ordinal that was only reserved, and nothing at all when
// no attempt ever dialed.
func TestProviderTokenSettleOperationTargetsTheLatestDialedAttempt(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	ref, attempt := f.prepareAndReserve(g, agent, 1)

	err := g.SettleOperation(f.ctx, ref, sendingpolicy.SettlementProviderAccepted, "ses-early")
	if !errors.Is(err, sendingpolicy.ErrAttemptStale) {
		t.Fatalf("settle before any dial err = %v, want ErrAttemptStale", err)
	}

	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}
	if err := g.RedeemProviderCall(f.ctx, *auth); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	// The worker died after the socket opened; a later execution re-reserved
	// ordinal two but never consumed it. Delayed evidence belongs to ordinal one.
	if _, next, err := g.Reserve(f.ctx, ref); err != nil || next.Attempt() != 2 {
		t.Fatalf("re-reserve: attempt=%v err=%v", next, err)
	}
	if err := g.SettleOperation(f.ctx, ref, sendingpolicy.SettlementProviderAccepted, "<ses-late@us-east-2.amazonses.com>"); err != nil {
		t.Fatalf("settle by operation: %v", err)
	}
	if got := f.providerMessageID(ref.ID(), 1); got == nil || *got != "ses-late" {
		t.Fatalf("attempt one bound = %v, want ses-late", got)
	}
	var bound int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM sending_feedback_correlations
		 WHERE operation_id = $1 AND provider_message_id IS NOT NULL`, ref.ID()).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 1 {
		t.Fatalf("%d attempts carry a provider id, want exactly the dialed one", bound)
	}
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{
		Attempt: auth.Attempt(), Outcome: sendingpolicy.SettlementProviderAccepted, ProviderMessageID: "ses-late",
	}); err != nil {
		t.Fatalf("replay by token: %v", err)
	}
}

// TestProviderTokenLookupOperationResolvesOnlyDurableOperations: a reference
// can be recovered for an operation that exists, and for nothing else.
func TestProviderTokenLookupOperationResolvesOnlyDurableOperations(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	agent := f.agent(f.user("standard"))
	_, ref := f.prepareMessage(g, f.message(agent, "own_address", 1))

	got, err := g.LookupOperation(f.ctx, ref.ID())
	if err != nil || got.ID() != ref.ID() || got.Purpose() != sendingpolicy.PurposeCustomerMessage {
		t.Fatalf("lookup = %+v err=%v, want the prepared operation", got, err)
	}
	if _, err := g.LookupOperation(f.ctx, "msg_never_prepared"); !errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
		t.Fatalf("lookup of an unknown operation err = %v, want ErrSourceUnavailable", err)
	}
	if _, err := g.LookupOperation(f.ctx, ""); !errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
		t.Fatalf("lookup of an empty id err = %v, want ErrSourceUnavailable", err)
	}
}
