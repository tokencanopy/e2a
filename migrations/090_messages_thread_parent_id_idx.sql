-- e2a:no-transaction
-- Adds id as the trailing key so bounded child-detach scans lock rows in index
-- order. A new name upgrades databases that already applied migration 088.
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_thread_parent_id_idx
    ON messages (thread_parent_id, id)
    WHERE thread_parent_id IS NOT NULL;
