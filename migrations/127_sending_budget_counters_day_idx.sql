-- 127_sending_budget_counters_day_idx.sql
-- e2a:no-transaction
--
-- Access path for the sending-ledger retention janitor, which deletes day
-- counters for closed days (`day <= today - 2 ORDER BY day LIMIT n`). The
-- primary key (scope, scope_id, day) leads with scope, so it cannot serve a
-- predicate on day alone.
--
-- CREATE INDEX CONCURRENTLY + e2a:no-transaction: counter rows are locked
-- and updated on every budgeted send; a plain CREATE INDEX would block them.
--
-- OPS NOTE — invalid-index recovery: an interrupted CONCURRENTLY build leaves
-- an INVALID index that IF NOT EXISTS then skips. To recover:
--     DROP INDEX CONCURRENTLY IF EXISTS sending_budget_counters_day_idx;
-- then re-run this statement.
CREATE INDEX CONCURRENTLY IF NOT EXISTS sending_budget_counters_day_idx
    ON sending_budget_counters (day);
