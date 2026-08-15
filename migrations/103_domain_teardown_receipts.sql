-- Durable, owner-scoped receipt for asynchronous sending-identity teardown.
--
-- The domain row is deleted before provider convergence completes. Retaining
-- this receipt lets an authenticated retry of the same DELETE distinguish a
-- completed teardown from a lost pending response, without exposing another
-- account's deletion state.

CREATE TABLE IF NOT EXISTS domain_teardown_receipts (
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

CREATE INDEX IF NOT EXISTS idx_domain_teardown_receipts_user
	ON domain_teardown_receipts(user_id, domain, receipt_id DESC);

-- Receipts deliberately survive re-registration. A keyed DELETE is bound to
-- the deleted registration's incarnation and must keep polling that historical
-- receipt without touching a same-name replacement. Unkeyed polling selects
-- the newest owner-scoped receipt, and the HTTP layer never consults one while
-- a live domain row exists.
