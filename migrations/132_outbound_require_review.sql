-- 132_outbound_require_review.sql
--
-- Outbound protection: a `require_review` switch (issue #989).
--
-- "Hold every outbound send for review" was only expressible by composing
-- outbound_policy='allowlist' with an EMPTY allowlist and action='review': no
-- recipient matches, so the non-match action fires for every send. That works,
-- but it overloads "nothing matched" to mean "we decided to hold", and an empty
-- allowlist is indistinguishable from an unfinished trust ramp.
--
-- This adds an explicit per-agent boolean so the posture reads as what it is.
-- When true, the outbound gate holds every send for review regardless of the
-- gate policy, the allowlist, and the configured non-match action.
--
-- ADDITIVE and idempotent. Default false preserves today's behavior (including
-- the empty-allowlist composition, which is left working).

ALTER TABLE agent_identities ADD COLUMN IF NOT EXISTS outbound_require_review BOOLEAN NOT NULL DEFAULT false;
