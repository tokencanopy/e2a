-- 096: webhook health-notification metadata (design
-- docs/design/2026-08-08-webhook-health-notifications.md).
--
-- warn_notified_at is the early-warning dedupe marker: the maintenance
-- sweep's warn pass stamps it (and enqueues the warning email) in one
-- transaction, and a successful delivery clears it so a webhook that
-- recovers and later degrades warns again. NULL = armed.
--
-- auto_disable_reason carries the short, customer-facing failure reason
-- (e.g. "HTTP 404") captured from the most recent terminal delivery
-- error when the auto-disable sweep trips a webhook. Cleared on
-- re-enable so a healthy webhook never shows a stale cause. It is
-- derived from webhook_subscriber_deliveries.last_error, which the
-- delivery worker already restricts to sanitized, customer-facing
-- strings — never internal hosts, IPs, or DB identifiers.
--
-- Both nullable, no backfill: nothing was notified before this shipped,
-- and rows already auto-disabled at cutover stay silent (the sweep only
-- enqueues on the enabled -> disabled transition).
ALTER TABLE webhooks
  ADD COLUMN IF NOT EXISTS warn_notified_at    TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS auto_disable_reason TEXT;
