-- Durable, owner-scoped receipt for asynchronous sending-identity teardown.
--
-- The domain row is deleted before provider convergence completes. Retaining
-- this receipt lets an authenticated retry of the same DELETE distinguish a
-- completed teardown from a lost pending response, without exposing another
-- account's deletion state.

CREATE TABLE IF NOT EXISTS domain_teardown_receipts (
	domain TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	state TEXT NOT NULL CHECK (state IN ('pending', 'manual_review', 'confirmed')),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	CHECK (domain = lower(domain))
);

CREATE INDEX IF NOT EXISTS idx_domain_teardown_receipts_user
	ON domain_teardown_receipts(user_id);

-- A receipt describes one deleted registration incarnation. Re-registering
-- the same DNS name invalidates it before the new row becomes visible, so an
-- old owner or cleanup process can never use a stale "confirmed" receipt to
-- remove DNS from under a replacement identity.
CREATE OR REPLACE FUNCTION clear_domain_teardown_receipt_on_registration()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
	DELETE FROM domain_teardown_receipts WHERE domain = NEW.domain;
	RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS domains_clear_teardown_receipt ON domains;
CREATE TRIGGER domains_clear_teardown_receipt
BEFORE INSERT ON domains
FOR EACH ROW
EXECUTE FUNCTION clear_domain_teardown_receipt_on_registration();
