# Agent self-signup

Status: implementation design

## 1. Problem and goals

A coding agent that discovers e2a without a human-created API key needs a safe way to acquire one inbox and one narrowly-scoped credential. The public signup flow must be immediately useful for receiving mail while requiring control of a declared human address before normal outbound privileges are granted.

The flow creates exactly one provisional agent identity, sends a six-digit verification code to the declared human, supports approval from either the agent or the signed-in human, and reuses the existing outbound-review policy after verification. Repeating signup for the same normalized `(human_email, display_name)` rotates the credential and verification code instead of creating another identity.

## 2. Non-goals

- Anonymous creation of account-scoped credentials.
- A second message-review queue or a second outbound delivery path.
- Letting provisional identities create agents, domains, keys, webhooks, or other account resources.
- Making the hosted shared domain mandatory for self-hosters; deployments without `shared_domain` report signup as unavailable.
- Changing normal authenticated agent creation.

## 3. Caller experience

`POST /v1/agent-signup` is public and accepts `human_email`, `display_name`, optional `note_to_human`, optional `harness`, and optional `current_api_key`. It returns `201` for a new provisional identity and `200` for a possession-authorized re-signup. A re-signup of an existing pair requires its current key. Both successful responses reveal the newly rotated agent-scoped API key once, the inbox address, signup id, status, verification expiry, and the provisional restrictions.

The agent verifies with its returned key:

`POST /v1/agent-signup/verify`

with `code` and optional `review_outbound`. The human can instead sign in and use:

- `GET /v1/agent-signup/pending` (cursor-paginated)
- `POST /v1/agent-signup/{id}/approve`
- `POST /v1/agent-signup/{id}/reject`

Approval accepts the same optional `review_outbound`. Rejection deactivates the identity and revokes every key associated with that signup identity. All operations are beta in the v1 contract.

## 4. Ownership and lifecycle model

The signup transaction creates an isolated provisional user under the non-routable `agents.localhost` namespace, creates one agent, creates the signup row, and mints an agent-scoped key. If the declared human already has an account with verified custom domains, the inbox uses the primary domain (then the oldest verified domain as a deterministic fallback); otherwise it uses the deployment shared domain. The provisional row does not consume quota from or expose itself in the human's account. Verification creates or resolves the human's claimable placeholder account and atomically transfers the live agent, key, and signup record under that account's plan cap. A successful verified browser login may claim only a placeholder with the reserved `agent-signup:` subject prefix; ordinary email conflicts are never merged.

States are `pending`, `verified`, and `rejected`. Only `pending` can transition. Verification codes are HMAC-hashed, expire after 48 hours, permit five failed attempts, and are replaced on every re-signup. Re-signup proves possession of the current credential and revokes it before returning the replacement.

The unique normalized pair `(human_email, display_name)` is the idempotency key. The inbox address is generated from the display name plus a stable collision suffix when needed, and never changes on re-signup.

## 5. Provisional enforcement

The agent-scoped key already imposes the hard management ceiling: account-scoped endpoints reject it, so it cannot create another identity. A new store-backed guard is invoked inside the shared `DeliverOutbound` tail used by send, reply, and forward. While the signup is pending it:

1. rejects any To/Cc/Bcc address other than the normalized human email with HTTP 403, code `pending_human_verification`, and an actionable message;
2. serializes attempts per signup and records at most five accepted attempts in a rolling 24-hour ledger;
3. returns HTTP 429, code `rate_limited`, after the fifth accepted attempt.

Inbound SMTP remains unchanged, so anyone may deliver to the provisional inbox. Verified signups bypass the provisional guard. Rejected identities and their credentials are inactive.

## 6. Verification delivery and review primitive

Verification mail is composed as a platform notification and submitted through the existing authorized provider seam. It includes the six-digit code, the agent-supplied note as escaped content, and a link to the dashboard pending-signups view. Signup fails closed when notification delivery is not configured; it never returns a usable credential without attempting the verification mail.

When `review_outbound` is selected at verification or approval, the transition writes the existing agent protection configuration: recipient policy `allowlist`, the human email as the allowlist, and action `review`. Messages to the human continue directly; messages to anyone else enter the existing `/v1/reviews` queue. When it is false, ordinary plan behavior applies. No signup-specific review storage or worker is introduced.

## 7. Security, privacy, and abuse controls

- Normalize and validate human addresses and display names before lookup.
- Never log API keys, verification codes, notes, or full human addresses.
- Store only an HMAC of the verification code.
- Apply a dedicated anonymous per-source rate limit to public REST callers. Internal MCP-proxy calls are keyed by a digest of normalized human email, so all callers targeting one recipient share a budget without collapsing every MCP user into one global proxy bucket.
- Keep a durable five-per-24-hour verification-mail ledger per normalized human email across every source IP and transport.
- Use constant-time code comparison and uniform verification failures.
- Scope pending list/approve/reject by the authenticated account's verified email as well as signup id.
- Revoke existing signup keys transactionally on re-signup and rejection.
- Escape the note in HTML and bound its size in the public request contract.
- Use only synthetic addresses and content in source, tests, fixtures, and docs.

## 8. Rollout, compatibility, and verification

The migration is additive: new signup tables and indexes only. Existing agents, keys, review policies, and delivery paths are unchanged. Hosted discovery is added at `/llms.txt` and `/agent-signup.md`; SDKs, CLI, and MCP expose the same two agent-side operations. MCP permits only signup and verification without an existing bearer and retains the small anonymous body limit.

Verification covers store transitions and concurrency, the shared outbound guard across send/reply/forward, public/auth boundaries, OpenAPI and generated SDK drift, dashboard approval/rejection, MCP anonymous tool routing, CLI commands, and an end-to-end local signup→restricted-send→verify→review-policy flow.
