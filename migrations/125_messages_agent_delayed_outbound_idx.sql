-- 125_messages_agent_delayed_outbound_idx.sql
-- e2a:no-transaction
--
-- Supports the deferred-erase recent-external-send lookup
-- (internal/identity/account_erase_defer.go, arm B): an OLD outbound message
-- that was scheduled or held for review can be submitted long after its
-- created_at (a hold's TTL is unbounded), so that arm cannot bound
-- created_at and would otherwise walk every message of the agent through
-- idx_messages_agent_created while the agent row is locked. It is keyed by
-- the fire/approval instant, GREATEST(scheduled_at, reviewed_at), so a lookup
-- for "anything that went out in the last N days" range-scans only those.
-- Scheduled and review-held sends are a small fraction of outbound mail, so
-- the partial index is small; the query repeats its predicate verbatim so the
-- planner can prove the implication.
--
-- CREATE INDEX CONCURRENTLY + e2a:no-transaction for the same reasons as
-- 106/107: messages is the hottest table and a plain CREATE INDEX would block
-- writes for the whole build.
--
-- OPS NOTE — invalid-index recovery: an interrupted CONCURRENTLY build leaves
-- an INVALID index that IF NOT EXISTS then skips. To recover:
--     DROP INDEX CONCURRENTLY IF EXISTS idx_messages_agent_delayed_outbound;
-- then re-run this statement.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_agent_delayed_outbound
    ON messages (agent_id, (GREATEST(scheduled_at, reviewed_at)))
    WHERE direction = 'outbound' AND (scheduled_at IS NOT NULL OR reviewed_at IS NOT NULL);
