package sendingpolicy_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// The account-trash tests pin the "source-deletion check" (docs/design/
// account-soft-deletion.md §4.7): a trashed account (users.deleted_at set) may
// not send, and the refusal comes from the gate's own users predicate — the
// trash writes nothing to account_sending_controls, so a pause and its class
// survive the trash and a restore exactly.

func (f *fixture) trash(userID string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, userID); err != nil {
		f.t.Fatalf("trash account: %v", err)
	}
}

type controlSnapshot struct {
	state, class, reason string
	updatedAt            time.Time
	present              bool
}

func (f *fixture) control(userID string) controlSnapshot {
	f.t.Helper()
	var s controlSnapshot
	err := f.pool.QueryRow(f.ctx, `
		SELECT state, pause_class, reason, updated_at FROM account_sending_controls WHERE user_id = $1`, userID,
	).Scan(&s.state, &s.class, &s.reason, &s.updatedAt)
	if err == nil {
		s.present = true
	}
	return s
}

func TestTrashedAccountQueuedSendIsRefusedAtConsume(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	user := f.user("standard")
	_, attempt := f.prepareAndReserve(g, f.agent(user), 1)
	before := f.control(user)

	f.trash(user)

	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if d.Allow || auth != nil {
		t.Fatal("a trashed account must not be authorized to reach the provider")
	}
	assertTerminal(t, d, sendingpolicy.ReasonAccountDeleted)
	if after := f.control(user); after != before {
		t.Fatalf("consume on a trashed account touched the control row: before=%+v after=%+v", before, after)
	}
}

func TestTrashedAccountIsRefusedAtRedemption(t *testing.T) {
	f := newFixture(t)
	g := f.gate(enforcingPolicy(nil))
	user := f.user("standard")
	_, attempt := f.prepareAndReserve(g, f.agent(user), 1)
	_, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: auth=%v err=%v", auth, err)
	}

	// The trash commits after authorization but before the socket opens.
	f.trash(user)

	if err := g.RedeemProviderCall(f.ctx, *auth); !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
		t.Fatalf("redeem after trash err = %v, want ErrAuthorizationInvalid (no provider call)", err)
	}
	if _, callState := f.reservationState(attempt.OperationID(), attempt.Attempt()); callState == "started" {
		t.Fatal("the reservation reached call_state=started for a trashed account")
	}
}

func TestTrashedAccountIsRefusedAtAcceptanceWithoutControlWrite(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	user := f.user("standard")
	agent := f.agent(user)
	f.pause(user)
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE account_sending_controls SET pause_class = 'abuse' WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	before := f.control(user)
	msg := f.message(agent, "relay", 1)

	f.trash(user)

	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, gotErr := g.PrepareExternalTx(f.ctx, tx, msg)
	_ = tx.Rollback(f.ctx)
	if !errors.Is(gotErr, sendingpolicy.ErrSourceUnavailable) {
		t.Fatalf("prepare for a trashed account err = %v, want ErrSourceUnavailable", gotErr)
	}
	if after := f.control(user); after != before || after.class != "abuse" || after.state != "paused" {
		t.Fatalf("the pause did not survive the trash exactly: before=%+v after=%+v", before, after)
	}
}

// TestOperatorPauseCarriesClassAndWorksOnTrashedAccounts pins the pause
// command's contract: a class and an optional evidence reference are recorded
// with an audit event; it works on a trashed account (the lever that makes a
// later purge write abuse tombstones); a resume resets the class and starts a
// new detector epoch.
func TestOperatorPauseCarriesClassAndWorksOnTrashedAccounts(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	user := f.user("standard")
	f.trash(user)

	if _, err := m.SetAccountPause(f.ctx, sendingpolicy.AccountPauseChange{
		AccountID: user, Paused: true, Class: "fraud", Actor: "op", Reason: "r",
	}); err == nil {
		t.Fatal("an unknown pause class was accepted")
	}
	rec, err := m.SetAccountPause(f.ctx, sendingpolicy.AccountPauseChange{
		AccountID: user, Paused: true, Class: sendingpolicy.PauseClassAbuse,
		EvidenceRef: "INC-SYNTHETIC-9", Actor: "op", Reason: "synthetic abuse",
	})
	if err != nil {
		t.Fatalf("pause a trashed account: %v", err)
	}
	if rec.State != "paused" || rec.PauseClass != "abuse" || rec.EvidenceRef != "INC-SYNTHETIC-9" || rec.AccountStatus != "trashed" {
		t.Fatalf("readback = %+v", rec)
	}
	var evClass, evRef, newState string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT pause_class, evidence_ref, new_state FROM account_sending_control_events
		 WHERE account_ref = $1 ORDER BY created_at DESC LIMIT 1`, user).Scan(&evClass, &evRef, &newState); err != nil {
		t.Fatalf("audit event: %v", err)
	}
	if evClass != "abuse" || evRef != "INC-SYNTHETIC-9" || newState != "paused" {
		t.Fatalf("audit event = %s/%s/%s", evClass, evRef, newState)
	}

	var epochBefore int64
	if err := f.pool.QueryRow(f.ctx, `SELECT outcome_epoch FROM account_sending_controls WHERE user_id = $1`, user).Scan(&epochBefore); err != nil {
		t.Fatal(err)
	}
	rec, err = m.SetAccountPause(f.ctx, sendingpolicy.AccountPauseChange{AccountID: user, Actor: "op", Reason: "cleared"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	var epochAfter int64
	if err := f.pool.QueryRow(f.ctx, `SELECT outcome_epoch FROM account_sending_controls WHERE user_id = $1`, user).Scan(&epochAfter); err != nil {
		t.Fatal(err)
	}
	// A resume keeps the class and the evidence reference (history), appends
	// its own event, and starts a new detector epoch.
	if rec.State != "active" || rec.PauseClass != "abuse" || rec.EvidenceRef != "INC-SYNTHETIC-9" || epochAfter != epochBefore+1 {
		t.Fatalf("resume readback = %+v epoch %d→%d", rec, epochBefore, epochAfter)
	}
	var events int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM account_sending_control_events WHERE account_ref = $1`, user).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("control events = %d, want pause + resume", events)
	}
	// A later pause under a lesser class never downgrades abuse.
	rec, err = m.SetAccountPause(f.ctx, sendingpolicy.AccountPauseChange{
		AccountID: user, Paused: true, Class: sendingpolicy.PauseClassBilling, Actor: "op", Reason: "billing hold",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.PauseClass != "abuse" {
		t.Fatalf("re-pause downgraded the class to %s", rec.PauseClass)
	}
}
