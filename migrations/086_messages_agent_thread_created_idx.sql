-- e2a:no-transaction
-- Thread grouping and bounded invariant/observability scans on the
-- production-sized messages table.
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_agent_thread_created_idx
    ON messages (agent_id, thread_id, created_at, id)
    WHERE thread_id IS NOT NULL;
