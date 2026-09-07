-- 121_sending_feedback_indexes.sql
--
-- Access paths for the B8 feedback provenance tables. Migration 114 created
-- them with only their primary keys, which left three hot queries on
-- sequential scans over tables that grow one row per send for the lifetime
-- of an account:
--
--   * the startup keyring-coverage gate (recipients by key version),
--   * the hourly retention janitor (correlations/events by expires_at),
--   * the account-deletion retention stamp (events by correlation).
--
-- Index-only additions on tables that no hot path writes more than once per
-- submission; no lock beyond the brief metadata lock each CREATE takes.

SET LOCAL lock_timeout = '2s';

CREATE INDEX IF NOT EXISTS sending_feedback_recipients_key_version_idx
    ON sending_feedback_recipients (hmac_key_version);

CREATE INDEX IF NOT EXISTS sending_feedback_correlations_expiry_idx
    ON sending_feedback_correlations (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS sending_feedback_events_expiry_idx
    ON sending_feedback_events (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS sending_feedback_events_correlation_idx
    ON sending_feedback_events (correlation_id);
