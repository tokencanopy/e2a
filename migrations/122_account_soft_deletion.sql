-- 122_account_soft_deletion.sql
-- Account soft deletion (trash), identity tombstones, pause classes and
-- deleted-account abuse-evidence summaries.
--
-- Expand-only. Every existing row keeps its meaning: no user is trashed, no
-- agent is marked as trashed-by-account, every session stays unrestricted,
-- and every existing sending-control row reads as an operator pause class
-- (the column default). Nothing here changes behaviour until the server code
-- that reads these columns is deployed.

-- Account trash. deleted_at is the trash stamp; purge_token is set ONLY when a
-- purge claims the row (the agent semantics: a non-null token means the
-- irreversible purge has committed its claim and a restore must refuse).
ALTER TABLE users ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS restored_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS purge_token TEXT;

DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_purge_token_requires_trash_check
        CHECK (purge_token IS NULL OR deleted_at IS NOT NULL);
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE INDEX IF NOT EXISTS users_deleted_at_idx
    ON users (deleted_at) WHERE deleted_at IS NOT NULL;

-- Explicit marker for agents trashed as part of an account trash. Restore
-- revives exactly these (never an agent the owner had trashed themselves),
-- and the per-agent janitor purge skips them so an account's content is never
-- purged out from under a skipped account purge.
ALTER TABLE agent_identities
    ADD COLUMN IF NOT EXISTS trashed_by_account BOOLEAN NOT NULL DEFAULT false;

DO $$ BEGIN
    ALTER TABLE agent_identities ADD CONSTRAINT agent_identities_trashed_by_account_check
        CHECK (NOT trashed_by_account OR deleted_at IS NOT NULL);
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- A restricted session is issued to a sign-in that resolves to a trashed
-- account. It authorizes exactly two actions — restore, or erase now — and
-- nothing else; every ordinary session lookup excludes it.
ALTER TABLE user_sessions
    ADD COLUMN IF NOT EXISTS restricted BOOLEAN NOT NULL DEFAULT false;

-- Pause classes. `abuse` is the one that matters to deletion: a purge of an
-- account whose control row carries it writes long-lived abuse tombstones.
-- evidence_ref is an optional operator reference (e.g. a private incident id)
-- carried into the deleted-account summary; it is never customer-visible.
ALTER TABLE account_sending_controls
    ADD COLUMN IF NOT EXISTS pause_class TEXT NOT NULL DEFAULT 'operator';
ALTER TABLE account_sending_controls
    ADD COLUMN IF NOT EXISTS evidence_ref TEXT;

DO $$ BEGIN
    ALTER TABLE account_sending_controls ADD CONSTRAINT account_sending_controls_pause_class_check
        CHECK (pause_class IN ('operator', 'abuse', 'billing', 'system'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

DO $$ BEGIN
    ALTER TABLE account_sending_controls ADD CONSTRAINT account_sending_controls_evidence_ref_check
        CHECK (evidence_ref IS NULL OR (btrim(evidence_ref) <> '' AND char_length(evidence_ref) <= 200));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- The control audit records the class (and evidence reference) a pause or
-- resume was applied with. Nullable: rows written before this migration, and
-- by any writer that predates it, carry none.
ALTER TABLE account_sending_control_events
    ADD COLUMN IF NOT EXISTS pause_class TEXT;
ALTER TABLE account_sending_control_events
    ADD COLUMN IF NOT EXISTS evidence_ref TEXT;

DO $$ BEGIN
    ALTER TABLE account_sending_control_events ADD CONSTRAINT account_sending_control_events_pause_class_check
        CHECK (pause_class IS NULL OR pause_class IN ('operator', 'abuse', 'billing', 'system'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- Identity tombstones: keyed digests (HMAC-SHA256 under the dedicated
-- tombstone key, never the feedback keyring) of the identifiers a purged
-- account held. account_ref is the purged user id — not a foreign key, the
-- row outlives the user by design.
CREATE TABLE IF NOT EXISTS identity_tombstones (
    kind        TEXT NOT NULL CHECK (kind IN ('login_subject', 'email', 'domain')),
    digest      BYTEA NOT NULL CHECK (octet_length(digest) = 32),
    key_version INTEGER NOT NULL CHECK (key_version > 0),
    class       TEXT NOT NULL CHECK (class IN ('recent_deletion', 'abuse')),
    account_ref TEXT NOT NULL CHECK (btrim(account_ref) <> ''),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (kind, digest, key_version),
    CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS identity_tombstones_expires_idx
    ON identity_tombstones (expires_at);
CREATE INDEX IF NOT EXISTS identity_tombstones_account_idx
    ON identity_tombstones (account_ref);

-- Deleted-account summary: a compact abuse-evidence record written once, at
-- purge, for every purged account. It holds counts, a bounded recipient-domain
-- histogram (domains only), per-day send counts, keyed digests of subjects and
-- verified domains, and the pause state at purge. It never holds message
-- bodies, recipient addresses, or the owner email in the clear. Retention is
-- expires_at (the abuse hold when the account was abuse-paused, otherwise the
-- recent-deletion hold); the janitor deletes expired rows.
CREATE TABLE IF NOT EXISTS deleted_account_summaries (
    account_ref             TEXT PRIMARY KEY CHECK (btrim(account_ref) <> ''),
    account_created_at      TIMESTAMPTZ NOT NULL,
    deleted_at              TIMESTAMPTZ NOT NULL,
    purged_at               TIMESTAMPTZ NOT NULL,
    account_class           TEXT NOT NULL,
    sending_state           TEXT CHECK (sending_state IS NULL OR sending_state IN ('active', 'paused')),
    pause_class             TEXT CHECK (pause_class IS NULL OR pause_class IN ('operator', 'abuse', 'billing', 'system')),
    pause_reason            TEXT,
    evidence_ref            TEXT,
    agents_count            INTEGER NOT NULL CHECK (agents_count >= 0),
    messages_count          BIGINT NOT NULL CHECK (messages_count >= 0),
    outbound_sends_count    BIGINT NOT NULL CHECK (outbound_sends_count >= 0),
    first_outbound_at       TIMESTAMPTZ,
    last_outbound_at        TIMESTAMPTZ,
    recipient_domains       JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(recipient_domains) = 'array'),
    daily_sends             JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(daily_sends) = 'array'),
    subject_digests         JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(subject_digests) = 'array'),
    verified_domain_digests JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(verified_domain_digests) = 'array'),
    digest_key_version      INTEGER CHECK (digest_key_version IS NULL OR digest_key_version > 0),
    retention_class         TEXT NOT NULL CHECK (retention_class IN ('recent_deletion', 'abuse')),
    expires_at              TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > purged_at),
    CHECK (purged_at >= deleted_at)
);

CREATE INDEX IF NOT EXISTS deleted_account_summaries_expires_idx
    ON deleted_account_summaries (expires_at);
