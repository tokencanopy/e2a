-- Mailbox-local email reply topology. All columns are nullable so old binaries
-- can roll back safely and historical rows remain intentionally threadless.
ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS thread_id text,
    ADD COLUMN IF NOT EXISTS thread_parent_id text,
    ADD COLUMN IF NOT EXISTS rfc_message_id_key text;
