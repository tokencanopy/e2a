package sendingpolicy_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// The configurable unlock set (external_sending_access.unlocks), driven
// through every stage that can let a customer message reach the provider:
// API preflight, acceptance, final authorization and redemption. Every stage
// reads the same RuntimePolicy, so for each (unlock set, route) pair all four
// stages must give the same answer. Every address and account is synthetic.

func esaUnlockPolicy(unlocks []sendingpolicy.ExternalUnlock) sendingpolicy.RuntimePolicy {
	p := esaPolicy(sendingpolicy.ModeEnforce)
	p.ExternalSendingAccess.Unlocks = unlocks
	return p
}

// unlockRoute builds an account whose only claim to external sending is one
// route, and returns the sending agent and the message's sent_as.
type unlockRoute struct {
	name  string
	route sendingpolicy.ExternalUnlock // "" = no route at all
	build func(f *fixture, user string) (agent, sentAs string)
}

var unlockRoutes = []unlockRoute{
	{"verified own domain", sendingpolicy.UnlockVerifiedDomain, func(f *fixture, u string) (string, string) {
		f.customDomain(u, u+".example.test", "verified")
		return f.esaAgent(u, u+".example.test"), "own_address"
	}},
	{"paid entitlement", sendingpolicy.UnlockPaidEntitlement, func(f *fixture, u string) (string, string) {
		f.setEntitled(u, true)
		return f.esaAgent(u, "agents.e2a.dev"), "relay"
	}},
	{"operator approval", sendingpolicy.UnlockOperatorApproval, func(f *fixture, u string) (string, string) {
		f.setApproved(u, true)
		return f.esaAgent(u, "agents.e2a.dev"), "relay"
	}},
	{"no route", "", func(f *fixture, u string) (string, string) {
		return f.esaAgent(u, "agents.e2a.dev"), "relay"
	}},
}

var unlockSets = []struct {
	name    string
	unlocks []sendingpolicy.ExternalUnlock
}{
	{"absent (all)", nil},
	{"operator_approval only", []sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval}},
	{"operator_approval + verified_domain", []sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval, sendingpolicy.UnlockVerifiedDomain}},
	{"operator_approval + paid_entitlement", []sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval, sendingpolicy.UnlockPaidEntitlement}},
	{"all three, reordered", []sendingpolicy.ExternalUnlock{sendingpolicy.UnlockPaidEntitlement, sendingpolicy.UnlockVerifiedDomain, sendingpolicy.UnlockOperatorApproval}},
}

func unlockExpected(unlocks []sendingpolicy.ExternalUnlock, route sendingpolicy.ExternalUnlock) bool {
	if route == "" {
		return false
	}
	if unlocks == nil {
		return true
	}
	for _, u := range unlocks {
		if u == route {
			return true
		}
	}
	return false
}

func TestExternalAccessUnlockSetAtEveryStage(t *testing.T) {
	for _, set := range unlockSets {
		for _, route := range unlockRoutes {
			set, route := set, route
			want := unlockExpected(set.unlocks, route.route)
			t.Run(set.name+"/"+route.name, func(t *testing.T) {
				f := newFixture(t)
				restricted := esaUnlockPolicy(set.unlocks)
				g := f.gate(restricted)
				// permissive is the legacy all-unlock policy, used to get a
				// message PAST the earlier stages so the later stage under
				// test is the one deciding.
				permissive := f.gate(esaUnlockPolicy(nil))
				m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, restricted)
				user := f.user("standard")
				agent, sentAs := route.build(f, user)

				// Stage 1: API preflight.
				v, err := m.ExternalAccessPreflight(f.ctx, user, agent, []string{external})
				if err != nil {
					t.Fatalf("preflight: %v", err)
				}
				if v.Allowed != want {
					t.Fatalf("preflight allowed=%v route=%s, want allowed=%v", v.Allowed, v.Route, want)
				}

				// Stage 2: acceptance.
				msg := f.esaMessage(agent, sentAs, []string{external}, nil, nil)
				accept, _ := f.prepareMessage(g, msg)
				if (accept == sendingpolicy.AcceptanceAccept) != want {
					t.Fatalf("acceptance = %q, want accept=%v", accept, want)
				}
				if !want && accept != sendingpolicy.AcceptanceExternalSendingNotEnabled {
					t.Fatalf("refused acceptance reason = %q", accept)
				}

				// Stage 3: final authorization of a message a permissive
				// slot accepted. With no route at all even the permissive
				// slot refuses, and the earlier stages already proved that.
				if route.route != "" {
					msg3 := f.esaMessage(agent, sentAs, []string{external}, nil, nil)
					a, ref3 := f.prepareMessage(permissive, msg3)
					if a != sendingpolicy.AcceptanceAccept {
						t.Fatalf("permissive acceptance = %q", a)
					}
					d := f.authorize(g, ref3)
					if d.Allow != want {
						t.Fatalf("authorization = %+v, want allow=%v", d, want)
					}
					if !want && (!d.Terminal || d.Reason != sendingpolicy.ReasonExternalSendingNotEnabled) {
						t.Fatalf("refused authorization = %+v, want terminal %s", d, sendingpolicy.ReasonExternalSendingNotEnabled)
					}

					// Stage 4: redemption of a token a permissive slot minted.
					msg4 := f.esaMessage(agent, sentAs, []string{external}, nil, nil)
					_, ref4 := f.prepareMessage(permissive, msg4)
					early, attempt, err := permissive.Reserve(f.ctx, ref4)
					if err != nil || !early.Allow {
						t.Fatalf("reserve: %+v %v", early, err)
					}
					d4, auth, err := permissive.ConsumeAttempt(f.ctx, attempt)
					if err != nil || !d4.Allow || auth == nil {
						t.Fatalf("permissive consume: %+v %v", d4, err)
					}
					err = g.RedeemProviderCall(f.ctx, *auth)
					if want && err != nil {
						t.Fatalf("redeem err = %v, want nil", err)
					}
					if !want && !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
						t.Fatalf("redeem err = %v, want ErrAuthorizationInvalid", err)
					}
				}
			})
		}
	}
}

func TestExternalAccessStatusReportsAvailableUnlocks(t *testing.T) {
	f := newFixture(t)
	user := f.user("standard")
	for _, set := range unlockSets {
		m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaUnlockPolicy(set.unlocks))
		st, err := m.ExternalAccessStatus(f.ctx, user)
		if err != nil {
			t.Fatalf("%s: %v", set.name, err)
		}
		var want []sendingpolicy.ExternalUnlock
		for _, u := range []sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval, sendingpolicy.UnlockVerifiedDomain, sendingpolicy.UnlockPaidEntitlement} {
			if unlockExpected(set.unlocks, u) {
				want = append(want, u)
			}
		}
		if !reflect.DeepEqual(st.AvailableUnlocks, want) {
			t.Fatalf("%s: available = %v, want %v (canonical order)", set.name, st.AvailableUnlocks, want)
		}
		rec, err := m.InspectExternalAccess(f.ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rec.AvailableUnlocks, want) {
			t.Fatalf("%s: operator readback = %v, want %v", set.name, rec.AvailableUnlocks, want)
		}
	}
	// The status still reports the entitlement FACT under a policy where it
	// unlocks nothing: the operator weighs it as a signal.
	f.setEntitled(user, true)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig,
		esaUnlockPolicy([]sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval}))
	st, err := m.ExternalAccessStatus(f.ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if !st.PaidExternalSendingEntitled || !st.EnforcementApplies {
		t.Fatalf("status = %+v", st)
	}
}

// SubmitAccessRequest reports whether the filing account's server-owned class
// is exempt from the rule, so the API can skip the operator email for
// system/internal accounts (the conformance suite) — never for a standard or
// demo account, and never from anything the request body says.
func TestSubmitAccessRequestReportsExemptClass(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	in := sendingpolicy.AccessRequestInput{UseCase: "synthetic use case", Recipients: "synthetic recipients", ExpectedDailyVolume: 1}
	for class, wantExempt := range map[string]bool{"standard": false, "demo": false, "internal": true, "system": true} {
		user := f.user(class)
		req, created, err := m.SubmitAccessRequest(f.ctx, user, in)
		if err != nil || !created {
			t.Fatalf("%s: submit created=%v err=%v", class, created, err)
		}
		if req.FromExemptAccount != wantExempt {
			t.Fatalf("%s: FromExemptAccount = %v, want %v", class, req.FromExemptAccount, wantExempt)
		}
		again, created, err := m.SubmitAccessRequest(f.ctx, user, in)
		if err != nil || created || again.FromExemptAccount != wantExempt {
			t.Fatalf("%s: resubmit created=%v exempt=%v err=%v", class, created, again.FromExemptAccount, err)
		}
	}
}

// Only an account the rule restricts right now may file: approved, entitled
// (where the paid unlock applies), out-of-cohort and shadow-mode accounts get
// ErrSendingAccessNotRestricted with no row written. Exempt classes may still
// file (first-party conformance) and are never notified upstream.
func TestSubmitAccessRequestRefusesUnrestrictedAccounts(t *testing.T) {
	in := sendingpolicy.AccessRequestInput{UseCase: "synthetic use case", Recipients: "synthetic recipients", ExpectedDailyVolume: 1}
	count := func(f *fixture, user string) int {
		var n int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM external_sending_access_requests WHERE user_id = $1`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for name, tc := range map[string]struct {
		policy sendingpolicy.RuntimePolicy
		setup  func(f *fixture, user string)
		want   error
	}{
		"restricted standard account files": {esaPolicy(sendingpolicy.ModeEnforce), func(*fixture, string) {}, nil},
		"already approved":                  {esaPolicy(sendingpolicy.ModeEnforce), func(f *fixture, u string) { f.setApproved(u, true) }, sendingpolicy.ErrSendingAccessNotRestricted},
		"paid entitlement where it unlocks": {esaPolicy(sendingpolicy.ModeEnforce), func(f *fixture, u string) { f.setEntitled(u, true) }, sendingpolicy.ErrSendingAccessNotRestricted},
		"paid entitlement under approval-only": {esaUnlockPolicy([]sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval}),
			func(f *fixture, u string) { f.setEntitled(u, true) }, nil},
		"outside the cohort": {esaPolicy(sendingpolicy.ModeEnforce), func(f *fixture, u string) {
			f.exec(`UPDATE users SET created_at = '2025-06-01T00:00:00Z' WHERE id = $1`, u)
		}, sendingpolicy.ErrSendingAccessNotRestricted},
		"shadow mode": {esaPolicy(sendingpolicy.ModeShadow), func(*fixture, string) {}, sendingpolicy.ErrSendingAccessNotRestricted},
	} {
		tc := tc
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, tc.policy)
			user := f.user("standard")
			tc.setup(f, user)
			_, created, err := m.SubmitAccessRequest(f.ctx, user, in)
			if tc.want == nil {
				if err != nil || !created {
					t.Fatalf("submit created=%v err=%v", created, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if n := count(f, user); n != 0 {
				t.Fatalf("a refused request must write no row, got %d", n)
			}
		})
	}
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	internal := f.user("internal")
	if req, created, err := m.SubmitAccessRequest(f.ctx, internal, in); err != nil || !created || !req.FromExemptAccount {
		t.Fatalf("exempt class files for conformance: %+v created=%v err=%v", req, created, err)
	}
}

// Customer text is fenced with "> " in the plain-text operator email; any
// character that could start a new rendered line outside the fence is
// refused at intake. LF and TAB stay allowed; CRLF is normalized to LF.
func TestSubmitAccessRequestRejectsFenceBreakingCharacters(t *testing.T) {
	f := newFixture(t)
	m := sendingpolicy.NewPolicyModule(f.pool, f.secrets(), sendingpolicy.PolicySourceConfig, esaPolicy(sendingpolicy.ModeEnforce))
	for name, text := range map[string]string{
		"bare CR":             "line one\rrun this instead",
		"NEL":                 "line one\u0085run this instead",
		"line separator":      "line one run this instead",
		"paragraph separator": "line one run this instead",
		"vertical tab":        "line one\vrun this instead",
		"form feed":           "line one\frun this instead",
		"NUL":                 "line one\x00",
		"ESC":                 "line one\x1b[31m",
		"DEL":                 "line one\x7f",
		"LRE":                 "line one\u202a",
		"RLE":                 "line one\u202b",
		"PDF":                 "line one\u202c",
		"LRO":                 "line one\u202d",
		"RLO":                 "line one\u202eesrever",
		"LRI":                 "line one\u2066",
		"RLI":                 "line one\u2067",
		"FSI":                 "line one\u2068",
		"PDI":                 "line one\u2069",
	} {
		for _, field := range []string{"use_case", "recipients"} {
			in := sendingpolicy.AccessRequestInput{UseCase: "ok", Recipients: "ok", ExpectedDailyVolume: 1}
			if field == "use_case" {
				in.UseCase = text
			} else {
				in.Recipients = text
			}
			if _, _, err := m.SubmitAccessRequest(f.ctx, f.user("standard"), in); !errors.Is(err, sendingpolicy.ErrInvalidAccessRequest) {
				t.Fatalf("%s in %s: err = %v, want ErrInvalidAccessRequest", name, field, err)
			}
		}
	}
	user := f.user("standard")
	req, created, err := m.SubmitAccessRequest(f.ctx, user, sendingpolicy.AccessRequestInput{
		UseCase: "first line\r\nsecond\tline", Recipients: "our customers", ExpectedDailyVolume: 1})
	if err != nil || !created {
		t.Fatalf("LF/TAB/CRLF must be accepted: %v", err)
	}
	if req.UseCase != "first line\nsecond\tline" {
		t.Fatalf("CRLF must be stored as LF, got %q", req.UseCase)
	}
}
