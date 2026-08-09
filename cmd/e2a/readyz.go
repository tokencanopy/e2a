package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/migrations"
)

// Readiness is evaluated on a background ticker rather than inside the request,
// and a failing probe only flips readiness once it has been failing for
// readinessFailureGrace.
//
// The request-path version of this check was an availability hazard. It called
// pool.Ping with a 2s deadline, so when the pool was saturated the check queued
// behind real work and timed out — reporting "database unreachable" for a
// database that was merely busy. HAProxy's L7 check then marked the only
// backend DOWN and served its own 503 to everything, including endpoints that
// never touch the database. A burst of six concurrent cascading deletes was
// enough to take a whole slot out of rotation that way; the database never
// actually went away.
//
// Probing off the request path fixes both halves. The handler no longer
// competes for a pooled connection, so it answers instantly under load, and the
// grace window means transient saturation degrades latency instead of removing
// the instance. A genuinely unreachable database still flips readiness within
// the grace window, which is what the check is actually for.
const (
	readinessProbeInterval = 2 * time.Second
	readinessProbeTimeout  = 5 * time.Second
	readinessFailureGrace  = 15 * time.Second
	// poolProbeTimeout bounds the pool-usability check. Short on purpose: it
	// distinguishes "closed/broken" from "busy" and must never wait on capacity.
	poolProbeTimeout = 250 * time.Millisecond
)

type readinessState struct {
	ready  bool
	reason string
}

// readinessMonitor keeps an instance-local readiness verdict fresh in the
// background so /readyz can answer without touching the pool.
type readinessMonitor struct {
	pool     *pgxpool.Pool
	draining *atomic.Bool
	latest   string

	interval time.Duration
	grace    time.Duration
	timeout  time.Duration

	mu     sync.Mutex // guards lastOK
	lastOK time.Time

	// The monitor's own connection, deliberately outside the pool — see probe().
	connMu sync.Mutex
	conn   *pgx.Conn

	// probeFn overrides the real probe in tests.
	probeFn func() (string, error)

	state    atomic.Pointer[readinessState]
	stop     chan struct{}
	stopOnce sync.Once
}

// newReadinessMonitor probes once synchronously, then keeps the verdict fresh
// in the background. The first probe is synchronous so a just-started instance
// never reports a stale "starting" to the load balancer, and so the verdict is
// deterministic for callers (and tests) immediately after construction.
//
// Before that first success the instance is not ready, which preserves the
// guarantee this check exists for: a freshly deployed instance whose migrations
// did not apply never joins the rotation.
func newReadinessMonitor(pool *pgxpool.Pool, draining *atomic.Bool) *readinessMonitor {
	return newReadinessMonitorWithConfig(pool, draining,
		readinessProbeInterval, readinessFailureGrace, readinessProbeTimeout)
}

func newReadinessMonitorWithConfig(pool *pgxpool.Pool, draining *atomic.Bool, interval, grace, timeout time.Duration) *readinessMonitor {
	m := &readinessMonitor{
		pool:     pool,
		draining: draining,
		latest:   latestMigration(),
		interval: interval,
		grace:    grace,
		timeout:  timeout,
		stop:     make(chan struct{}),
	}
	m.state.Store(&readinessState{ready: false, reason: "starting"})
	m.evaluate()
	go m.run()
	return m
}

func (m *readinessMonitor) Stop() {
	m.stopOnce.Do(func() {
		close(m.stop)
		m.closeProbeConn()
	})
}

// evaluate runs one probe and folds it into the published verdict.
func (m *readinessMonitor) evaluate() {
	reason, err := m.probe()
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastOK = now
		m.state.Store(&readinessState{ready: true})
		return
	}
	// Hold the previous verdict while the failure is still inside the grace
	// window: a busy database is not an absent one, and evicting the instance
	// for transient pressure is what turned a slow database into a total
	// outage. Before the first success lastOK is zero, so an instance that has
	// never been ready flips immediately rather than being granted a grace
	// period it has not earned.
	if m.lastOK.IsZero() || now.Sub(m.lastOK) > m.grace {
		m.state.Store(&readinessState{ready: false, reason: reason})
	}
}

func (m *readinessMonitor) run() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.evaluate()
		}
	}
}

// probe returns a not-ready reason and the underlying error, or a nil error
// when the instance is serviceable.
//
// It runs on a connection the monitor owns, NOT one borrowed from the shared
// pool. Probing through the pool would reintroduce the bug in slower motion:
// pool.Ping has to acquire a pooled connection, so a saturated pool starves the
// probe exactly as it starved the old inline check, and a saturation lasting
// longer than the grace window would still evict the instance. A dedicated
// connection makes the probe measure what it claims to measure — whether the
// database is reachable — independently of how busy the pool is.
func (m *readinessMonitor) probe() (string, error) {
	if m.probeFn != nil {
		return m.probeFn()
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()

	// The dedicated connection deliberately cannot observe the pool, so check
	// the pool's own usability separately: an instance whose pool is closed or
	// broken cannot serve, however reachable the database is. A short deadline
	// keeps this from becoming the starvation path it replaces — under
	// saturation the acquire simply times out, and a timeout means BUSY, which
	// is precisely the condition readiness must tolerate. Any other error means
	// the pool itself is unusable.
	acqCtx, acqCancel := context.WithTimeout(ctx, poolProbeTimeout)
	c, err := m.pool.Acquire(acqCtx)
	acqCancel()
	switch {
	case err == nil:
		c.Release()
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// Saturated, not broken. Keep going.
	default:
		return "connection pool unavailable", err
	}

	conn, err := m.probeConn(ctx)
	if err != nil {
		return "database unreachable", err
	}
	if err := conn.Ping(ctx); err != nil {
		// A connection the server has since closed looks identical to an
		// unreachable database on first use. Drop it so the next tick redials
		// rather than reporting a stale socket as an outage forever.
		m.closeProbeConn()
		return "database unreachable", err
	}
	applied, err := latestMigrationApplied(ctx, conn, m.latest)
	if err != nil {
		m.closeProbeConn()
		// The database answered the ping, so this is a failing query rather
		// than an unreachable server — do not mislabel it in the readiness
		// reason or the operator chases the wrong thing.
		return "migration check failed", err
	}
	if !applied {
		return "migrations not applied", errNotApplied
	}
	return "", nil
}

// probeConn lazily dials (and memoizes) the monitor's own connection, built
// from the pool's connection config so it targets the same database with the
// same credentials.
func (m *readinessMonitor) probeConn(ctx context.Context) (*pgx.Conn, error) {
	m.connMu.Lock()
	defer m.connMu.Unlock()
	if m.conn != nil && !m.conn.IsClosed() {
		return m.conn, nil
	}
	conn, err := pgx.ConnectConfig(ctx, m.pool.Config().ConnConfig)
	if err != nil {
		return nil, err
	}
	m.conn = conn
	return conn, nil
}

func (m *readinessMonitor) closeProbeConn() {
	m.connMu.Lock()
	defer m.connMu.Unlock()
	if m.conn != nil {
		_ = m.conn.Close(context.Background())
		m.conn = nil
	}
}

var errNotApplied = fmt.Errorf("latest migration not applied")

// handler reports instance-local readiness from the background verdict. Unlike
// /api/health (shallow liveness — never restart on a DB blip), /readyz signals
// "ready to serve" and is the direct guard against the deploy-but-migration-
// didn't-apply failure mode. It must NOT exercise downstream/round-trip
// dependencies — that is /selftest's job (see docs/design/prober-selftest.md).
//
// draining flips readiness to 503 the moment shutdown begins, ahead of the
// dependency verdict: a terminating instance must leave the LB rotation even
// though its DB is healthy — while /api/health stays green so the orchestrator
// does not hard-kill it mid-drain.
func (m *readinessMonitor) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.draining != nil && m.draining.Load() {
			writeNotReady(w, "draining")
			return
		}
		st := m.state.Load()
		if st == nil || !st.ready {
			reason := "starting"
			if st != nil && st.reason != "" {
				reason = st.reason
			}
			writeNotReady(w, reason)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ready"}`)
	}
}

func writeNotReady(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, `{"status":"not_ready","reason":%q}`, reason)
}

// latestMigrationApplied reports whether the given migration filename is
// recorded in schema_migrations. An empty filename (no embedded migrations)
// trivially counts as applied.
// querier is satisfied by both *pgxpool.Pool and *pgx.Conn, so the migration
// check can run on the monitor's dedicated connection.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func latestMigrationApplied(ctx context.Context, pool querier, latest string) (bool, error) {
	if latest == "" {
		return true, nil
	}
	var applied bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`, latest,
	).Scan(&applied)
	return applied, err
}

// latestMigration returns the highest-sorted embedded migration filename (e.g.
// "037_account_class.sql"), or "" if none are embedded. Numbered filenames sort
// lexically in apply order, matching what RunMigrations records.
func latestMigration() string {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[len(names)-1]
}
