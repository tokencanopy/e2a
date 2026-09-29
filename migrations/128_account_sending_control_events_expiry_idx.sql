-- 128_account_sending_control_events_expiry_idx.sql
-- e2a:no-transaction
--
-- Access path for the sending-ledger retention janitor, which deletes pause
-- audit rows past their stamped expiry (`expires_at <= $1 ORDER BY
-- expires_at LIMIT n`). Migration 113 gave this table only its primary key.
-- The table is small today (operator and detector transitions only), but the
-- detector will write to it automatically once armed.
--
-- CREATE INDEX CONCURRENTLY + e2a:no-transaction, matching 126/127, so the
-- build never blocks a pause or resume.
--
-- OPS NOTE — invalid-index recovery: an interrupted CONCURRENTLY build leaves
-- an INVALID index that IF NOT EXISTS then skips. To recover:
--     DROP INDEX CONCURRENTLY IF EXISTS account_sending_control_events_expiry_idx;
-- then re-run this statement.
CREATE INDEX CONCURRENTLY IF NOT EXISTS account_sending_control_events_expiry_idx
    ON account_sending_control_events (expires_at, id);
