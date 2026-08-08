-- 095_account_limits_outbound_footer.sql
--
-- Per-account entitlement for the operator-configured outbound footer
-- (config `outbound_footer:` block): when the deployment enables the
-- feature, an account WITH a row gets the footer appended to its
-- SMTP-egress outbound mail only when this column is true; accounts with
-- NO row fall back to `outbound_footer.default_enabled` in config —
-- exactly the existing account_limits row-vs-defaults contract (016).
--
-- Like plan_code, the semantics of who gets the footer are opaque to the
-- OSS server: whatever provisions the row (the hosted billing sidecar,
-- admin tooling, manual SQL) writes this column; the OSS server only
-- reads it at send time. Default FALSE keeps self-host deployments and
-- every existing row byte-for-byte unchanged.
--
-- ALTER TABLE ... ADD COLUMN ... DEFAULT FALSE is metadata-only on
-- Postgres 11+ (constant default). Safe on the small account_limits
-- table regardless. Idempotent via IF NOT EXISTS.

ALTER TABLE account_limits
    ADD COLUMN IF NOT EXISTS outbound_footer_enabled BOOLEAN NOT NULL DEFAULT FALSE;
