package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
)

// Contract tests for /v1/agents/{email}/contacts — the outreach surface, from
// design §8.3.
//
// The authorization model here is the deliberate divergence in this feature:
// unlike /v1/contacts (account-only) and unlike agent_suppressions
// (account-only), an AGENT-scoped credential may read and write its own
// engagements. That is the point — the agent runs its own outreach loop. It is
// safe only because consent lives in a different table the agent cannot touch,
// so these tests pin both halves: an agent CAN move its own stage, and CANNOT
// reach a sibling agent's engagements or account-wide contact identity.

type engagementFixture struct {
	mu   sync.Mutex
	rows map[string]identity.ContactEngagement // key: userID \x00 agentID \x00 address
}

func (f *engagementFixture) key(userID, agentID, address string) string {
	return userID + "\x00" + identity.NormalizeEmail(agentID) + "\x00" + identity.NormalizeMailboxAddress(address)
}

func newEngagementsServer(t *testing.T, mutate func(*Deps, *engagementFixture)) *httptest.Server {
	t.Helper()
	fixture := &engagementFixture{rows: map[string]identity.ContactEngagement{}}
	user := &identity.User{ID: "u_1", Email: "owner@example.com"}
	other := &identity.User{ID: "u_2", Email: "stranger@example.com"}
	clock := time.Unix(1700000000, 0).UTC()

	deps := Deps{
		PrincipalAuthenticator: func(r *http.Request) (*identity.Principal, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer account":
				return &identity.Principal{User: user, Scope: identity.ScopeAccount}, nil
			case "Bearer raise-agent":
				return &identity.Principal{User: user, Scope: identity.ScopeAgent, AgentID: "raise@example.com"}, nil
			case "Bearer support-agent":
				return &identity.Principal{User: user, Scope: identity.ScopeAgent, AgentID: "support@example.com"}, nil
			case "Bearer other-account":
				return &identity.Principal{User: other, Scope: identity.ScopeAccount}, nil
			default:
				return nil, errors.New("unauthorized")
			}
		},
		GetAgent: func(_ context.Context, address string) (*identity.AgentIdentity, error) {
			switch identity.NormalizeEmail(address) {
			case "raise@example.com", "support@example.com":
				return &identity.AgentIdentity{ID: identity.NormalizeEmail(address), Email: address, UserID: "u_1"}, nil
			case "foreign@example.com":
				return &identity.AgentIdentity{ID: "foreign@example.com", Email: address, UserID: "u_2"}, nil
			}
			return nil, identity.ErrAgentNotFound
		},
		UpsertEngagement: func(_ context.Context, userID, agentID, address string, stage *string, next **time.Time, metadata map[string]any) (identity.ContactEngagement, bool, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			k := fixture.key(userID, agentID, address)
			e, exists := fixture.rows[k]
			if !exists {
				e = identity.ContactEngagement{
					AgentEmail: identity.NormalizeEmail(agentID),
					Address:    identity.NormalizeMailboxAddress(address),
					ContactID:  "cnt_" + identity.NormalizeMailboxAddress(address),
					Metadata:   map[string]any{}, ContactMetadata: map[string]any{},
					CreatedAt: clock, UpdatedAt: clock,
				}
			}
			if stage != nil {
				e.Stage = *stage
			}
			if next != nil {
				e.NextActionAt = *next
			}
			if metadata != nil {
				e.Metadata = metadata
			}
			fixture.rows[k] = e
			return e, !exists, nil
		},
		GetEngagement: func(_ context.Context, userID, agentID, address string) (identity.ContactEngagement, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			e, ok := fixture.rows[fixture.key(userID, agentID, address)]
			if !ok {
				return identity.ContactEngagement{}, identity.ErrEngagementNotFound
			}
			return e, nil
		},
		ListEngagements: func(_ context.Context, userID, agentID string, f identity.EngagementFilter, limit int, _ time.Time, _ string) ([]identity.ContactEngagement, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			prefix := userID + "\x00" + identity.NormalizeEmail(agentID) + "\x00"
			var out []identity.ContactEngagement
			for k, e := range fixture.rows {
				if !strings.HasPrefix(k, prefix) {
					continue
				}
				if f.Stage != "" && e.Stage != f.Stage {
					continue
				}
				if f.Replied != nil && e.Replied() != *f.Replied {
					continue
				}
				if f.Suppressed != nil && e.Suppressed != *f.Suppressed {
					continue
				}
				if !f.NextActionBefore.IsZero() &&
					(e.NextActionAt == nil || e.NextActionAt.After(f.NextActionBefore)) {
					continue
				}
				out = append(out, e)
			}
			if limit > 0 && len(out) > limit {
				out = out[:limit]
			}
			return out, nil
		},
		DeleteEngagement: func(_ context.Context, userID, agentID, address string) (bool, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			k := fixture.key(userID, agentID, address)
			_, ok := fixture.rows[k]
			delete(fixture.rows, k)
			return ok, nil
		},
		CursorSecret: "engagement-test-secret",
	}
	if mutate != nil {
		mutate(&deps, fixture)
	}
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	return srv
}

const raisePath = "/v1/agents/raise%40example.com/contacts"

func enrollVia(t *testing.T, srv *httptest.Server, token, address string, body map[string]any) map[string]any {
	t.Helper()
	code, resp := sendJSON(t, http.MethodPut, srv.URL+raisePath+"/"+urlEsc(address), token, body)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("enroll %s = %d %v; want 200/201", address, code, resp)
	}
	return resp
}

func urlEsc(address string) string {
	return strings.ReplaceAll(strings.ReplaceAll(address, "+", "%2B"), "@", "%40")
}

// TestAgentCredentialCanDriveItsOwnOutreach is the divergence this surface
// exists for. An agent-scoped credential MUST be able to read and advance its
// own engagements — that is the whole outreach loop — even though the same
// credential is forbidden from account-wide contact identity.
func TestAgentCredentialCanDriveItsOwnOutreach(t *testing.T) {
	srv := newEngagementsServer(t, nil)

	enrollVia(t, srv, "raise-agent", "partner@fund.vc",
		map[string]any{"stage": "touch1", "next_action_at": "2026-07-29T09:00:00Z"})

	code, body := sendJSON(t, http.MethodGet, srv.URL+raisePath, "raise-agent", nil)
	if code != http.StatusOK {
		t.Fatalf("agent list = %d %v; want 200 — the agent must be able to read its own outreach", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	item, _ := items[0].(map[string]any)
	if item["stage"] != "touch1" {
		t.Errorf("stage = %v, want touch1", item["stage"])
	}

	// And the SAME credential must still be barred from account-wide identity.
	code, body = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts", "raise-agent", nil)
	if code != http.StatusForbidden {
		t.Errorf("agent credential on /v1/contacts = %d %v; want 403 — engagements are agent-owned, identity is not",
			code, body)
	}
}

// TestAgentCredentialCannotReachSiblingAgent pins the other half: an agent may
// drive its own outreach and only its own.
func TestAgentCredentialCannotReachSiblingAgent(t *testing.T) {
	srv := newEngagementsServer(t, nil)
	sibling := "/v1/agents/support%40example.com/contacts"

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, sibling, nil},
		{http.MethodPut, sibling + "/partner%40fund.vc", map[string]any{"stage": "hijacked"}},
		{http.MethodDelete, sibling + "/partner%40fund.vc?confirm=DELETE", nil},
	} {
		code, body := sendJSON(t, tc.method, srv.URL+tc.path, "raise-agent", tc.body)
		if code != http.StatusForbidden {
			t.Errorf("%s %s as raise-agent = %d %v; want 403", tc.method, tc.path, code, body)
		}
	}
}

// TestEngagementsRejectForeignAndMissingAgentsIdentically pins that this
// surface cannot enumerate another account's agents.
func TestEngagementsRejectForeignAndMissingAgentsIdentically(t *testing.T) {
	srv := newEngagementsServer(t, nil)
	var first string
	for _, agent := range []string{"missing%40example.com", "foreign%40example.com"} {
		code, body := sendJSON(t, http.MethodGet,
			srv.URL+"/v1/agents/"+agent+"/contacts", "account", nil)
		if code != http.StatusNotFound {
			t.Fatalf("GET %s = %d %v; want 404", agent, code, body)
		}
		msg, _ := body["error"].(map[string]any)["message"].(string)
		if first == "" {
			first = msg
		} else if msg != first {
			t.Errorf("missing and foreign agents give different messages: %q vs %q", first, msg)
		}
	}
}

// TestEnrollReturns201ThenUpdatesWith200 pins PUT's create/update distinction.
func TestEnrollReturns201ThenUpdatesWith200(t *testing.T) {
	srv := newEngagementsServer(t, nil)
	path := raisePath + "/partner%40fund.vc"

	code, body := sendJSON(t, http.MethodPut, srv.URL+path, "account", map[string]any{"stage": "touch1"})
	if code != http.StatusCreated {
		t.Fatalf("first PUT = %d %v; want 201", code, body)
	}
	code, body = sendJSON(t, http.MethodPut, srv.URL+path, "account", map[string]any{"stage": "touch2"})
	if code != http.StatusOK {
		t.Fatalf("second PUT = %d %v; want 200", code, body)
	}
	if body["stage"] != "touch2" {
		t.Errorf("stage = %v, want touch2", body["stage"])
	}
}

// TestEnrollLeavesOmittedFieldsAlone pins the partial-update contract at the
// wire level: advancing the stage after a send must not clear the schedule.
func TestEnrollLeavesOmittedFieldsAlone(t *testing.T) {
	srv := newEngagementsServer(t, nil)
	path := raisePath + "/partner%40fund.vc"

	sendJSON(t, http.MethodPut, srv.URL+path, "account",
		map[string]any{"stage": "touch1", "next_action_at": "2026-07-29T09:00:00Z"})
	code, body := sendJSON(t, http.MethodPut, srv.URL+path, "account", map[string]any{"stage": "touch2"})
	if code != http.StatusOK {
		t.Fatalf("stage-only PUT = %d %v", code, body)
	}
	if body["next_action_at"] == nil {
		t.Error("next_action_at cleared by a stage-only update — every touch would rewind the schedule")
	}
}

// TestEngagementRejectsServerOwnedFields pins that a client cannot fake the
// derived record. Being able to write `replied` or `last_outbound_at` would let
// a caller silently remove someone from their own due list.
func TestEngagementRejectsServerOwnedFields(t *testing.T) {
	srv := newEngagementsServer(t, nil)
	path := raisePath + "/partner%40fund.vc"

	for _, field := range []string{"replied", "last_outbound_at", "outbound_count", "suppressed", "address"} {
		code, body := sendJSON(t, http.MethodPut, srv.URL+path, "account", map[string]any{field: "x"})
		if code != http.StatusUnprocessableEntity {
			t.Errorf("PUT %s = %d %v; want 422 — derived fields are server-owned", field, code, body)
		}
	}
}

// TestEngagementListEmbedsContactIdentity pins that the listing carries the
// contact's name and metadata inline. Without it an agent-scoped caller could
// not compose a message at all, because it is barred from /v1/contacts.
func TestEngagementListEmbedsContactIdentity(t *testing.T) {
	srv := newEngagementsServer(t, func(d *Deps, f *engagementFixture) {
		base := d.UpsertEngagement
		d.UpsertEngagement = func(ctx context.Context, userID, agentID, address string, stage *string, next **time.Time, m map[string]any) (identity.ContactEngagement, bool, error) {
			e, created, err := base(ctx, userID, agentID, address, stage, next, m)
			if err == nil {
				f.mu.Lock()
				e.DisplayName = "A. Partner"
				e.ContactMetadata = map[string]any{"fund": "Example Capital"}
				f.rows[f.key(userID, agentID, address)] = e
				f.mu.Unlock()
			}
			return e, created, err
		}
	})
	enrollVia(t, srv, "account", "partner@fund.vc", map[string]any{"stage": "touch1"})

	code, body := sendJSON(t, http.MethodGet, srv.URL+raisePath, "raise-agent", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, body)
	}
	items, _ := body["items"].([]any)
	item, _ := items[0].(map[string]any)
	contact, ok := item["contact"].(map[string]any)
	if !ok {
		t.Fatalf("engagement has no embedded contact object: %v", item)
	}
	if contact["display_name"] != "A. Partner" {
		t.Errorf("embedded display_name = %v", contact["display_name"])
	}
	meta, _ := contact["metadata"].(map[string]any)
	if meta["fund"] != "Example Capital" {
		t.Errorf("embedded contact metadata = %v", contact["metadata"])
	}
}

// TestEngagementListAppliesOutreachFilters pins the query the feature exists
// for. Each excluded row is excluded for a DIFFERENT reason, so a dropped
// filter surfaces as a specific extra rather than a count mismatch.
func TestEngagementListAppliesOutreachFilters(t *testing.T) {
	srv := newEngagementsServer(t, func(d *Deps, f *engagementFixture) {
		base := d.UpsertEngagement
		d.UpsertEngagement = func(ctx context.Context, userID, agentID, address string, stage *string, next **time.Time, m map[string]any) (identity.ContactEngagement, bool, error) {
			e, created, err := base(ctx, userID, agentID, address, stage, next, m)
			if err == nil && strings.HasPrefix(address, "replied@") {
				f.mu.Lock()
				first := time.Unix(1700000000, 0).UTC()
				later := first.Add(time.Hour)
				e.FirstOutboundAt, e.LastInboundAt = &first, &later
				f.rows[f.key(userID, agentID, address)] = e
				f.mu.Unlock()
			}
			if err == nil && strings.HasPrefix(address, "blocked@") {
				f.mu.Lock()
				e.Suppressed = true
				f.rows[f.key(userID, agentID, address)] = e
				f.mu.Unlock()
			}
			return e, created, err
		}
	})

	due := map[string]any{"stage": "touch1", "next_action_at": "2026-01-01T00:00:00Z"}
	enrollVia(t, srv, "account", "due@fund.vc", due)
	enrollVia(t, srv, "account", "replied@fund.vc", due)
	enrollVia(t, srv, "account", "blocked@fund.vc", due)
	enrollVia(t, srv, "account", "notdue@fund.vc",
		map[string]any{"stage": "touch1", "next_action_at": "2099-01-01T00:00:00Z"})

	q := raisePath + "?replied=false&suppressed=false&next_action_before=2026-07-29T00:00:00Z"
	code, body := sendJSON(t, http.MethodGet, srv.URL+q, "raise-agent", nil)
	if code != http.StatusOK {
		t.Fatalf("outreach query = %d %v", code, body)
	}
	items, _ := body["items"].([]any)
	var addrs []string
	for _, raw := range items {
		it, _ := raw.(map[string]any)
		addrs = append(addrs, fmt.Sprint(it["address"]))
	}
	if len(addrs) != 1 || addrs[0] != "due@fund.vc" {
		t.Errorf("outreach query returned %v, want [due@fund.vc] "+
			"(replied/suppressed/not-yet-due must all be excluded)", addrs)
	}
}

// TestUnenrollTouchesNeitherContactNorConsent pins design §8.6 invariant 5 at
// the wire: un-enrolling is not consent.
func TestUnenrollTouchesNeitherContactNorConsent(t *testing.T) {
	var contactDeletes, suppressionWrites int
	srv := newEngagementsServer(t, func(d *Deps, _ *engagementFixture) {
		d.DeleteContact = func(context.Context, string, string) (bool, error) {
			contactDeletes++
			return true, nil
		}
		d.RemoveAgentSuppression = func(context.Context, string, string, string) (bool, error) {
			suppressionWrites++
			return true, nil
		}
	})
	enrollVia(t, srv, "account", "partner@fund.vc", map[string]any{"stage": "touch1"})

	path := raisePath + "/partner%40fund.vc"
	code, body := sendJSON(t, http.MethodDelete, srv.URL+path, "raise-agent", nil)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("DELETE without confirm = %d %v; want 422", code, body)
	}
	code, body = sendJSON(t, http.MethodDelete, srv.URL+path+"?confirm=DELETE", "raise-agent", nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE = %d %v; want 200", code, body)
	}
	if body["deleted"] != true {
		t.Errorf("delete result = %v", body)
	}
	if contactDeletes != 0 {
		t.Errorf("un-enrolling deleted the contact (%d calls) — the person is account-level", contactDeletes)
	}
	if suppressionWrites != 0 {
		t.Errorf("un-enrolling touched consent (%d calls) — it must never restore sendability", suppressionWrites)
	}
}

// TestGetMissingEngagement pins the dedicated not-found code.
func TestGetMissingEngagement(t *testing.T) {
	srv := newEngagementsServer(t, nil)
	code, body := sendJSON(t, http.MethodGet, srv.URL+raisePath+"/absent%40fund.vc", "raise-agent", nil)
	if code != http.StatusNotFound || errCode(body) != "engagement_not_found" {
		t.Errorf("GET absent = %d %v; want 404 engagement_not_found", code, body)
	}
}
