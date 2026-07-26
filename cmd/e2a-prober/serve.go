package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/selftest"
	"github.com/tokencanopy/e2a/internal/telemetry"
)

// run is one full battery execution with a timestamp.
type run struct {
	At      time.Time         `json:"at"`
	OK      bool              `json:"ok"`
	Results []selftest.Result `json:"results"`
}

// prober holds the shared sink and a ring of recent runs for /metrics + /status.
type prober struct {
	cfg  config
	sink *selftest.HTTPSink

	mu           sync.Mutex
	ring         []run // most-recent-last, capped
	scenarioRuns map[string]map[selftest.Status]uint64
}

const ringCap = 50

func newProber(cfg config) *prober {
	return &prober{
		cfg:          cfg,
		sink:         selftest.NewHTTPSink(),
		scenarioRuns: make(map[string]map[selftest.Status]uint64),
	}
}

func (p *prober) probe() *selftest.Probe {
	return &selftest.Probe{
		HTTPBaseURL:   p.cfg.BaseURL,
		APIKey:        p.cfg.APIKey,
		AgentEmail:    p.cfg.AgentEmail,
		SMTPAddr:      p.cfg.SMTPAddr,
		WebhookSecret: p.cfg.WebhookSecret,
		MCPBaseURL:    p.cfg.MCPBaseURL,
		RequireMCP:    p.cfg.RequireMCP,
		Sink:          p.sink,
		Timeout:       p.cfg.Timeout,
	}
}

func (p *prober) runOnce(ctx context.Context) run {
	results := selftest.Run(ctx, p.probe(), selftest.All, true /* smokeOnly */)
	r := run{At: time.Now(), OK: selftest.Worst(results) == selftest.StatusPass, Results: results}
	p.recordRun(r)
	return r
}

func (p *prober) recordRun(r run) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ring = append(p.ring, r)
	if len(p.ring) > ringCap {
		p.ring = p.ring[len(p.ring)-ringCap:]
	}
	for _, res := range r.Results {
		if p.scenarioRuns[res.Name] == nil {
			p.scenarioRuns[res.Name] = make(map[selftest.Status]uint64)
		}
		p.scenarioRuns[res.Name][res.Status]++
	}
}

// requireRunConfig validates the env needed to talk to a live instance.
func requireRunConfig(cfg config) error {
	missing := []string{}
	for k, v := range map[string]string{
		"E2A_PROBE_BASE_URL":       cfg.BaseURL,
		"E2A_PROBE_SMTP_ADDR":      cfg.SMTPAddr,
		"E2A_PROBE_AGENT_EMAIL":    cfg.AgentEmail,
		"E2A_PROBE_API_KEY":        cfg.APIKey,
		"E2A_PROBE_WEBHOOK_SECRET": cfg.WebhookSecret,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env: %v", missing)
	}
	return nil
}

// cmdRunOnce runs the battery once and exits non-zero on any failure. It hosts
// the sink on cfg.Listen so the probe webhook (seeded → cfg.SinkURL) can reach it.
func cmdRunOnce(ctx context.Context, cfg config) error {
	if err := requireRunConfig(cfg); err != nil {
		return err
	}
	p := newProber(cfg)
	srv := &http.Server{Addr: cfg.Listen, Handler: p.mux()}
	go srv.ListenAndServe()
	defer srv.Close()

	r := p.runOnce(ctx)
	for _, res := range r.Results {
		fmt.Printf("%-22s %-4s %5dms  %s\n", res.Name, res.Status, res.DurationMS, res.Detail)
	}
	if !r.OK {
		return fmt.Errorf("self-test failed")
	}
	return nil
}

// cmdServe loops the battery on an interval and serves /sink, /healthz,
// /metrics, /status. Runs forever (until the process is signalled).
func cmdServe(ctx context.Context, cfg config) error {
	if err := requireRunConfig(cfg); err != nil {
		return err
	}
	p := newProber(cfg)
	srv := &http.Server{Addr: cfg.Listen, Handler: p.mux()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[prober] serve: %v", err)
		}
	}()
	log.Printf("[prober] serving on %s, probing %s every %s", cfg.Listen, cfg.BaseURL, cfg.Interval)

	tk := time.NewTicker(cfg.Interval)
	defer tk.Stop()
	p.runOnce(ctx) // probe immediately, don't wait a full interval
	for {
		select {
		case <-ctx.Done():
			return srv.Close()
		case <-tk.C:
			r := p.runOnce(ctx)
			if !r.OK {
				log.Printf("[prober] probe FAILED: %s", firstFailure(r.Results))
			}
		}
	}
}

// cmdValidate is a pre-flight: config parses, DB reachable, migrations applied,
// probe identity present. No round-trip.
func cmdValidate(ctx context.Context, cfg config) error {
	if cfg.AgentEmail == "" {
		return fmt.Errorf("E2A_PROBE_AGENT_EMAIL is required")
	}
	pool, err := openPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("db ping: %w", err)
	}
	if err := migrationsApplied(ctx, pool); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := identity.NewStore(pool).GetAgentByID(ctx, cfg.AgentEmail); err != nil {
		return fmt.Errorf("probe agent %s not found (run `e2a-prober seed`): %w", cfg.AgentEmail, err)
	}
	fmt.Println("validate: ok (db reachable, migrations applied, probe identity present)")
	return nil
}

func (p *prober) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/sink", p.sink)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/status", p.handleStatus)
	mux.HandleFunc("/metrics", p.handleMetrics)
	return mux
}

// handleStatus reports recent runs (optionally since a unix timestamp) and the
// count of consecutive green runs at the tail — the deploy bake-gate polls this
// and promotes once consecutive_green ≥ N.
func (p *prober) handleStatus(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
			since = time.Unix(sec, 0)
		}
	}
	p.mu.Lock()
	var runs []run
	for _, rn := range p.ring {
		if rn.At.After(since) {
			runs = append(runs, rn)
		}
	}
	p.mu.Unlock()

	consecutive := 0
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].OK {
			consecutive++
		} else {
			break
		}
	}
	out := map[string]any{
		"since":             since.Unix(),
		"runs":              runs,
		"consecutive_green": consecutive,
		"healthy":           len(runs) > 0 && runs[len(runs)-1].OK,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleMetrics emits Prometheus text for the latest run.
func (p *prober) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	var last *run
	if len(p.ring) > 0 {
		lastRun := p.ring[len(p.ring)-1]
		last = &lastRun
	}
	scenarioRuns := make(map[string]map[selftest.Status]uint64, len(p.scenarioRuns))
	for scenario, counts := range p.scenarioRuns {
		scenarioRuns[scenario] = make(map[selftest.Status]uint64, len(counts))
		for outcome, count := range counts {
			scenarioRuns[scenario][outcome] = count
		}
	}
	p.mu.Unlock()
	build := telemetry.NormalizeBuildLabel(p.cfg.MetricsBuild)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP e2a_selftest_success Whether the last probe run fully passed (1) or not (0).")
	fmt.Fprintln(w, "# TYPE e2a_selftest_success gauge")
	if last == nil {
		fmt.Fprintf(w, "e2a_selftest_success{build=%q} 0\n", build)
		return
	}
	fmt.Fprintf(w, "e2a_selftest_success{build=%q} %d\n", build, b2i(last.OK))
	fmt.Fprintln(w, "# HELP e2a_selftest_scenario_success Per-scenario result of the last run (1 pass, 0 otherwise).")
	fmt.Fprintln(w, "# TYPE e2a_selftest_scenario_success gauge")
	var total time.Duration
	for _, res := range last.Results {
		fmt.Fprintf(w, "e2a_selftest_scenario_success{build=%q,scenario=%q} %d\n", build, res.Name, b2i(res.Status == selftest.StatusPass))
		total += time.Duration(res.DurationMS) * time.Millisecond
	}
	fmt.Fprintln(w, "# HELP e2a_selftest_scenario_runs_total Completed prober scenario runs by outcome.")
	fmt.Fprintln(w, "# TYPE e2a_selftest_scenario_runs_total counter")
	// Iterate in the stable scenario catalog order. Warn/fail/pass are all
	// explicit outcomes; omit zero-value series until they occur.
	for _, sc := range selftest.All {
		for _, outcome := range []selftest.Status{selftest.StatusPass, selftest.StatusWarn, selftest.StatusFail} {
			if count := scenarioRuns[sc.Name][outcome]; count > 0 {
				fmt.Fprintf(w, "e2a_selftest_scenario_runs_total{build=%q,scenario=%q,outcome=%q} %d\n", build, sc.Name, outcome, count)
			}
		}
	}
	// Per-scenario latency: the raw material for the black-box MCP/WS/inbound
	// SLI aggregations (docs/observability.md) — a scenario can pass while its
	// latency degrades, and this is where that shows first.
	fmt.Fprintln(w, "# HELP e2a_selftest_scenario_duration_seconds Per-scenario duration of the last run.")
	fmt.Fprintln(w, "# TYPE e2a_selftest_scenario_duration_seconds gauge")
	for _, res := range last.Results {
		fmt.Fprintf(w, "e2a_selftest_scenario_duration_seconds{build=%q,scenario=%q} %.3f\n", build, res.Name, float64(res.DurationMS)/1000)
	}
	fmt.Fprintln(w, "# HELP e2a_selftest_duration_seconds Total duration of the last probe run.")
	fmt.Fprintln(w, "# TYPE e2a_selftest_duration_seconds gauge")
	fmt.Fprintf(w, "e2a_selftest_duration_seconds{build=%q} %.3f\n", build, total.Seconds())
}

func firstFailure(results []selftest.Result) string {
	for _, r := range results {
		if r.Status != selftest.StatusPass {
			return fmt.Sprintf("%s: %s", r.Name, r.Detail)
		}
	}
	return ""
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
