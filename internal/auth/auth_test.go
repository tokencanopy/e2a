package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func setupUserAuth(t *testing.T) (*auth.UserAuth, *identity.Store, string) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	cfg := &config.OAuthConfig{
		GoogleClientID:     "test",
		GoogleClientSecret: "test",
		RedirectURL:        "http://localhost/api/auth/callback",
	}
	ua := auth.NewUserAuth(cfg, store, false)

	// Create a user and session
	user, err := store.CreateOrGetUser(ctx, "josh@test.com", "Josh", "google-sub-123")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	token, err := store.CreateUserSession(ctx, user.ID)
	if err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}
	return ua, store, token
}

func authedRequest(method, url, sessionToken string) *http.Request {
	req := httptest.NewRequest(method, url, nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sessionToken})
	return req
}

func TestHandleAgentActivity_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("GET", "/api/dashboard/agents/agent%40test.example.com/activity", nil)
	w := httptest.NewRecorder()
	ua.HandleAgentActivity(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleAgentActivity_AgentNotFound(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedRequest("GET", "/api/dashboard/agents/agent%40nonexistent.example.com/activity", token)
	w := httptest.NewRecorder()
	ua.HandleAgentActivity(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleAgentActivity_NotOwned(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	// Create agent owned by a different user (not the authenticated user)
	otherUser, _ := store.CreateOrGetUser(ctx, "other@example.com", "Other", "google-other")
	store.ClaimOrCreateDomain(ctx, "unowned.example.com", otherUser.ID)
	store.CreateAgent(ctx, "agent@unowned.example.com", "unowned.example.com", "", "https://example.com/webhook", "", otherUser.ID)

	req := authedRequest("GET", "/api/dashboard/agents/agent%40unowned.example.com/activity", token)
	w := httptest.NewRecorder()
	ua.HandleAgentActivity(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unowned agent", w.Code)
	}
}

func TestHandleAgentActivity_EmptyActivity(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.GetUserSession(ctx, token)
	store.ClaimOrCreateDomain(ctx, "empty-activity.example.com", user.ID)
	store.CreateAgent(ctx, "agent@empty-activity.example.com", "empty-activity.example.com", "", "https://example.com/webhook", "", user.ID)

	req := authedRequest("GET", "/api/dashboard/agents/agent%40empty-activity.example.com/activity", token)
	w := httptest.NewRecorder()
	ua.HandleAgentActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var activity []identity.Message
	json.NewDecoder(w.Body).Decode(&activity)
	if len(activity) != 0 {
		t.Errorf("expected empty activity, got %d", len(activity))
	}
}

func TestHandleAgentActivity_WithMessages(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.GetUserSession(ctx, token)
	store.ClaimOrCreateDomain(ctx, "active.example.com", user.ID)
	agent, _ := store.CreateAgent(ctx, "agent@active.example.com", "active.example.com", "", "https://example.com/webhook", "", user.ID)

	// Create some activity ("" id lets the store generate one).
	store.CreateInboundMessage(ctx, "", agent.ID, "alice@gmail.com", "bot@active.example.com", "", "Hello", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
	store.CreateOutboundMessage(ctx, agent.ID, []string{"alice@gmail.com"}, nil, nil, "Re: Hello", "reply", "smtp", "", "", nil)
	store.CreateInboundMessage(ctx, "", agent.ID, "bob@gmail.com", "bot@active.example.com", "", "Hi", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})

	req := authedRequest("GET", "/api/dashboard/agents/agent%40active.example.com/activity", token)
	w := httptest.NewRecorder()
	ua.HandleAgentActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var activity []identity.Message
	json.NewDecoder(w.Body).Decode(&activity)
	if len(activity) != 3 {
		t.Fatalf("expected 3 activities, got %d", len(activity))
	}

	// Most recent first
	if activity[0].Direction != "inbound" || activity[0].Subject != "Hi" {
		t.Errorf("first entry: direction=%q subject=%q", activity[0].Direction, activity[0].Subject)
	}
	if activity[1].Direction != "outbound" || activity[1].Method != "smtp" {
		t.Errorf("second entry: direction=%q method=%q", activity[1].Direction, activity[1].Method)
	}
	if activity[2].Direction != "inbound" || activity[2].Subject != "Hello" {
		t.Errorf("third entry: direction=%q subject=%q", activity[2].Direction, activity[2].Subject)
	}
}

// --- API key endpoints (Item #3) ---

// authedJSON constructs a POST/PATCH with a JSON body and session cookie.
// Local to auth_test so the existing authedRequest stays body-less.
func authedJSON(method, url, sessionToken, body string) *http.Request {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sessionToken})
	return req
}

// TestHandleListAPIKeys_ReturnsLastUsedAtAndExpiresAt: the dashboard
// API keys table needs both columns to render. Asserts the JSON
// response actually carries them (previously dropped from the SELECT).
func TestHandleListAPIKeys_ReturnsLastUsedAtAndExpiresAt(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "josh@test.com", "Josh", "google-sub-123")
	expiresAt := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	store.CreateAPIKey(ctx, user.ID, "with-exp", &expiresAt)
	store.CreateAPIKey(ctx, user.ID, "no-exp", nil)

	req := authedRequest("GET", "/api/keys", token)
	w := httptest.NewRecorder()
	ua.HandleListAPIKeys(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var keys []identity.APIKey
	if err := json.NewDecoder(w.Body).Decode(&keys); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	// One key must carry ExpiresAt; the other must have it nil.
	var sawExpiry, sawNoExpiry bool
	for _, k := range keys {
		if k.Name == "with-exp" {
			if k.ExpiresAt == nil || !k.ExpiresAt.Equal(expiresAt) {
				t.Errorf("with-exp ExpiresAt = %v, want %v", k.ExpiresAt, expiresAt)
			}
			sawExpiry = true
		}
		if k.Name == "no-exp" {
			if k.ExpiresAt != nil {
				t.Errorf("no-exp ExpiresAt = %v, want nil", k.ExpiresAt)
			}
			sawNoExpiry = true
		}
	}
	if !sawExpiry || !sawNoExpiry {
		t.Errorf("expected both with-exp and no-exp in response; sawExpiry=%v sawNoExpiry=%v", sawExpiry, sawNoExpiry)
	}
}

// TestHandleCreateAPIKey_AcceptsExpiresAt: POST /api/keys with a
// future RFC 3339 timestamp persists it on the row.
func TestHandleCreateAPIKey_AcceptsExpiresAt(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "josh@test.com", "Josh", "google-sub-123")

	expiresAt := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	body := `{"name":"ci","expires_at":"` + expiresAt.Format(time.RFC3339) + `"}`
	req := authedJSON("POST", "/api/keys", token, body)
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created identity.APIKey
	json.NewDecoder(w.Body).Decode(&created)
	if created.ExpiresAt == nil || !created.ExpiresAt.Equal(expiresAt) {
		t.Errorf("created.ExpiresAt = %v, want %v", created.ExpiresAt, expiresAt)
	}

	// Round-trip through the DB to confirm it was actually persisted.
	keys, _ := store.ListAPIKeys(ctx, user.ID, 0, time.Time{}, "")
	if len(keys) != 1 || keys[0].ExpiresAt == nil || !keys[0].ExpiresAt.Equal(expiresAt) {
		t.Errorf("persisted ExpiresAt mismatch: %+v", keys)
	}
}

// TestHandleCreateAPIKey_NameBoundRuneSemantics: the dashboard POST /api/keys
// shares the /v1 createApiKey name cap (identity.MaxAPIKeyNameLen, counted in
// Unicode code points). A 200-code-point CJK name (600 bytes) is accepted —
// byte-counting would reject it — and 201 code points is a 400.
func TestHandleCreateAPIKey_NameBoundRuneSemantics(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	atLimit := strings.Repeat("日", identity.MaxAPIKeyNameLen)
	req := authedJSON("POST", "/api/keys", token, `{"name":"`+atLimit+`"}`)
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("name at %d code points: status = %d, want 201; body=%s", identity.MaxAPIKeyNameLen, w.Code, w.Body.String())
	}

	over := strings.Repeat("日", identity.MaxAPIKeyNameLen+1)
	req = authedJSON("POST", "/api/keys", token, `{"name":"`+over+`"}`)
	w = httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("name over the code-point limit: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "name too long") {
		t.Errorf("expected 'name too long' error, got: %s", w.Body.String())
	}
}

// TestHandleCreateAPIKey_RejectsPastExpiresAt: a past timestamp must
// be a 400 — silently swallowing it would issue a key that's already
// expired, which is worse UX than rejecting at create time.
func TestHandleCreateAPIKey_RejectsPastExpiresAt(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	req := authedJSON("POST", "/api/keys", token, `{"name":"backdated","expires_at":"`+past+`"}`)
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleCreateAPIKey_RejectsMalformedExpiresAt: not-RFC-3339 →
// 400, not "silently fall back to NULL." The handler chooses to fail
// loudly because misformed input is a real client bug worth surfacing.
func TestHandleCreateAPIKey_RejectsMalformedExpiresAt(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedJSON("POST", "/api/keys", token, `{"name":"bad","expires_at":"next-tuesday"}`)
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// --- PATCH /api/auth/me (Item #8) ---

func TestHandleUpdateMe_HappyPath(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	req := authedJSON("PATCH", "/api/auth/me", token, `{"name":"Jamie"}`)
	w := httptest.NewRecorder()
	ua.HandleUpdateMe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got identity.User
	json.NewDecoder(w.Body).Decode(&got)
	if got.Name != "Jamie" {
		t.Errorf("returned Name = %q, want Jamie", got.Name)
	}

	// Round-trip: a follow-up GetUserByID sees the persisted name.
	persisted, _ := store.GetUserByID(ctx, got.ID)
	if persisted.Name != "Jamie" {
		t.Errorf("persisted Name = %q, want Jamie", persisted.Name)
	}
}

func TestHandleUpdateMe_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("PATCH", "/api/auth/me", strings.NewReader(`{"name":"Jamie"}`))
	w := httptest.NewRecorder()
	ua.HandleUpdateMe(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleUpdateMe_ValidationErrors(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	// Each case asserts that the validation gate fires and a 400 is
	// returned BEFORE any DB write. The handler chooses to reject rather
	// than normalize so a caller's exact bytes round-trip cleanly through
	// /me on the next GET.
	cases := []struct {
		name string
		body string
	}{
		{"empty string", `{"name":""}`},
		{"leading whitespace", `{"name":" Jamie"}`},
		{"trailing whitespace", `{"name":"Jamie "}`},
		{"too long", `{"name":"` + strings.Repeat("a", 81) + `"}`},
		{"missing name field", `{}`},
		{"malformed JSON", `{not-json}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authedJSON("PATCH", "/api/auth/me", token, tc.body)
			w := httptest.NewRecorder()
			ua.HandleUpdateMe(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleUpdateMe_MaxBoundary(t *testing.T) {
	// 80 chars must succeed; 81 already covered by ValidationErrors.
	ua, _, token := setupUserAuth(t)

	body := `{"name":"` + strings.Repeat("a", 80) + `"}`
	req := authedJSON("PATCH", "/api/auth/me", token, body)
	w := httptest.NewRecorder()
	ua.HandleUpdateMe(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("80-char name should succeed; got status=%d body=%s", w.Code, w.Body.String())
	}
}

// --- Dashboard stats endpoint ---

// TestHandleDashboardStats_HappyPath: GET /api/dashboard/stats wraps
// store.GetDashboardStats. Asserts the JSON shape (sections for today
// / pending / delivery / window).
func TestHandleDashboardStats_HappyPath(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	// Need to look up the user that setupUserAuth created so we can
	// seed usage rows against the right user_id.
	user, _ := store.GetUserSession(ctx, token)

	// Seed today + yesterday rows.
	_, err := store.GetDashboardStats(ctx, user.ID, 0)
	if err != nil {
		t.Fatalf("pre-seed GetDashboardStats: %v", err)
	}

	req := authedRequest("GET", "/api/dashboard/stats", token)
	w := httptest.NewRecorder()
	ua.HandleDashboardStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var got struct {
		Today struct {
			Inbound          int `json:"inbound"`
			Outbound         int `json:"outbound"`
			InboundDeltaPct  int `json:"inbound_delta_pct"`
			OutboundDeltaPct int `json:"outbound_delta_pct"`
		} `json:"today"`
		Pending struct {
			Count         int `json:"count"`
			OldestSeconds int `json:"oldest_seconds"`
		} `json:"pending"`
		DeliverySuccessPct float64 `json:"delivery_success_pct"`
		SampleWindowDays   int     `json:"sample_window_days"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SampleWindowDays != 7 {
		t.Errorf("sample_window_days = %d, want 7", got.SampleWindowDays)
	}
}

func TestHandleDashboardStats_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("GET", "/api/dashboard/stats", nil)
	w := httptest.NewRecorder()
	ua.HandleDashboardStats(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// TestHandleDashboardStats_WindowQueryParam: the same endpoint powers
// both the dashboard at-a-glance strip (default 7-day window) and the
// Settings 30-day usage card via ?window=30. The handler must
// faithfully forward the query value to the store + echo the
// effective window back as sample_window_days.
func TestHandleDashboardStats_WindowQueryParam(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()
	user, _ := store.GetUserSession(ctx, token)

	// Default: no query → 7-day window.
	req := authedRequest("GET", "/api/dashboard/stats", token)
	w := httptest.NewRecorder()
	ua.HandleDashboardStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("default status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var defaultBody struct {
		SampleWindowDays int `json:"sample_window_days"`
	}
	json.NewDecoder(w.Body).Decode(&defaultBody)
	if defaultBody.SampleWindowDays != 7 {
		t.Errorf("default sample_window_days = %d, want 7", defaultBody.SampleWindowDays)
	}

	// ?window=30 → 30-day window.
	req = authedRequest("GET", "/api/dashboard/stats?window=30", token)
	w = httptest.NewRecorder()
	ua.HandleDashboardStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("window=30 status = %d, want 200", w.Code)
	}
	var thirtyBody struct {
		SampleWindowDays int `json:"sample_window_days"`
		InboundWindow    int `json:"inbound_window"`
		OutboundWindow   int `json:"outbound_window"`
	}
	json.NewDecoder(w.Body).Decode(&thirtyBody)
	if thirtyBody.SampleWindowDays != 30 {
		t.Errorf("window=30 sample_window_days = %d, want 30", thirtyBody.SampleWindowDays)
	}

	// Bad value: handler falls back to default rather than erroring,
	// per the comment on HandleDashboardStats.
	req = authedRequest("GET", "/api/dashboard/stats?window=not-a-number", token)
	w = httptest.NewRecorder()
	ua.HandleDashboardStats(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("garbage window status = %d, want 200 (handler ignores junk)", w.Code)
	}

	// Out-of-range: handler clamps at the store layer (90).
	req = authedRequest("GET", "/api/dashboard/stats?window=9999", token)
	w = httptest.NewRecorder()
	ua.HandleDashboardStats(w, req)
	var clampedBody struct {
		SampleWindowDays int `json:"sample_window_days"`
	}
	json.NewDecoder(w.Body).Decode(&clampedBody)
	if clampedBody.SampleWindowDays != 90 {
		t.Errorf("window=9999 clamped to %d, want 90", clampedBody.SampleWindowDays)
	}

	_ = user
}

type meBody struct {
	identity.User
	OnboardingSurveyPending bool `json:"onboarding_survey_pending"`
}

// rawPool opens a plain connection to the same test database setupUserAuth
// used, for reading columns no store method exposes. Store has no pool
// accessor by design.
func rawPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testutil.TestDBURL())
	if err != nil {
		t.Fatalf("rawPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func decodeMe(t *testing.T, w *httptest.ResponseRecorder) meBody {
	t.Helper()
	var got meBody
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	return got
}

func TestHandleMe_SurveyPendingFollowsFlagAndAnswer(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	// Flag off (the default): never pending.
	w := httptest.NewRecorder()
	ua.HandleMe(w, authedRequest("GET", "/api/auth/me", token))
	if got := decodeMe(t, w); got.OnboardingSurveyPending {
		t.Fatal("pending=true with the flag off")
	}

	ua.SetOnboardingSurveyEnabled(true)
	w = httptest.NewRecorder()
	ua.HandleMe(w, authedRequest("GET", "/api/auth/me", token))
	got := decodeMe(t, w)
	if !got.OnboardingSurveyPending {
		t.Fatal("pending=false for an unanswered user with the flag on")
	}

	if _, err := store.RecordAcquisitionSurvey(ctx, got.ID, "github", nil); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	ua.HandleMe(w, authedRequest("GET", "/api/auth/me", token))
	if got := decodeMe(t, w); got.OnboardingSurveyPending {
		t.Fatal("pending=true after answering")
	}
}

func TestHandleUpdateMe_SurveyHappyPathEveryValue(t *testing.T) {
	// One database, one user per source: standing up a fresh test DB per
	// subtest multiplies the shared-Postgres truncate contention.
	ua, store, _ := setupUserAuth(t)
	pool := rawPool(t)
	ua.SetOnboardingSurveyEnabled(true)
	ctx := context.Background()
	for _, source := range identity.AcquisitionSources {
		t.Run(source, func(t *testing.T) {
			u, err := store.CreateOrGetUser(ctx, source+"@example.test", "S", "sub-"+source)
			if err != nil {
				t.Fatal(err)
			}
			token, err := store.CreateUserSession(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			body := `{"onboarding_survey":{"source":"` + source + `"}}`
			if source == "other" {
				body = `{"onboarding_survey":{"source":"other","detail":"  a newsletter  "}}`
			}
			w := httptest.NewRecorder()
			ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, body))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			got := decodeMe(t, w)
			if got.OnboardingSurveyPending {
				t.Error("pending still true in the PATCH response")
			}
			var stored, detail string
			if err := pool.QueryRow(context.Background(),
				`SELECT acquisition_source, COALESCE(acquisition_detail,'') FROM users WHERE id=$1`, got.ID).Scan(&stored, &detail); err != nil {
				t.Fatal(err)
			}
			if stored != source {
				t.Errorf("stored source = %q, want %q", stored, source)
			}
			if source == "other" && detail != "a newsletter" {
				t.Errorf("detail = %q, want trimmed 'a newsletter'", detail)
			}
		})
	}
}

func TestHandleUpdateMe_SurveyValidation(t *testing.T) {
	ua, _, token := setupUserAuth(t)
	ua.SetOnboardingSurveyEnabled(true)
	long := strings.Repeat("é", 201) // 201 code points, 402 bytes
	cases := []struct {
		name string
		body string
	}{
		{"unknown source", `{"onboarding_survey":{"source":"carrier_pigeon"}}`},
		{"empty source", `{"onboarding_survey":{"source":""}}`},
		{"missing source", `{"onboarding_survey":{"detail":"x"}}`},
		{"detail too long", `{"onboarding_survey":{"source":"other","detail":"` + long + `"}}`},
		{"detail with skipped", `{"onboarding_survey":{"source":"skipped","detail":"why"}}`},
		{"detail with NUL", `{"onboarding_survey":{"source":"other","detail":"a\u0000b"}}`},
		{"detail with newline", `{"onboarding_survey":{"source":"other","detail":"a\nb"}}`},
		{"bad name blocks whole request", `{"name":"","onboarding_survey":{"source":"github"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, tc.body))
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
	// Nothing was written by any rejected request.
	w := httptest.NewRecorder()
	ua.HandleMe(w, authedRequest("GET", "/api/auth/me", token))
	if got := decodeMe(t, w); !got.OnboardingSurveyPending {
		t.Fatal("a rejected request recorded an answer")
	}
	// 200 code points of multibyte text is allowed.
	ok := strings.Repeat("é", 200)
	w = httptest.NewRecorder()
	ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, `{"onboarding_survey":{"source":"other","detail":"`+ok+`"}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("200-char detail: status = %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateMe_SurveyWriteOnceReturns409(t *testing.T) {
	ua, _, token := setupUserAuth(t)
	ua.SetOnboardingSurveyEnabled(true)
	first := httptest.NewRecorder()
	ua.HandleUpdateMe(first, authedJSON("PATCH", "/api/auth/me", token, `{"onboarding_survey":{"source":"github"}}`))
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	ua.HandleUpdateMe(second, authedJSON("PATCH", "/api/auth/me", token, `{"onboarding_survey":{"source":"search"}}`))
	if second.Code != http.StatusConflict {
		t.Fatalf("second: status = %d, want 409; body=%s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "onboarding_survey_already_answered") {
		t.Errorf("body = %s", second.Body.String())
	}
}

func TestHandleUpdateMe_SurveyDisabledReturns404(t *testing.T) {
	ua, _, token := setupUserAuth(t) // flag stays off
	w := httptest.NewRecorder()
	ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, `{"onboarding_survey":{"source":"github"}}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "onboarding_survey_disabled") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestHandleUpdateMe_NameAndSurveyTogether(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ua.SetOnboardingSurveyEnabled(true)
	w := httptest.NewRecorder()
	ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, `{"name":"Jamie","onboarding_survey":{"source":"word_of_mouth"}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	got := decodeMe(t, w)
	if got.Name != "Jamie" || got.OnboardingSurveyPending {
		t.Errorf("got name=%q pending=%v, want Jamie/false", got.Name, got.OnboardingSurveyPending)
	}

	// A second combined body hits the write-once conflict BEFORE the name
	// write, so the 409 leaves the name untouched.
	w = httptest.NewRecorder()
	ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, `{"name":"Renamed","onboarding_survey":{"source":"github"}}`))
	if w.Code != http.StatusConflict {
		t.Fatalf("second: status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	persisted, err := store.GetUserByID(context.Background(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Name != "Jamie" {
		t.Errorf("name after 409 = %q, want Jamie (partial write)", persisted.Name)
	}
}
