-- 114_sending_feedback_provenance.sql
-- Deletion-resistant provider-feedback attribution and detector aggregates.

CREATE TABLE IF NOT EXISTS sending_feedback_correlations (
    correlation_id TEXT PRIMARY KEY CHECK (btrim(correlation_id) <> ''),
    operation_id TEXT NOT NULL CHECK (btrim(operation_id) <> ''),
    submission_attempt INTEGER NOT NULL CHECK (submission_attempt > 0),
    source_account_ref TEXT,
    policy_subject_ref TEXT NOT NULL CHECK (btrim(policy_subject_ref) <> ''),
    purpose TEXT NOT NULL CHECK (purpose IN (
        'customer_message', 'customer_notification', 'critical_operational',
        'violation_operational', 'public_feedback_notification', 'trusted_system'
    )),
    shared_reputation BOOLEAN NOT NULL DEFAULT false,
    tenant_mode TEXT NOT NULL CHECK (tenant_mode IN ('none', 'required')),
    provider_message_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    UNIQUE (operation_id, submission_attempt)
);

CREATE INDEX IF NOT EXISTS sending_feedback_correlations_provider_message_idx
    ON sending_feedback_correlations (provider_message_id)
    WHERE provider_message_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS sending_feedback_correlations_account_retention_idx
    ON sending_feedback_correlations (source_account_ref, expires_at, created_at)
    WHERE source_account_ref IS NOT NULL;

CREATE TABLE IF NOT EXISTS sending_feedback_recipients (
    correlation_id TEXT NOT NULL CHECK (btrim(correlation_id) <> ''),
    recipient_hmac BYTEA NOT NULL,
    hmac_key_version INTEGER NOT NULL CHECK (hmac_key_version > 0),
    detector_bucket TEXT NOT NULL DEFAULT 'none'
        CHECK (detector_bucket IN (
            'none', 'delivered', 'terminal_other', 'hard_bounce', 'complaint'
        )),
    evidence_rank INTEGER NOT NULL DEFAULT 0 CHECK (evidence_rank BETWEEN 0 AND 4),
    provider_occurred_at TIMESTAMPTZ,
    evidence_event_id TEXT,
    bucket_epoch BIGINT CHECK (bucket_epoch IS NULL OR bucket_epoch > 0),
    bucket_day DATE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((bucket_epoch IS NULL) = (bucket_day IS NULL)),
    PRIMARY KEY (correlation_id, recipient_hmac)
);

CREATE TABLE IF NOT EXISTS sending_feedback_events (
    provider_event_id TEXT PRIMARY KEY CHECK (btrim(provider_event_id) <> ''),
    correlation_id TEXT NOT NULL CHECK (btrim(correlation_id) <> ''),
    provider_occurred_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS account_sending_outcomes_daily (
    user_id TEXT NOT NULL,
    outcome_epoch BIGINT NOT NULL CHECK (outcome_epoch > 0),
    day DATE NOT NULL,
    shared_reputation BOOLEAN NOT NULL,
    delivered_count INTEGER NOT NULL DEFAULT 0 CHECK (delivered_count >= 0),
    terminal_other_count INTEGER NOT NULL DEFAULT 0 CHECK (terminal_other_count >= 0),
    hard_bounce_count INTEGER NOT NULL DEFAULT 0 CHECK (hard_bounce_count >= 0),
    complaint_count INTEGER NOT NULL DEFAULT 0 CHECK (complaint_count >= 0),
    PRIMARY KEY (user_id, outcome_epoch, day, shared_reputation)
);

CREATE INDEX IF NOT EXISTS account_sending_outcomes_daily_scan_idx
    ON account_sending_outcomes_daily (user_id, outcome_epoch, day, shared_reputation);

-- Bound the account-owned aggregate FK independently from all hot-table
-- metadata changes.
SET LOCAL lock_timeout = '2s';

-- Keep the FK's SHARE ROW EXCLUSIVE parent/child locks within the bounded tail.
LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE account_sending_outcomes_daily IN SHARE ROW EXCLUSIVE MODE;

DO $$
BEGIN
    ALTER TABLE account_sending_outcomes_daily
        ADD CONSTRAINT account_sending_outcomes_daily_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE NOT VALID;
EXCEPTION WHEN duplicate_object THEN
    NULL;
END
$$;
