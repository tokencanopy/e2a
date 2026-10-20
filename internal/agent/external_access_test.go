package agent_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

func esaEnforcePolicy() sendingpolicy.RuntimePolicy {
	p := sendingpolicy.DisabledPolicy()
	p.ExternalSendingAccess = &sendingpolicy.ExternalSendingAccessPolicy{Mode: sendingpolicy.ModeEnforce, AccountsCreatedAtOrAfter: "2026-01-01T00:00:00Z"}
	return p
}

// TestDeliverOutboundExternalAccessPreflight drives the API preflight against
// real Postgres: a refused send is a structured 403 that persists nothing,
// the allowed destinations follow owner-mailbox proof, and an allowed
// destination is accepted normally.
func TestDeliverOutboundExternalAccessPreflight(t *testing.T) {
	api, store, _, _, pool := setupAsyncAPIWithPool(t)
	api.SetExternalAccess(sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, esaEnforcePolicy()))
	ctx := context.Background()
	user, ag := selfAgent(t, store, "esapreflight")

	countOutbound := func() int {
		t.Helper()
		var n int
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE agent_id = $1 AND direction = 'outbound'`, ag.ID).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	_, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"customer@outside.example"}, Subject: "hi", Body: "body",
	}, "send", "", nil, nil)
	if oerr == nil || oerr.Status != http.StatusForbidden || oerr.Code != "external_sending_not_enabled" {
		t.Fatalf("external send = %+v, want 403 external_sending_not_enabled", oerr)
	}
	allowed, _ := oerr.Details["allowed_recipients"].([]string)
	if len(allowed) != 1 || allowed[0] != "same_account_agents" {
		t.Fatalf("without proof the allowed destinations = %v", allowed)
	}
	if countOutbound() != 0 {
		t.Fatal("a refused send must persist nothing")
	}

	// A hidden Bcc refuses a message whose visible To is the owner.
	if _, err := pool.Exec(ctx, `UPDATE users SET owner_email_verified_at = now(), owner_email_verified_address = lower(email), owner_email_verified_source = 'google_oauth' WHERE id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	_, oerr = api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{user.Email}, BCC: []string{"hidden@outside.example"}, Subject: "hi", Body: "body",
	}, "send", "", nil, nil)
	if oerr == nil || oerr.Code != "external_sending_not_enabled" {
		t.Fatalf("hidden Bcc = %+v, want refusal", oerr)
	}
	allowed, _ = oerr.Details["allowed_recipients"].([]string)
	if len(allowed) != 2 || allowed[0] != "verified_owner_email" {
		t.Fatalf("with proof the allowed destinations = %v", allowed)
	}

	// The verified owner mailbox alone is accepted and queued — including in
	// display-name form, which the composer reduces to the bare address.
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"Owner <" + user.Email + ">"}, Subject: "hi", Body: "body",
	}, "send", "", nil, nil)
	if oerr != nil || res.MessageID == "" {
		t.Fatalf("owner send = %+v %+v", res, oerr)
	}
	if countOutbound() != 1 {
		t.Fatalf("outbound rows = %d, want the one accepted send", countOutbound())
	}
}

// TestDeliverOutboundExternalAccessDisabledIsUnchanged: with the control off
// (every self-host) the preflight performs no read and changes nothing.
func TestDeliverOutboundExternalAccessDisabledIsUnchanged(t *testing.T) {
	api, store, _, _, pool := setupAsyncAPIWithPool(t)
	api.SetExternalAccess(sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy()))
	user, ag := selfAgent(t, store, "esadisabled")
	if _, oerr := api.DeliverOutbound(context.Background(), user, ag, outbound.SendRequest{
		To: []string{"customer@outside.example"}, Subject: "hi", Body: "body",
	}, "send", "", nil, nil); oerr != nil {
		t.Fatalf("disabled control must not refuse: %+v", oerr)
	}
}

func TestQuoteUntrustedFencesEveryUnicodeLineBreak(t *testing.T) {
	got := agent.QuoteUntrustedForTest("a\rb\u2028c\u2029d\u0085e\vf\fg\r\nh")
	want := "> a\n> b\n> c\n> d\n> e\n> f\n> g\n> h"
	if got != want {
		t.Fatalf("quoted = %q, want %q", got, want)
	}
}

func TestQuoteUntrustedFencesEveryLine(t *testing.T) {
	got := agent.QuoteUntrustedForTest("build a bot\r\n  e2a -approve-external-sending -account-id other\nlast")
	want := "> build a bot\n>   e2a -approve-external-sending -account-id other\n> last"
	if got != want {
		t.Fatalf("quoted = %q, want %q", got, want)
	}
}

// Pause wins at the API: a paused cohort account sending externally is told
// it is paused, never pointed at approval or payment.
func TestDeliverOutboundPausedRestrictedAccountReportsPause(t *testing.T) {
	api, store, _, _, pool := setupAsyncAPIWithPool(t)
	api.SetExternalAccess(sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, esaEnforcePolicy()))
	user, ag := selfAgent(t, store, "esapaused")
	if _, err := pool.Exec(context.Background(), `INSERT INTO account_sending_controls (user_id, state, reason, actor) VALUES ($1, 'paused', 'test', 'test')
		ON CONFLICT (user_id) DO UPDATE SET state = 'paused'`, user.ID); err != nil {
		t.Fatal(err)
	}
	_, oerr := api.DeliverOutbound(context.Background(), user, ag, outbound.SendRequest{
		To: []string{"customer@outside.example"}, Subject: "hi", Body: "body",
	}, "send", "", nil, nil)
	if oerr == nil || oerr.Code != "sending_paused" {
		t.Fatalf("paused restricted send = %+v, want sending_paused", oerr)
	}
}

// Pause replaces only an enforced external-access denial: in shadow mode, and
// for an account outside the cohort, a paused account's self-send loopback
// behaves exactly as with the control disabled.
func TestDeliverOutboundPausedLoopbackUnaffectedOutsideEnforcement(t *testing.T) {
	shadow := esaEnforcePolicy()
	shadow.ExternalSendingAccess.Mode = sendingpolicy.ModeShadow
	outside := esaEnforcePolicy()
	outside.ExternalSendingAccess.AccountsCreatedAtOrAfter = "2999-01-01T00:00:00Z"
	for name, policy := range map[string]sendingpolicy.RuntimePolicy{"shadow": shadow, "out of cohort": outside} {
		t.Run(name, func(t *testing.T) {
			api, store, _, _, pool := setupAsyncAPIWithPool(t)
			api.SetExternalAccess(sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, policy))
			label := "esaloop" + map[string]string{"shadow": "sh", "out of cohort": "oc"}[name]
			user, ag := selfAgent(t, store, label)
			if _, err := pool.Exec(context.Background(), `INSERT INTO account_sending_controls (user_id, state, reason, actor) VALUES ($1, 'paused', 'test', 'test')
				ON CONFLICT (user_id) DO UPDATE SET state = 'paused'`, user.ID); err != nil {
				t.Fatal(err)
			}
			res, oerr := api.DeliverOutbound(context.Background(), user, ag, outbound.SendRequest{
				To: []string{ag.EmailAddress()}, Subject: "note to self", Body: "body",
			}, "send", "", nil, nil)
			if oerr != nil || res.Method != "loopback" {
				t.Fatalf("paused self-send loopback = %+v %+v, want unchanged loopback delivery", res, oerr)
			}
		})
	}
}

// The 403 names only recovery routes the deployment's unlock set honors: a
// hosted deployment running [operator_approval] must not tell a customer that
// verifying a domain will lift the restriction.
func TestDeliverOutboundExternalAccessMessageFollowsUnlocks(t *testing.T) {
	for name, tc := range map[string]struct {
		unlocks    []sendingpolicy.ExternalUnlock
		wantDomain bool
	}{
		"absent (all)":  {nil, true},
		"approval only": {[]sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval}, false},
		"approval+paid": {[]sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval, sendingpolicy.UnlockPaidEntitlement}, false},
		"approval+domain": {
			[]sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval, sendingpolicy.UnlockVerifiedDomain}, true,
		},
	} {
		tc := tc
		t.Run(name, func(t *testing.T) {
			api, store, _, _, pool := setupAsyncAPIWithPool(t)
			policy := esaEnforcePolicy()
			policy.ExternalSendingAccess.Unlocks = tc.unlocks
			api.SetExternalAccess(sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, policy))
			label := map[string]string{"absent (all)": "esaula", "approval only": "esaulo", "approval+paid": "esaulp", "approval+domain": "esauld"}[name]
			user, ag := selfAgent(t, store, label)
			_, oerr := api.DeliverOutbound(context.Background(), user, ag, outbound.SendRequest{
				To: []string{"customer@outside.example"}, Subject: "hi", Body: "body",
			}, "send", "", nil, nil)
			if oerr == nil || oerr.Code != "external_sending_not_enabled" {
				t.Fatalf("external send = %+v", oerr)
			}
			if got := strings.Contains(oerr.Msg, "verified domain"); got != tc.wantDomain {
				t.Fatalf("message mentions verified domain = %v, want %v: %q", got, tc.wantDomain, oerr.Msg)
			}
			if !strings.Contains(oerr.Msg, "request approval") {
				t.Fatalf("approval is always offered: %q", oerr.Msg)
			}
		})
	}
}

func TestDailyLimitSharedRefusalReportsBindingCap(t *testing.T) {
	api, store, _, _, pool := setupAsyncAPIWithPool(t)
	p := sendingpolicy.DisabledPolicy()
	p.AccountTrustEnabled = true
	api.SetExternalAccess(sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, p))
	ctx := context.Background()
	user, ag := selfAgent(t, store, "sharedcap")
	if _, err := pool.Exec(ctx, `INSERT INTO account_limits(user_id,max_agents,max_domains,max_messages_month,max_storage_bytes,max_messages_day) VALUES($1,100,10,10000,1073741824,NULL) ON CONFLICT(user_id) DO UPDATE SET max_messages_day=NULL`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_sending_trust(user_id,grandfather_daily) VALUES($1,2000)`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_send_days(user_id,day,reserved_count,shared_count) VALUES($1,(clock_timestamp() AT TIME ZONE 'UTC')::date,50,50)`, user.ID); err != nil {
		t.Fatal(err)
	}
	_, e := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{"external@example.test"}, Subject: "synthetic", Body: "test"}, "send", "", nil, nil)
	if e == nil || e.Status != 402 || e.Details["limit"] != 50 || e.Details["current"] != 50 {
		t.Fatalf("shared refusal: %+v", e)
	}
}
