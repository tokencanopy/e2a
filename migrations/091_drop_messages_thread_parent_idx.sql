-- e2a:no-transaction
-- The composite replacement from migration 090 covers the old prefix lookup.
DROP INDEX CONCURRENTLY IF EXISTS messages_thread_parent_idx;
