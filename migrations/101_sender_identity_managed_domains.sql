-- Durable ownership ledger for SES identities managed by e2a.
--
-- Domain rows disappear before asynchronous SES teardown runs, so the
-- domains table alone cannot distinguish an e2a-created orphan from another
-- identity in the same SES account. This ledger survives domain deletion
-- until provider teardown succeeds, giving the periodic reconciler a safe,
-- bounded set to converge after River exhausts its normal retry budget.

CREATE TABLE IF NOT EXISTS sender_identity_managed_domains (
	domain TEXT PRIMARY KEY,
	incarnation TEXT NOT NULL,
	applied_incarnation TEXT,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Adopt provider work already recorded by the legacy release. Restrict this
-- to customer-owned rows and actual sender-identity status: verified alone is
-- not provider intent (self-hosts may have this feature disabled, and the
-- built-in shared domain has no per-customer DKIM key).
INSERT INTO sender_identity_managed_domains (domain, incarnation, applied_incarnation)
SELECT domain, verification_token, verification_token
  FROM domains
 WHERE user_id IS NOT NULL AND sending_status <> 'none'
ON CONFLICT (domain) DO NOTHING;

-- Migration 101 runs while the old blue/green slot is still live. That binary
-- does not know about the ledger, so a one-time backfill alone has a race: it
-- may finish a provision and write sending_status after the SELECT above.
-- Capture that feature-specific transition independently of worker version.
-- The provider-side ownership tag remains the authority for mutation/deletion;
-- a ledger row alone can never authorize touching an unrelated SES identity.
CREATE OR REPLACE FUNCTION track_sender_identity_managed_domain()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
	IF NEW.user_id IS NOT NULL AND NEW.sending_status <> 'none' AND (
		TG_OP = 'INSERT'
		OR OLD.sending_status = 'none'
		OR OLD.verification_token IS DISTINCT FROM NEW.verification_token
	) THEN
		INSERT INTO sender_identity_managed_domains
			(domain, incarnation, applied_incarnation, updated_at)
		VALUES (NEW.domain, NEW.verification_token, NULL, now())
		ON CONFLICT (domain) DO UPDATE
		SET incarnation = EXCLUDED.incarnation,
		    applied_incarnation = NULL,
		    updated_at = now();
	END IF;
	RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS domains_track_sender_identity_managed ON domains;
CREATE TRIGGER domains_track_sender_identity_managed
AFTER INSERT OR UPDATE OF sending_status, verification_token ON domains
FOR EACH ROW
EXECUTE FUNCTION track_sender_identity_managed_domain();
