-- 079_contacts.sql
--
-- Account-level contact identity (docs/design/2026-07-24-contacts-and-outreach-state.md
-- §3.2). A contact is one person this account corresponds with, keyed by the
-- normalized address. Per-agent outreach state does NOT live here — it belongs
-- on contact_engagements (080), because consent and outreach stage are
-- per-relationship while identity is not.
--
-- The address is stored in the canonical form produced by
-- identity.NormalizeMailboxAddress, which is the SAME key suppression lookups
-- use. That parity is load-bearing: a contact keyed differently from its
-- suppression would look sendable in the contact list and be blocked at send
-- time (or, worse, the reverse). TestContactKeyMatchesSuppressionKey pins it.
--
-- New table only — no ALTER on a prod-sized table, so this is safe to apply
-- online. Idempotent via IF NOT EXISTS.

CREATE TABLE IF NOT EXISTS contacts (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    address         TEXT NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- How this contact first entered the account. Not a lifecycle state: it
    -- records provenance and never changes after insert.
    source          TEXT NOT NULL CHECK (source IN ('import', 'manual', 'inbound')),
    -- Set only when source = 'import'. Groups a single import request so it can
    -- be reversed wholesale (DELETE /v1/contacts/imports/{batch_id}).
    import_batch_id TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, address)
);

-- List pagination: keyset on (created_at DESC, id) within a tenant, matching
-- the ordering every other v1 collection uses.
CREATE INDEX IF NOT EXISTS contacts_list_idx
    ON contacts (user_id, created_at DESC, id);

-- Import-batch lookup and reversal. Partial — only import-sourced rows carry a
-- batch, so the index stays small relative to the table.
CREATE INDEX IF NOT EXISTS contacts_batch_idx
    ON contacts (user_id, import_batch_id)
    WHERE import_batch_id IS NOT NULL;
