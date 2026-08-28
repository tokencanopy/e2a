-- 112_sending_controls_foreign_key.sql
--
-- Install the controls ownership FK in a source-free transaction. Migration
-- 110 may retain legacy agent/message/webhook rows while it rewrites River
-- jobs, so requesting a users table lock there would invert account deletion's
-- users-to-cascade-source order. The bootstrap already ran in 110; remove only
-- users deleted before this transaction makes new writes enforce the FK.

SET LOCAL lock_timeout = '2s';

LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE account_sending_controls IN SHARE ROW EXCLUSIVE MODE;

DELETE FROM account_sending_controls AS controls
WHERE NOT EXISTS (
    SELECT 1 FROM users WHERE users.id = controls.user_id
);

DO $$
BEGIN
    ALTER TABLE account_sending_controls
        ADD CONSTRAINT account_sending_controls_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE NOT VALID;
EXCEPTION WHEN duplicate_object THEN
    NULL;
END
$$;
