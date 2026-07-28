-- 082_contacts_max.sql
--
-- Per-account contact cap. Mirrors max_templates (050) and max_webhooks (024):
-- a plain column on account_limits so a specific account can be raised without
-- a deploy.
--
-- 10000 is a SAFETY CEILING, not a plan lever. Contacts are beta with no
-- billing attached, so the only job this does today is stop one account
-- growing storage without bound. It is deliberately far above the design's
-- own working assumption (hundreds to low thousands per account): an account
-- that reaches it is doing something the design did not anticipate, which is
-- exactly when a conversation beats a silent limit. Ten times the 1000-row
-- import cap, so a legitimate upload-correct-reupload cycle has headroom.
--
-- Worst case with the 16KB metadata cap is ~160MB for one account; realistic
-- metadata (a fund name, a check size) is nearer 10-20MB.
--
-- ADD COLUMN with a default is metadata-only on PG11+, so this is safe on a
-- prod-sized account_limits.

ALTER TABLE account_limits
    ADD COLUMN IF NOT EXISTS max_contacts INTEGER NOT NULL DEFAULT 10000;
