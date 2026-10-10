-- 132_owner_email_proof_oidc_source.sql
-- Allow 'oidc' as an owner-mailbox proof source. A generic OIDC login whose
-- verified ID token asserts email_verified for the account's current email can
-- now record the same proof a verified Google login records (migration 121).
-- Only the allowed-source list widens; the completeness and normalization
-- checks are unchanged. Idempotent.
DO $$ BEGIN
    ALTER TABLE users DROP CONSTRAINT IF EXISTS users_owner_email_verified_source_check;
    ALTER TABLE users ADD CONSTRAINT users_owner_email_verified_source_check
        CHECK (owner_email_verified_source IS NULL OR owner_email_verified_source IN ('google_oauth', 'oidc'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;
