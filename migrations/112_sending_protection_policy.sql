-- 112_sending_protection_policy.sql
-- Expand-only sending-protection policy authority. Runtime behavior remains disabled.

CREATE TABLE IF NOT EXISTS sending_protection_runtime_policy (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    generation BIGINT NOT NULL CHECK (generation >= 0),
    schema_version INTEGER NOT NULL CHECK (schema_version > 0),
    policy JSONB NOT NULL,
    policy_sha256 TEXT NOT NULL CHECK (policy_sha256 ~ '^[0-9a-f]{64}$'),
    activated_at TIMESTAMPTZ NOT NULL,
    activated_by TEXT NOT NULL CHECK (btrim(activated_by) <> '')
);

CREATE TABLE IF NOT EXISTS sending_protection_policy_events (
    generation BIGINT PRIMARY KEY CHECK (generation > 0),
    prior_generation BIGINT NOT NULL CHECK (prior_generation >= 0 AND prior_generation < generation),
    prior_policy_sha256 TEXT NOT NULL CHECK (prior_policy_sha256 ~ '^[0-9a-f]{64}$'),
    new_policy_sha256 TEXT NOT NULL CHECK (new_policy_sha256 ~ '^[0-9a-f]{64}$'),
    actor TEXT NOT NULL CHECK (btrim(actor) <> ''),
    reason TEXT NOT NULL CHECK (btrim(reason) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sending_protection_runtime_attestation (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    revision BIGINT NOT NULL CHECK (revision >= 0),
    active_billing_digest TEXT NOT NULL,
    active_billing_contract INTEGER NOT NULL CHECK (active_billing_contract >= 0),
    rollback_billing_digest TEXT NOT NULL,
    rollback_billing_contract INTEGER NOT NULL CHECK (rollback_billing_contract >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT NOT NULL CHECK (btrim(updated_by) <> '')
);

CREATE TABLE IF NOT EXISTS sending_operator_recipient_versions (
    logical_version INTEGER PRIMARY KEY CHECK (logical_version > 0),
    commitment_key_id TEXT NOT NULL CHECK (commitment_key_id ~ '^[0-9a-f]{64}$'),
    recipient_commitment TEXT NOT NULL CHECK (recipient_commitment ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL CHECK (btrim(created_by) <> '')
);

CREATE OR REPLACE FUNCTION reject_sending_operator_recipient_version_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'sending operator recipient versions are append-only';
END
$$;

DROP TRIGGER IF EXISTS sending_operator_recipient_versions_append_only ON sending_operator_recipient_versions;
CREATE TRIGGER sending_operator_recipient_versions_append_only
BEFORE UPDATE OR DELETE ON sending_operator_recipient_versions
FOR EACH ROW EXECUTE FUNCTION reject_sending_operator_recipient_version_mutation();

DROP TRIGGER IF EXISTS sending_operator_recipient_versions_reject_truncate ON sending_operator_recipient_versions;
CREATE TRIGGER sending_operator_recipient_versions_reject_truncate
BEFORE TRUNCATE ON sending_operator_recipient_versions
FOR EACH STATEMENT EXECUTE FUNCTION reject_sending_operator_recipient_version_mutation();

CREATE TABLE IF NOT EXISTS sending_ramp_grandfathering (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    policy_generation BIGINT NOT NULL CHECK (policy_generation >= 0),
    completed_at TIMESTAMPTZ NOT NULL,
    completed_by TEXT NOT NULL CHECK (btrim(completed_by) <> '')
);

INSERT INTO sending_protection_runtime_policy
    (singleton, generation, schema_version, policy, policy_sha256, activated_at, activated_by)
VALUES (
    true,
    0,
    1,
    '{"all_customer_global_daily_recipients":5000,"bounce_min_outcomes":50,"bounce_pause_basis_points":400,"budget_hold_max_days":7,"budget_mode":"disabled","complaint_pause_basis_points":8,"critical_operational_daily_recipients":100,"daily_unlimited_plan_codes":["starter","pro","scale"],"default_account_daily_recipients":100,"detector_interval_seconds":300,"detector_mode":"disabled","detector_window_days":7,"operator_notice_recipient_version":1,"probation_global_daily_recipients":150,"ramp_days":30,"ramp_enabled":false,"ramp_start_daily":150,"ramp_target_daily":2000,"sending_control_audit_retention_days":90,"sending_feedback_post_account_retention_days":30,"shared_domain_account_daily_recipients":50,"shared_reputation_bounce_min_outcomes":1,"tenant_header_canary_account_ids":[],"tenant_header_mode":"disabled","tenant_provisioning_mode":"disabled","tenant_suppression_sync_mode":"disabled","violation_operational_daily_recipients":100}'::jsonb,
    '198d8cfb3220b6094a3b8dfe13cb0e2ff97c512ad87ae14609e580ae335c9ce6',
    now(),
    'migration'
)
ON CONFLICT (singleton) DO NOTHING;

INSERT INTO sending_protection_runtime_attestation
    (singleton, revision, active_billing_digest, active_billing_contract,
     rollback_billing_digest, rollback_billing_contract, updated_by)
VALUES (true, 0, '', 0, '', 0, 'migration')
ON CONFLICT (singleton) DO NOTHING;
