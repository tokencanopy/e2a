package messagelifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxMetricsWindow bounds one metrics read. The aggregate drives off
// idx_messages_agent_created and then probes the lifecycle ledger once per
// message in range, so its cost scales with the agent's volume inside the
// window rather than with the size of either table. A quarter is the widest
// span that keeps the worst-case probe count bounded for a high-volume agent;
// wider ranges belong to a pre-aggregated rollup, not to this live read.
const MaxMetricsWindow = 92 * 24 * time.Hour

// ReasonCodeCount is one canonical reason code's tally inside a cohort window.
//
// The two counts answer different questions and routinely differ:
//
//   - Observations counts ledger rows. Retryable codes are appended once per
//     attempt, so a message deferred four times contributes four observations
//     of submission.temporary_failure.
//   - Messages counts distinct messages carrying at least one such row. This
//     is the grain funnel arithmetic needs: counting observations there would
//     let a single retried message push a stage above its own denominator.
type ReasonCodeCount struct {
	ReasonCode   ReasonCode
	Stage        Stage
	Outcome      Outcome
	Retryable    bool
	Observations int64
	Messages     int64
}

// AgentMetrics is the aggregate for one agent over one cohort window.
type AgentMetrics struct {
	// Counts holds one entry per reason code observed in the window, ordered
	// by reason code. Codes with no observations are absent rather than zero.
	Counts []ReasonCodeCount

	// MessagesInWindow and MessagesWithLifecycle expose ledger coverage.
	//
	// This aggregate reads only PERSISTED transitions. The per-message read
	// path (ListForMessage) additionally reconstructs observations from
	// durable message state, so a message written before the ledger shipped —
	// or by a path that never appended — shows a lifecycle there while
	// contributing nothing here. Reporting both counts turns that gap into a
	// number the caller can see instead of a silent undercount that reads as
	// a delivery problem.
	MessagesInWindow      int64
	MessagesWithLifecycle int64

	// ReconstructedObservations counts ledger rows in the window that were
	// derived from durable state rather than observed at the boundary. They
	// are included in Counts; this field makes the inferred share visible.
	ReconstructedObservations int64
}

// CountByReasonCode aggregates persisted lifecycle observations for one
// agent's messages, anchored on the message's own created_at.
//
// The window is anchored on the SEND cohort, not on observation time, and is
// half-open: [start, end). Anchoring on occurred_at would let a bounce that
// lands six hours after midnight pollute the following day's rate and would
// leave every ratio permanently unable to converge, because the numerator and
// denominator would be drawn from different populations. The cost of cohort
// anchoring is that a recent window keeps moving as late feedback arrives —
// callers must treat roughly the last 72 hours as still settling.
//
// Trashed-but-unpurged messages are deliberately included: the mail was still
// accepted and still sent, and excluding it would silently restate history
// whenever a customer cleans up an inbox.
func (s *Store) CountByReasonCode(ctx context.Context, agentID string, start, end time.Time) (AgentMetrics, error) {
	var metrics AgentMetrics
	if agentID == "" {
		return metrics, fmt.Errorf("message lifecycle metrics: agent id required")
	}
	if !end.After(start) {
		return metrics, fmt.Errorf("message lifecycle metrics: end must be after start")
	}
	if end.Sub(start) > MaxMetricsWindow {
		return metrics, fmt.Errorf("message lifecycle metrics: window exceeds %s", MaxMetricsWindow)
	}

	// One repeatable-read snapshot so the coverage counts describe exactly the
	// same population as the tallies. Read across two statements they could
	// straddle a concurrent insert and report more lifecycle-bearing messages
	// than messages.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return metrics, fmt.Errorf("begin message lifecycle metrics read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT t.reason_code, t.stage, t.outcome, t.retryable,
		       count(*)::bigint AS observations,
		       count(DISTINCT t.message_id)::bigint AS messages,
		       count(*) FILTER (WHERE t.reconstructed)::bigint AS reconstructed
		FROM messages m
		JOIN message_lifecycle_transitions t ON t.message_id = m.id
		WHERE m.agent_id = $1
		  AND m.created_at >= $2
		  AND m.created_at < $3
		GROUP BY t.reason_code, t.stage, t.outcome, t.retryable
		ORDER BY t.reason_code
	`, agentID, start, end)
	if err != nil {
		return metrics, fmt.Errorf("aggregate message lifecycle metrics: %w", err)
	}
	for rows.Next() {
		var count ReasonCodeCount
		var reconstructed int64
		if err := rows.Scan(&count.ReasonCode, &count.Stage, &count.Outcome, &count.Retryable,
			&count.Observations, &count.Messages, &reconstructed); err != nil {
			rows.Close()
			return metrics, fmt.Errorf("scan message lifecycle metrics: %w", err)
		}
		metrics.ReconstructedObservations += reconstructed
		metrics.Counts = append(metrics.Counts, count)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return metrics, fmt.Errorf("iterate message lifecycle metrics: %w", err)
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
		WHERE m.agent_id = $1
		  AND m.created_at >= $2
		  AND m.created_at < $3
	`, agentID, start, end).Scan(&metrics.MessagesInWindow, &metrics.MessagesWithLifecycle)
	if err != nil {
		return metrics, fmt.Errorf("count message lifecycle coverage: %w", err)
	}

	return metrics, nil
}
