package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/tokencanopy/e2a/internal/limits"
)

// quotaFakeEnforcer drives checkApproveQuota: CheckMessageSend returns the
// configured error; every other method is inert.
type quotaFakeEnforcer struct {
	fakeFooterEnforcer
	sendErr error
	units   int
}

func (f *quotaFakeEnforcer) CheckMessageSend(_ context.Context, _ string, units int) error {
	f.units = units
	return f.sendErr
}

// TestCheckApproveQuota pins the approve-time flow-cap re-check: a
// LimitExceededError becomes a 402 limit_exceeded OutboundError whose
// Details mirror the LimitExceededDetails wire shape; a transient enforcer
// error fails open (the external path re-checks at worker claim time); a
// nil enforcer disables the check entirely (self-host default).
func TestCheckApproveQuota(t *testing.T) {
	ctx := context.Background()

	t.Run("nil_enforcer_allows", func(t *testing.T) {
		a := &API{}
		if oerr := a.checkApproveQuota(ctx, "u1", 1); oerr != nil {
			t.Fatalf("nil enforcer: got %+v, want nil", oerr)
		}
	})

	t.Run("under_cap_allows_and_forwards_real_units", func(t *testing.T) {
		enf := &quotaFakeEnforcer{}
		a := &API{enforcer: enf}
		// The approve probe must charge the merged draft's real recipient
		// count, not a fixed 1 — a 50-recipient draft near the cap otherwise
		// gets an untruthful "accepted".
		if oerr := a.checkApproveQuota(ctx, "u1", 7); oerr != nil {
			t.Fatalf("under cap: got %+v, want nil", oerr)
		}
		if enf.units != 7 {
			t.Errorf("probe units = %d, want 7 (real merged recipient count)", enf.units)
		}
	})

	t.Run("over_cap_402", func(t *testing.T) {
		enf := &quotaFakeEnforcer{sendErr: &limits.LimitExceededError{
			Resource: "messages_month", Limit: 3000, Current: 3000,
			Limits: limits.Limits{PlanCode: "free", UpgradeURL: "https://e2a.dev/api/billing/portal"},
		}}
		a := &API{enforcer: enf}
		oerr := a.checkApproveQuota(ctx, "u1", 1)
		if oerr == nil {
			t.Fatal("over cap: got nil, want 402")
		}
		if oerr.Status != http.StatusPaymentRequired || oerr.Code != "limit_exceeded" {
			t.Errorf("status/code = %d/%q, want 402/limit_exceeded", oerr.Status, oerr.Code)
		}
		if oerr.Details["resource"] != "messages_month" {
			t.Errorf("details.resource = %v, want messages_month", oerr.Details["resource"])
		}
		if oerr.Details["limit"] != int64(3000) || oerr.Details["current"] != int64(3000) {
			t.Errorf("details limit/current = %v/%v, want 3000/3000", oerr.Details["limit"], oerr.Details["current"])
		}
		if oerr.Details["plan_code"] != "free" {
			t.Errorf("details.plan_code = %v, want free", oerr.Details["plan_code"])
		}
	})

	t.Run("transient_error_fails_open", func(t *testing.T) {
		enf := &quotaFakeEnforcer{sendErr: errors.New("db down")}
		a := &API{enforcer: enf}
		if oerr := a.checkApproveQuota(ctx, "u1", 1); oerr != nil {
			t.Fatalf("transient error: got %+v, want nil (fail open)", oerr)
		}
	})
}
