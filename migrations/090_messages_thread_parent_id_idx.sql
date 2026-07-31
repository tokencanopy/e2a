-- e2a:no-transaction
-- Adds id as the trailing key so bounded child-detach scans lock rows in index
-- order. A new name upgrades databases that already applied migration 088.
-- The valid 088 index is deliberately retained as a fallback; remove it only
-- in a later release after this replacement is verified valid in production.
--
-- OPS NOTE — invalid-index recovery: if the CONCURRENTLY build is interrupted,
-- the migration runner refuses to record this migration while the same-name
-- index is invalid. The 088 fallback remains valid. Drop only the invalid
-- replacement, then restart migration:
--   DROP INDEX CONCURRENTLY IF EXISTS messages_thread_parent_id_idx;
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_thread_parent_id_idx
    ON messages (thread_parent_id, id)
    WHERE thread_parent_id IS NOT NULL;
