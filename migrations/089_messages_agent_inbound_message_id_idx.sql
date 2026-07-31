-- e2a:no-transaction
-- Exact direction-aware fallback for directly referenced legacy inbound
-- anchors that predate rfc_message_id_key.
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_agent_inbound_message_id_idx
    ON messages (agent_id, email_message_id)
    WHERE direction = 'inbound' AND email_message_id <> '';
