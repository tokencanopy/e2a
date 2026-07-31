-- 094_agent_send_rate_windows.sql
-- Durable per-agent fire-time send rate limit (internal/sendrate): one row per
-- agent holding the timestamps of its recent provider submissions — a
-- sliding-window log enforcing "max 60 submissions/min/agent" at the River
-- send worker, immediately before upstream SMTP submission. The acceptance-time
-- in-memory limiter (internal/httpapi checkSendLimit) stays as abuse control;
-- this table caps what the provider actually sees, including scheduled-send
-- bursts and multi-replica deployments the in-memory limiter cannot
-- coordinate.
--
-- Rows are tiny (at most one window's worth of timestamps, pruned on every
-- Reserve), so no janitor sweep is needed. agent_id cascades with the agent
-- row, matching the 067 FK convention.
CREATE TABLE IF NOT EXISTS agent_send_rate_windows (
    agent_id   TEXT PRIMARY KEY REFERENCES agent_identities(id) ON DELETE CASCADE,
    events     TIMESTAMPTZ[] NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
