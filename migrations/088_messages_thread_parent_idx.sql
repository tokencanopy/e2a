-- e2a:no-transaction
-- Supports clearing surviving child pointers during individual and retention
-- purges without a self-referencing foreign key.
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_thread_parent_idx
    ON messages (thread_parent_id)
    WHERE thread_parent_id IS NOT NULL;
