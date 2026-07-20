-- 067_batches.sql
--
-- Storage for the batch-send feature — the primitive that lets one API call
-- fan out N independent messages, each with its own message_id/state/retry
-- envelope. See docs/design/batch-send.md §3 for the full data-model rationale;
-- this migration lands the SQL side of that design.
--
-- One `batches` row per POST /v1/agents/{email}/batches accept-tx (§1.1); each
-- `messages` row created by that accept-tx carries the minted `batch_id` back
-- to the parent. Items dropped by the suppression filter (§2.2) get NO
-- `messages` row — the drop is recorded on the batch header in
-- `suppressed_json` so the caller can still see what was filtered.
--
-- FK strategy — chosen so `batches` and `messages` have opposite lifecycles:
--   batches.user_id            ON DELETE CASCADE — user deletion cascades; the
--                                                  batch header has no meaning
--                                                  once the owner is gone.
--   batches.agent_id           ON DELETE CASCADE — same reasoning; an agent's
--                                                  batch history dies with the
--                                                  agent (matches how messages
--                                                  cascade on agent_id today).
--   messages.batch_id (below)  ON DELETE SET NULL — deleting a batch row MUST
--                                                  NOT cascade-delete messages;
--                                                  messages are the record of
--                                                  what was sent and outlive
--                                                  the batch header. The
--                                                  90-day retention/janitor
--                                                  sweep on messages is
--                                                  unchanged.
--
-- Idempotent: CREATE TABLE / CREATE INDEX / ADD COLUMN all use IF NOT EXISTS.
-- Additive only — no destructive ALTERs.

CREATE TABLE IF NOT EXISTS batches (
    batch_id        TEXT        PRIMARY KEY,
    user_id         TEXT        NOT NULL REFERENCES users(id)            ON DELETE CASCADE,
    agent_id        TEXT        NOT NULL REFERENCES agent_identities(id) ON DELETE CASCADE,
    requested       INTEGER     NOT NULL,
    accepted        INTEGER     NOT NULL,
    suppressed_json JSONB       NOT NULL DEFAULT '[]'::jsonb,
    request_id      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Sanity constraints on the counters. `requested` matches the accepted-time
-- cap on len(request.messages) (docs/design/batch-send.md §14 Q5); `accepted`
-- is `requested` minus per-item suppression drops (§2.2). NOT VALID is not
-- used because batches is a new table with no historical rows to scan.
ALTER TABLE batches DROP CONSTRAINT IF EXISTS batches_requested_range_check;
ALTER TABLE batches ADD CONSTRAINT batches_requested_range_check
    CHECK (requested >= 1 AND requested <= 100);

ALTER TABLE batches DROP CONSTRAINT IF EXISTS batches_accepted_range_check;
ALTER TABLE batches ADD CONSTRAINT batches_accepted_range_check
    CHECK (accepted >= 0 AND accepted <= requested);

-- Per-owner listing is the primary read path (a future listBatches endpoint —
-- §11 polish — plus dashboard queries). DESC on created_at so the most-recent
-- page needs no reverse scan.
CREATE INDEX IF NOT EXISTS batches_user_created_at_idx
    ON batches (user_id, created_at DESC);

-- Per-agent listing supports GET /v1/agents/{email}/batches if we add it later.
CREATE INDEX IF NOT EXISTS batches_agent_created_at_idx
    ON batches (agent_id, created_at DESC);

-- messages.batch_id — nullable FK back to the batches row for messages
-- created as children of a batch. NULL for single-send messages (the
-- overwhelming majority of rows), so a partial index (below) keeps the
-- index small.
ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS batch_id TEXT REFERENCES batches(batch_id) ON DELETE SET NULL;

-- Partial index — skip the NULLs. Rollup query for GET /v1/batches/{id}
-- (docs/design/batch-send.md §7.1) is
--   SELECT delivery_status, count(*) FROM messages WHERE batch_id = $1 GROUP BY delivery_status
-- and this partial index makes that a single-table lookup at ≤100 rows
-- per batch.
CREATE INDEX IF NOT EXISTS messages_batch_id_idx
    ON messages (batch_id) WHERE batch_id IS NOT NULL;
