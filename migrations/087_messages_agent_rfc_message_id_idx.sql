-- e2a:no-transaction
-- Exact mailbox-local RFC Message-ID anchor lookup. Duplicate wire IDs are
-- valid, so this index is deliberately non-unique.
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_agent_rfc_message_id_idx
    ON messages (agent_id, rfc_message_id_key, created_at, id)
    WHERE rfc_message_id_key IS NOT NULL;
