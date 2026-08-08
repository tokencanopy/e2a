# Outbound `/v1` — `deliverOutbound` extraction plan

> **✅ Shipped — historical plan.** The `DeliverOutbound` core extraction landed,
> and `send` ships nested at `POST /v1/agents/{email}/messages` (the unified
> shape this doc framed as "Slice 2"). Kept as the original extraction plan; see
> api-v1-redesign §9/§11 for the as-built. The body below is design-time
> vocabulary: as built, every `{address}` placeholder is `{email}`, and a held
> send returns **200** with `status:"pending_review"` (not 202
> `pending_approval` — 202 was retired as a GA success code).

| | |
|---|---|
| **Status** | Done (shipped 2026-06) |
| **Part of** | Slice 1 (api-v1-redesign), PR #208 |
| **Risk** | HIGH — live email/SES (money), HITL owner-notification, idempotency |

## Why this is gated, not auto-ported

Every other `/v1` resource ported cleanly because it was store-backed and
HTTP-thin. The outbound path is not: the legacy handlers
(`handleSendEmail`, `handleReplyToMessage`, `handleForwardMessage`,
`handleSendTestEmail`) are woven through with HTTP-response state —
`idempotencyGuard`'s `capturingWriter`, `markSideEffectCommitted(w)`, and
`holdForApproval(w, r, …)` which writes the 202 directly. A bug here
double-sends to SES (cost + reputation) or mis-fires owner-approval emails.
This refactor must be executed with focus and verified against the existing
outbound/HITL/self-send/idempotency **integration** suite (Postgres on :5433),
not ground out in autonomous ticks. The reusable `runIdempotent` helper
(already shipped, `internal/httpapi/idempotency.go`) is the v1 replacement for
the `capturingWriter`.

## Step 1 — extract the HTTP-free cores in `internal/agent`

```go
// OutboundResult discriminates the delivery outcome (HTTP-free).
type OutboundResult struct {
    Held              bool       // HITL: queued as pending_approval
    PendingMessageID  string
    ApprovalExpiresAt *time.Time
    MessageID         string     // provider/loopback id (when sent)
    Method            string     // "smtp" | "loopback"
    // SideEffectCommitted is always true on a non-error return: the
    // pending row / loopback row / SES handoff has happened, so the
    // idempotency key must be Completed (cached), never Released.
}

// HoldForApprovalCore is holdForApproval minus the HTTP write: marshal
// attachments, CreatePendingOutboundMessage, NotifyPendingApprovalAsync,
// publishPendingApproval; returns the pending *identity.Message.
func (a *API) HoldForApprovalCore(ctx context.Context, agent *identity.AgentIdentity,
    req outbound.SendRequest, msgType, replyToEmailMessageID string) (*identity.Message, error)

// DeliverOutbound is the shared send/reply/forward tail (HTTP-free):
//   HITL?      -> HoldForApprovalCore            -> {Held:true,...}
//   self-send? -> performSelfSend                -> {Method:"loopback",...}
//   else       -> sender.Send + CreateOutboundMessage + publishSent -> {Method:"smtp",...}
// Caller has already: authed, resolved+owned the agent, rate-limited,
// domain-verified, run enforcer.CheckMessageSend, built the SendRequest.
func (a *API) DeliverOutbound(ctx context.Context, user *identity.User,
    agent *identity.AgentIdentity, req outbound.SendRequest,
    msgType, replyToEmailMessageID, conversationID, subject string) (*OutboundResult, error)
```

Rewire the legacy handlers to: build `SendRequest` + validate + resolve +
limits (unchanged) → `DeliverOutbound` → on success `markSideEffectCommitted(w)`
and write the same JSON the handler writes today. **No behavior change.**

## Step 2 — verify the extraction (gate)

Run the full existing suite against local Postgres — must stay green
(no behavior change):

```
make test-integration   # identity/agent incl. selfsend, hitl, idempotency, forward
make test-e2e
```

These tests (`idempotency_sideeffect_test`, `selfsend_test`, `hitl_api_test`,
`hitl_approval_api_test`, `forward_api_test`, `hitl_magic_api_test`) are the
safety net that proves the cores behave identically.

## Step 3 — the `/v1` endpoints (Slice 1 keeps the *separate* routes)

Per design §9, the *unified* `POST /agents/{address}/messages` is **Slice 2**.
Slice 1 ports the existing separate routes onto Huma, sharing `DeliverOutbound`
via `runIdempotent`:

| `/v1` route | builds SendRequest | msgType |
|---|---|---|
| `POST /v1/agents/{address}/messages` | sender = path agent; body (`to`/subject/body/html/cc/bcc/attachments); `from` optional, must equal the agent | `send` |
| `POST /v1/agents/{address}/messages/{id}/reply` | from inbound + body | `reply` |
| `POST /v1/agents/{address}/messages/{id}/forward` | from inbound + body | `forward` |
| `POST /v1/agents/{address}/test` | server-built test message | `test` |

Each handler input carries `RawBody []byte` + `IdempotencyKey string
\`header:"Idempotency-Key"\``, and runs `DeliverOutbound` inside
`runIdempotent` so a non-error return is cached and a pre-side-effect failure
releases the key. Response views: `{status, message_id, method}` for sent/
loopback; `{status:"pending_approval", message_id, approval_expires_at}` (202)
for held — mirroring the legacy bodies.

## Step 4 — tests

Unit (httptest + fakes): sent path, loopback/self-send, HITL→202 held,
validation 400s (missing subject/body, CRLF subject, no recipients), domain
unverified 403, over-cap 402, idempotency replay (reuse the `runIdempotent`
guarantees). Plus a real-binary e2e via Mailpit (`make docker-up`) exercising
an actual send.

## Related sensitive items (same risk class, also gated)

- **`DELETE /v1/account`** — billing-hook notify + OAuth-row cascade +
  SERIALIZABLE delete. Extract `DeleteUserDataCore`; verify against
  `user_data_rights_api_test`.
- **`GET /v1/account/export`** — `ExportUserData` + OAuth connections;
  `Content-Disposition` attachment. Lower risk (read), but couples OAuth.
