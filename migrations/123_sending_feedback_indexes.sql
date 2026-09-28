-- 123_sending_feedback_indexes.sql
--
-- Access paths for the B8 feedback provenance tables. Migration 114 created
-- sending_feedback_events with only its primary key and no expiry index, and
-- gave sending_feedback_correlations an expiry index led by
-- source_account_ref, which the retention janitor's `WHERE expires_at <= $1`
-- cannot use. Both of the janitor's deletes and the account-deletion
-- retention stamp were therefore sequential scans over tables that grow one
-- row per send.
--
-- Deliberately NOT indexed: sending_feedback_recipients.hmac_key_version.
-- Its only reader is the startup keyring gate, whose predicate is
-- "version NOT IN (held versions)" — a btree cannot serve that, so the index
-- would be write amplification on the hottest of these tables with no
-- reader. That scan is boot-only and bounded in practice by the gate's
-- LIMIT 1 in the failing case.
--
-- Index-only additions. Note these are NON-concurrent CREATE INDEX, which
-- takes SHARE and blocks writes to the table for its duration. These tables
-- are NOT empty at the release that carries this migration: the gate has
-- written a correlation (plus one recipient row per envelope address) for
-- every authorized provider attempt since the sending-policy gate shipped,
-- so a production deployment holds weeks of rows. That is still a small
-- table by index-build standards — one row per send, a few narrow columns —
-- so the build completes in well under a second to a few seconds, and the
-- only writers it blocks are feedback/authorization inserts, which wait
-- rather than fail. lock_timeout bounds the wait to ACQUIRE the lock, so a
-- long-running transaction on these tables fails the migration (and the
-- boot, which retries) instead of queueing every writer behind it. A
-- deployment with a far larger backlog should pre-create these indexes
-- CONCURRENTLY by hand; IF NOT EXISTS then makes this file a no-op.

SET LOCAL lock_timeout = '2s';

CREATE INDEX IF NOT EXISTS sending_feedback_correlations_expiry_idx
    ON sending_feedback_correlations (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS sending_feedback_events_expiry_idx
    ON sending_feedback_events (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS sending_feedback_events_correlation_idx
    ON sending_feedback_events (correlation_id);
