-- Persist the first observation that a database-verified sender identity has
-- fallen back to provider-pending. Brief provider rechecks do not flap live
-- sending, but a state that remains pending past the bounded grace period must
-- fail closed instead of remaining verified forever.

ALTER TABLE sender_identity_managed_domains
	ADD COLUMN IF NOT EXISTS provider_pending_since TIMESTAMPTZ;
