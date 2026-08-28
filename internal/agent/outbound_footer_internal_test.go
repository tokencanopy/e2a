package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
)

// fakeFooterEnforcer is a minimal limits.Enforcer whose Get returns a fixed
// resolved Limits (the enforcer is what already folds row-vs-default; that
// resolution is pinned in internal/limits tests).
type fakeFooterEnforcer struct {
	lim   limits.Limits
	err   error
	calls int
}

func (f *fakeFooterEnforcer) Get(ctx context.Context, userID string) (limits.Limits, error) {
	f.calls++
	return f.lim, f.err
}
func (f *fakeFooterEnforcer) CheckAgentCreate(context.Context, string) error      { return nil }
func (f *fakeFooterEnforcer) CheckDomainCreate(context.Context, string) error     { return nil }
func (f *fakeFooterEnforcer) CheckMessageSend(context.Context, string, int) error { return nil }
func (f *fakeFooterEnforcer) CheckInboundMessage(context.Context, string) error   { return nil }
func (f *fakeFooterEnforcer) Invalidate(string)                                   {}

// TestResolveOutboundFooterMatrix pins the gating decision:
//
//	append := cfg.Enabled && resolvedEntitlement && class == standard
//
// with fail-closed behavior on every uncertain input (limits error, nil
// user, empty class, missing enforcer).
func TestResolveOutboundFooterMatrix(t *testing.T) {
	stdUser := &identity.User{ID: "u1", AccountClass: "standard"}
	entitled := &fakeFooterEnforcer{lim: limits.Limits{OutboundFooterEnabled: true}}
	unentitled := &fakeFooterEnforcer{lim: limits.Limits{OutboundFooterEnabled: false}}

	cases := []struct {
		name     string
		enabled  bool
		enforcer limits.Enforcer
		user     *identity.User
		want     bool
	}{
		{"config_off_even_when_entitled", false, entitled, stdUser, false},
		{"entitled_standard", true, entitled, stdUser, true},
		{"unentitled_row", true, unentitled, stdUser, false},
		{"limits_error_fails_closed", true, &fakeFooterEnforcer{err: errors.New("db down")}, stdUser, false},
		{"nil_enforcer", true, nil, stdUser, false},
		{"nil_user", true, entitled, nil, false},
		{"class_internal", true, entitled, &identity.User{ID: "u2", AccountClass: "internal"}, false},
		{"class_system", true, entitled, &identity.User{ID: "u3", AccountClass: "system"}, false},
		{"class_demo", true, entitled, &identity.User{ID: "u4", AccountClass: "demo"}, false},
		{"class_empty_fails_closed", true, entitled, &identity.User{ID: "u5"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &API{}
			a.SetOutboundFooterEnabled(tc.enabled)
			if tc.enforcer != nil {
				a.SetEnforcer(tc.enforcer)
			}
			if got := a.resolveOutboundFooter(context.Background(), tc.user); got != tc.want {
				t.Errorf("resolveOutboundFooter = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveOutboundFooterMasterSwitchSkipsLimitsRead: with the feature off,
// the decision must not cost a limits read (the common self-host case).
func TestResolveOutboundFooterMasterSwitchSkipsLimitsRead(t *testing.T) {
	enf := &fakeFooterEnforcer{lim: limits.Limits{OutboundFooterEnabled: true}}
	a := &API{}
	a.SetEnforcer(enf)
	a.SetOutboundFooterEnabled(false)
	if a.resolveOutboundFooter(context.Background(), &identity.User{ID: "u1", AccountClass: "standard"}) {
		t.Fatal("master switch off must resolve false")
	}
	if enf.calls != 0 {
		t.Errorf("limits reads with feature off = %d, want 0", enf.calls)
	}
}
