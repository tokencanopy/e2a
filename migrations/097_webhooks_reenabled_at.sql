-- 097: re-enable resets the auto-disable/warn evidence window.
--
-- The health-notification feature's own recovery instruction is "fix the
-- endpoint, then re-enable it". Without a reset, the >=10 failed rows that
-- caused an auto-disable are still inside the 72h breaker window after the
-- user re-enables, so the next 5-minute sweep would re-disable and send a
-- false "we disabled your webhook" email — a loop for any low-traffic
-- webhook. reenabled_at is stamped on every PATCH that sets enabled=true;
-- the breaker and warn sweeps only count delivery rows created after it.
ALTER TABLE webhooks
  ADD COLUMN IF NOT EXISTS reenabled_at TIMESTAMPTZ;
