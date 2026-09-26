CREATE TABLE IF NOT EXISTS agent_signups (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id TEXT NOT NULL UNIQUE REFERENCES agent_identities(id) ON DELETE CASCADE,
    human_email TEXT NOT NULL,
    display_name TEXT NOT NULL,
    display_name_key TEXT NOT NULL,
    note_to_human TEXT NOT NULL DEFAULT '',
    harness TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'verified', 'rejected')),
    code_hash TEXT NOT NULL,
    code_expires_at TIMESTAMPTZ NOT NULL,
    verification_attempts INTEGER NOT NULL DEFAULT 0 CHECK (verification_attempts >= 0),
    verification_sent_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    review_outbound BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at TIMESTAMPTZ,
    rejected_at TIMESTAMPTZ,
    UNIQUE (human_email, display_name_key)
);

CREATE INDEX IF NOT EXISTS agent_signups_pending_human_idx
    ON agent_signups (human_email, created_at DESC, id DESC)
    WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS agent_signup_send_events (
    id BIGSERIAL PRIMARY KEY,
    signup_id TEXT NOT NULL REFERENCES agent_signups(id) ON DELETE CASCADE,
    sent_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS agent_signup_send_events_window_idx
    ON agent_signup_send_events (signup_id, sent_at);
