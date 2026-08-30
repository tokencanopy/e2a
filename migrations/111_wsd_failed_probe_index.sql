-- 111_wsd_failed_probe_index.sql
-- e2a:no-transaction
--
-- Partial index backing the webhook_status derivation's failing-deliveries
-- probe (webhookStatusSQL in internal/identity/store.go):
--
--   EXISTS (SELECT 1 FROM webhook_subscriber_deliveries wsd
--           JOIN webhooks w ON w.id = wsd.webhook_id
--           WHERE ... wsd.status = 'failed'
--             AND wsd.last_attempt_at > now() - interval '24 hours')
--
-- webhook_subscriber_deliveries previously had NO index on webhook_id, so
-- this correlated probe walked the whole table — once per agent row — in
-- every surface that derives webhook_status (ListAgentsByUser, the account
-- export, MCP list_agents). Measured on staging 2026-08-28: 479,767 delivery
-- rows made the probe ~600ms per agent; a 3,072-agent account's GDPR export
-- exceeded the 60s proxy timeout, failing the release conformance gate. The
-- partial predicate keeps the index tiny (failed rows only — typically a
-- sliver of deliveries) while making the probe an index range scan.
--
-- CREATE INDEX CONCURRENTLY so the build doesn't block delivery writes on
-- deployments with large tables; the runner's e2a:no-transaction directive
-- (internal/identity/migrate.go) is required for CONCURRENTLY and the file
-- must stay single-statement.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_wsd_failed_by_webhook
    ON webhook_subscriber_deliveries (webhook_id, last_attempt_at)
    WHERE status = 'failed';
