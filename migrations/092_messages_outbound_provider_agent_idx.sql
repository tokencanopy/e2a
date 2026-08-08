-- e2a:no-transaction
-- Exact mailbox-local fallback for referenced legacy outbound anchors. The
-- provider-first key extends the older provider-only feedback-correlation
-- access path with the owning mailbox.
--
-- OPS NOTE — invalid-index recovery: if the CONCURRENTLY build is interrupted,
-- the migration runner refuses to record this migration while the same-name
-- index is invalid. Drop that invalid index, then restart migration:
--   DROP INDEX CONCURRENTLY IF EXISTS messages_outbound_provider_agent_idx;
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_outbound_provider_agent_idx
    ON messages (provider_message_id, agent_id, id)
    INCLUDE (thread_id)
    WHERE direction = 'outbound' AND provider_message_id <> '';
