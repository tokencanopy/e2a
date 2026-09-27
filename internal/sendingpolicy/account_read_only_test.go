package sendingpolicy_test

import (
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// TestAccountPauseReadbackReportsReadOnly pins the operator readback's
// read_only flag against identity.Store.AccountReadOnly (what the request
// guards consult) across every pause class and a resume, applied through the
// operator command's own path (docs/design/account-read-only.md).
func TestAccountPauseReadbackReportsReadOnly(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	store := identity.NewStore(f.pool)

	check := func(label string, rec sendingpolicy.AccountPauseRecord, user string, want bool) {
		t.Helper()
		guard, err := store.AccountReadOnly(f.ctx, user)
		if err != nil {
			t.Fatalf("%s: AccountReadOnly: %v", label, err)
		}
		if rec.ReadOnly() != want || guard != want {
			t.Fatalf("%s: readback ReadOnly()=%v guard=%v, want %v", label, rec.ReadOnly(), guard, want)
		}
	}
	for _, class := range []string{sendingpolicy.PauseClassOperator, sendingpolicy.PauseClassBilling, sendingpolicy.PauseClassSystem, sendingpolicy.PauseClassAbuse} {
		user := f.user("standard")
		rec, err := m.SetAccountPause(f.ctx, sendingpolicy.AccountPauseChange{
			AccountID: user, Paused: true, Class: class, Actor: "op", Reason: "synthetic",
		})
		if err != nil {
			t.Fatalf("pause %s: %v", class, err)
		}
		check("paused/"+class, rec, user, class == sendingpolicy.PauseClassAbuse)
		rec, err = m.SetAccountPause(f.ctx, sendingpolicy.AccountPauseChange{AccountID: user, Actor: "op", Reason: "cleared"})
		if err != nil {
			t.Fatalf("resume %s: %v", class, err)
		}
		check("resumed/"+class, rec, user, false)
	}
}
