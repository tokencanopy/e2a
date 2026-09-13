-- Durable per-recipient verification-mail throttle. Kept separate from the
-- lifecycle migration so upgrades remain append-only after 121 has run.
CREATE TABLE IF NOT EXISTS agent_signup_verification_events (
    id BIGSERIAL PRIMARY KEY,
    human_email TEXT NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS agent_signup_verification_events_window_idx
    ON agent_signup_verification_events (human_email, sent_at);
