-- 124_sending_feedback_orphan_retention.sql
--
-- Backfill the post-deletion retention horizon on feedback provenance whose
-- account is already gone.
--
-- The sending-policy gate writes customer-purpose correlations with
-- expires_at NULL: they must outlive the message for as long as the account
-- exists. B8 stamps the horizon in the account purge's seal transaction —
-- but only for accounts purged by a release that carries B8. Every account
-- erased or purged earlier left its correlations (and their events) with a
-- NULL expiry that nothing would ever stamp, i.e. retained forever, beyond
-- the disclosed 30-day post-deletion window.
--
-- This stamps them now, by the same rule the seal uses:
--   * customer-purpose correlations (customer_message, customer_notification)
--     whose source_account_ref has no users row get expires_at = now() + 30
--     days — the policy default sending_feedback_post_account_retention_days
--     (identity.DefaultFeedbackRetention). A trashed account still has its
--     users row and is deliberately NOT stamped, exactly as the seal defers
--     until purge.
--   * every event with a NULL expiry whose correlation now has one inherits
--     the correlation's expiry. Recipient rows carry no expiry column; the
--     retention janitor removes them with their correlation.
--
-- The sending-policy reconcile job runs the same predicate daily, which also
-- catches a correlation authorized in a race with a purge. This file is the
-- one-time backlog pass.
--
-- Idempotent: only NULL expiries are written, so re-applying changes
-- nothing already stamped. Row-level UPDATEs only (no table lock beyond
-- ROW EXCLUSIVE); lock_timeout bounds waiting on a row a concurrent feedback
-- transaction holds, failing the boot (which retries) rather than hanging it.

SET LOCAL lock_timeout = '5s';

UPDATE sending_feedback_correlations c
   SET expires_at = now() + interval '30 days'
 WHERE c.expires_at IS NULL
   AND c.purpose IN ('customer_message', 'customer_notification')
   AND c.source_account_ref IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = c.source_account_ref);

UPDATE sending_feedback_events e
   SET expires_at = c.expires_at
  FROM sending_feedback_correlations c
 WHERE c.correlation_id = e.correlation_id
   AND e.expires_at IS NULL
   AND c.expires_at IS NOT NULL;
