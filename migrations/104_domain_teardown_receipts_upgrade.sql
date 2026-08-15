-- Forward upgrade for prerelease environments that already applied the first
-- migration 103 shape. Migrations are immutable once recorded, so changing
-- 103 alone would leave those databases with a domain-primary-key receipt and
-- a trigger that erases history on re-registration.

DROP TRIGGER IF EXISTS domains_clear_teardown_receipt ON domains;
DROP FUNCTION IF EXISTS clear_domain_teardown_receipt_on_registration();

DO $$
BEGIN
	IF EXISTS (
		SELECT 1
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'domain_teardown_receipts'
		  AND column_name = 'domain'
	) AND NOT EXISTS (
		SELECT 1
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'domain_teardown_receipts'
		  AND column_name = 'incarnation'
	) THEN
		CREATE TEMP TABLE domain_teardown_receipts_legacy_upgrade
		ON COMMIT DROP AS
		SELECT domain, user_id, state, created_at, updated_at
		FROM domain_teardown_receipts;

		DROP TABLE domain_teardown_receipts;
		CREATE TABLE domain_teardown_receipts (
			receipt_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			domain TEXT NOT NULL,
			incarnation TEXT NOT NULL,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			state TEXT NOT NULL CHECK (state IN ('pending', 'manual_review', 'confirmed')),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (domain, incarnation),
			CHECK (incarnation <> ''),
			CHECK (domain = lower(domain))
		);

		INSERT INTO domain_teardown_receipts
			(domain, incarnation, user_id, state, created_at, updated_at)
		SELECT domain,
		       'legacy:' || md5(domain || E'\x1f' || user_id || E'\x1f' || created_at::text),
		       user_id,
		       state,
		       created_at,
		       updated_at
		FROM domain_teardown_receipts_legacy_upgrade;
	END IF;
END
$$;

CREATE INDEX IF NOT EXISTS idx_domain_teardown_receipts_user
	ON domain_teardown_receipts(user_id, domain, receipt_id DESC);
