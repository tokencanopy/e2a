package identity

import "context"

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
	return s.drainAgentChunks(ctx, agentID, userID)
}

// ThreadAnchorBatchQueryForTest exposes the exact production lookup so generic
// planner tests cannot silently drift to a simplified, index-friendly shape.
func ThreadAnchorBatchQueryForTest() string {
	return threadAnchorBatchQuery
}
