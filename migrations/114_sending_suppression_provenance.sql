-- 114_sending_suppression_provenance.sql
--
-- Keep this bounded transaction limited to the unavoidable suppressions
-- metadata lock. Safe defaults preserve the pre-sync local DELETE path; this
-- migration never contacts SES or changes existing local suppression behavior.

SET LOCAL lock_timeout = '2s';

ALTER TABLE suppressions
    ADD COLUMN IF NOT EXISTS sync_generation BIGINT NOT NULL DEFAULT 1;

ALTER TABLE suppressions
    ADD COLUMN IF NOT EXISTS removal_pending BOOLEAN NOT NULL DEFAULT false;

DO $$
BEGIN
    ALTER TABLE suppressions ADD CONSTRAINT suppressions_sync_generation_check
        CHECK (sync_generation > 0) NOT VALID;
EXCEPTION WHEN duplicate_object THEN NULL;
END
$$;
