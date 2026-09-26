-- 121_external_sending_access.sql
-- Expand-only schema for external sending access (restricting what a new
-- account may send through the shared platform identity).
--
-- Nothing here changes behaviour on its own: the gate only consults these
-- columns when the runtime policy carries an `external_sending_access` object
-- in shadow or enforce mode, and every existing row starts unapproved,
-- unentitled and without owner-mailbox proof.

-- Owner-mailbox proof. owner_email_verified_at is the proof (NULL = none).
-- It is written only by a trusted login flow (Google OAuth asserting
-- email_verified) — never backfilled from users.email, bootstrap, or
-- provisioning. The exact mailbox that was verified and its source are bound
-- alongside it: the exception is valid only while that address still equals
-- the account's current (normalized) email, so an email change invalidates the
-- proof without any write here.
ALTER TABLE users ADD COLUMN IF NOT EXISTS owner_email_verified_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS owner_email_verified_address TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS owner_email_verified_source TEXT;

DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_owner_email_verified_source_check
        CHECK (owner_email_verified_source IS NULL OR owner_email_verified_source IN ('google_oauth'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- The three proof columns are set together or not at all, and the address is
-- stored in its normalized (trimmed, lower-case) form.
DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_owner_email_verified_complete_check
        CHECK (
            (owner_email_verified_at IS NULL AND owner_email_verified_address IS NULL AND owner_email_verified_source IS NULL)
            OR (owner_email_verified_at IS NOT NULL AND owner_email_verified_address IS NOT NULL AND owner_email_verified_source IS NOT NULL
                AND owner_email_verified_address = lower(btrim(owner_email_verified_address))
                AND owner_email_verified_address <> '')
        );
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- Operator grant. Deliberately separate from `state` (active/paused): an
-- approved account can still be paused, and pause always wins.
ALTER TABLE account_sending_controls
    ADD COLUMN IF NOT EXISTS external_sending_approved BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE account_sending_controls
    ADD COLUMN IF NOT EXISTS external_sending_access_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE account_sending_controls
    ADD COLUMN IF NOT EXISTS external_sending_access_changed_at TIMESTAMPTZ;

DO $$ BEGIN
    ALTER TABLE account_sending_controls ADD CONSTRAINT account_sending_controls_external_access_revision_check
        CHECK (external_sending_access_revision >= 0);
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- A grant can only exist after an operator change, and revision 0 means no
-- operator has ever touched it.
DO $$ BEGIN
    ALTER TABLE account_sending_controls ADD CONSTRAINT account_sending_controls_external_access_changed_check
        CHECK ((external_sending_access_revision = 0) = (external_sending_access_changed_at IS NULL)
               AND (external_sending_access_revision > 0 OR NOT external_sending_approved));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- Billing-issued paid-base entitlement. Provider-neutral: the hosted billing
-- writer derives it from verified subscription state; the OSS gate reads only
-- this boolean. Existing writers of account_limits do not name the column, so
-- they neither set nor clear it.
ALTER TABLE account_limits
    ADD COLUMN IF NOT EXISTS external_sending_entitled BOOLEAN NOT NULL DEFAULT false;

-- Append-only audit of every operator change to the grant. Like
-- account_sending_control_events it carries no foreign key: the audit outlives
-- the account and is reaped only by its own expires_at (the control-audit
-- retention in the runtime policy).
CREATE TABLE IF NOT EXISTS external_sending_access_events (
    id TEXT PRIMARY KEY,
    account_ref TEXT NOT NULL CHECK (btrim(account_ref) <> ''),
    old_approved BOOLEAN NOT NULL,
    new_approved BOOLEAN NOT NULL,
    old_revision BIGINT NOT NULL CHECK (old_revision >= 0),
    new_revision BIGINT NOT NULL CHECK (new_revision = old_revision + 1),
    actor TEXT NOT NULL CHECK (btrim(actor) <> ''),
    reason TEXT NOT NULL CHECK (btrim(reason) <> '' AND char_length(reason) <= 1000),
    request_ref TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (old_approved <> new_approved),
    CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS external_sending_access_events_account_idx
    ON external_sending_access_events (account_ref, created_at);
CREATE INDEX IF NOT EXISTS external_sending_access_events_expires_idx
    ON external_sending_access_events (expires_at);

-- Rows are immutable once written; only retention (DELETE) may remove them.
CREATE OR REPLACE FUNCTION external_sending_access_events_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'external_sending_access_events rows are append-only';
END
$$;

DROP TRIGGER IF EXISTS external_sending_access_events_no_update ON external_sending_access_events;
CREATE TRIGGER external_sending_access_events_no_update
    BEFORE UPDATE ON external_sending_access_events
    FOR EACH ROW EXECUTE FUNCTION external_sending_access_events_immutable();

-- Customer approval requests: a private support queue. The account is bound
-- server-side from the authenticated principal; the request is customer data
-- and is deleted with the account. At most one pending request per account.
CREATE TABLE IF NOT EXISTS external_sending_access_requests (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'approved', 'declined')),
    use_case TEXT NOT NULL CHECK (btrim(use_case) <> '' AND char_length(use_case) <= 2000),
    recipients TEXT NOT NULL CHECK (btrim(recipients) <> '' AND char_length(recipients) <= 1000),
    expected_daily_volume INTEGER NOT NULL CHECK (expected_daily_volume BETWEEN 1 AND 1000000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at TIMESTAMPTZ,
    decided_by TEXT,
    CHECK ((state = 'pending') = (decided_at IS NULL)),
    CHECK ((decided_at IS NULL) = (decided_by IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS external_sending_access_requests_one_pending_idx
    ON external_sending_access_requests (user_id) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS external_sending_access_requests_user_idx
    ON external_sending_access_requests (user_id, created_at DESC);

-- Terminal lifecycle reason for queued mail the gate definitively refuses
-- because the account may not send to those recipients.
INSERT INTO message_lifecycle_reason_codes (code, stage, outcome, retryable)
VALUES ('submission.external_sending_not_enabled', 'submission', 'failed', false)
ON CONFLICT (code) DO NOTHING;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM message_lifecycle_reason_codes
         WHERE code = 'submission.external_sending_not_enabled'
           AND stage = 'submission' AND outcome = 'failed' AND retryable = false
    ) THEN
        RAISE EXCEPTION 'message lifecycle catalog mismatch for code submission.external_sending_not_enabled';
    END IF;
END
$$;
