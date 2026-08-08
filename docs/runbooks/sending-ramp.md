# Sending ramp operations

The sending ramp is an operator-managed safeguard for newly verified custom
sender domains. End users can read its state through the domain API (the
`sending_ramp` field — see
[`docs/api.md` → Domains](../api.md#domains-v1domains)) but cannot change the
schedule, exempt themselves, or reset progression.

## Progression and timeout

The worker reserves unique recipient capacity before provider I/O. A UTC day
advances the scope only after the provider accepts at least 50% of that day's
snapshotted recipient limit, rounded up. The next limit begins on the following
UTC day; reaching the final threshold does not make the current day unlimited.

Messages deferred by capacity or temporary ramp-store errors remain queued for
at most 72 hours from acceptance. At the horizon they fail with
`ramp_capacity_timeout`, release pending ramp capacity, and emit the normal
terminal failure outcome.

Monitor outbound queue age, repeated `ramp reservation failed` logs, and
`ramp_capacity_timeout` failures. A growing oldest-job age indicates either
sustained over-cap admission or a ramp-store incident.

## Exemptions

Migration `067_domain_sending_ramp.sql` exempts domains that were already
sending-verified when the feature shipped. A verified domain that sends while `sending_ramp.enabled` is
false is also persistently exempt. Enabling the feature later does not revoke
those exemptions. This prevents a rollout from unexpectedly throttling an
established sender.

## Operator-only reset

A reset re-arms every exact sender domain belonging to one tenant under one
organizational-domain scope. Confirm the tenant and scope with the preview
queries. Never run the deletes using only a domain suffix.

```sql
BEGIN;

-- Replace both literals. The scope must be the registrable organizational
-- domain used by sending_ramp_scopes, for example example.com.
\set ramp_user_id 'usr_replace_me'
\set ramp_scope 'example.com'

SELECT user_id, domain, status, active_days, last_qualified_day,
       start_daily, target_daily, ramp_days
  FROM sending_ramp_scopes
 WHERE user_id = :'ramp_user_id' AND domain = :'ramp_scope'
 FOR UPDATE;

SELECT domain, sending_status, sending_ramp_status
  FROM domains
 WHERE user_id = :'ramp_user_id'
   AND (domain = :'ramp_scope' OR domain LIKE '%.' || :'ramp_scope')
 ORDER BY domain
 FOR UPDATE;

-- Stop here if either preview contains an unexpected tenant or domain.
DELETE FROM sending_ramp_reservations
 WHERE user_id = :'ramp_user_id' AND domain = :'ramp_scope';

DELETE FROM domain_send_counters
 WHERE user_id = :'ramp_user_id' AND domain = :'ramp_scope';

DELETE FROM sending_ramp_scopes
 WHERE user_id = :'ramp_user_id' AND domain = :'ramp_scope';

UPDATE domains
   SET sending_ramp_status = 'inactive'
 WHERE user_id = :'ramp_user_id'
   AND sending_ramp_status = 'exempt'
   AND (domain = :'ramp_scope' OR domain LIKE '%.' || :'ramp_scope');

-- Inspect the affected rows and use exactly one of these:
ROLLBACK;
-- COMMIT;
```

Run the transaction while outbound workers for the tenant are paused. After a
commit, the next eligible external send creates a fresh scope using the current
operator schedule. A rollback leaves all ramp state unchanged.

Historical counters older than 35 days and terminal reservations older than
seven days are pruned daily. Pending reservations and their counters are never
removed by maintenance.
