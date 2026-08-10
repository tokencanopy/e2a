-- 099_agent_purge_token.sql
--
-- A permanent purge commits between bounded chunks, so the agent row needs an
-- immutable claim that distinguishes this irreversible drain from ordinary
-- trash and from a later inbox that reuses the same address.
ALTER TABLE agent_identities
    ADD COLUMN IF NOT EXISTS purge_token TEXT;

-- Fail closed during a rolling deploy: an older binary's legacy restore only
-- clears deleted_at. It must not make a partially purged inbox live while the
-- durable claim remains.
DO $$ BEGIN
    ALTER TABLE agent_identities
        ADD CONSTRAINT agent_identities_purge_requires_deleted
        CHECK (purge_token IS NULL OR deleted_at IS NOT NULL) NOT VALID;
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- Validate without holding ACCESS EXCLUSIVE for the table scan. Existing rows
-- all have a NULL token, so validation is deterministic and non-destructive.
ALTER TABLE agent_identities
    VALIDATE CONSTRAINT agent_identities_purge_requires_deleted;

-- Keeps the token-aware janitor's common empty sweep on indexes. The existing
-- deleted_at partial index supplies the other arm of its OR predicate.
CREATE INDEX IF NOT EXISTS agent_identities_purge_token_idx
    ON agent_identities (purge_token)
    WHERE purge_token IS NOT NULL;
