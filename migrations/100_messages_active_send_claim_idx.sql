-- e2a:no-transaction
--
-- Every purge chunk checks for a fresh provider-call lease. Keep that guard
-- proportional to the active sends, rather than rescanning the shrinking
-- inbox once per committed chunk. Build concurrently so migration startup
-- does not block message writes on an existing installation.
--
-- OPS NOTE — invalid-index recovery: if the concurrent build is interrupted,
-- drop the invalid index before retrying:
--
--   DROP INDEX CONCURRENTLY IF EXISTS messages_active_send_claim_idx;
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_active_send_claim_idx
    ON messages (agent_id, send_claimed_at)
    WHERE delivery_status = 'sending' AND send_claimed_at IS NOT NULL;
