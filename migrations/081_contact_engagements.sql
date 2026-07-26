-- 081_contact_engagements.sql
--
-- Per-agent outreach state (docs/design/2026-07-24-contacts-and-outreach-state.md
-- §3.2). An engagement is one agent's relationship with one contact: what stage
-- it is at, when to act next, and what has actually happened on the wire.
--
-- WHY THIS IS A SEPARATE TABLE FROM contacts:
-- identity is account-level (one row per human), but consent and outreach stage
-- are per-relationship. The same partner may be worked by raise@ and support@
-- with independent history, and an unsubscribe from one is not an unsubscribe
-- from the other. This is the same two-level shape suppressions /
-- agent_suppressions already uses, for the same reason.
--
-- agent_id CARRIES NO FOREIGN KEY, matching agent_suppressions — but for the
-- opposite lifetime, which is the subtle part:
--
--   * agent_id IS the agent's email address (agent_identities.id), so a
--     recreated agent at the same address inherits anything left behind.
--   * For SUPPRESSIONS that is deliberate and correct: consent must survive
--     deletion and recreation.
--   * For ENGAGEMENTS it would be a misfire. A recreated raise@ inheriting last
--     year's stage='touch3' and past-due next_action_at would wake up and mail
--     investors it never contacted. So engagements survive TRASH (a restored
--     agent must get its outreach state back) but are PURGED with the agent on
--     hard delete, in the same janitor sweep.
--
-- Derived columns are materialized rather than computed on read: the whole
-- point of the resource is the "who is due and has not replied" query, and a
-- correlated aggregate over the prod-sized messages table for every row of
-- every request would defeat it. A janitor reconciliation sweep recomputes them
-- and emits a metric on any correction, so drift is a bug signal rather than
-- routine.
--
-- New table only — no ALTER on a prod-sized table. Idempotent.

CREATE TABLE IF NOT EXISTS contact_engagements (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    -- No FK: see the lifetime note above. Normalized agent address.
    agent_id   TEXT NOT NULL,
    -- Denormalized recipient address so the outreach query and the due sweep
    -- never need to join contacts. Mirrors agent_suppressions.address.
    address    TEXT NOT NULL,

    -- Agent-owned. e2a never interprets these; stage in particular is an opaque
    -- string with no server-side state machine.
    stage          TEXT NOT NULL DEFAULT '',
    next_action_at TIMESTAMPTZ,
    metadata       JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- e2a-derived. Maintained at the seams that already exist (the lifecycle
    -- transition recorder for outbound, the relay for inbound) and reconciled
    -- by the janitor. `replied` is NOT stored: it is
    -- last_inbound_at > first_outbound_at, computed on read so it cannot drift
    -- independently of the timestamps it derives from.
    first_outbound_at    TIMESTAMPTZ,
    last_outbound_at     TIMESTAMPTZ,
    last_inbound_at      TIMESTAMPTZ,
    outbound_count       INTEGER NOT NULL DEFAULT 0,
    inbound_count        INTEGER NOT NULL DEFAULT 0,
    last_conversation_id TEXT NOT NULL DEFAULT '',

    -- The next_action_at value a contact.due was already emitted for. Fire only
    -- when it differs from next_action_at, so writing a new next_action_at
    -- re-arms automatically. NULL = never notified.
    notified_next_action_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, agent_id, contact_id)
);

-- The outreach listing: one agent's engagements, newest first, matching the
-- ordering every other v1 collection uses.
CREATE INDEX IF NOT EXISTS ce_list_idx
    ON contact_engagements (user_id, agent_id, created_at DESC, id);

-- Address lookup for a single engagement and for the inbound/outbound derived
-- updates, which arrive knowing (agent, recipient) rather than a contact id.
CREATE INDEX IF NOT EXISTS ce_address_idx
    ON contact_engagements (user_id, agent_id, address);

-- The due sweep. Partial so it stays small regardless of total engagements:
-- only rows that are actually armed and not yet notified are indexed.
CREATE INDEX IF NOT EXISTS ce_due_idx
    ON contact_engagements (next_action_at)
    WHERE next_action_at IS NOT NULL
      AND notified_next_action_at IS DISTINCT FROM next_action_at;
