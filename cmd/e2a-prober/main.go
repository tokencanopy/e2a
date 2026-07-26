// Command e2a-prober runs the e2a critical-path self-test (internal/selftest)
// against a live instance, four ways:
//
//	seed      — idempotently provision the synthetic system-class probe account,
//	            agent, API key, and webhook (prints credentials to capture in env).
//	validate  — pre-flight: config parses, DB reachable, migrations applied,
//	            probe identity present. No round-trip. (vault-diagnose style.)
//	run-once  — run the battery once, exit 0 (all pass) / 1 (any fail). For CI.
//	serve     — loop every interval; host /sink, /healthz, /metrics, /status for
//	            the continuous monitor and the deploy bake-gate.
//
// The probe runs under a system-class account, so its traffic is never metered
// (usage.PolicyFor); self-send uses loopback (no egress); inbound is synthetic
// mail to the probe agent (no real recipient, no owner notification).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/migrations"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "seed":
		err = cmdSeed(ctx, configFromEnv())
	case "seed-conformance":
		err = cmdSeedConformance(ctx, configFromEnv())
	case "validate":
		err = cmdValidate(ctx, configFromEnv())
	case "run-once":
		err = cmdRunOnce(ctx, configFromEnv())
	case "serve":
		err = cmdServe(ctx, configFromEnv())
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2a-prober %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `e2a-prober — e2a critical-path self-test runner

usage: e2a-prober <seed|seed-conformance|validate|run-once|serve>

env:
  E2A_DATABASE_URL              Postgres URL (seed, seed-conformance, validate)
  E2A_CONFORMANCE_AGENT_EMAIL   primary agent for the conformance account (seed-conformance)
  E2A_PROBE_BASE_URL        e2a HTTP base, e.g. http://e2a:8080 (run-once, serve)
  E2A_PROBE_SMTP_ADDR       e2a SMTP listener host:port (run-once, serve)
  E2A_PROBE_AGENT_EMAIL     synthetic probe agent address
  E2A_PROBE_API_KEY         probe agent API key (Bearer)
  E2A_PROBE_WEBHOOK_SECRET  probe webhook signing secret (HMAC verify)
  E2A_PROBE_SINK_URL        URL the probe webhook posts to (== this prober's /sink)
  E2A_PROBE_LISTEN          serve/run-once sink bind addr (default :8090)
  E2A_PROBE_INTERVAL        serve probe interval (default 30s)
  E2A_PROBE_TIMEOUT         round-trip await timeout (default 30s)
  E2A_METRICS_BUILD         release/image identifier attached to every metric
`)
}

// openPool opens a pgx pool from E2A_DATABASE_URL (seed/validate only).
func openPool(ctx context.Context, cfg config) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("E2A_DATABASE_URL is required")
	}
	return pgxpool.New(ctx, cfg.DatabaseURL)
}

// migrationsApplied reports nil if every embedded migration is recorded in
// schema_migrations, else an error listing the pending ones — reused by both
// validate and serve's /readyz-style checks via RunMigrations(ModeVerify),
// which never applies anything.
func migrationsApplied(ctx context.Context, pool *pgxpool.Pool) error {
	return identity.RunMigrations(ctx, pool, migrations.FS, identity.ModeVerify)
}
