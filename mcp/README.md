# @e2a/mcp-server

[Model Context Protocol](https://modelcontextprotocol.io) server for [e2a](https://e2a.dev) — gives any MCP-aware AI agent its own email inbox to send, receive, reply, and (optionally) review held outbound mail before it ships.

Works with Google ADK, LangChain, OpenAI Agents SDK, Claude Desktop, Cursor, Cline, and any other MCP host.

## Connect

e2a's MCP server is hosted. Point your MCP host at the Streamable HTTP endpoint:

```
https://api.e2a.dev/mcp
```

Two ways to authenticate:

- **OAuth 2.1 (recommended for interactive hosts)** — add e2a as a connector and authorize in the browser. No key is pasted into config.
- **Bearer API key (programmatic / self-host)** — send your [e2a dashboard](https://e2a.dev) API key in the `Authorization: Bearer <e2a API key>` header.

An agent-scoped credential resolves its agent server-side. Account-scoped callers pass the agent `email` per tool call.

## Quick start

### Google ADK (Python)

```python
import os

from google.adk.agents import Agent
from google.adk.tools.mcp_tool.mcp_toolset import McpToolset
from google.adk.tools.mcp_tool.mcp_session_manager import StreamableHTTPConnectionParams

root_agent = Agent(
    model="gemini-flash-latest",
    name="e2a_agent",
    instruction="Help the user manage their email. Reply to threads with `reply_to_message` to preserve threading headers.",
    tools=[
        McpToolset(
            connection_params=StreamableHTTPConnectionParams(
                url="https://api.e2a.dev/mcp",
                headers={
                    "Authorization": f"Bearer {os.environ['E2A_API_KEY']}",
                },
                timeout=30,
            ),
        ),
    ],
)
```

### LangChain (Python)

Using [`langchain-mcp-adapters`](https://github.com/langchain-ai/langchain-mcp-adapters):

```python
import os

from langchain.agents import create_agent
from langchain_mcp_adapters.client import MultiServerMCPClient

client = MultiServerMCPClient({
    "e2a": {
        "transport": "http",
        "url": os.getenv("E2A_MCP_URL", "https://api.e2a.dev/mcp"),
        "headers": {
            "Authorization": f"Bearer {os.environ['E2A_API_KEY']}",
        },
    },
})

tools = await client.get_tools()
agent = create_agent(
    "anthropic:claude-sonnet-4-6",
    tools=tools,
    system_prompt="Reply with reply_to_message; pending_review is accepted, so do not retry it.",
)
```

### OpenAI Agents SDK (Python)

```python
import os

from agents import Agent, Runner
from agents.mcp import MCPServerStreamableHttp

async with MCPServerStreamableHttp(
    name="e2a",
    params={
        "url": os.getenv("E2A_MCP_URL", "https://api.e2a.dev/mcp"),
        "headers": {
            "Authorization": f"Bearer {os.environ['E2A_API_KEY']}",
        },
        "timeout": 30,
    },
    cache_tools_list=True,
    max_retry_attempts=3,
) as e2a:
    agent = Agent(name="e2a_agent", mcp_servers=[e2a])
    result = await Runner.run(agent, "Reply to the latest unread email politely.")
```

### Claude Desktop / Cline / Cursor

Add e2a as a remote MCP server in the host's config (`claude_desktop_config.json`, `cline_mcp_settings.json`, etc.):

```json
{
  "mcpServers": {
    "e2a": {
      "url": "https://api.e2a.dev/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_E2A_API_KEY"
      }
    }
  }
}
```

Hosts that support OAuth connectors can instead add `https://api.e2a.dev/mcp` as a connector and authorize in the browser — no key pasted.

## Tools

The server exposes up to **71** tools spanning agents, messages, human-in-the-loop
approval, attachments, domains, events, webhooks, API keys, contacts/outreach,
and email templates (beta).
**The visible set depends on your credential's scope:** an **agent**-scoped
credential sees the 20 runtime/inbox tools (read, send, reply, restore
messages, per-agent outreach); an **account**-scoped credential also sees the
51 admin/setup tools (agent/domain/webhook/event/template/API-key/contact
management — **and HITL review discovery plus approve/reject, which is an
account-owner action, never agent self-approval**) — all 71.
Every tool carries MCP annotations (`readOnlyHint`/`destructiveHint`/
`idempotentHint`) so hosts can auto-approve reads and flag destructive actions.
The tables below highlight the most commonly used ones — your MCP host's tool list
shows the set your scope allows, with per-tool descriptions.

### Identity

| Tool | Description |
| --- | --- |
| `whoami` | Get the authenticated account's identity — user, scope, plan/limits; for an agent-scoped credential, also the bound agent address. |
| `list_agents` | List agent inboxes; pass `deleted:true` to list the 30-day trash. (Admin/account-scoped.) |
| `get_agent` | Get one agent inbox by its full email address. |
| `create_agent` | Register a new agent by its full email address — on a verified domain you own, or the deployment's shared domain. No delivery "mode": inbound is always available via `list_messages` (poll) or a `create_webhook` subscription. (Admin/account-scoped.) |
| `restore_agent` | Restore a soft-deleted agent and its configuration. (Admin/account-scoped.) |

> **Webhook deliveries are signed — verify them.** Push delivery is a top-level
> resource (`create_webhook`), not a per-agent mode. e2a HMAC-signs every webhook
> delivery against the webhook's signing secret (returned once from `create_webhook`
> / `rotate_webhook_secret`). Your handler must verify the signature on every
> request — the [e2a SDK](https://www.npmjs.com/package/@e2a/sdk) exposes
> `constructEvent(rawBody, signatureHeader, secret)` which verifies and returns a
> typed event in one call (throws on a bad signature). Or skip webhooks entirely
> and poll via `list_messages`.

### Messages

Scheduled sending via `send_at` / `scheduled_at` is **beta and may change
before it is declared stable**.

| Tool | Description |
| --- | --- |
| `send_message` | Send a new email. Immediate queueing returns `status: accepted`; future `send_at` returns `status: scheduled` plus `scheduled_at`; a review hold returns `status: pending_review`. All three are durable success outcomes — do not re-send. |
| `reply_to_message` | Reply to a message — one the agent received (replies to its sender) or one it sent (continues the thread to the original recipients). Preserves In-Reply-To / References for thread continuity and accepts the same optional `send_at` as `send_message`. |
| `forward_message` | Forward a message into a new thread, carrying its existing attachments by default. Accepts the same optional `send_at` as `send_message`. |
| `list_messages` | List mail; pass `deleted:true` to list trash. Filter by `read_status` (unread / read / all) and sender with reserved-word-safe `from_`; cursor-paginated (`cursor` + `limit` in, `next_cursor` out). |
| `delete_message` | Move a message to the trash (restorable for ~30 days). Requires `confirm: true`. Permanent deletion is deliberately not exposed over MCP — use the REST API/SDK. |
| `restore_message` | Restore a soft-deleted message and resume its retention clock. |
| `get_message` | Fetch full body, headers, and attachment metadata for one message. |
| `get_attachment` | Get one attachment's metadata + a short-lived `download_url` (fetch the bytes out of band); `inline: true` returns base64 `data` for small files (≤256 KB). |
| `update_message_labels` | Add or remove labels on a message. |

### Human-in-the-loop approval

`list_reviews`/`get_review` surface every account-scoped hold: outbound drafts
awaiting send approval and inbound messages held by a screening gate. The
`email.review_requested` webhook is an additional push notification for either
direction and carries the same `message_id`. The
`approve_review`/`reject_review` tools branch on direction: approving an inbound
hold releases it to the agent's inbox (no send, no draft, override fields
ignored); rejecting one drops it so it never reaches the agent.

| Tool | Description |
| --- | --- |
| `list_reviews` | List inbound and outbound messages awaiting human review across the account, soonest-expiring first. |
| `get_review` | Get the full held message (body, recipients, and screening context where applicable). |
| `approve_review` | Outbound: send a held message, optionally with reviewer edits (subject / body / recipients). Inbound: release the screening hold to the agent's inbox (overrides ignored). Account-scoped — never agent self-approval. |
| `reject_review` | Outbound: discard a held message. Inbound: drop the screening hold so it never reaches the agent. The optional `reason` is stored for audit. Account-scoped. |

### Domains

| Tool | Description |
| --- | --- |
| `register_domain` | Register a custom sending domain; returns the MX + TXT DNS records to publish. (Admin/account-scoped.) |

### API keys

All admin/account-scoped. `create_api_key` mints **agent-scoped keys only** —
the key is bound to one inbox and can act only as that agent. Account-scoped
(workspace-admin) keys cannot be created over MCP; mint those from the
dashboard or the raw API, where a human is in the loop.

| Tool | Description |
| --- | --- |
| `list_api_keys` | List the account's API keys — metadata only (secrets are shown once, at creation). |
| `create_api_key` | Mint a new agent-scoped key bound to an inbox; returns the plaintext key ONCE — store it immediately. |
| `delete_api_key` | Revoke a key permanently (requires `confirm: true`). |

### Contacts (beta)

The people an account corresponds with (`list_contacts` / `get_contact` /
`create_contact` / `update_contact` / `delete_contact`, admin/account-scoped)
and one agent's per-contact outreach state — stage, next action, reply/
suppression facts (`list_outreach_contacts` / `get_outreach_contact` /
`set_outreach_contact` / `delete_outreach_contact`, available to account scope
or an agent-scoped credential for its bound inbox). Import is inert — it
records identity and enrolls outreach without sending anything.

| Tool | Description |
| --- | --- |
| `list_contacts` / `get_contact` | List account contacts (filter by provenance/import batch/creation window) or fetch one by address. |
| `create_contact` / `update_contact` / `delete_contact` | Create, partially update (`etag`-guarded via `if_match`), or delete a contact's identity and all of its per-agent outreach rows. Deleting a contact does not remove suppressions. |
| `import_contacts` | Bulk-import contacts from rows, optionally enrolling them into an agent's outreach. |
| `delete_contact_import` | Reverse an import batch, removing untouched contacts and the enrolments it created. |
| `list_outreach_contacts` / `get_outreach_contact` | List or fetch the contacts an agent is working, with server-derived reply/delivery facts. |
| `set_outreach_contact` | Enroll a contact in an agent's outreach, or update the agent-owned fields (stage, next action). |
| `delete_outreach_contact` | Un-enroll a contact from an agent's outreach; the contact and its suppressions survive. |

### Templates (beta)

Reusable email templates with `{{variable}}` interpolation (a flat Mustache
subset — no loops/sections; missing variables render as empty strings), plus a
read-only catalog of pre-built starters (`welcome`, `verify-code`,
`password-reset`, `receipt`, `agent-status`, `daily-digest`,
`approval-request`). Send with a template via `send_message`'s `template_id` /
`template_alias` + `template_data` (mutually exclusive with literal
subject/body). All template management tools are admin/account-scoped. Beta —
shapes may change before templates are declared stable.

| Tool | Description |
| --- | --- |
| `list_templates` / `get_template` | List the account's stored templates (summary rows); `get_template` returns the full body sources. |
| `create_template` | Create a template from literal source — or copy a starter verbatim with `from_starter`. |
| `update_template` / `delete_template` | Edit (re-parses changed parts) or delete a template. |
| `validate_template` | Dry-run source: parse errors, a rendered preview against `test_data`, and `suggestedData` placeholders. |
| `list_starter_templates` / `get_starter_template` | Browse the starter catalog; the detail view includes full body sources and per-variable metadata. |

## Errors

Every failed tool call returns an MCP result with `isError: true` and **two
representations of the error**:

- **`structuredContent`** — the machine-branchable form. Branch on this, not
  the text:

  ```json
  {
    "code": "domain_not_verified",
    "retryable": false,
    "status": 403,
    "request_id": "req_abc123",
    "retry_after_seconds": 30,
    "details": { "field": "…" }
  }
  ```

  | Field | Presence | Meaning |
  | --- | --- | --- |
  | `code` | always | Stable snake_case token from the API error envelope (e.g. `domain_not_verified`, `rate_limited`, `message_not_pending`). Errors raised by the MCP layer itself (bad/missing arguments, `confirm:true` guards) carry `invalid_request`. |
  | `retryable` | always | `true` when a retry could plausibly succeed (rate limit, 5xx, connection failure). |
  | `status` | API errors only | HTTP status of the API response (`0` = connection-level failure). Absent when no request was made (MCP-layer validation). |
  | `request_id` | when available | Server request id — quote it in support requests. |
  | `retry_after_seconds` | when available | Back-off hint for retryable errors. |
  | `details` | when available | Structured field-level detail from the envelope (omitted if oversized). |

- **`content` (text)** — the human-readable form, unchanged and stable:
  `e2a error [<code>] (retryable)?: <message>` for API errors, `e2a error:
  <message>` for MCP-layer errors. Existing agents that parse this text keep
  working, but new integrations should read `structuredContent` instead.

Successful results are unaffected (JSON in a text block; list tools return a
domain-named array plus `next_cursor` while more pages remain).

## Operating the HTTP server

Self-hosters run the HTTP transport via `mcp/Dockerfile` or the `mcp`
service in the repo's `docker-compose.yaml`. Day-2 operations (SLOs,
failure classes, correlation) are covered by
[`docs/runbooks/mcp-server.md`](../docs/runbooks/mcp-server.md); this
section is the reference for configuration and surface.

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `3000` | Listen port. |
| `E2A_API_URL` | `https://api.e2a.dev` | Base URL of the e2a API server this transport forwards bearers to (and probes for readiness). Canonical name; `E2A_URL` and `E2A_BASE_URL` are legacy aliases, still accepted with a deprecation warning. |
| `MCP_ALLOWED_HOSTS` | `api.e2a.dev` | Comma-separated Host allowlist (DNS-rebinding guard). Requests to `/mcp` and the discovery document with a Host outside the list get 421. Ports are stripped before comparison. |
| `MCP_PUBLIC_URL` | unset | Externally reachable URL of this MCP server, used verbatim in the RFC 9728 protected-resource metadata and the 401 `WWW-Authenticate` challenge. Set when the fronting proxy's view of the URL differs from the inbound Host (local dev: `http://localhost:8765`). |
| `MCP_AUTHORIZATION_SERVER_URL` | `$E2A_API_URL` | Authorization-server URL advertised in protected-resource metadata. Override when the bearer-forwarding URL is container-internal but OAuth clients must reach the AS from the host. |
| `E2A_TRUST_PROXY` | `loopback` | Express `trust proxy` setting — which hops' `X-Forwarded-*` headers are honored for discovery URLs. `true`/`false`, a hop count, or a preset/subnet list. |
| `MCP_RESOLVE_TIMEOUT_MS` | `5000` | Positive-int bound (ms) on the whoami probe that resolves a bearer to its principal, with at most 1 retry. On timeout/backend 5xx the request is served fail-closed at least-privilege agent scope, uncached, and logged as `auth_resolution result=fallback`. |
| `MCP_SESSION_IDLE_MS` | `300000` | **Legacy name — sizes the resolve cache, not sessions.** TTL (ms) of a cached bearer→principal resolution. The transport is stateless; there are no sessions to time out. |
| `MCP_MAX_SESSIONS` | `500` | **Legacy name — sizes the resolve cache, not sessions.** Max cached bearer→principal entries; oldest evicted past the cap. A miss costs one `whoami`, never a disconnect. |

### Health endpoints

All three are unauthenticated:

- `GET /healthz` — process liveness only, never touches the backend:
  `{ "ok": true }`. Wire to your liveness probe.
- `GET /readyz` — readiness: probes `{E2A_API_URL}/api/health` (2s timeout,
  result cached 10s so a scraping fleet can't cause a probe storm). 200
  `{ "ok": true, "checks": { "api": "ok" } }` when reachable; 503
  `{ "ok": false, "checks": { "api": "unreachable" }, "request_id": "…" }`
  + `Retry-After: 10` (the failure-cache TTL) when not. Wire to your readiness probe.
- `GET /metrics` — Prometheus text exposition. Example lines:

  ```
  mcp_http_requests_total{route="mcp",status_class="2xx"} 1523
  mcp_http_request_duration_seconds_bucket{route="mcp",le="0.5"} 1480
  mcp_auth_resolutions_total{result="cache_hit"} 1490
  mcp_tool_executions_total{tool="list_messages",outcome="ok"} 412
  mcp_readyz_checks_total{result="ok"} 60
  mcp_resolve_cache_entries 7
  ```

  Route labels are bounded: `mcp | healthz | readyz | metrics | discovery | other`.

### Structured logs

Single-line JSON on stderr (GCE-shaped `severity` / `event` / `message`
plus fields). Request-scoped events carry `request_id` (the response's
`X-Request-Id`): `auth_resolution` (`result`, `duration_ms`, `scope?`;
`invalid`/`fallback` at WARNING), `http_request` (`route`, `method`,
`status`, `duration_ms`; successful probe/scrape traffic on
`healthz`/`readyz`/`metrics` is metered but not logged), `tool_execution` (`tool`, `outcome`,
`duration_ms`, `error_code?`), `terminal_error`. Bearer tokens are never
logged; the resolve cache is keyed by SHA-256 fingerprints. An inbound
`X-Request-Id` matching `^[A-Za-z0-9_-]{1,64}$` is honored; otherwise the
server mints `mcpreq_<12hex>`. The id is echoed on every response but is
**not** forwarded to the backend API.

### Client retry policy

Failed tool calls return `structuredContent` (see [Errors](#errors)):
branch on `code` / `retryable` / `retry_after_seconds`. Retry only
`retryable: true` errors with exponential backoff + jitter, ~3 attempts
max. Never retry 4xx (401 → re-authenticate; 421 → fix the URL).
`accepted`, `scheduled`, and `pending_review` on send tools are success outcomes
— do not retry them. A future `send_at` to the sending agent's own address
returns `400 invalid_request` when direct loopback applies; review holds take
precedence and drop the schedule. The transport is stateless, so "reconnect"
after an interruption is simply re-POSTing; MCP clients do this automatically.

## Links

- [e2a docs](https://e2a.dev)
- [Source](https://github.com/tokencanopy/e2a/tree/main/mcp)
- [Issues](https://github.com/tokencanopy/e2a/issues)
- [Model Context Protocol](https://modelcontextprotocol.io)

## License

Apache-2.0
