-- e2a:no-transaction
-- Remove the branch-only thread grouping index. No current production query
-- reads thread_id by this key; retaining it would add write amplification and
-- storage cost to the messages table. IF EXISTS also cleans up databases that
-- briefly ran the unreleased 086 implementation.
DROP INDEX CONCURRENTLY IF EXISTS messages_agent_thread_created_idx;
