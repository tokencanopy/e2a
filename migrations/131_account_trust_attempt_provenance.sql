-- Attempt units have different meanings across runtime-policy generations.
-- Only explicitly external-recipient units may earn account trust progress.
-- Existing attempts remain NULL and conservatively earn no account trust.
ALTER TABLE sending_budget_reservations ADD COLUMN IF NOT EXISTS account_trust_units INTEGER
 CHECK (account_trust_units IS NULL OR account_trust_units >= 0);
