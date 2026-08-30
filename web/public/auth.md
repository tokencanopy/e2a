# auth.md

This guide explains how an agent connects to e2a, the open-source email API for AI agents. It covers how to obtain credentials today, handle them safely, and follow the protocol as it evolves.

Two hosts are relevant:

- **API** — `https://api.e2a.dev` — the resource server you will call (`/v1/...`, MCP at `/mcp`).
- **Dashboard & docs** — `https://e2a.dev` — where the user manages agents, domains, API keys, and billing, and where these `.md` docs live.

## Current state

e2a already implements the OAuth 2.1 surface that MCP clients depend on: RFC 8414 authorization-server metadata at [`/.well-known/oauth-authorization-server`](https://api.e2a.dev/.well-known/oauth-authorization-server), RFC 7591 Dynamic Client Registration at `/oauth2/register` (rate-limited per IP), `authorization_code` + `refresh_token` grants with PKCE S256, RFC 7009 revocation, and RFC 6750 Bearer challenges on 401s. MCP clients can register and onboard without any human-supplied secret — the user only sees a browser consent screen.

e2a also implements the first pieces of the WorkOS [auth.md](https://github.com/workos/auth.md) autonomous-agent flow: a JSON Web Key Set at `/.well-known/jwks.json`, an `agent_auth` block in the AS metadata, and a bootstrap endpoint (`POST /agent/identity`) that exchanges an agent-scoped API key for a long-lived identity assertion, redeemable at `/oauth2/token` via `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer` for a short-lived access token. Still missing: an RFC 9728 protected-resource metadata document, ID-JAG assertion intake from third parties, and the email-OTP claim ceremony. See [Agent identity](#agent-identity) for where we're heading. Each e2a agent has a stable, provisioned email address and can obtain an e2a-issued identity assertion; that issuer-backed assertion, rather than arbitrary inbound mail, is the basis for the identity-provider direction.

## Use the existing tooling first

Before you do anything credential-shaped, check whether the user has already wired e2a into your environment. Most of the time they have, and you should not be handling credentials (API keys or OAuth tokens) yourself.

1. **e2a MCP server** — if you are an MCP client (Claude, Cursor, Codex, ChatGPT, etc.), e2a's MCP server is hosted: point your MCP client at the Streamable HTTP endpoint `https://api.e2a.dev/mcp` (OAuth 2.1 — no key pasted) to get inbox, send, reply, agents, and domains as tools. Repository: `https://github.com/tokencanopy/e2a/tree/main/mcp`.
2. **e2a agent skill** — a guidance document covering inbox patterns, idempotency, attachment handling, and webhook verification. Load it before calling the API directly so you don't relearn the gotchas on the user's dime.
3. **e2a CLI** — if you are running shell commands, prefer `e2a` over hand-rolled `curl`. It handles auth resolution, retries, and non-interactive flags. Install: `npm i -g @e2a/cli`. Repository: `https://github.com/tokencanopy/e2a/tree/main/cli`.

If any of these is already configured, use it and stop. Do not ask the user for an API key you do not need.

## Credentials

Authenticated `/v1/...` endpoints accept two credential shapes, dispatched by token prefix:

- **OAuth access token** (`ate2a_…`, refresh `rte2a_…`) — issued by the e2a OAuth server to a registered client after the user consents in a browser. Use this if you are an MCP client.
- **API key** (`e2a_acct_…` for account scope, `e2a_agt_…` for a single-agent scope) — issued through the dashboard, CLI, or account API and supplied to your environment out of band. Use this if you are a CLI, script, server-side integration, or direct API consumer.

Both are presented as `Authorization: Bearer <credential>`.

### Path A — MCP client via OAuth DCR

If you are an MCP client, you do not need an API key. Run the standard discovery + DCR + authorization-code flow:

1. **Discover** — `GET https://api.e2a.dev/.well-known/oauth-authorization-server`. Read `registration_endpoint`, `authorization_endpoint`, `token_endpoint`, and `scopes_supported` (`agent` and `account`).
2. **Register** — `POST` your client metadata to `registration_endpoint` (RFC 7591), including `scope: "agent"` for agent-only access or `scope: "agent account"` to make workspace-admin consent eligible. Account scope is eligible only when every registered redirect URI is HTTPS or an HTTP loopback URI. Omitting `scope` registers the full ceiling the redirect URIs allow. You'll receive a `client_id`; token endpoint auth method is `none` because you are a public client.
3. **Authorize** — redirect the user to `authorization_endpoint` with `response_type=code`, your `client_id`, `redirect_uri`, a space-delimited scope matching the registered access (`scope=agent` or `scope=agent account`, query-encoded), and PKCE S256 (`code_challenge`, `code_challenge_method=S256`). The user logs in and chooses the exact grant.
4. **Token exchange** — `POST` `code` + `code_verifier` to `token_endpoint`. You receive `access_token` (prefix `ate2a_…`) and `refresh_token`.
5. **Use** — present the access token as a bearer; refresh with the refresh token before `expires_in`.

Access tokens carry the user identity that consented to your client; every `/v1/...` call is scoped to that user.

### Path B — Direct API consumer via API key

The user issues an API key from the e2a dashboard and supplies it to you through a secure channel — never by pasting it into chat.

#### How to pick the key up

Look for it in this order. Stop at the first one that exists:

1. `E2A_API_KEY` in your process environment.
2. A project `.env` file the user has told you to read.
3. The user's CLI config at `~/.e2a/config.json` (populated via `e2a login`; used automatically when you invoke the `e2a` CLI).

If you're invoking the e2a MCP server, you don't pick the key up at all — the server reads `E2A_API_KEY` from its own environment (set in the MCP client's `env` block) and you call tools through it.

If none of the above is set and you genuinely need a key, **do not ask the user to paste it into the conversation**. Instead, tell them to:

- Create one in the e2a dashboard.
- Put it in `E2A_API_KEY` in their shell, `.env`, MCP client config, or run `e2a login` to populate `~/.e2a/config.json` — whichever matches how they invoke you.
- Resume the task once it is set.

This keeps the key out of your transcript, out of any logs the user shares, and out of the model provider's training data.

API keys may have an optional hard expiry chosen at creation; a key created
without `expires_at` does not expire automatically. Treat a `401` on a
previously-working key as expiry or revocation: drop it from memory and ask the
user to refresh whichever source you read it from.

### How to use the credential

Whether `access_token` or `api_key`, present it as a bearer token. The message surface is agent-scoped — the sending agent is in the path (URL-encode the `@`), so there is no `from` field in the body. Example send:

```http
POST /v1/agents/bot%40agents.e2a.dev/messages HTTP/1.1
Host: api.e2a.dev
Authorization: Bearer $CREDENTIAL
Content-Type: application/json
Idempotency-Key: <UUIDv4>

{
  "to": ["alice@example.com"],
  "subject": "Hello from your agent",
  "text": "Plain-text body. Required.",
  "html": "<p>Optional HTML alternative.</p>"
}
```

For a literal send, `subject` and the plain-text `text` body are required; the
HTML alternative is optional. A template send uses `template_id` or
`template_alias` instead and must omit literal `subject`, `text`, and `html`.

Read the credential from the environment at the moment of the call. Do not copy it into variables you log, do not echo it back to the user, do not include it in commit messages, PR descriptions, error reports, or screenshots. If you are running a shell command, never interpolate the credential inline — reference the environment variable so it does not appear in command history.

Set an `Idempotency-Key` (UUIDv4 recommended) per logical operation on side-effectful calls such as sends and replies. Reuse the **same** key on transport retries (network failures, timeouts) — the server replays the original response. Same key with a different body returns `422`; a genuinely new operation needs a fresh key.

### Handling `pending_review`

If a send, reply, or forward returns **`202 Accepted`** with
`status: "pending_review"`, the server accepted the message but did not dispatch
it:

```json
{
  "message_id": "msg_abc123",
  "status": "pending_review",
  "approval_expires_at": "2026-05-28T13:00:00Z"
}
```

Do not retry the send — another call can queue a duplicate. Surface the status,
`message_id`, and `approval_expires_at` to the calling user, then stop.

### Errors

| Status | Where | Meaning | What to do |
| --- | --- | --- | --- |
| `400` | send, reply | Missing `subject` or `text`; malformed recipient; CRLF in subject. | Fix the payload before retrying. |
| `401` first use | any | Credential missing, malformed, revoked, or for a different environment. | Ask the user to confirm the value in their `E2A_API_KEY` / config is current and active in the dashboard. MCP clients should restart at discovery. |
| `401` on previously-working credential | any | Revoked or rotated. | Drop the cached value. API-key consumers re-read from the same source you loaded it from. MCP clients refresh, then re-run the authorization-code flow if refresh fails. |
| `403` | send, reply | Agent's sending domain is not verified. | Ask the user to register and verify the domain in the dashboard (`POST /v1/domains` then `POST /v1/domains/{domain}/verify`). |
| `409` | send, reply, approve | An in-flight request with this `Idempotency-Key` is still being processed, or the message is no longer in the expected state. | Wait and re-poll `GET /v1/agents/{address}/messages/{id}`. |
| `422` | send, reply | `Idempotency-Key` reused with a different body. | Mint a fresh key for the new payload. |
| `429` | any | Rate limited (60 sends/agent/minute; 200 agent registrations/IP/hour on `/v1/agents`). | Back off; honor `Retry-After` (delay-seconds form). |

The `WWW-Authenticate` header on 401 responses tells you whether the failing credential was an OAuth token (carries RFC 6750 §3.1 `error="invalid_token"` params) or an API key (bare `Bearer realm="e2a"`). MCP clients should branch on this.

## Agent identity

This section describes e2a's bet on where agent auth is heading. The bootstrap identity endpoint below is shipped (behind an opt-in signing-key config); third-party ID-JAG consumption and the email-loop claim ceremony are still direction, not shipped surface. If you are implementing today, use the credential paths above for anything beyond the identity bootstrap.

Every e2a agent has a stable, provisioned email address. For a custom domain, the owner proves control through DNS records and a verification token. For the shared `agents.e2a.dev` domain, e2a provisions the address directly. e2a also evaluates SPF, DKIM, and DMARC on inbound messages and returns structured evidence about the From domain. That evidence does not prove a person, mailbox, or message content. When agent-identity signing is enabled, the separate OAuth identity assertion is the claim e2a issues and signs.

We are building two pieces on top of this:

### e2a as an identity provider

e2a already operates as an OAuth issuer at `https://api.e2a.dev` (see AS metadata above), publishes a JSON Web Key Set at `https://api.e2a.dev/.well-known/jwks.json`, and lets an agent bootstrap a long-lived identity assertion from its API key via `POST /agent/identity`, redeemable for a short-lived access token at `/oauth2/token`. The remaining work is issuing audience-bound [ID-JAG](https://datatracker.ietf.org/doc/draft-ietf-oauth-identity-assertion-authz-grant/) assertions (`urn:ietf:params:oauth:token-type:id-jag`) that a *third-party* service can verify, with:

- `iss` = `https://api.e2a.dev`
- `sub` = the agent's provisioned email
- `email` / `email_verified: true`
- `aud` = the third-party service the agent is registering with
- Short `exp` (≤5 minutes), fresh `jti`

Any auth.md-implementing service that adds e2a to its trust list will be able to onboard an e2a agent without an OTP ceremony. The e2a-issued assertion vouches for the agent identity, which is tied to a stable, provisioned email address across agent runtimes.

If you operate an agent service and want to accept e2a-issued assertions, watch for `iss: https://api.e2a.dev` to land in the WorkOS reference trust list, or open an issue at `https://github.com/tokencanopy/e2a/issues` to pre-register.

### Email-loop claim completion (proposed)

The WorkOS auth.md OTP ceremony assumes a human reads a 6-digit code back to the agent. For agents that have an e2a inbox, we are prototyping an inbox-driven completion:

1. The third-party service sends a single-click approval mail to the user.
2. The user clicks "confirm".
3. The service emails a confirmation to the agent's e2a inbox.
4. The agent receives the confirmation via WebSocket or webhook and posts to `/agent/auth/claim/complete`.

No code reading, no copy-paste, no transcript leakage. This is uniquely possible for e2a because the agent's mailbox is part of the product. We will publish a flow extension when the prototype is stable; if you are building an auth.md service and want to support this from day one, open an issue at `https://github.com/tokencanopy/e2a/issues`.

## Discovery

What e2a publishes today:

- **RFC 8414 authorization-server metadata** at [`https://api.e2a.dev/.well-known/oauth-authorization-server`](https://api.e2a.dev/.well-known/oauth-authorization-server) — advertises `authorization_endpoint`, `token_endpoint`, `registration_endpoint`, `revocation_endpoint`, supported grants (`authorization_code`, `refresh_token`), PKCE (`S256`), and the RFC 9207 `iss` parameter. Request the `agent` scope (`account` for workspace-admin access).
- **RFC 6750 Bearer challenges** on every 401 from `/v1/...` — `WWW-Authenticate: Bearer realm="e2a"` for unknown/missing credentials, plus RFC 6750 §3.1 error params for OAuth-bearer failures.

What e2a publishes for the autonomous-agent path today (gated behind an opt-in signing-key config; see [Agent identity](#agent-identity)):

- An `agent_auth` block in the AS metadata (`identity_endpoint`, `jwks_uri`, and related fields).
- A JSON Web Key Set at `/.well-known/jwks.json`.
- A bootstrap endpoint, `POST /agent/identity`, that mints an identity assertion from an agent-scoped API key — e2a's adaptation of the flow (the bootstrap credential is a domain-verified agent-scoped key, not an ownerless self-registration).

What's missing for full auth.md compliance:

- An RFC 9728 protected-resource metadata document at `/.well-known/oauth-protected-resource`, and `resource_metadata="..."` parameter on the WWW-Authenticate challenge so agents can auto-discover it.
- ID-JAG assertion intake so third-party services can register e2a-issued assertions (the `anonymous` and `identity_assertion + id-jag` flows from the spec).
- The email-OTP claim ceremony (see [Email-loop claim completion](#email-loop-claim-completion-proposed)).

When these land, this document will be updated and the AS metadata will carry the canonical machine-readable description.

## Revocation

The user revokes API keys in the e2a dashboard. OAuth access tokens are revoked via `POST /oauth2/revoke` (RFC 7009) or by the user disconnecting the client in the dashboard. Either way, you will discover revocation as a `401` on a previously-working credential — drop it and re-acquire from the same source you loaded it from. Once e2a issues ID-JAGs, providers will be able to POST logout tokens to a `revocation_uri` advertised in AS metadata; that is not in scope for the current credential paths.

## References

- WorkOS [auth.md protocol](https://workos.com/auth-md) — the open spec this document follows
- [github.com/workos/auth.md](https://github.com/workos/auth.md) — reference implementation
- e2a [OpenAPI contract](https://e2a.dev/v1/openapi.yaml) — full reference for the endpoints above
