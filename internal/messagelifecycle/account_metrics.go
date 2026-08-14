package messagelifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxMetricsAgents bounds the per-agent breakdown. Plans allow agent counts in
// the tens of thousands, and a dashboard cannot render that many rows anyway,
// so the breakdown returns the busiest agents and reports that it truncated
// rather than silently presenting a partial list as the whole account.
const MaxMetricsAgents = 200

// AgentMetricsGroup is one agent's slice of an account aggregate. AgentEmail
// is the agent's address, which is also its primary key.
type AgentMetricsGroup struct {
	AgentEmail string
	Metrics    AgentMetrics
}

// AccountMetrics is the aggregate across every agent an account owns.
type AccountMetrics struct {
	// Totals covers the whole account and is always complete — it is computed
	// across every agent, never from the possibly-truncated Agents slice.
	Totals AgentMetrics

	// Agents is the optional per-agent breakdown, busiest first. Empty when
	// the caller did not ask for it.
	Agents []AgentMetricsGroup

	// AgentsTruncated reports that the account owns more agents with traffic
	// than Agents contains. Totals remain accurate; only the breakdown is cut.
	AgentsTruncated bool

	// Days is the optional per-day breakdown, oldest first. Empty when the
	// caller did not ask for it.
	//
	// It is produced inside the SAME transaction as Totals, not by a separate
	// read. The API promises that bucket counters sum to the window totals, and
	// two independent reads cannot honour that: a write landing between them
	// makes the chart contradict the tiles it sits under, which is precisely
	// the failure a documented sum-to-totals contract must not have.
	Days []DayBucket
}

// CountByReasonCodeForAccount aggregates persisted lifecycle observations
// across every agent owned by userID, on the same cohort-window contract as
// CountByReasonCode (see its doc comment: half-open, anchored on the message's
// own created_at, feedback still settling for ~72h).
//
// Trashed-but-unpurged MESSAGES are included: that mail was still sent, and
// dropping it would let inbox housekeeping restate past delivery rates.
//
// Trashed AGENTS are excluded. That mail was also still sent, so this does
// restate history slightly — the tradeoff is deliberate. A deleted agent is
// gone from every other account-scoped surface (quota counts, list, get
// without an explicit opt-in), so counting it only here was the odd one out;
// and because agent deletion is soft, an account that churns agents keeps
// every tombstone for the whole 30-day retention window. Past a few thousand
// of them the planner abandons idx_messages_agent_created and sequentially
// scans the entire messages/transitions tables: measured on a test account
// with ~7k trashed agents, the same aggregate went 22ms → 1015ms. Scoping to
// live agents keeps the cost proportional to what the account currently owns
// rather than to everything it has ever owned.
//
// When groupByAgent is set, the busiest MaxMetricsAgents agents also come back
// individually. Totals are computed independently of that cap.
func (s *Store) CountByReasonCodeForAccount(ctx context.Context, userID string, start, end time.Time, groupByAgent, bucketByDay bool) (AccountMetrics, error) {
	var metrics AccountMetrics
	if userID == "" {
		return metrics, fmt.Errorf("message lifecycle account metrics: user id required")
	}
	if !end.After(start) {
		return metrics, fmt.Errorf("message lifecycle account metrics: end must be after start")
	}
	if end.Sub(start) > MaxMetricsWindow {
		return metrics, fmt.Errorf("message lifecycle account metrics: window exceeds %s", MaxMetricsWindow)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return metrics, fmt.Errorf("begin message lifecycle account metrics read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	totals, err := accountTotalsTx(ctx, tx, userID, start, end)
	if err != nil {
		return metrics, err
	}
	metrics.Totals = totals

	if bucketByDay {
		days, err := accountDaysTx(ctx, tx, userID, start, end)
		if err != nil {
			return metrics, err
		}
		metrics.Days = days
	}

	if !groupByAgent {
		return metrics, nil
	}

	// Coverage per agent comes first because it supplies both the ordering
	// (busiest agents win the cap) and the coverage numbers themselves.
	coverage, truncated, err := accountAgentCoverageTx(ctx, tx, userID, start, end)
	if err != nil {
		return metrics, err
	}
	metrics.AgentsTruncated = truncated
	if len(coverage) == 0 {
		return metrics, nil
	}

	agentIDs := make([]string, 0, len(coverage))
	for _, group := range coverage {
		agentIDs = append(agentIDs, group.AgentEmail)
	}
	counts, err := accountAgentCountsTx(ctx, tx, agentIDs, start, end)
	if err != nil {
		return metrics, err
	}
	for i := range coverage {
		coverage[i].Metrics.Counts = counts[coverage[i].AgentEmail].Counts
		coverage[i].Metrics.ReconstructedObservations = counts[coverage[i].AgentEmail].ReconstructedObservations
	}
	metrics.Agents = coverage
	return metrics, nil
}

func accountTotalsTx(ctx context.Context, tx pgx.Tx, userID string, start, end time.Time) (AgentMetrics, error) {
	var totals AgentMetrics
	rows, err := tx.Query(ctx, `
		SELECT t.reason_code, t.stage, t.outcome, t.retryable,
		       count(*)::bigint,
		       count(DISTINCT t.message_id)::bigint,
		       count(*) FILTER (WHERE t.reconstructed)::bigint
		FROM messages m
		JOIN agent_identities a ON a.id = m.agent_id
		JOIN message_lifecycle_transitions t ON t.message_id = m.id
		WHERE a.user_id = $1
		  AND a.deleted_at IS NULL
		  AND m.created_at >= $2
		  AND m.created_at < $3
		GROUP BY t.reason_code, t.stage, t.outcome, t.retryable
		ORDER BY t.reason_code
	`, userID, start, end)
	if err != nil {
		return totals, fmt.Errorf("aggregate account lifecycle metrics: %w", err)
	}
	for rows.Next() {
		var count ReasonCodeCount
		var reconstructed int64
		if err := rows.Scan(&count.ReasonCode, &count.Stage, &count.Outcome, &count.Retryable,
			&count.Observations, &count.Messages, &reconstructed); err != nil {
			rows.Close()
			return totals, fmt.Errorf("scan account lifecycle metrics: %w", err)
		}
		totals.ReconstructedObservations += reconstructed
		totals.Counts = append(totals.Counts, count)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return totals, fmt.Errorf("iterate account lifecycle metrics: %w", err)
	}

	err = tx.QueryRow(ctx, `
		SELECT count(*)::bigint,
		       count(*) FILTER (
		           WHERE EXISTS (
		               SELECT 1 FROM message_lifecycle_transitions t
		               WHERE t.message_id = m.id
		           )
		       )::bigint
		FROM messages m
		JOIN agent_identities a ON a.id = m.agent_id
		WHERE a.user_id = $1
		  AND a.deleted_at IS NULL
		  AND m.created_at >= $2
		  AND m.created_at < $3
	`, userID, start, end).Scan(&totals.MessagesInWindow, &totals.MessagesWithLifecycle)
	if err != nil {
		return totals, fmt.Errorf("count account lifecycle coverage: %w", err)
	}
	return totals, nil
}

// accountAgentCoverageTx returns per-agent coverage for agents with traffic in
// the window, busiest first, capped at MaxMetricsAgents. The bool reports
// whether agents were cut.
func accountAgentCoverageTx(ctx context.Context, tx pgx.Tx, userID string, start, end time.Time) ([]AgentMetricsGroup, bool, error) {
	// One extra row answers "was there more" without a second count query.
	rows, err := tx.Query(ctx, `
		SELECT m.agent_id,
		       count(*)::bigint AS in_window,
		       count(*) FILTER (
		           WHERE EXISTS (
		               SELECT 1 FROM message_lifecycle_transitions t
		               WHERE t.message_id = m.id
		           )
		       )::bigint
		FROM messages m
		JOIN agent_identities a ON a.id = m.agent_id
		WHERE a.user_id = $1
		  AND a.deleted_at IS NULL
		  AND m.created_at >= $2
		  AND m.created_at < $3
		GROUP BY m.agent_id
		ORDER BY in_window DESC, m.agent_id ASC
		LIMIT $4
	`, userID, start, end, MaxMetricsAgents+1)
	if err != nil {
		return nil, false, fmt.Errorf("aggregate per-agent lifecycle coverage: %w", err)
	}
	var groups []AgentMetricsGroup
	for rows.Next() {
		var group AgentMetricsGroup
		if err := rows.Scan(&group.AgentEmail, &group.Metrics.MessagesInWindow, &group.Metrics.MessagesWithLifecycle); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan per-agent lifecycle coverage: %w", err)
		}
		groups = append(groups, group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, fmt.Errorf("iterate per-agent lifecycle coverage: %w", err)
	}
	truncated := len(groups) > MaxMetricsAgents
	if truncated {
		groups = groups[:MaxMetricsAgents]
	}
	return groups, truncated, nil
}

// accountAgentCountsTx tallies reason codes for an explicit agent set. Scoping
// by agent ID rather than re-joining on user_id keeps this read aligned with
// exactly the agents the coverage pass already selected.
func accountAgentCountsTx(ctx context.Context, tx pgx.Tx, agentIDs []string, start, end time.Time) (map[string]AgentMetrics, error) {
	rows, err := tx.Query(ctx, `
		SELECT m.agent_id, t.reason_code, t.stage, t.outcome, t.retryable,
		       count(*)::bigint,
		       count(DISTINCT t.message_id)::bigint,
		       count(*) FILTER (WHERE t.reconstructed)::bigint
		FROM messages m
		JOIN message_lifecycle_transitions t ON t.message_id = m.id
		WHERE m.agent_id = ANY($1)
		  AND m.created_at >= $2
		  AND m.created_at < $3
		GROUP BY m.agent_id, t.reason_code, t.stage, t.outcome, t.retryable
		ORDER BY m.agent_id, t.reason_code
	`, agentIDs, start, end)
	if err != nil {
		return nil, fmt.Errorf("aggregate per-agent lifecycle metrics: %w", err)
	}
	defer rows.Close()

	byAgent := make(map[string]AgentMetrics, len(agentIDs))
	for rows.Next() {
		var agentID string
		var count ReasonCodeCount
		var reconstructed int64
		if err := rows.Scan(&agentID, &count.ReasonCode, &count.Stage, &count.Outcome, &count.Retryable,
			&count.Observations, &count.Messages, &reconstructed); err != nil {
			return nil, fmt.Errorf("scan per-agent lifecycle metrics: %w", err)
		}
		entry := byAgent[agentID]
		entry.Counts = append(entry.Counts, count)
		entry.ReconstructedObservations += reconstructed
		byAgent[agentID] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate per-agent lifecycle metrics: %w", err)
	}
	return byAgent, nil
}

// MaxMetricsBuckets bounds a bucketed read. The widest window (92 days) at
// daily granularity is 92 buckets; the cap is the guard against a future
// finer granularity turning one request into an unbounded row count.
const MaxMetricsBuckets = 128

// DayBucket is one calendar day's tally, in UTC.
type DayBucket struct {
	// Day is midnight UTC starting the bucket. Buckets are contiguous and
	// gap-filled by the caller, because a day with no traffic must render as
	// a zero on a chart rather than vanish and distort the line's slope.
	Day    time.Time
	Counts []ReasonCodeCount
}

// accountDaysTx returns per-day reason-code tallies for one account, on the
// same cohort-window contract as its caller: a message lands in the bucket of
// its own creation day, in UTC.
//
// It takes the caller's transaction rather than the pool so the buckets and
// the window totals are read from one snapshot — see AccountMetrics.Days.
//
// UTC rather than a caller timezone is deliberate. A bucket boundary that
// moves with the reader would make two people comparing the same chart see
// different daily numbers, and the endpoint has no timezone input to make
// that choice explicit.
func accountDaysTx(ctx context.Context, tx pgx.Tx, userID string, start, end time.Time) ([]DayBucket, error) {
	rows, err := tx.Query(ctx, `
		SELECT date_trunc('day', m.created_at AT TIME ZONE 'UTC') AS day,
		       t.reason_code, t.stage, t.outcome, t.retryable,
		       count(*)::bigint,
		       count(DISTINCT t.message_id)::bigint
		FROM messages m
		JOIN agent_identities a ON a.id = m.agent_id
		JOIN message_lifecycle_transitions t ON t.message_id = m.id
		WHERE a.user_id = $1
		  AND a.deleted_at IS NULL
		  AND m.created_at >= $2
		  AND m.created_at < $3
		GROUP BY day, t.reason_code, t.stage, t.outcome, t.retryable
		ORDER BY day, t.reason_code
	`, userID, start, end)
	if err != nil {
		return nil, fmt.Errorf("aggregate daily lifecycle metrics: %w", err)
	}
	defer rows.Close()

	byDay := make(map[time.Time][]ReasonCodeCount)
	for rows.Next() {
		var day time.Time
		var count ReasonCodeCount
		if err := rows.Scan(&day, &count.ReasonCode, &count.Stage, &count.Outcome,
			&count.Retryable, &count.Observations, &count.Messages); err != nil {
			return nil, fmt.Errorf("scan daily lifecycle metrics: %w", err)
		}
		key := day.UTC()
		byDay[key] = append(byDay[key], count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily lifecycle metrics: %w", err)
	}

	// Gap-fill every day in the window. A quiet day that simply disappears
	// would let a chart draw a straight line across it, which reads as steady
	// traffic rather than none.
	// The first bucket is the UTC day CONTAINING start, so it may cover only
	// part of that day — the same partial-bucket convention every time series
	// uses, and preferable to silently shifting the window to a day boundary.
	buckets := make([]DayBucket, 0, len(byDay)+1)
	for day := start.UTC().Truncate(24 * time.Hour); day.Before(end.UTC()); day = day.AddDate(0, 0, 1) {
		buckets = append(buckets, DayBucket{Day: day, Counts: byDay[day]})
		if len(buckets) >= MaxMetricsBuckets {
			break
		}
	}
	return buckets, nil
}
