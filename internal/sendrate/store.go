// Package sendrate enforces the per-agent fire-time submission budget — the
// durable, Postgres-backed counterpart of the acceptance-time in-memory send
// limit (internal/httpapi checkSendLimit, 60 submissions/min/agent). The
// outbound send worker (internal/outboundsend) reserves one slot immediately
// before each provider submission, so scheduled sends that accumulated as
// River jobs — and multi-replica deployments the in-memory limiter cannot
// coordinate — cannot burst past the advertised per-agent rate at the
// upstream provider.
//
// Storage is one row per agent (migration 094) holding a sliding-window log
// of recent reservation timestamps. Reserve serializes on the row lock
// (SELECT ... FOR UPDATE), so the limit holds across replicas and concurrent
// workers without SKIP LOCKED or multi-statement races. All timestamps come
// from the DB server's clock (clock_timestamp() at statement execution —
// never the app's, and never the transaction-start time, which would stamp
// events early under lock contention) — app-clock skew never enters the
// window math.
//
// Crash semantics: a slot is consumed at Reserve, BEFORE submission. A crash
// between Reserve and Deliver burns one slot without a submission — the
// provider sees FEWER sends than allowed (the fail-safe direction) — and
// River re-drives the job, which reserves again once the window has capacity.
// A crash between Deliver and MarkSent consumes no second slot: the
// provider_accepted_at evidence settles the row as sent on re-drive without
// re-submission. Rows self-prune on every Reserve (entries older than the
// window are dropped), so the limiter adds no janitor or cleanup obligation;
// a row is at most `limit` timestamps.
package sendrate

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Decision is the outcome of one reservation attempt. When Allowed is false,
// RetryAt is the earliest instant the agent's window frees a slot (the oldest
// kept event aging out).
type Decision struct {
	Allowed bool
	RetryAt time.Time
}

// Store reserves fire-time submission slots against agent_send_rate_windows.
// window and limit are constructor-fixed (production wires 1 minute / 60 in
// cmd/e2a/main.go; tests use short windows).
type Store struct {
	pool   *pgxpool.Pool
	window time.Duration
	limit  int
}

func NewStore(pool *pgxpool.Pool, window time.Duration, limit int) *Store {
	return &Store{pool: pool, window: window, limit: limit}
}

// Reserve consumes one slot in agentID's sliding window, or reports when the
// window next frees capacity. The whole read-modify-write runs in one
// transaction guarded by the agent's row lock: ensure-row, lock, prune
// expired entries, then append or defer.
func (s *Store) Reserve(ctx context.Context, agentID string) (Decision, error) {
	if agentID == "" {
		return Decision{}, errors.New("sendrate: empty agent id")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(ctx)

	// First use: ensure the row exists so the FOR UPDATE below always has a
	// row to lock. Concurrent first sends serialize on the PK insert.
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_send_rate_windows (agent_id) VALUES ($1)
		ON CONFLICT (agent_id) DO NOTHING`, agentID); err != nil {
		return Decision{}, err
	}
	// clock_timestamp() is the DB server's clock at statement execution —
	// i.e. approximately row-lock grant time — never the app's: every replica
	// prunes and appends against the same timeline. (Plain now() would be the
	// TRANSACTION-start time: under lock contention a queued txn would stamp
	// its event early, letting it age out of the window early and widening
	// the effective budget by the lock wait.)
	var events []time.Time
	var now time.Time
	if err := tx.QueryRow(ctx, `
		SELECT events, clock_timestamp() FROM agent_send_rate_windows
		 WHERE agent_id = $1 FOR UPDATE`, agentID).Scan(&events, &now); err != nil {
		return Decision{}, err
	}

	// Prune entries at or older than the window edge. Events append
	// chronologically, so kept stays oldest-first. kept must be non-nil:
	// pgx encodes a nil slice as NULL, which the NOT NULL column rejects.
	cutoff := now.Add(-s.window)
	kept := make([]time.Time, 0, len(events)+1)
	for _, at := range events {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}

	if len(kept) >= s.limit {
		// Over budget: write back just the pruned window (keeps the row
		// bounded) and report when the oldest kept event ages out. A
		// non-positive limit (constructor misuse) denies everything and
		// retries a full window out — kept may be empty, so don't index it.
		if _, err := tx.Exec(ctx, `
			UPDATE agent_send_rate_windows SET events = $2, updated_at = clock_timestamp()
			 WHERE agent_id = $1`, agentID, kept); err != nil {
			return Decision{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Decision{}, err
		}
		retryAt := now.Add(s.window)
		if len(kept) > 0 {
			retryAt = kept[0].Add(s.window)
		}
		return Decision{Allowed: false, RetryAt: retryAt}, nil
	}

	kept = append(kept, now)
	if _, err := tx.Exec(ctx, `
		UPDATE agent_send_rate_windows SET events = $2, updated_at = clock_timestamp()
		 WHERE agent_id = $1`, agentID, kept); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	return Decision{Allowed: true}, nil
}

// Window returns the store's sliding window — exposed so callers (the send
// worker's snooze clamp) cannot diverge from the limiter's real window.
func (s *Store) Window() time.Duration { return s.window }
