-- Account trust is opt-in. No existing account or domain is automatically exempted.
CREATE TABLE IF NOT EXISTS account_sending_trust (
 user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 archived_clean_days INTEGER NOT NULL DEFAULT 0 CHECK (archived_clean_days >= 0),
 grandfather_daily INTEGER NOT NULL DEFAULT 0 CHECK (grandfather_daily BETWEEN 0 AND 2000)
);
CREATE TABLE IF NOT EXISTS account_send_days (
 user_id TEXT NOT NULL REFERENCES account_sending_trust(user_id) ON DELETE CASCADE,
 day DATE NOT NULL,
 reserved_count INTEGER NOT NULL DEFAULT 0 CHECK (reserved_count >= 0),
 shared_count INTEGER NOT NULL DEFAULT 0 CHECK (shared_count >= 0),
 confirmed_count INTEGER NOT NULL DEFAULT 0 CHECK (confirmed_count >= 0),
 breached BOOLEAN NOT NULL DEFAULT false,
 PRIMARY KEY (user_id, day)
);
CREATE TABLE IF NOT EXISTS account_send_reservations (
 message_id TEXT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL REFERENCES account_sending_trust(user_id) ON DELETE CASCADE,
 day DATE NOT NULL,
 units INTEGER NOT NULL CHECK (units > 0),
 shared BOOLEAN NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('reserved','confirmed','released')),
 FOREIGN KEY (user_id,day) REFERENCES account_send_days(user_id,day)
);
CREATE INDEX IF NOT EXISTS account_send_reservations_owner ON account_send_reservations(user_id,day);

-- Internal-only customer messages still need an authorization token but expose
-- zero external recipients. Preserve all existing positive reservations.
ALTER TABLE sending_budget_reservations DROP CONSTRAINT IF EXISTS sending_budget_reservations_units_check;
ALTER TABLE sending_budget_reservations ADD CONSTRAINT sending_budget_reservations_units_check CHECK (units >= 0) NOT VALID;
ALTER TABLE sending_budget_reservations VALIDATE CONSTRAINT sending_budget_reservations_units_check;
