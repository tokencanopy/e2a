package identity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SetExpiredDeleteBatchForTest overrides the DeleteExpiredMessages batch size for
// the duration of a test and returns a restore func. Lets the external identity_test
// package exercise the multi-batch drain loop cheaply (a few rows, tiny batch) instead
// of seeding >5000 rows. Compiled only under test.
func SetExpiredDeleteBatchForTest(n int64) (restore func()) {
	prev := expiredDeleteBatch
	expiredDeleteBatch = n
	return func() { expiredDeleteBatch = prev }
}

// SetThreadChildDetachBatchForTest shrinks the per-statement child-pointer
// rewrite bound so integration tests can prove a high-fanout parent drains
// across several committed maintenance transactions.
func SetThreadChildDetachBatchForTest(n int64) (restore func()) {
	prev := threadChildDetachBatch
	threadChildDetachBatch = n
	return func() { threadChildDetachBatch = prev }
}

// AgentPurgeCancelChunkRowsForTest is the per-transaction bound on messages
// that carry a durable send job, so a test can assert the bound rather than
// re-declaring the number and drifting from it.
const AgentPurgeCancelChunkRowsForTest = agentPurgeCancelChunkRows

// DrainAgentChunksForTest exposes the chunked permanent-delete drain WITHOUT
// its claim prologue. DeleteAgent owns the threshold decision and the claim, so
// nothing production-side needs this — but the drain's per-chunk guards
// (restored mid-drain, send lease taken mid-drain) and its resume-from-a-
// committed-prefix behaviour are only reachable by interrupting the drain
// itself, which is exactly what the external identity_test package must do to
// prove they work.
func (s *Store) DrainAgentChunksForTest(ctx context.Context, agentID, userID string) (int64, error) {
	var (
		deletedAt *time.Time
		token     *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT deleted_at, purge_token FROM agent_identities
		  WHERE id = $1 AND user_id = $2`, agentID, userID).Scan(&deletedAt, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if deletedAt == nil {
		return 0, ErrNotInTrash
	}
	if token == nil {
		claimed := "pur_" + generateID()
		if _, err := s.pool.Exec(ctx,
			`UPDATE agent_identities SET purge_token = $2
			  WHERE id = $1 AND purge_token IS NULL`, agentID, claimed); err != nil {
			return 0, err
		}
		token = &claimed
	}
	return s.drainAgentChunks(ctx, agentID, userID, *token)
}

// ClaimAgentPurgeForTest installs a durable token without starting the drain,
// allowing concurrency tests to place a writer precisely at the final seal.
func (s *Store) ClaimAgentPurgeForTest(ctx context.Context, agentID, userID string) (string, error) {
	token := "pur_" + generateID()
	err := s.pool.QueryRow(ctx,
		`UPDATE agent_identities
		    SET deleted_at = now(), purge_token = $3
		  WHERE id = $1 AND user_id = $2
		  RETURNING purge_token`, agentID, userID, token).Scan(&token)
	return token, err
}

// SealAgentPurgeForTest exposes one real final sealing transaction.
func (s *Store) SealAgentPurgeForTest(ctx context.Context, agentID, userID, token string) (bool, bool, int64, error) {
	return s.sealAgentPurge(ctx, agentID, userID, token)
}

// DrainAgentWithTokenForTest proves that a stale token cannot attach to a
// later same-owner incarnation at the same address.
func (s *Store) DrainAgentWithTokenForTest(ctx context.Context, agentID, userID, token string) (int64, error) {
	return s.drainAgentChunks(ctx, agentID, userID, token)
}

// ThreadAnchorBatchQueryForTest exposes the exact production lookup so generic
// planner tests cannot silently drift to a simplified, index-friendly shape.
func ThreadAnchorBatchQueryForTest() string {
	return threadAnchorBatchQuery
}
