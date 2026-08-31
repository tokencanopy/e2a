// Package selftest defines the e2a critical-path self-test: a small battery of
// scenarios that exercise the real product paths (liveness, authenticated read,
// the inbound SMTP→webhook round-trip with HMAC verification, and a self-send
// loopback) against a running instance.
//
// The same battery is consumed three ways (see docs/design/prober-selftest.md):
//   - the e2e tests (in-process, against testutil.TestServer);
//   - the `e2a selftest` CLI (self-hosters validating an install);
//   - cmd/e2a-prober (continuous prod monitor + deploy bake-gate signal).
//
// Scenarios are data (a []Scenario), so adding a check is a list edit. Each
// scenario carries a SmokeSafe flag: only read-only / round-trip / loopback
// scenarios that cause no real-world side effect (no external email, no owner
// notifications, no metering) may run against production. The prober runs only
// the SmokeSafe subset against live prod; the full set runs in-process.
package selftest

import (
	"context"
	"net/http"
	"time"
)

// Status is the tri-state result of a single scenario, mirroring the IETF
// health-check vocabulary (draft-inadarei-api-health-check).
type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Result is the outcome of one scenario run.
type Result struct {
	Name       string `json:"name"`
	Status     Status `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"` // human-readable; never contains secrets
}

// Scenario is one critical-path check. Run is given the resolved Probe and
// returns a Result; it must not panic on failure — return StatusFail with a
// Detail instead.
type Scenario struct {
	Name      string
	SmokeSafe bool
	Run       func(ctx context.Context, p *Probe) Result
}

// Probe carries everything a scenario needs to talk to a running instance. It
// is transport-only config plus collaborators; it holds no test scaffolding so
// both the shipped prober and the in-process tests construct it directly.
type Probe struct {
	HTTPBaseURL          string        // e.g. http://e2a:8080 — API + /api/health
	APIKey               string        // probe agent's API key (Bearer)
	AgentEmail           string        // the synthetic probe agent address
	SMTPAddr             string        // host:port of the inbound SMTP listener
	WebhookSecret        string        // signing secret of the probe webhook (HMAC verify)
	MCPBaseURL           string        // deployed streamable-HTTP MCP endpoint, e.g. http://mcp-server:3000/mcp; empty ⇒ mcp scenario skips
	RequireMCP           bool          // when true, an empty MCPBaseURL FAILS the mcp scenario instead of skipping — set on stacks where MCP must be probed (prod)
	DelegatedTokenURL    string        // trusted endpoint that vends one short-lived delegated token; empty ⇒ delegated scenario skips
	DelegatedTokenSecret string        // dedicated Bearer credential for DelegatedTokenURL; never logged
	RequireDelegatedAuth bool          // when true, incomplete delegated config FAILS instead of skipping
	Sink                 *HTTPSink     // receives the webhook callback for the round-trip
	HTTP                 *http.Client  // nil → defaultHTTPClient
	Timeout              time.Duration // round-trip await timeout; 0 → defaultRoundTripTimeout
}

func (p *Probe) httpClient() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return defaultHTTPClient
}

// roundTripTimeout is the await bound for the inbound round-trip. It must exceed
// the outbox drain + River delivery latency in production; the prober sets it
// from E2A_PROBE_TIMEOUT.
func (p *Probe) roundTripTimeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return defaultRoundTripTimeout
}

var defaultHTTPClient = &http.Client{Timeout: 10 * time.Second}

// Run executes the given scenarios in order and returns their results. When
// smokeOnly is true, scenarios with SmokeSafe=false are skipped (use this when
// running against production). Run never aborts early — every scenario runs so
// the caller sees the full picture in one pass.
func Run(ctx context.Context, p *Probe, scenarios []Scenario, smokeOnly bool) []Result {
	results := make([]Result, 0, len(scenarios))
	for _, sc := range scenarios {
		if smokeOnly && !sc.SmokeSafe {
			continue
		}
		start := time.Now()
		res := sc.Run(ctx, p)
		res.Name = sc.Name
		if res.DurationMS == 0 {
			res.DurationMS = time.Since(start).Milliseconds()
		}
		results = append(results, res)
	}
	return results
}

// Worst returns the most severe status across results (fail > warn > pass).
// An empty slice is treated as fail — "no checks ran" is not healthy.
func Worst(results []Result) Status {
	if len(results) == 0 {
		return StatusFail
	}
	worst := StatusPass
	for _, r := range results {
		switch r.Status {
		case StatusFail:
			return StatusFail
		case StatusWarn:
			worst = StatusWarn
		}
	}
	return worst
}
