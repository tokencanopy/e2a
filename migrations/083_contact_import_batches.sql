-- 083_contact_import_batches.sql
--
-- Durable receipts for every contact import, including batches that only
-- updated existing contacts or enrolled them with an agent. Engagement
-- provenance lets reversal remove only enrolments created by that import.

CREATE TABLE IF NOT EXISTS contact_import_batches (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    id         TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
);

CREATE INDEX IF NOT EXISTS contact_import_batches_created_idx
    ON contact_import_batches (user_id, created_at DESC, id);

ALTER TABLE contact_engagements
    ADD COLUMN IF NOT EXISTS import_batch_id TEXT;

CREATE INDEX IF NOT EXISTS contact_engagements_import_batch_idx
    ON contact_engagements (user_id, import_batch_id)
    WHERE import_batch_id IS NOT NULL;
