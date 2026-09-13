-- Make signup notification delivery durable and bound provisional credentials
-- to the verification window. The nonce is non-secret: the six-digit code is
-- derived with the deployment HMAC secret and never stored in plaintext.
ALTER TABLE agent_signups
    ADD COLUMN IF NOT EXISTS code_nonce TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS shared_domain TEXT NOT NULL DEFAULT '';

UPDATE api_keys AS k
   SET expires_at = s.code_expires_at
  FROM agent_signups AS s
 WHERE k.agent_id = s.agent_id
   AND s.status = 'pending'
   AND k.revoked_at IS NULL
   AND k.expires_at IS NULL;

CREATE INDEX IF NOT EXISTS agent_signups_pending_expiry_idx
    ON agent_signups (code_expires_at, id)
    WHERE status = 'pending';
