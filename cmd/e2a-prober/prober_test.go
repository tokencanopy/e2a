package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/selftest"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

func TestSeedProbe_ProvisionsSystemAccountIdempotently(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	const agentEmail = "agent@probe.prober-test"
	const sinkURL = "http://prober:8090/sink"

	res, err := seedProbe(ctx, store, agentEmail, sinkURL)
	if err != nil {
		t.Fatalf("seedProbe: %v", err)
	}
	if res.APIKey == "" {
		t.Error("expected an API key on first seed")
	}
	if res.WebhookSecret == "" {
		t.Error("expected a webhook secret on first seed")
	}

	agent, err := store.GetAgentByID(ctx, agentEmail)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}
	var class string
	if err := pool.QueryRow(ctx, `SELECT account_class FROM users WHERE id = $1`, agent.UserID).Scan(&class); err != nil {
		t.Fatalf("read account_class: %v", err)
	}
	if class != "system" {
		t.Errorf("account_class = %q, want system", class)
	}

	// Re-run: idempotent. No duplicate agent; webhook reused (secret empty).
	res2, err := seedProbe(ctx, store, agentEmail, sinkURL)
	if err != nil {
		t.Fatalf("seedProbe re-run: %v", err)
	}
	if res2.WebhookSecret != "" {
		t.Error("re-seed should reuse the existing webhook (empty secret)")
	}
	if res2.APIKey != "" {
		t.Error("re-seed should not mint a new API key when one exists (empty key)")
	}
	whs, err := store.ListWebhooksByUser(ctx, agent.UserID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListWebhooksByUser: %v", err)
	}
	count := 0
	for _, wh := range whs {
		if wh.URL == sinkURL {
			count++
		}
	}
	if count != 1 {
		t.Errorf("webhooks targeting sink = %d, want 1 (no duplicate on re-seed)", count)
	}
	var agents int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_identities WHERE user_id = $1`, agent.UserID).Scan(&agents); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if agents != 1 {
		t.Errorf("agent count = %d, want 1 (no duplicate on re-seed)", agents)
	}
}

// Regression: the server seeds the configured shared domain as a verified,
// ownerless row on every boot (EnsureSharedDomain). ClaimOrCreateDomain cannot
// claim that row, so seed must adopt it. Before the AdoptSharedDomain fallback,
// seeding the probe on the shared domain failed with "already claimed by another
// account" — which meant the prober gate could never come up against a real
// server (this was never exercised because the prober had never been deployed).
func TestSeedProbe_AdoptsPreSeededSharedDomain(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	const shared = "agents.shared.prober-test"
	const agentEmail = "probe@" + shared
	const sinkURL = "http://prober:8090/sink"

	// Simulate the server's boot-time EnsureSharedDomain (verified, user_id NULL).
	if err := store.EnsureSharedDomain(ctx, shared); err != nil {
		t.Fatalf("EnsureSharedDomain: %v", err)
	}

	res, err := seedProbe(ctx, store, agentEmail, sinkURL)
	if err != nil {
		t.Fatalf("seedProbe against pre-seeded shared domain: %v", err)
	}
	if res.APIKey == "" || res.WebhookSecret == "" {
		t.Error("expected API key + webhook secret on first seed")
	}

	// The probe user now owns the shared-domain row, and it stays verified.
	agent, err := store.GetAgentByID(ctx, agentEmail)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}
	var owner *string
	var verified bool
	if err := pool.QueryRow(ctx, `SELECT user_id, verified FROM domains WHERE domain = $1`, shared).Scan(&owner, &verified); err != nil {
		t.Fatalf("read domain row: %v", err)
	}
	if owner == nil || *owner != agent.UserID {
		t.Errorf("domain owner = %v, want probe user %s", owner, agent.UserID)
	}
	if !verified {
		t.Error("shared domain should remain verified after adopt")
	}

	// Idempotent: re-seeding succeeds (adopt returns the already-owned row).
	if _, err := seedProbe(ctx, store, agentEmail, sinkURL); err != nil {
		t.Fatalf("re-seed against adopted shared domain: %v", err)
	}
}

func TestHandleStatus_ConsecutiveGreen(t *testing.T) {
	p := newProber(config{})
	base := time.Unix(1_700_000_000, 0)
	// oldest → newest: green, red, green, green  → tail consecutive_green = 2
	p.ring = []run{
		{At: base, OK: true},
		{At: base.Add(1 * time.Second), OK: false},
		{At: base.Add(2 * time.Second), OK: true},
		{At: base.Add(3 * time.Second), OK: true},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status?since=0", nil)
	p.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	var out struct {
		ConsecutiveGreen int  `json:"consecutive_green"`
		Healthy          bool `json:"healthy"`
		Runs             []struct {
			OK bool `json:"ok"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ConsecutiveGreen != 2 {
		t.Errorf("consecutive_green = %d, want 2", out.ConsecutiveGreen)
	}
	if !out.Healthy {
		t.Error("healthy = false, want true (last run green)")
	}
	if len(out.Runs) != 4 {
		t.Errorf("runs = %d, want 4", len(out.Runs))
	}

	// since filter excludes earlier runs.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/status?since="+itoa(base.Add(2*time.Second).Unix()-1), nil)
	p.handleStatus(rec2, req2)
	var out2 struct {
		Runs []struct{} `json:"runs"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &out2)
	if len(out2.Runs) != 2 {
		t.Errorf("since-filtered runs = %d, want 2", len(out2.Runs))
	}
}

func TestHandleMetrics(t *testing.T) {
	p := newProber(config{})
	p.recordRun(run{
		At: time.Unix(1_700_000_000, 0),
		OK: true,
		Results: []selftest.Result{
			{Name: "liveness", Status: selftest.StatusPass, DurationMS: 10},
			{Name: "inbound_round_trip", Status: selftest.StatusPass, DurationMS: 200},
		},
	})
	p.recordRun(run{
		At: time.Unix(1_700_000_060, 0),
		OK: false,
		Results: []selftest.Result{
			{Name: "liveness", Status: selftest.StatusPass, DurationMS: 12},
			{Name: "inbound_round_trip", Status: selftest.StatusFail, DurationMS: 250},
		},
	})
	rec := httptest.NewRecorder()
	p.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"e2a_selftest_success 0",
		`e2a_selftest_scenario_success{scenario="liveness"} 1`,
		`e2a_selftest_scenario_success{scenario="inbound_round_trip"} 0`,
		`e2a_selftest_scenario_runs_total{scenario="liveness",outcome="pass"} 2`,
		`e2a_selftest_scenario_runs_total{scenario="inbound_round_trip",outcome="pass"} 1`,
		`e2a_selftest_scenario_runs_total{scenario="inbound_round_trip",outcome="fail"} 1`,
		"e2a_selftest_duration_seconds 0.262",
		// Per-scenario latency is the raw material for the MCP/WS/inbound
		// SLI aggregations in docs/observability.md.
		`e2a_selftest_scenario_duration_seconds{scenario="liveness"} 0.012`,
		`e2a_selftest_scenario_duration_seconds{scenario="inbound_round_trip"} 0.250`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n---\n%s", want, body)
		}
	}
}

func TestConfigRequireMCP(t *testing.T) {
	t.Setenv("E2A_PROBE_REQUIRE_MCP", "true")
	if cfg := configFromEnv(); !cfg.RequireMCP {
		t.Error("E2A_PROBE_REQUIRE_MCP=true not honored")
	}
	t.Setenv("E2A_PROBE_REQUIRE_MCP", "")
	if cfg := configFromEnv(); cfg.RequireMCP {
		t.Error("RequireMCP should default to false")
	}
	// The flag must reach the Probe the scenarios actually run with.
	t.Setenv("E2A_PROBE_REQUIRE_MCP", "1")
	pr := newProber(configFromEnv())
	if !pr.probe().RequireMCP {
		t.Error("RequireMCP not threaded into selftest.Probe")
	}
}

func TestHandleMetrics_NoRuns(t *testing.T) {
	p := newProber(config{})
	rec := httptest.NewRecorder()
	p.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "e2a_selftest_success 0") {
		t.Errorf("expected success 0 with no runs, got:\n%s", rec.Body.String())
	}
}

func TestScenarioCountersRemainMonotonicWhenRunRingEvicts(t *testing.T) {
	p := newProber(config{})
	for i := 0; i < ringCap+3; i++ {
		p.recordRun(run{
			At: time.Unix(int64(i+1), 0),
			OK: true,
			Results: []selftest.Result{
				{Name: "liveness", Status: selftest.StatusPass},
			},
		})
	}
	if len(p.ring) != ringCap {
		t.Fatalf("ring length = %d, want cap %d", len(p.ring), ringCap)
	}
	rec := httptest.NewRecorder()
	p.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := `e2a_selftest_scenario_runs_total{scenario="liveness",outcome="pass"} 53`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("metrics missing monotonic count %q:\n%s", want, rec.Body.String())
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func TestRequireRunConfig(t *testing.T) {
	if err := requireRunConfig(config{}); err == nil {
		t.Error("empty config should be rejected")
	}
	full := config{BaseURL: "http://x", SMTPAddr: "x:25", AgentEmail: "a@x", APIKey: "k", WebhookSecret: "s"}
	if err := requireRunConfig(full); err != nil {
		t.Errorf("complete config rejected: %v", err)
	}
	// Missing one field is still rejected and names it.
	missing := full
	missing.APIKey = ""
	if err := requireRunConfig(missing); err == nil || !strings.Contains(err.Error(), "E2A_PROBE_API_KEY") {
		t.Errorf("missing API key: err = %v, want it named", err)
	}
}

// TestRunOnce_RecordsFailure drives a real runOnce against a server that fails
// every request and confirms the failure flows through to the ring, /status
// (healthy=false), and /metrics (success 0) — the signals the deploy bake-gate
// and monitor consume.
func TestRunOnce_RecordsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newProber(config{
		BaseURL: srv.URL, SMTPAddr: "127.0.0.1:1", AgentEmail: "agent@probe.test",
		APIKey: "k", WebhookSecret: "s", Timeout: 200 * time.Millisecond,
	})
	r := p.runOnce(context.Background())
	if r.OK {
		t.Fatal("runOnce against a 500 server should not be OK")
	}
	if len(p.ring) != 1 {
		t.Fatalf("ring has %d runs, want 1", len(p.ring))
	}

	// /status reflects unhealthy, zero consecutive green.
	rec := httptest.NewRecorder()
	p.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status?since=0", nil))
	var st struct {
		ConsecutiveGreen int  `json:"consecutive_green"`
		Healthy          bool `json:"healthy"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Healthy || st.ConsecutiveGreen != 0 {
		t.Errorf("status: healthy=%v consecutive_green=%d, want false/0", st.Healthy, st.ConsecutiveGreen)
	}

	// /metrics reports the failed run.
	rec2 := httptest.NewRecorder()
	p.handleMetrics(rec2, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec2.Body.String(), "e2a_selftest_success 0") {
		t.Errorf("metrics missing success 0:\n%s", rec2.Body.String())
	}
}

func TestCmdValidate_Failures(t *testing.T) {
	ctx := context.Background()

	// Missing agent email → rejected before any DB work.
	if err := cmdValidate(ctx, config{}); err == nil {
		t.Error("validate without agent email should fail")
	}

	// DB reachable + migrations recorded, but the probe identity is absent.
	// Re-running the production migration path verifies it is idempotent before
	// cmdValidate's migrations-applied check reaches the identity check.
	pool := testutil.TestDB(t)
	if err := identity.RunMigrations(ctx, pool, migrations.FS, identity.ModeAuto); err != nil {
		t.Fatalf("record migrations: %v", err)
	}
	err := cmdValidate(ctx, config{
		DatabaseURL: testutil.TestDBURL(),
		AgentEmail:  "missing@nowhere.test",
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("validate with missing identity: err = %v, want a not-found error", err)
	}
}
