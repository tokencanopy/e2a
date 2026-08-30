-- 108_external_principal_mappings.sql
--
-- Canonical mapping from an external OIDC principal to a local user, for
-- the delegated access-token verifier (config `delegated:`). A verified
-- delegated token's (iss, sub) pair resolves through this table with one
-- exact read-only lookup; authentication never creates rows, never looks
-- up by subject alone or by email, and an unmapped pair is a plain 401
-- (no existence oracle).
--
-- Why not users.google_subject: that column carries the user's LOGIN
-- identity ("bootstrap:<external_ref>" for provisioned accounts, a real
-- Google/OIDC subject for migrated ones) and overwriting it would break
-- standalone sign-in. This table is additive: multiple intentionally
-- attached issuer/subject pairs may map to one user, while the primary
-- key guarantees one pair never maps to two users.
--
-- Rows are written only by the signed internal provisioning/attach
-- surface (provision with external_issuer, and
-- POST /api/internal/users/external-principals/attach). ON DELETE
-- CASCADE follows the repo-wide users(id) convention so account
-- deletion cannot strand a live delegated mapping.
CREATE TABLE IF NOT EXISTS external_principal_mappings (
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject)
);

-- Account deletion and operator tooling walk mappings by user.
CREATE INDEX IF NOT EXISTS external_principal_mappings_user_id_idx
    ON external_principal_mappings (user_id);
