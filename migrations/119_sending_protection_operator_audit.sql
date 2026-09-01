-- 119_sending_protection_operator_audit.sql
-- Persist the operator reason for every runtime-attestation mutation and
-- every first registration of a permanent operator-recipient version.

CREATE TABLE IF NOT EXISTS sending_protection_runtime_attestation_events (
    revision BIGINT PRIMARY KEY CHECK (revision > 0),
    prior_revision BIGINT NOT NULL CHECK (prior_revision >= 0 AND prior_revision < revision),
    prior_attestation_sha256 TEXT NOT NULL CHECK (prior_attestation_sha256 ~ '^[0-9a-f]{64}$'),
    new_attestation_sha256 TEXT NOT NULL CHECK (new_attestation_sha256 ~ '^[0-9a-f]{64}$'),
    actor TEXT NOT NULL CHECK (btrim(actor) <> ''),
    reason TEXT NOT NULL CHECK (btrim(reason) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE sending_operator_recipient_versions
    ADD COLUMN IF NOT EXISTS reason TEXT NOT NULL DEFAULT 'migration'
        CHECK (btrim(reason) <> '');
