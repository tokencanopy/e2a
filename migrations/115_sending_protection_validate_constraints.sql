-- 115_sending_protection_validate_constraints.sql
--
-- Validate the hot-table CHECKs and account FKs only after the migrations that
-- install them have committed. VALIDATE CONSTRAINT uses the weaker SHARE UPDATE
-- EXCLUSIVE lock, which remains compatible with ordinary
-- SELECT/INSERT/UPDATE/DELETE traffic. Bound acquisition in case another DDL or
-- maintenance operation already holds an incompatible lock.

SET LOCAL lock_timeout = '2s';

ALTER TABLE messages
    VALIDATE CONSTRAINT messages_local_hold_check;

ALTER TABLE suppressions
    VALIDATE CONSTRAINT suppressions_sync_generation_check;

ALTER TABLE account_sending_controls
    VALIDATE CONSTRAINT account_sending_controls_user_id_fkey;

ALTER TABLE account_sending_outcomes_daily
    VALIDATE CONSTRAINT account_sending_outcomes_daily_user_id_fkey;
