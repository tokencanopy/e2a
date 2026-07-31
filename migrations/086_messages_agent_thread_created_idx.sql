-- e2a:no-transaction
-- Thread grouping and bounded invariant/observability scans on the
-- production-sized messages table.
--
-- OPS NOTE — invalid-index recovery: if the CONCURRENTLY build is interrupted,
-- the migration runner refuses to record this migration while the same-name
-- index is invalid. Drop that invalid index, then restart migration:
--   DROP INDEX CONCURRENTLY IF EXISTS messages_agent_thread_created_idx;
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_agent_thread_created_idx
    ON messages (agent_id, thread_id, created_at, id)
    WHERE thread_id IS NOT NULL;
