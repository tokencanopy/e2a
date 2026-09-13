package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
)

func signupHTTPFixture(t *testing.T) *testSignupCalls {
	t.Helper()
	return &testSignupCalls{}
}

type testSignupCalls struct {
	verified      bool
	approved      bool
	rejected      bool
	currentAPIKey string
}

func sampleSignup() *identity.AgentSignup {
	return &identity.AgentSignup{
		ID: "asu_example", UserID: "u_1", AgentID: "build-bot@agents.example.test",
		HumanEmail: "owner@example.test", DisplayName: "Build Bot", NoteToHuman: "Approve this build agent.",
		Harness: "test-harness", Status: identity.AgentSignupPending,
		CodeExpiresAt: time.Unix(1700100000, 0).UTC(), VerificationSentAt: time.Unix(1700000000, 0).UTC(),
		CreatedAt: time.Unix(1700000000, 0).UTC(), UpdatedAt: time.Unix(1700000000, 0).UTC(),
	}
}

func withSignupDeps(calls *testSignupCalls) func(*Deps) {
	return func(d *Deps) {
		d.PrincipalAuthenticator = func(r *http.Request) (*identity.Principal, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer agent":
				return &identity.Principal{User: &identity.User{ID: "u_1", Email: "owner@example.test"}, Scope: identity.ScopeAgent, AgentID: "build-bot@agents.example.test"}, nil
			case "Bearer good":
				return &identity.Principal{User: &identity.User{ID: "u_1", Email: "owner@example.test"}, Scope: identity.ScopeAccount}, nil
			default:
				return nil, errors.New("unauthorized")
			}
		}
		d.RegisterAgentSignup = func(_ context.Context, human, display, note, harness, currentAPIKey string) (*identity.AgentSignupResult, error) {
			calls.currentAPIKey = currentAPIKey
			s := sampleSignup()
			s.HumanEmail, s.DisplayName, s.NoteToHuman, s.Harness = human, display, note, harness
			return &identity.AgentSignupResult{Signup: s, APIKey: &identity.APIKey{PlaintextKey: "e2a_agt_secret"}, Created: display != "Existing Bot"}, nil
		}
		d.VerifyAgentSignup = func(_ context.Context, agentID, code string, review bool) (*identity.AgentSignup, error) {
			if agentID != "build-bot@agents.example.test" || code != "123456" {
				return nil, identity.ErrAgentSignupCodeInvalid
			}
			calls.verified = true
			s := sampleSignup()
			s.Status, s.ReviewOutbound = identity.AgentSignupVerified, review
			return s, nil
		}
		d.ListPendingAgentSignups = func(_ context.Context, human string, limit int, after time.Time, afterID string) ([]identity.AgentSignup, error) {
			if human != "owner@example.test" {
				return nil, errors.New("wrong owner")
			}
			return []identity.AgentSignup{*sampleSignup()}, nil
		}
		d.ApproveAgentSignup = func(_ context.Context, id, human, userID string, review bool) (*identity.AgentSignup, error) {
			if id != "asu_example" || human != "owner@example.test" || userID != "u_1" {
				return nil, identity.ErrAgentSignupNotFound
			}
			calls.approved = true
			s := sampleSignup()
			s.Status, s.ReviewOutbound = identity.AgentSignupVerified, review
			return s, nil
		}
		d.RejectAgentSignup = func(_ context.Context, id, human string) error {
			if id != "asu_example" || human != "owner@example.test" {
				return identity.ErrAgentSignupNotFound
			}
			calls.rejected = true
			return nil
		}
		d.AgentSignupConsoleURL = "https://console.example.test/agent-signups"
	}
}

func TestCreateAgentSignupIsPublicAndReturnsOneTimeKey(t *testing.T) {
	calls := signupHTTPFixture(t)
	srv := testServer(t, withSignupDeps(calls))
	code, body := postJSON(t, srv.URL+"/v1/agent-signup", "", map[string]any{
		"human_email": "owner@example.test", "display_name": "Build Bot",
		"note_to_human": "Approve this build agent.", "harness": "test-harness",
	})
	if code != 201 || body["api_key"] != "e2a_agt_secret" || body["inbox"] != "build-bot@agents.example.test" || body["status"] != "pending" {
		t.Fatalf("status/body = %d %#v", code, body)
	}
	restrictions := body["restrictions"].(map[string]any)
	if restrictions["sends_per_24h"] != float64(5) || restrictions["can_create_identities"] != false {
		t.Fatalf("restrictions = %#v", restrictions)
	}
}

func TestCreateAgentSignupReplayReturns200(t *testing.T) {
	calls := signupHTTPFixture(t)
	srv := testServer(t, withSignupDeps(calls))
	code, body := postJSON(t, srv.URL+"/v1/agent-signup", "", map[string]any{
		"human_email": "owner@example.test", "display_name": "Existing Bot",
		"current_api_key": "e2a_agt_current",
	})
	if code != 200 || body["api_key"] != "e2a_agt_secret" {
		t.Fatalf("status/body = %d %#v", code, body)
	}
	// The possession capability is passed only to the lifecycle service; it is
	// never treated as HTTP authentication for the public endpoint.
	if calls.currentAPIKey != "e2a_agt_current" {
		t.Fatalf("current key = %q", calls.currentAPIKey)
	}
}

func TestVerifyAgentSignupRequiresBoundAgentKey(t *testing.T) {
	calls := signupHTTPFixture(t)
	srv := testServer(t, withSignupDeps(calls))
	if code, _ := postJSON(t, srv.URL+"/v1/agent-signup/verify", "good", map[string]any{"code": "123456"}); code != 403 {
		t.Fatalf("account key status = %d", code)
	}
	code, body := postJSON(t, srv.URL+"/v1/agent-signup/verify", "agent", map[string]any{"code": "123456", "review_outbound": true})
	if code != 200 || !calls.verified || body["status"] != "verified" || body["review_outbound"] != true {
		t.Fatalf("status/body/calls = %d %#v %#v", code, body, calls)
	}
}

func TestPendingAgentSignupsAreAccountScopedAndActionable(t *testing.T) {
	calls := signupHTTPFixture(t)
	srv := testServer(t, withSignupDeps(calls))
	code, body := getJSON(t, srv.URL+"/v1/agent-signup/pending", "good")
	if code != 200 || len(body["items"].([]any)) != 1 {
		t.Fatalf("list status/body = %d %#v", code, body)
	}
	code, body = postJSON(t, srv.URL+"/v1/agent-signup/asu_example/approve", "good", map[string]any{"review_outbound": true})
	if code != 200 || !calls.approved || body["status"] != "verified" {
		t.Fatalf("approve status/body = %d %#v", code, body)
	}
	code, body = postJSON(t, srv.URL+"/v1/agent-signup/asu_example/reject", "good", map[string]any{})
	if code != 200 || !calls.rejected || body["status"] != "rejected" {
		t.Fatalf("reject status/body = %d %#v", code, body)
	}
}

func TestCreateAgentSignupValidatesPublicInput(t *testing.T) {
	srv := testServer(t, withSignupDeps(signupHTTPFixture(t)))
	for _, body := range []map[string]any{
		{"human_email": "not-an-email", "display_name": "Bot"},
		{"human_email": "owner@example.test", "display_name": ""},
	} {
		if code, _ := postJSON(t, srv.URL+"/v1/agent-signup", "", body); code != 422 {
			t.Fatalf("body %#v status=%d want 422", body, code)
		}
	}
}
