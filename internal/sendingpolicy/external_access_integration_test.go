package sendingpolicy_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/migrations"
)

// External sending access, driven through the real gate against real
// Postgres. Every address and account is synthetic.

const esaCutoff = "2026-01-01T00:00:00Z"

// esaPolicy is the disabled default with the external-access control armed.
// Budgets stay disabled so these tests isolate the permission from the
// independent budget controls.
func esaPolicy(mode sendingpolicy.Mode) sendingpolicy.RuntimePolicy {
	p := sendingpolicy.DisabledPolicy()
	p.ExternalSendingAccess = &sendingpolicy.ExternalSendingAccessPolicy{Mode: mode, AccountsCreatedAtOrAfter: esaCutoff}
	return p
}

// esaMessage inserts an outbound message with an explicit envelope.
func (f *fixture) esaMessage(agentID, sentAs string, to, cc, bcc []string) string {
	f.t.Helper()
	messageSeq++
	id := fmt.Sprintf("msg_esa_%d", messageSeq)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO messages (id, agent_id, direction, to_recipients, cc, bcc, sent_as, status)
		 VALUES ($1, $2, 'outbound', $3, $4, $5, $6, 'sent')`,
		id, agentID, to, cc, bcc, sentAs,
	); err != nil {
		f.t.Fatalf("insert message: %v", err)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *fixture) proveOwner(userID string) {
	f.exec(`UPDATE users SET owner_email_verified_at = now(), owner_email_verified_address = lower(email),
	        owner_email_verified_source = 'google_oauth' WHERE id = $1`, userID)
}

func (f *fixture) ownerEmail(userID string) string {
	f.t.Helper()
	var email string
	if err := f.pool.QueryRow(f.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		f.t.Fatal(err)
	}
	return email
}

func (f *fixture) setApproved(userID string, approved bool) {
	f.exec(`INSERT INTO account_sending_controls (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, userID)
	f.exec(`UPDATE account_sending_controls SET external_sending_approved = $2,
	        external_sending_access_revision = external_sending_access_revision + 1,
	        external_sending_access_changed_at = now() WHERE user_id = $1`, userID, approved)
}

// setEntitled writes the billing-issued entitlement column the way the hosted
// billing writer does. The plan code is deliberately a paid-looking one in
// both directions: plan_code is never authorization.
func (f *fixture) setEntitled(userID string, entitled bool) {
	f.plan(userID, "pro")
	f.exec(`UPDATE account_limits SET external_sending_entitled = $2 WHERE user_id = $1`, userID, entitled)
}

// esaAgent inserts a live agent whose id is its address, the production shape.
func (f *fixture) esaAgent(userID, domain string) string {
	f.t.Helper()
	agentSeq++
	id := fmt.Sprintf("agent-%d@%s", agentSeq, domain)
	f.exec(`INSERT INTO agent_identities (id, user_id, registered_domain, name) VALUES ($1, $2, $3, $1)`, id, userID, domain)
	return id
}

func (f *fixture) customDomain(userID, domain, sendingStatus string) {
	f.exec(`INSERT INTO domains (domain, user_id, verified, verified_at, sending_status) VALUES ($1, $2, true, now(), $3)`,
		domain, userID, sendingStatus)
}

func (f *fixture) operationExists(messageID string) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sending_provider_operations WHERE operation_id = $1`, messageID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n > 0
}

const external = "someone@outside.example"

func TestExternalAccessAcceptanceMatrix(t *testing.T) {
	type setup func(f *fixture, user string) (agent string, to, cc, bcc []string, sentAs string)

	shared := func(f *fixture, user string) string { return f.esaAgent(user, "agents.e2a.dev") }

	cases := []struct {
		name  string
		mode  sendingpolicy.Mode
		build setup
		want  sendingpolicy.AcceptanceDecision
	}{
		{"disabled keeps existing behavior", sendingpolicy.ModeDisabled, func(f *fixture, u string) (string, []string, []string, []string, string) {
			return shared(f, u), []string{external}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceAccept},
		{"shadow never blocks", sendingpolicy.ModeShadow, func(f *fixture, u string) (string, []string, []string, []string, string) {
			return shared(f, u), []string{external}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceAccept},
		{"external To is refused", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			return shared(f, u), []string{external}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"verified owner mailbox is allowed", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.proveOwner(u)
			return shared(f, u), []string{f.ownerEmail(u)}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceAccept},
		{"owner mailbox case variant is the same mailbox", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.proveOwner(u)
			return shared(f, u), []string{"  " + upper(f.ownerEmail(u))}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceAccept},
		{"owner mailbox without proof is refused", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			return shared(f, u), []string{f.ownerEmail(u)}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"owner email change invalidates proof", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.proveOwner(u)
			f.exec(`UPDATE users SET email = 'changed-' || email WHERE id = $1`, u)
			return shared(f, u), []string{f.ownerEmail(u)}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"same-account live agent is allowed", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			sibling := shared(f, u)
			return shared(f, u), []string{sibling}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceAccept},
		{"trashed same-account agent is refused", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			sibling := shared(f, u)
			f.exec(`UPDATE agent_identities SET deleted_at = now() WHERE id = $1`, sibling)
			return shared(f, u), []string{sibling}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"another account's agent is refused", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			foreign := shared(f, f.user("standard"))
			return shared(f, u), []string{foreign}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"external Cc refuses the whole message", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.proveOwner(u)
			return shared(f, u), []string{f.ownerEmail(u)}, []string{external}, nil, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"external Bcc refuses the whole message", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			sibling := shared(f, u)
			return shared(f, u), []string{sibling}, nil, []string{external}, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"operator approval allows external", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.setApproved(u, true)
			return shared(f, u), []string{external}, nil, []string{"other@outside.example"}, "relay"
		}, sendingpolicy.AcceptanceAccept},
		{"paid entitlement allows external", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.setEntitled(u, true)
			return shared(f, u), []string{external}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceAccept},
		{"paid plan code without the entitlement column is not entitled", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.setEntitled(u, false)
			return shared(f, u), []string{external}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"missing account_limits row is not entitled", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			return shared(f, u), []string{external}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"pause wins over approval", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.setApproved(u, true)
			f.pause(u)
			return shared(f, u), []string{external}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceSendingPaused},
		{"account created before the cutoff is outside the cohort", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.exec(`UPDATE users SET created_at = '2025-06-01T00:00:00Z' WHERE id = $1`, u)
			return shared(f, u), []string{external}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceAccept},
		{"verified own domain sending as itself is allowed", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.customDomain(u, u+".example.test", "verified")
			return f.esaAgent(u, u+".example.test"), []string{external}, nil, nil, "own_address"
		}, sendingpolicy.AcceptanceAccept},
		{"own domain whose sending verification failed is refused", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.customDomain(u, u+".example.test", "failed")
			return f.esaAgent(u, u+".example.test"), []string{external}, nil, nil, "own_address"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"verified domain elsewhere does not approve a shared agent", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			f.customDomain(u, u+".example.test", "verified")
			return shared(f, u), []string{external}, nil, nil, "relay"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
		{"own_address marker alone is not identity proof", sendingpolicy.ModeEnforce, func(f *fixture, u string) (string, []string, []string, []string, string) {
			return shared(f, u), []string{external}, nil, nil, "own_address"
		}, sendingpolicy.AcceptanceExternalSendingNotEnabled},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			g := f.gate(esaPolicy(tc.mode))
			user := f.user("standard")
			agent, to, cc, bcc, sentAs := tc.build(f, user)
			msg := f.esaMessage(agent, sentAs, to, cc, bcc)
			got, ref := f.prepareMessage(g, msg)
			if got != tc.want {
				t.Fatalf("acceptance = %q, want %q", got, tc.want)
			}
			if tc.want != sendingpolicy.AcceptanceAccept {
				if !ref.IsZero() || f.operationExists(msg) {
					t.Fatal("a refused acceptance must create no provider operation")
				}
				return
			}
			if d := f.authorize(g, ref); !d.Allow {
				t.Fatalf("an accepted message must also authorize: %+v", d)
			}
		})
	}
}

func upper(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 32
		}
	}
	return string(out)
}

func TestExternalAccessExemptClassIsOutsideTheRule(t *testing.T) {
	for _, class := range []string{"system", "internal"} {
		f := newFixture(t)
		g := f.gate(esaPolicy(sendingpolicy.ModeEnforce))
		user := f.user(class)
		msg := f.esaMessage(f.esaAgent(user, "agents.e2a.dev"), "relay", []string{external}, nil, nil)
		if got, _ := f.prepareMessage(g, msg); got != sendingpolicy.AcceptanceAccept {
			t.Fatalf("class %s: acceptance = %q", class, got)
		}
	}
	f := newFixture(t)
	g := f.gate(esaPolicy(sendingpolicy.ModeEnforce))
	user := f.user("demo")
	msg := f.esaMessage(f.esaAgent(user, "agents.e2a.dev"), "relay", []string{external}, nil, nil)
	if got, _ := f.prepareMessage(g, msg); got != sendingpolicy.AcceptanceExternalSendingNotEnabled {
		t.Fatalf("an unlisted class must be restricted, got %q", got)
	}
}

// A message accepted before enforcement (or while approved) is refused at
// final authorization once the rule binds, terminally, with its units given
// back — queued mail never waits for a later approval.
func TestExternalAccessRevocationBetweenAcceptanceAndAuthorizationIsTerminal(t *testing.T) {
	f := newFixture(t)
	policy := esaPolicy(sendingpolicy.ModeEnforce)
	policy.BudgetMode = sendingpolicy.ModeEnforce
	g := f.gate(policy)
	user := f.user("standard")
	f.setApproved(user, true)
	msg := f.esaMessage(f.esaAgent(user, "agents.e2a.dev"), "relay", []string{external}, nil, nil)
	accept, ref := f.prepareMessage(g, msg)
	if accept != sendingpolicy.AcceptanceAccept {
		t.Fatalf("approved acceptance = %q", accept)
	}
	early, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil || !early.Allow {
		t.Fatalf("reserve: %+v %v", early, err)
	}
	f.setApproved(user, false)

	d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if auth != nil || d.Allow || !d.Terminal || d.Reason != sendingpolicy.ReasonExternalSendingNotEnabled {
		t.Fatalf("decision = %+v, want terminal %s", d, sendingpolicy.ReasonExternalSendingNotEnabled)
	}
	if reserved, _ := f.counter(sendingpolicy.ScopeAccountDaily, user); reserved != 0 {
		t.Fatalf("a terminal refusal must release its reservation, reserved=%d", reserved)
	}
}

// The final authority is redemption: a revocation that commits after the
// token was minted but before the socket opens refuses the call.
func TestExternalAccessRevocationBeforeRedemptionInvalidates(t *testing.T) {
	for name, revoke := range map[string]func(f *fixture, user, sibling string){
		"grant revoked":    func(f *fixture, user, _ string) { f.setApproved(user, false) },
		"entitlement lost": func(f *fixture, user, _ string) { f.setEntitled(user, false) },
		"recipient trashed": func(f *fixture, _, sibling string) {
			f.exec(`UPDATE agent_identities SET deleted_at = now() WHERE id = $1`, sibling)
		},
		"owner email change": func(f *fixture, user, _ string) {
			f.exec(`UPDATE users SET email = 'new-' || email WHERE id = $1`, user)
		},
		"domain verification lost": func(f *fixture, user, _ string) {
			f.exec(`UPDATE domains SET sending_status = 'failed' WHERE user_id = $1`, user)
		},
		"domain ownership unverified": func(f *fixture, user, _ string) {
			f.exec(`UPDATE domains SET verified = false WHERE user_id = $1`, user)
		},
	} {
		revoke := revoke
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			g := f.gate(esaPolicy(sendingpolicy.ModeEnforce))
			user := f.user("standard")
			agent := f.esaAgent(user, "agents.e2a.dev")
			sibling := f.esaAgent(user, "agents.e2a.dev")
			sentAs := "relay"
			var to []string
			switch name {
			case "domain verification lost", "domain ownership unverified":
				f.customDomain(user, user+".example.test", "verified")
				agent = f.esaAgent(user, user+".example.test")
				sentAs = "own_address"
				to = []string{external}
			case "grant revoked":
				f.setApproved(user, true)
				to = []string{external}
			case "entitlement lost":
				f.setEntitled(user, true)
				to = []string{external}
			case "recipient trashed":
				to = []string{sibling}
			case "owner email change":
				f.proveOwner(user)
				to = []string{f.ownerEmail(user)}
			}
			msg := f.esaMessage(agent, sentAs, to, nil, nil)
			_, ref := f.prepareMessage(g, msg)
			early, attempt, err := g.Reserve(f.ctx, ref)
			if err != nil || !early.Allow {
				t.Fatalf("reserve: %+v %v", early, err)
			}
			d, auth, err := g.ConsumeAttempt(f.ctx, attempt)
			if err != nil || !d.Allow || auth == nil {
				t.Fatalf("consume: %+v %v", d, err)
			}
			revoke(f, user, sibling)
			if err := g.RedeemProviderCall(f.ctx, *auth); !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
				t.Fatalf("redeem err = %v, want ErrAuthorizationInvalid", err)
			}
			// The next execution is refused terminally, never sent.
			_, next, err := g.Reserve(f.ctx, ref)
			if err != nil {
				t.Fatalf("re-reserve: %v", err)
			}
			d2, _, err := g.ConsumeAttempt(f.ctx, next)
			if err != nil || !d2.Terminal || d2.Reason != sendingpolicy.ReasonExternalSendingNotEnabled {
				t.Fatalf("retry decision = %+v err=%v", d2, err)
			}
		})
	}
}

// Dependency failures fail closed: an unreadable decision is an error, never
// an allow and never a claim that approval is absent.
func TestExternalAccessFailsClosedOnReadError(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	user := f.user("standard")
	agent := f.esaAgent(user, "agents.e2a.dev")
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := m.ExternalAccessPreflight(ctx, user, agent, []string{external}); err == nil {
		t.Fatal("a preflight that could not read state must return an error")
	}
	if _, err := m.ExternalAccessStatus(ctx, user); err == nil {
		t.Fatal("a status read that could not read state must return an error")
	}
	// Disabled mode performs no read at all and so cannot fail.
	off := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	v, err := off.ExternalAccessPreflight(ctx, user, agent, []string{external})
	if err != nil || !v.Allowed || v.Route != sendingpolicy.RouteNotApplicable {
		t.Fatalf("disabled preflight = %+v err=%v", v, err)
	}
}

func TestExternalAccessPreflightAndStatus(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	user := f.user("standard")
	agent := f.esaAgent(user, "agents.e2a.dev")
	sibling := f.esaAgent(user, "agents.e2a.dev")

	st, err := m.ExternalAccessStatus(f.ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if st != (sendingpolicy.ExternalAccessStatus{EnforcementApplies: true}) {
		t.Fatalf("fresh status = %+v", st)
	}
	if v, err := m.ExternalAccessPreflight(f.ctx, user, agent, []string{sibling}); err != nil || !v.Allowed || v.Route != sendingpolicy.RouteRestrictedRecipients {
		t.Fatalf("sibling preflight = %+v err=%v", v, err)
	}
	if v, err := m.ExternalAccessPreflight(f.ctx, user, agent, []string{sibling, external}); err != nil || v.Allowed {
		t.Fatalf("mixed preflight = %+v err=%v", v, err)
	}

	f.proveOwner(user)
	f.setApproved(user, true)
	f.setEntitled(user, true)
	st, err = m.ExternalAccessStatus(f.ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	want := sendingpolicy.ExternalAccessStatus{EnforcementApplies: true, SharedExternalApproved: true, PaidExternalSendingEntitled: true, OwnerRecipientVerified: true}
	if st != want {
		t.Fatalf("status = %+v, want %+v", st, want)
	}

	shadow := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeShadow))
	if st, err := shadow.ExternalAccessStatus(f.ctx, user); err != nil || st.EnforcementApplies {
		t.Fatalf("shadow must not report enforcement: %+v err=%v", st, err)
	}
}

func TestExternalAccessOperatorChanges(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	user := f.user("standard")

	rec, err := m.InspectExternalAccess(f.ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Approved || rec.Revision != 0 || rec.ChangedAt != nil || !rec.EnforcementApplies {
		t.Fatalf("fresh record = %+v", rec)
	}

	change := sendingpolicy.ExternalAccessChange{AccountID: user, Approved: true, ExpectedRevision: 1, Actor: "cli:test", Reason: "reviewed use case"}
	if _, err := m.SetExternalAccess(f.ctx, change); !errors.Is(err, sendingpolicy.ErrStaleExternalAccessRevision) {
		t.Fatalf("stale revision err = %v", err)
	}
	if n := f.accessEvents(user); n != 0 {
		t.Fatalf("a stale change must write nothing, events=%d", n)
	}

	change.ExpectedRevision = 0
	res, err := m.SetExternalAccess(f.ctx, change)
	if err != nil || res.NoOp || !res.Record.Approved || res.Record.Revision != 1 {
		t.Fatalf("approve = %+v err=%v", res, err)
	}
	// Retrying the same change after a lost response inspects as stale, and
	// the same state at the current revision is a documented no-op.
	if _, err := m.SetExternalAccess(f.ctx, change); !errors.Is(err, sendingpolicy.ErrStaleExternalAccessRevision) {
		t.Fatalf("replayed change err = %v", err)
	}
	change.ExpectedRevision = 1
	res, err = m.SetExternalAccess(f.ctx, change)
	if err != nil || !res.NoOp || res.Record.Revision != 1 {
		t.Fatalf("same-state change = %+v err=%v", res, err)
	}
	if n := f.accessEvents(user); n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}

	// Approval never clears a pause, and revocation leaves the paid
	// entitlement visible to the operator.
	f.pause(user)
	f.setEntitled(user, true)
	res, err = m.SetExternalAccess(f.ctx, sendingpolicy.ExternalAccessChange{AccountID: user, Approved: false, ExpectedRevision: 1, Actor: "cli:test", Reason: "abuse report"})
	if err != nil || res.Record.Approved || !res.Record.Paused || !res.Record.PaidEntitled || res.Record.Revision != 2 {
		t.Fatalf("revoke = %+v err=%v", res, err)
	}
	if n := f.accessEvents(user); n != 2 {
		t.Fatalf("events = %d, want 2", n)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE external_sending_access_events SET reason = 'x' WHERE account_ref = $1`, user); err == nil {
		t.Fatal("access events must be append-only")
	}

	if _, err := m.SetExternalAccess(f.ctx, sendingpolicy.ExternalAccessChange{AccountID: "usr_missing", Approved: true, Actor: "cli:test", Reason: "x"}); !errors.Is(err, sendingpolicy.ErrAccountNotFound) {
		t.Fatalf("missing account err = %v", err)
	}
	if _, err := m.SetExternalAccess(f.ctx, sendingpolicy.ExternalAccessChange{AccountID: user, Approved: true, ExpectedRevision: 2, Actor: "cli:test", Reason: "  "}); err == nil {
		t.Fatal("a blank reason must be refused")
	}
}

func (f *fixture) accessEvents(user string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM external_sending_access_events WHERE account_ref = $1`, user).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestExternalAccessRequests(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	user := f.user("standard")
	in := sendingpolicy.AccessRequestInput{UseCase: "support replies", Recipients: "our customers", ExpectedDailyVolume: 20}

	if _, _, err := m.SubmitAccessRequest(f.ctx, user, sendingpolicy.AccessRequestInput{UseCase: " ", Recipients: "x", ExpectedDailyVolume: 1}); !errors.Is(err, sendingpolicy.ErrInvalidAccessRequest) {
		t.Fatalf("blank use case err = %v", err)
	}
	if _, _, err := m.SubmitAccessRequest(f.ctx, user, sendingpolicy.AccessRequestInput{UseCase: "x", Recipients: "x", ExpectedDailyVolume: 0}); !errors.Is(err, sendingpolicy.ErrInvalidAccessRequest) {
		t.Fatalf("zero volume err = %v", err)
	}

	first, created, err := m.SubmitAccessRequest(f.ctx, user, in)
	if err != nil || !created || first.State != "pending" {
		t.Fatalf("first submit = %+v created=%v err=%v", first, created, err)
	}
	again, created, err := m.SubmitAccessRequest(f.ctx, user, sendingpolicy.AccessRequestInput{UseCase: "different", Recipients: "different", ExpectedDailyVolume: 5})
	if err != nil || created || again.ID != first.ID || again.UseCase != "support replies" {
		t.Fatalf("pending resubmit must return the existing request: %+v created=%v err=%v", again, created, err)
	}

	// Decline, then appeal twice more; the fourth request in the window is
	// rate limited.
	for i := 0; i < 2; i++ {
		latest, err := m.LatestAccessRequest(f.ctx, user)
		if err != nil || latest == nil {
			t.Fatalf("latest: %v", err)
		}
		if err := m.DeclineExternalAccessRequest(f.ctx, user, latest.ID, "cli:test"); err != nil {
			t.Fatalf("decline: %v", err)
		}
		if err := m.DeclineExternalAccessRequest(f.ctx, user, latest.ID, "cli:test"); !errors.Is(err, sendingpolicy.ErrAccessRequestNotPending) {
			t.Fatalf("double decline err = %v", err)
		}
		if _, created, err := m.SubmitAccessRequest(f.ctx, user, in); err != nil || !created {
			t.Fatalf("appeal %d: created=%v err=%v", i, created, err)
		}
	}
	latest, _ := m.LatestAccessRequest(f.ctx, user)
	if err := m.DeclineExternalAccessRequest(f.ctx, user, latest.ID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SubmitAccessRequest(f.ctx, user, in); !errors.Is(err, sendingpolicy.ErrAccessRequestRateLimited) {
		t.Fatalf("fourth request err = %v", err)
	}

	// Approving through the operator command decides a pending request; a
	// request of another account cannot be decided through this account.
	other := f.user("standard")
	req, _, err := m.SubmitAccessRequest(f.ctx, other, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetExternalAccess(f.ctx, sendingpolicy.ExternalAccessChange{AccountID: user, Approved: true, ExpectedRevision: 0, Actor: "cli:test", Reason: "x", RequestID: req.ID}); !errors.Is(err, sendingpolicy.ErrAccessRequestNotFound) {
		t.Fatalf("cross-account request err = %v", err)
	}
	if n := f.accessEvents(user); n != 0 {
		t.Fatalf("a failed decision must roll back the grant, events=%d", n)
	}
	res, err := m.SetExternalAccess(f.ctx, sendingpolicy.ExternalAccessChange{AccountID: other, Approved: true, ExpectedRevision: 0, Actor: "cli:test", Reason: "x", RequestID: req.ID})
	if err != nil || !res.Record.Approved || res.Record.PendingRequestID != "" {
		t.Fatalf("approve with request = %+v err=%v", res, err)
	}
	latest, _ = m.LatestAccessRequest(f.ctx, other)
	if latest.State != "approved" || latest.DecidedAt == nil {
		t.Fatalf("request after approval = %+v", latest)
	}

	// Requests are customer data and leave with the account.
	f.exec(`DELETE FROM users WHERE id = $1`, other)
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM external_sending_access_requests WHERE user_id = $1`, other).Scan(&n); err != nil || n != 0 {
		t.Fatalf("requests after account deletion = %d err=%v", n, err)
	}
	if f.accessEvents(other) != 1 {
		t.Fatal("the audit event outlives the account until its retention expires")
	}
	_ = pgx.ErrNoRows
}

func TestExternalAccessMigrationIsIdempotentAndConservative(t *testing.T) {
	f := newFixture(t)
	user := f.user("standard")
	f.exec(`INSERT INTO account_sending_controls (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user)
	f.plan(user, "pro")

	sql, err := migrations.FS.ReadFile("121_external_sending_access.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.pool.Exec(f.ctx, string(sql)); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}

	// Existing rows stay unapproved, unentitled and without proof — a paid
	// plan_code is not the entitlement and a nonempty email is not proof.
	var approved, entitled bool
	var revision int64
	var proof *string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT c.external_sending_approved, c.external_sending_access_revision, l.external_sending_entitled, u.owner_email_verified_address
		  FROM users u JOIN account_sending_controls c ON c.user_id = u.id JOIN account_limits l ON l.user_id = u.id
		 WHERE u.id = $1`, user).Scan(&approved, &revision, &entitled, &proof); err != nil {
		t.Fatal(err)
	}
	if approved || revision != 0 || entitled || proof != nil {
		t.Fatalf("defaults: approved=%v revision=%d entitled=%v proof=%v", approved, revision, entitled, proof)
	}

	for name, stmt := range map[string]string{
		"approval without an operator revision": `UPDATE account_sending_controls SET external_sending_approved = true WHERE user_id = $1`,
		"negative revision":                     `UPDATE account_sending_controls SET external_sending_access_revision = -1 WHERE user_id = $1`,
		"partial owner proof":                   `UPDATE users SET owner_email_verified_at = now() WHERE id = $1`,
		"unnormalized owner proof": `UPDATE users SET owner_email_verified_at = now(), owner_email_verified_source = 'google_oauth',
		                             owner_email_verified_address = 'Mixed@Example.test' WHERE id = $1`,
		"unknown proof source": `UPDATE users SET owner_email_verified_at = now(), owner_email_verified_source = 'profile_edit',
		                         owner_email_verified_address = 'x@example.test' WHERE id = $1`,
	} {
		if _, err := f.pool.Exec(f.ctx, stmt, user); err == nil {
			t.Errorf("%s: constraint must reject", name)
		}
	}
	f.exec(`INSERT INTO external_sending_access_requests (id, user_id, use_case, recipients, expected_daily_volume) VALUES ('esar_a', $1, 'x', 'y', 1)`, user)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO external_sending_access_requests (id, user_id, use_case, recipients, expected_daily_volume) VALUES ('esar_b', $1, 'x', 'y', 1)`, user); err == nil {
		t.Error("a second pending request for one account must be rejected")
	}
	var reason string
	if err := f.pool.QueryRow(f.ctx, `SELECT stage || '/' || outcome || '/' || retryable::text FROM message_lifecycle_reason_codes WHERE code = 'submission.external_sending_not_enabled'`).Scan(&reason); err != nil || reason != "submission/failed/false" {
		t.Fatalf("lifecycle reason row = %q err=%v", reason, err)
	}
}

func TestExternalAccessEntitlementIsTheBillingColumnOnly(t *testing.T) {
	f := newFixture(t)
	g := f.gate(esaPolicy(sendingpolicy.ModeEnforce))
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	user := f.user("standard")
	agent := f.esaAgent(user, "agents.e2a.dev")
	// A trialing/past_due account keeps a paid plan_code; without the column
	// it is not entitled.
	for _, code := range []string{"pro", "starter", "scale"} {
		f.plan(user, code)
		if got, _ := f.prepareMessage(g, f.esaMessage(agent, "relay", []string{external}, nil, nil)); got != sendingpolicy.AcceptanceExternalSendingNotEnabled {
			t.Fatalf("plan_code %q alone must not entitle, got %q", code, got)
		}
	}
	if st, err := m.ExternalAccessStatus(f.ctx, user); err != nil || st.PaidExternalSendingEntitled {
		t.Fatalf("status = %+v err=%v", st, err)
	}
	f.setEntitled(user, true)
	if got, _ := f.prepareMessage(g, f.esaMessage(agent, "relay", []string{external}, nil, nil)); got != sendingpolicy.AcceptanceAccept {
		t.Fatalf("the entitlement column must allow external sending, got %q", got)
	}
	if st, err := m.ExternalAccessStatus(f.ctx, user); err != nil || !st.PaidExternalSendingEntitled {
		t.Fatalf("status = %+v err=%v", st, err)
	}
}

// Concurrent operator changes at one inspected revision: exactly one wins,
// the other is stale and writes nothing.
func TestExternalAccessConcurrentChangesSerialize(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	user := f.user("standard")
	results := make(chan error, 2)
	for _, approved := range []bool{true, true} {
		approved := approved
		go func() {
			_, err := m.SetExternalAccess(f.ctx, sendingpolicy.ExternalAccessChange{
				AccountID: user, Approved: approved, ExpectedRevision: 0, Actor: "cli:test", Reason: "race",
			})
			results <- err
		}()
	}
	var ok, stale int
	for i := 0; i < 2; i++ {
		switch err := <-results; {
		case err == nil:
			ok++
		case errors.Is(err, sendingpolicy.ErrStaleExternalAccessRevision):
			stale++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || stale != 1 || f.accessEvents(user) != 1 {
		t.Fatalf("ok=%d stale=%d events=%d, want exactly one winner", ok, stale, f.accessEvents(user))
	}
}

// An operator change and an in-flight authorization for the same account take
// their locks in the same order and must not deadlock.
func TestExternalAccessChangeDoesNotDeadlockWithAuthorization(t *testing.T) {
	f := newFixture(t)
	policy := esaPolicy(sendingpolicy.ModeEnforce)
	g := f.gate(policy)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, policy)
	user := f.user("standard")
	f.setApproved(user, true)
	agent := f.esaAgent(user, "agents.e2a.dev")
	refs := make([]sendingpolicy.OperationRef, 0, 8)
	for i := 0; i < 8; i++ {
		_, ref := f.prepareMessage(g, f.esaMessage(agent, "relay", []string{external}, nil, nil))
		refs = append(refs, ref)
	}
	done := make(chan error, len(refs)+1)
	for _, ref := range refs {
		ref := ref
		go func() {
			_, attempt, err := g.Reserve(f.ctx, ref)
			if err == nil {
				_, _, err = g.ConsumeAttempt(f.ctx, attempt)
			}
			done <- err
		}()
	}
	go func() {
		_, err := m.SetExternalAccess(f.ctx, sendingpolicy.ExternalAccessChange{AccountID: user, Approved: false, ExpectedRevision: 1, Actor: "cli:test", Reason: "race"})
		done <- err
	}()
	for i := 0; i < len(refs)+1; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent authorization/change failed (deadlock?): %v", err)
		}
	}
}

// Malformed or empty recipients are a validation problem, never a permission
// answer — and never for accounts the rule does not bind.
func TestExternalAccessPreflightMalformedIsNotAPermissionAnswer(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	for _, class := range []string{"standard", "system"} {
		user := f.user(class)
		agent := f.esaAgent(user, "agents.e2a.dev")
		for _, rcpts := range [][]string{nil, {`"john doe"@example.com`}, {"not-an-address"}} {
			v, err := m.ExternalAccessPreflight(f.ctx, user, agent, rcpts)
			if err != nil || !v.Allowed || v.Paused {
				t.Fatalf("class %s recipients %v: %+v err=%v, want not a permission answer", class, rcpts, v, err)
			}
		}
	}
}

// Pause wins at the preflight too, before any access answer.
func TestExternalAccessPreflightReportsPauseFirst(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	user := f.user("standard")
	f.setApproved(user, true)
	f.pause(user)
	v, err := m.ExternalAccessPreflight(f.ctx, user, f.esaAgent(user, "agents.e2a.dev"), []string{external})
	if err != nil || !v.Paused || v.Allowed {
		t.Fatalf("paused preflight = %+v err=%v", v, err)
	}
}

// Feature off: no status object and no request intake.
func TestExternalAccessDisabledSurfaces(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	user := f.user("standard")
	if _, err := m.ExternalAccessStatus(f.ctx, user); !errors.Is(err, sendingpolicy.ErrExternalAccessDisabled) {
		t.Fatalf("status err = %v", err)
	}
	if _, _, err := m.SubmitAccessRequest(f.ctx, user, sendingpolicy.AccessRequestInput{UseCase: "x", Recipients: "y", ExpectedDailyVolume: 1}); !errors.Is(err, sendingpolicy.ErrExternalAccessDisabled) {
		t.Fatalf("submit err = %v", err)
	}
	if _, err := m.LatestAccessRequest(f.ctx, user); !errors.Is(err, sendingpolicy.ErrExternalAccessDisabled) {
		t.Fatalf("latest err = %v", err)
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM external_sending_access_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d err=%v, want none filed", n, err)
	}
}
