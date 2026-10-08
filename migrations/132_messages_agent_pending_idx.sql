-- 132_messages_agent_pending_idx.sql
-- e2a:no-transaction
--
-- Supports ListAgentsByUser's pending_count: count outbound review holds by
-- agent without scanning that agent's message history or every agent's holds.
-- The existing idx_messages_pending_review leads with approval_expires_at for
-- global expiry sweeps; it cannot directly seek to one agent's holds.
-- Match the current query's all-time predicate (pending_approval was retired
-- in 044); do not add a date or deleted_at filter absent from the query.
--
-- Build concurrently because messages is a hot table. If an interrupted build
-- leaves an invalid index, the migration runner refuses to mark this applied.
-- Recovery: DROP INDEX CONCURRENTLY IF EXISTS idx_messages_agent_pending;
-- then retry the migration.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_agent_pending
    ON messages (agent_id)
    WHERE status = 'pending_review' AND direction = 'outbound';
