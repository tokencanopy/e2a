package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// getRaw issues a GET and returns the full response so header assertions are
// possible (getJSON discards headers).
func getRaw(t *testing.T, url, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPollRateLimited(t *testing.T) {
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			if r.Header.Get("Authorization") == "Bearer good" {
				return &identity.User{ID: "u_1"}, nil
			}
			return nil, errors.New("no")
		},
		ListWebhooks: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.Webhook, error) {
			t.Error("ListWebhooks must NOT be reached when poll-limited")
			return nil, nil
		},
		// blocked: 3s retry-after, quota 60, 0 remaining, resets in 12s.
		PollLimit: func(key string) (bool, time.Duration, int, int, int) {
			if key != "u_1" {
				t.Errorf("poll key = %q, want u_1", key)
			}
			return false, 3 * time.Second, 60, 0, 12
		},
	}))
	t.Cleanup(srv.Close)

	resp := getRaw(t, srv.URL+"/v1/webhooks", "good")
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("want 429, got %d", resp.StatusCode)
	}
	for h, want := range map[string]string{
		"Retry-After": "3", "RateLimit-Limit": "60",
		"RateLimit-Remaining": "0", "RateLimit-Reset": "12",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("header %s = %q, want %q", h, got, want)
		}
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if errCode(body) != "rate_limited" {
		t.Fatalf("want rate_limited, got %v", body)
	}
}

func TestPollRateHeadersOnAllowed(t *testing.T) {
	reached := false
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			return &identity.User{ID: "u_1"}, nil
		},
		ListWebhooks: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.Webhook, error) {
			reached = true
			return []identity.Webhook{}, nil
		},
		PollLimit: func(key string) (bool, time.Duration, int, int, int) {
			return true, 0, 60, 59, 60
		},
	}))
	t.Cleanup(srv.Close)

	resp := getRaw(t, srv.URL+"/v1/webhooks", "good")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if !reached {
		t.Error("ListWebhooks should be reached when allowed")
	}
	if got := resp.Header.Get("RateLimit-Remaining"); got != "59" {
		t.Errorf("RateLimit-Remaining = %q, want 59", got)
	}
	if got := resp.Header.Get("RateLimit-Limit"); got != "60" {
		t.Errorf("RateLimit-Limit = %q, want 60", got)
	}
	// Retry-After must NOT be present on a successful response.
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("Retry-After should be absent on 200, got %q", got)
	}
}

// TestNonPollLimitedReadNotThrottled guards the parity fix: reads the legacy
// surface never poll-limited (listAgents/getAgent/domains/events/limits/export)
// must NOT be throttled on /v1 either — even with a PollLimit that always
// blocks, listAgents (absent from pollLimitedOps) is reached and returns 200.
func TestNonPollLimitedReadNotThrottled(t *testing.T) {
	reached := false
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			return &identity.User{ID: "u_1"}, nil
		},
		ListAgents: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.AgentIdentity, error) {
			reached = true
			return []identity.AgentIdentity{}, nil
		},
		// Would block every request IF it were consulted — it must not be.
		PollLimit: func(key string) (bool, time.Duration, int, int, int) {
			t.Error("PollLimit must NOT be consulted for listAgents (not a poll-limited op)")
			return false, time.Second, 60, 0, 60
		},
	}))
	t.Cleanup(srv.Close)

	resp := getRaw(t, srv.URL+"/v1/agents", "good")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200 (listAgents not poll-limited), got %d", resp.StatusCode)
	}
	if !reached {
		t.Error("ListAgents should be reached")
	}
	if resp.Header.Get("RateLimit-Limit") != "" {
		t.Errorf("non-poll-limited read should carry no RateLimit-Limit header, got %q", resp.Header.Get("RateLimit-Limit"))
	}
}

// TestPollRateLimitExemptInternalClass asserts trusted internal traffic bypasses
// the poll limiter: an internal-class principal reaches the handler even though
// PollLimit would block, and the limiter is never consulted (nor its headers set).
func TestPollRateLimitExemptInternalClass(t *testing.T) {
	reached := false
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			return &identity.User{ID: "u_int", AccountClass: "internal"}, nil
		},
		ListWebhooks: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.Webhook, error) {
			reached = true
			return []identity.Webhook{}, nil
		},
		PollLimit: func(key string) (bool, time.Duration, int, int, int) {
			t.Error("PollLimit must NOT be consulted for an exempt (internal) class")
			return false, time.Second, 60, 0, 60
		},
	}))
	t.Cleanup(srv.Close)

	resp := getRaw(t, srv.URL+"/v1/webhooks", "good")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200 (exempt class bypasses poll limit), got %d", resp.StatusCode)
	}
	if !reached {
		t.Error("handler should be reached for an exempt class")
	}
	if resp.Header.Get("RateLimit-Limit") != "" {
		t.Errorf("exempt class should carry no RateLimit-Limit header, got %q", resp.Header.Get("RateLimit-Limit"))
	}
}

// TestRegRateLimitExemptSystemClass asserts a system-class principal (the prober)
// bypasses the per-IP registration limiter even when RegLimit would block.
func TestRegRateLimitExemptSystemClass(t *testing.T) {
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			return &identity.User{ID: "u_sys", AccountClass: "system"}, nil
		},
		CreateAgent: func(ctx context.Context, email, domain, name, webhookURL, agentMode, userID string) (*identity.AgentIdentity, error) {
			return &identity.AgentIdentity{ID: email, Email: email, UserID: userID}, nil
		},
		RegLimit: func(key string) (bool, time.Duration, int, int, int) {
			t.Error("RegLimit must NOT be consulted for an exempt (system) class")
			return false, 30 * time.Second, 200, 0, 3600
		},
	}))
	t.Cleanup(srv.Close)

	// The exemption is proven by RegLimit never being consulted and no 429;
	// the exact non-429 status depends on handler internals we don't assert.
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{"slug": "probe"})
	if code == 429 {
		t.Fatalf("exempt class must not be reg-limited, got 429 %v", body)
	}
}

func TestRegRateLimited(t *testing.T) {
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			return &identity.User{ID: "u_1"}, nil
		},
		CreateAgent: func(ctx context.Context, email, domain, name, webhookURL, agentMode, userID string) (*identity.AgentIdentity, error) {
			t.Error("CreateAgent must NOT be reached when reg-limited")
			return nil, nil
		},
		RegLimit: func(key string) (bool, time.Duration, int, int, int) {
			return false, 30 * time.Second, 200, 0, 3600
		},
	}))
	t.Cleanup(srv.Close)

	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"slug": "bot",
	})
	if code != 429 || errCode(body) != "rate_limited" {
		t.Fatalf("want 429 rate_limited, got %d %v", code, body)
	}
}

// serverWithSendLimit builds a minimal server whose SendLimit always blocks,
// to assert the 429 path on the outbound chokepoint.
func serverWithSendLimit(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			if r.Header.Get("Authorization") == "Bearer good" {
				return &identity.User{ID: "u_1"}, nil
			}
			return nil, errors.New("no")
		},
		GetAgent: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			return &identity.AgentIdentity{ID: "support@acme.com", Email: "support@acme.com", UserID: "u_1", DomainVerified: true}, nil
		},
		SendLimit: func(key string) (bool, time.Duration) { return false, 7 * time.Second },
		DeliverOutbound: func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, req outbound.SendRequest, mt, rt string, ref *identity.Message, ic agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			t.Error("DeliverOutbound must NOT be reached when rate-limited")
			return &agent.OutboundResult{}, nil
		},
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSendRateLimited(t *testing.T) {
	srv := serverWithSendLimit(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages", "good", map[string]any{
		"to": []string{"a@x.com"}, "subject": "Hi", "text": "hello",
	})
	if code != 429 || errCode(body) != "rate_limited" {
		t.Fatalf("want 429 rate_limited, got %d %v", code, body)
	}
	e, _ := body["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if d == nil || d["retry_after_seconds"].(float64) != 7 {
		t.Fatalf("expected retry_after_seconds=7 in details, got %v", body)
	}
}

// TestSendRateLimitSetsRetryAfterHeader pins the fix for the handler-raised
// send 429: the per-agent send limiter is enforced INSIDE the outbound handler
// (returns a StatusError), so — unlike the middleware reg/poll limiters — its
// Retry-After header is stamped via WithRetryAfter → stampRequestID rather than
// by the middleware. serverWithSendLimit blocks with a 7s retry-after.
func TestSendRateLimitSetsRetryAfterHeader(t *testing.T) {
	srv := serverWithSendLimit(t)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/agents/support%40acme.com/messages",
		strings.NewReader(`{"to":["a@x.com"],"subject":"Hi","text":"hello"}`))
	req.Header.Set("Authorization", "Bearer good")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("want 429, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want 7 (handler-raised send 429 must carry the header)", got)
	}
}

// TestClientIPIgnoresXForwardedFor locks in the per-IP-limiter security
// contract: the caller key comes from CF-Connecting-IP (edge-set, not
// client-controllable), never from a forgeable X-Forwarded-For. A
// regression here re-opens the rate-limit bypass where an attacker
// rotates XFF to get unlimited budget on the registration / attachment /
// feedback limiters.
func TestClientIPIgnoresXForwardedFor(t *testing.T) {
	cases := []struct {
		name string
		cf   string
		xff  string
		addr string
		want string
	}{
		{"xff is ignored entirely", "", "1.2.3.4", "10.0.0.1:5555", "10.0.0.1"},
		{"forged xff cannot override cf", "9.9.9.9", "1.2.3.4, 9.9.9.9", "10.0.0.1:5555", "9.9.9.9"},
		{"cf preferred over remoteaddr", "9.9.9.9", "", "10.0.0.1:5555", "9.9.9.9"},
		{"remoteaddr fallback when no cf", "", "", "10.0.0.1:5555", "10.0.0.1"},
		{"bracketed ipv6 remoteaddr", "", "", "[::1]:5555", "::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.addr
			if tc.cf != "" {
				r.Header.Set("CF-Connecting-IP", tc.cf)
			}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSignupRateLimitKeySeparatesInternalProxyRecipients(t *testing.T) {
	makeRequest := func(email string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/agent-signup", strings.NewReader(`{"human_email":"`+email+`"}`))
		r.RemoteAddr = "172.30.0.10:5555"
		return r
	}
	first := signupRateLimitKey(makeRequest("owner@example.test"))
	if same := signupRateLimitKey(makeRequest("OWNER@example.test")); same != first {
		t.Fatalf("normalized recipient keys differ: %q != %q", same, first)
	}
	if other := signupRateLimitKey(makeRequest("other@example.test")); other == first {
		t.Fatalf("different MCP recipients share key %q", first)
	}
	public := makeRequest("owner@example.test")
	public.RemoteAddr = "203.0.113.9:5555"
	if got := signupRateLimitKey(public); got != "source:203.0.113.9" {
		t.Fatalf("public key = %q", got)
	}
}
