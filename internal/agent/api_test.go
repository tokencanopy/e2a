package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

func setupAPI(t *testing.T) (*httptest.Server, *identity.Store, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	noopUsage := usage.NewNoopUsageTracker()
	api := agent.NewAPI(store, sender, smtpRelay, nil, noopUsage, "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetIdempotencyStore(idempotency.NewStore(pool))
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, store, pool
}

func TestOIDCRoutesAbsentUnlessWired(t *testing.T) {
	api := agent.NewAPI(nil, nil, nil, nil, usage.NewNoopUsageTracker(), "", "", "", "", false)
	router := mux.NewRouter()
	api.RegisterRoutes(router)

	for _, name := range []string{"oidc-login", "oidc-callback"} {
		if router.Get(name) != nil {
			t.Errorf("disabled OIDC route %s was registered", name)
		}
	}
}

func TestUnsubscribeLimiterHasIndependentProviderFriendlyBudget(t *testing.T) {
	api := agent.NewAPI(nil, nil, nil, nil, usage.NewNoopUsageTracker(), "", "", "", "", false)
	const key = "203.0.113.9"
	for i := 0; i < 120; i++ {
		if ok, _, _, _, _ := api.DownloadLimitAllow(key); !ok {
			t.Fatalf("attachment request %d unexpectedly limited", i+1)
		}
	}
	if ok, _, _, _, _ := api.DownloadLimitAllow(key); ok {
		t.Fatal("attachment limiter should be exhausted")
	}
	ok, _, limit, remaining, _ := api.UnsubscribeLimitAllow(key)
	if !ok || limit != 600 || remaining != 599 {
		t.Fatalf("unsubscribe snapshot = ok=%v limit=%d remaining=%d, want independent 600/minute budget", ok, limit, remaining)
	}
}

func TestOIDCRoutesPresentWhenWired(t *testing.T) {
	api := agent.NewAPI(nil, nil, nil, nil, usage.NewNoopUsageTracker(), "", "", "", "", false)
	api.SetOIDCAuth(&auth.OIDCAuth{})
	router := mux.NewRouter()
	api.RegisterRoutes(router)

	for _, name := range []string{"oidc-login", "oidc-callback"} {
		if router.Get(name) == nil {
			t.Errorf("enabled OIDC route %s was not registered", name)
		}
	}
}

// createTestUser creates a user and API key, returning the bearer token for authenticated requests.
func createTestUser(t *testing.T, store *identity.Store, email string) string {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, email, "Test User", "google-"+email)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	key, err := store.CreateAPIKey(ctx, user.ID, "test-key-"+email, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return key.PlaintextKey
}

// authedPost sends an authenticated POST request with the given API key.
func authedPost(t *testing.T, url, payload, apiKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewBufferString(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealthEndpoint(t *testing.T) {
	server, _, _ := setupAPI(t)
	resp, err := http.Get(server.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func setupAPIWithSMTP(t *testing.T) (*httptest.Server, *identity.Store, *pgxpool.Pool, func() []testutil.SMTPMessage) {
	t.Helper()
	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: smtpAddr.Host, Port: smtpAddr.Port})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	noopUsage := usage.NewNoopUsageTracker()
	api := agent.NewAPI(store, sender, smtpRelay, nil, noopUsage, "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	// Platform mail (public feedback) crosses the authorized provider seam.
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	api.SetProviderSubmitter(outbound.NewProviderSubmitter(smtpRelay, gate), gate)
	api.SetIdempotencyStore(idempotency.NewStore(pool))
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, store, pool, smtpDone
}

// ============================================================
// Feedback endpoint tests
// ============================================================

func TestFeedback_ValidSubmission(t *testing.T) {
	server, _, _ := setupAPI(t)

	payload := `{"email":"user@example.com","category":"bug","message":"Something is broken"}`
	resp, err := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Without GITHUB_FEEDBACK_TOKEN, should still return 200 (graceful fallback)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func TestFeedback_EmptyMessage(t *testing.T) {
	server, _, _ := setupAPI(t)

	payload := `{"email":"user@example.com","category":"bug","message":""}`
	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestFeedback_WhitespaceOnlyMessage(t *testing.T) {
	server, _, _ := setupAPI(t)

	payload := `{"email":"","category":"general","message":"   \n\t  "}`
	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for whitespace-only message", resp.StatusCode)
	}
}

func TestFeedback_InvalidCategory(t *testing.T) {
	server, _, _ := setupAPI(t)

	payload := `{"message":"hello","category":"invalid"}`
	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for invalid category", resp.StatusCode)
	}
}

func TestFeedback_DefaultCategory(t *testing.T) {
	server, _, _ := setupAPI(t)

	// No category provided — should default to "general" and succeed
	payload := `{"message":"just a thought"}`
	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (default category)", resp.StatusCode)
	}
}

func TestFeedback_AllCategories(t *testing.T) {
	server, _, _ := setupAPI(t)

	for _, cat := range []string{"bug", "feature", "general"} {
		t.Run(cat, func(t *testing.T) {
			payload := fmt.Sprintf(`{"message":"test %s","category":"%s"}`, cat, cat)
			resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
			defer resp.Body.Close()

			if resp.StatusCode != 200 {
				t.Errorf("status = %d, want 200 for category %s", resp.StatusCode, cat)
			}
		})
	}
}

func TestFeedback_InvalidJSON(t *testing.T) {
	server, _, _ := setupAPI(t)

	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(`not json`))
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for invalid JSON", resp.StatusCode)
	}
}

func TestFeedback_MessageTooLong(t *testing.T) {
	server, _, _ := setupAPI(t)

	msg := bytes.Repeat([]byte("a"), 5001)
	payload := fmt.Sprintf(`{"message":"%s"}`, string(msg))
	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for message too long", resp.StatusCode)
	}
}

func TestFeedback_RateLimit(t *testing.T) {
	server, _, _ := setupAPI(t)

	// Send 10 requests (the limit)
	for i := 0; i < 10; i++ {
		payload := fmt.Sprintf(`{"message":"feedback %d"}`, i)
		resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
	}

	// 11th request should be rate limited
	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(`{"message":"one too many"}`))
	defer resp.Body.Close()

	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 for rate-limited request", resp.StatusCode)
	}
}

func TestFeedback_OptionalEmail(t *testing.T) {
	server, _, _ := setupAPI(t)

	// No email field at all — should succeed
	payload := `{"message":"anonymous feedback","category":"general"}`
	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 for feedback without email", resp.StatusCode)
	}
}

func TestFeedback_EmailTooLong(t *testing.T) {
	server, _, _ := setupAPI(t)

	longEmail := string(bytes.Repeat([]byte("a"), 255)) + "@example.com"
	payload := fmt.Sprintf(`{"message":"test","email":"%s"}`, longEmail)
	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for email too long", resp.StatusCode)
	}
}

func TestFeedback_OversizedBody(t *testing.T) {
	server, _, _ := setupAPI(t)

	// Send a body larger than 64KB to trigger MaxBytesReader
	hugeMsg := string(bytes.Repeat([]byte("x"), 70*1024))
	payload := fmt.Sprintf(`{"message":"%s"}`, hugeMsg)
	resp, _ := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for oversized body", resp.StatusCode)
	}
}

func TestFeedback_MethodNotAllowed(t *testing.T) {
	server, _, _ := setupAPI(t)

	req, _ := http.NewRequest("GET", server.URL+"/api/feedback", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()

	if resp.StatusCode != 405 {
		t.Errorf("status = %d, want 405 for GET on feedback endpoint", resp.StatusCode)
	}
}

// With the email channel configured (and GitHub off), a submission delivers
// via the platform relay: one message, To+Cc in the envelope, submitter in
// Reply-To and the body.
func TestFeedback_EmailNotification(t *testing.T) {
	server, _, _, smtpDone := setupAPIWithSMTP(t)
	t.Setenv("GITHUB_FEEDBACK_TOKEN", "")
	t.Setenv("FEEDBACK_NOTIFY_TO", "feedback-to@example.com")
	t.Setenv("FEEDBACK_NOTIFY_CC", "feedback-cc@example.com")

	payload := `{"email":"user@example.com","category":"bug","message":"Something is broken"}`
	resp, err := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (email channel delivers with GitHub off)", resp.StatusCode)
	}

	msgs := smtpDone()
	if len(msgs) != 1 {
		t.Fatalf("smtp messages = %d, want 1", len(msgs))
	}
	m := msgs[0]

	if m.From != "noreply@test.e2a.dev" {
		t.Errorf("envelope from = %q, want noreply@test.e2a.dev", m.From)
	}
	// RCPT TO is issued from the token's canonical (sorted) recipient set,
	// so compare as a set: the wire order is the seam's, not the form's.
	gotRcpts := append([]string(nil), m.Recipients...)
	sort.Strings(gotRcpts)
	wantRcpts := []string{"feedback-cc@example.com", "feedback-to@example.com"}
	if strings.Join(gotRcpts, ",") != strings.Join(wantRcpts, ",") {
		t.Errorf("recipients = %v, want %v", m.Recipients, wantRcpts)
	}
	for _, want := range []string{
		"To: feedback-to@example.com",
		"Cc: feedback-cc@example.com",
		"Reply-To: user@example.com",
		"Category: bug",
		"Submitted by: user@example.com",
		"Something is broken",
	} {
		if !strings.Contains(m.Data, want) {
			t.Errorf("message data missing %q", want)
		}
	}

	// Subject is Q-encoded — decode before comparing. (fakesmtp captures
	// Data with bare "\n" line endings, not the on-wire CRLF.)
	headers := m.Data
	if i := strings.Index(headers, "\n\n"); i >= 0 {
		headers = headers[:i]
	}
	headers = strings.ReplaceAll(headers, "\n ", " ") // unfold
	subject := ""
	for _, line := range strings.Split(headers, "\n") {
		if strings.HasPrefix(line, "Subject: ") {
			subject = strings.TrimPrefix(line, "Subject: ")
		}
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(subject)
	if err != nil {
		t.Fatalf("decode subject %q: %v", subject, err)
	}
	if want := "[e2a feedback] [bug] Something is broken"; decoded != want {
		t.Errorf("subject = %q, want %q", decoded, want)
	}
}

// When every configured channel fails (here: email on but the relay is
// unreachable, GitHub off), the submission must surface a 500 so the user
// knows it didn't land — the old log-only fallback only applies when NO
// channel is configured.
func TestFeedback_AllChannelsFail_500(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	deadRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: "127.0.0.1", Port: 1})
	sender := outbound.NewSender(deadRelay, "test.e2a.dev")
	api := agent.NewAPI(store, sender, deadRelay, nil, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	deadGate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	api.SetProviderSubmitter(outbound.NewProviderSubmitter(deadRelay, deadGate), deadGate)
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	t.Setenv("GITHUB_FEEDBACK_TOKEN", "")
	t.Setenv("FEEDBACK_NOTIFY_TO", "feedback-to@example.com")
	t.Setenv("FEEDBACK_NOTIFY_CC", "")

	payload := `{"category":"general","message":"this should not land anywhere"}`
	resp, err := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500 when every configured channel fails", resp.StatusCode)
	}
}
