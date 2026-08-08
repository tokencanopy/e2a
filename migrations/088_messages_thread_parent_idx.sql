-- e2a:no-transaction
-- Supports clearing surviving child pointers during individual and retention
-- purges without a self-referencing foreign key.
--
-- OPS NOTE — invalid-index recovery: if the CONCURRENTLY build is interrupted,
-- the migration runner refuses to record this migration while the same-name
-- index is invalid. Drop that invalid index, then restart migration:
--   DROP INDEX CONCURRENTLY IF EXISTS messages_thread_parent_idx;
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_thread_parent_idx
    ON messages (thread_parent_id)
    WHERE thread_parent_id IS NOT NULL;
