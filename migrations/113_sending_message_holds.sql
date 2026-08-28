-- 113_sending_message_holds.sql
--
-- Keep this bounded transaction limited to the unavoidable messages metadata
-- lock. It follows the account FK transactions so it cannot invert the
-- messages-to-users order used by irreversible account deletion.

SET LOCAL lock_timeout = '2s';

ALTER TABLE messages ADD COLUMN IF NOT EXISTS local_hold_class TEXT;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS local_hold_anchor TIMESTAMPTZ;

DO $$
BEGIN
    ALTER TABLE messages ADD CONSTRAINT messages_local_hold_check CHECK (
        (local_hold_class IS NULL AND local_hold_anchor IS NULL)
        OR
        (local_hold_class IN ('rate_ramp_or_provider', 'tenant_setup', 'policy_budget')
            AND local_hold_anchor IS NOT NULL)
    ) NOT VALID;
EXCEPTION WHEN duplicate_object THEN
    NULL;
END
$$;
