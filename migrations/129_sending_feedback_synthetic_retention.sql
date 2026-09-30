-- 129_sending_feedback_synthetic_retention.sql
--
-- Give feedback provenance of system- and internal-class accounts a fixed
-- 30-day horizon.
--
-- The sending-policy gate writes customer-purpose correlations with
-- expires_at NULL: they are kept for the source account's lifetime, and the
-- account purge stamps the post-deletion horizon. System and internal
-- accounts (the standing prober, monitors, conformance) are never deleted,
-- so their correlations — nearly all of the table on a hosted deployment,
-- since the prober sends every 30 seconds — would be kept forever. From this
-- release the gate stamps them at creation (created_at + the post-account
-- retention, 30 days by default); this file is the one-time backlog pass for
-- rows written before it, using the same 30-day default
-- (sending_feedback_post_account_retention_days).
--
-- Only rows whose source account's server-owned users.account_class is
-- 'system' or 'internal' are touched; standard and demo accounts keep the
-- account-lifetime rule. Events inherit their correlation's expiry;
-- recipient rows carry no expiry and are removed with their correlation. The
-- existing feedback retention janitor (GCFeedback) then deletes whatever is
-- already past its horizon on its next hourly pass.
--
-- Idempotent: only NULL expiries are written. Row-level UPDATEs only;
-- lock_timeout bounds waiting on a row a concurrent feedback transaction
-- holds, failing the boot (which retries) rather than hanging it. Each UPDATE
-- also has a 30-second statement budget; a timeout rolls the migration back.
-- This is a single-transaction backfill, not a batched sweep. If the backlog
-- cannot fit the budget, stop rollout and arrange a reviewed batched backfill.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

UPDATE sending_feedback_correlations c
   SET expires_at = c.created_at + interval '30 days'
  FROM users u
 WHERE u.id = c.source_account_ref
   AND u.account_class IN ('system', 'internal')
   AND c.expires_at IS NULL
   AND c.purpose IN ('customer_message', 'customer_notification');

UPDATE sending_feedback_events e
   SET expires_at = c.expires_at
  FROM sending_feedback_correlations c
 WHERE c.correlation_id = e.correlation_id
   AND e.expires_at IS NULL
   AND c.expires_at IS NOT NULL;
